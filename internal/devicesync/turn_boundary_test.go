package devicesync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

func TestQueuedUploadTurnNewNotificationCapturesFreshVersion(t *testing.T) {
	for _, exported := range []bool{false, true} {
		t.Run(map[bool]string{false: "append", true: "export"}[exported], func(t *testing.T) {
			e := newEnv(t, Config{MaxRequestBytes: 16 << 10, SealAfter: -1}, 2<<20)
			sc := NewScheduler(e.sy, SchedulerConfig{})
			sp := e.spec("notified.jsonl", transcript.StorageJSONLAppend)
			first := jsonlLines(130, 200, 300)
			extra := jsonlLines(131, 30, 300)
			all := append(bytes.Clone(first), extra...)
			var oldFn, newFn ExportFunc
			oldCalls, newCalls := 0, 0
			if exported {
				sp = SourceSpec{Path: "devin:boundary.db#fresh", Agent: transcript.AgentDevin, StorageKind: transcript.StorageSQLite, Parser: "devin-export@2", SessionKey: "fresh", Export: true}
				oldFn = func(context.Context, []byte) (Export, error) { oldCalls++; return Export{Data: first}, nil }
				newFn = func(context.Context, []byte) (Export, error) { newCalls++; return Export{Data: all}, nil }
			} else {
				appendFile(t, sp.Path, first)
			}
			outcome, err := e.sy.syncTurn(t.Context(), sp, oldFn, -1, nil, captureSource)
			if err != nil || outcome != uploadPending {
				t.Fatalf("initial upload turn: %v,%v", outcome, err)
			}
			sc.due(sp.Path, &job{spec: sp, action: resumeUpload, exportFn: oldFn})
			if exported {
				sc.NotifyExportFunc(sp, newFn)
			} else {
				appendFile(t, sp.Path, extra)
				sc.Notify(sp)
			}
			sc.mu.Lock()
			j := sc.ready[sp.Path]
			action := j.action
			queued := len(sc.ready)
			sc.mu.Unlock()
			if action != captureSource || queued != 1 {
				t.Fatalf("fresh notification retained stale upload-only action: action=%v queued=%d", action, queued)
			}
			sc.runOnce(t.Context())
			if sc.Progress().Queued != 0 {
				t.Fatalf("fresh version did not drain: %+v", sc.Progress())
			}
			src, err := e.store.source(t.Context(), sp.Path, nil)
			if err != nil {
				t.Fatal(err)
			}
			fid := ""
			if !exported {
				fid = fileIDOf(t, sp.Path)
			}
			e.requireServerHas(sp.Path, fid, src.Gen, all)
			if exported && (oldCalls != 1 || newCalls != 1) {
				t.Fatalf("upload-only continuation rebuilt export: old=%d new=%d", oldCalls, newCalls)
			}
		})
	}
}

func TestProtectedUploadTurnRejectsMissingCompanionDigestAndSymlinkSwap(t *testing.T) {
	for _, kind := range []string{"missing historical digest", "symlink swap"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t, Config{MaxRequestBytes: 16 << 10, SealAfter: -1}, 1<<20)
			sp := authorizedSpec(e, "protected-tool.txt", transcript.StorageCompanion)
			data := jsonlLines(132, 250, 300)
			appendFile(t, sp.Path, data)
			a := fileAuthorization(t, sp, int64(len(data)))
			sum := sha256.Sum256(data)
			a.Proof.ContentSHA = sum[:]
			tr := &turnRecordingTransport{Transport: e.client}
			e.sy.tr = tr
			outcome, err := e.sy.syncTurn(t.Context(), sp, nil, -1, a, captureSource)
			if err != nil || outcome != uploadPending {
				t.Fatalf("initial protected turn: %v,%v", outcome, err)
			}
			before := len(tr.requests)
			switch kind {
			case "missing historical digest":
				if _, err := e.store.db.Exec(`UPDATE devsync_gens SET capture_proof=json_remove(capture_proof,'$.ContentSHA')`); err != nil {
					t.Fatal(err)
				}
			case "symlink swap":
				outside := e.path("outside-tool.txt")
				appendFile(t, outside, data)
				if err := os.Remove(sp.Path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, sp.Path); err != nil {
					t.Fatal(err)
				}
			}
			_, err = e.sy.syncTurn(t.Context(), sp, nil, -1, a, resumeUpload)
			if err == nil || len(tr.requests) != before {
				t.Fatalf("unsafe companion continuation sent bytes: err=%v before=%d after=%d", err, before, len(tr.requests))
			}
			if kind == "missing historical digest" {
				var unproven *UnprovenCaptureError
				if !errors.As(err, &unproven) || !unproven.Captured {
					t.Fatalf("missing historical digest not held as captured: %v", err)
				}
			}
		})
	}
}

