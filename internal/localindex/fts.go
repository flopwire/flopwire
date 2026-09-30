package localindex

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"sync"
	"time"

	"modernc.org/sqlite"
)

// The two FTS5 tables live in database files of their own, <index>-tok
// and <index>-tri, each written by its own goroutine and connection.
// SQLite allows one writer per database file, and with both tables in the
// main file the single writer spent most of a bulk load tokenizing and
// merging FTS segments (about 350 of 400 writer seconds on the reference
// corpus). Split, the three writers run in parallel.
//
// Consistency. The main transaction that changes a row's text appends
// (row id, op) to fts_queue, an AUTOINCREMENT log in the main database.
// After it commits, the writer hands the rows' FTS text to each shard; a
// shard applies it in one transaction together with the highest queue
// sequence it covers (fts_meta.applied). On Open, each shard replays the
// queue past its applied sequence from the stored message text, so a crash
// between the main commit and a shard commit loses nothing. Entries every
// shard has applied are pruned.
//
// Readers attach both files (ATTACH ... AS tok / tri), so queries name the
// tables unqualified, as before.

const (
	opDel = 1 // drop the row's FTS entry
	opIns = 2 // index the row's current text
)

// maxShardQueue bounds the FTS text waiting for one shard; the main writer
// blocks beyond it, which holds back the parse workers.
const maxShardQueue = 2 << 20

// ftsOp is one fts_queue entry as a shard applies it. Each carries its own
// sequence, so a shard skips exactly the entries it has applied, however
// the queue was cut into work items.
type ftsOp struct {
	seq  int64
	id   int64
	del  bool
	text string // opIns: the row's text
}

// ftsWork is one main transaction's FTS changes for one shard.
type ftsWork struct {
	seq   int64 // highest fts_queue sequence covered (the shard may own none of it)
	ops   []ftsOp
	bytes int
}

func (w *ftsWork) add(op ftsOp) {
	w.ops = append(w.ops, op)
	w.bytes += len(op.text)
}

type ftsShard struct {
	table    string // fts_tok | fts_tri
	schema   string // tok | tri0, tri1, ...: the ATTACH name
	path     string
	detail   Detail
	tokenize string
	db       *sql.DB
	// Trigram rows are split across parts by row id (id % parts == part):
	// trigram indexing is the most expensive write, and each part has its
	// own writer.
	tri         bool
	part, parts int

	mu      sync.Mutex
	cond    *sync.Cond
	queue   []*ftsWork
	queued  int
	applied int64
	err     error // the last failed apply, cleared when a retry succeeds
	closing bool
	// draining: the store is closing; submit no longer blocks on
	// backpressure (a failing shard would hold the writer forever).
	draining bool
	stop     chan struct{} // closed by close: ends a retry backoff
	done     chan struct{}
}

const shardPageSize = 32 << 10

// ftsHashSize is FTS5's pending-term buffer per shard (default 1MB). A2
// used 8MB; each shard then held tens of MB of hash entries in SQLite's
// allocator, which rounds and retains. At 1MB with 2MB commits a bulk load
// takes the same wall time (automerge 16 absorbs the extra segments).
const ftsHashSize = 1 << 20

func shardPath(index, schema string) string { return index + "-" + schema }

// removeShards deletes the FTS shard files of the index at path (and
// their WAL and shared-memory files).
func removeShards(path string) error {
	ents, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return err
	}
	tok, tri := filepath.Base(shardPath(path, "tok")), filepath.Base(shardPath(path, "tri"))
	for _, e := range ents {
		if n := e.Name(); strings.HasPrefix(n, tok) || strings.HasPrefix(n, tri) {
			if err := os.Remove(filepath.Join(filepath.Dir(path), n)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("localindex: remove FTS shard: %w", err)
			}
		}
	}
	return nil
}

// owns reports whether row id belongs to this shard.
func (sh *ftsShard) owns(id int64) bool {
	return !sh.tri || id%int64(sh.parts) == int64(sh.part)
}

