package devin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Turn and session state the device agent reads for presence.
//
// Devin fires no Stop hook for a turn the user interrupts (Esc twice or
// Ctrl-C in the TUI). It writes a marker node instead, as the turn's last
// node (probes, devin 3000.11.1, 2026-10-04):
//
//   - interrupted while the model responds: a system node
//     "[Response interrupted by user]";
//   - interrupted while a tool runs (run_subagent included): the tool's
//     result node "Canceled due to user interrupt", with
//     metadata.extensions."chisel/tool_failure".reason "Canceled".
//
// Both carry metadata.created_at, the time of the interrupt. Devin
// rewrites a session's rows from time to time; the JSON, created_at
// included, is kept, while the row's own created_at column is not.

// InterruptRecentNodes is how many of a session's newest nodes
// InterruptedSince reads: an interrupt is the turn's last node, and a
// subagent sent to the background can add its nodes after it.
const InterruptRecentNodes = 16

// interruptSQL reads the newest nodes that may be interrupt markers.
const interruptSQL = `SELECT chat_message FROM (
	SELECT row_id, chat_message FROM message_nodes WHERE session_id = ? ORDER BY row_id DESC LIMIT ?)
	WHERE instr(chat_message, 'interrupt') > 0 OR instr(chat_message, '"Canceled"') > 0`

// InterruptedSince reports whether one of the session's newest nodes marks
// a turn the user interrupted at or after since.
func InterruptedSince(ctx context.Context, dbPath, session string, since time.Time) (bool, error) {
	if session == "" {
		return false, nil
	}
	db, err := openForHook(dbPath)
	if err != nil {
		return false, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, interruptSQL, session, InterruptRecentNodes)
	if err != nil {
		return false, fmt.Errorf("devin: interrupt: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return false, fmt.Errorf("devin: interrupt: %w", err)
		}
		if at, ok := interruptAt(raw); ok && !at.Before(since) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// interruptAt is when a node marks an interrupted turn.
func interruptAt(raw string) (time.Time, bool) {
	var n struct {
		Role     string `json:"role"`
		Content  any    `json:"content"`
		Metadata struct {
			CreatedAt  string `json:"created_at"`
			Extensions struct {
				Failure struct {
					Reason string `json:"reason"`
				} `json:"chisel/tool_failure"`
			} `json:"extensions"`
		} `json:"metadata"`
	}
	if json.Unmarshal([]byte(raw), &n) != nil {
		return time.Time{}, false
	}
	text, _ := n.Content.(string)
	var marker bool
	switch n.Role {
	case "system":
		marker = strings.HasPrefix(text, "[Response interrupted by user]")
	case "tool":
		marker = strings.HasPrefix(text, "Canceled due to user interrupt") || n.Metadata.Extensions.Failure.Reason == "Canceled"
	}
	if !marker {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, n.Metadata.CreatedAt)
	return at, err == nil
}

// Sessions reports which of ids the store holds: a session deleted from
// Devin (`devin rm`, ACP session/delete) is gone from its sessions table.
func Sessions(ctx context.Context, dbPath string, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	db, err := openForHook(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	b, _ := json.Marshal(ids)
	rows, err := db.QueryContext(ctx, `SELECT id FROM sessions WHERE id IN (SELECT value FROM json_each(?))`, string(b))
	if err != nil {
		return nil, fmt.Errorf("devin: sessions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("devin: sessions: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}
