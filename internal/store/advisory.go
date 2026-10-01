package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AdvisoryLock names one session-level advisory lock a pooled connection
// holds: hashtextextended(Key, 0) when Key is set, else ID.
type AdvisoryLock struct {
	Key    string
	ID     int64
	Shared bool
}

func (l AdvisoryLock) unlockSQL() (string, any) {
	fn := "pg_advisory_unlock"
	if l.Shared {
		fn = "pg_advisory_unlock_shared"
	}
	if l.Key != "" {
		return `SELECT ` + fn + `(hashtextextended($1,0))`, l.Key
	}
	return `SELECT ` + fn + `($1)`, l.ID
}

// heldCond holds while backend $1 still holds any of the advisory locks
// named by text keys $2 or ids $3. A one-bigint advisory lock shows in
// pg_locks as classid (high 32 bits), objid (low 32 bits), objsubid 1.
const heldCond = `EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND pid=$1 AND objsubid=1
	AND ((classid::bigint<<32)|objid::bigint) = ANY(ARRAY(SELECT hashtextextended(k,0) FROM unnest($2::text[]) k) || $3::bigint[]))`

// ReleaseFaults injects failures into ReleaseAdvisoryLocks (tests only;
// process-wide, so not for parallel tests).
type ReleaseFaults struct {
	Unlock    bool // the explicit unlocks fail
	UnlockAll bool // pg_advisory_unlock_all fails
	// Abandon replaces closing the connection by taking it out of the pool
	// unclosed, as when the client cannot reach the server: its backend
	// keeps its locks until it is terminated.
	Abandon bool
}

var releaseFaults atomic.Pointer[ReleaseFaults]

// SetReleaseFaults installs faults for ReleaseAdvisoryLocks until the
// returned function runs (tests only). That function also closes the
// connections Abandon took out of the pool.
func SetReleaseFaults(f ReleaseFaults) (restore func()) {
	releaseFaults.Store(&f)
	return func() {
		releaseFaults.Store(nil)
		abandoned.Lock()
		defer abandoned.Unlock()
		for _, c := range abandoned.conns {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = c.Close(ctx)
			cancel()
		}
		abandoned.conns = nil
	}
}

var abandoned struct {
	sync.Mutex
	conns []*pgx.Conn
}

// ReleaseAdvisoryLocks releases the session-level advisory locks conn
// holds before it goes back to the pool, and returns only once no backend
// holds them, or with an error saying it could not make sure.
//
//  1. Each lock is released explicitly.
//  2. If that fails (an error, or a lock that was not held), every
//     session-level advisory lock of the connection is released with
//     pg_advisory_unlock_all(). That is safe on a pooled connection: its
//     holder has it exclusively, and nothing returns a connection to the
//     pool while it holds a session-level lock (this function is how every
//     holder gives them up), so the only such locks are the caller's.
//  3. If that fails too, the connection is unusable. Closing it alone frees
//     the locks only when its backend exits: after Close returns, or much
//     later when the server cannot see the client go (a network
//     partition). So the backend is terminated from a side connection,
//     which frees the locks before the backend exits; then the connection
//     is closed (the caller's Release drops it from the pool) and pg_locks
//     is polled until the locks are gone. The backend is terminated only
//     while it holds one of these locks, and before this side closes the
//     connection: a reused process id would have to belong to a backend
//     that already holds one of these very locks.
func ReleaseAdvisoryLocks(conn *pgxpool.Conn, pool *pgxpool.Pool, locks ...AdvisoryLock) error {
	if conn == nil || len(locks) == 0 {
		return nil
	}
	faults := releaseFaults.Load()
	if faults == nil {
		faults = &ReleaseFaults{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !faults.Unlock && unlockEach(ctx, conn, locks) {
		return nil
	}
	if !faults.UnlockAll {
		if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock_all()`); err == nil {
			return nil
		}
	}

	pid := conn.Conn().PgConn().PID()
	err := awaitReleased(pool, pid, locks, sync.OnceFunc(func() {
		if faults.Abandon {
			abandoned.Lock()
			abandoned.conns = append(abandoned.conns, conn.Hijack())
			abandoned.Unlock()
			return
		}
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Conn().Close(closeCtx)
	}))
	if err != nil {
		err = fmt.Errorf("store: advisory locks of backend %d may still be held: %w", pid, err)
		slog.Warn(err.Error())
	}
	return err
}

// awaitReleased terminates backend pid if it holds one of locks, runs
// closeConn (once, also on every early return), and waits until pid holds
// none of them. It works on a side connection of its own: the pool may
// have no free slot while the caller still holds conn.
func awaitReleased(pool *pgxpool.Pool, pid uint32, locks []AdvisoryLock, closeConn func()) error {
	defer closeConn()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if pool == nil {
		return errors.New("no pool to reach the server through")
	}
	keys, ids := []string{}, []int64{}
	for _, l := range locks {
		if l.Key != "" {
			keys = append(keys, l.Key)
		} else {
			ids = append(ids, l.ID)
		}
	}
	side, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	if err != nil {
		return err
	}
	defer side.Close(context.Background())
	termErr := func() error {
		termCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
		defer cancel()
		_, err := side.Exec(termCtx, `SELECT pg_terminate_backend($1::int,5000) WHERE `+heldCond, pid, keys, ids)
		return err
	}()
	closeConn()
	for {
		var held bool
		err := side.QueryRow(ctx, `SELECT `+heldCond, pid, keys, ids).Scan(&held)
		if err == nil && !held {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Join(termErr, err, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// unlockEach releases each lock and reports whether all were held and
// released.
func unlockEach(ctx context.Context, conn *pgxpool.Conn, locks []AdvisoryLock) bool {
	for _, l := range locks {
		sql, arg := l.unlockSQL()
		var ok bool
		if err := conn.QueryRow(ctx, sql, arg).Scan(&ok); err != nil || !ok {
			return false
		}
	}
	return true
}
