package localindex

import (
	"context"
	"database/sql"
)

// Outline is a conversation's skeleton for read --outline: its user
// prompts and tool calls in order (live rows on the active path), the
// tool calls that failed, and the subagents each call spawned.
type Outline struct {
	Rows []*Row
	// Failed holds the tool call ids whose call or result is marked
	// failed.
	Failed map[string]bool
	// Spawned maps a tool call row id to the session ids of the subagent
	// conversations it spawned.
	Spawned map[int64][]string
}

// outlineWhere selects the outline rows of conversation ?.
const outlineWhere = ` WHERE m.conversation_id = ? AND m.superseded = 0 AND m.on_active_path IS NOT 0 AND m.kind IN ('user', 'tool_call')`

// OutlineKey is an outline row's place in the outline's order.
type OutlineKey struct {
	Ordinal int64
	ID      int64
}

// Outline returns up to limit rows of a conversation's outline, after
// the row after when it is set.
func (s *Store) Outline(ctx context.Context, convID int64, after *OutlineKey, limit int) (*Outline, error) {
	out := &Outline{Failed: map[string]bool{}, Spawned: map[int64][]string{}}
	where, args := outlineWhere, []any{convID}
	if after != nil {
		where += ` AND (m.ordinal > ? OR m.ordinal = ? AND m.id > ?)`
		args = append(args, after.Ordinal, after.Ordinal, after.ID)
	}
	err := s.stream(ctx, `SELECT `+rowCols+rowFrom+where+` ORDER BY m.ordinal, m.id LIMIT ?`,
		append(args, limit), func(r *Row) bool {
			out.Rows = append(out.Rows, r)
			return true
		})
	if err != nil {
		return nil, err
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT DISTINCT tool_call_id FROM messages WHERE conversation_id = ? AND superseded = 0
		AND is_error = 1 AND tool_call_id IS NOT NULL`, convID)
	if err != nil {
		return nil, err
	}
	if err := eachRow(rows, func(sc scanner) error {
		var id string
		err := sc.Scan(&id)
		out.Failed[id] = true
		return err
	}); err != nil {
		return nil, err
	}
	rows, err = s.rdb.QueryContext(ctx, `SELECT spawned_by_message_id, session_id FROM conversations
		WHERE parent_conversation_id = ? AND spawned_by_message_id IS NOT NULL ORDER BY started_at, id`, convID)
	if err != nil {
		return nil, err
	}
	err = eachRow(rows, func(sc scanner) error {
		var msg int64
		var sid string
		err := sc.Scan(&msg, &sid)
		out.Spawned[msg] = append(out.Spawned[msg], sid)
		return err
	})
	return out, err
}

func eachRow(rows *sql.Rows, fn func(scanner) error) error {
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
