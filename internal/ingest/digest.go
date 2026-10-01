package ingest

import (
	"context"
	"errors"
	"time"

	"github.com/flopwire/flopwire/internal/digest"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/jackc/pgx/v5"
)

// digestMode is how refreshDigest treats the stored counts.
type digestMode int

const (
	// digestAppend adds the counts of msgs, rows a flush only added.
	digestAppend digestMode = iota
	// digestRecount recounts over the live rows (rows replaced or
	// superseded).
	digestRecount
	// digestFold folds msgs and leaves the counts to the recount when the
	// parse completes.
	digestFold
)

// refreshDigest folds msgs, the rows a flush wrote for conversation conv,
// into its stored digest and updates its parent's subagent count, in the
// flush's transaction. A conversation without a digest yet is recounted.
func refreshDigest(ctx context.Context, tx pgx.Tx, conv string, msgs []*transcript.Message, mode digestMode) error {
	var (
		prev          []byte
		cwd, root     *string
		title         *string
		branches      []string
		start, last   *time.Time
		remote, agent string
	)
	if err := tx.QueryRow(ctx, `SELECT digest,cwd,repo_root,branches,started_at,last_activity_at,COALESCE(extra->'git'->>'repository_url',''),agent,title
		FROM conversations WHERE id=$1`, conv).Scan(&prev, &cwd, &root, &branches, &start, &last, &remote, &agent, &title); err != nil {
		return err
	}
	c := digest.Conv{Title: deref(title), Cwd: deref(cwd), RepoRoot: deref(root), Remote: normalizeRemote(remote), Branches: branches}
	if start != nil {
		c.Started = *start
	}
	if last != nil {
		c.Last = *last
	}
	var subagents int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM conversations k JOIN conversations c ON c.id=$1
		WHERE k.device_id=c.device_id AND k.agent=c.agent AND k.parent_native_session_id=c.session_id AND k.id<>c.id`, conv).Scan(&subagents); err != nil {
		return err
	}
	if prev == nil {
		mode = digestRecount
	}
	var out []byte
	switch mode {
	case digestFold:
		out = digest.Fold(prev, c, msgs)
	case digestRecount:
		n, err := digestCounts(ctx, tx, conv)
		if err != nil {
			return err
		}
		n.Subagents = subagents
		out = digest.Update(prev, c, msgs, n)
	default:
		failed, err := newFailed(ctx, tx, conv, msgs)
		if err != nil {
			return err
		}
		out = digest.Append(prev, c, msgs, failed, subagents)
	}
	if _, err := tx.Exec(ctx, `UPDATE conversations SET digest=$2 WHERE id=$1`, conv, out); err != nil {
		return err
	}
	// A subagent changes its parent's count.
	_, err := tx.Exec(ctx, `UPDATE conversations p SET digest=jsonb_set(p.digest,'{subagents}',to_jsonb((SELECT count(*) FROM conversations k
			WHERE k.device_id=p.device_id AND k.agent=p.agent AND k.parent_native_session_id=p.session_id AND k.id<>p.id)))
		FROM conversations c WHERE c.id=$1 AND p.device_id=c.device_id AND p.agent=c.agent AND p.session_id=c.parent_native_session_id
			AND p.id<>c.id AND p.digest IS NOT NULL`, conv)
	return err
}

// recountDigests recounts the digests of conversations ids (sorted) from
// their live rows. A conversation deleted meanwhile is skipped.
func recountDigests(ctx context.Context, tx pgx.Tx, ids []string) error {
	for _, id := range ids {
		if err := refreshDigest(ctx, tx, id, nil, digestRecount); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	return nil
}

// digestCounts counts a conversation's live rows for its digest.
func digestCounts(ctx context.Context, tx pgx.Tx, conv string) (digest.Counts, error) {
	n := digest.Counts{Messages: map[string]int{}, Tools: map[string]int{}}
	rows, err := tx.Query(ctx, `SELECT kind,COALESCE(tool_name,''),count(*) FROM messages
		WHERE conversation_id=$1 AND NOT superseded AND on_active_path IS NOT FALSE GROUP BY 1,2`, conv)
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
	if err := tx.QueryRow(ctx, `SELECT count(DISTINCT COALESCE(tool_call_id,id::text)) FROM messages
		WHERE conversation_id=$1 AND NOT superseded AND on_active_path IS NOT FALSE AND is_error`, conv).Scan(&n.Failed); err != nil {
		return n, err
	}
	// Claude repeats an API message's usage on each of its lines; the line
	// with the most output tokens holds the final count. The message
	// written last is where an append continues (Counts.Usage).
	rows, err = tx.Query(ctx, `SELECT COALESCE(mid,''),COALESCE(i,0),COALESCE(o,0),COALESCE(cr,0),COALESCE(cc,0) FROM (
			SELECT DISTINCT ON (COALESCE(enrichment->>'message_id',id::text)) enrichment->>'message_id' AS mid,
				(enrichment->'usage'->>'input_tokens')::bigint AS i,(enrichment->'usage'->>'output_tokens')::bigint AS o,
				(enrichment->'usage'->>'cache_read_input_tokens')::bigint AS cr,(enrichment->'usage'->>'cache_creation_input_tokens')::bigint AS cc,
				max(ordinal) OVER (PARTITION BY COALESCE(enrichment->>'message_id',id::text)) AS last
			FROM messages WHERE conversation_id=$1 AND NOT superseded AND enrichment ? 'usage'
			ORDER BY COALESCE(enrichment->>'message_id',id::text),(enrichment->'usage'->>'output_tokens')::bigint DESC NULLS LAST) u
		ORDER BY last`, conv)
	if err != nil {
		return n, err
	}
	defer rows.Close()
	for rows.Next() {
		var mark digest.UsageMark
		t := &mark.Tokens
		if err := rows.Scan(&mark.Msg, &t.Input, &t.Output, &t.CacheRead, &t.CacheCreation); err != nil {
			return n, err
		}
		n.Tokens.Input, n.Tokens.Output = n.Tokens.Input+t.Input, n.Tokens.Output+t.Output
		n.Tokens.CacheRead, n.Tokens.CacheCreation = n.Tokens.CacheRead+t.CacheRead, n.Tokens.CacheCreation+t.CacheCreation
		n.Usage = &mark
	}
	return n, rows.Err()
}

// newFailed counts the failed tool calls that msgs, rows just appended,
// add: each failed row without a call id, and each call id whose live
// failed rows are all among msgs.
func newFailed(ctx context.Context, tx pgx.Tx, conv string, msgs []*transcript.Message) (int, error) {
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
		var all int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM messages
			WHERE conversation_id=$1 AND tool_call_id=$2 AND is_error AND NOT superseded AND on_active_path IS NOT FALSE`, conv, id).Scan(&all); err != nil {
			return 0, err
		}
		if all == k {
			n++
		}
	}
	return n, nil
}
