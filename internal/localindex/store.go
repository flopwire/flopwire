// Package localindex is the device-local SQLite index (spec §4, §6.3, §8 of
// notes/local-search/README.md): the §4 message model plus two contentless
// FTS5 tables, one for ranked token search and one trigram table for exact
// substring and regex candidate selection.
//
// Concurrency: one writer goroutine owns a single write connection and runs
// every mutation in its own transaction; queries use a separate read-only
// pool. The database runs in WAL mode, so readers never block the writer.
//
// The index is derived data. It is rebuildable from the harness files, holds
// the owner's own data unredacted, and keeps the extracted text of every row
// (zstd-compressed) so search survives a harness deleting its transcript.
package localindex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver (pure Go)
)

// Detail is an FTS5 detail level.
type Detail string

const (
	// DetailColumn stores which rows contain a token but not its positions:
	// a smaller index, no phrase queries, and term frequency saturating at
	// one per row. Queries here never use phrases, and find verifies every
	// candidate against the stored text, so both tables work at either level.
	DetailColumn Detail = "column"
	// DetailFull stores token positions.
	DetailFull Detail = "full"
)

// Details are the detail levels of the two FTS tables, and how many parts
// the trigram table is split into.
type Details struct {
	Tok      Detail // fts_tok
	Tri      Detail // fts_tri
	TriParts int
}

// Options configure Open.
type Options struct {
	// DeviceID stamps sources and conversations. Defaults to "local".
	DeviceID string
	// TokDetail and TriDetail apply when the database is created; an
	// existing database keeps the levels it was created with.
	TokDetail Detail
	TriDetail Detail
	// TriParts splits the trigram table into this many files, each with
	// its own writer (row id modulo TriParts). Default 2. Fixed at creation.
	TriParts int
	// ReadConns bounds the read pool (at least 2). Defaults to GOMAXPROCS.
	ReadConns int
	// RepoRoot resolves a cwd to its git root when a conversation is first
	// seen. Defaults to FindRepoRoot (walks up looking for .git).
	RepoRoot func(cwd string) string
	// ReadOnly opens an existing index for queries only (the CLI and the
	// MCP server): no lock, no schema or shard writes, no FTS queue replay.
	// Writes return ErrReadOnly. Queries see what the agent's shards have
	// committed, which can lag the main database by about a second.
	ReadOnly bool
	// LockFile is an index lock the caller already holds (Store.LockFile,
	// passed across an agent's re-exec). A writing Open takes the lock
	// itself when it is nil.
	LockFile *os.File
	// WriteCacheMB is the write connection's page cache. Default 16.
	WriteCacheMB int
	// ShardCacheMB is each FTS shard writer's page cache. Default 8.
	ShardCacheMB int
	// DeferCommit answers writes before their transaction commits, keeping
	// it open for more (see writer). Readers of the read pool may lag up to
	// a second behind; Sync waits for the commit, and every read the store
	// offers for sources runs on the writer, so it sees its own writes. If
	// a deferred commit fails, OnCommitError is called: the rows and
	// watermarks of the lost transaction are gone together.
	DeferCommit   bool
	OnCommitError func(error)
	// SyncOnly keeps only the agent's bookkeeping (sources, watermarks,
	// conversations, companions, placements): no message rows and no FTS
	// shards (ModeSyncOnly). The mode is stored in the index; opening it in
	// the other mode rebuilds it. A read-only Open of a sync-only index
	// fails with ErrSyncOnly.
	SyncOnly bool
	// ReadCacheMB is each read connection's page cache. Default 16. The
	// agent, which reads only source rows, keeps it small: every pooled
	// connection fills its cache as the database grows.
	ReadCacheMB int
}

