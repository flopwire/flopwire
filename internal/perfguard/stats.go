package perfguard

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TableCost is the work done on one table: tuples read and written, and
// buffer pages accessed (hit or read from disk).
type TableCost struct {
	SeqTupRead  int64 // tuples returned by sequential scans
	IdxTupRead  int64 // index entries returned by scans of the table's indexes
	IdxTupFetch int64 // live heap tuples fetched by index and bitmap scans
	TupIns      int64
	TupUpd      int64
	TupDel      int64
	HeapBlks    int64 // heap_blks_hit + heap_blks_read
	IdxBlks     int64 // idx_blks_hit + idx_blks_read
	ToastBlks   int64 // toast_blks_hit + toast_blks_read + tidx_blks_hit + tidx_blks_read
}

// Rows is the number of tuples the work touched: read by sequential scans,
// read through an index (index-only scans included), and written. It is
// the primary cost metric.
func (c TableCost) Rows() int64 {
	return c.SeqTupRead + c.IdxTupRead + c.TupIns + c.TupUpd + c.TupDel
}

// Blocks is the number of buffer page accesses the work made. It is
// secondary: hint bits, page pruning and plan choices at small sizes move
// it, so scaling assertions report it but do not gate on it.
func (c TableCost) Blocks() int64 { return c.HeapBlks + c.IdxBlks + c.ToastBlks }

func (c TableCost) add(o TableCost) TableCost {
	return TableCost{c.SeqTupRead + o.SeqTupRead, c.IdxTupRead + o.IdxTupRead, c.IdxTupFetch + o.IdxTupFetch, c.TupIns + o.TupIns, c.TupUpd + o.TupUpd,
		c.TupDel + o.TupDel, c.HeapBlks + o.HeapBlks, c.IdxBlks + o.IdxBlks, c.ToastBlks + o.ToastBlks}
}

func (c TableCost) sub(o TableCost) TableCost {
	return TableCost{c.SeqTupRead - o.SeqTupRead, c.IdxTupRead - o.IdxTupRead, c.IdxTupFetch - o.IdxTupFetch, c.TupIns - o.TupIns, c.TupUpd - o.TupUpd,
		c.TupDel - o.TupDel, c.HeapBlks - o.HeapBlks, c.IdxBlks - o.IdxBlks, c.ToastBlks - o.ToastBlks}
}

func (c TableCost) String() string {
	return fmt.Sprintf("rows=%d (seq=%d idx=%d fetch=%d ins=%d upd=%d del=%d) blocks=%d (heap=%d idx=%d toast=%d)",
		c.Rows(), c.SeqTupRead, c.IdxTupRead, c.IdxTupFetch, c.TupIns, c.TupUpd, c.TupDel, c.Blocks(), c.HeapBlks, c.IdxBlks, c.ToastBlks)
}

// Snapshot is the cumulative cost per table ("schema.table") of one
// database at one moment.
type Snapshot map[string]TableCost

// Cost is the work between two snapshots, per table, plus the statements
// the code under test sent when a Counter was attached.
type Cost struct {
	Tables     map[string]TableCost
	Statements int64 // -1 when no Counter was attached
	// Pages is the SQLite b-tree pages the work fetched (page cache hits
	// plus misses) on the measured connections. It is SQLite's read cost:
	// a full scan fetches every page of the table, a keyed lookup a few.
	// Postgres leaves it 0 and reports blocks per table instead.
	Pages int64
}

// Total sums the cost over every table.
func (c Cost) Total() TableCost {
	var t TableCost
	for _, tc := range c.Tables {
		t = t.add(tc)
	}
	return t
}

// String lists the total and every table with a nonzero cost.
func (c Cost) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "total: %s statements=%d", c.Total(), c.Statements)
	if c.Pages != 0 {
		fmt.Fprintf(&b, " pages=%d", c.Pages)
	}
	for _, name := range slices.Sorted(maps.Keys(c.Tables)) {
		if tc := c.Tables[name]; tc != (TableCost{}) {
			fmt.Fprintf(&b, "\n  %s: %s", name, tc)
		}
	}
	return b.String()
}

// Sub returns the per-table cost from before to s.
func (s Snapshot) Sub(before Snapshot) Cost {
	c := Cost{Tables: map[string]TableCost{}, Statements: -1}
	for name, after := range s {
		c.Tables[name] = after.sub(before[name])
	}
	return c
}

type untracedKey struct{}

// untraced marks harness statements so a Counter on the same pool skips them.
func untraced(ctx context.Context) context.Context {
	return context.WithValue(ctx, untracedKey{}, true)
}

// FlushConn forces conn's pending statistics into shared memory.
func FlushConn(ctx context.Context, conn *pgx.Conn) error {
	_, err := conn.Exec(untraced(ctx), `SELECT pg_stat_force_next_flush()`)
	return err
}

const statsQuery = `
SELECT s.schemaname || '.' || s.relname,
       coalesce(s.seq_tup_read, 0),
       coalesce((SELECT sum(i.idx_tup_read) FROM pg_stat_user_indexes i WHERE i.relid = s.relid), 0)::bigint,
       coalesce(s.idx_tup_fetch, 0),
       s.n_tup_ins, s.n_tup_upd, s.n_tup_del,
       coalesce(io.heap_blks_hit, 0) + coalesce(io.heap_blks_read, 0),
       coalesce(io.idx_blks_hit, 0) + coalesce(io.idx_blks_read, 0),
       coalesce(io.toast_blks_hit, 0) + coalesce(io.toast_blks_read, 0)
         + coalesce(io.tidx_blks_hit, 0) + coalesce(io.tidx_blks_read, 0)
FROM pg_stat_user_tables s JOIN pg_statio_user_tables io USING (relid)`

