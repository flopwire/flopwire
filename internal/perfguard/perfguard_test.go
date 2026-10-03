package perfguard

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// recorder captures a guard's failures so a self-test can assert that the
// guard fires. Everything else goes to the real test.
type recorder struct {
	testing.TB
	mu     sync.Mutex
	errors []string
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recorder) failed() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.errors, "\n")
}

func exec(t testing.TB, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// itemsPool is a fresh database with items(id pk, grp unindexed, v) of n
// rows and a filler table of fixed size.
func itemsPool(t testing.TB, n, filler int) (*pgxpool.Pool, *Counter) {
	t.Helper()
	pool, c := NewPool(t)
	exec(t, pool, `CREATE TABLE items (id int PRIMARY KEY, grp int NOT NULL, v int NOT NULL, note text)`)
	exec(t, pool, `CREATE TABLE filler (id int PRIMARY KEY)`)
	exec(t, pool, `INSERT INTO items SELECT i, i, i % 7, 'note ' || i FROM generate_series(1, $1) i`, n)
	exec(t, pool, `INSERT INTO filler SELECT generate_series(1, $1)`, filler)
	return pool, c
}

func TestSnapshotFlushesEveryConnection(t *testing.T) {
	pool, counter := itemsPool(t, 1000, 0)
	ctx := context.Background()
	cost := Measure(t, pool, counter, func() {
		// Three connections held at once, so the work is spread over
		// three backends; the snapshot follows immediately.
		var conns []*pgxpool.Conn
		for range 3 {
			c, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			conns = append(conns, c)
		}
		for i, c := range conns {
			var n int
			if err := c.QueryRow(ctx, `SELECT count(*) FROM items WHERE v >= 0`).Scan(&n); err != nil || n != 1000 {
				t.Fatalf("count: %d %v", n, err)
			}
			if _, err := c.Exec(ctx, `UPDATE items SET v = v + 1 WHERE id = $1`, i+1); err != nil {
				t.Fatal(err)
			}
		}
		for _, c := range conns {
			c.Release()
		}
	})
	items := cost.Tables["public.items"]
	if items.SeqTupRead != 3000 || items.TupUpd != 3 || items.IdxTupRead != 3 {
		t.Fatalf("items cost %s, want seq=3000 upd=3 idx=3\n%s", items, cost)
	}
	if cost.Statements != 6 {
		t.Fatalf("statements %d, want 6 (harness statements must not count)\n%s", cost.Statements, counter)
	}
}

func TestSnapshotRefusesBusyPool(t *testing.T) {
	pool, _ := NewPool(t)
	c, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := TakeSnapshot(ctx, pool); err == nil {
		t.Fatal("snapshot with an acquired connection succeeded")
	}
}

// Work on a second pool to the same database cannot be flushed; a
// snapshot that ignored it would read zero cost and pass any guard.
func TestSnapshotRefusesForeignConnection(t *testing.T) {
	pool, _ := itemsPool(t, 100, 0)
	ctx := context.Background()
	other, err := pgx.Connect(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(ctx)
	if _, err := other.Exec(ctx, `SELECT count(*) FROM items`); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if _, err := TakeSnapshot(short, pool); err == nil || !strings.Contains(err.Error(), "not through the measured pool") {
		t.Fatalf("snapshot with an unflushed foreign connection: err = %v", err)
	}
	// Passed explicitly, the connection is flushed and counted.
	before, err := TakeSnapshot(ctx, pool, other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Exec(ctx, `SELECT count(*) FROM items`); err != nil {
		t.Fatal(err)
	}
	after, err := TakeSnapshot(ctx, pool, other)
	if err != nil {
		t.Fatal(err)
	}
	if got := after.Sub(before).Tables["public.items"].SeqTupRead; got != 100 {
		t.Fatalf("seq_tup_read on the extra connection = %d, want 100", got)
	}
}

// A pool connection destroyed during the work (closed, or released
// mid-transaction) flushes only when its backend exits, after the client
// returns. The snapshot must wait for that, not miss or refuse it.
func TestSnapshotWaitsForDestroyedConnection(t *testing.T) {
	pool, counter := itemsPool(t, 1000, 0)
	ctx := context.Background()
	cost := Measure(t, pool, counter, func() {
		c, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, `SELECT count(*) FROM items`); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, `BEGIN`); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, `SELECT count(*) FROM items`); err != nil {
			t.Fatal(err)
		}
		c.Release() // in a transaction: the pool destroys it
	})
	if got := cost.Tables["public.items"].SeqTupRead; got != 2000 {
		t.Fatalf("seq_tup_read = %d, want 2000 (destroyed connection's stats missed)\n%s", got, cost)
	}
}

