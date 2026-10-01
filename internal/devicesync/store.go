package devicesync

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

// Store persists per-source sync state: the current generation and its
// change-detection watermark, every generation's local manifest, and what
// the server has acknowledged. The acknowledged watermark is the upload
// queue: whatever lies past it is pending.
//
// Tables are prefixed devsync_ so the store can share a database with the
// local index (A2): pass that *sql.DB to NewStore. OpenStore opens a
// dedicated SQLite file instead.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS devsync_sources (
  id           INTEGER PRIMARY KEY,
  path         TEXT NOT NULL UNIQUE,
  spec         TEXT NOT NULL,         -- SourceSpec JSON
  generation   INTEGER NOT NULL,      -- current generation, -1 before the first
  watermark    TEXT                   -- transcript.Watermark JSON of the current generation
);
CREATE TABLE IF NOT EXISTS devsync_gens (
  source_id     INTEGER NOT NULL,
  generation    INTEGER NOT NULL,
  file_id       TEXT NOT NULL,
  previous      TEXT,                 -- syncproto.SourceRef JSON
  parent        TEXT,                 -- syncproto.SourceRef JSON: the companion's parent at capture
  size          INTEGER NOT NULL,     -- bytes captured: tail end
  change_time   INTEGER NOT NULL,
  captured_at   INTEGER NOT NULL,
  entries       INTEGER NOT NULL,     -- local manifest length
  tail_offset   INTEGER NOT NULL,
  tail_size     INTEGER NOT NULL,     -- 0: no tail
  tail_hash     BLOB,
  acked         INTEGER NOT NULL,     -- manifest entries the server committed
  tail_acked    INTEGER NOT NULL,     -- the current tail (or its absence) is stored
  closed        INTEGER NOT NULL,     -- a newer generation exists; only upload remains
  lost          INTEGER NOT NULL,     -- unacked bytes are gone; upload stopped
  srv_tail_off  INTEGER NOT NULL,     -- provisional tail the server holds (last answer),
  srv_tail_size INTEGER NOT NULL,     -- the base for tail deltas
  redactions    TEXT,                 -- JSON {rule: count} of secrets masked in this generation
  PRIMARY KEY (source_id, generation)
);
CREATE TABLE IF NOT EXISTS devsync_manifest (
  source_id  INTEGER NOT NULL,
  generation INTEGER NOT NULL,
  ordinal    INTEGER NOT NULL,
  hash       BLOB NOT NULL,
  offset     INTEGER NOT NULL,
  size       INTEGER NOT NULL,
  PRIMARY KEY (source_id, generation, ordinal)
);
CREATE INDEX IF NOT EXISTS devsync_manifest_hash ON devsync_manifest (hash);
CREATE TABLE IF NOT EXISTS devsync_known (hash BLOB PRIMARY KEY) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS devsync_sources_session ON devsync_sources (json_extract(spec, '$.SessionKey'));
`

// OpenStore opens (creating) a dedicated SQLite state file.
func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s, err := NewStore(db)
	if err != nil {
		db.Close()
	}
	return s, err
}

// NewStore creates the devsync_ tables in db if needed.
func NewStore(db *sql.DB) (*Store, error) {
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("devicesync: schema: %w", err)
	}
	// Tables from an older build lack columns; the state lives in the
	// index database, which is rebuilt rather than migrated.
	if _, err := db.Exec(`SELECT redactions FROM devsync_gens LIMIT 0`); err != nil {
		return nil, fmt.Errorf("devicesync: sync state is from an older version: rebuild the index (%w)", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

type sourceRow struct {
	ID        int64
	Spec      SourceSpec
	Gen       int64
	Watermark *transcript.Watermark
}

type genRow struct {
	SourceID, Gen          int64
	FileID                 string
	Previous               *syncproto.SourceRef
	Parent                 *syncproto.SourceRef // companion's parent identity at capture
	Size                   int64
	ChangeTime, CapturedAt int64
	Entries                int64
	Tail                   syncproto.Tail // Size 0: none
	Acked                  int64
	TailAcked              bool
	Closed, Lost           bool
	SrvTailOff, SrvTailLen int64
	// Redactions counts the secrets masked in the generation's captured
	// bytes, per rule (redact.RulesVersion).
	Redactions map[string]int64
}

// done reports whether nothing of the generation remains to upload.
func (g *genRow) done() bool { return g.Lost || (g.Acked == g.Entries && g.TailAcked) }

func (g *genRow) boundary() int64 { return g.Tail.Offset }

// source loads a source by path, creating it when spec is non-nil.
func (s *Store) source(ctx context.Context, path string, spec *SourceSpec) (*sourceRow, error) {
	row := &sourceRow{}
	var specJSON string
	var wm sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id, spec, generation, watermark FROM devsync_sources WHERE path = ?`, path).
		Scan(&row.ID, &specJSON, &row.Gen, &wm)
	if errors.Is(err, sql.ErrNoRows) && spec != nil {
		raw, _ := json.Marshal(spec)
		res, err := s.db.ExecContext(ctx, `INSERT INTO devsync_sources (path, spec, generation) VALUES (?, ?, -1)`, path, string(raw))
		if err != nil {
			return nil, err
		}
		row.ID, _ = res.LastInsertId()
		row.Spec, row.Gen = *spec, -1
		return row, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(specJSON), &row.Spec); err != nil {
		return nil, err
	}
	if spec != nil && *spec != row.Spec {
		raw, _ := json.Marshal(spec)
		if _, err := s.db.ExecContext(ctx, `UPDATE devsync_sources SET spec = ? WHERE id = ?`, string(raw), row.ID); err != nil {
			return nil, err
		}
		row.Spec = *spec
	}
	if wm.Valid {
		row.Watermark = &transcript.Watermark{}
		if err := json.Unmarshal([]byte(wm.String), row.Watermark); err != nil {
			return nil, err
		}
	}
	return row, nil
}

