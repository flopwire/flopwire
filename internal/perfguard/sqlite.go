package perfguard

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// SQLiteDriver is the database/sql driver name of a wrapper around
// modernc.org/sqlite (the "sqlite" driver) that measures the connections
// whose DSN a SQLiteCounter claims (see CountSQLite). Other connections
// are the plain driver's, unwrapped.
const SQLiteDriver = "perfguard-sqlite"

var base driver.Driver

func init() {
	db, err := sql.Open("sqlite", "")
	if err != nil {
		panic(err)
	}
	base = db.Driver() // the registered driver, with its package-level registrations
	_ = db.Close()
	sql.Register(SQLiteDriver, sqliteDriver{})
}

// SQLiteCounter measures the work of the SQLite connections it claims:
// statements executed (one per Exec or Query, BEGIN and COMMIT included;
// a multi-statement Exec counts once), rows written per table (inserts,
// updates and deletes, through the pre-update hook, FTS shadow tables
// included), and b-tree pages fetched (page cache hits plus misses). It
// is safe for concurrent use.
type SQLiteCounter struct {
	key string

	mu     sync.Mutex
	total  int64
	bySQL  map[string]int64
	writes map[string]TableCost
	pages  int64
}

var counters struct {
	sync.Mutex
	m map[string]*SQLiteCounter
}

// CountSQLite returns a counter for every connection SQLiteDriver opens
// from now on whose DSN contains key, normally a path under the test's
// t.TempDir(). It is dropped when the test ends.
func CountSQLite(t testing.TB, key string) *SQLiteCounter {
	t.Helper()
	if key == "" {
		t.Fatal("perfguard: CountSQLite needs a nonempty key")
	}
	c := &SQLiteCounter{key: key}
	counters.Lock()
	defer counters.Unlock()
	if counters.m == nil {
		counters.m = map[string]*SQLiteCounter{}
	}
	if _, dup := counters.m[key]; dup {
		t.Fatalf("perfguard: %q is already counted", key)
	}
	counters.m[key] = c
	t.Cleanup(func() {
		counters.Lock()
		delete(counters.m, key)
		counters.Unlock()
	})
	return c
}

func counterFor(dsn string) *SQLiteCounter {
	counters.Lock()
	defer counters.Unlock()
	var best *SQLiteCounter
	for k, c := range counters.m {
		if strings.Contains(dsn, k) && (best == nil || len(k) > len(best.key)) {
			best = c
		}
	}
	return best
}

// OpenSQLite opens dsn through SQLiteDriver with one connection, the way
// the stores open their write handles.
func OpenSQLite(t testing.TB, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open(SQLiteDriver, dsn)
	if err != nil {
		t.Fatalf("perfguard: open %s: %v", dsn, err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// MeasureSQLite runs fn and returns the cost it caused on c's
// connections: statements, rows written per table (Tables, insert,
// update and delete counts) and pages fetched (Pages). fn must finish
// its statements (rows closed) before it returns.
func MeasureSQLite(c *SQLiteCounter, fn func()) Cost {
	before := c.snapshot()
	fn()
	after := c.snapshot()
	cost := Cost{Tables: map[string]TableCost{}, Statements: after.total - before.total, SQLitePages: after.pages - before.pages}
	for name, w := range after.writes {
		if d := w.sub(before.writes[name]); d != (TableCost{}) {
			cost.Tables[name] = d
		}
	}
	return cost
}

type sqliteSnap struct {
	total, pages int64
	writes       map[string]TableCost
}

func (c *SQLiteCounter) snapshot() sqliteSnap {
	c.mu.Lock()
	defer c.mu.Unlock()
	return sqliteSnap{c.total, c.pages, maps.Clone(c.writes)}
}

// Statements is the number of statements counted so far.
func (c *SQLiteCounter) Statements() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

// BySQL returns the count per statement text (whitespace collapsed).
func (c *SQLiteCounter) BySQL() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.bySQL)
}

// Reset sets every count to zero.
func (c *SQLiteCounter) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total, c.bySQL, c.writes, c.pages = 0, nil, nil, 0
}

