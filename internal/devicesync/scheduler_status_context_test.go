package devicesync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

func TestSchedulerStatusContextCancelsBusyConnectionKeepsHealth(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{})
	sc.Flush(e.spec("queued.jsonl", transcript.StorageJSONLAppend))
	stopped := errors.New("synthetic TLS pin mismatch")
	sc.mu.Lock()
	sc.down, sc.lastErr, sc.halted = true, stopped, stopped
	sc.retryAt = time.Now().Add(time.Minute)
	sc.failing["synthetic-failed-source"] = &failure{err: errors.New("synthetic source failure"), attempts: 2}
	sc.mu.Unlock()
	// Capture exclusively owns this connection until explicitly released. A
	// status request must cancel its pool wait, not wait for capture to finish.
	held, err := e.store.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	before := e.store.db.Stats().WaitCount
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type answer struct {
		st  Status
		err error
	}
	done := make(chan answer, 1)
	go func() { st, err := sc.StatusContext(ctx); done <- answer{st, err} }()
	waitFor(t, "status waiting for sole connection", func() bool { return e.store.db.Stats().WaitCount > before })
	cancel()
	var result answer
	select {
	case result = <-done:
	case <-time.After(500 * time.Millisecond):
		held.Close()
		<-done
		t.Fatal("status ignored cancellation while capture retained the sole connection")
	}
	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("error=%v", result.err)
	}
	if result.st.Queued != 1 || !result.st.ServerDown || result.st.Stopped != stopped.Error() || result.st.LastError != stopped.Error() || result.st.RetryAt.IsZero() || len(result.st.Failing) != 1 || result.st.Failing[0].Attempts != 2 {
		t.Fatalf("lost live health during unavailable redactions: %+v", result.st)
	}
	if result.st.Redactions != nil || result.st.RedactedSources != 0 {
		t.Fatal("failed redaction query published partial totals")
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	fresh, err := sc.StatusContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Redactions == nil || fresh.Queued != 1 || fresh.Stopped != stopped.Error() {
		t.Fatalf("fresh status did not recover: %+v", fresh)
	}
}
