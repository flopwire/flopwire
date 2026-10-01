package ingest

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var errParseBusy = errors.New("ingest: another worker is parsing this source")

// beforeFenceClose, when set (tests), runs after a parse releases its
// fences and before it closes the fence connection.
var beforeFenceClose func()

// ParseSource serializes every extraction, including idle refreshes, across
// server processes. This prevents stale row writes as well as stale cursors.
func (q *Queue) ParseSource(ctx context.Context, sourceID string) error {
	return q.parseFenced(ctx, sourceID)
}

func (q *Queue) refreshSource(ctx context.Context, sourceID string) error {
	return q.parseFenced(ctx, sourceID, "flopwire:idle-reparse")
}

func (q *Queue) parseFenced(ctx context.Context, sourceID string, extra ...string) error {
	// The fence uses a separate connection: extraction needs the pool for
	// short transactions and must also work when the pool has one slot.
	// Closing this connection releases all fences even on parse failure.
	conn, err := pgx.ConnectConfig(ctx, q.Pool.Config().ConnConfig.Copy())
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Release the fences before returning: Close alone frees them only
		// when the server backend exits, after Close returns, and the
		// next parse of the source (or the next idle refresh) would find
		// them held and give up with errParseBusy. If this fails, Close
		// still frees them, later.
		_, _ = conn.Exec(closeCtx, `SELECT pg_advisory_unlock_all()`)
		if beforeFenceClose != nil {
			beforeFenceClose()
		}
		_ = conn.Close(closeCtx)
	}()
	keys := append(extra, "flopwire:source-parse:"+sourceID)
	for _, key := range keys {
		var got bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,0))`, key).Scan(&got); err != nil {
			return err
		}
		if !got {
			return errParseBusy
		}
	}
	return q.parseSource(ctx, sourceID)
}