func TestUploadTurnsRespectHeldDescriptorCapAndRepairOrRecordGap(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(map[bool]string{false: "reopen and hash repair", true: "deleted uncached source gap"}[deleted], func(t *testing.T) {
			e := newEnv(t, Config{MaxRequestBytes: 16 << 10, SealAfter: -1, MaxHeldFiles: 1}, 1<<20)
			first := e.spec("first-held.jsonl", transcript.StorageJSONLAppend)
			second := e.spec("second-reopened.jsonl", transcript.StorageJSONLAppend)
			dataA := jsonlLines(133, 200, 300)
			dataB := jsonlLines(134, 200, 300)
			appendFile(t, first.Path, dataA)
			appendFile(t, second.Path, dataB)
			fidA, fidB := fileIDOf(t, first.Path), fileIDOf(t, second.Path)
			for _, sp := range []SourceSpec{first, second} {
				outcome, err := e.sy.syncTurn(t.Context(), sp, nil, -1, nil, captureSource)
				if err != nil || outcome != uploadPending {
					t.Fatalf("initial capped descriptor turn: %v,%v", outcome, err)
				}
				if len(e.sy.held) > 1 {
					t.Fatalf("held descriptors exceeded cap: %d", len(e.sy.held))
				}
			}
			srcB, err := e.store.source(t.Context(), second.Path, nil)
			if err != nil {
				t.Fatal(err)
			}
			originalB, err := e.store.gen(t.Context(), srcB.ID, srcB.Gen)
			if err != nil {
				t.Fatal(err)
			}
			if len(e.sy.held) != 1 || e.sy.held[srcB.ID] != nil {
				t.Fatal("second source bypassed descriptor cap")
			}
			if deleted {
				if err := os.Remove(second.Path); err != nil {
					t.Fatal(err)
				}
			}
			for _, sp := range []SourceSpec{second, first} {
				done := false
				for range 30 {
					outcome, err := e.sy.syncTurn(t.Context(), sp, nil, -1, nil, resumeUpload)
					if err != nil {
						// The first failed reopen invalidates the live watermark;
						// the next turn can record the now-confirmed vanished gap.
						if deleted && sp.Path == second.Path && errors.Is(err, ErrSourceChanged) {
							continue
						}
						t.Fatalf("continuation after descriptor cap: %v", err)
					}
					if len(e.sy.held) > 1 {
						t.Fatalf("continuation exceeded descriptor cap: %d", len(e.sy.held))
					}
					if outcome == syncDone {
						done = true
						break
					}
				}
				if !done {
					t.Fatal("capped descriptor source never completed")
				}
			}
			e.requireServerHas(first.Path, fidA, 0, dataA)
			if !deleted {
				e.requireServerHas(second.Path, fidB, 0, dataB)
			} else {
				g, err := e.store.gen(t.Context(), srcB.ID, srcB.Gen)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(e.logs.String(), "lost before upload") || g.Entries >= originalB.Entries || !g.done() {
					t.Fatalf("deleted source not reported as truthful gap: generation=%+v logs=%s", g, e.logs.String())
				}
				got, err := e.srv.Reconstruct(second.Path, fidB, 0)
				if err != nil {
					t.Fatal(err)
				}
				if len(got) >= len(dataB) || !bytes.Equal(got, dataB[:len(got)]) {
					t.Fatal("deleted source claimed uncaptured remainder")
				}
			}
			if len(e.sy.held) != 0 {
				t.Fatalf("completed turns leaked held descriptors: %d", len(e.sy.held))
			}
		})
	}
}
