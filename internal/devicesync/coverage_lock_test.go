package devicesync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/coverage"
)

func TestCoverageHeldSchedulerMutexReturnsUnknownWithoutWaiting(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{})
	sc.mu.Lock()
	locked := true
	defer func() {
		if locked {
			sc.mu.Unlock()
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	type result struct {
		snapshot *coverage.UploadSnapshot
		err      error
	}
	done := make(chan result, 1)
	go func() { snapshot, err := sc.CoverageContext(ctx); done <- result{snapshot, err} }()
	var got result
	select {
	case got = <-done:
	case <-time.After(500 * time.Millisecond):
		cancel()
		sc.mu.Unlock()
		locked = false
		<-done
		t.Fatal("coverage waited on Scheduler.mu instead of returning busy unknown")
	}
	sc.mu.Unlock()
	locked = false
	if got.err == nil || got.snapshot != nil {
		t.Fatalf("held scheduler invented zero counters: snapshot=%+v error=%v", got.snapshot, got.err)
	}
}

func TestCoverageHeldSpoolMutexKeepsQueueAndCapturedFactsUnknownBlock(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{})
	e.spool.mu.Lock()
	locked := true
	defer func() {
		if locked {
			e.spool.mu.Unlock()
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	type answer struct {
		snapshot *coverage.UploadSnapshot
		err      error
	}
	done := make(chan answer, 1)
	go func() { snapshot, err := sc.CoverageContext(ctx); done <- answer{snapshot, err} }()
	var got answer
	select {
	case got = <-done:
	case <-time.After(500 * time.Millisecond):
		cancel()
		e.spool.mu.Unlock()
		locked = false
		<-done
		t.Fatal("coverage waited on spool mutex")
	}
	e.spool.mu.Unlock()
	locked = false
	if !errors.Is(got.err, ErrCoverageSpoolBusy) || got.snapshot == nil || got.snapshot.Captured == nil || got.snapshot.Captured.PendingGenerations != 0 || got.snapshot.BlockingReason != "" {
		t.Fatalf("snapshot=%+v err=%v", got.snapshot, got.err)
	}
}
