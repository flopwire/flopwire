package localindex

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

// SourceState is a source row with its watermark.
type SourceState struct {
	ID          int64
	Source      transcript.Source
	Generation  int64                 // current generation; 0 before the first
	Watermark   *transcript.Watermark // nil until the first save
	CursorState []byte
	Extraction  *transcript.ExtractionCheckpoint
}

// EnsureSource returns the source row for (device, path, file id), creating
// it on first sight. A new file identity at the same path is a new source.
func (s *Store) EnsureSource(ctx context.Context, src transcript.Source) (SourceState, error) {
	var st SourceState
	err := s.write(ctx, func(w *writeTx) error {
		_, err := w.exec(`INSERT INTO sources (device_id, agent, path, file_id, session_key, storage_kind, parser, first_seen_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (device_id, path, file_id) DO UPDATE SET
			  session_key = coalesce(excluded.session_key, session_key), parser = excluded.parser`,
			s.opts.DeviceID, string(src.Agent), src.Path, src.FileID.String(), nullStr(src.SessionKey),
			string(src.StorageKind), src.Parser, time.Now().UnixMilli())
		if err != nil {
			return err
		}
		st, err = loadSource(w.ctx, w.tx, `WHERE device_id = ? AND path = ? AND file_id = ?`, s.opts.DeviceID, src.Path, src.FileID.String())
		return err
	})
	return st, err
}

// SourcesByPath returns every source row recorded at path (one per file
// identity seen there), newest first. Source reads run on the writer, so
// they see writes not yet committed (Options.DeferCommit); on a read-only
// store they use the read pool.
func (s *Store) SourcesByPath(ctx context.Context, path string) ([]SourceState, error) {
	var out []SourceState
	err := s.readSources(ctx, func(ctx context.Context, q dbtx) error {
		var err error
		out, err = sourcesWith(ctx, q, sourceCols, `WHERE device_id = ? AND path = ? ORDER BY id DESC`, s.opts.DeviceID, path)
		return err
	})
	return out, err
}

