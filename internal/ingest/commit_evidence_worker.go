package ingest

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CommitEvidenceWorker drains the durable projection backlog independently of
// reads. One transaction and one connection bound its concurrency and memory.
type CommitEvidenceWorker struct {
	Pool    *pgxpool.Pool
	Log     *slog.Logger
	Timeout time.Duration
}

func (w *CommitEvidenceWorker) Run(ctx context.Context) {
	recent := true
	for ctx.Err() == nil {
		worked, err := w.step(ctx, recent)
		recent = !recent
		if err != nil && ctx.Err() == nil && w.Log != nil {
			w.Log.Warn("commit evidence projection retry", "error", err)
		}
		delay := time.Second
		if worked && err == nil {
			delay = 100 * time.Millisecond
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (w *CommitEvidenceWorker) step(ctx context.Context, recent bool) (bool, error) {
	timeout := w.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	work, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tx, err := w.Pool.Begin(work)
	if err != nil {
		return false, err
	}
	defer rollbackCommitEvidence(tx)
	var conv string
	var revision int64
	order := "c.id"
	if recent {
		order = "p.parsed_at DESC NULLS LAST,c.id"
	}
	err = tx.QueryRow(work, `SELECT c.id::text,d.revision FROM conversations c JOIN commit_evidence_dirty d ON d.conversation_id=c.id
 LEFT JOIN source_parse_state p ON p.source_id=c.source_id
 WHERE c.hidden_at IS NULL AND NOT d.limited AND (d.retry_after IS NULL OR d.retry_after<=now())
 ORDER BY `+order+` FOR UPDATE OF c SKIP LOCKED LIMIT 1`).Scan(&conv, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	err = RefreshCommitEvidence(work, tx, conv)
	if err == nil {
		err = tx.Commit(work)
	}
	if err == nil {
		return true, nil
	}
	// Roll back before retry bookkeeping; never expose a partial projection.
	rollbackCommitEvidence(tx)
	if ctx.Err() != nil {
		return true, err
	}
	retry, retryCancel := context.WithTimeout(ctx, time.Second)
	defer retryCancel()
	// A newer mutation must keep its fresh work eligible. Guard the revision.
	_, deferErr := w.Pool.Exec(retry, `UPDATE commit_evidence_dirty SET retry_after=now()+interval '30 seconds' WHERE conversation_id=$1 AND revision=$2`, conv, revision)
	return true, errors.Join(err, deferErr)
}

func rollbackCommitEvidence(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
