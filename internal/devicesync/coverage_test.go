package devicesync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/coverage"
	"github.com/flopwire/flopwire/internal/transcript"
)

func TestCoverageCapturedPendingTailAndLostRemainDistinct(t *testing.T) {
	e := newEnv(t, Config{MaxRequestBytes: 16 << 10, SealAfter: -1}, 1<<20)
	sp := e.spec("capture.jsonl", transcript.StorageJSONLAppend)
	appendFile(t, sp.Path, jsonlLines(221, 500, 100))
	outcome, err := e.sy.syncTurn(t.Context(), sp, nil, -1, nil, captureSource)
	if err != nil || outcome != uploadPending {
		t.Fatalf("outcome=%v err=%v", outcome, err)
	}
	sc := NewScheduler(e.sy, SchedulerConfig{})
	report, err := sc.CoverageContext(t.Context())
	if err != nil || report.Captured == nil {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	c := report.Captured
	if report.QueuedSourceChecks != 0 || c.PendingGenerations != 1 || c.PendingManifestEntries <= 0 || c.PendingManifestBytes <= 0 || c.PendingTailBytes <= 0 || c.LostGenerations != 0 || c.TruncatedGenerations != 0 {
		t.Fatalf("pending metadata=%+v queue=%d", c, report.QueuedSourceChecks)
	}
	// An empty scheduler does not mean there are no durable pending captures.
	if _, err := e.store.db.Exec(`UPDATE devsync_gens SET lost=1`); err != nil {
		t.Fatal(err)
	}
	report, err = sc.CoverageContext(t.Context())
	if err != nil || report.Captured.PendingGenerations != 0 || report.Captured.PendingManifestEntries != 0 || report.Captured.PendingManifestBytes != 0 || report.Captured.PendingTailBytes != 0 || report.Captured.LostGenerations != 1 {
		t.Fatalf("lost observation=%+v err=%v", report, err)
	}
}

func TestCoverageCancelsBusyStorePreservingMemoryUnknownCaptures(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{})
	sc.Flush(e.spec("queued.jsonl", transcript.StorageJSONLAppend))
	sc.mu.Lock()
	sc.running = 1
	sc.halted = errors.New("synthetic pin mismatch")
	sc.down = true
	sc.failing["synthetic"] = &failure{}
	sc.mu.Unlock()
	held, err := e.store.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	before := e.store.db.Stats().WaitCount
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type answer struct {
		report *coverage.UploadSnapshot
		err    error
	}
	done := make(chan answer, 1)
	go func() { report, err := sc.CoverageContext(ctx); done <- answer{report, err} }()
	waitFor(t, "coverage waiting for sole connection", func() bool { return e.store.db.Stats().WaitCount > before })
	cancel()
	var a answer
	select {
	case a = <-done:
	case <-time.After(500 * time.Millisecond):
		held.Close()
		<-done
		t.Fatal("coverage ignored pool-wait cancellation")
	}
	if !errors.Is(a.err, context.Canceled) || a.report == nil || a.report.QueuedSourceChecks != 1 || a.report.ActiveSourceTurns != 1 || a.report.Captured != nil {
		t.Fatalf("unknown captures lost memory observation: %+v err=%v", a.report, a.err)
	}
	if a.report.BlockingReason != "pin_or_permanent_stop" || a.report.FailingSources != 1 {
		t.Fatalf("blocking=%s failures=%d", a.report.BlockingReason, a.report.FailingSources)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	fresh, err := sc.CoverageContext(t.Context())
	if err != nil || fresh.Captured == nil || fresh.Captured.PendingGenerations != 0 {
		t.Fatalf("fresh=%+v err=%v", fresh, err)
	}
}