func openShard(index string, indexID int64, schema, table string, detail Detail, tokenize string, cacheMB int) (*ftsShard, error) {
	sh := &ftsShard{table: table, schema: schema, path: shardPath(index, schema), detail: detail, tokenize: tokenize,
		stop: make(chan struct{}), done: make(chan struct{})}
	sh.cond = sync.NewCond(&sh.mu)
	// 32KB pages: FTS5 reads and writes segments sequentially (flushes,
	// merges), and every page is one pread or pwrite; at 4KB those
	// syscalls were a third of a bulk load's CPU.
	db, err := sql.Open("sqlite", dsn(sh.path, false, cacheMB, true))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	sh.db = db
	// cache_size again after page_size: the DSN's value was converted to a
	// page count at 4KB pages, which is eight times the memory at 32KB.
	for _, q := range []string{fmt.Sprintf(`PRAGMA page_size = %d`, shardPageSize), `PRAGMA journal_mode = WAL`,
		fmt.Sprintf(`PRAGMA cache_size = -%d`, cacheMB*1000)} {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			return nil, err
		}
	}
	var owner int64
	if err := db.QueryRow(`SELECT value FROM fts_meta WHERE key = 'index_id'`).Scan(&owner); err == nil && owner != indexID {
		// Left from another index at this path: start over.
		for _, q := range []string{`DROP TABLE IF EXISTS ` + table, `DELETE FROM fts_meta`} {
			if _, err := db.Exec(q); err != nil {
				db.Close()
				return nil, err
			}
		}
	}
	stmts := append(sh.createStmts(),
		`CREATE TABLE IF NOT EXISTS fts_meta (key TEXT PRIMARY KEY, value INTEGER NOT NULL) WITHOUT ROWID`,
		`INSERT OR IGNORE INTO fts_meta VALUES ('applied', 0)`,
		fmt.Sprintf(`INSERT OR REPLACE INTO fts_meta VALUES ('index_id', %d)`, indexID))
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			return nil, fmt.Errorf("localindex: %s: %w", sh.path, err)
		}
	}
	if err := db.QueryRow(`SELECT value FROM fts_meta WHERE key = 'applied'`).Scan(&sh.applied); err != nil {
		db.Close()
		return nil, err
	}
	go pprof.Do(context.Background(), pprof.Labels("fts", schema), func(context.Context) { sh.run() })
	return sh, nil
}