// HasSources reports whether the index holds any source: false until the
// agent has indexed a first transcript.
func (s *Store) HasSources(ctx context.Context) (bool, error) {
	var ok bool
	err := s.readSources(ctx, func(ctx context.Context, q dbtx) error {
		return q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sources)`).Scan(&ok)
	})
	return ok, err
}

// Source loads one source row by id.
func (s *Store) Source(ctx context.Context, id int64) (SourceState, error) {
	var st SourceState
	err := s.readSources(ctx, func(ctx context.Context, q dbtx) error {
		var err error
		st, err = loadSource(ctx, q, `WHERE id = ?`, id)
		return err
	})
	return st, err
}

// dbtx is a transaction or a pool.
type dbtx interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

// readSources runs fn on the writer, or on the read pool of a read-only
// store.
func (s *Store) readSources(ctx context.Context, fn func(context.Context, dbtx) error) error {
	if s.readOnly {
		return fn(ctx, s.rdb)
	}
	return s.write(ctx, func(w *writeTx) error { return fn(w.ctx, w.tx) })
}

func sourcesWith(ctx context.Context, q dbtx, cols, where string, args ...any) ([]SourceState, error) {
	rows, err := q.QueryContext(ctx, cols+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SourceState
	for rows.Next() {
		st, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

const sourceCols = `SELECT id, agent, path, file_id, session_key, storage_kind, parser, generation,
	wm_dev, wm_ino, wm_size, wm_ctime, wm_sampled_at, wm_offset, wm_line_no,
	wm_head_len, wm_head_hash, wm_anchor_len, wm_anchor_hash, cursor_state, extraction_report FROM sources `

// sourceColsNoState is sourceCols without the cursor state, which can be
// large (Codex keeps its seen-message set there).
const sourceColsNoState = `SELECT id, agent, path, file_id, session_key, storage_kind, parser, generation,
	wm_dev, wm_ino, wm_size, wm_ctime, wm_sampled_at, wm_offset, wm_line_no,
	wm_head_len, wm_head_hash, wm_anchor_len, wm_anchor_hash, NULL, extraction_report FROM sources `

func loadSource(ctx context.Context, q dbtx, where string, args ...any) (SourceState, error) {
	return scanSource(q.QueryRowContext(ctx, sourceCols+where, args...))
}

func scanSource(sc interface{ Scan(...any) error }) (SourceState, error) {
	var (
		report                         sql.NullString
		st                             SourceState
		agent, fileID, kind            string
		sessionKey                     sql.NullString
		dev, ino, size, ctime, sampled sql.NullInt64
		off, line, headLen, anchorLen  sql.NullInt64
		headHash, anchorHash           []byte
	)
	err := sc.Scan(
		&st.ID, &agent, &st.Source.Path, &fileID, &sessionKey, &kind, &st.Source.Parser, &st.Generation,
		&dev, &ino, &size, &ctime, &sampled, &off, &line, &headLen, &headHash, &anchorLen, &anchorHash, &st.CursorState, &report)
	if err != nil {
		return st, err
	}
	if report.Valid {
		st.Extraction = &transcript.ExtractionCheckpoint{}
		if err := json.Unmarshal([]byte(report.String), st.Extraction); err != nil {
			return st, err
		}
	}
	st.Source.Agent = transcript.Agent(agent)
	st.Source.StorageKind = transcript.StorageKind(kind)
	st.Source.SessionKey = sessionKey.String
	st.Source.FileID = parseFileID(fileID)
	if off.Valid {
		wm := &transcript.Watermark{
			Identity:  transcript.Identity{ID: transcript.FileID{Dev: uint64(dev.Int64), Ino: uint64(ino.Int64)}, Size: size.Int64, CTime: ctime.Int64},
			SampledAt: sampled.Int64, Offset: off.Int64, LineNo: line.Int64, HeadLen: headLen.Int64, AnchorLen: anchorLen.Int64,
		}
		copy(wm.HeadHash[:], headHash)
		copy(wm.AnchorSum[:], anchorHash)
		st.Watermark = wm
	}
	return st, nil
}

// ListSources returns every source row of this device with its watermark,
// for an indexer loading its gate state at startup. With agent set, only
// that agent's sources; with fileID set, only sources of that identity.
// Cursor state is left out.
func (s *Store) ListSources(ctx context.Context, agent transcript.Agent, fileID string) ([]SourceState, error) {
	q, args := `WHERE device_id = ?`, []any{s.opts.DeviceID}
	if agent != "" {
		q, args = q+` AND agent = ?`, append(args, string(agent))
	}
	if fileID != "" {
		q, args = q+` AND file_id = ?`, append(args, fileID)
	}
	var out []SourceState
	err := s.readSources(ctx, func(ctx context.Context, db dbtx) error {
		var err error
		out, err = sourcesWith(ctx, db, sourceColsNoState, q+` ORDER BY id`, args...)
		return err
	})
	return out, err
}

// MoveSource records that a source's file moved to path (a Codex rollout
// archived from sessions/ to archived_sessions/ keeps its inode and
// session id). Rows, watermark and generation stay with the source.
func (s *Store) MoveSource(ctx context.Context, id int64, path string) error {
	return s.write(ctx, func(w *writeTx) error {
		res, err := w.exec(`UPDATE sources SET path = ? WHERE id = ?`, path, id)
		if err != nil {
			return err
		}
		return mustAffect(res, "source", id)
	})
}

// CompanionSizes returns the recorded size of every companion file by path.
func (s *Store) CompanionSizes(ctx context.Context) (map[string]int64, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT path, size FROM companions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var p string
		var n int64
		if err := rows.Scan(&p, &n); err != nil {
			return nil, err
		}
		out[p] = n
	}
	return out, rows.Err()
}

func parseFileID(s string) transcript.FileID {
	var id transcript.FileID
	var dev, ino uint64
	var i int
	for i = 0; i < len(s) && s[i] != ':'; i++ {
		dev = dev*10 + uint64(s[i]-'0')
	}
	for i++; i < len(s); i++ {
		ino = ino*10 + uint64(s[i]-'0')
	}
	id.Dev, id.Ino = dev, ino
	return id
}

// SaveWatermark records the source's watermark and parser cursor state.
// ApplyBatch can do the same atomically with the rows it writes.
func (s *Store) SaveWatermark(ctx context.Context, sourceID int64, wm transcript.Watermark, cursorState []byte) error {
	return s.write(ctx, func(w *writeTx) error { return w.saveWatermark(sourceID, &wm, cursorState) })
}

func (w *writeTx) saveWatermark(sourceID int64, wm *transcript.Watermark, state []byte) error {
	res, err := w.exec(`UPDATE sources SET wm_dev = ?, wm_ino = ?, wm_size = ?, wm_ctime = ?, wm_sampled_at = ?,
		wm_offset = ?, wm_line_no = ?, wm_head_len = ?, wm_head_hash = ?, wm_anchor_len = ?, wm_anchor_hash = ?, cursor_state = ?
		WHERE id = ?`,
		int64(wm.Identity.ID.Dev), int64(wm.Identity.ID.Ino), wm.Identity.Size, wm.Identity.CTime, wm.SampledAt,
		wm.Offset, wm.LineNo, wm.HeadLen, wm.HeadHash[:], wm.AnchorLen, wm.AnchorSum[:], state, sourceID)
	if err != nil {
		return err
	}
	return mustAffect(res, "source", sourceID)
}

// StartGeneration records generation g of the source and makes it current.
// Rows written afterwards carry it as source_generation; SupersedeAbsent
// then retires rows of earlier generations that the new one did not touch.
func (s *Store) StartGeneration(ctx context.Context, sourceID int64, g transcript.Generation, reason string) error {
	return s.write(ctx, func(w *writeTx) error { return w.startGeneration(sourceID, &g, reason) })
}

func (w *writeTx) startGeneration(sourceID int64, g *transcript.Generation, reason string) error {
	captured := g.CapturedAt
	if captured.IsZero() {
		captured = time.Now()
	}
	if _, err := w.exec(`INSERT INTO generations (source_id, generation, size, change_time, captured_at, complete, reason)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (source_id, generation) DO UPDATE SET size = excluded.size, change_time = excluded.change_time,
		  captured_at = excluded.captured_at, complete = excluded.complete`,
		sourceID, g.Generation, g.Size, nullTime(g.ChangeTime), captured.UnixMilli(), boolInt(g.Complete), nullStr(reason)); err != nil {
		return err
	}
	res, err := w.exec(`UPDATE sources SET generation = max(generation, ?) WHERE id = ?`, g.Generation, sourceID)
	if err != nil {
		return err
	}
	return mustAffect(res, "source", sourceID)
}

// Companion is a file beside a transcript that is archived with it: a
// Claude subagent meta.json or a tool-results/ file (decision 10).
type Companion struct {
	SessionID  string // owning conversation, when known
	Agent      transcript.Agent
	SourceID   int64
	Path       string
	Kind       string // subagent_meta | tool_result | ...
	Size       int64
	ContentSHA []byte
	// MessageNativeID names the tool_result row whose preview this file's
	// text replaced, when it did.
	MessageNativeID string
}

// UpsertCompanion records a companion file keyed by path.
func (s *Store) UpsertCompanion(ctx context.Context, c Companion) error {
	return s.write(ctx, func(w *writeTx) error {
		var convID, msgID sql.NullInt64
		if c.SessionID != "" {
			row, err := w.queryRow(`SELECT id FROM conversations WHERE device_id = ? AND agent = ? AND session_id = ?`, w.s.opts.DeviceID, string(c.Agent), c.SessionID)
			if err != nil {
				return err
			}
			if err := row.Scan(&convID); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if convID.Valid && c.MessageNativeID != "" {
			row, err := w.queryRow(`SELECT id FROM messages WHERE conversation_id = ? AND native_id = ? AND superseded_by IS NULL ORDER BY part LIMIT 1`, convID.Int64, c.MessageNativeID)
			if err != nil {
				return err
			}
			if err := row.Scan(&msgID); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		_, err := w.exec(`INSERT INTO companions (conversation_id, source_id, path, kind, size, content_sha, indexed_text, message_id, seen_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (path) DO UPDATE SET conversation_id = coalesce(excluded.conversation_id, conversation_id),
			  source_id = coalesce(excluded.source_id, source_id), kind = excluded.kind, size = excluded.size,
			  content_sha = excluded.content_sha, indexed_text = excluded.indexed_text,
			  message_id = coalesce(excluded.message_id, message_id), seen_at = excluded.seen_at`,
			convID, nullInt(c.SourceID), c.Path, c.Kind, c.Size, c.ContentSHA, boolInt(msgID.Valid), msgID, time.Now().UnixMilli())
		return err
	})
}

func mustAffect(res sql.Result, what string, id int64) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return &NotFoundError{What: what, ID: id}
	}
	return nil
}

// NotFoundError reports a missing row.
type NotFoundError struct {
	What string
	ID   int64
}

func (e *NotFoundError) Error() string { return "localindex: no " + e.What + " with id " + itoa(e.ID) }
