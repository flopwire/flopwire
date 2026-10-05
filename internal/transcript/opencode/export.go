package opencode

// Export and import of one session's rows (format opencode-export@2).
//
// The device does not upload opencode.db: it is one database for every
// session, rewritten in place, and it holds account tokens. It uploads one
// export per session instead, through devicesync's SyncExportFunc, at the
// source path ExportPath(db, id), the way it uploads Devin sessions. The
// server rebuilds a throwaway SQLite store from an export (LoadExport) and
// runs the same Parser over it, so the parser cursor carries over from one
// export generation to the next.
//
// Format: JSON lines, in order: one "session" record (absent when the
// session has no session row), the session's messages, then its parts,
// each in id order. A subagent's session record carries spawned_by, the
// parent's task part read at export time, and depth, since the export
// holds no rows of its ancestors. A session with no rows at all exports one "gone" record
// (never zero bytes); the parser then supersedes its rows.
//
// @2 appends new and changed records. LoadExport keeps the last record
// per key. Deletions start a whole export and a new sync generation.

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"

	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"
)

// ExportFormat names the export format; it is the source's parser string
// on the wire.
const ExportFormat = "opencode-export@2"

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
	T  string `json:"t"` // session | message | part | gone
	ID string `json:"id,omitempty"`

	// session
	ParentID     *string `json:"parent_id,omitempty"`
	Directory    *string `json:"directory,omitempty"`
	Title        *string `json:"title,omitempty"`
	Version      *string `json:"version,omitempty"`
	Agent        *string `json:"agent,omitempty"`
	Model        *string `json:"model,omitempty"`
	Slug         *string `json:"slug,omitempty"`
	TimeArchived *int64  `json:"time_archived,omitempty"`
	SpawnedBy    *string `json:"spawned_by,omitempty"`
	Depth        *int    `json:"depth,omitempty"` // a subagent's ancestors, read at export time

	// session, message, part
	TimeCreated *int64 `json:"time_created,omitempty"`
	TimeUpdated *int64 `json:"time_updated,omitempty"`

	// message, part
	MessageID *string `json:"message_id,omitempty"`
	Data      *string `json:"data,omitempty"`
}

// ListSessions returns every session id in the store (session rows and
// sessions that only have messages or parts), sorted.
func ListSessions(ctx context.Context, dbPath string) ([]string, error) {
	db, err := openReadOnly(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT id FROM session UNION SELECT session_id FROM message UNION SELECT session_id FROM part ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("opencode: list sessions: %w", err)
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

	sessions, err := loadSessions(ctx, tx)
	if err != nil {
		return nil, err
	}
	if s := sessions[sessionID]; s != nil {
		rec := exportRecord{T: "session", ID: s.id, ParentID: &s.parent, Directory: &s.cwd, Title: &s.title, Version: &s.version,
			Agent: &s.agent, Model: &s.model, Slug: &s.slug, TimeCreated: &s.created, TimeUpdated: &s.updated}
		if s.archived > 0 {
			rec.TimeArchived = &s.archived
		}
		if s.parent != "" {
			by, err := spawningCall(ctx, tx, s.parent, s.id)
			if err != nil {
				return nil, err
			}
			rec.SpawnedBy = &by
			depth := sessionDepth(sessions, s)
			rec.Depth = &depth
		}
		if err := enc.Encode(&rec); err != nil {
			return nil, err
		}
	}
	for _, q := range []struct{ t, sql string }{
		{"message", `SELECT id, NULL, time_created, time_updated, data FROM message WHERE session_id = ? ORDER BY id`},
		{"part", `SELECT id, message_id, time_created, time_updated, data FROM part WHERE session_id = ? ORDER BY id`},
	} {
		rows, err := tx.QueryContext(ctx, q.sql, sessionID)
		if err != nil {
			return nil, fmt.Errorf("opencode: export %s %s: %w", q.t, sessionID, err)
		}
		for rows.Next() {
			r := exportRecord{T: q.t}
			var msg sql.NullString
			var created, updated int64
			var data string
			if err := rows.Scan(&r.ID, &msg, &created, &updated, &data); err != nil {
				rows.Close()
				return nil, err
			}
			r.TimeCreated, r.TimeUpdated, r.Data = &created, &updated, &data
			if msg.Valid {
				r.MessageID = &msg.String
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
	}
	if buf.Len() == 0 {
		err := enc.Encode(exportRecord{T: "gone", ID: sessionID})
		return buf.Bytes(), err
	}
	return buf.Bytes(), nil
}

// exportSchema is the subset of the store the parser reads, plus
// session.spawned_by and session.depth.
const exportSchema = `
CREATE TABLE session (id TEXT PRIMARY KEY, parent_id TEXT, directory TEXT, title TEXT, version TEXT, agent TEXT,
  model TEXT, slug TEXT, time_created INTEGER, time_updated INTEGER, time_archived INTEGER, spawned_by TEXT, depth INTEGER);
CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL,
  time_updated INTEGER NOT NULL, data TEXT NOT NULL);
CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, time_created INTEGER NOT NULL,
  time_updated INTEGER NOT NULL, data TEXT NOT NULL);
CREATE INDEX part_session_idx ON part (session_id);
CREATE INDEX message_session_idx ON message (session_id);
`

// LoadExport builds a SQLite store at dir/opencode.db from an export of
// the session sessionID and returns its path, for Parser.Parse. Malformed
// lines are skipped, like any unknown record.
func LoadExport(ctx context.Context, r io.Reader, sessionID, dir string) (string, error) {
	path := filepath.Join(dir, "opencode.db")
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
		return "", fmt.Errorf("opencode: export schema: %w", err)
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
			_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO session VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, sessionID,
				rec.ParentID, rec.Directory, rec.Title, rec.Version, rec.Agent, rec.Model, rec.Slug,
				rec.TimeCreated, rec.TimeUpdated, rec.TimeArchived, rec.SpawnedBy, rec.Depth)
		case "message":
			if rec.ID == "" || rec.Data == nil || rec.TimeCreated == nil || rec.TimeUpdated == nil {
				continue
			}
			_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO message VALUES (?,?,?,?,?)`, rec.ID, sessionID,
				*rec.TimeCreated, *rec.TimeUpdated, *rec.Data)
		case "part":
			if rec.ID == "" || rec.MessageID == nil || rec.Data == nil || rec.TimeCreated == nil || rec.TimeUpdated == nil {
				continue
			}
			_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO part VALUES (?,?,?,?,?,?)`, rec.ID, *rec.MessageID, sessionID,
				*rec.TimeCreated, *rec.TimeUpdated, *rec.Data)
		}
		if err != nil {
			return "", fmt.Errorf("opencode: load export: %w", err)
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("opencode: read export: %w", err)
	}
	return path, tx.Commit()
}

// SessionContains reports whether any part of the session holds s, reading
// the store read-only. `flopwire probe` checks a subagent's session with it.
func SessionContains(ctx context.Context, dbPath, session, s string) (bool, error) {
	db, err := openReadOnly(dbPath)
	if err != nil {
		return false, err
	}
	defer db.Close()
	var n int
	err = db.QueryRowContext(ctx, `SELECT count(*) FROM part WHERE session_id = ? AND instr(data, ?) > 0`, session, s).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("opencode: search session %s: %w", session, err)
	}
	return n > 0, nil
}