// createStmts create the FTS table if needed and set its tuning.
func (sh *ftsShard) createStmts() []string {
	t := sh.table
	return []string{
		fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS %s USING fts5(text, content='', contentless_delete=1, detail=%s, tokenize=%s)`, t, sh.detail, sh.tokenize),
		// Merges of 16 segments at a time with a crisis merge at 32
		// (defaults 4 and 16): on a synthetic trigram load with 32KB pages,
		// 33.3s against 41.4s with automerge 8, at the same memory.
		fmt.Sprintf(`INSERT INTO %s (%s, rank) VALUES ('hashsize', %d)`, t, t, ftsHashSize),
		fmt.Sprintf(`INSERT INTO %s (%s, rank) VALUES ('automerge', 16)`, t, t),
		fmt.Sprintf(`INSERT INTO %s (%s, rank) VALUES ('crisismerge', 32)`, t, t),
	}
}

// submit queues w, blocking while the shard is too far behind (also while
// it is failing: its retries hold the queue, and memory stays bounded).
func (sh *ftsShard) submit(w *ftsWork) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	for sh.queued > maxShardQueue && !sh.closing && !sh.draining {
		sh.cond.Wait()
	}
	sh.queue = append(sh.queue, w)
	sh.queued += w.bytes
	sh.cond.Broadcast()
}

// wait blocks until every change up to seq is committed in the shard. It
// returns the shard's error while the shard is failing; the shard keeps
// retrying, so a later wait can succeed.
func (sh *ftsShard) wait(seq int64) error {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	for sh.applied < seq && sh.err == nil && !sh.closing {
		sh.cond.Wait()
	}
	if sh.applied >= seq {
		return nil
	}
	if sh.err != nil {
		return sh.err
	}
	return ErrClosed
}

func (sh *ftsShard) appliedSeq() int64 {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.applied
}

// Retry backoff for a failed shard transaction.
var (
	shardRetryMin = 50 * time.Millisecond
	shardRetryMax = 5 * time.Second
)

// shardFault, when set (tests), fails an apply before it starts.
var shardFault func(sh *ftsShard) error

func (sh *ftsShard) run() {
	defer close(sh.done)
	backoff := time.Duration(0)
	for {
		sh.mu.Lock()
		for len(sh.queue) == 0 && !sh.closing {
			sh.cond.Wait()
		}
		if len(sh.queue) == 0 || (sh.closing && sh.err != nil) {
			// Closing: what is left (or failing) is replayed from
			// fts_queue at the next Open.
			sh.mu.Unlock()
			return
		}
		// Everything queued goes into one transaction, up to about one
		// commit's worth, so segments stay large.
		n, bytes := 0, 0
		for n < len(sh.queue) && (n == 0 || bytes+sh.queue[n].bytes <= commitFTSSize) {
			bytes += sh.queue[n].bytes
			n++
		}
		batch := sh.queue[:n:n]
		sh.mu.Unlock()

		err := sh.apply(batch)

		sh.mu.Lock()
		if err != nil {
			// Keep the batch at the head of the queue and retry it:
			// applied never moves past a batch that did not commit, so
			// fts_queue keeps its entries until they are applied.
			sh.err = fmt.Errorf("localindex: %s: %w", sh.path, err)
			sh.cond.Broadcast()
			sh.mu.Unlock()
			backoff = min(max(2*backoff, shardRetryMin), shardRetryMax)
			select {
			case <-time.After(backoff):
			case <-sh.stop:
			}
			continue
		}
		backoff = 0
		sh.err = nil
		sh.queue = sh.queue[n:]
		sh.queued -= bytes
		sh.applied = max(sh.applied, batch[len(batch)-1].seq)
		sh.cond.Broadcast()
		sh.mu.Unlock()
	}
}

func (sh *ftsShard) apply(batch []*ftsWork) error {
	if shardFault != nil {
		if err := shardFault(sh); err != nil {
			return err
		}
	}
	ctx := context.Background()
	tx, err := sh.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // after Commit it is a no-op
	del, err := tx.PrepareContext(ctx, `DELETE FROM `+sh.table+` WHERE rowid = ?`)
	if err != nil {
		return err
	}
	defer del.Close()
	ins, err := tx.PrepareContext(ctx, `INSERT INTO `+sh.table+` (rowid, text) VALUES (?, ?)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	var applied int64
	if err := tx.QueryRowContext(ctx, `SELECT value FROM fts_meta WHERE key = 'applied'`).Scan(&applied); err != nil {
		return err
	}
	last := applied
	for _, w := range batch {
		for _, op := range w.ops {
			if op.seq <= applied {
				continue // applied before (replayed at Open, or a retried batch)
			}
			if op.del {
				_, err = del.ExecContext(ctx, op.id)
			} else {
				text := op.text
				if !sh.tri {
					text = tokText(text) // here, on the shard's goroutine, not held while queued
				}
				_, err = ins.ExecContext(ctx, op.id, text)
			}
			if err != nil {
				return err
			}
		}
		last = max(last, w.seq)
	}
	if last > applied {
		if _, err := tx.ExecContext(ctx, `UPDATE fts_meta SET value = ? WHERE key = 'applied'`, last); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// drain stops submit from blocking, so a writer held by backpressure can
// finish while the store closes.
func (sh *ftsShard) drain() {
	sh.mu.Lock()
	sh.draining = true
	sh.cond.Broadcast()
	sh.mu.Unlock()
}

func (sh *ftsShard) close() error {
	sh.mu.Lock()
	if !sh.closing {
		sh.closing = true
		close(sh.stop)
	}
	sh.cond.Broadcast()
	sh.mu.Unlock()
	<-sh.done
	return sh.db.Close()
}

// checkShards rebuilds, from the stored message text, every shard whose
// applied sequence the fts_queue cannot bring up to date: a shard file
// that was lost or recreated (applied below entries already pruned), one
// ahead of the main database (the main file lost commits, so sequences and
// row ids will be reused), or one whose rebuild did not finish. It runs in
// Open before the writer starts.
func (s *Store) checkShards(ctx context.Context) error {
	var top, low sql.NullInt64
	if err := s.wdb.QueryRowContext(ctx, `SELECT (SELECT seq FROM sqlite_sequence WHERE name = 'fts_queue'),
		(SELECT min(seq) FROM fts_queue)`).Scan(&top, &low); err != nil {
		return err
	}
	first := top.Int64 + 1 // the oldest sequence the queue still holds
	if low.Valid {
		first = low.Int64
	}
	for _, sh := range s.shards {
		var rebuilding int64
		if err := sh.db.QueryRowContext(ctx, `SELECT count(*) FROM fts_meta WHERE key = 'rebuilding'`).Scan(&rebuilding); err != nil {
			return err
		}
		applied := sh.appliedSeq()
		if rebuilding == 0 && applied <= top.Int64 && applied >= first-1 {
			continue
		}
		slog.Warn("localindex: rebuilding FTS shard", "shard", sh.path, "applied", applied, "queue", fmt.Sprintf("%d..%d", first, top.Int64))
		if err := s.rebuildShard(ctx, sh, top.Int64); err != nil {
			return err
		}
	}
	return nil
}

// rebuildShard empties the shard and indexes every row it owns from
// messages, then records upto as applied. A crash midway leaves the
// 'rebuilding' marker, and the next Open starts over.
func (s *Store) rebuildShard(ctx context.Context, sh *ftsShard, upto int64) error {
	reset := append([]string{`INSERT OR REPLACE INTO fts_meta VALUES ('rebuilding', 1)`, `UPDATE fts_meta SET value = 0 WHERE key = 'applied'`,
		`DROP TABLE IF EXISTS ` + sh.table},
		sh.createStmts()...)
	for _, q := range reset {
		if _, err := sh.db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	q := `SELECT id, text FROM messages ORDER BY id`
	var args []any
	if sh.tri {
		q = `SELECT id, text FROM messages WHERE id % ? = ? ORDER BY id`
		args = []any{sh.parts, sh.part}
	}
	rows, err := s.wdb.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	w := &ftsWork{}
	flush := func() error {
		if len(w.ops) > 0 {
			if err := sh.apply([]*ftsWork{w}); err != nil {
				return err
			}
		}
		w = &ftsWork{}
		return nil
	}
	var n int64
	for rows.Next() {
		var id int64
		var z []byte
		if err := rows.Scan(&id, &z); err != nil {
			return err
		}
		text, err := decompress(z)
		if err != nil {
			return fmt.Errorf("row %d: %w", id, err)
		}
		if text == "" {
			continue
		}
		// Sequences above 0 so apply takes them: applied was reset.
		w.add(ftsOp{seq: 1, id: id, text: text})
		if n++; w.bytes >= commitFTSSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	for _, q := range []string{fmt.Sprintf(`UPDATE fts_meta SET value = %d WHERE key = 'applied'`, upto), `DELETE FROM fts_meta WHERE key = 'rebuilding'`} {
		if _, err := sh.db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	sh.mu.Lock()
	sh.applied = upto
	sh.mu.Unlock()
	slog.Info("localindex: FTS shard rebuilt", "shard", sh.path, "rows", n)
	return nil
}

// recoverShards replays fts_queue entries a shard has not applied, from
// the stored message text, and waits until they are committed. It runs in
// Open before the writer starts.
func (s *Store) recoverShards(ctx context.Context) error {
	for _, sh := range s.shards {
		rows, err := s.wdb.QueryContext(ctx, `SELECT q.seq, q.msg_id, q.op, m.text FROM fts_queue q
			LEFT JOIN messages m ON m.id = q.msg_id WHERE q.seq > ? ORDER BY q.seq`, sh.appliedSeq())
		if err != nil {
			return err
		}
		w := &ftsWork{}
		var last int64
		for rows.Next() {
			var seq, id int64
			var op int
			var z []byte
			if err := rows.Scan(&seq, &id, &op, &z); err != nil {
				rows.Close()
				return err
			}
			last = seq
			if !sh.owns(id) {
				continue
			}
			if op == opDel || z == nil {
				w.add(ftsOp{seq: seq, id: id, del: true})
			} else {
				text, err := decompress(z)
				if err != nil {
					rows.Close()
					return err
				}
				// A changed row has an opDel entry before this one.
				if text != "" {
					w.add(ftsOp{seq: seq, id: id, text: text})
				}
			}
			w.seq = seq
			if w.bytes >= commitFTSSize {
				// Wait for each chunk: a failing shard fails Open here
				// instead of blocking the next submit on backpressure.
				sh.submit(w)
				if err := sh.wait(w.seq); err != nil {
					rows.Close()
					return err
				}
				w = &ftsWork{}
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if last == 0 {
			continue
		}
		w.seq = last
		sh.submit(w)
		if err := sh.wait(last); err != nil {
			return err
		}
	}
	return nil
}

// attachConnector opens read connections with both FTS shards attached.
type attachConnector struct {
	dsn    string
	shards []*ftsShard
}

func (c *attachConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := (&sqlite.Driver{}).Open(c.dsn)
	if err != nil {
		return nil, err
	}
	ex, ok := conn.(driver.ExecerContext)
	if !ok {
		conn.Close()
		return nil, errors.New("localindex: sqlite connection cannot exec")
	}
	for _, sh := range c.shards {
		if _, err := ex.ExecContext(ctx, `ATTACH DATABASE ? AS `+sh.schema, []driver.NamedValue{{Ordinal: 1, Value: "file:" + sh.path}}); err != nil {
			conn.Close()
			return nil, fmt.Errorf("localindex: attach %s: %w", sh.path, err)
		}
	}
	return conn, nil
}

func (c *attachConnector) Driver() driver.Driver { return &sqlite.Driver{} }
