package devicesync

import (
	"context"
	"database/sql"
	"time"
)

const scheduleHintsSchema = `CREATE TABLE IF NOT EXISTS devsync_schedule_hints (
 path TEXT PRIMARY KEY,
 activity_at INTEGER NOT NULL,
 waiting_since INTEGER NOT NULL
) WITHOUT ROWID;`

type pendingSource struct {
	Spec  SourceSpec
	Hints scheduleHints
}

func hintTime(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}
func hintNanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// loadScheduleHints keeps historical service age on restart. Legacy pending
// rows use their earliest pending capture; capture time is not new activity.
func (s *Store) loadScheduleHints(ctx context.Context, path string) (scheduleHints, error) {
	var activity, waiting int64
	err := s.db.QueryRowContext(ctx, `SELECT coalesce(h.activity_at,0),
 coalesce(nullif(h.waiting_since,0),(SELECT min(g.captured_at) FROM devsync_gens g
 WHERE g.source_id=s.id AND g.lost=0 AND (g.acked<g.entries OR g.tail_acked=0)),0)
 FROM devsync_sources s LEFT JOIN devsync_schedule_hints h ON h.path=s.path WHERE s.path=?`, path).Scan(&activity, &waiting)
	if err == sql.ErrNoRows {
		err = s.db.QueryRowContext(ctx, `SELECT activity_at,waiting_since FROM devsync_schedule_hints WHERE path=?`, path).Scan(&activity, &waiting)
		if err == sql.ErrNoRows {
			return scheduleHints{}, nil
		}
	}
	return scheduleHints{ActivityAt: hintTime(activity), WaitingSince: hintTime(waiting)}, err
}

// admitScheduleHints runs on the serial worker, never on Notify. It merges
// primary facts without creating a capture or changing source identity.
func (s *Store) admitScheduleHints(ctx context.Context, path string, h scheduleHints) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO devsync_schedule_hints(path,activity_at,waiting_since) VALUES(?,?,?)
 ON CONFLICT(path) DO UPDATE SET activity_at=max(activity_at,excluded.activity_at),
 waiting_since=CASE WHEN waiting_since=0 THEN excluded.waiting_since WHEN excluded.waiting_since=0 THEN waiting_since ELSE min(waiting_since,excluded.waiting_since) END`, path, hintNanos(h.ActivityAt), hintNanos(h.WaitingSince))
	return err
}

// serviceScheduleHints records successful service, including a pending turn.
// A finished episode has zero waiting age; activity remains a historical fact.
func (s *Store) serviceScheduleHints(ctx context.Context, path string, h scheduleHints) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO devsync_schedule_hints(path,activity_at,waiting_since) VALUES(?,?,?)
 ON CONFLICT(path) DO UPDATE SET activity_at=max(activity_at,excluded.activity_at),waiting_since=excluded.waiting_since`, path, hintNanos(h.ActivityAt), hintNanos(h.WaitingSince))
	return err
}
