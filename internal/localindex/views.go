package localindex

import (
	"context"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

// Read helpers for the retrieval verbs (PR A3): provenance, whole
// conversations, subagent children, and the lookups self-session exclusion
// needs.

// MatchExpr renders q as an fts_tri MATCH expression; all is true for a
// query with no constraint, and expr is "" with all false for one that
// matches nothing.
func (q *TrigramQuery) MatchExpr() (expr string, all bool) { return q.matchExpr() }

// RowSource is where a message row came from.
type RowSource struct {
	SourceID   int64
	Generation int64 // the source generation the row was last seen in
}

// RowSources returns the source and generation of each message id.
func (s *Store) RowSources(ctx context.Context, ids []int64) (map[int64]RowSource, error) {
	out := make(map[int64]RowSource, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, source_id, source_generation FROM messages WHERE id IN (SELECT value FROM json_each(?))`, int64JSON(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var rs RowSource
		if err := rows.Scan(&id, &rs.SourceID, &rs.Generation); err != nil {
			return nil, err
		}
		out[id] = rs
	}
	return out, rows.Err()
}

// ConversationMessages returns a conversation's rows in order (ordinal, id),
// under the visibility flags of f (superseded, branches, kinds, time), at
// most limit rows; more reports whether rows were left out.
func (s *Store) ConversationMessages(ctx context.Context, convID int64, f Filter, limit int) (rows []*Row, more bool, err error) {
	vis := Filter{IncludeSuperseded: f.IncludeSuperseded, IncludeBranches: f.IncludeBranches, Kinds: f.Kinds, Since: f.Since, Until: f.Until}
	where, args := vis.where()
	q := `SELECT ` + rowCols + rowFrom + ` WHERE m.conversation_id = ? AND ` + where + ` ORDER BY m.ordinal, m.id LIMIT ?`
	err = s.stream(ctx, q, append(append([]any{convID}, args...), limit+1), func(r *Row) bool {
		rows = append(rows, r)
		return true
	})
	if len(rows) > limit {
		rows, more = rows[:limit], true
	}
	return rows, more, err
}

// ChildConversationIDs returns the ids of conversations whose parent is
// convID, oldest first.
func (s *Store) ChildConversationIDs(ctx context.Context, convID int64) ([]int64, error) {
	return s.ids(ctx, `SELECT id FROM conversations WHERE parent_conversation_id = ? ORDER BY started_at, id`, convID)
}

// SessionConversationIDs returns the conversation of (agent, sessionID) on
// this device and every conversation below it (subagents, recursively, by
// resolved parent id or by unresolved native parent session id). An empty
// agent matches any.
func (s *Store) SessionConversationIDs(ctx context.Context, agent transcript.Agent, sessionID string) ([]int64, error) {
	return s.ids(ctx, `WITH RECURSIVE tree(id, agent, session_id) AS (
		  SELECT id, agent, session_id FROM conversations WHERE session_id = ? AND (? = '' OR agent = ?)
		  UNION
		  SELECT c.id, c.agent, c.session_id FROM conversations c JOIN tree t
		    ON c.parent_conversation_id = t.id OR (c.parent_conversation_id IS NULL AND c.agent = t.agent AND c.parent_session_id = t.session_id)
		) SELECT id FROM tree ORDER BY id`, sessionID, string(agent), string(agent))
}

// SessionActiveSince reports whether a conversation of sessionID was
// active at or after t.
func (s *Store) SessionActiveSince(ctx context.Context, sessionID string, t time.Time) (bool, error) {
	var ok bool
	err := s.rdb.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM conversations WHERE session_id = ? AND last_activity_at >= ?)`,
		sessionID, t.UnixMilli()).Scan(&ok)
	return ok, err
}

func (s *Store) ids(ctx context.Context, q string, args ...any) ([]int64, error) {
	rows, err := s.rdb.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
