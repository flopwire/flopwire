package devicesync

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

// These tests exercise serial production entry points. They do not qualify
// parallel Syncers or external writers to the spool directory.
func wiringRequireOwner(t *testing.T, spool *Spool, held bool) {
	t.Helper()
	free := spool.referenceMu.TryLock()
	if free {
		spool.referenceMu.Unlock()
	}
	if free == held {
		t.Fatalf("reference owner free=%v, want held=%v", free, held)
	}
}

func wiringRequireFileOwnerFree(t *testing.T, spool *Spool) {
	t.Helper()
	if !spool.mu.TryLock() {
		t.Fatal("file owner held across external callback")
	}
	spool.mu.Unlock()
}

func wiringSealFixture(t *testing.T) (*env, *sourceRow, ExportFunc, []byte) {
	t.Helper()
	now := time.Unix(1800000000, 0)
	e := newEnv(t, Config{SealAfter: time.Minute, Now: func() time.Time { return now }}, 4096)
	spec := tailCaptureSpec(e.dir)
	src := tailCaptureSource(t, e.store, spec)
	body := []byte("{\"record\":\"owned publication\"}\n")
	export := lifecycleExport(body, []byte("owned-state"))
	if err := e.sy.ordinaryOperation().capture(t.Context(), src, export, -1, nil); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	return e, src, export, body
}

func wiringRequireSealLedger(t *testing.T, e *env, sid int64, body []byte, sealed bool) {
	t.Helper()
	var entries, size, tailSize, acked int64
	var tailAcked, lost bool
	if err := e.store.db.QueryRow(`SELECT entries,size,tail_size,acked,tail_acked,lost FROM devsync_gens WHERE source_id=? AND generation=0`, sid).Scan(&entries, &size, &tailSize, &acked, &tailAcked, &lost); err != nil {
		t.Fatal(err)
	}
	wantEntries, wantTail := int64(0), int64(len(body))
	if sealed {
		wantEntries, wantTail = 1, 0
	}
	// Local sealing changes the tail to empty; the server must still ACK
	// that change. Both fixture states remain unacknowledged before upload.
	if entries != wantEntries || size != int64(len(body)) || tailSize != wantTail || acked != 0 || tailAcked || lost {
		t.Fatalf("ledger entries=%d size=%d tail=%d ack=%d/%v lost=%v", entries, size, tailSize, acked, tailAcked, lost)
	}
	if sealed {
		var ordinal, offset, n int64
		var hash []byte
		if err := e.store.db.QueryRow(`SELECT ordinal,offset,size,hash FROM devsync_manifest WHERE source_id=? AND generation=0`, sid).Scan(&ordinal, &offset, &n, &hash); err != nil {
			t.Fatal(err)
		}
		want := syncproto.Sum(body)
		if ordinal != 0 || offset != 0 || n != int64(len(body)) || !bytes.Equal(hash, want[:]) {
			t.Fatal("sealed manifest differs from literal bytes")
		}
	}
}