const genCols = `source_id, generation, file_id, previous, parent, size, change_time, captured_at, entries,
  tail_offset, tail_size, tail_hash, acked, tail_acked, closed, lost, srv_tail_off, srv_tail_size, redactions`

func scanGen(sc interface{ Scan(...any) error }) (*genRow, error) {
	g := &genRow{}
	var prev, parent, red sql.NullString
	var th []byte
	err := sc.Scan(&g.SourceID, &g.Gen, &g.FileID, &prev, &parent, &g.Size, &g.ChangeTime, &g.CapturedAt, &g.Entries,
		&g.Tail.Offset, &g.Tail.Size, &th, &g.Acked, &g.TailAcked, &g.Closed, &g.Lost, &g.SrvTailOff, &g.SrvTailLen, &red)
	if err != nil {
		return nil, err
	}
	if red.Valid {
		if err := json.Unmarshal([]byte(red.String), &g.Redactions); err != nil {
			return nil, err
		}
	}
	copy(g.Tail.Hash[:], th)
	if prev.Valid {
		g.Previous = &syncproto.SourceRef{}
		if err := json.Unmarshal([]byte(prev.String), g.Previous); err != nil {
			return nil, err
		}
	}
	if parent.Valid {
		g.Parent = &syncproto.SourceRef{}
		if err := json.Unmarshal([]byte(parent.String), g.Parent); err != nil {
			return nil, err
		}
	}
	return g, nil
}

func (s *Store) gen(ctx context.Context, sid, gen int64) (*genRow, error) {
	g, err := scanGen(s.db.QueryRowContext(ctx, `SELECT `+genCols+` FROM devsync_gens WHERE source_id = ? AND generation = ?`, sid, gen))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return g, err
}