// String lists statements by descending count, for failure messages.
func (c *SQLiteCounter) String() string {
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

func (c *SQLiteCounter) statement(query string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bySQL == nil {
		c.bySQL = map[string]int64{}
	}
	c.total++
	c.bySQL[normalize(query)]++
}

func (c *SQLiteCounter) write(d sqlite.SQLitePreUpdateData) {
	name := d.DatabaseName + "." + d.TableName
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writes == nil {
		c.writes = map[string]TableCost{}
	}
	w := c.writes[name]
	switch d.Op {
	case sqlite3.SQLITE_INSERT:
		w.TupIns++
	case sqlite3.SQLITE_UPDATE:
		w.TupUpd++
	case sqlite3.SQLITE_DELETE:
		w.TupDel++
	}
	c.writes[name] = w
}

// sqliteDriver opens connections through the registered "sqlite" driver
// and wraps those a counter claims.
type sqliteDriver struct{}

func (sqliteDriver) Open(dsn string) (driver.Conn, error) {
	conn, err := base.Open(dsn)
	if err != nil {
		return nil, err
	}
	c := counterFor(dsn)
	if c == nil {
		return conn, nil
	}
	st, ok := conn.(sqlite.DBStatus)
	hooks, ok2 := conn.(sqlite.HookRegisterer)
	if !ok || !ok2 {
		conn.Close()
		return nil, fmt.Errorf("perfguard: %T lacks DBStatus or hooks", conn)
	}
	hooks.RegisterPreUpdateHook(c.write)
	w := &countedConn{Conn: conn, c: c, st: st}
	w.pagesSince() // the connection's own setup does not count
	return w, nil
}

// countedConn counts the statements run on a modernc connection and the
// pages they fetch. database/sql never uses a connection concurrently,
// so reading its status after each statement is safe.
type countedConn struct {
	driver.Conn
	c  *SQLiteCounter
	st sqlite.DBStatus
}

// pagesSince returns the pages fetched since the last call, resetting
// the connection's counters.
func (w *countedConn) pagesSince() int64 {
	hit, _, _ := w.st.Status(sqlite.DBStatusCacheHit, true)
	miss, _, _ := w.st.Status(sqlite.DBStatusCacheMiss, true)
	return int64(hit + miss)
}

func (w *countedConn) done() {
	p := w.pagesSince()
	w.c.mu.Lock()
	w.c.pages += p
	w.c.mu.Unlock()
}

func (w *countedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	w.c.statement("BEGIN")
	tx, err := w.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	w.done()
	if err != nil {
		return nil, err
	}
	return &countedTx{tx, w}, nil
}

type countedTx struct {
	driver.Tx
	w *countedConn
}

func (t *countedTx) Commit() error {
	t.w.c.statement("COMMIT")
	defer t.w.done()
	return t.Tx.Commit()
}

func (t *countedTx) Rollback() error {
	t.w.c.statement("ROLLBACK")
	defer t.w.done()
	return t.Tx.Rollback()
}

func (w *countedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	w.c.statement(query)
	defer w.done()
	return w.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (w *countedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	w.c.statement(query)
	rows, err := w.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err != nil {
		w.done()
		return nil, err
	}
	return &countedRows{Rows: rows, w: w}, nil
}

func (w *countedConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	st, err := w.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, query)
	w.done() // preparing reads the schema, not data, but keep it out of the next statement
	if err != nil {
		return nil, err
	}
	return &countedStmt{Stmt: st, w: w, query: query}, nil
}

func (w *countedConn) Ping(ctx context.Context) error {
	if p, ok := w.Conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (w *countedConn) ResetSession(ctx context.Context) error {
	if r, ok := w.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (w *countedConn) IsValid() bool {
	if v, ok := w.Conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

// Raw access (sql.Conn.Raw) reaches the modernc connection.
func (w *countedConn) Unwrap() driver.Conn { return w.Conn }

type countedStmt struct {
	driver.Stmt
	w     *countedConn
	query string
}

func (s *countedStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	s.w.c.statement(s.query)
	defer s.w.done()
	return s.Stmt.(driver.StmtExecContext).ExecContext(ctx, args)
}

func (s *countedStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	s.w.c.statement(s.query)
	rows, err := s.Stmt.(driver.StmtQueryContext).QueryContext(ctx, args)
	if err != nil {
		s.w.done()
		return nil, err
	}
	return &countedRows{Rows: rows, w: s.w}, nil
}

// countedRows adds the pages fetched while reading to the counter when
// the rows close. It forwards the column type methods the modernc rows
// implement.
type countedRows struct {
	driver.Rows
	w *countedConn
}

func (r *countedRows) Close() error {
	defer r.w.done()
	return r.Rows.Close()
}

func (r *countedRows) ColumnTypeDatabaseTypeName(i int) string {
	return r.Rows.(driver.RowsColumnTypeDatabaseTypeName).ColumnTypeDatabaseTypeName(i)
}

func (r *countedRows) ColumnTypeLength(i int) (int64, bool) {
	return r.Rows.(driver.RowsColumnTypeLength).ColumnTypeLength(i)
}

func (r *countedRows) ColumnTypeNullable(i int) (bool, bool) {
	return r.Rows.(driver.RowsColumnTypeNullable).ColumnTypeNullable(i)
}

func (r *countedRows) ColumnTypePrecisionScale(i int) (int64, int64, bool) {
	return r.Rows.(driver.RowsColumnTypePrecisionScale).ColumnTypePrecisionScale(i)
}

func (r *countedRows) ColumnTypeScanType(i int) reflect.Type {
	return r.Rows.(driver.RowsColumnTypeScanType).ColumnTypeScanType(i)
}

// Querier is satisfied by *sql.DB, *sql.Conn and *sql.Tx.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// SQLitePlan returns the EXPLAIN QUERY PLAN detail lines of query, one
// per plan node, indented by depth.
func SQLitePlan(db Querier, query string, args ...any) ([]string, error) {
	rows, err := db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	depth := map[int]int{}
	var out []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			return nil, err
		}
		d := 0
		if parent != 0 {
			d = depth[parent] + 1
		}
		depth[id] = d
		out = append(out, strings.Repeat("  ", d)+detail)
	}
	return out, rows.Err()
}

// AssertSQLitePlan fails t when SQLite's plan for query reads a whole
// table or sorts: a "SCAN" of a table (a covering-index scan included,
// it still reads every entry) other than those in allow, or a "USE TEMP
// B-TREE" (ORDER BY, GROUP BY or DISTINCT without an index that yields
// that order), or an automatic index (SQLite reads the whole table to
// build a transient index because no index serves the lookup). Scans of FTS5 tables and of subquery results (SCAN
// (subquery-N), SCAN CONSTANT ROW) are allowed. The plan is printed on
// failure.
func AssertSQLitePlan(t testing.TB, db Querier, allow []string, query string, args ...any) {
	t.Helper()
	plan, err := SQLitePlan(db, query, args...)
	if err != nil {
		t.Fatalf("perfguard: explain query plan: %v\nquery: %s", err, query)
	}
	if bad := SQLiteFullScans(plan, allow); len(bad) > 0 {
		t.Errorf("perfguard: unbounded SQLite plan: %s\nquery: %s\nplan:\n%s", strings.Join(bad, ", "), query, strings.Join(plan, "\n"))
	}
}

// SQLiteFullScans returns the plan lines that scan a whole table, use a
// temporary b-tree or build an automatic index, skipping scans of the tables in allow.
func SQLiteFullScans(plan []string, allow []string) []string {
	var bad []string
	for _, line := range plan {
		d := strings.TrimSpace(line)
		switch {
		case strings.Contains(d, "TEMP B-TREE"), strings.Contains(d, "AUTOMATIC"):
			bad = append(bad, d)
		case strings.HasPrefix(d, "SCAN "):
			f := strings.Fields(d)
			table := f[1]
			if strings.HasPrefix(table, "(") || table == "CONSTANT" || strings.Contains(d, "VIRTUAL TABLE INDEX") {
				continue
			}
			if slices.Contains(allow, table) {
				continue
			}
			bad = append(bad, d)
		}
	}
	return bad
}
