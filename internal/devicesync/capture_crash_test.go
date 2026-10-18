package devicesync

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/syncproto/synctest"
	"github.com/flopwire/flopwire/internal/transcript"
)

// Literal fixture bytes are independent of the chunker/parser. This tests
// Store/Spool primitives and process death, not the full capture loop or power loss.
var captureCrashH = []byte("shared pending chunk H\n")
var captureCrashJ = []byte("new pending chunk J\n")

func captureCrashSpec(dir, name string) SourceSpec {
	return SourceSpec{Path: filepath.Join(dir, name+".jsonl"), Agent: transcript.AgentCodex, StorageKind: transcript.StorageJSONLAppend, Parser: "codex@1", Export: true}
}

func captureCrashGeneration(src *sourceRow, bodies ...[]byte) (*genRow, []syncproto.Entry, *transcript.Watermark) {
	g := &genRow{SourceID: src.ID, Gen: 0, CapturedAt: time.Unix(1700000000, 0).UnixNano(), Entries: int64(len(bodies)), TailAcked: true}
	var entries []syncproto.Entry
	for i, body := range bodies {
		entries = append(entries, syncproto.Entry{Ordinal: int64(i), Hash: syncproto.Sum(body), Offset: g.Size, Size: int64(len(body))})
		g.Size += int64(len(body))
	}
	g.Tail.Offset = g.Size
	return g, entries, &transcript.Watermark{Offset: g.Size, LineNo: int64(len(bodies))}
}

func TestSaveCaptureCrashHelper(t *testing.T) {
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
	ctx := context.Background()
	aSpec, bSpec := captureCrashSpec(dir, "a"), captureCrashSpec(dir, "b")
	a, err := store.source(ctx, aSpec.Path, &aSpec)
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.source(ctx, bSpec.Path, &bSpec)
	if err != nil {
		t.Fatal(err)
	}
	if err := spool.PutChunk(syncproto.Sum(captureCrashH), captureCrashH); err != nil {
		t.Fatal(err)
	}
	g, entries, wm := captureCrashGeneration(a, captureCrashH)
	if err := store.saveCapture(ctx, a, g, entries, wm, nil); err != nil {
		t.Fatal(err)
	}
	// A already durably needs H. B publishes the duplicate H and unique J
	// before its manifest transaction starts, exactly as capture does.
	if err := spool.PutChunk(syncproto.Sum(captureCrashH), captureCrashH); err != nil {
		t.Fatal(err)
	}
	if err := spool.PutChunk(syncproto.Sum(captureCrashJ), captureCrashJ); err != nil {
		t.Fatal(err)
	}
	signal, control := os.NewFile(3, "capture-barrier"), os.NewFile(4, "capture-control")
	defer signal.Close()
	defer control.Close()
	barrier := func() error {
		if _, err := fmt.Fprintln(signal, phase); err != nil {
			return err
		}
		// Parent keeps this pipe open until SIGKILL. EOF is not permission
		// to continue and accidentally commit the pre-commit case.
		_, err := io.Copy(io.Discard, control)
		if err != nil {
			return err
		}
		return errors.New("crash barrier released without process kill")
	}
	g, entries, wm = captureCrashGeneration(b, captureCrashH, captureCrashJ)
	var guards []func() error
	if phase == "before" {
		guards = append(guards, barrier)
	}
	if err := store.saveCapture(ctx, b, g, entries, wm, nil, guards...); err != nil {
		t.Fatal(err)
	}
	if phase == "after" {
		if err := barrier(); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("crash helper passed its requested barrier")
}

func killCaptureHelperAtBarrier(t *testing.T, dir, phase string, check func()) {
	t.Helper()
	killNamedCaptureHelperAtBarrier(t, dir, phase, "TestSaveCaptureCrashHelper", check)
}

func killNamedCaptureHelperAtBarrier(t *testing.T, dir, phase, helper string, check func()) {
	t.Helper()
	signalRead, signalWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		signalRead.Close()
		signalWrite.Close()
		t.Fatal(err)
	}
	defer signalRead.Close()
	defer signalWrite.Close()
	defer controlRead.Close()
	defer controlWrite.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^"+helper+"$", "-test.count=1")
	cmd.Env = append(os.Environ(), "FLOPWIRE_CAPTURE_CRASH_CHILD=1", "FLOPWIRE_CAPTURE_CRASH_DIR="+dir, "FLOPWIRE_CAPTURE_CRASH_PHASE="+phase)
	cmd.ExtraFiles = []*os.File{signalWrite, controlRead}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	signalWrite.Close()
	controlRead.Close()
	var reap sync.Once
	var waitErr error
	finish := func() { reap.Do(func() { _ = cmd.Process.Kill(); waitErr = cmd.Wait() }) }
	defer finish() // Fatal checks also kill and reap; never leave a blocked helper.
	barrier := make(chan string, 1)
	go func() {
		line, err := bufio.NewReader(signalRead).ReadString('\n')
		if err != nil {
			barrier <- "read failure: " + err.Error()
			return
		}
		barrier <- line
	}()
	select {
	case got := <-barrier:
		if got != phase+"\n" {
			finish()
			t.Fatalf("capture barrier missing: got %q; helper %s", got, output.String())
		}
	case <-time.After(15 * time.Second):
		finish()
		t.Fatalf("capture barrier %s timed out; helper %s", phase, output.String())
	}
	check() // Verify the boundary while the child remains blocked, before kill.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill at capture barrier: %v", err)
	}
	finish()
	var exit *exec.ExitError
	if !errors.As(waitErr, &exit) || exit.ExitCode() != -1 {
		t.Fatalf("helper did not die by signal: %v; output %s", waitErr, output.String())
	}
}