func TestSpoolWiringCaptureOwnsPublicationThroughCommit(t *testing.T) {
	e, src, export, body := wiringSealFixture(t)
	hash := syncproto.Sum(body)
	reader, err := sql.Open("sqlite", "file:"+filepath.Join(e.dir, "sync.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	attempting := make(chan struct{})
	entered := make(chan struct{})
	cleanup := make(chan error, 1)
	commit := func(ctx context.Context, src *sourceRow, g *genRow, add []syncproto.Entry, wm *transcript.Watermark, state []byte, guards ...func() error) error {
		wiringRequireOwner(t, e.spool, true)
		got, ok, err := e.spool.Chunk(hash)
		if err != nil || !ok || !bytes.Equal(got, body) {
			t.Fatalf("chunk publication: %v %v", ok, err)
		}
		wiringRequireSealLedger(t, e, src.ID, body, false)
		go func() {
			close(attempting)
			cleanup <- e.spool.withReferences(e.store, func(r *spoolReferenceScope) error {
				close(entered)
				return r.releaseChunks(ctx, []syncproto.Hash{hash})
			})
		}()
		<-attempting
		// TryLock establishes that the competing cleanup cannot enter its callback;
		// no sleep or scheduler delay is used as evidence of ownership.
		wiringRequireOwner(t, e.spool, true)
		select {
		case <-entered:
			t.Fatal("cleanup entered before capture commit")
		default:
		}
		guards = append(guards, func() error {
			wiringRequireOwner(t, e.spool, true)
			var pendingManifest int
			if err := reader.QueryRowContext(ctx, `SELECT count(*) FROM devsync_manifest WHERE source_id=? AND generation=0`, src.ID).Scan(&pendingManifest); err != nil {
				return err
			}
			if pendingManifest != 0 {
				return fmt.Errorf("uncommitted publication visible: %d", pendingManifest)
			}
			return nil
		})
		return e.store.saveCapture(ctx, src, g, add, wm, state, guards...)
	}
	if err := e.sy.ordinaryOperation().captureWithCommit(t.Context(), src, export, -1, nil, commit); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-cleanup:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not finish after owner release")
	}
	wiringRequireSealLedger(t, e, src.ID, body, true)
	got, ok, err := e.spool.Chunk(hash)
	if err != nil || !ok || !bytes.Equal(got, body) {
		t.Fatalf("pending chunk removed: %v %v", ok, err)
	}
	wiringRequireOwner(t, e.spool, false)
}

func TestSpoolWiringFailedAndUncertainCapturePreserveBytes(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed=%v", committed), func(t *testing.T) {
			e, src, export, body := wiringSealFixture(t)
			stop := errors.New("capture boundary stopped")
			commit := func(ctx context.Context, src *sourceRow, g *genRow, add []syncproto.Entry, wm *transcript.Watermark, state []byte, guards ...func() error) error {
				wiringRequireOwner(t, e.spool, true)
				if !committed {
					guards = append(guards, func() error { wiringRequireOwner(t, e.spool, true); return stop })
				}
				if err := e.store.saveCapture(ctx, src, g, add, wm, state, guards...); err != nil {
					return err
				}
				return stop
			}
			if err := e.sy.ordinaryOperation().captureWithCommit(t.Context(), src, export, -1, nil, commit); !errors.Is(err, stop) {
				t.Fatalf("capture: %v", err)
			}
			wiringRequireSealLedger(t, e, src.ID, body, committed)
			hash := syncproto.Sum(body)
			tail, ok, err := e.spool.TailVersion(src.ID, 0, hash)
			if err != nil || !ok || !bytes.Equal(tail, body) {
				t.Fatalf("old tail removed: %v %v", ok, err)
			}
			chunk, ok, err := e.spool.Chunk(hash)
			if err != nil || !ok || !bytes.Equal(chunk, body) {
				t.Fatalf("published chunk removed: %v %v", ok, err)
			}
			if e.spool.Used() != 2*int64(len(body)) {
				t.Fatalf("retained accounting=%d", e.spool.Used())
			}
			wiringRequireOwner(t, e.spool, false)
		})
	}
}

func TestSpoolWiringExporterAndTransportRunWithoutOwner(t *testing.T) {
	e := newEnv(t, Config{SealAfter: -1, HasThreshold: 1}, 1<<20)
	var has, flush atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wiringRequireOwner(t, e.spool, false)
		wiringRequireFileOwnerFree(t, e.spool)
		if strings.HasSuffix(r.URL.Path, "/has") {
			has.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/flush") {
			flush.Add(1)
		}
		e.srv.ServeHTTP(w, r)
	}))
	defer server.Close()
	e.client.Server = server.URL
	body := bytes.Repeat([]byte("{\"record\":\"transport owns no spool references\"}\n"), 1000)
	spec := tailCaptureSpec(e.dir)
	exports := 0
	fn := func(context.Context, []byte) (Export, error) {
		wiringRequireOwner(t, e.spool, false)
		wiringRequireFileOwnerFree(t, e.spool)
		exports++
		return Export{Data: body, State: []byte("network-state")}, nil
	}
	if err := e.sy.SyncExportFunc(t.Context(), spec, fn); err != nil {
		t.Fatal(err)
	}
	if exports == 0 || has.Load() == 0 || flush.Load() == 0 {
		t.Fatalf("missing path coverage exporter=%d has=%d flush=%d", exports, has.Load(), flush.Load())
	}
	e.requireServerHas(spec.Path, "", 0, body)
}

