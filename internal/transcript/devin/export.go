package devin

// Export and import of one session's rows (format devin-export@2).
//
// The device does not upload sessions.db: it is one 650MB database for every
// session, rewritten in place. It uploads one export per session instead,
// through devicesync's SyncExportFunc, at the source path
// ExportPath(db, id). The server rebuilds a throwaway SQLite store from an
// export (LoadExport) and runs the same Parser over it, so every rule of
// the parser (main chain, compaction copies, tool_call_state, deletions)
// applies unchanged, and the parser cursor carries over from one export
// generation to the next.
//
// Format: JSON lines. A whole export (ExportFrom with no state) is one
// "session" record (absent when the session has no sessions row), the
// session's message_nodes in row_id order, then its tool_call_state rows
// in rowid order. Row ids are kept, so the parser's row-id watermarks mean
// the same thing on both sides. A session with neither a sessions row nor
// nodes exports one "gone" record (never zero bytes, which a device cannot
// tell apart from nothing to send); the parser then supersedes its rows.
//
// @2: the export is append-only while the session only grows. Each change
// appends what it added (ExportFrom with the previous state): a session
// record when the sessions row changed, the new nodes, new and newly
// settled tool rows. So a record may appear more than once; the last one
// for a key wins (LoadExport upserts). A live session's sync then costs
// what changed, not the session: a 30k-node, 255MB session was exported,
// redacted and chunked whole every 10s. A change an append cannot carry
// (a deleted row) starts the export over, which syncs as a new generation.

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
)

// ExportFormat names the export format; it is the source's parser string
// suffix on the wire.
const ExportFormat = "devin-export@2"

// ExportPath is the device source path of one session's export.
func ExportPath(dbPath, sessionID string) string { return dbPath + "#" + sessionID }

// SessionOfExport returns the session id of an ExportPath, or "".
func SessionOfExport(path string) string {
	if i := strings.LastIndexByte(path, '#'); i >= 0 {
		return path[i+1:]
	}
	return ""
}

type exportRecord struct {
	T string `json:"t"` // session | node | tool

	// session
	ID               string  `json:"id,omitempty"`
	WorkingDirectory *string `json:"working_directory,omitempty"`
	BackendType      *string `json:"backend_type,omitempty"`
	Model            *string `json:"model,omitempty"`
	AgentMode        *string `json:"agent_mode,omitempty"`
	CreatedAt        *int64  `json:"created_at,omitempty"`
	LastActivityAt   *int64  `json:"last_activity_at,omitempty"`
	Title            *string `json:"title,omitempty"`
	MainChainID      *int64  `json:"main_chain_id,omitempty"`
	Hidden           int64   `json:"hidden,omitempty"`

	// node
	RowID        int64   `json:"row_id,omitempty"`
	NodeID       int64   `json:"node_id,omitempty"`
	ParentNodeID *int64  `json:"parent_node_id,omitempty"`
	ChatMessage  *string `json:"chat_message,omitempty"`

	// tool
	Rowid              int64   `json:"rowid,omitempty"`
	ToolCallID         string  `json:"tool_call_id,omitempty"`
	ToolCallJSON       *string `json:"tool_call_json,omitempty"`
	ToolCallUpdateJSON *string `json:"tool_call_update_json,omitempty"`
}

