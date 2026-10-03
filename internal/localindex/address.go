package localindex

import (
	"context"
	"database/sql"
	"errors"
)

// Lookups for retrieval addresses (SESSION/ORDINAL[:LINE], path:line).

// SessionsWithPrefix returns up to limit distinct session ids starting
// with prefix, in order.
func (s *Store) SessionsWithPrefix(ctx context.Context, prefix string, limit int) ([]string, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT DISTINCT session_id FROM conversations INDEXED BY conversations_session
		WHERE session_id >= ? AND session_id < ? ORDER BY session_id LIMIT ?`, prefix, prefix+"\xff", limit)
	if err != nil {
		return nil, err
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

// SessionNeighbours returns, for each id, the session ids just before
// and after it in byte order: the ones sharing its longest prefix, which
// decide its shortest unique prefix.
func (s *Store) SessionNeighbours(ctx context.Context, ids []string) (map[string][]string, error) {
	out := make(map[string][]string, len(ids))
	for _, id := range ids {
		if _, done := out[id]; done {
			continue
		}
		var nb []string
		for _, q := range []string{
			`SELECT session_id FROM conversations INDEXED BY conversations_session WHERE session_id < ? ORDER BY session_id DESC LIMIT 1`,
			`SELECT session_id FROM conversations INDEXED BY conversations_session WHERE session_id > ? ORDER BY session_id LIMIT 1`,
		} {
			var x string
			err := s.rdb.QueryRowContext(ctx, q, id).Scan(&x)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return nil, err
			}
			nb = append(nb, x)
		}
		out[id] = nb
	}
	return out, nil
}

// MessageByOrdinal returns the row of session sessionID at ordinal,
// preferring a live row on the active path and then the newest version;
// nil when there is none.
func (s *Store) MessageByOrdinal(ctx context.Context, sessionID string, ordinal int64) (*Row, error) {
	var r *Row
	err := s.stream(ctx, `SELECT `+rowCols+rowFrom+` WHERE m.conversation_id IN (SELECT id FROM conversations WHERE session_id = ?) AND m.ordinal = ?
		ORDER BY m.superseded, m.on_active_path IS 0, m.version DESC, m.id DESC LIMIT 1`,
		[]any{sessionID, ordinal}, func(x *Row) bool { r = x; return false })
	return r, err
}

// FirstMessage returns the first row of a session under the default view
// (live, on the active path), or its first row of any kind when it has
// none; nil when the session has no rows.
func (s *Store) FirstMessage(ctx context.Context, sessionID string) (*Row, error) {
	var r *Row
	err := s.stream(ctx, firstLiveSQL, []any{sessionID}, func(x *Row) bool { r = x; return false })
	if err != nil || r != nil {
		return r, err
	}
	err = s.stream(ctx, `SELECT `+rowCols+rowFrom+` WHERE m.conversation_id IN (SELECT id FROM conversations WHERE session_id = ?)
		ORDER BY m.superseded, m.on_active_path IS 0, c.depth, m.ordinal, m.id LIMIT 1`,
		[]any{sessionID}, func(x *Row) bool { r = x; return false })
	return r, err
}

// LeadingMessages returns up to n live rows of a session's top-level
// conversation of agent, in ordinal order: where its title was taken from.
func (s *Store) LeadingMessages(ctx context.Context, sessionID, agent string, n int) ([]*Row, error) {
	var out []*Row
	err := s.stream(ctx, `SELECT `+rowCols+rowFrom+` WHERE c.session_id = ? AND c.agent = ? AND c.depth = 0 AND c.deleted_in_generation IS NULL
		AND m.superseded = 0 ORDER BY m.ordinal, m.id LIMIT ?`, []any{sessionID, agent, n}, func(x *Row) bool { out = append(out, x); return true })
	return out, err
}

// firstLiveSQL selects the first live row on the active path of a
// session: the shallowest conversation's lowest ordinal, each
// conversation's first row read from messages_default in ordinal order,
// so the session's length does not matter. FirstMessage falls back to
// the full order over all rows when there is none.
var firstLiveSQL = `SELECT ` + rowCols + rowFrom + ` WHERE m.id = (SELECT f.id FROM (SELECT k.depth AS depth,
		  (SELECT id FROM messages WHERE conversation_id = k.id AND superseded = 0 AND on_active_path IS NOT 0 ORDER BY ordinal, id LIMIT 1) AS fid
		FROM conversations k WHERE k.session_id = ?) x JOIN messages f ON f.id = x.fid ORDER BY x.depth, f.ordinal, f.id LIMIT 1)`

// MessagesAtLine returns the rows recorded at a transcript line (a JSONL
// line can hold several), live rows first, in ordinal order.
func (s *Store) MessagesAtLine(ctx context.Context, path string, lineNo int64) ([]*Row, error) {
	var out []*Row
	err := s.stream(ctx, `SELECT `+rowCols+rowFrom+` WHERE m.source_id IN (SELECT id FROM sources WHERE path = ?) AND m.line_no = ?
		ORDER BY m.superseded, m.ordinal, m.id`, []any{path, lineNo}, func(x *Row) bool { out = append(out, x); return true })
	return out, err
}

// SubstringQuery is the trigram query for a substring of at least three
// characters; shorter substrings get a query with no constraint.
func (s *Store) SubstringQuery(pattern string) *TrigramQuery {
	if runeLen(pattern) < 3 {
		return &TrigramQuery{Op: TrigramAll}
	}
	return &TrigramQuery{Op: TrigramAnd, expr: s.substringExpr(pattern)}
}
