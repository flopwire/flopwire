package opencode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Row metadata is kept instead of transcript content. Comparing IDs also
// detects a delete paired with an insert, even when the row count is unchanged.
type exportRowState struct {
	RowID   int64  `json:"r"`
	Created int64  `json:"c"`
	Updated int64  `json:"u"`
	Bytes   int64  `json:"b"`
	Message string `json:"m,omitempty"`
	Hash    string `json:"h"`
}

type exportState struct {
	Version    int                       `json:"v"`
	SessionID  string                    `json:"id"`
	Session    [32]byte                  `json:"s"`
	HasSession bool                      `json:"has_session"`
	Messages   map[string]exportRowState `json:"messages"`
	Parts      map[string]exportRowState `json:"parts"`
}

// ExportFrom returns new and changed records since prev, or a whole export
// when state is unusable or a row vanished. State must be saved with its
// export bytes. All records end on a newline for independent redaction.
//
// Metadata is checked for every row, but content is read only for new or
// changed rows and those within lagMS of the previous highest timestamp.
// The overlap covers writes stamped before a concurrent snapshot, and
// same-millisecond streaming edits. Rewriting old content without changing
// its timestamp, rowid or byte length outside that window is not detected.
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
	e := exportWriter{ctx: ctx, tx: tx, id: sessionID}
	var old exportState
	appended = len(prev) != 0 && json.Unmarshal(prev, &old) == nil && old.Version == 1 && old.SessionID == sessionID && old.Messages != nil && old.Parts != nil
	session, err := e.session()
	if err != nil {
		return nil, false, nil, err
	}
	messages, err := e.rows("message")
	if err != nil {
		return nil, false, nil, err
	}
	parts, err := e.rows("part")
	if err != nil {
		return nil, false, nil, err
	}
	if appended && (old.HasSession && len(session) == 0 || missingExportRows(old.Messages, messages) || missingExportRows(old.Parts, parts)) {
		appended = false
	}
	if !appended {
		old = exportState{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if len(session) != 0 && (!old.HasSession || sha256.Sum256(session) != old.Session) {
		if err := enc.Encode(session); err != nil {
			return nil, false, nil, err
		}
	}
	if err := e.writeRows(enc, "message", messages, old.Messages); err != nil {
		return nil, false, nil, err
	}
	if err := e.writeRows(enc, "part", parts, old.Parts); err != nil {
		return nil, false, nil, err
	}
	if !appended && buf.Len() == 0 {
		if err := enc.Encode(exportRecord{T: "gone", ID: sessionID}); err != nil {
			return nil, false, nil, err
		}
	}
	next := exportState{Version: 1, SessionID: sessionID, Session: sha256.Sum256(session), HasSession: len(session) != 0, Messages: messages, Parts: parts}
	state, err = json.Marshal(next)
	return buf.Bytes(), appended, state, err
}

func missingExportRows(old, current map[string]exportRowState) bool {
	for id := range old {
		if _, ok := current[id]; !ok {
			return true
		}
	}
	return false
}

type exportWriter struct {
	ctx context.Context
	tx  *sql.Tx
	id  string
}

func (e *exportWriter) session() (json.RawMessage, error) {
	sessions, err := loadSessions(e.ctx, e.tx)
	if err != nil {
		return nil, err
	}
	s := sessions[e.id]
	if s == nil {
		return nil, nil
	}
	r := exportRecord{T: "session", ID: s.id, ParentID: &s.parent, Directory: &s.cwd, Title: &s.title, Version: &s.version,
		Agent: &s.agent, Model: &s.model, Slug: &s.slug, TimeCreated: &s.created, TimeUpdated: &s.updated}
	if s.archived > 0 {
		r.TimeArchived = &s.archived
	}
	if s.parent != "" {
		by, err := spawningCall(e.ctx, e.tx, s.parent, s.id)
		if err != nil {
			return nil, err
		}
		r.SpawnedBy = &by
		depth := sessionDepth(sessions, s)
		r.Depth = &depth
	}
	// Use the wire encoding so the next state compares the same bytes.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func (e *exportWriter) rows(table string) (map[string]exportRowState, error) {
	message := "NULL"
	if table == "part" {
		message = "message_id"
	}
	// octet_length reads the stored byte length without loading old content.
	// https://www.sqlite.org/lang_corefunc.html#octet_length
	rows, err := e.tx.QueryContext(e.ctx, `SELECT id, rowid, time_created, time_updated, octet_length(data), `+message+` FROM `+table+` WHERE session_id = ?`, e.id)
	if err != nil {
		return nil, fmt.Errorf("opencode: export %s metadata: %w", table, err)
	}
	defer rows.Close()
	out := map[string]exportRowState{}
	for rows.Next() {
		var id string
		var s exportRowState
		var msg sql.NullString
		if err := rows.Scan(&id, &s.RowID, &s.Created, &s.Updated, &s.Bytes, &msg); err != nil {
			return nil, err
		}
		s.Message = msg.String
		out[id] = s
	}
	return out, rows.Err()
}

func (e *exportWriter) writeRows(enc *json.Encoder, table string, current, old map[string]exportRowState) error {
	var watermark int64
	for _, s := range old {
		watermark = max(watermark, s.Updated)
	}
	// Fetch candidates in bounded batches, avoiding one content query per row.
	var ids []string
	for id, s := range current {
		before, ok := old[id]
		s.Hash = before.Hash
		current[id] = s
		if !ok || s != before || s.Updated >= watermark-lagMS {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	for batch := range slices.Chunk(ids, 500) {
		args := []any{e.id}
		for _, id := range batch {
			args = append(args, id)
		}
		message := "NULL"
		if table == "part" {
			message = "message_id"
		}
		q := `SELECT id, ` + message + `, time_created, time_updated, data FROM ` + table + ` WHERE session_id = ? AND id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",") + `) ORDER BY id`
		rows, err := e.tx.QueryContext(e.ctx, q, args...)
		if err != nil {
			return fmt.Errorf("opencode: export %s content: %w", table, err)
		}
		for rows.Next() {
			r := exportRecord{T: table}
			var msg sql.NullString
			var created, updated int64
			var data string
			if err := rows.Scan(&r.ID, &msg, &created, &updated, &data); err != nil {
				rows.Close()
				return err
			}
			r.TimeCreated, r.TimeUpdated, r.Data = &created, &updated, &data
			if msg.Valid {
				r.MessageID = &msg.String
			}
			b, err := json.Marshal(r)
			if err != nil {
				rows.Close()
				return err
			}
			s := current[r.ID]
			sum := sha256.Sum256(b)
			s.Hash = hex.EncodeToString(sum[:])
			current[r.ID] = s
			if before, ok := old[r.ID]; !ok || s.Hash != before.Hash {
				if err := enc.Encode(r); err != nil {
					rows.Close()
					return err
				}
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}
