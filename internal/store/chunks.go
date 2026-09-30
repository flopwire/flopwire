package store

// Orphan-chunk reconciliation: the raw-object ledger of the CASS-era stack
// (#6: d4eb6a1, 4c3ee87), remapped from per-segment objects to
// content-addressed chunks.
//
// Contract for the ingest path (B3), which owns reservation and commit:
//
//   - Hold pg_advisory_lock(hashtextextended(ChunkLockKey(hash), 0)) from the
//     reservation until the manifest transaction commits or the upload is
//     abandoned. The reconciler takes the same lock, so it never deletes an
//     object an uploader is still about to reference.
//   - Reserve before the S3 put: INSERT the chunk row with state 'uploading'
//     and cleanup_after = now() + a grace period (ON CONFLICT: reuse a
//     'committed' row; take over an orphan-owned or 'deletion_pending' one,
//     clearing its deletion_job_id; retry later on 'purging_delete').
//   - In the manifest transaction, lock the chunk rows (FOR SHARE or FOR
//     UPDATE), require state 'committed' or the uploader's own 'uploading'
//     reservation, set 'committed', and insert the manifest entries. A chunk
//     in 'deletion_pending' or 'purging_delete' is never referenced as is.
//
// Every state other than 'committed' counts against storage quota (Usage sums
// all chunk rows).

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/flopwire/flopwire/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ChunkLockKey names the advisory lock shared by a chunk's uploader and the
// orphan reconciler.
func ChunkLockKey(hash []byte) string { return "chunk:" + hex.EncodeToString(hash) }

// ReconcileOrphanChunks deletes objects whose upload never committed a
// manifest reference, oldest first, and reports how many it removed. A
// manifest reference always wins: such a chunk becomes 'committed'.
func (p *Postgres) ReconcileOrphanChunks(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	hashes, err := collect[[]byte](p.pool.Query(ctx, `SELECT hash FROM chunks
		WHERE state IN ('uploading','cleanup_pending','purging') AND cleanup_after<=now()
		ORDER BY cleanup_after,created_at LIMIT $1`, limit))(func(row pgx.Row) ([]byte, error) {
		var h []byte
		return h, row.Scan(&h)
	})
	if err != nil {
		return 0, err
	}
	cleaned := 0
	var errs []error
	for _, hash := range hashes {
		ok, err := p.reconcileChunk(ctx, hash)
		if err != nil {
			errs = append(errs, err)
		} else if ok {
			cleaned++
		}
	}
	return cleaned, errors.Join(errs...)
}

func (p *Postgres) reconcileChunk(ctx context.Context, hash []byte) (bool, error) {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Release()
	key := ChunkLockKey(hash)
	if _, err = conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1,0))`, key); err != nil {
		return false, err
	}
	defer unlockSession(conn, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, key)
	target := hex.EncodeToString(hash)
	var objectKey string
	proceed := false
	err = inConnTx(ctx, conn, func(tx pgx.Tx) error {
		var state string
		err := tx.QueryRow(ctx, `SELECT object_key,state FROM chunks WHERE hash=$1 FOR UPDATE`, hash).Scan(&objectKey, &state)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && state != "uploading" && state != "cleanup_pending" && state != "purging" {
			return nil
		}
		if err != nil {
			return err
		}
		var referenced bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM manifest_entries WHERE chunk_hash=$1)`, hash).Scan(&referenced); err != nil {
			return err
		}
		if referenced {
			_, err = tx.Exec(ctx, `UPDATE chunks SET state='committed',cleanup_after=NULL,last_error_class='',updated_at=now() WHERE hash=$1`, hash)
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE chunks SET state='purging',attempts=attempts+1,updated_at=now() WHERE hash=$1`, hash); err != nil {
			return err
		}
		proceed = true
		return insertAudit(domain.AuditEvent{ID: uuid.NewString(), Action: "chunk.cleanup.attempt", TargetType: "chunk", TargetID: target, Metadata: map[string]any{"outcome": "started"}, CreatedAt: time.Now().UTC()})(ctx, tx)
	})
	if err != nil || !proceed {
		return false, err
	}
	if err = p.removeObject(ctx, objectKey); err != nil {
		p.markChunkCleanupPending(conn, hash, err)
		return false, fmt.Errorf("remove orphan chunk: %w", err)
	}
	err = inConnTx(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM chunks WHERE hash=$1 AND state='purging' AND NOT EXISTS (SELECT 1 FROM manifest_entries WHERE chunk_hash=$1)`, hash); err != nil {
			return err
		}
		return insertAudit(domain.AuditEvent{ID: uuid.NewString(), Action: "chunk.cleanup", TargetType: "chunk", TargetID: target, Metadata: map[string]any{"outcome": "deleted"}, CreatedAt: time.Now().UTC()})(ctx, tx)
	})
	if err != nil {
		p.markChunkCleanupPending(conn, hash, err)
		return false, err
	}
	return true, nil
}

func (p *Postgres) markChunkCleanupPending(conn *pgxpool.Conn, hash []byte, cause error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = inConnTx(ctx, conn, func(tx pgx.Tx) error {
		var attempts int
		var state string
		if err := tx.QueryRow(ctx, `SELECT attempts,state FROM chunks WHERE hash=$1 FOR UPDATE`, hash).Scan(&attempts, &state); err != nil {
			return err
		}
		if state != "uploading" && state != "cleanup_pending" && state != "purging" {
			return nil
		}
		next := time.Now().UTC().Add(reconciliationBackoff(attempts, rand.Float64()))
		class := fmt.Sprintf("%T", cause)
		if _, err := tx.Exec(ctx, `UPDATE chunks SET state='cleanup_pending',cleanup_after=$2,last_error_class=$3,updated_at=now() WHERE hash=$1`, hash, next, class); err != nil {
			return err
		}
		return insertAudit(domain.AuditEvent{ID: uuid.NewString(), Action: "chunk.cleanup.pending", TargetType: "chunk", TargetID: hex.EncodeToString(hash), Metadata: map[string]any{"error_class": class, "attempts": attempts, "next_retry_at": next}, CreatedAt: time.Now().UTC()})(ctx, tx)
	})
}

// reconciliationBackoff doubles from 30s to a 6h ceiling with ±20% jitter.
func reconciliationBackoff(attempts int, jitter float64) time.Duration {
	delay := 30 * time.Second
	for i := 0; i < attempts && delay < 6*time.Hour; i++ {
		delay *= 2
	}
	jitter = max(0, min(1, jitter))
	return min(time.Duration(float64(delay)*(0.8+0.4*jitter)), 6*time.Hour)
}

// DependencyStatus reports readiness probes of the server's dependencies.
type DependencyStatus struct{ Database, ObjectStore error }

// CheckDependencies pings Postgres and confirms the bucket exists.
func (p *Postgres) CheckDependencies(ctx context.Context) DependencyStatus {
	status := DependencyStatus{Database: p.pool.Ping(ctx)}
	if p.objects == nil {
		status.ObjectStore = errors.New("object client is not configured")
		return status
	}
	if exists, err := p.objects.BucketExists(ctx, p.bucket); err != nil {
		status.ObjectStore = err
	} else if !exists {
		status.ObjectStore = errors.New("bucket does not exist")
	}
	return status
}
