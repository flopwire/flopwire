package devicesync

import (
	"github.com/flopwire/flopwire/internal/workmetrics"
	"sync/atomic"
)

type schedulerWork struct {
	turn, proof, authorize, captureUpload workmetrics.Operation
	settled                               atomic.Uint64
}

// DiagnosticsSnapshot is process-local scheduler work, without source identity.
// Operation wall times overlap; they are not CPU attribution.
type DiagnosticsSnapshot struct {
	SettledTurns uint64                 `json:"settled_turns"`
	Operations   []workmetrics.Snapshot `json:"operations"`
}

// Diagnostics reads only atomic counters and never takes scheduler/storage locks.
func (s *Scheduler) Diagnostics() DiagnosticsSnapshot {
	w := &s.work
	return DiagnosticsSnapshot{SettledTurns: w.settled.Load(), Operations: []workmetrics.Snapshot{
		w.turn.Snapshot("turn"), w.proof.Snapshot("no_work_probe"), w.authorize.Snapshot("authorize"), w.captureUpload.Snapshot("capture_upload"),
	}}
}
