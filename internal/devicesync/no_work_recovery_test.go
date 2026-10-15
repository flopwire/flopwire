package devicesync

import (
	"context"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

// An unchanged file is not settled while its captured bytes await the server.
// Exercise the real capture/ack path, then prove the same job becomes a no-op.
func TestSchedulerNoWorkPendingRecovery(t *testing.T) {
	now := time.Now().Add(time.Hour)
	e := newEnv(t, Config{Now: func() time.Time { return now }}, 1<<20)
	sp := e.spec("recover.jsonl", transcript.StorageJSONLAppend)
	data := jsonlLines(42, 100, 100)
	appendFile(t, sp.Path, data)
	e.srv.SetDown(true)
	ctx := context.Background()
	if err := e.sy.Sync(ctx, sp); err == nil {
		t.Fatal("expected offline upload failure")
	}
	e.srv.SetDown(false)
	sc := NewScheduler(e.sy, SchedulerConfig{})
	calls := 0
	authorize := func(context.Context, SourceSpec) (*CaptureAuthorization, error) { calls++; return nil, nil }
	for i := 0; i < 20; i++ {
		out, err := sc.syncJobTurn(ctx, &job{spec: sp}, -1, false, authorize)
		if err != nil {
			t.Fatal(err)
		}
		if out == syncDone {
			break
		}
		if i == 19 {
			t.Fatal("recovery did not drain")
		}
	}
	if calls == 0 {
		t.Fatal("pending upload bypassed fresh authorization")
	}
	e.requireServerHas(sp.Path, fileIDOf(t, sp.Path), 0, data)
	beforeCalls, beforeFlush := calls, e.flushes()
	if !e.sy.noWork(ctx, sp) {
		t.Fatal("fully recovered source should be settled")
	}
	if _, err := sc.syncJobTurn(ctx, &job{spec: sp}, -1, false, authorize); err != nil {
		t.Fatal(err)
	}
	if calls != beforeCalls || e.flushes() != beforeFlush {
		t.Fatal("settled source repeated authorization or upload")
	}
	// A new notification after no-op completion must still capture fresh bytes.
	more := jsonlLines(43, 10, 100)
	appendFile(t, sp.Path, more)
	sc.Flush(sp)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	sc.SetAuthorize(authorize)
	go func() { defer close(done); sc.Run(runCtx) }()
	waitFor(t, "new notification drained", func() bool { return sc.Status().Queued == 0 && e.flushes() > beforeFlush })
	cancel()
	<-done
	if calls == beforeCalls {
		t.Fatal("new bytes bypassed fresh authorization")
	}
	e.requireServerHas(sp.Path, fileIDOf(t, sp.Path), 0, append(data, more...))
}
