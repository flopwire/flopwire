package devin

// Export and import of one session's rows (format devin-export@1).
//
// The device does not upload sessions.db: it is one 650MB database for every
// session, rewritten in place. It uploads one export per session instead,
// through devicesync's SyncExport, at the source path ExportPath(db, id).
// The server rebuilds a throwaway SQLite store from an export (LoadExport)
// and runs the same Parser over it, so every rule of the parser (main chain,
// compaction copies, tool_call_state, deletions) applies unchanged, and the
// parser cursor carries over from one export generation to the next.
//
// Format: JSON lines, in order: one "session" record (absent when the
// session has no sessions row), the session's message_nodes in row_id
// order, then its tool_call_state rows in rowid order. Row ids are kept, so
// the parser's row-id watermarks mean the same thing on both sides. A
// session with neither a sessions row nor nodes exports one "gone" record
// (never zero bytes, which a device cannot tell apart from nothing to
// send); the parser then supersedes its rows.

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"
)

// ExportFormat names the export format; it is the source's parser string
// suffix on the wire.
const ExportFormat = "devin-export@1"

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

// Export returns one session's export, read in one snapshot of the store.
func Export(ctx context.Context, dbPath, sessionID string) ([]byte, error) {
	db, err := openReadOnly(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	tx, err := snapshot(ctx, db)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // read-only
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	var s exportRecord
	var chain sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT id, working_directory, backend_type, model, agent_mode, created_at,
		last_activity_at, title, main_chain_id, COALESCE(hidden, 0) FROM sessions WHERE id = ?`, sessionID).
		Scan(&s.ID, &s.WorkingDirectory, &s.BackendType, &s.Model, &s.AgentMode, &s.CreatedAt, &s.LastActivityAt, &s.Title, &chain, &s.Hidden)
	switch {
	case err == nil:
		s.T = "session"
		if chain.Valid {
			s.MainChainID = &chain.Int64
		}
		if err := enc.Encode(&s); err != nil {
			return nil, err
		}
	case !errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("devin: export session %s: %w", sessionID, err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT row_id, node_id, parent_node_id, chat_message, created_at
		FROM message_nodes WHERE session_id = ? ORDER BY row_id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("devin: export nodes %s: %w", sessionID, err)
	}
	for rows.Next() {
		r := exportRecord{T: "node"}
		var parent sql.NullInt64
		var created int64
		if err := rows.Scan(&r.RowID, &r.NodeID, &parent, &r.ChatMessage, &created); err != nil {
			rows.Close()
			return nil, err
		}
		r.CreatedAt = &created
		if parent.Valid {
			r.ParentNodeID = &parent.Int64
		}
		if err := enc.Encode(&r); err != nil {
			rows.Close()
			return nil, err
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if buf.Len() == 0 {
		err := enc.Encode(exportRecord{T: "gone", ID: sessionID})
		return buf.Bytes(), err
	}
	rows, err = tx.QueryContext(ctx, `SELECT rowid, tool_call_id, tool_call_json, tool_call_update_json
		FROM tool_call_state WHERE session_id = ? ORDER BY rowid`, sessionID)
	if err != nil {
		// Stores without the table have no tool state to export.
		if strings.Contains(err.Error(), "no such table") {
			return buf.Bytes(), nil
		}
		return nil, fmt.Errorf("devin: export tool state %s: %w", sessionID, err)
	}
	defer rows.Close()
	for rows.Next() {
		r := exportRecord{T: "tool"}
		if err := rows.Scan(&r.Rowid, &r.ToolCallID, &r.ToolCallJSON, &r.ToolCallUpdateJSON); err != nil {
			return nil, err
		}
		if err := enc.Encode(&r); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), rows.Err()
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
// the session sessionID and returns its path, for Parser.Parse. Malformed
// lines are skipped, like any unknown record.
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