// TakeSnapshot flushes the statistics of every pool connection (and of
// conns, separate connections the work also used) and reads the cumulative
// cost of every user table.
//
// It first waits, up to five seconds or half of ctx's remaining time, until every client
// backend connected to the database has been flushed or has exited (a
// backend flushes on exit before it leaves pg_stat_activity). That covers
// a pool connection the health check holds for a moment and a connection
// destroyed during the work, whose backend may still be exiting. It fails
// when a pool connection stays acquired, or when another connection to the
// database (a second pool, say) stays open: its counts cannot be flushed,
// and a snapshot without them would silently under-count.
func TakeSnapshot(ctx context.Context, pool *pgxpool.Pool, conns ...*pgx.Conn) (Snapshot, error) {
	ctx = untraced(ctx)
	// The wait ends before ctx does, so the queries can still report why.
	wait := snapshotWait
	if d, ok := ctx.Deadline(); ok {
		wait = min(wait, time.Until(d)/2)
	}
	deadline := time.Now().Add(wait)
	flushed := map[int32]bool{}
	for _, c := range conns {
		if err := FlushConn(ctx, c); err != nil {
			return nil, err
		}
		flushed[int32(c.PgConn().PID())] = true
	}
	for {
		for _, c := range pool.AcquireAllIdle(ctx) {
			err := FlushConn(ctx, c.Conn())
			flushed[int32(c.Conn().PgConn().PID())] = true
			c.Release()
			if err != nil {
				return nil, err
			}
		}
		acquired := pool.Stat().AcquiredConns()
		pids, err := unflushedBackends(ctx, pool, flushed)
		if err != nil {
			return nil, err
		}
		if acquired == 0 && len(pids) == 0 {
			break
		}
		if time.Now().After(deadline) {
			if acquired > 0 {
				return nil, fmt.Errorf("perfguard: %d pool connections are in use; their statistics cannot be flushed", acquired)
			}
			return nil, fmt.Errorf("perfguard: client backends %v are connected to the database but not through the measured pool; "+
				"their statistics cannot be flushed (pass their *pgx.Conn to TakeSnapshot, or do the work on the pool)", pids)
		}
		time.Sleep(20 * time.Millisecond)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_stat_clear_snapshot()`); err != nil {
		return nil, err
	}
	rows, err := conn.Query(ctx, statsQuery)
	if err != nil {
		return nil, err
	}
	snap := Snapshot{}
	for rows.Next() {
		var name string
		var tc TableCost
		if err := rows.Scan(&name, &tc.SeqTupRead, &tc.IdxTupRead, &tc.IdxTupFetch, &tc.TupIns, &tc.TupUpd, &tc.TupDel,
			&tc.HeapBlks, &tc.IdxBlks, &tc.ToastBlks); err != nil {
			rows.Close()
			return nil, err
		}
		snap[name] = tc
	}
	return snap, rows.Err()
}

// snapshotWait bounds how long TakeSnapshot waits for unflushed backends.
const snapshotWait = 5 * time.Second

// unflushedBackends lists the client backends of the pool's database,
// other than the querying one, that are not in flushed.
func unflushedBackends(ctx context.Context, pool *pgxpool.Pool, flushed map[int32]bool) ([]int32, error) {
	rows, err := pool.Query(ctx, `
SELECT pid FROM pg_stat_activity
WHERE datname = current_database() AND backend_type = 'client backend'
  AND pid <> pg_backend_pid() AND NOT pid = ANY ($1) ORDER BY pid`, slices.Collect(maps.Keys(flushed)))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int32])
}

// DisableAutovacuum turns off autovacuum and autoanalyze on every user
// table (and its TOAST table) in the pool's database, so background
// maintenance does not add to measured block counts.
func DisableAutovacuum(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(untraced(ctx), `
DO $$
DECLARE r record;
BEGIN
  FOR r IN SELECT c.oid::regclass AS rel, c.reltoastrelid <> 0 AS toast
           FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
           WHERE c.relkind IN ('r', 'm') AND n.nspname NOT IN ('pg_catalog', 'information_schema')
             AND n.nspname NOT LIKE 'pg\_%'
             AND NOT coalesce('autovacuum_enabled=false' = ANY (c.reloptions), false)
  LOOP
    IF r.toast THEN
      EXECUTE format('ALTER TABLE %s SET (autovacuum_enabled = false, toast.autovacuum_enabled = false)', r.rel);
    ELSE
      EXECUTE format('ALTER TABLE %s SET (autovacuum_enabled = false)', r.rel);
    END IF;
  END LOOP;
END $$`)
	return err
}

// Measure runs fn and returns the cost it caused in the pool's database.
// counter, when not nil, must be attached to pool (see NewPool); its
// statements during fn are reported. fn must leave the pool idle. Measure
// disables autovacuum on the existing tables first.
func Measure(t testing.TB, pool *pgxpool.Pool, counter *Counter, fn func()) Cost {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := DisableAutovacuum(ctx, pool); err != nil {
		t.Fatalf("perfguard: disable autovacuum: %v", err)
	}
	before, err := TakeSnapshot(ctx, pool)
	if err != nil {
		t.Fatalf("perfguard: snapshot before: %v", err)
	}
	var q0 int64
	if counter != nil {
		q0 = counter.Statements()
	}
	fn()
	var q1 int64
	if counter != nil {
		q1 = counter.Statements()
	}
	after, err := TakeSnapshot(ctx, pool)
	if err != nil {
		t.Fatalf("perfguard: snapshot after: %v", err)
	}
	cost := after.Sub(before)
	if counter != nil {
		cost.Statements = q1 - q0
	}
	return cost
}
