package devin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// toolState is the enrichment read from one tool_call_state row.
type toolState struct {
	isError    bool
	toolName   string
	enrichment map[string]any
}

// acpToolCall and acpToolCallUpdate are the serialized acp::ToolCall and
// acp::ToolCallUpdate payloads, reduced to the filter fields.
type acpToolCall struct {
	Kind      string `json:"kind"`
	Locations []struct {
		Path string `json:"path"`
	} `json:"locations"`
	RawInput struct {
		Command string `json:"command"`
	} `json:"rawInput"`
	Meta struct {
		ToolName string `json:"cognition.ai/inferenceToolName"`
	} `json:"_meta"`
}

type acpToolCallUpdate struct {
	Status string `json:"status"`
	Meta   struct {
		Cwd          string `json:"cognition.ai/cwd"`
		ToolName     string `json:"cognition.ai/inferenceToolName"`
		Canceled     bool   `json:"cognition.ai/canceled"`
		Rejected     bool   `json:"cognition.ai/rejected"`
		TerminalExit *struct {
			ExitCode *int64 `json:"exit_code"`
		} `json:"terminal_exit"`
	} `json:"_meta"`
}

// parseToolState builds enrichment for a tool call row: status, acp kind,
// commands [{cmd, cwd, exit_code}] for execute calls (the Codex
// codex-events@1 shape), changed_paths for edits, paths for reads and
// searches. is_error is status "failed" or a nonzero exit code. Malformed
// JSON degrades to fewer fields; it never fails the parse.
func parseToolState(callJSON, updateJSON sql.NullString) *toolState {
	var c acpToolCall
	var u acpToolCallUpdate
	if callJSON.Valid {
		_ = json.Unmarshal([]byte(callJSON.String), &c)
	}
	if updateJSON.Valid {
		_ = json.Unmarshal([]byte(updateJSON.String), &u)
	}
	e := map[string]any{}
	if u.Status != "" {
		e["status"] = u.Status
	} else if !updateJSON.Valid {
		e["status"] = "pending"
	}
	if c.Kind != "" {
		e["tool_kind"] = c.Kind
	}
	if u.Meta.Canceled {
		e["canceled"] = true
	}
	if u.Meta.Rejected {
		e["rejected"] = true
	}
	var exit *int64
	if u.Meta.TerminalExit != nil {
		exit = u.Meta.TerminalExit.ExitCode
	}
	if c.RawInput.Command != "" {
		cmd := map[string]any{"cmd": c.RawInput.Command}
		if u.Meta.Cwd != "" {
			cmd["cwd"] = u.Meta.Cwd
		}
		if exit != nil {
			cmd["exit_code"] = *exit
		}
		e["commands"] = []map[string]any{cmd}
	}
	var paths []string
	for _, l := range c.Locations {
		if l.Path != "" {
			paths = append(paths, l.Path)
		}
	}
	if len(paths) > 0 {
		if c.Kind == "edit" {
			e["changed_paths"] = paths
		} else {
			e["paths"] = paths
		}
	}
	ts := &toolState{
		isError:    u.Status == "failed" || (exit != nil && *exit != 0),
		toolName:   firstNonEmpty(u.Meta.ToolName, c.Meta.ToolName),
		enrichment: e,
	}
	return ts
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// toolLookup reads tool_call_state rows of one session by primary key.
type toolLookup struct {
	stmt      *sql.Stmt
	ctx       context.Context
	sessionID string
	err       error
}

func newToolLookup(ctx context.Context, tx *sql.Tx) (*toolLookup, error) {
	stmt, err := tx.PrepareContext(ctx, `
		SELECT tool_call_json, tool_call_update_json
		  FROM tool_call_state
		 WHERE session_id = ? AND tool_call_id = ?`)
	if err != nil {
		return nil, fmt.Errorf("devin: prepare tool_call_state: %w", err)
	}
	return &toolLookup{stmt: stmt, ctx: ctx}, nil
}

func (l *toolLookup) get(callID string) *toolState {
	if callID == "" || l.err != nil {
		return nil
	}
	var c, u sql.NullString
	err := l.stmt.QueryRowContext(l.ctx, l.sessionID, callID).Scan(&c, &u)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		l.err = fmt.Errorf("devin: read tool_call_state %s/%s: %w", l.sessionID, callID, err)
		return nil
	}
	return parseToolState(c, u)
}

