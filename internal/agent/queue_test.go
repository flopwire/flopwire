package agent

import (
	"fmt"
	"testing"

	"github.com/flopwire/flopwire/internal/perfguard"
)

// queueAgent is an Agent with only what the queue operations use.
func queueAgent() *Agent {
	return &Agent{wake: make(chan struct{}, 1)}
}

// promoteAll queues n targets for a background re-parse, then changes
// every one (a sweep: normal queue), then hits every one again on the
// fast lane (urgent), the way a startup pass over a large corpus does.
// It returns the queue entries examined.
func promoteAll(a *Agent, n int) []*target {
	ts := make([]*target, n)
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range ts {
		ts[i] = &target{path: fmt.Sprint("/t/", i)}
		a.enqueueBackgroundLocked(ts[i])
	}
	for _, t := range ts {
		a.enqueueLocked(t, false)
	}
	for _, t := range ts {
		a.enqueueLocked(t, true)
	}
	for _, t := range ts {
		a.enqueueLocked(t, true) // already urgent: a no-op
	}
	return ts
}

// Moving a target between queues is constant time, so promoting every
// queued target is linear in the queue length, not quadratic
// (perf-guards.md #7, agent.go removeTarget).
func TestQueuePromotionLinear(t *testing.T) {
	perfguard.AssertScaling(t, perfguard.Linear, 500, 8, func(t testing.TB, n int) perfguard.Cost {
		a := queueAgent()
		promoteAll(a, n)
		// Queue steps stand in for statements: the guard's count metric.
		return perfguard.Cost{Statements: int64(a.urgent.steps + a.normal.steps + a.background.steps)}
	})
}

// Promotions keep each queue in arrival order and every target in
// exactly one queue.
func TestQueuePromotionOrder(t *testing.T) {
	a := queueAgent()
	ts := make([]*target, 6)
	a.mu.Lock()
	for i := range ts {
		ts[i] = &target{path: fmt.Sprint("/t/", i)}
		a.enqueueBackgroundLocked(ts[i])
	}
	a.enqueueLocked(ts[3], false) // background -> normal
	a.enqueueLocked(ts[1], false)
	a.enqueueLocked(ts[1], false)    // already normal: stays put
	a.enqueueLocked(ts[5], true)     // background -> urgent
	a.enqueueLocked(ts[3], true)     // normal -> urgent
	a.enqueueLocked(ts[5], true)     // already urgent
	a.enqueueBackgroundLocked(ts[1]) // queued: no-op
	a.mu.Unlock()
	paths := func(q *queue) string {
		var s []string
		for _, t := range q.targets() {
			s = append(s, t.path)
		}
		return fmt.Sprint(s)
	}
	if got := paths(&a.urgent); got != "[/t/5 /t/3]" {
		t.Errorf("urgent %s", got)
	}
	if got := paths(&a.normal); got != "[/t/1]" {
		t.Errorf("normal %s", got)
	}
	if got := paths(&a.background); got != "[/t/0 /t/2 /t/4]" {
		t.Errorf("background %s", got)
	}
	if n := a.urgent.len() + a.normal.len() + a.background.len(); n != len(ts) {
		t.Errorf("%d queued, want %d", n, len(ts))
	}
	for _, want := range []string{"/t/5", "/t/3", "/t/1", "/t/0"} {
		var got *target
		switch {
		case a.urgent.len() > 0:
			got = a.urgent.pop()
		case a.normal.len() > 0:
			got = a.normal.pop()
		default:
			got = a.background.pop()
		}
		if got.path != want || got.in != nil {
			t.Fatalf("popped %s (in %p), want %s", got.path, got.in, want)
		}
	}
}
