package devicesync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

type turnRecordedFlush struct {
	path       string
	generation int64
}
type turnRecordingTransport struct {
	syncproto.Transport
	requests          []turnRecordedFlush
	active, maxActive int
	before            func(context.Context, *syncproto.FlushRequest)
}

func (r *turnRecordingTransport) Flush(ctx context.Context, req *syncproto.FlushRequest) (*syncproto.FlushResponse, error) {
	r.active++
	defer func() { r.active-- }()
	r.maxActive = max(r.maxActive, r.active)
	r.requests = append(r.requests, turnRecordedFlush{req.Header.Source.Path, req.Header.Generation})
	if r.before != nil {
		r.before(ctx, req)
	}
	return r.Transport.Flush(ctx, req)
}

func TestSchedulerTurnsAlternateSourcesAfterFullCapture(t *testing.T) {
	e := newEnv(t, Config{MaxRequestBytes: 16 << 10, SealAfter: -1}, 1<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{})
	a := e.spec("large.jsonl", transcript.StorageJSONLAppend)
	b := SourceSpec{Path: "devin:synthetic.db#small", Agent: transcript.AgentDevin, StorageKind: transcript.StorageSQLite, Parser: "devin-export@2", SessionKey: "small", Export: true}
	large := jsonlLines(120, 250, 300)
	smallData := jsonlLines(121, 5, 100)
	appendFile(t, a.Path, large)
	tr := &turnRecordingTransport{Transport: e.client}
	e.sy.tr = tr
	tr.before = func(ctx context.Context, req *syncproto.FlushRequest) {
		if len(tr.requests) != 1 {
			return
		}
		src, err := e.store.source(ctx, a.Path, nil)
		if err != nil {
			t.Fatal(err)
		}
		g, err := e.store.gen(ctx, src.ID, src.Gen)
		if err != nil {
			t.Fatal(err)
		}
		if src.Watermark == nil || src.Watermark.Offset != int64(len(large)) || g.Size != int64(len(large)) || g.Entries <= int64(len(req.Header.Entries)) {
			t.Fatalf("first upload occurred before complete capture: watermark=%+v generation=%+v batch=%d", src.Watermark, g, len(req.Header.Entries))
		}
	}
	sc.due(a.Path, &job{spec: a, lane: changedLane})
	sc.due(b.Path, &job{spec: b, lane: changedLane, exportFn: func(context.Context, []byte) (Export, error) { return Export{Data: smallData}, nil }})
	sc.runOnce(t.Context())
	if len(tr.requests) < 3 || tr.requests[0].path != a.Path || tr.requests[1].path != b.Path || tr.requests[2].path != a.Path {
		t.Fatalf("source turn order: %+v", tr.requests)
	}
	if tr.maxActive != 1 || sc.Progress().Queued != 0 {
		t.Fatalf("serial turns did not drain: concurrent=%d status=%+v", tr.maxActive, sc.Progress())
	}
	e.requireServerHas(a.Path, fileIDOf(t, a.Path), 0, large)
	e.requireServerHas(b.Path, "", 0, smallData)
}

