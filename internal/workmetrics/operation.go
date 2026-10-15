// Package workmetrics records bounded, process-local operation wall times.
package workmetrics

import (
	"sync/atomic"
	"time"
)

// Operation is a fixed operation's cumulative counters. Its zero value is ready
// to use. Do not copy an Operation after first use.
type Operation struct {
	calls, completed, inFlight, total, max atomic.Uint64
}

// Snapshot is an approximate concurrent observation. Durations measure wall
// time, including waits; nested and concurrent operations are not additive CPU.
type Snapshot struct {
	Name        string `json:"name"`
	Calls       uint64 `json:"calls"`
	Completed   uint64 `json:"completed"`
	InFlight    uint64 `json:"in_flight"`
	TotalWallNS uint64 `json:"total_wall_ns"`
	MaxWallNS   uint64 `json:"max_wall_ns"`
}

// Span measures one call without allocating a closure.
type Span struct {
	operation *Operation
	start     time.Time
}

// Begin starts a call. Invoke the returned Span's End exactly once.
func (o *Operation) Begin() Span {
	o.calls.Add(1)
	o.inFlight.Add(1)
	return Span{operation: o, start: time.Now()}
}

// End completes the call and records its wall duration.
func (s Span) End() {
	o := s.operation
	n := uint64(time.Since(s.start))
	o.total.Add(n)
	for old := o.max.Load(); n > old; old = o.max.Load() {
		if o.max.CompareAndSwap(old, n) {
			break
		}
	}
	o.completed.Add(1)
	o.inFlight.Add(^uint64(0))
}

// Snapshot only reads atomic counters; it does not reset them.
func (o *Operation) Snapshot(name string) Snapshot {
	return Snapshot{Name: name, Calls: o.calls.Load(), Completed: o.completed.Load(), InFlight: o.inFlight.Load(), TotalWallNS: o.total.Load(), MaxWallNS: o.max.Load()}
}
