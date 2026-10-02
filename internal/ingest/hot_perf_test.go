package ingest

import (
	"testing"

	"github.com/flopwire/flopwire/internal/perfguard"
)

// conversationIndexBytes is the size of each index on conversations.
func conversationIndexBytes(t testing.TB, e *env) map[string]int64 {
	t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT c.relname::text,pg_relation_size(c.oid) FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
		WHERE i.indrelid='conversations'::regclass`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var name string
		var n int64
		if err := rows.Scan(&name, &n); err != nil {
			t.Fatal(err)
		}
		out[name] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// A flush writes a conversation's hot fields (last activity, digest,
// digest_stale) to conversation_activity, not to the conversations row:
// appends that change none of its cold columns (title, cwd, repo, session
// id, hide state) leave the row, and so its indexes, untouched. Each
// update of a conversations row is non-HOT whenever an indexed column
// changes, and adds entries to every index, the three pg_trgm GIN
// indexes included, which never shrink.
func TestPerfFlushLeavesConversationRow(t *testing.T) {
	const appends = 24
	e, counter := perfEnv(t)
	a := newAppendSession(t, e, 64)
	a.records(t, appendBatch) // warm: the first append after a full parse
	before := conversationIndexBytes(t, e)
	cost := perfguard.Measure(t, e.pool, counter, func() {
		for range appends {
			a.records(t, appendBatch)
		}
	})
	after := conversationIndexBytes(t, e)
	sameDigest(t, e, a.session.SessionID)

	if upd := cost.Tables["public.conversations"].TupUpd; upd != 0 {
		t.Errorf("%d appends updated conversations rows %d times, want 0: a flush that changes no cold column rewrites the row", appends, upd)
	}
	for name, b := range before {
		if after[name] != b {
			t.Errorf("index %s grew from %d to %d bytes over %d appends", name, b, after[name], appends)
		}
	}
	// One write of the activity row per append: the digest and the last
	// activity go together.
	if upd := cost.Tables["public.conversation_activity"].TupUpd; upd > appends {
		t.Errorf("%d appends updated conversation_activity %d times, want at most one each", appends, upd)
	}
	var upd, hot int64
	if err := e.pool.QueryRow(e.ctx, `SELECT COALESCE(sum(n_tup_upd),0)::bigint,COALESCE(sum(n_tup_hot_upd),0)::bigint
		FROM pg_stat_user_tables WHERE relname='conversation_activity'`).Scan(&upd, &hot); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d appends: conversations %s", appends, cost.Tables["public.conversations"])
	t.Logf("conversation_activity: %d updates, %d HOT (cumulative)", upd, hot)
	for _, name := range []string{"conversations_session_trgm_idx", "conversations_title_trgm_idx", "conversations_place_trgm_idx"} {
		t.Logf("%s: %d -> %d bytes", name, before[name], after[name])
	}
}