func TestSchedulerTurnKeepsNewerExporterNotification(t *testing.T) {
	e := newEnv(t, Config{MaxRequestBytes: 16 << 10, SealAfter: -1}, 2<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{Document: Cadence{Debounce: time.Hour, MaxWait: time.Hour}})
	sp := SourceSpec{Path: "devin:synthetic.db#changing", Agent: transcript.AgentDevin, StorageKind: transcript.StorageSQLite, Parser: "devin-export@2", SessionKey: "changing", Export: true}
	oldData := jsonlLines(122, 160, 300)
	newData := jsonlLines(123, 180, 300)
	oldCalls, newCalls := 0, 0
	oldFn := func(context.Context, []byte) (Export, error) { oldCalls++; return Export{Data: oldData}, nil }
	newFn := func(context.Context, []byte) (Export, error) { newCalls++; return Export{Data: newData}, nil }
	newer := sp
	newer.Checkout = "/synthetic/new-checkout"
	tr := &turnRecordingTransport{Transport: e.client}
	e.sy.tr = tr
	tr.before = func(_ context.Context, _ *syncproto.FlushRequest) {
		if len(tr.requests) != 1 {
			return
		}
		sc.NotifyExportFunc(newer, newFn)
		sc.Flush(newer)
		sc.mu.Lock()
		defer sc.mu.Unlock()
		if len(sc.ready) != 1 || sc.ready[sp.Path] == nil || sc.ready[sp.Path].spec.Checkout != newer.Checkout || sc.ready[sp.Path].exportFn == nil {
			t.Fatal("new exporter notification lost while old turn runs")
		}
	}
	sc.due(sp.Path, &job{spec: sp, exportFn: oldFn})
	sc.runOnce(t.Context())
	if oldCalls != 1 || newCalls == 0 || sc.Progress().Queued != 0 {
		t.Fatalf("continuation replaced newer exporter: old=%d new=%d status=%+v", oldCalls, newCalls, sc.Progress())
	}
	src, err := e.store.source(t.Context(), sp.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if src.Spec.Checkout != newer.Checkout || src.Gen != 1 {
		t.Fatalf("latest exporter association: %+v", src)
	}
	e.requireServerHas(sp.Path, "", 0, oldData)
	e.requireServerHas(sp.Path, "", 1, newData)
	if tr.maxActive != 1 {
		t.Fatalf("concurrent manifests=%d", tr.maxActive)
	}
}

func TestSchedulerTurnReacquiresAuthorizationAndHoldsDeniedContinuation(t *testing.T) {
	e := newEnv(t, Config{MaxRequestBytes: 16 << 10, SealAfter: -1}, 1<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{BackoffMin: time.Hour, BackoffMax: time.Hour})
	a := authorizedSpec(e, "protected-large.jsonl", transcript.StorageJSONLAppend)
	a.SessionKey = "protected-owner"
	b := e.spec("ordinary-small.jsonl", transcript.StorageJSONLAppend)
	large := jsonlLines(124, 250, 300)
	smallData := jsonlLines(125, 5, 100)
	appendFile(t, a.Path, large)
	appendFile(t, b.Path, smallData)
	acquires, releases, active, errorCalls := 0, 0, 0, 0
	deny := true
	sc.SetAuthorize(func(_ context.Context, sp SourceSpec) (*CaptureAuthorization, error) {
		if sp.Path != a.Path {
			return nil, nil
		}
		if active != 0 {
			t.Fatal("next turn acquired before previous lease release")
		}
		acquires++
		active++
		auth := fileAuthorization(t, sp, int64(len(large)))
		released := false
		auth.Release = func() {
			if released {
				t.Fatal("lease released twice")
			}
			released = true
			active--
			releases++
		}
		auth.OnError = func(context.Context, error) {
			if released {
				t.Fatal("error callback after release")
			}
			errorCalls++
		}
		if deny && acquires == 2 {
			return auth, errors.New("synthetic policy denial between turns")
		}
		return auth, nil
	})
	tr := &turnRecordingTransport{Transport: e.client}
	e.sy.tr = tr
	sc.due(a.Path, &job{spec: a, lane: changedLane})
	sc.due(b.Path, &job{spec: b, lane: changedLane})
	sc.runOnce(t.Context())
	if acquires != 2 || releases != 2 || active != 0 || errorCalls != 1 {
		t.Fatalf("turn leases acquires=%d releases=%d active=%d errors=%d", acquires, releases, active, errorCalls)
	}
	if len(tr.requests) != 2 || tr.requests[0].path != a.Path || tr.requests[1].path != b.Path {
		t.Fatalf("denied continuation sent a manifest: %+v", tr.requests)
	}
	src, err := e.store.source(t.Context(), a.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := e.store.pendingGens(t.Context(), src.ID)
	if err != nil || len(pending) != 1 || pending[0].Acked >= pending[0].Entries {
		t.Fatalf("denial lost captured pending bytes: %+v,%v", pending, err)
	}
	if st := sc.Progress(); st.Queued != 1 || len(st.Failing) != 1 || st.Failing[0].Path != a.Path {
		t.Fatalf("denial held wrong source: %+v", st)
	}
	deny = false
	sc.Flush(a)
	sc.runOnce(t.Context())
	if acquires <= 2 || acquires != releases || active != 0 || errorCalls != 1 || sc.Progress().Queued != 0 {
		t.Fatalf("fresh authorization did not resume turns: acquires=%d releases=%d active=%d errors=%d status=%+v", acquires, releases, active, errorCalls, sc.Progress())
	}
	if acquires != len(tr.requests) {
		t.Fatalf("protected turn reused a lease: acquisitions=%d manifests=%d (one ordinary manifest and one denied acquisition cancel)", acquires, len(tr.requests))
	}
	e.requireServerHas(a.Path, fileIDOf(t, a.Path), 0, large)
	e.requireServerHas(b.Path, fileIDOf(t, b.Path), 0, smallData)
}
