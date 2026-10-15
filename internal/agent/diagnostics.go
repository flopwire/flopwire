package agent

import (
	"os"
	"runtime/metrics"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/workmetrics"
)

type agentWork struct {
	watcherEvent, storePoll, idleTrim          workmetrics.Operation
	index, fastLane                            workmetrics.Operation
	sweep, discover, authorize                 workmetrics.Operation
	refresh, refreshLockWait, capturedChildren workmetrics.Operation
	refreshFromSweep, refreshFromAuthorization workmetrics.Operation
}

// DiagnosticsSnapshot contains only process-local cumulative counters. Wall
// timings include waits and overlap each other; they cannot be added to obtain
// process CPU. ProcessCPUNS covers the entire process since process startup,
// while operation counters start when this Agent is constructed.
type DiagnosticsSnapshot struct {
	Sync                    *devicesync.DiagnosticsSnapshot `json:"sync,omitempty"`
	PID                     int                             `json:"pid"`
	StartedAt               time.Time                       `json:"started_at"`
	ObservedAt              time.Time                       `json:"observed_at"`
	UptimeNS                int64                           `json:"uptime_ns"`
	ProcessCPUNS            int64                           `json:"process_cpu_ns"`
	GCCyclesTotal           uint64                          `json:"gc_cycles_total"`
	HeapAllocatedBytesTotal uint64                          `json:"heap_allocated_bytes_total"`
	Operations              []workmetrics.Snapshot          `json:"operations"`
}

// Diagnostics reads counters without discovering sources, reading the index,
// taking agent locks, or resetting instrumentation. Concurrent reads are
// approximate observations, not a transactional view of every counter.
func (a *Agent) Diagnostics() DiagnosticsSnapshot {
	now := time.Now()
	samples := []metrics.Sample{{Name: "/gc/cycles/total:gc-cycles"}, {Name: "/gc/heap/allocs:bytes"}}
	metrics.Read(samples)
	w := &a.work
	snapshot := DiagnosticsSnapshot{
		PID: os.Getpid(), StartedAt: a.initializedAt, ObservedAt: now, UptimeNS: now.Sub(a.initializedAt).Nanoseconds(), ProcessCPUNS: cpuTime().Nanoseconds(),
		GCCyclesTotal: samples[0].Value.Uint64(), HeapAllocatedBytesTotal: samples[1].Value.Uint64(),
		Operations: []workmetrics.Snapshot{
			w.watcherEvent.Snapshot("watcher_event"), w.storePoll.Snapshot("store_poll"), w.idleTrim.Snapshot("idle_trim"),
			w.index.Snapshot("index_target"), w.fastLane.Snapshot("fast_lane"), w.sweep.Snapshot("sweep"), w.discover.Snapshot("discover_all"), w.authorize.Snapshot("authorize_capture"),
			w.refresh.Snapshot("refresh_cowork"), w.refreshLockWait.Snapshot("refresh_cowork_lock_wait"), w.capturedChildren.Snapshot("captured_claude_children"),
			w.refreshFromSweep.Snapshot("refresh_from_sweep"), w.refreshFromAuthorization.Snapshot("refresh_from_authorization"),
		},
	}
	if sync, ok := a.cfg.Sync.(interface {
		Diagnostics() devicesync.DiagnosticsSnapshot
	}); ok {
		d := sync.Diagnostics()
		snapshot.Sync = &d
	}
	return snapshot
}
