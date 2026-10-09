package devicesync

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/syncproto/synctest"
	"github.com/flopwire/flopwire/internal/transcript"
)

// Literal short exports stay provisional tails with the normal small chunker.
// The crash tests exercise actual capture, SQLite commit and restart upload;
// they do not qualify power loss or a provider's export implementation.
var tailCaptureH = []byte("{\"record\":\"old\"}\n")
var tailCaptureJ = append(bytes.Clone(tailCaptureH), []byte("{\"record\":\"new\"}\n")...)
var tailCaptureK = append(bytes.Clone(tailCaptureJ), []byte("{\"record\":\"a longer retry\"}\n")...)

func tailCaptureSpec(dir string) SourceSpec {
	return SourceSpec{Path: filepath.Join(dir, "synthetic-export"), Agent: transcript.AgentDevin, StorageKind: transcript.StorageSQLite, Parser: "devin-export@2", Export: true, SessionKey: "synthetic-tail-capture"}
}

func tailCaptureSource(t *testing.T, store *Store, spec SourceSpec) *sourceRow {
	t.Helper()
	src, err := store.source(t.Context(), spec.Path, &spec)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func requireTailCaptureLedger(t *testing.T, db *sql.DB, spec SourceSpec, body []byte, acked bool) {
	t.Helper()
	var current, gen, size, entries, offset, length, ack int64
	var tailAck, lost, closed bool
	var hash []byte
	var watermark, rawSpec, fileID string
	if err := db.QueryRow(`SELECT s.generation,s.watermark,s.spec,g.generation,g.size,g.entries,g.tail_offset,g.tail_size,g.tail_hash,g.acked,g.tail_acked,g.lost,g.closed,g.file_id FROM devsync_sources s JOIN devsync_gens g ON g.source_id=s.id WHERE s.path=?`, spec.Path).Scan(&current, &watermark, &rawSpec, &gen, &size, &entries, &offset, &length, &hash, &ack, &tailAck, &lost, &closed, &fileID); err != nil {
		t.Fatal(err)
	}
	wantHash := syncproto.Sum(body)
	if current != 0 || gen != 0 || size != int64(len(body)) || entries != 0 || offset != 0 || length != size || !bytes.Equal(hash, wantHash[:]) || ack != 0 || tailAck != acked || lost || closed || fileID != "" {
		t.Fatalf("capture ledger differs: current=%d gen=%d size=%d entries=%d tail=%d/%d ack=%d/%v lost=%v closed=%v", current, gen, size, entries, offset, length, ack, tailAck, lost, closed)
	}
	var wm storedWatermark
	if err := json.Unmarshal([]byte(watermark), &wm); err != nil || wm.Offset != size || string(wm.Export) != strconv.Itoa(len(body)) {
		t.Fatalf("capture continuation differs: offset=%d export=%q err=%v", wm.Offset, wm.Export, err)
	}
	var stored storedSpec
	if err := json.Unmarshal([]byte(rawSpec), &stored); err != nil || stored.SourceSpec.Path != spec.Path || stored.SourceSpec.Agent != spec.Agent || stored.SourceSpec.StorageKind != spec.StorageKind || !stored.SourceSpec.Export || stored.SourceSpec.SessionKey != spec.SessionKey {
		t.Fatalf("source identity differs: %v", err)
	}
	var generations, manifests int
	if err := db.QueryRow(`SELECT count(*) FROM devsync_gens`).Scan(&generations); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM devsync_manifest`).Scan(&manifests); err != nil {
		t.Fatal(err)
	}
	if generations != 1 || manifests != 0 {
		t.Fatalf("unexpected generation/manifest counts %d/%d", generations, manifests)
	}
}

func TestTailCaptureCapPreservesCommittedLegacy(t *testing.T) {
	e := newEnv(t, Config{SealAfter: -1}, int64(len(tailCaptureH)+len(tailCaptureJ)-1))
	sp := tailCaptureSpec(e.dir)
	src := tailCaptureSource(t, e.store, sp)
	g := &growingExport{log: bytes.Clone(tailCaptureH)}
	if err := e.sy.ordinaryOperation().capture(t.Context(), src, g.fn, -1, nil); err != nil {
		t.Fatal(err)
	}
	// Under this fixture's exclusive owner, emulate a legacy-only installation.
	if _, err := PrepareLegacyTails(t.Context(), e.store.db, e.spool.dir); err != nil {
		t.Fatal(err)
	}
	g.log = bytes.Clone(tailCaptureJ)
	if err := e.sy.ordinaryOperation().capture(t.Context(), src, g.fn, -1, nil); !errors.Is(err, ErrSpoolFull) {
		t.Fatalf("want cap refusal, got %v", err)
	}
	requireTailCaptureLedger(t, e.store.db, sp, tailCaptureH, false)
	assertReconcileFile(t, filepath.Join(e.spool.dir, "tails", fmt.Sprintf("%d-0", src.ID)), tailCaptureH)
	if e.spool.Used() != int64(len(tailCaptureH)) {
		t.Fatalf("charged bytes changed on refusal: %d", e.spool.Used())
	}
}

func TestTailCaptureRetryReclaimsOnlyUncommittedVersion(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("commit=%v", committed), func(t *testing.T) {
			want := tailCaptureH
			if committed {
				want = tailCaptureJ
			}
			e := newEnv(t, Config{SealAfter: -1}, int64(len(want)+len(tailCaptureK)))
			sp := tailCaptureSpec(e.dir)
			src := tailCaptureSource(t, e.store, sp)
			g := &growingExport{log: bytes.Clone(tailCaptureH)}
			if err := e.sy.ordinaryOperation().capture(t.Context(), src, g.fn, -1, nil); err != nil {
				t.Fatal(err)
			}
			g.log = bytes.Clone(tailCaptureJ)
			stop := errors.New("publication handoff rejected")
			commit := func(ctx context.Context, src *sourceRow, gen *genRow, entries []syncproto.Entry, wm *transcript.Watermark, state []byte, guards ...func() error) error {
				if !committed {
					guards = append(guards, func() error { return stop })
				}
				if err := e.store.saveCapture(ctx, src, gen, entries, wm, state, guards...); err != nil {
					return err
				}
				return stop // committed, but cleanup has not run
			}
			if err := e.sy.ordinaryOperation().captureWithCommit(t.Context(), src, g.fn, -1, nil, commit); !errors.Is(err, stop) {
				t.Fatalf("want handoff rejection, got %v", err)
			}
			requireTailCaptureLedger(t, e.store.db, sp, want, false)
			assertReconcileFile(t, reconcileVersionPath(e.spool, src.ID, 0, syncproto.Sum(tailCaptureH)), tailCaptureH)
			assertReconcileFile(t, reconcileVersionPath(e.spool, src.ID, 0, syncproto.Sum(tailCaptureJ)), tailCaptureJ)
			// The fixed fixture cap requires reconciliation before publishing K.
			g.log = bytes.Clone(tailCaptureK)
			src = tailCaptureSource(t, e.store, sp) // discard any failed in-memory mutations
			if err := e.sy.ordinaryOperation().capture(t.Context(), src, g.fn, -1, nil); err != nil {
				t.Fatalf("same-source retry: %v", err)
			}
			requireTailCaptureLedger(t, e.store.db, sp, tailCaptureK, false)
			assertReconcileFile(t, reconcileVersionPath(e.spool, src.ID, 0, syncproto.Sum(tailCaptureK)), tailCaptureK)
			if e.spool.Used() != int64(len(tailCaptureK)) {
				t.Fatalf("obsolete versions remain charged: %d", e.spool.Used())
			}
		})
	}
}

func TestTailCaptureCrashHelper(t *testing.T) {
	if os.Getenv("FLOPWIRE_CAPTURE_CRASH_CHILD") != "1" {
		t.Skip("subprocess helper")
	}
	dir, phase := os.Getenv("FLOPWIRE_CAPTURE_CRASH_DIR"), os.Getenv("FLOPWIRE_CAPTURE_CRASH_PHASE")
	if dir == "" || (phase != "before" && phase != "after") {
		t.Fatal("invalid helper configuration")
	}
	store, err := OpenStore(filepath.Join(dir, "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	spool, err := OpenSpool(filepath.Join(dir, "spool"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	sy, err := NewSyncer(Config{Chunk: small, SealAfter: -1}, store, spool, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sy.Close()
	src := tailCaptureSource(t, store, tailCaptureSpec(dir))
	g := &growingExport{log: bytes.Clone(tailCaptureH)}
	if err := sy.ordinaryOperation().capture(t.Context(), src, g.fn, -1, nil); err != nil {
		t.Fatal(err)
	}
	g.log = bytes.Clone(tailCaptureJ)
	signal, control := os.NewFile(3, "capture-barrier"), os.NewFile(4, "capture-control")
	defer signal.Close()
	defer control.Close()
	barrier := func() error {
		if _, err := fmt.Fprintln(signal, phase); err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, control); err != nil {
			return err
		}
		return errors.New("barrier released without process kill")
	}
	commit := func(ctx context.Context, src *sourceRow, gen *genRow, entries []syncproto.Entry, wm *transcript.Watermark, state []byte, guards ...func() error) error {
		if phase == "before" {
			guards = append(guards, barrier)
		}
		if err := store.saveCapture(ctx, src, gen, entries, wm, state, guards...); err != nil {
			return err
		}
		return barrier()
	}
	if err := sy.ordinaryOperation().captureWithCommit(t.Context(), src, g.fn, -1, nil, commit); err != nil {
		t.Fatal(err)
	}
	t.Fatal("capture passed its crash barrier")
}

func TestTailCaptureProcessDeathSelectsCommittedVersion(t *testing.T) {
	for _, phase := range []string{"before", "after"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			sp := tailCaptureSpec(dir)
			want := tailCaptureH
			if phase == "after" {
				want = tailCaptureJ
			}
			killNamedCaptureHelperAtBarrier(t, dir, phase, "TestTailCaptureCrashHelper", func() {
				db := tailPreparationReadOnly(t, filepath.Join(dir, "sync.db"))
				requireTailCaptureLedger(t, db, sp, want, false)
				for _, body := range [][]byte{tailCaptureH, tailCaptureJ} {
					assertReconcileFile(t, filepath.Join(dir, "spool", "tails", "1-0-"+syncproto.Sum(body).String()), body)
				}
			})
			store, err := OpenStore(filepath.Join(dir, "sync.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			spool, err := OpenSpool(filepath.Join(dir, "spool"), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			srv := synctest.New("device-token")
			http := httptest.NewServer(srv)
			defer http.Close()
			client := &syncproto.Client{Server: http.URL, Token: "device-token", HTTP: http.Client()}
			sy, err := NewSyncer(Config{Chunk: small, SealAfter: -1}, store, spool, client)
			if err != nil {
				t.Fatal(err)
			}
			defer sy.Close()
			assertReconcileFile(t, reconcileVersionPath(spool, 1, 0, syncproto.Sum(want)), want)
			if spool.Used() != int64(len(want)) {
				t.Fatalf("startup retained uncommitted bytes: %d", spool.Used())
			}
			if err := sy.Resume(t.Context(), sp); err != nil {
				t.Fatal(err)
			}
			got, err := srv.Reconstruct(sp.Path, "", 0)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("original generation upload: %q, %v", got, err)
			}
			requireTailCaptureLedger(t, store.db, sp, want, true)
			pending, err := store.pendingGens(t.Context(), 1)
			if err != nil || len(pending) != 0 {
				t.Fatalf("pending generation did not drain: %d, %v", len(pending), err)
			}
		})
	}
}