// ListSessions returns every session id in the store (sessions rows and
// sessions that only have nodes), sorted.
func ListSessions(ctx context.Context, dbPath string) ([]string, error) {
	db, err := openReadOnly(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT id FROM sessions UNION SELECT session_id FROM message_nodes ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("devin: list sessions: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Export returns one session's whole export, read in one snapshot of the
// store.
func Export(ctx context.Context, dbPath, sessionID string) ([]byte, error) {
	data, _, _, err := ExportFrom(ctx, dbPath, sessionID, nil)
	return data, err
}

// exportState is what an incremental export resumes from: what the
// export so far holds of the session. ExportFrom returns it encoded; the
// caller keeps it with the bytes it describes (devicesync saves it with
// the capture) and passes it back.
type exportState struct {
	V       int     `json:"v"`
	Gone    bool    `json:"gone,omitempty"` // the export is the "gone" record
	Session bool    `json:"s,omitempty"`    // a session record was exported
	Meta    uint64  `json:"meta,omitempty"` // hash of the last session record
	Nodes   int64   `json:"n"`              // message_nodes rows exported
	MaxRow  int64   `json:"max"`            // highest row_id exported
	Tools   int64   `json:"tn"`             // tool_call_state rows at the last export
	ToolMax int64   `json:"tmax"`           // highest tool_call_state rowid exported
	Pending []int64 `json:"pend,omitempty"` // tool rows exported without their completion update
}

const exportStateVersion = 1

// ExportFrom returns what the session's export gained since the export
// that prev describes (appended true), or, when prev is nil or unusable
// or the store changed in a way an append cannot carry, the whole export
// (appended false). state describes the export after data.
//
// An append carries: the session record when the sessions row changed,
// message_nodes rows past the exported row_id, and tool_call_state rows
// that are new or were exported awaiting their completion update. Records
// repeat across appends; LoadExport keeps the last of each. A row deleted
// at or below the exported row_id, a sessions row that vanished, fewer
// tool_call_state rows, or a vanished session yield the whole export. Like
// the parser, an in-place rewrite of an already exported node or settled
// tool row is not detected.
func ExportFrom(ctx context.Context, dbPath, sessionID string, prev []byte) (data []byte, appended bool, state []byte, err error) {
	db, err := openReadOnly(dbPath)
	if err != nil {
		return nil, false, nil, err
	}
	defer db.Close()
	tx, err := snapshot(ctx, db)
	if err != nil {
		return nil, false, nil, err
	}
	defer tx.Rollback() //nolint:errcheck // read-only
	e := &exporter{ctx: ctx, tx: tx, id: sessionID}
	e.enc = json.NewEncoder(&e.buf)
	e.enc.SetEscapeHTML(false)
	var st exportState
	if prev != nil && json.Unmarshal(prev, &st) == nil && st.V == exportStateVersion {
		ok, err := e.appendTo(&st)
		if err != nil {
			return nil, false, nil, err
		}
		if ok {
			state, err := json.Marshal(&st)
			return e.buf.Bytes(), true, state, err
		}
		e.buf.Reset()
	}
	st, err = e.full()
	if err != nil {
		return nil, false, nil, err
	}
	state, err = json.Marshal(&st)
	return e.buf.Bytes(), false, state, err
}

type exporter struct {
	ctx context.Context
	tx  *sql.Tx
	id  string
	buf bytes.Buffer
	enc *json.Encoder
}

// session reads the sessions row as a record and its hash; nil when there
// is none.
func (e *exporter) session() (*exportRecord, uint64, error) {
	var s exportRecord
	var chain sql.NullInt64
	err := e.tx.QueryRowContext(e.ctx, `SELECT id, working_directory, backend_type, model, agent_mode, created_at,
		last_activity_at, title, main_chain_id, COALESCE(hidden, 0) FROM sessions WHERE id = ?`, e.id).
		Scan(&s.ID, &s.WorkingDirectory, &s.BackendType, &s.Model, &s.AgentMode, &s.CreatedAt, &s.LastActivityAt, &s.Title, &chain, &s.Hidden)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("devin: export session %s: %w", e.id, err)
	}
	s.T = "session"
	if chain.Valid {
		s.MainChainID = &chain.Int64
	}
	b, err := json.Marshal(&s)
	if err != nil {
		return nil, 0, err
	}
	h := fnv.New64a()
	h.Write(b)
	return &s, h.Sum64(), nil
}

// nodes writes the session's message_nodes rows past row_id after, in
// row_id order, and returns how many and the highest row_id.
func (e *exporter) nodes(after int64) (n, maxRow int64, err error) {
	rows, err := e.tx.QueryContext(e.ctx, `SELECT row_id, node_id, parent_node_id, chat_message, created_at
		FROM message_nodes WHERE session_id = ? AND row_id > ? ORDER BY row_id`, e.id, after)
	if err != nil {
		return 0, 0, fmt.Errorf("devin: export nodes %s: %w", e.id, err)
	}
	defer rows.Close()
	for rows.Next() {
		r := exportRecord{T: "node"}
		var parent sql.NullInt64
		var created int64
		if err := rows.Scan(&r.RowID, &r.NodeID, &parent, &r.ChatMessage, &created); err != nil {
			return 0, 0, err
		}
		r.CreatedAt = &created
		if parent.Valid {
			r.ParentNodeID = &parent.Int64
		}
		if err := e.enc.Encode(&r); err != nil {
			return 0, 0, err
		}
		n, maxRow = n+1, r.RowID
	}
	return n, maxRow, rows.Err()
}

// tools writes the session's tool_call_state rows past rowid after, then
// those among also (rows exported pending) that now have their completion
// update. It returns the rows past after and among also that still await
// their update, and the highest rowid written.
func (e *exporter) tools(after int64, also []int64) (pending []int64, maxRow int64, err error) {
	maxRow = after
	write := func(q string, args ...any) error {
		rows, err := e.tx.QueryContext(e.ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r := exportRecord{T: "tool"}
			if err := rows.Scan(&r.Rowid, &r.ToolCallID, &r.ToolCallJSON, &r.ToolCallUpdateJSON); err != nil {
				return err
			}
			if err := e.enc.Encode(&r); err != nil {
				return err
			}
			if r.ToolCallUpdateJSON == nil {
				pending = append(pending, r.Rowid)
			}
			maxRow = max(maxRow, r.Rowid)
		}
		return rows.Err()
	}
	const cols = `SELECT rowid, tool_call_id, tool_call_json, tool_call_update_json FROM tool_call_state WHERE session_id = ?`
	if err := write(cols+` AND rowid > ? ORDER BY rowid`, e.id, after); err != nil {
		// Stores without the table have no tool state to export.
		if strings.Contains(err.Error(), "no such table") {
			return nil, after, nil
		}
		return nil, 0, fmt.Errorf("devin: export tool state %s: %w", e.id, err)
	}
	for part := range slices.Chunk(also, 500) {
		args := []any{e.id}
		for _, r := range part {
			args = append(args, r)
		}
		in := ` AND rowid IN (` + strings.TrimSuffix(strings.Repeat("?,", len(part)), ",") + `)`
		if err := write(cols+in+` AND tool_call_update_json IS NOT NULL ORDER BY rowid`, args...); err != nil {
			return nil, 0, fmt.Errorf("devin: export settled tools %s: %w", e.id, err)
		}
		rows, err := e.tx.QueryContext(e.ctx, `SELECT rowid FROM tool_call_state WHERE session_id = ?`+in+` AND tool_call_update_json IS NULL`, args...)
		if err != nil {
			return nil, 0, fmt.Errorf("devin: export pending tools %s: %w", e.id, err)
		}
		for rows.Next() {
			var r int64
			if err := rows.Scan(&r); err != nil {
				rows.Close()
				return nil, 0, err
			}
			pending = append(pending, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, 0, err
		}
	}
	slices.Sort(pending)
	return pending, maxRow, nil
}

// toolCount is the session's tool_call_state row count.
func (e *exporter) toolCount() (int64, error) {
	var n int64
	err := e.tx.QueryRowContext(e.ctx, `SELECT count(*) FROM tool_call_state WHERE session_id = ?`, e.id).Scan(&n)
	if err != nil && strings.Contains(err.Error(), "no such table") {
		return 0, nil
	}
	return n, err
}

// full writes the whole export: the session record, every node, every
// tool row; or the "gone" record.
func (e *exporter) full() (exportState, error) {
	st := exportState{V: exportStateVersion}
	s, meta, err := e.session()
	if err != nil {
		return st, err
	}
	if s != nil {
		if err := e.enc.Encode(s); err != nil {
			return st, err
		}
		st.Session, st.Meta = true, meta
	}
	if st.Nodes, st.MaxRow, err = e.nodes(0); err != nil {
		return st, err
	}
	if e.buf.Len() == 0 {
		return exportState{V: exportStateVersion, Gone: true}, e.enc.Encode(exportRecord{T: "gone", ID: e.id})
	}
	if st.Pending, st.ToolMax, err = e.tools(0, nil); err != nil {
		return st, err
	}
	st.Tools, err = e.toolCount()
	return st, err
}

// appendTo writes what the session gained since st and updates st; false
// when an append cannot carry the change (the caller exports whole).
func (e *exporter) appendTo(st *exportState) (bool, error) {
	s, meta, err := e.session()
	if err != nil {
		return false, err
	}
	var count, upTo int64
	if err := e.tx.QueryRowContext(e.ctx, `SELECT count(*), COALESCE(sum(row_id <= ?), 0) FROM message_nodes WHERE session_id = ?`,
		st.MaxRow, e.id).Scan(&count, &upTo); err != nil {
		return false, fmt.Errorf("devin: export count %s: %w", e.id, err)
	}
	if st.Gone {
		return s == nil && count == 0, nil // still gone: nothing to add
	}
	if s == nil && (st.Session || count == 0) || upTo != st.Nodes {
		return false, nil
	}
	tools, err := e.toolCount()
	if err != nil {
		return false, fmt.Errorf("devin: export tool count %s: %w", e.id, err)
	}
	if tools < st.Tools {
		return false, nil
	}
	if s != nil && (!st.Session || meta != st.Meta) {
		if err := e.enc.Encode(s); err != nil {
			return false, err
		}
		st.Session, st.Meta = true, meta
	}
	n, maxRow, err := e.nodes(st.MaxRow)
	if err != nil {
		return false, err
	}
	if n > 0 {
		st.Nodes, st.MaxRow = st.Nodes+n, maxRow
	}
	pending, toolMax, err := e.tools(st.ToolMax, st.Pending)
	if err != nil {
		return false, err
	}
	// A pending row that vanished (replaced under a new rowid, written
	// above) is dropped from the pending set.
	st.Pending, st.ToolMax, st.Tools = pending, toolMax, tools
	return true, nil
}

// exportSchema is the subset of the store the parser reads.
const exportSchema = `
CREATE TABLE sessions (id TEXT PRIMARY KEY, working_directory TEXT, backend_type TEXT, model TEXT,
  agent_mode TEXT, created_at INTEGER, last_activity_at INTEGER, title TEXT, main_chain_id INTEGER, hidden INTEGER);
CREATE TABLE message_nodes (row_id INTEGER PRIMARY KEY, session_id TEXT NOT NULL, node_id INTEGER NOT NULL,
  parent_node_id INTEGER, chat_message TEXT NOT NULL, created_at INTEGER NOT NULL, UNIQUE(session_id, node_id));
CREATE TABLE tool_call_state (session_id TEXT NOT NULL, tool_call_id TEXT NOT NULL, tool_call_json TEXT,
  tool_call_update_json TEXT, PRIMARY KEY (session_id, tool_call_id));
`

// LoadExport builds a SQLite store at dir/sessions.db from an export of
// the session sessionID and returns its path, for Parser.Parse. A record
// that repeats a key (an appended export) replaces the earlier one.
// Malformed lines are skipped, like any unknown record.
func LoadExport(ctx context.Context, r io.Reader, sessionID, dir string) (string, error) {
	path := filepath.Join(dir, "sessions.db")
	q := url.Values{}
	q.Add("_pragma", "journal_mode(off)")
	q.Add("_pragma", "synchronous(off)")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String())
	if err != nil {
		return "", err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, exportSchema); err != nil {
		return "", fmt.Errorf("devin: export schema: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback() //nolint:errcheck // committed below
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 256<<20)
	for sc.Scan() {
		var rec exportRecord
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		switch rec.T {
		case "session":
			_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO sessions VALUES (?,?,?,?,?,?,?,?,?,?)`, sessionID,
				rec.WorkingDirectory, rec.BackendType, rec.Model, rec.AgentMode, rec.CreatedAt, rec.LastActivityAt, rec.Title, rec.MainChainID, rec.Hidden)
		case "node":
			if rec.ChatMessage == nil || rec.CreatedAt == nil {
				continue
			}
			_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO message_nodes VALUES (?,?,?,?,?,?)`, rec.RowID, sessionID,
				rec.NodeID, rec.ParentNodeID, *rec.ChatMessage, *rec.CreatedAt)
		case "tool":
			_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO tool_call_state(rowid, session_id, tool_call_id, tool_call_json, tool_call_update_json) VALUES (?,?,?,?,?)`,
				rec.Rowid, sessionID, rec.ToolCallID, rec.ToolCallJSON, rec.ToolCallUpdateJSON)
		}
		if err != nil {
			return "", fmt.Errorf("devin: load export: %w", err)
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("devin: read export: %w", err)
	}
	return path, tx.Commit()
}
