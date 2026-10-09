package devicesync

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

func drainTurns(t *testing.T, e *env, sp SourceSpec) {
	t.Helper()
	action := captureSource
	for range 1000 {
		before := e.flushes()
		pending, err := e.sy.syncTurn(t.Context(), sp, nil, -1, nil, action)
		if err != nil {
			t.Fatal(err)
		}
		if e.flushes()-before > 1 {
			t.Fatal("turn dispatched multiple manifests")
		}
		if pending == syncDone {
			return
		}
		action = captureSource
		if pending == uploadPending {
			action = resumeUpload
		}
	}
	t.Fatal("turns did not finish")
}

func restartTurnEnv(t *testing.T, e *env) {
	t.Helper()
	cfg, capBytes := e.sy.cfg, e.spool.cap
	e.sy.Close()
	if err := e.store.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	e.store, err = OpenStore(e.path("sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	e.spool, err = OpenSpool(e.path("spool"), capBytes)
	if err != nil {
		t.Fatal(err)
	}
	e.sy, err = NewSyncer(cfg, e.store, e.spool, e.client)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.sy.Close)
}

func TestTurnRejectedCurrentDurablyRecapturedBeforeYield(t *testing.T) {
	e := newEnv(t, Config{MaxRequestBytes: 16 << 10, SealAfter: -1}, 1<<20)
	sp := e.spec("generation.jsonl", transcript.StorageJSONLAppend)
	data := jsonlLines(142, 200, 200)
	appendFile(t, sp.Path, data)
	e.sync(sp)
	copy(data, bytes.ToUpper(data[:len(data)/2]))
	if err := os.WriteFile(sp.Path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.db.Exec(`DELETE FROM devsync_sources; DELETE FROM devsync_gens; DELETE FROM devsync_manifest`); err != nil {
		t.Fatal(err)
	}
	before := e.flushes()
	pending, err := e.sy.syncTurn(t.Context(), sp, nil, -1, nil, captureSource)
	if err != nil || pending == syncDone || e.flushes() != before+1 {
		t.Fatalf("pending=%v err=%v requests=%d", pending, err, e.flushes()-before)
	}
	src, err := e.store.source(t.Context(), sp.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	gens, err := e.store.pendingGens(t.Context(), src.ID)
	if err != nil || len(gens) != 1 || gens[0].Gen != 1 || gens[0].Size != int64(len(data)) {
		t.Fatalf("recapture=%+v err=%v", gens, err)
	}
	// Recreate both the syncer and spool; no old held descriptor survives.
	restartTurnEnv(t, e)
	specs, err := e.store.PendingSpecs(t.Context())
	if err != nil || len(specs) != 1 || specs[0].Path != sp.Path {
		t.Fatalf("restart pending=%+v err=%v", specs, err)
	}
	drainTurns(t, e, sp)
	e.requireServerHas(sp.Path, fileIDOf(t, sp.Path), 1, data)
}

type rejectResumeTransport struct {
	syncproto.Transport
	calls int
}

func (r *rejectResumeTransport) Flush(ctx context.Context, req *syncproto.FlushRequest) (*syncproto.FlushResponse, error) {
	r.calls++
	if r.calls == 2 {
		if req.Payload != nil {
			if _, err := io.Copy(io.Discard, req.Payload); err != nil {
				return nil, err
			}
		}
		return &syncproto.FlushResponse{Status: syncproto.StatusNewGeneration, Generation: req.Header.Generation}, nil
	}
	return r.Transport.Flush(ctx, req)
}

func TestTurnRejectedUploadContinuationDurablyRecaptures(t *testing.T) {
	e := newEnv(t, Config{MaxRequestBytes: 16 << 10, SealAfter: -1}, 1<<20)
	sp := e.spec("resume-rejected.jsonl", transcript.StorageJSONLAppend)
	data := jsonlLines(143, 300, 200)
	appendFile(t, sp.Path, data)
	tr := &rejectResumeTransport{Transport: e.client}
	e.sy.tr = tr
	outcome, err := e.sy.syncTurn(t.Context(), sp, nil, -1, nil, captureSource)
	if err != nil || outcome != uploadPending {
		t.Fatalf("first outcome=%v err=%v", outcome, err)
	}
	outcome, err = e.sy.syncTurn(t.Context(), sp, nil, -1, nil, resumeUpload)
	if err != nil || outcome != uploadPending || tr.calls != 2 {
		t.Fatalf("rejection outcome=%v err=%v calls=%d", outcome, err, tr.calls)
	}
	src, err := e.store.source(t.Context(), sp.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	gens, err := e.store.pendingGens(t.Context(), src.ID)
	if err != nil || len(gens) != 1 || gens[0].Gen != 1 || gens[0].Size != int64(len(data)) {
		t.Fatalf("replacement=%+v err=%v", gens, err)
	}
	restartTurnEnv(t, e)
	drainTurns(t, e, sp)
	e.requireServerHas(sp.Path, fileIDOf(t, sp.Path), 1, data)
}

func TestTurnSpoolRecoveryUsesSameRequestBudget(t *testing.T) {
	v1, v2 := jsonlLines(151, 150, 200), jsonlLines(152, 150, 200)
	e := newEnv(t, Config{MaxRequestBytes: 16 << 10, SealAfter: -1}, int64(max(len(v1), len(v2))+1024))
	sp := e.spec("document.json", transcript.StorageJSONDoc)
	if err := os.WriteFile(sp.Path, v1, 0600); err != nil {
		t.Fatal(err)
	}
	e.srv.SetDown(true)
	if err := e.sy.Sync(t.Context(), sp); err == nil {
		t.Fatal("expected initial outage")
	}
	if err := os.WriteFile(sp.Path+".new", v2, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(sp.Path+".new", sp.Path); err != nil {
		t.Fatal(err)
	}
	e.srv.SetDown(false)
	before := e.flushes()
	pending, err := e.sy.syncTurn(t.Context(), sp, nil, -1, nil, captureSource)
	if err != nil || pending != capturePending || e.flushes() != before+1 {
		t.Fatalf("pending=%v err=%v manifests=%d", pending, err, e.flushes()-before)
	}
	// v2 still cannot fit, but old v1 remains durable and resumes after reopening.
	restartTurnEnv(t, e)
	specs, err := e.store.PendingSpecs(t.Context())
	if err != nil || len(specs) != 1 {
		t.Fatalf("pending=%v err=%v", specs, err)
	}
	drainTurns(t, e, sp)
	e.requireServerHas(sp.Path, fileIDOf(t, sp.Path), 1, v2)
	if e.spool.Used() != 0 {
		t.Fatalf("spool not released: %d", e.spool.Used())
	}
}

type stalledTurnTransport struct {
	syncproto.Transport
	calls      int
	progressOn int
	notify     func()
}

func (s *stalledTurnTransport) Flush(ctx context.Context, r *syncproto.FlushRequest) (*syncproto.FlushResponse, error) {
	s.calls++
	if s.notify != nil {
		s.notify()
	}
	resp, err := s.Transport.Flush(ctx, r)
	if err != nil {
		return resp, err
	}
	if s.calls != s.progressOn {
		resp.AckedEntries = 0
		if len(r.Header.Entries) > 0 {
			resp.AckedEntries = r.Header.Entries[0].Ordinal
		}
		resp.TailAcked = false
	}
	return resp, nil
}

func TestTurnStallsRemainCumulativeAcrossProgressAndNotifications(t *testing.T) {
	e := newEnv(t, Config{MaxRequestBytes: 16 << 10, SealAfter: -1}, 1<<20)
	sp := e.spec("stalls.jsonl", transcript.StorageJSONLAppend)
	appendFile(t, sp.Path, jsonlLines(160, 500, 200))
	tr := &stalledTurnTransport{Transport: e.client, progressOn: 2}
	e.sy.tr = tr
	// A newer notification on every response must not reset the generation's
	// cumulative stall threshold; progress on response two does not reset it.
	sc := NewScheduler(e.sy, SchedulerConfig{BackoffMin: time.Hour, BackoffMax: time.Hour})
	tr.notify = func() { sc.Notify(sp); sc.Flush(sp) }
	sc.due(sp.Path, &job{spec: sp})
	sc.runOnce(t.Context())
	if tr.calls != 4 {
		t.Fatalf("requests before cumulative stall failure=%d", tr.calls)
	}
	st := sc.Progress()
	if len(st.Failing) != 1 || !strings.Contains(st.Failing[0].Error, "no progress") {
		t.Fatalf("failure=%+v", st)
	}
	// The failure is not permanent poison: a backed-off attempt can progress.
	e.sy.tr = e.client
	sc.Flush(sp)
	sc.runOnce(t.Context())
	if counts := snapshotStalls(&e.sy.stalls); len(counts) != 0 {
		t.Fatalf("completed stall buckets: %v", counts)
	}
}

func TestTurnOversizedVersionWithoutFreeablePendingIsError(t *testing.T) {
	e := newEnv(t, Config{MaxRequestBytes: 16 << 10}, 1024)
	sp := e.spec("too-large.json", transcript.StorageJSONDoc)
	if err := os.WriteFile(sp.Path, jsonlLines(170, 100, 200), 0600); err != nil {
		t.Fatal(err)
	}
	pending, err := e.sy.syncTurn(t.Context(), sp, nil, -1, nil, captureSource)
	if pending != syncDone || !errors.Is(err, ErrSpoolFull) {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
}

func TestTurnTerminalSalvagePrunesStallState(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := e.spec("salvage.jsonl", transcript.StorageJSONLAppend)
	appendFile(t, sp.Path, jsonlLines(171, 100, 200))
	e.srv.SetDown(true)
	if err := e.sy.Sync(t.Context(), sp); err == nil {
		t.Fatal("expected outage")
	}
	src, err := e.store.source(t.Context(), sp.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	g, err := e.store.gen(t.Context(), src.ID, src.Gen)
	if err != nil {
		t.Fatal(err)
	}
	key := stallKey{src.ID, g.Gen}
	e.sy.stalls.record(key)
	e.sy.stalls.record(key)
	if err := e.sy.cut(t.Context(), src, g, g.Acked); err != nil {
		t.Fatal(err)
	}
	if counts := snapshotStalls(&e.sy.stalls); !g.done() || len(counts) != 0 {
		t.Fatalf("terminal salvage retained stalls: done=%v stalls=%v", g.done(), counts)
	}
}

func TestSchedulerTurnsMeasureLazyExportCost(t *testing.T) {
	for _, appendExport := range []bool{false, true} {
		t.Run(map[bool]string{false: "opencode-whole", true: "devin-append"}[appendExport], func(t *testing.T) {
			e := newEnv(t, Config{MaxRequestBytes: 16 << 10, SealAfter: -1}, 2<<20)
			sp := SourceSpec{Path: "synthetic:database#cost", Agent: transcript.AgentOpencode, StorageKind: transcript.StorageSQLite, Parser: "opencode-export@1", Export: true}
			if appendExport {
				sp.Agent, sp.Parser = transcript.AgentDevin, "devin-export@2"
			}
			data := jsonlLines(181, 400, 300)
			calls := 0
			fn := func(_ context.Context, prev []byte) (Export, error) {
				calls++
				if appendExport && len(prev) > 0 {
					return Export{Append: true, State: prev}, nil
				}
				return Export{Data: data, State: []byte("captured-version")}, nil
			}
			sc := NewScheduler(e.sy, SchedulerConfig{})
			sc.due(sp.Path, &job{spec: sp, exportFn: fn})
			sc.runOnce(t.Context())
			stats := e.sy.CaptureStats()
			if e.flushes() < 2 || calls != 1 {
				t.Fatalf("calls=%d manifests=%d", calls, e.flushes())
			}
			want := int64(len(data))
			if !appendExport {
				want *= int64(calls)
			}
			if stats.Exported != want {
				t.Fatalf("exported=%d want=%d", stats.Exported, want)
			}
			t.Logf("manifests=%d exporter_calls=%d version_bytes=%d exported=%d scanned=%d captures=%d", e.flushes(), calls, len(data), stats.Exported, stats.Scanned, stats.Captures)
			e.requireServerHas(sp.Path, "", 0, data)
		})
	}
}
