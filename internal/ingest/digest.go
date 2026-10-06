package ingest

import (
	"context"
	"errors"
	"slices"
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
	// superseded) and clears digest_stale.
	digestRecount
	// digestFold folds msgs and leaves the counts to the recount when the
	// parse completes. It sets digest_stale: a parse that dies before
	// then leaves the recount to the next parse that touches the
	// conversation (complete).
	digestFold
)

// refreshDigest folds msgs, the rows a flush wrote for conversation conv,
// into its stored digest and updates its parent's subagent count, in the
// flush's transaction. A conversation without a digest yet is recounted.
// last is the flush's harness activity of conv (zero for none). The
// newest written message (all stored messages on recount) also advances
// activity. The stored timestamp never decreases, and the digest and
// activity row use the same microsecond timestamp in one write per flush.
func refreshDigest(ctx context.Context, tx pgx.Tx, conv string, msgs []*transcript.Message, mode digestMode, last time.Time) error {
	var (
		prev          []byte
		cwd, root     *string
		title         *string
		branches      []string
		start, stored *time.Time
		remote, agent string
	)
	if err := tx.QueryRow(ctx, `SELECT a.digest,c.cwd,c.repo_root,c.branches,c.started_at,a.last_activity_at,COALESCE(c.extra->'git'->>'repository_url',''),c.agent,c.title
		FROM conversations c JOIN conversation_activity a ON a.conversation_id=c.id WHERE c.id=$1`, conv).Scan(&prev, &cwd, &root, &branches, &start, &stored, &remote, &agent, &title); err != nil {
		return err
	}
	c := digest.Conv{Title: deref(title), Cwd: deref(cwd), RepoRoot: deref(root), Remote: normalizeRemote(remote), Branches: branches}
	if start != nil {
		c.Started = *start
	}
	if prev == nil {
		mode = digestRecount
	}
	for _, m := range msgs {
		if m.TS.After(last) {
			last = m.TS
		}
	}
	if mode == digestRecount {
		// Activity includes every ingested row, even superseded versions
		// and rows outside the active path; only digest counts filter them.
		var newest *time.Time
		if err := tx.QueryRow(ctx, `SELECT max(ts) FROM messages WHERE conversation_id=$1`, conv).Scan(&newest); err != nil {
			return err
		}
		if newest != nil && newest.After(last) {
			last = *newest
		}
	}
	// timestamptz keeps microseconds, as pgx sends them (truncated).
	last = last.Truncate(time.Microsecond)
	if stored != nil && stored.After(last) {
		last = *stored
	}
	c.Last = last
	var subagents int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM conversations k JOIN conversations c ON c.id=$1
		WHERE k.device_id=c.device_id AND k.agent=c.agent AND k.parent_native_session_id=c.session_id AND k.id<>c.id`, conv).Scan(&subagents); err != nil {
		return err
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
	// The caller holds conv's conversations row (the flush's upsert, or
	// recountDigests' lock), which covers its activity row.
	if _, err := tx.Exec(ctx, `UPDATE conversation_activity SET digest=$2,digest_stale=CASE $3::int WHEN 1 THEN false WHEN 2 THEN true ELSE digest_stale END,
			last_activity_at=GREATEST(last_activity_at,$4::timestamptz) WHERE conversation_id=$1`,
		conv, out, int(mode), nullTime(last)); err != nil {
		return err
	}
	// A subagent changes its parent's count. The parent's conversations
	// row is locked first, as an update of it would lock it, and then its
	// activity row. The caller locked it already, in session order with
	// conv (lockWithParents, or the flush's lockFlushSQL), so this lock
	// does not wait.
	_, err := tx.Exec(ctx, `UPDATE conversation_activity a SET digest=jsonb_set(a.digest,'{subagents}',to_jsonb((SELECT count(*) FROM conversations k
			WHERE k.device_id=p.device_id AND k.agent=p.agent AND k.parent_native_session_id=p.session_id AND k.id<>p.id)))
		FROM (SELECT p.id,p.device_id,p.agent,p.session_id FROM conversations p JOIN conversations c ON c.id=$1
			WHERE p.device_id=c.device_id AND p.agent=c.agent AND p.session_id=c.parent_native_session_id AND p.id<>c.id
			FOR NO KEY UPDATE OF p) p
		WHERE a.conversation_id=p.id AND a.digest IS NOT NULL`, conv)
	return err
}

// recountDigests recounts the digests of conversations ids from their live
// rows. A conversation deleted meanwhile is skipped.
//
// Two sources can write one conversation at once (a file replaced at the
// same path keeps its session, and the old source's refresh runs beside
// the new one's live parse under a different source fence), so the
// recount first locks the rows: a flush holds its conversation's row from
// its upsert to its commit, and a count taken without the lock could
// overwrite a digest that flush commits meanwhile. A subagent's refresh
// also updates its parent's count, so the parents are locked with them
// (lockWithParents).
func recountDigests(ctx context.Context, tx pgx.Tx, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	locked, err := lockWithParents(ctx, tx, ids)
	if err != nil {
		return err
	}
	for _, id := range locked {
		if !slices.Contains(ids, id) {
			continue // a parent, locked only for its subagent count
		}
		if err := refreshDigest(ctx, tx, id, nil, digestRecount, time.Time{}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	return nil
}

// parentsSQL selects, as p, the parent conversation of each subagent k:
// the conversation of k's parent native session on k's device, as
// refreshDigest and resolveLinks find it.
const parentsSQL = `SELECT p.id FROM conversations k JOIN conversations p ON p.device_id=k.device_id AND p.agent=k.agent
	AND p.session_id=k.parent_native_session_id AND p.id<>k.id WHERE k.id=ANY($1::uuid[])`

// lockWithParentsSQL locks conversations $1 and their parents in the order
// of store.LockConversationsSQL. It returns each locked row's natural key
// and parent session as locked (a row updated while the lock waited is
// returned as updated).
const lockWithParentsSQL = `SELECT id::text,device_id::text,agent,session_id,COALESCE(parent_native_session_id,'') FROM conversations
	WHERE id IN (SELECT unnest($1::uuid[]) UNION ` + parentsSQL + `)
	ORDER BY session_id COLLATE "C",id FOR UPDATE`

// lockWithParents locks conversations ids and the parent of each subagent
// among them, all in store.LockConversationsSQL's order, and returns the
// locked ids in that order. A writer that updates a subagent and then its
// parent (the parent's subagent count, the subagent's link) must hold the
// parent in that order too: a hide or deletion of the parent's tree locks
// the parent and its subagents in it, and a parent locked after its
// subagents deadlocks with it.
//
// A subagent's parent session can be set between the statement's snapshot
// and its lock (its first flush, which holds the subagent's row): the
// parent is then missing from the locked set. The locks are then dropped
// (rolled back to a savepoint) and taken again, up to three times.
func lockWithParents(ctx context.Context, tx pgx.Tx, ids []string) ([]string, error) {
	type key struct{ device, agent, session string }
	for attempt := 0; ; attempt++ {
		sp, err := tx.Begin(ctx)
		if err != nil {
			return nil, err
		}
		rows, err := sp.Query(ctx, lockWithParentsSQL, ids)
		if err != nil {
			return nil, err
		}
		var locked []string
		have := map[key]bool{}
		var want []key
		var row struct {
			id string
			k  key
			p  string
		}
		_, err = pgx.ForEachRow(rows, []any{&row.id, &row.k.device, &row.k.agent, &row.k.session, &row.p}, func() error {
			locked = append(locked, row.id)
			have[row.k] = true
			if row.p != "" && row.p != row.k.session && slices.Contains(ids, row.id) {
				want = append(want, key{row.k.device, row.k.agent, row.p})
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		// A parent session without a locked conversation is usually one
		// not stored yet; only then is the database asked.
		missing := false
		if slices.ContainsFunc(want, func(k key) bool { return !have[k] }) {
			if err := sp.QueryRow(ctx, `SELECT EXISTS(`+parentsSQL+` AND NOT p.id=ANY($2::uuid[]))`, ids, locked).Scan(&missing); err != nil {
				return nil, err
			}
		}
		if !missing || attempt == 3 {
			return locked, sp.Commit(ctx)
		}
		if err := sp.Rollback(ctx); err != nil {
			return nil, err
		}
	}
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
	if len(calls) == 0 {
		return n, nil
	}
	ids := make([]string, 0, len(calls))
	for id := range calls {
		ids = append(ids, id)
	}
	rows, err := tx.Query(ctx, `SELECT tool_call_id,count(*) FROM messages
		WHERE conversation_id=$1 AND tool_call_id=ANY($2::text[]) AND is_error AND NOT superseded AND on_active_path IS NOT FALSE
		GROUP BY tool_call_id`, conv, ids)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var all int
		if err := rows.Scan(&id, &all); err != nil {
			return 0, err
		}
		if all == calls[id] {
			n++
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return n, nil
}