// Store is an open local index.
type Store struct {
	path    string
	opts    Options
	wdb     *sql.DB // exactly one connection, used only by the writer goroutine
	rdb     *sql.DB // read pool
	reqs    chan writeReq
	quit    chan struct{}
	wg      sync.WaitGroup
	once    sync.Once
	details Details
	shards  []*ftsShard // fts_tok, then the fts_tri parts
	tri     []*ftsShard // the fts_tri parts
	lastSeq int64       // highest fts_queue sequence handed to the shards (writer only)

	readOnly bool
	tombs    *tombstones // local message redactions (writing stores)
	// reconcileDue: a transaction that may have held redaction masks was
	// lost; the writer applies the sidecar again first thing (redact.go).
	reconcileDue atomic.Bool
	lock         *os.File // the index lock (writing stores)
	keepLock     bool     // LockFile handed the lock to the caller
}

type writeReq struct {
	ctx  context.Context
	fn   func(*writeTx) error
	done chan error
	wait bool // answer at commit, with its error, even with DeferCommit
}

// ErrClosed is returned by writes after Close.
var ErrClosed = errors.New("localindex: store closed")

// ErrReadOnly is returned by writes to a store opened with ReadOnly.
var ErrReadOnly = errors.New("localindex: store is read-only")

// dsn builds a connection string. The driver runs _pragma entries in
// lexicographic order, so a file that needs a page size other than the
// default passes noWAL and sets page_size, then journal_mode, itself
// (journal_mode=WAL fixes the page size of a new file).
func dsn(path string, readOnly bool, cacheMB int, noWAL bool) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	if !noWAL {
		q.Add("_pragma", "journal_mode(WAL)")
	}
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(OFF)")
	q.Add("_pragma", "temp_store(MEMORY)")                         // statement journals stay off disk
	q.Add("_pragma", fmt.Sprintf("cache_size(-%d)", cacheMB*1000)) // page cache per connection
	if readOnly {
		q.Add("_pragma", "query_only(1)")
	}
	q.Set("_txlock", "immediate")
	return "file:" + path + "?" + q.Encode()
}

// Open opens (creating if needed) the index at path. A writing Open takes
// the index lock and fails with *LockedError when another process holds
// it; a ReadOnly Open needs an existing index and takes no lock.
func Open(path string, opts Options) (*Store, error) {
	if opts.DeviceID == "" {
		opts.DeviceID = "local"
	}
	if opts.TokDetail == "" {
		opts.TokDetail = DefaultTokDetail
	}
	if opts.TriDetail == "" {
		opts.TriDetail = DefaultTriDetail
	}
	if opts.TriParts <= 0 {
		opts.TriParts = 2
	}
	// At least two: find walks candidates on one connection while it
	// loads their rows on another.
	if opts.ReadConns <= 0 {
		opts.ReadConns = runtime.GOMAXPROCS(0)
	}
	opts.ReadConns = max(opts.ReadConns, 2)
	if opts.RepoRoot == nil {
		opts.RepoRoot = NewRepoRootCache().Resolve
	}
	if opts.WriteCacheMB <= 0 {
		opts.WriteCacheMB = 16
	}
	if opts.ShardCacheMB <= 0 {
		opts.ShardCacheMB = 8
	}
	if opts.ReadCacheMB <= 0 {
		opts.ReadCacheMB = 16
	}
	if opts.ReadOnly {
		return openReadOnly(path, opts)
	}
	lock, err := acquireLock(path, opts.LockFile)
	if err != nil {
		return nil, err
	}
	s, err := openWriter(path, opts)
	if err != nil {
		lock.Close()
		return nil, err
	}
	s.lock = lock
	return s, nil
}

// sqliteDriver is the database/sql driver of the write, shard and read
// connections. Tests set it to perfguard.SQLiteDriver, which wraps
// "sqlite" to count statements and pages on the connections a test claims.
var sqliteDriver = "sqlite"

