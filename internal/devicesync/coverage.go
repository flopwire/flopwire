package devicesync

import (
	"context"
	"github.com/flopwire/flopwire/internal/syncproto"
	"time"

	"github.com/flopwire/flopwire/internal/coverage"
)

// CoverageContext reports scheduled source checks separately from retained
// captures. A failed database read leaves the capture counters unknown.
func (s *Scheduler) CoverageContext(ctx context.Context) (*coverage.UploadSnapshot, error) {
	s.mu.Lock()
	st := &coverage.UploadSnapshot{QueuedSourceChecks: len(s.ready) + len(s.waiting), ActiveSourceTurns: s.running, FailingSources: len(s.failing)}
	now := time.Now()
	switch {
	case s.halted != nil:
		st.BlockingReason = "pin_or_permanent_stop"
	case s.sy.spool.Blocked():
		st.BlockingReason = "spool_capacity"
	case syncproto.Busy(s.lastErr) && now.Before(s.retryAt):
		st.BlockingReason = "server_admission_cooldown"
	case s.down:
		st.BlockingReason = "server_unavailable"
	case now.Before(s.retryAt):
		st.BlockingReason = "retry_backoff"
	}
	s.mu.Unlock()
	captured, err := s.sy.store.capturedPendingContext(ctx)
	if err != nil {
		return st, err
	}
	st.Captured = captured
	return st, nil
}

// One statement gives a coherent observation of retained pending generations,
// including provisional tails. Lost generations are not upload work.
func (s *Store) capturedPendingContext(ctx context.Context) (*coverage.CapturedSnapshot, error) {
	st := new(coverage.CapturedSnapshot)
	err := s.db.QueryRowContext(ctx, `WITH pending AS (
 SELECT source_id,generation,entries,acked,tail_size,tail_acked FROM devsync_gens
 WHERE lost=0 AND (acked<entries OR tail_acked=0)
 ) SELECT (SELECT count(*) FROM devsync_gens WHERE lost<>0),count(*),coalesce(sum(entries-acked),0),
 (SELECT coalesce(sum(m.size),0) FROM devsync_manifest m JOIN pending p
 ON p.source_id=m.source_id AND p.generation=m.generation WHERE m.ordinal>=p.acked),
 coalesce(sum(CASE WHEN tail_acked=0 THEN tail_size ELSE 0 END),0) FROM pending`).Scan(
		&st.LostGenerations, &st.PendingGenerations, &st.PendingManifestEntries, &st.PendingManifestBytes, &st.PendingTailBytes)
	if err != nil {
		return nil, err
	}
	return st, nil
}