func TestSpoolWiringTailPlanningQueriesOutsideFileOwner(t *testing.T) {
	for _, sweep := range []bool{false, true} {
		t.Run(fmt.Sprintf("sweep=%v", sweep), func(t *testing.T) {
			e, src, _, body := wiringSealFixture(t)
			orphan := []byte("{\"record\":\"obsolete\"}\n")
			if err := e.spool.PutTailVersion(src.ID, 0, syncproto.Sum(orphan), orphan); err != nil {
				t.Fatal(err)
			}
			calls := 0
			required := func(sid, gen int64) (syncproto.Tail, bool, error) {
				wiringRequireOwner(t, e.spool, true)
				if !e.spool.mu.TryLock() {
					return syncproto.Tail{}, false, errors.New("reference query under file owner")
				}
				e.spool.mu.Unlock()
				calls++
				return e.store.requiredTail(t.Context(), sid, gen)
			}
			err := e.spool.withReferences(e.store, func(*spoolReferenceScope) error {
				if sweep {
					return e.spool.sweepTails(t.Context(), required)
				}
				return e.spool.reconcileTailVersions(t.Context(), src.ID, required)
			})
			if err != nil {
				t.Fatal(err)
			}
			if calls == 0 {
				t.Fatal("no reference queries")
			}
			got, ok, err := e.spool.TailVersion(src.ID, 0, syncproto.Sum(body))
			if err != nil || !ok || !bytes.Equal(got, body) {
				t.Fatalf("required tail removed: %v %v", ok, err)
			}
			if _, ok, err := e.spool.TailVersion(src.ID, 0, syncproto.Sum(orphan)); err != nil || ok {
				t.Fatalf("orphan retained: %v %v", ok, err)
			}
			if e.spool.Used() != int64(len(body)) {
				t.Fatalf("charged bytes=%d", e.spool.Used())
			}
		})
	}
}

func TestSpoolWiringReleaseUsesDurableAndVerifiedReferences(t *testing.T) {
	e, src, export, body := wiringSealFixture(t)
	if err := e.sy.ordinaryOperation().capture(t.Context(), src, export, -1, nil); err != nil {
		t.Fatal(err)
	}
	g := lifecycleGeneration(t, e, src)
	hash := syncproto.Sum(body)
	// Mutating the caller's ACK snapshot does not authorize deleting the
	// committed pending chunk. The wrapper must consult SQLite while owned.
	g.Acked = 1
	e.sy.release(t.Context(), src, g, []syncproto.Hash{hash})
	got, ok, err := e.spool.Chunk(hash)
	if err != nil || !ok || !bytes.Equal(got, body) {
		t.Fatalf("in-memory ACK deleted pending chunk: %v %v", ok, err)
	}
	if err := e.store.updateGen(t.Context(), g, nil); err != nil {
		t.Fatal(err)
	}
	corrupt := bytes.Repeat([]byte("!"), len(body))
	if err := os.WriteFile(e.spool.chunkPath(hash), corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	e.sy.release(t.Context(), src, g, []syncproto.Hash{hash})
	got, ok, err = e.spool.Chunk(hash)
	if err != nil || !ok || !bytes.Equal(got, corrupt) || e.spool.Used() != int64(len(body)) {
		t.Fatalf("unverified cleanup changed chunk/accounting: found=%v err=%v used=%d", ok, err, e.spool.Used())
	}
	if err := os.WriteFile(e.spool.chunkPath(hash), body, 0600); err != nil {
		t.Fatal(err)
	}
	e.sy.release(t.Context(), src, g, []syncproto.Hash{hash})
	if _, ok, err = e.spool.Chunk(hash); err != nil || ok || e.spool.Used() != 0 {
		t.Fatalf("verified ACK cleanup: found=%v err=%v used=%d", ok, err, e.spool.Used())
	}
}

func TestSpoolWiringCutFailurePreservesPendingFiles(t *testing.T) {
	e, src, export, body := wiringSealFixture(t)
	if err := e.sy.ordinaryOperation().capture(t.Context(), src, export, -1, nil); err != nil {
		t.Fatal(err)
	}
	hash := syncproto.Sum(body)
	if _, err := e.store.db.Exec(`CREATE TRIGGER reject_wiring_cut BEFORE UPDATE ON devsync_gens BEGIN SELECT RAISE(ABORT,'cut rejected'); END`); err != nil {
		t.Fatal(err)
	}
	if err := e.sy.cut(t.Context(), src, lifecycleGeneration(t, e, src), 0); err == nil {
		t.Fatal("cut guard did not reject")
	}
	wiringRequireSealLedger(t, e, src.ID, body, true)
	got, ok, err := e.spool.Chunk(hash)
	if err != nil || !ok || !bytes.Equal(got, body) || e.spool.Used() != int64(len(body)) {
		t.Fatalf("failed cut removed pending bytes: %v %v used=%d", ok, err, e.spool.Used())
	}
	if _, err := e.store.db.Exec(`DROP TRIGGER reject_wiring_cut`); err != nil {
		t.Fatal(err)
	}
	if err := e.sy.cut(t.Context(), src, lifecycleGeneration(t, e, src), 0); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := e.spool.Chunk(hash); err != nil || ok || e.spool.Used() != 0 {
		t.Fatalf("committed cut cleanup: %v %v used=%d", ok, err, e.spool.Used())
	}
	wiringRequireOwner(t, e.spool, false)
}
