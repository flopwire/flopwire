package localindex

import (
	"database/sql"
	"encoding/json"
	"time"

	"github.com/flopwire/flopwire/internal/digest"
	"github.com/flopwire/flopwire/internal/transcript"
)

// refreshDigest folds msgs, the rows a batch wrote for the conversation,
// into its stored digest and updates its parent's subagent count. When
// the batch only added rows (appended), their counts are added to the
// stored ones; otherwise (full: a row replaced, rows superseded, no
// digest yet) the aggregates are recounted over the live rows.
func (w *writeTx) refreshDigest(convID int64, msgs []*transcript.Message, full bool) error {
	var (
		prev, cwd, root, branches, remote, title sql.NullString
		start, last                              sql.NullInt64
		agent, session, device                   string
	)
	row, err := w.queryRow(`SELECT c.digest, c.cwd, c.repo_root, c.branches, c.started_at, c.last_activity_at, c.agent, c.session_id, c.device_id, c.title,
		  (SELECT p.remote FROM placements p WHERE p.agent = c.agent AND p.session_id = c.session_id)
		FROM conversations c WHERE c.id = ?`, convID)
	if err != nil {
		return err
	}
	if err := row.Scan(&prev, &cwd, &root, &branches, &start, &last, &agent, &session, &device, &title, &remote); err != nil {
		return err
	}
	c := digest.Conv{Title: title.String, Cwd: cwd.String, RepoRoot: root.String, Remote: remote.String}
	if branches.Valid {
		_ = json.Unmarshal([]byte(branches.String), &c.Branches)
	}
	if start.Valid {
		c.Started = time.UnixMilli(start.Int64)
	}
	if last.Valid {
		c.Last = time.UnixMilli(last.Int64)
	}
	var out []byte
	if full || !prev.Valid {
		n, err := w.digestCounts(convID, agent, session, device)
		if err != nil {
			return err
		}
		out = digest.Update([]byte(prev.String), c, msgs, n)
	} else {
		failed, err := w.newFailed(convID, msgs)
		if err != nil {
			return err
		}
		subs, err := w.subagentCount(convID, agent, session, device)
		if err != nil {
			return err
		}
		out = digest.Append([]byte(prev.String), c, msgs, failed, subs)
	}
	if _, err := w.exec(`UPDATE conversations SET digest = ? WHERE id = ?`, string(out), convID); err != nil {
		return err
	}
	// A subagent changes its parent's count.
	_, err = w.exec(`UPDATE conversations SET digest = json_set(digest, '$.subagents',
		  (SELECT count(*) FROM conversations k WHERE k.device_id = conversations.device_id AND k.agent = conversations.agent
		     AND k.parent_session_id = conversations.session_id AND k.id <> conversations.id))
		WHERE digest IS NOT NULL AND id = (SELECT parent_conversation_id FROM conversations WHERE id = ?)`, convID)
	return err
}

// digestCounts counts a conversation's live rows for its digest.
func (w *writeTx) digestCounts(convID int64, agent, session, device string) (digest.Counts, error) {
	n := digest.Counts{Messages: map[string]int{}, Tools: map[string]int{}}
	rows, err := w.tx.QueryContext(w.ctx, `SELECT kind, ifnull(tool_name, ''), count(*) FROM messages
		WHERE conversation_id = ? AND superseded = 0 AND on_active_path IS NOT 0 GROUP BY 1, 2`, convID)
	if err != nil {
		return n, err
	}
	for rows.Next() {
		var kind, tool string
		var k int
		if err := rows.Scan(&kind, &tool, &k); err != nil {
			rows.Close()
			return n, err
		}
		n.Messages[kind] += k
		if kind == transcript.KindToolCall.String() && tool != "" {
			n.Tools[tool] += k
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return n, err
	}
	row, err := w.queryRow(`SELECT count(DISTINCT ifnull(tool_call_id, id)) FROM messages
		WHERE conversation_id = ? AND superseded = 0 AND on_active_path IS NOT 0 AND is_error = 1`, convID)
	if err != nil {
		return n, err
	}
	if err := row.Scan(&n.Failed); err != nil {
		return n, err
	}
	if n.Subagents, err = w.subagentCount(convID, agent, session, device); err != nil {
		return n, err
	}
	// Claude repeats an API message's usage on each of its lines; the line
	// with the most output tokens holds the final count. The message
	// written last is where an append continues (Counts.Usage).
	rows, err = w.tx.QueryContext(w.ctx, `SELECT ifnull(mid, ''), ifnull(i, 0), ifnull(o, 0), ifnull(cr, 0), ifnull(cc, 0), last FROM (
		  SELECT json_extract(enrichment, '$.message_id') AS mid,
		    json_extract(enrichment, '$.usage.input_tokens') AS i, json_extract(enrichment, '$.usage.output_tokens') AS o,
		    json_extract(enrichment, '$.usage.cache_read_input_tokens') AS cr, json_extract(enrichment, '$.usage.cache_creation_input_tokens') AS cc,
		    row_number() OVER (PARTITION BY ifnull(json_extract(enrichment, '$.message_id'), id)
		      ORDER BY json_extract(enrichment, '$.usage.output_tokens') DESC) AS rn,
		    max(id) OVER (PARTITION BY ifnull(json_extract(enrichment, '$.message_id'), id)) AS last
		  FROM messages WHERE conversation_id = ? AND superseded = 0 AND enrichment LIKE '%"usage"%'
		) WHERE rn = 1`, convID)
	if err != nil {
		return n, err
	}
	defer rows.Close()
	var lastID int64
	for rows.Next() {
		var mark digest.UsageMark
		var last int64
		t := &mark.Tokens
		if err := rows.Scan(&mark.Msg, &t.Input, &t.Output, &t.CacheRead, &t.CacheCreation, &last); err != nil {
			return n, err
		}
		n.Tokens.Input, n.Tokens.Output = n.Tokens.Input+t.Input, n.Tokens.Output+t.Output
		n.Tokens.CacheRead, n.Tokens.CacheCreation = n.Tokens.CacheRead+t.CacheRead, n.Tokens.CacheCreation+t.CacheCreation
		if last > lastID {
			lastID, n.Usage = last, &mark
		}
	}
	return n, rows.Err()
}

// subagentCount counts the conversations naming this one as parent.
func (w *writeTx) subagentCount(convID int64, agent, session, device string) (int, error) {
	row, err := w.queryRow(`SELECT count(*) FROM conversations WHERE device_id = ? AND agent = ? AND parent_session_id = ? AND id <> ?`,
		device, agent, session, convID)
	if err != nil {
		return 0, err
	}
	var n int
	return n, row.Scan(&n)
}

// newFailed counts the failed tool calls that msgs, rows just appended,
// add: each failed row without a call id, and each call id whose live
// failed rows are all among msgs.
func (w *writeTx) newFailed(convID int64, msgs []*transcript.Message) (int, error) {
	n := 0
	calls := map[string]int{}
	for _, m := range msgs {
		if !m.IsError || m.Superseded || m.OnActivePath != nil && !*m.OnActivePath {
			continue
		}
		if m.ToolCallID == "" {
			n++
		} else {
			calls[m.ToolCallID]++
		}
	}
	for id, k := range calls {
		row, err := w.queryRow(`SELECT count(*) FROM messages
			WHERE conversation_id = ? AND tool_call_id = ? AND is_error = 1 AND superseded = 0 AND on_active_path IS NOT 0`, convID, id)
		if err != nil {
			return 0, err
		}
		var all int
		if err := row.Scan(&all); err != nil {
			return 0, err
		}
		if all == k {
			n++
		}
	}
	return n, nil
}
