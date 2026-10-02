package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// advisoryHeld counts the advisory locks any backend of the test database
// holds.
func advisoryHeld(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_locks WHERE locktype='advisory'
		AND database=(SELECT oid FROM pg_database WHERE datname=current_database())`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func lockedConn(t *testing.T, pool *pgxpool.Pool) *pgxpool.Conn {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := PinBackend(ctx, conn); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{`SELECT pg_advisory_lock(hashtextextended('chunk:test',0))`, `SELECT pg_advisory_lock_shared(-42)`} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if n := advisoryHeld(t, pool); n != 2 {
		t.Fatalf("%d advisory locks held, want 2", n)
	}
	return conn
}

var testLocks = []AdvisoryLock{{Key: "chunk:test"}, {ID: -42, Shared: true}}

// When an explicit unlock fails, the connection's locks are released at
// once and the connection stays usable.
func TestReleaseAdvisoryLocksFallsBackToUnlockAll(t *testing.T) {
	f := newSchemaFixture(t)
	defer SetReleaseFaults(ReleaseFaults{Unlock: true})()
	conn := lockedConn(t, f.pool)
	defer conn.Release()
	if err := ReleaseAdvisoryLocks(conn, f.pool, testLocks...); err != nil {
		t.Fatal(err)
	}
	if n := advisoryHeld(t, f.pool); n != 0 {
		t.Fatalf("%d advisory locks held after release", n)
	}
	if conn.Conn().IsClosed() {
		t.Fatal("a connection released cleanly by unlock_all was closed")
	}
}

// When the connection cannot release its locks at all, closing it frees
// them only once its backend exits, which a server that cannot see the
// client go (here: a socket left open) delays without bound. The release
// returns only once no backend holds them.
func TestReleaseAdvisoryLocksFreesLocksOfUnreachableBackend(t *testing.T) {
	f := newSchemaFixture(t)
	defer SetReleaseFaults(ReleaseFaults{Unlock: true, UnlockAll: true, Abandon: true})()
	conn := lockedConn(t, f.pool)
	defer conn.Release()
	if err := ReleaseAdvisoryLocks(conn, f.pool, testLocks...); err != nil {
		t.Fatal(err)
	}
	if n := advisoryHeld(t, f.pool); n != 0 {
		t.Fatalf("%d advisory locks still held after release returned", n)
	}
}

// The deletion worker and the orphan reconciler free their locks before
// they return, even when unlocking fails.
func TestDeletionWorkerAndReconcilerFreeLocksWhenUnlockFails(t *testing.T) {
	ctx := context.Background()
	f := newDeletionFixture(t)
	if _, err := f.p.RequestConversationDeletion(ctx, f.convA, f.user, f.device, false); err != nil {
		t.Fatal(err)
	}
	exec(t, f.pool, `INSERT INTO chunks(hash,size,stored_size,object_key,state,cleanup_after) VALUES($1,5,5,'chunks/orphan','uploading',now()-interval '1 minute')`, hash32("orphan"))
	defer SetReleaseFaults(ReleaseFaults{Unlock: true, UnlockAll: true, Abandon: true})()
	if n, err := f.p.ProcessDeletionJobs(ctx); err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if n := advisoryHeld(t, f.pool); n != 0 {
		t.Fatalf("the deletion worker returned with %d advisory locks held", n)
	}
	if n, err := f.p.ReconcileOrphanChunks(ctx, 10); err != nil || n != 1 {
		t.Fatalf("reconcile: %d %v", n, err)
	}
	if n := advisoryHeld(t, f.pool); n != 0 {
		t.Fatalf("the reconciler returned with %d advisory locks held", n)
	}
}

// When neither unlock works, the connection is dropped from the pool and
// its locks are gone when the release returns.
func TestReleaseAdvisoryLocksDropsFailedConnection(t *testing.T) {
	f := newSchemaFixture(t)
	defer SetReleaseFaults(ReleaseFaults{Unlock: true, UnlockAll: true})()
	conn := lockedConn(t, f.pool)
	defer conn.Release()
	if err := ReleaseAdvisoryLocks(conn, f.pool, testLocks...); err != nil {
		t.Fatal(err)
	}
	if n := advisoryHeld(t, f.pool); n != 0 {
		t.Fatalf("%d advisory locks still held after release returned", n)
	}
	if !conn.Conn().IsClosed() {
		t.Fatal("a connection that could not release its locks went back to the pool")
	}
}

// The terminate fallback matches the backend by process id and start
// time. A backend whose start time differs (the server reused the process
// id) is not terminated, even while it holds one of the locks.
func TestReleaseAdvisoryLocksSparesBackendWithOtherStartTime(t *testing.T) {
	f := newSchemaFixture(t)
	restore := SetReleaseFaults(ReleaseFaults{Unlock: true, UnlockAll: true, Abandon: true})
	defer restore()
	defer func(w time.Duration) { releaseWait = w }(releaseWait)
	releaseWait = 500 * time.Millisecond
	conn := lockedConn(t, f.pool)
	defer conn.Release()
	pid := conn.Conn().PgConn().PID()
	cd := conn.Conn().PgConn().CustomData()
	cd[backendStartKey] = cd[backendStartKey].(time.Time).Add(-time.Second)
	if err := ReleaseAdvisoryLocks(conn, f.pool, testLocks...); err == nil {
		t.Fatal("release reported success while the locks are still held")
	}
	var alive int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity WHERE pid=$1`, pid).Scan(&alive); err != nil {
		t.Fatal(err)
	}
	if alive != 1 || advisoryHeld(t, f.pool) != 2 {
		t.Fatalf("a backend with another start time was terminated: alive %d, locks %d", alive, advisoryHeld(t, f.pool))
	}
	restore() // closes the abandoned connection: its backend exits
	deadline := time.Now().Add(5 * time.Second)
	for advisoryHeld(t, f.pool) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("locks outlived the abandoned connection")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
