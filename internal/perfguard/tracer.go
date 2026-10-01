package perfguard

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Counter counts the statements sent through the connections it traces:
// one per Exec, Query or QueryRow (BEGIN and COMMIT included), one per
// queued batch statement, one per CopyFrom. It implements pgx.QueryTracer,
// pgx.BatchTracer and pgx.CopyFromTracer and is safe for concurrent use.
type Counter struct {
	mu    sync.Mutex
	total int64
	bySQL map[string]int64
}

var (
	_ pgx.QueryTracer    = (*Counter)(nil)
	_ pgx.BatchTracer    = (*Counter)(nil)
	_ pgx.CopyFromTracer = (*Counter)(nil)
)

func (c *Counter) count(ctx context.Context, sql string, n int64) {
	if ctx.Value(untracedKey{}) != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bySQL == nil {
		c.bySQL = map[string]int64{}
	}
	c.total += n
	c.bySQL[normalize(sql)] += n
}

func normalize(sql string) string { return strings.Join(strings.Fields(sql), " ") }

// Statements is the number of statements counted so far.
func (c *Counter) Statements() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

// BySQL returns the count per statement text (whitespace collapsed).
func (c *Counter) BySQL() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.bySQL)
}

// Reset sets every count to zero.
func (c *Counter) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total, c.bySQL = 0, nil
}

// String lists statements by descending count, for failure messages.
func (c *Counter) String() string {
	by := c.BySQL()
	keys := slices.SortedFunc(maps.Keys(by), func(a, b string) int {
		if by[a] != by[b] {
			return int(by[b] - by[a])
		}
		return strings.Compare(a, b)
	})
	var b strings.Builder
	fmt.Fprintf(&b, "%d statements", c.Statements())
	for _, k := range keys {
		fmt.Fprintf(&b, "\n  %6d  %s", by[k], k)
	}
	return b.String()
}

func (c *Counter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	c.count(ctx, d.SQL, 1)
	return ctx
}

func (c *Counter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *Counter) TraceBatchStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceBatchStartData) context.Context {
	if d.Batch != nil {
		for _, q := range d.Batch.QueuedQueries {
			c.count(ctx, q.SQL, 1)
		}
	}
	return ctx
}

func (c *Counter) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}
func (c *Counter) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData)     {}

func (c *Counter) TraceCopyFromStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceCopyFromStartData) context.Context {
	c.count(ctx, "COPY "+d.TableName.Sanitize(), 1)
	return ctx
}

func (c *Counter) TraceCopyFromEnd(context.Context, *pgx.Conn, pgx.TraceCopyFromEndData) {}

// Attach sets a new Counter as cfg's tracer and returns it. It replaces any
// tracer already set.
func Attach(cfg *pgxpool.Config) *Counter {
	c := &Counter{}
	cfg.ConnConfig.Tracer = c
	return c
}

// NewPool creates a fresh pgtest database and a pool on it with a Counter
// attached and parallel query off. The pool closes when the test ends. It skips the test when
// pgtest's database environment is unset.
func NewPool(t testing.TB) (*pgxpool.Pool, *Counter) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(pgtest.NewDatabase(t))
	if err != nil {
		t.Fatalf("perfguard: parse url: %v", err)
	}
	c := Attach(cfg)
	// Parallel workers flush their statistics when they exit, after the
	// leader returns, so a snapshot could miss their reads.
	cfg.ConnConfig.RuntimeParams["max_parallel_workers_per_gather"] = "0"
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("perfguard: pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, c
}