func TestCounterCountsBatchAndCopy(t *testing.T) {
	pool, counter := itemsPool(t, 10, 0)
	ctx := context.Background()
	counter.Reset()
	exec(t, pool, `UPDATE items SET v = 0 WHERE id = 1`)
	rows, err := pool.Query(ctx, `SELECT id FROM items`)
	if err != nil {
		t.Fatal(err)
	}
	rows.Close()
	b := &pgx.Batch{}
	for i := range 5 {
		b.Queue(`UPDATE items SET v = $2 WHERE id = $1`, i+1, i)
	}
	if err := pool.SendBatch(ctx, b).Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.CopyFrom(ctx, pgx.Identifier{"filler"}, []string{"id"}, pgx.CopyFromRows([][]any{{1}, {2}})); err != nil {
		t.Fatal(err)
	}
	if got := counter.Statements(); got != 8 {
		t.Fatalf("statements %d, want 1+1+5+1=8\n%s", got, counter)
	}
	if got := counter.BySQL()["UPDATE items SET v = $2 WHERE id = $1"]; got != 5 {
		t.Fatalf("batched update counted %d, want 5\n%s", got, counter)
	}
}

// perItem runs one statement per item; rescan makes each one scan the
// whole table (quadratic), otherwise each is a primary key lookup.
func perItem(rescan bool, filler int) func(t testing.TB, n int) Cost {
	return func(t testing.TB, n int) Cost {
		pool, counter := itemsPool(t, n, filler)
		ctx := context.Background()
		return Measure(t, pool, counter, func() {
			var sum int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM filler`).Scan(&sum); err != nil {
				t.Fatal(err)
			}
			q := `SELECT v FROM items WHERE id = $1`
			if rescan {
				q = `SELECT sum(v) FROM items WHERE grp = $1`
			}
			for i := 1; i <= n; i++ {
				var v int
				if err := pool.QueryRow(ctx, q, i).Scan(&v); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestAssertScalingLinearPasses(t *testing.T) {
	AssertScaling(t, Linear, 60, 8, perItem(false, 0))
}

func TestAssertScalingCatchesQuadratic(t *testing.T) {
	r := &recorder{TB: t}
	AssertScaling(r, Linear, 60, 8, perItem(true, 0))
	if msg := r.failed(); !strings.Contains(msg, "public.items") || !strings.Contains(msg, "grows past bound") {
		t.Fatalf("quadratic operation passed or lacked a per-table breakdown: %q", msg)
	}
}

// A large fixed cost (a 20000-row scan per operation) pulls the raw ratio
// of a quadratic operation at n=60, k=8 down to about 8; the size-1
// baseline subtraction must still expose it, and must not fail the linear
// operation with the same overhead.
func TestAssertScalingBaselineExposesQuadraticUnderFixedCost(t *testing.T) {
	AssertScaling(t, Linear, 60, 8, perItem(false, 20000))
	r := &recorder{TB: t}
	AssertScaling(r, Linear, 60, 8, perItem(true, 20000))
	if r.failed() == "" {
		t.Fatal("quadratic operation with a large fixed cost passed")
	}
}

// Tuples deleted earlier in the transaction are invisible to later scans,
// so seq_tup_read does not count them, yet each scan still passes over
// their pages. This is how a foreign-key action after a cascade scans: a
// scan per deleted row over the rows already deleted. The rows metric
// alone sees it as linear.
func TestAssertScalingCatchesQuadraticOverDeadTuples(t *testing.T) {
	deadScan := func(t testing.TB, n int) Cost {
		pool, counter := itemsPool(t, n, 0)
		ctx := context.Background()
		return Measure(t, pool, counter, func() {
			err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `DELETE FROM items`); err != nil {
					return err
				}
				for i := 1; i <= n; i++ {
					if _, err := tx.Exec(ctx, `SELECT count(*) FROM items WHERE grp = $1`, i); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	r := &recorder{TB: t}
	AssertScaling(r, Linear, 60, 8, deadScan)
	if msg := r.failed(); !strings.Contains(msg, "seq pages") {
		t.Fatalf("quadratic scans over dead tuples passed the linear class: %q", msg)
	}
}

func TestAssertScalingConstant(t *testing.T) {
	lookup := func(t testing.TB, n int) Cost {
		pool, counter := itemsPool(t, n, 0)
		return Measure(t, pool, counter, func() {
			exec(t, pool, `SELECT v FROM items WHERE id = 1`)
		})
	}
	AssertScaling(t, Constant, 60, 8, lookup)
	fullScan := func(t testing.TB, n int) Cost {
		pool, counter := itemsPool(t, n, 0)
		return Measure(t, pool, counter, func() {
			exec(t, pool, `SELECT max(v) FROM items`)
		})
	}
	r := &recorder{TB: t}
	AssertScaling(r, Constant, 60, 8, fullScan)
	if r.failed() == "" {
		t.Fatal("full scan passed the constant class")
	}
	// At k=2 a linear operation grows ×2, which a ×2 bound let through.
	r = &recorder{TB: t}
	AssertScaling(r, Constant, 60, 2, fullScan)
	if r.failed() == "" {
		t.Fatal("full scan passed the constant class at k=2")
	}
	// A large fixed cost (a 20000-row scan) must not hide a linear term
	// from the constant class either.
	fixedPlusScan := func(t testing.TB, n int) Cost {
		pool, counter := itemsPool(t, n, 20000)
		return Measure(t, pool, counter, func() {
			exec(t, pool, `SELECT count(*) FROM filler`)
			exec(t, pool, `SELECT max(v) FROM items`)
		})
	}
	r = &recorder{TB: t}
	AssertScaling(r, Constant, 60, 8, fixedPlusScan)
	if r.failed() == "" {
		t.Fatal("full scan behind a large fixed cost passed the constant class")
	}
}

func TestAssertIndexedPlan(t *testing.T) {
	pool, _ := itemsPool(t, 100, 0)
	exec(t, pool, `CREATE INDEX items_v ON items (v)`)
	exec(t, pool, `ANALYZE items`)
	AssertIndexedPlan(t, pool, `SELECT * FROM items WHERE id = $1`, 5)
	AssertIndexedPlan(t, pool, `SELECT * FROM items ORDER BY id LIMIT 10`)
	AssertIndexedPlan(t, pool, `SELECT * FROM items WHERE id > $1 ORDER BY id LIMIT 10`, 5)
	AssertIndexedPlan(t, pool, `SELECT max(id) FROM items`)
	AssertIndexedPlan(t, pool, `SELECT * FROM items i LEFT JOIN filler f ON f.id = i.grp ORDER BY i.id LIMIT 10`)

	for _, tc := range []struct {
		name, query string
		args        []any
	}{
		{"unindexed predicate", `SELECT * FROM items WHERE grp = $1`, []any{5}},
		{"cast defeats primary key", `SELECT * FROM items WHERE id::text > $1`, []any{"5"}},
		// With seq scans off the planner reads the whole items_v index
		// instead; the scan has a Filter but no Index Cond.
		{"full index scan fallback", `SELECT * FROM items WHERE note LIKE $1 ORDER BY v`, []any{"%7"}},
		{"sort above full scan", `SELECT * FROM items ORDER BY note LIMIT 10`, nil},
		// A Limit does not bound a scan that filters: it reads until it
		// finds enough matching rows, the whole index when few match.
		{"limit over filtered full index scan", `SELECT * FROM items WHERE grp = $1 ORDER BY id LIMIT 10`, []any{5}},
		{"max() of unindexed group per row", `SELECT id, (SELECT max(i2.id) FROM items i2 WHERE i2.grp = items.v) FROM items ORDER BY id LIMIT 10`, nil},
		// An inner join can drop outer rows, so the Limit does not
		// bound the outer scan either.
		{"limit over inner join", `SELECT i.* FROM items i JOIN filler f ON f.id = i.grp ORDER BY i.id LIMIT 10`, nil},
		// An init plan runs to completion, whatever the Limit above it.
		{"init plan under limit", `SELECT id, ARRAY(SELECT id FROM items ORDER BY id) FROM items ORDER BY id LIMIT 10`, nil},
	} {
		r := &recorder{TB: t}
		AssertIndexedPlan(r, pool, tc.query, tc.args...)
		msg := r.failed()
		t.Logf("%s: %s", tc.name, strings.SplitN(msg, "\n", 2)[0])
		if !strings.Contains(msg, "on items") || !strings.Contains(msg, `"Node Type"`) {
			t.Errorf("%s: not caught, or plan not printed: %q", tc.name, msg)
		}
	}
	AssertIndexedPlanExcept(t, pool, []string{"items"}, `SELECT * FROM items WHERE grp = $1`, 5)
}

func TestAssertPlanUsesIndex(t *testing.T) {
	pool, _ := itemsPool(t, 100, 0)
	exec(t, pool, `CREATE INDEX items_grp_v ON items (grp, v)`)
	exec(t, pool, `CREATE INDEX items_v ON items (v)`)
	exec(t, pool, `ANALYZE items`)
	const query = `SELECT count(*) FROM items WHERE v = $1`
	AssertPlanUsesIndex(t, pool, "items_v", query, 3)

	// Without items_v the planner serves v = $1 from items_grp_v: an
	// Index Cond on its second column, which reads the whole index.
	// AssertIndexedPlan accepts that plan; AssertPlanUsesIndex does not.
	exec(t, pool, `DROP INDEX items_v`)
	AssertIndexedPlan(t, pool, query, 3)
	r := &recorder{TB: t}
	AssertPlanUsesIndex(r, pool, "items_v", query, 3)
	if msg := r.failed(); !strings.Contains(msg, "does not probe index items_v") || !strings.Contains(msg, "items_grp_v") {
		t.Fatalf("missing index not caught, or plan not printed: %q", msg)
	}
	// With no index on v at all, the full-scan check fails too.
	exec(t, pool, `DROP INDEX items_grp_v`)
	r = &recorder{TB: t}
	AssertPlanUsesIndex(r, pool, "items_v", query, 3)
	if msg := r.failed(); !strings.Contains(msg, "unbounded scan") || !strings.Contains(msg, "does not probe index items_v") {
		t.Fatalf("unindexed query not caught: %q", msg)
	}
}

func TestIndexProbes(t *testing.T) {
	plan := `[{"Plan": {"Node Type": "Aggregate", "Plans": [
	  {"Node Type": "Bitmap Heap Scan", "Relation Name": "a", "Plans": [
	    {"Node Type": "Bitmap Index Scan", "Index Name": "a_x", "Index Cond": "(x = 1)"}]},
	  {"Node Type": "Index Scan", "Relation Name": "b", "Index Name": "b_pkey", "Parent Relationship": "SubPlan"},
	  {"Node Type": "Index Only Scan", "Relation Name": "c", "Index Name": "c_y", "Index Cond": "(y = 1)"},
	  {"Node Type": "Index Scan", "Relation Name": "a", "Index Name": "a_x", "Index Cond": "(x = 2)"}]}}]`
	if got := IndexProbes(plan); !slices.Equal(got, []string{"a_x", "c_y"}) {
		t.Fatalf("probes %v", got)
	}
}

func TestFullScansNestedLoopInner(t *testing.T) {
	// A Limit stops the outer side of a nested loop early, not the inner
	// side, which is rescanned per outer row.
	plan := `[{"Plan": {"Node Type": "Limit", "Plans": [{"Node Type": "Nested Loop", "Join Type": "Left", "Plans": [
	  {"Node Type": "Index Scan", "Parent Relationship": "Outer", "Relation Name": "a", "Index Name": "a_pkey"},
	  {"Node Type": "Index Scan", "Parent Relationship": "Inner", "Relation Name": "b", "Index Name": "b_pkey"}]}]}}]`
	got := FullScans(plan)
	if len(got) != 1 || got[0].Relation != "b" {
		t.Fatalf("FullScans = %v, want only the inner scan of b", got)
	}
}

func TestFullScansLimitedOuterOnlyThroughLeftJoin(t *testing.T) {
	for join, want := range map[string]int{"Left": 0, "Inner": 1, "Semi": 1, "Anti": 1} {
		plan := `[{"Plan": {"Node Type": "Limit", "Plans": [{"Node Type": "Nested Loop", "Parent Relationship": "Outer", "Join Type": "` + join + `", "Plans": [
		  {"Node Type": "Index Scan", "Parent Relationship": "Outer", "Relation Name": "a", "Index Name": "a_pkey"},
		  {"Node Type": "Index Scan", "Parent Relationship": "Inner", "Relation Name": "b", "Index Name": "b_pkey", "Index Cond": "(id = a.b_id)"}]}]}}]`
		if got := FullScans(plan); len(got) != want {
			t.Errorf("%s join: FullScans = %v, want %d", join, got, want)
		}
	}
}

func TestClaudeTranscript(t *testing.T) {
	const n = 501
	data := ClaudeTranscript(n)
	if !bytes.Equal(data, ClaudeTranscript(n)) {
		t.Fatal("generator is not deterministic")
	}
	s := ClaudeSession{}
	if !bytes.Equal(data, append(s.Lines(0, 200), s.Lines(200, n)...)) {
		t.Fatal("Lines(0,n) != Lines(0,m)+Lines(m,n)")
	}
	if got := bytes.Count(data, []byte("\n")); got != n {
		t.Fatalf("%d lines, want %d", got, n)
	}
	src := transcript.Source{Agent: transcript.AgentClaude, Path: "/home/u/.claude/projects/-workspace-perf/" + s.withDefaults().SessionID + ".jsonl",
		StorageKind: transcript.StorageJSONLAppend, Parser: claude.ParserName}
	var c transcript.Collector
	if _, err := (&claude.Parser{}).Parse(context.Background(), transcript.Input{Source: &src, R: bytes.NewReader(data), Size: int64(len(data))}, transcript.Cursor{}, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Messages) != n {
		t.Fatalf("parsed %d messages, want %d", len(c.Messages), n)
	}
	kinds := map[transcript.Kind]int{}
	for _, m := range c.Messages {
		kinds[m.Kind]++
	}
	want := map[transcript.Kind]int{transcript.KindUser: 126, transcript.KindAssistant: 125, transcript.KindToolCall: 125, transcript.KindToolResult: 125}
	for k, w := range want {
		if kinds[k] != w {
			t.Errorf("kind %v: %d messages, want %d (all: %v)", k, kinds[k], w, kinds)
		}
	}
}
