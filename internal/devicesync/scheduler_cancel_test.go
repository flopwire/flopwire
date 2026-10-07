package devicesync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

func TestSchedulerCancellationStopsDispatch(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(map[bool]string{false: "filtered", true: "allowed"}[allow], func(t *testing.T) {
			e := newEnv(t, Config{}, 1<<20)
			sc := NewScheduler(e.sy, SchedulerConfig{})
			for _, name := range []string{"first.jsonl", "second.jsonl"} {
				sp := e.spec(name, transcript.StorageJSONLAppend)
				appendFile(t, sp.Path, jsonlLines(9, 10, 100))
				sc.due(sp.Path, &job{spec: sp})
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			filters, authorizations := 0, 0
			sc.SetFilter(func(SourceSpec) bool { filters++; cancel(); return allow })
			sc.SetAuthorize(func(context.Context, SourceSpec) (*CaptureAuthorization, error) {
				authorizations++
				return nil, nil
			})
			sc.runOnce(ctx)
			if filters != 1 || authorizations != 0 || e.sy.CaptureStats().Captures != 0 || e.flushes() != 0 {
				t.Fatalf("continued after cancellation: filters=%d authorization=%d captures=%+v flushes=%d", filters, authorizations, e.sy.CaptureStats(), e.flushes())
			}
			wantQueued := 1
			if allow {
				wantQueued = 2
			}
			if st := sc.Progress(); st.Queued != wantQueued || sc.running != 0 {
				t.Fatalf("later work dispatched or active state retained: %+v", st)
			}
			sc.SetFilter(nil)
			sc.SetAuthorize(nil)
			sc.runOnce(t.Context())
			if e.flushes() != wantQueued || sc.Progress().Queued != 0 {
				t.Fatalf("retained notifications did not resume: flushes=%d queued=%d", e.flushes(), sc.Progress().Queued)
			}
		})
	}
}

func TestSchedulerCancelledRunDoesNotRepin(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	repins := 0
	sc := NewScheduler(e.sy, SchedulerConfig{Repin: func() bool { repins++; return true }})
	sc.halted = errors.New("synthetic pin rejection")
	sc.repinAt = time.Now().Add(-time.Second)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := sc.Run(ctx); !errors.Is(err, context.Canceled) || repins != 0 {
		t.Fatalf("cancelled run: err=%v repins=%d", err, repins)
	}
}

func TestSchedulerCancellationKeepsNewerNotification(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{})
	sp := e.spec("changed.jsonl", transcript.StorageJSONLAppend)
	appendFile(t, sp.Path, jsonlLines(17, 10, 100))
	sc.due(sp.Path, &job{spec: sp})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	newer := sp
	newer.Checkout = "/synthetic/new-checkout"
	sc.SetFilter(func(SourceSpec) bool {
		sc.Flush(newer)
		cancel()
		return true
	})
	sc.runOnce(ctx)
	sc.mu.Lock()
	j := sc.ready[sp.Path]
	if j == nil || j.spec.Checkout != newer.Checkout || len(sc.ready) != 1 {
		sc.mu.Unlock()
		t.Fatal("cancelled dispatch overwrote or duplicated the newer notification")
	}
	sc.mu.Unlock()
	sc.SetFilter(nil)
	sc.runOnce(t.Context())
	if e.flushes() != 1 || sc.Progress().Queued != 0 {
		t.Fatalf("newer notification did not resume: flushes=%d queued=%d", e.flushes(), sc.Progress().Queued)
	}
}

func TestSchedulerProgressDoesNotWaitForCaptureConnection(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{})
	sc.Flush(e.spec("queued.jsonl", transcript.StorageJSONLAppend))
	held, err := e.store.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	before := e.store.db.Stats().WaitCount
	done := make(chan Status, 1)
	go func() { done <- sc.Progress() }()
	select {
	case st := <-done:
		if st.Queued != 1 || st.Redactions != nil {
			t.Fatalf("unexpected progress: %+v", st)
		}
	case <-time.After(time.Second):
		t.Fatal("progress waited for capture's database connection")
	}
	if e.store.db.Stats().WaitCount != before {
		t.Fatal("progress queried optional diagnostics")
	}
}

func TestSchedulerCancelledAuthorizationRetainsNotification(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{})
	sp := e.spec("uncaptured.jsonl", transcript.StorageJSONLAppend)
	appendFile(t, sp.Path, jsonlLines(23, 10, 100))
	sc.due(sp.Path, &job{spec: sp})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sc.SetAuthorize(func(context.Context, SourceSpec) (*CaptureAuthorization, error) {
		cancel()
		return nil, ctx.Err()
	})
	sc.runOnce(ctx)
	if sc.Progress().Queued != 1 || e.sy.CaptureStats().Captures != 0 {
		t.Fatal("cancelled authorization lost an uncaptured notification")
	}
	sc.SetAuthorize(nil)
	sc.runOnce(t.Context())
	if e.flushes() != 1 || sc.Progress().Queued != 0 {
		t.Fatalf("notification did not resume: flushes=%d queued=%d", e.flushes(), sc.Progress().Queued)
	}
}