func openWriter(path string, opts Options) (*Store, error) {
	wdb, err := sql.Open(sqliteDriver, dsn(path, false, opts.WriteCacheMB, false))
	if err != nil {
		return nil, err
	}
	wdb.SetMaxOpenConns(1)
	wdb.SetMaxIdleConns(1)
	wdb.SetConnMaxLifetime(0)
	mode := ModeFull
	if opts.SyncOnly {
		mode = ModeSyncOnly
	}
	details, rebuilt, err := migrate(wdb, Details{Tok: opts.TokDetail, Tri: opts.TriDetail, TriParts: opts.TriParts}, mode)
	if err != nil {
		wdb.Close()
		return nil, fmt.Errorf("localindex: migrate: %w", err)
	}
	if opts.SyncOnly {
		// No shards: remove any a full index left, and give the space the
		// dropped message rows held back to the file system.
		if err := removeShards(path); err != nil {
			wdb.Close()
			return nil, err
		}
		if rebuilt {
			if _, err := wdb.Exec(`VACUUM`); err != nil {
				wdb.Close()
				return nil, fmt.Errorf("localindex: vacuum: %w", err)
			}
		}
		details.TriParts = 0
	}
	var indexID int64
	if err := wdb.QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'index_id'`).Scan(&indexID); err != nil {
		wdb.Close()
		return nil, fmt.Errorf("localindex: index id: %w", err)
	}
	s := &Store{path: path, opts: opts, wdb: wdb, reqs: make(chan writeReq), quit: make(chan struct{}), details: details}
	for i := -1; i < details.TriParts && !opts.SyncOnly; i++ {
		schema, table, tokenize, detail := "tok", "fts_tok", tokTokenize, details.Tok
		if i >= 0 {
			schema, table, tokenize, detail = "tri"+strconv.Itoa(i), "fts_tri", triTokenize, details.Tri
		}
		shard, err := openShard(path, indexID, schema, table, detail, tokenize, opts.ShardCacheMB)
		if err != nil {
			s.closeShards()
			wdb.Close()
			return nil, err
		}
		if i >= 0 {
			shard.tri, shard.part, shard.parts = true, i, details.TriParts
			s.tri = append(s.tri, shard)
		}
		s.shards = append(s.shards, shard)
	}
	if err := s.checkShards(context.Background()); err != nil {
		s.closeShards()
		wdb.Close()
		return nil, fmt.Errorf("localindex: rebuild FTS shard: %w", err)
	}
	if err := s.recoverShards(context.Background()); err != nil {
		s.closeShards()
		wdb.Close()
		return nil, fmt.Errorf("localindex: replay FTS queue: %w", err)
	}
	if err := s.loadTombstones(); err != nil {
		s.closeShards()
		wdb.Close()
		return nil, fmt.Errorf("localindex: redactions: %w", err)
	}
	s.rdb = sql.OpenDB(&attachConnector{dsn: dsn(path, true, opts.ReadCacheMB, false), shards: s.shards})
	s.rdb.SetMaxOpenConns(opts.ReadConns)
	s.rdb.SetMaxIdleConns(opts.ReadConns)
	s.wg.Add(1)
	go s.writer()
	// Redactions the sidecar holds and the rows may not reflect (a lost
	// transaction, a sidecar beside a new database).
	if err := s.writeWait(context.Background(), func(w *writeTx) error { return w.reconcile() }); err != nil {
		s.Close()
		return nil, fmt.Errorf("localindex: apply redactions: %w", err)
	}
	return s, nil
}

func (s *Store) closeShards() error {
	var errs []error
	for _, sh := range s.shards {
		errs = append(errs, sh.close())
	}
	return errors.Join(errs...)
}

// Path is the database file, for callers that keep their own tables in the
// same database (devicesync's devsync_ tables) on a connection of their own.
func (s *Store) Path() string { return s.path }

// Details reports the FTS tables' detail levels.
func (s *Store) Details() Details { return s.details }

// Defaults for new databases; see the PR notes for the measurements. The
// token table keeps positions: FTS5's bm25 over a contentless table at
// detail=column scores every row 0 (no instance counts), which left
// ranked search ordered by row id alone.
const (
	DefaultTokDetail = DetailFull
	DefaultTriDetail = DetailFull
)

// Close stops the writer, closes both pools and releases the index lock.
func (s *Store) Close() error {
	if s.readOnly {
		return s.rdb.Close()
	}
	s.once.Do(func() { close(s.quit) })
	for _, sh := range s.shards {
		sh.drain()
	}
	s.wg.Wait()
	err := errors.Join(s.rdb.Close(), s.closeShards(), s.wdb.Close())
	if s.lock != nil && !s.keepLock {
		err = errors.Join(err, s.lock.Close())
	}
	return err
}

// openReadOnly opens an existing index for queries: the read pool only,
// with the shard files attached. It writes nothing.
func openReadOnly(path string, opts Options) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn(path, true, 1, false))
	if err != nil {
		return nil, err
	}
	var v int
	var d Details
	var mode string
	err = db.QueryRow(`PRAGMA user_version`).Scan(&v)
	if err == nil && v != schemaVersion {
		err = fmt.Errorf("localindex: %s has schema version %d, want %d: the agent rebuilds it on its next start", path, v, schemaVersion)
	}
	if err == nil {
		err = db.QueryRow(`SELECT (SELECT value FROM meta WHERE key='tok_detail'), (SELECT value FROM meta WHERE key='tri_detail'),
			(SELECT CAST(value AS INTEGER) FROM meta WHERE key='tri_parts'), ifnull((SELECT value FROM meta WHERE key='mode'), '')`).Scan(&d.Tok, &d.Tri, &d.TriParts, &mode)
	}
	if err == nil && mode == ModeSyncOnly {
		err = ErrSyncOnly
	}
	db.Close()
	if err != nil {
		return nil, err
	}
	s := &Store{path: path, opts: opts, details: d, readOnly: true}
	for i := -1; i < d.TriParts; i++ {
		sh := &ftsShard{table: "fts_tok", schema: "tok"}
		if i >= 0 {
			sh = &ftsShard{table: "fts_tri", schema: "tri" + strconv.Itoa(i), tri: true, part: i, parts: d.TriParts}
			s.tri = append(s.tri, sh)
		}
		sh.path = shardPath(path, sh.schema)
		// ATTACH would create a missing file; the agent rebuilds it.
		if _, err := os.Stat(sh.path); err != nil {
			return nil, fmt.Errorf("localindex: FTS shard missing (the agent rebuilds it): %w", err)
		}
		s.shards = append(s.shards, sh)
	}
	s.rdb = sql.OpenDB(&attachConnector{dsn: dsn(path, true, opts.ReadCacheMB, false), shards: s.shards})
	s.rdb.SetMaxOpenConns(opts.ReadConns)
	s.rdb.SetMaxIdleConns(opts.ReadConns)
	return s, nil
}

// DB exposes the read pool for callers that need ad hoc queries (tests,
// diagnostics). Never write through it.
func (s *Store) DB() *sql.DB { return s.rdb }

// Transaction batching (Options.DeferCommit). The FTS writes of a
// transaction are applied at its end, so each FTS table gets one new
// segment per commit, sized by what the transaction indexed. Committing
// per request makes thousands of tiny segments when many small sources
// are indexed, and FTS5 then spends most of its time merging them.
const (
	commitIdle   = 50 * time.Millisecond // commit once no request arrived for this long
	commitMaxAge = time.Second           // or once the transaction is this old
	// or once this much text (per FTS table) awaits indexing. 2MB, not 8:
	// with the shard queue and FTS5 hash cut to match, a full-corpus bulk
	// load peaked at 274MB RSS instead of 519MB at the same wall time
	// (A4 measurements).
	commitFTSSize = 2 << 20
)

// writer runs write requests on the single write connection, several per
// transaction: requests queued while a transaction runs join it. Each
// request runs inside a savepoint, so a failing request is rolled back
// alone.
//
// Without DeferCommit a request is answered after the transaction that
// holds it commits, and the transaction commits as soon as the queue is
// empty. With DeferCommit a request is answered as soon as it ran, and the
// transaction stays open (commitIdle, commitMaxAge, commitFTSSize) so a
// bulk load builds few, large FTS segments; Sync waits for the commit.
func (s *Store) writer() {
	defer s.wg.Done()
	idle := time.NewTimer(time.Hour)
	defer idle.Stop()
	for {
		var r writeReq
		select {
		case <-s.quit:
			return
		case r = <-s.reqs:
		}
		tx, err := s.wdb.BeginTx(context.Background(), nil)
		if err != nil {
			r.done <- err
			continue
		}
		w := &writeTx{s: s, tx: tx, ctx: context.Background()}
		t := &txRun{w: w, started: time.Now()}
		if s.reconcileDue.Swap(false) {
			t.run(s.reconcileRequest())
		}
		t.run(r)
		for !t.full() {
			if !s.opts.DeferCommit || t.barrier {
				select {
				case r = <-s.reqs:
					t.run(r)
					continue
				default:
				}
				break
			}
			idle.Reset(commitIdle)
			select {
			case r = <-s.reqs:
				t.run(r)
				continue
			case <-idle.C:
			}
			break
		}
		t.commit()
	}
}

// txRun is one writer transaction and the requests it holds.
type txRun struct {
	w       *writeTx
	started time.Time
	waiting []writeReq // answered at commit
	barrier bool       // a Sync request is waiting: commit now
	n       int
}

func (t *txRun) full() bool {
	return t.barrier || time.Since(t.started) >= commitMaxAge || t.w.fts.bytes >= 2*commitFTSSize
}

func (t *txRun) run(r writeReq) {
	if r.fn == nil { // Sync barrier
		t.barrier = true
		t.waiting = append(t.waiting, r)
		return
	}
	if err := r.ctx.Err(); err != nil {
		r.done <- err
		return
	}
	w := t.w
	t.n++
	sp := "r" + itoa(int64(t.n))
	if _, err := w.tx.ExecContext(context.Background(), "SAVEPOINT "+sp); err != nil {
		r.done <- err
		return
	}
	mark := w.fts.mark()
	// Statements run with context.Background(), never r.ctx: cancelling a
	// statement interrupts SQLite, which rolls back the whole transaction
	// (every request it holds, some already answered), not the savepoint.
	// r.ctx is checked only before the request starts.
	err := r.fn(w)
	if err != nil {
		w.fts.undo(mark)
		_, rerr := w.tx.ExecContext(context.Background(), "ROLLBACK TO "+sp)
		if rerr == nil {
			_, rerr = w.tx.ExecContext(context.Background(), "RELEASE "+sp)
		}
		r.done <- err
		if rerr != nil {
			t.w.s.commitFailed(rerr)
		}
		return
	}
	if _, err := w.tx.ExecContext(context.Background(), "RELEASE "+sp); err != nil {
		r.done <- err
		return
	}
	if t.w.s.opts.DeferCommit && !r.wait {
		r.done <- nil
	} else {
		t.waiting = append(t.waiting, r)
		t.barrier = t.barrier || r.wait
	}
}

func (t *txRun) commit() {
	w, s := t.w, t.w.s
	defer w.closeStmts()
	w.ctx = context.Background()
	works, err := w.queueFTS()
	if err == nil && testHookCommit != nil {
		err = testHookCommit()
	}
	if err == nil {
		err = w.tx.Commit()
	} else {
		err = errors.Join(err, w.tx.Rollback())
	}
	if err == nil && works != nil {
		for i, sh := range s.shards {
			sh.submit(works[i])
		}
		s.lastSeq = works[0].seq
	}
	if err != nil {
		// The lost transaction may have held a redaction's row masks
		// whose tombstones are in the sidecar already.
		s.reconcileDue.Store(true)
	}
	if err != nil && s.opts.DeferCommit {
		s.commitFailed(err)
	}
	if err == nil && len(t.waiting) > 0 {
		// Answered requests are searchable: wait for the shards.
		for _, sh := range s.shards {
			if werr := sh.wait(s.lastSeq); werr != nil {
				err = werr
				s.commitFailed(werr)
			}
		}
	}
	for _, r := range t.waiting {
		r.done <- err
	}
}

// testHookCommit, when set, fails a commit with its error (tests).
var testHookCommit func() error

// commitFailed reports a lost deferred transaction: requests already
// answered were rolled back, rows and watermarks together, so the index is
// consistent but behind what callers were told.
func (s *Store) commitFailed(err error) {
	if s.opts.OnCommitError != nil {
		s.opts.OnCommitError(err)
	}
}

// ShrinkMemory releases the page caches of the writer connections (the
// main database and every FTS shard). An idle agent calls it so the memory
// a burst of indexing filled is not held while nothing is written.
func (s *Store) ShrinkMemory(ctx context.Context) error {
	if s.readOnly {
		return nil
	}
	err := s.write(ctx, func(w *writeTx) error {
		_, err := w.tx.ExecContext(w.ctx, `PRAGMA shrink_memory`)
		return err
	})
	for _, sh := range s.shards {
		if _, e := sh.db.ExecContext(ctx, `PRAGMA shrink_memory`); e != nil && err == nil {
			err = e
		}
	}
	return err
}

// Sync waits until every write answered so far is committed.
func (s *Store) Sync(ctx context.Context) error {
	return s.write(ctx, nil)
}

// write runs fn in one transaction on the writer goroutine.
func (s *Store) write(ctx context.Context, fn func(*writeTx) error) error {
	return s.send(ctx, writeReq{ctx: ctx, fn: fn, done: make(chan error, 1)})
}

// writeWait is write answered once fn's own transaction commits, with the
// commit's error, also with DeferCommit; the transaction commits as soon
// as fn ran.
func (s *Store) writeWait(ctx context.Context, fn func(*writeTx) error) error {
	return s.send(ctx, writeReq{ctx: ctx, fn: fn, done: make(chan error, 1), wait: true})
}

func (s *Store) send(ctx context.Context, r writeReq) error {
	fn := r.fn
	if s.readOnly {
		if fn == nil {
			return nil // Sync: nothing of ours to wait for
		}
		return ErrReadOnly
	}
	select {
	case s.reqs <- r:
	case <-s.quit:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	return <-r.done
}

// writeTx is the writer's transaction plus a per-transaction statement cache.
type writeTx struct {
	s     *Store
	tx    *sql.Tx
	ctx   context.Context
	stmts map[string]*sql.Stmt
	fts   ftsPending
}

func (w *writeTx) stmt(q string) (*sql.Stmt, error) {
	if st, ok := w.stmts[q]; ok {
		return st, nil
	}
	st, err := w.tx.PrepareContext(context.Background(), q)
	if err != nil {
		return nil, fmt.Errorf("prepare %q: %w", q, err)
	}
	if w.stmts == nil {
		w.stmts = map[string]*sql.Stmt{}
	}
	w.stmts[q] = st
	return st, nil
}

func (w *writeTx) exec(q string, args ...any) (sql.Result, error) {
	st, err := w.stmt(q)
	if err != nil {
		return nil, err
	}
	return st.ExecContext(w.ctx, args...)
}

func (w *writeTx) queryRow(q string, args ...any) (*sql.Row, error) {
	st, err := w.stmt(q)
	if err != nil {
		return nil, err
	}
	return st.QueryRowContext(w.ctx, args...), nil
}

func (w *writeTx) closeStmts() {
	for _, st := range w.stmts {
		st.Close()
	}
}