// pendingGens returns the source's generations with unacknowledged data,
// oldest first.
func (s *Store) pendingGens(ctx context.Context, sid int64) ([]*genRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+genCols+` FROM devsync_gens
	  WHERE source_id = ? AND lost = 0 AND (acked < entries OR tail_acked = 0) ORDER BY generation`, sid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*genRow
	for rows.Next() {
		g, err := scanGen(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// PendingSpecs lists sources with unacknowledged data: the upload queue.
func (s *Store) PendingSpecs(ctx context.Context) ([]SourceSpec, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT spec FROM devsync_sources s WHERE EXISTS (
	  SELECT 1 FROM devsync_gens g WHERE g.source_id = s.id AND g.lost = 0 AND (g.acked < g.entries OR g.tail_acked = 0))
	  ORDER BY s.path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SourceSpec
	for rows.Next() {
		var raw string
		var sp SourceSpec
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &sp); err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

func (s *Store) entries(ctx context.Context, sid, gen, from int64, limit int) ([]syncproto.Entry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ordinal, hash, offset, size FROM devsync_manifest
	  WHERE source_id = ? AND generation = ? AND ordinal >= ? ORDER BY ordinal LIMIT ?`, sid, gen, from, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []syncproto.Entry
	for rows.Next() {
		var e syncproto.Entry
		var h []byte
		if err := rows.Scan(&e.Ordinal, &h, &e.Offset, &e.Size); err != nil {
			return nil, err
		}
		copy(e.Hash[:], h)
		out = append(out, e)
	}
	return out, rows.Err()
}

// saveCapture commits one chunking pass atomically: new manifest entries,
// the generation row, the source's current generation and watermark, and
// (when a new generation starts) the previous generation's closing.
func (s *Store) saveCapture(ctx context.Context, src *sourceRow, g *genRow, add []syncproto.Entry, wm *transcript.Watermark) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for part := range slices.Chunk(add, batchRows) {
		args := make([]any, 0, 6*len(part))
		for _, e := range part {
			args = append(args, g.SourceID, g.Gen, e.Ordinal, e.Hash[:], e.Offset, e.Size)
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO devsync_manifest VALUES `+values(len(part), 6), args...); err != nil {
			return err
		}
	}
	var red any
	if len(g.Redactions) > 0 {
		raw, _ := json.Marshal(g.Redactions)
		red = string(raw)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO devsync_gens (`+genCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		g.SourceID, g.Gen, g.FileID, refJSON(g.Previous), refJSON(g.Parent), g.Size, g.ChangeTime, g.CapturedAt, g.Entries,
		g.Tail.Offset, g.Tail.Size, g.Tail.Hash[:], g.Acked, g.TailAcked, g.Closed, g.Lost, g.SrvTailOff, g.SrvTailLen, red); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE devsync_gens SET closed = 1 WHERE source_id = ? AND generation < ?`, g.SourceID, g.Gen); err != nil {
		return err
	}
	var wmJSON any
	if wm != nil {
		raw, _ := json.Marshal(wm)
		wmJSON = string(raw)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE devsync_sources SET generation = ?, watermark = ? WHERE id = ?`, g.Gen, wmJSON, g.SourceID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	src.Gen, src.Watermark = g.Gen, wm
	return nil
}

// updateGen records upload progress (acknowledgement, gap) of a
// generation, and remembers acknowledged chunk hashes as known to the server.
func (s *Store) updateGen(ctx context.Context, g *genRow, known []syncproto.Hash) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE devsync_gens SET entries = ?, tail_offset = ?, tail_size = ?, tail_hash = ?,
	  acked = ?, tail_acked = ?, lost = ?, srv_tail_off = ?, srv_tail_size = ? WHERE source_id = ? AND generation = ?`,
		g.Entries, g.Tail.Offset, g.Tail.Size, g.Tail.Hash[:], g.Acked, g.TailAcked, g.Lost, g.SrvTailOff, g.SrvTailLen, g.SourceID, g.Gen); err != nil {
		return err
	}
	if err := insertKnown(ctx, tx, known); err != nil {
		return err
	}
	return tx.Commit()
}

// batchRows bounds the rows one statement names (well under SQLite's
// 32766 bound parameters).
const batchRows = 500

// values returns n parenthesized groups of k placeholders: "(?, ?), (?, ?)".
func values(n, k int) string {
	group := "(" + strings.Repeat("?, ", k-1) + "?)"
	return strings.Repeat(group+", ", n-1) + group
}

func hashArgs(hs []syncproto.Hash) []any {
	args := make([]any, len(hs))
	for i, h := range hs {
		args[i] = h[:]
	}
	return args
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// insertKnown adds hashes to the known set, batchRows per statement.
func insertKnown(ctx context.Context, db execer, hs []syncproto.Hash) error {
	for part := range slices.Chunk(hs, batchRows) {
		if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO devsync_known VALUES `+values(len(part), 1), hashArgs(part)...); err != nil {
			return err
		}
	}
	return nil
}

// setWatermark replaces the current generation's watermark; nil forces the
// next capture to start a new generation after floor.
func (s *Store) setWatermark(ctx context.Context, src *sourceRow, wm *transcript.Watermark) error {
	var v any
	if wm != nil {
		raw, _ := json.Marshal(wm)
		v = string(raw)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE devsync_sources SET generation = ?, watermark = ? WHERE id = ?`, src.Gen, v, src.ID); err != nil {
		return err
	}
	src.Watermark = wm
	return nil
}

// remember marks hashes as held by the server.
func (s *Store) remember(ctx context.Context, hs []syncproto.Hash) error {
	return insertKnown(ctx, s.db, hs)
}

// forget drops hashes from the known set (the server said they are missing).
func (s *Store) forget(ctx context.Context, hs []syncproto.Hash) error {
	for part := range slices.Chunk(hs, batchRows) {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM devsync_known WHERE hash IN (`+placeholders(len(part))+`)`, hashArgs(part)...); err != nil {
			return err
		}
	}
	return nil
}

func placeholders(n int) string { return strings.Repeat("?, ", n-1) + "?" }

// hashSet runs query once per batchRows hashes, its IN list filled with
// them, and returns the hashes it selects.
func (s *Store) hashSet(ctx context.Context, query func(n int) string, hs []syncproto.Hash) (map[syncproto.Hash]bool, error) {
	out := map[syncproto.Hash]bool{}
	for part := range slices.Chunk(hs, batchRows) {
		rows, err := s.db.QueryContext(ctx, query(len(part)), hashArgs(part)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var b []byte
			if err := rows.Scan(&b); err != nil {
				rows.Close()
				return nil, err
			}
			var h syncproto.Hash
			copy(h[:], b)
			out[h] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// known returns which of hs the server is known to hold.
func (s *Store) known(ctx context.Context, hs []syncproto.Hash) (map[syncproto.Hash]bool, error) {
	return s.hashSet(ctx, func(n int) string {
		return `SELECT hash FROM devsync_known WHERE hash IN (` + placeholders(n) + `)`
	}, hs)
}

// referenced returns which of hs an unacknowledged manifest entry needs.
func (s *Store) referenced(ctx context.Context, hs []syncproto.Hash) (map[syncproto.Hash]bool, error) {
	return s.hashSet(ctx, referencedSQL, hs)
}

func referencedSQL(n int) string {
	return `SELECT DISTINCT m.hash FROM devsync_manifest m JOIN devsync_gens g
	  ON g.source_id = m.source_id AND g.generation = m.generation
	  WHERE m.hash IN (` + placeholders(n) + `) AND m.ordinal >= g.acked AND m.ordinal < g.entries AND g.lost = 0`
}

// pendingTail reports whether generation gen of source sid still has an
// unacknowledged tail, so its spooled copy is needed.
func (s *Store) pendingTail(ctx context.Context, sid, gen int64) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM devsync_gens WHERE source_id = ? AND generation = ?
	  AND lost = 0 AND tail_acked = 0 AND tail_size > 0`, sid, gen).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// renamedFrom finds the source a new source at spec.Path was moved from
// (Codex archives a rollout by renaming it into archived_sessions/): same
// agent, storage kind, session key and file name at another path, whose
// current generation has file identity fid, or whose file is gone when
// the move changed the identity (a copy across file systems). It returns
// nil when there is none or more than one.
func (s *Store) renamedFrom(ctx context.Context, spec *SourceSpec, fid string, gone func(path string) bool) (*syncproto.SourceRef, error) {
	if spec.SessionKey == "" || spec.Export || spec.StorageKind == transcript.StorageCompanion {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT s.path, s.spec, g.file_id FROM devsync_sources s
	  JOIN devsync_gens g ON g.source_id = s.id AND g.generation = s.generation
	  WHERE json_extract(s.spec, '$.SessionKey') = ? AND s.path <> ?`, spec.SessionKey, spec.Path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var same, moved []syncproto.SourceRef
	for rows.Next() {
		var path, raw, gfid string
		if err := rows.Scan(&path, &raw, &gfid); err != nil {
			return nil, err
		}
		var sp SourceSpec
		if json.Unmarshal([]byte(raw), &sp) != nil || sp.Agent != spec.Agent || sp.StorageKind != spec.StorageKind ||
			sp.Export || filepath.Base(path) != filepath.Base(spec.Path) {
			continue
		}
		ref := syncproto.SourceRef{Path: path, FileID: gfid}
		if fid != "" && gfid == fid {
			same = append(same, ref)
		} else {
			moved = append(moved, ref)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(same) == 1 {
		return &same[0], nil
	}
	if len(same) == 0 && len(moved) == 1 && gone(moved[0].Path) {
		return &moved[0], nil
	}
	return nil, nil
}

func refJSON(r *syncproto.SourceRef) any {
	if r == nil {
		return nil
	}
	raw, _ := json.Marshal(r)
	return string(raw)
}

// RedactionTotals sums the secrets masked in every source's current
// generation, per rule, and counts the sources with any.
func (s *Store) RedactionTotals(ctx context.Context) (map[string]int64, int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT j.key, SUM(j.value) FROM devsync_sources s
	  JOIN devsync_gens g ON g.source_id = s.id AND g.generation = s.generation, json_each(g.redactions) j
	  WHERE g.redactions IS NOT NULL GROUP BY j.key`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			return nil, 0, err
		}
		out[k] = n
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var srcs int64
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM devsync_sources s JOIN devsync_gens g ON g.source_id = s.id AND g.generation = s.generation
	  WHERE g.redactions IS NOT NULL`).Scan(&srcs)
	return out, srcs, err
}
