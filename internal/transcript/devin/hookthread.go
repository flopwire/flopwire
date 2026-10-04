package devin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"

	"github.com/flopwire/flopwire/internal/fsprobe"
)

// Which thread of a Devin session a hook runs in (issue #107).
//
// A run_subagent subagent runs inside its session: its hooks carry the
// session's id, the same input fields and the same environment as the
// session's own hooks, and nothing marks them (probes, devin 3000.11.1,
// 2026-10-03). The store tells them apart:
//
//   - The session's own assistant node, with its tool calls, is in
//     message_nodes before the tool's PreToolUse hook runs.
//   - A subagent's nodes are written when it finishes, on a root of their
//     own. While it runs, none of its tool calls is in the store, and the
//     run_subagent call that started it has no tool result.
//
// So a PostToolUse is the session's own only when its tool_use_id is a tool
// call of one of the session's last nodes, and a Stop is a subagent's
// while a run_subagent call among those nodes has no result. Both read
// only the session's last HookRecentNodes rows, newest first, by the
// message_nodes session index.
//
// A run_subagent call can stay without a result: when the devin process
// dies while the subagent runs, `devin -r` resumes the session from the
// call's parent, and the next prompt is written as the call's sibling
// (probe, devin 3000.11.1, 2026-10-04). Such a call is abandoned, not
// running. Devin writes each assistant node twice, as siblings with one
// message_id; only a sibling with another message_id is a fork. An
// interrupted call (Esc) gets the tool result "Canceled due to user
// interrupt".

// HookRecentNodes is how many of a session's newest nodes the checks read.
const HookRecentNodes = 64

// node is the part of a chat_message the checks read.
type hookNode struct {
	Role       string `json:"role"`
	ToolCallID string `json:"tool_call_id"`
	ToolCalls  []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"tool_calls"`

	parent    sql.NullInt64 // parent_node_id
	messageID string
}

// recentNodes returns the session's newest nodes, newest first. Tool
// output can be long: only an assistant or tool node is decoded; every
// node has its parent and message_id.
func recentNodes(ctx context.Context, dbPath, session string) ([]hookNode, error) {
	db, err := openForHook(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT parent_node_id,
		  CASE WHEN json_valid(chat_message) THEN COALESCE(json_extract(chat_message, '$.message_id'), '') ELSE '' END,
		  CASE WHEN instr(chat_message, '"tool_call') > 0 THEN chat_message ELSE '' END
		FROM message_nodes WHERE session_id = ? ORDER BY row_id DESC LIMIT ?`, session, HookRecentNodes)
	if err != nil {
		return nil, fmt.Errorf("devin: recent nodes: %w", err)
	}
	defer rows.Close()
	var out []hookNode
	for rows.Next() {
		var n hookNode
		var raw string
		if err := rows.Scan(&n.parent, &n.messageID, &raw); err != nil {
			return nil, fmt.Errorf("devin: recent nodes: %w", err)
		}
		if raw != "" {
			_ = json.Unmarshal([]byte(raw), &n)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// OwnToolCall reports whether toolUseID is a tool call of one of the
// session's newest assistant nodes: the hook of that tool call runs in
// the session's own conversation, not a subagent's.
func OwnToolCall(ctx context.Context, dbPath, session, toolUseID string) (bool, error) {
	if session == "" || toolUseID == "" {
		return false, nil
	}
	nodes, err := recentNodes(ctx, dbPath, session)
	if err != nil {
		return false, err
	}
	for _, n := range nodes {
		if n.Role != "assistant" {
			continue
		}
		for _, c := range n.ToolCalls {
			if c.ID == toolUseID {
				return true, nil
			}
		}
	}
	return false, nil
}

// SubagentRunning reports whether a run_subagent call among the session's
// newest nodes has no tool result yet and was not abandoned (a newer
// sibling node with another message_id): a subagent of the session runs.
func SubagentRunning(ctx context.Context, dbPath, session string) (bool, error) {
	if session == "" {
		return false, nil
	}
	nodes, err := recentNodes(ctx, dbPath, session)
	if err != nil {
		return false, err
	}
	answered := map[string]bool{}
	for _, n := range nodes {
		if n.Role == "tool" && n.ToolCallID != "" {
			answered[n.ToolCallID] = true
		}
	}
	for i, n := range nodes {
		if n.Role != "assistant" {
			continue
		}
		for _, c := range n.ToolCalls {
			if c.Name == "run_subagent" && !answered[c.ID] && !forked(nodes[:i], n) {
				return true, nil
			}
		}
	}
	return false, nil
}

// forked reports whether one of newer (the nodes written after n) is a
// sibling of n that is not n's own copy: the conversation went on from
// n's parent without n.
func forked(newer []hookNode, n hookNode) bool {
	if !n.parent.Valid {
		return false
	}
	for _, m := range newer {
		if m.parent == n.parent && m.messageID != n.messageID {
			return true
		}
	}
	return false
}

// openForHook opens the store read-only, like openReadOnly, with a busy
// timeout a hook can afford.
func openForHook(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if _, err := fsprobe.Stat(abs); err != nil {
		return nil, fmt.Errorf("devin: %w", err)
	}
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(50)")
	q.Add("_pragma", "query_only(1)")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: abs, RawQuery: q.Encode()}).String())
	if err != nil {
		return nil, fmt.Errorf("devin: open %s: %w", abs, err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}