func requireCaptureLedger(t *testing.T, db *sql.DB, dir string, bCommitted, aAcked, bAcked bool) {
	t.Helper()
	for _, name := range []string{"a", "b"} {
		spec := captureCrashSpec(dir, name)
		var current, generation, size, entries, acked, tailSize, tailOffset int64
		var tailAcked, lost, closed bool
		var watermark sql.NullString
		var count int
		if err := db.QueryRow(`SELECT generation,watermark FROM devsync_sources WHERE path=?`, spec.Path).Scan(&current, &watermark); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT count(*) FROM devsync_gens g JOIN devsync_sources s ON s.id=g.source_id WHERE s.path=?`, spec.Path).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if name == "b" && !bCommitted {
			if current != -1 || count != 0 || watermark.Valid {
				t.Fatalf("uncommitted B survived: current=%d generations=%d", current, count)
			}
			var manifests int
			if err := db.QueryRow(`SELECT count(*) FROM devsync_manifest m JOIN devsync_sources s ON s.id=m.source_id WHERE s.path=?`, spec.Path).Scan(&manifests); err != nil {
				t.Fatal(err)
			}
			if manifests != 0 {
				t.Fatal("uncommitted B manifest survived")
			}
			continue
		}
		if err := db.QueryRow(`SELECT g.generation,g.size,g.entries,g.acked,g.tail_offset,g.tail_size,g.tail_acked,g.lost,g.closed FROM devsync_gens g JOIN devsync_sources s ON s.id=g.source_id WHERE s.path=?`, spec.Path).Scan(&generation, &size, &entries, &acked, &tailOffset, &tailSize, &tailAcked, &lost, &closed); err != nil {
			t.Fatal(err)
		}
		wantSize, wantEntries := int64(len(captureCrashH)), int64(1)
		wantAck := int64(0)
		if name == "b" {
			wantSize += int64(len(captureCrashJ))
			wantEntries = 2
			if bAcked {
				wantAck = 2
			}
		} else if aAcked {
			wantAck = 1
		}
		if count != 1 || current != 0 || generation != 0 || size != wantSize || entries != wantEntries || acked != wantAck || tailOffset != wantSize || tailSize != 0 || !tailAcked || lost || closed {
			t.Fatalf("%s ledger current=%d generations=%d gen=%d size=%d entries=%d ack=%d tail=%d/%d/%v lost=%v", name, current, count, generation, size, entries, acked, tailOffset, tailSize, tailAcked, lost)
		}
		var wm storedWatermark
		if !watermark.Valid {
			t.Fatal("committed watermark missing")
		}
		if err := json.Unmarshal([]byte(watermark.String), &wm); err != nil {
			t.Fatal(err)
		}
		if wm.Offset != wantSize || wm.LineNo != wantEntries {
			t.Fatalf("%s watermark offset=%d line=%d", name, wm.Offset, wm.LineNo)
		}
		bodies := [][]byte{captureCrashH}
		if name == "b" {
			bodies = append(bodies, captureCrashJ)
		}
		rows, err := db.Query(`SELECT m.generation,m.ordinal,m.hash,m.offset,m.size FROM devsync_manifest m JOIN devsync_sources s ON s.id=m.source_id JOIN devsync_gens g ON g.source_id=m.source_id AND g.generation=m.generation WHERE s.path=? ORDER BY m.ordinal`, spec.Path)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		var expectedOffset int64
		for rows.Next() {
			var gen, ordinal, offset, length int64
			var hash []byte
			if err := rows.Scan(&gen, &ordinal, &hash, &offset, &length); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			if n >= len(bodies) {
				rows.Close()
				t.Fatal("extra manifest entry")
			}
			wantHash := syncproto.Sum(bodies[n])
			if gen != 0 || ordinal != int64(n) || offset != expectedOffset || length != int64(len(bodies[n])) || !bytes.Equal(hash, wantHash[:]) {
				rows.Close()
				t.Fatalf("%s manifest entry %d changed", name, n)
			}
			expectedOffset += length
			n++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
		if n != len(bodies) {
			t.Fatalf("%s manifest count=%d want=%d", name, n, len(bodies))
		}
	}
}

func requireCaptureChunk(t *testing.T, spool *Spool, body []byte, present bool) {
	t.Helper()
	got, ok, err := spool.Chunk(syncproto.Sum(body))
	if err != nil || ok != present || (present && !bytes.Equal(got, body)) {
		t.Fatalf("literal chunk present=%v want=%v err=%v", ok, present, err)
	}
}

func TestSaveCaptureProcessKillPreservesPendingSharedChunks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess pipe descriptors require Unix")
	}
	for _, phase := range []string{"before", "after"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			committed := phase == "after"
			killCaptureHelperAtBarrier(t, dir, phase, func() {
				// A separate read-only connection sees only committed WAL state.
				db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "sync.db")+"?mode=ro")
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				requireCaptureLedger(t, db, dir, committed, false, false)
				spool, err := OpenSpool(filepath.Join(dir, "spool"), 1<<20)
				if err != nil {
					t.Fatal(err)
				}
				requireCaptureChunk(t, spool, captureCrashH, true)
				requireCaptureChunk(t, spool, captureCrashJ, true)
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
			srv := synctest.New("synthetic-token")
			http := httptest.NewServer(srv)
			defer http.Close()
			client := &syncproto.Client{Server: http.URL, Token: "synthetic-token", HTTP: http.Client()}
			sy, err := NewSyncer(Config{Chunk: small}, store, spool, client)
			if err != nil {
				t.Fatal(err)
			}
			defer sy.Close()
			requireCaptureLedger(t, store.db, dir, committed, false, false)
			requireCaptureChunk(t, spool, captureCrashH, true)
			requireCaptureChunk(t, spool, captureCrashJ, committed)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := sy.Resume(ctx, captureCrashSpec(dir, "a")); err != nil {
				t.Fatal(err)
			}
			got, err := srv.Reconstruct(captureCrashSpec(dir, "a").Path, "", 0)
			if err != nil || !bytes.Equal(got, captureCrashH) {
				t.Fatalf("original A generation bytes differ: %v", err)
			}
			requireCaptureLedger(t, store.db, dir, committed, true, false)
			requireCaptureChunk(t, spool, captureCrashH, committed)
			if committed {
				requireCaptureChunk(t, spool, captureCrashJ, true)
				if err := sy.Resume(ctx, captureCrashSpec(dir, "b")); err != nil {
					t.Fatal(err)
				}
				want := append(append([]byte(nil), captureCrashH...), captureCrashJ...)
				got, err := srv.Reconstruct(captureCrashSpec(dir, "b").Path, "", 0)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("original B generation bytes differ: %v", err)
				}
				requireCaptureLedger(t, store.db, dir, true, true, true)
			}
			requireCaptureChunk(t, spool, captureCrashH, false)
			requireCaptureChunk(t, spool, captureCrashJ, false)
			if spool.Used() != 0 {
				t.Fatalf("spool not released after durable ACKs: %d", spool.Used())
			}
		})
	}
}
