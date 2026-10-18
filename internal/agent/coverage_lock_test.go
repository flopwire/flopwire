package agent

import (
	"context"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/coverage"
)

func TestCoverageHeldAgentMutexReturnsUnknownWithoutWaiting(t *testing.T) {
	f := newFixture(t, "-")
	f.a.mu.Lock()
	locked := true
	defer func() {
		if locked {
			f.a.mu.Unlock()
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	done := make(chan *coverage.Report, 1)
	go func() { done <- f.a.coverageReport(ctx) }()
	var report *coverage.Report
	select {
	case report = <-done:
	case <-time.After(500 * time.Millisecond):
		// Release and join the blocked diagnostic before failing the baseline;
		// the test must never leave a detached reporter touching the fixture.
		cancel()
		f.a.mu.Unlock()
		locked = false
		<-done
		t.Fatal("coverage waited on Agent.mu instead of returning busy unknowns")
	}
	f.a.mu.Unlock()
	locked = false
	if report == nil || report.Policy == nil || report.Policy.ServerCopiesRetained != nil || report.Policy.Cowork != nil || report.Unknown["server_copies"] == "" || report.Unknown["cowork_policy"] == "" {
		t.Fatalf("held local state invented policy observations: %+v", report)
	}
}