// toolTuple is the cheap per-session change signal for tool_call_state:
// row count, max rowid and rows still awaiting their completion update.
// Rows go from update NULL to a final update once; a later in-place rewrite
// of an already settled row does not move the tuple and is not detected.
type toolTuple struct {
	Count   int64 `json:"n"`
	MaxRow  int64 `json:"max"`
	Pending int64 `json:"pend"`
}

// loadToolTuple reads one session's tuple through the primary key index.
func loadToolTuple(ctx context.Context, tx *sql.Tx, sessionID string) (toolTuple, error) {
	var t toolTuple
	var maxRow, pending sql.NullInt64
	err := tx.QueryRowContext(ctx, `
		SELECT count(*), max(rowid), sum(tool_call_update_json IS NULL)
		  FROM tool_call_state
		 WHERE session_id = ?`, sessionID).Scan(&t.Count, &maxRow, &pending)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return toolTuple{}, nil // older stores
		}
		return t, fmt.Errorf("devin: tool_call_state tuple %s: %w", sessionID, err)
	}
	t.MaxRow, t.Pending = maxRow.Int64, pending.Int64
	return t, nil
}

// changedToolCalls lists the session's tool_call_ids that may have changed
// since prev: rows added after prev.MaxRow, and rows that were pending then
// (pendingIDs). When pendingIDs was not recorded (too many), every row
// still pending or settled after prev is a candidate: all ids.
func changedToolCalls(ctx context.Context, tx *sql.Tx, sessionID string, prevMax int64, pendingIDs []string, pendingOverflow bool) ([]string, error) {
	q := `SELECT tool_call_id FROM tool_call_state WHERE session_id = ? AND rowid > ?`
	args := []any{sessionID, prevMax}
	if pendingOverflow {
		q = `SELECT tool_call_id FROM tool_call_state WHERE session_id = ?`
		args = []any{sessionID}
	}
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("devin: changed tool calls %s: %w", sessionID, err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, id := range pendingIDs {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

// maxPendingIDs bounds the pending tool call ids kept in the cursor state
// per session. Beyond it the state records an overflow and the next change
// re-reads every tool call of the session.
const maxPendingIDs = 64

func pendingToolCalls(ctx context.Context, tx *sql.Tx, sessionID string) (ids []string, overflow bool, err error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT tool_call_id FROM tool_call_state
		 WHERE session_id = ? AND tool_call_update_json IS NULL
		 LIMIT ?`, sessionID, maxPendingIDs+1)
	if err != nil {
		return nil, false, fmt.Errorf("devin: pending tool calls %s: %w", sessionID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, false, err
		}
		ids = append(ids, id)
	}
	if len(ids) > maxPendingIDs {
		return nil, true, rows.Err()
	}
	return ids, false, rows.Err()
}

// rowsReferencingCalls returns row_ids of the session's nodes that carry
// one of ids: as a tool call of an assistant node or as the tool_call_id of
// a tool node.
func rowsReferencingCalls(ctx context.Context, tx *sql.Tx, sessionID string, ids []string) (map[int64]bool, error) {
	out := map[int64]bool{}
	for start := 0; start < len(ids); start += 200 {
		chunk := ids[start:min(start+200, len(ids))]
		in := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := []any{sessionID}
		for _, id := range chunk {
			args = append(args, id)
		}
		args = append(args, args[1:]...)
		rows, err := tx.QueryContext(ctx, `
			SELECT row_id FROM message_nodes m
			 WHERE session_id = ?
			   AND (json_extract(chat_message, '$.tool_call_id') IN (`+in+`)
			        OR EXISTS (SELECT 1 FROM json_each(m.chat_message, '$.tool_calls') j
			                    WHERE json_extract(j.value, '$.id') IN (`+in+`)))`, args...)
		if err != nil {
			return nil, fmt.Errorf("devin: rows referencing tool calls %s: %w", sessionID, err)
		}
		for rows.Next() {
			var r int64
			if err := rows.Scan(&r); err != nil {
				rows.Close()
				return nil, err
			}
			out[r] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
