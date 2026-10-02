package localindex

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/perfguard"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
)

func init() { sqliteDriver = perfguard.SQLiteDriver }

// perfIndex is a fresh index whose write connection is counted, with one
// Claude source at generation 1.
type perfIndex struct {
	s   *Store
	c   *perfguard.SQLiteCounter
	src transcript.Source
	id  int64
}

func newPerfIndex(t testing.TB) *perfIndex {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index.db")
	c := perfguard.CountSQLite(t, "file:"+path+"?") // the write connection, not the shards
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	src := transcript.Source{Agent: transcript.AgentClaude, Path: "/home/u/.claude/projects/-workspace-perf/" + perfguard.ClaudeSession{}.UUID(0) + ".jsonl",
		FileID: transcript.FileID{Dev: 1, Ino: 7}, StorageKind: transcript.StorageJSONLAppend, Parser: claude.ParserName}
	st, err := s.EnsureSource(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyBatch(context.Background(), Batch{SourceID: st.ID, NewGeneration: &transcript.Generation{Generation: 1}}); err != nil {
		t.Fatal(err)
	}
	return &perfIndex{s: s, c: c, src: src, id: st.ID}
}

// index parses data from cur to its end through a Sink, the way the agent
// indexes a transcript, and returns the cursor after it.
// gen > 1 is a rewrite: a new generation, absent rows retired at the end.
func (p *perfIndex) index(t testing.TB, data []byte, cur transcript.Cursor, gen int64) transcript.Cursor {
	t.Helper()
	ctx := context.Background()
	if gen > 1 {
		if err := p.s.StartGeneration(ctx, p.id, transcript.Generation{Generation: gen, Size: int64(len(data)), Complete: true}, "rewrite"); err != nil {
			t.Fatal(err)
		}
	}
	sink := p.s.NewSink(ctx, p.id, gen)
	sink.BatchSize = 250 // the agent's default (agent.Config.BatchRows)
	sink.SupersedeAbsent = gen > 1
	next, err := (&claude.Parser{}).Parse(ctx, transcript.Input{Source: &p.src, R: bytes.NewReader(data), Size: int64(len(data))}, cur, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Flush(&transcript.Watermark{Offset: next.Offset, LineNo: next.LineNo}, next.State); err != nil {
		t.Fatal(err)
	}
	return next
}

// Indexing a session is linear in its messages: no per-batch recount of
// everything indexed so far (perf-guards.md #7).
func TestIndexSessionScalesLinearly(t *testing.T) {
	perfguard.AssertScaling(t, perfguard.Linear, 1000, 8, func(t testing.TB, n int) perfguard.Cost {
		p := newPerfIndex(t)
		data := perfguard.ClaudeTranscript(n)
		return perfguard.MeasureSQLite(p.c, func() { p.index(t, data, transcript.Cursor{}, 1) })
	})
}

// Indexing fetches a bounded number of SQLite pages per message: one
// more index on messages adds a b-tree descent per row written. An index
// with a random key (messages_sha, on content_sha) cost +48% write CPU,
// because every row then dirties a different leaf page and each one is
// copied into the request savepoint's sub-journal; it fetched 22.53
// pages per message here, against 20.29 without it. Pages fetched are
// deterministic (cache hits plus misses), so the bound is tight: an index
// that belongs on the write path raises it, with its cost measured.
func TestIndexPagesPerMessage(t *testing.T) {
	const n = 4000
	const bound = 21.3 // 20.29 measured; a new index on messages adds ~2
	p := newPerfIndex(t)
	data := perfguard.ClaudeTranscript(n)
	c := perfguard.MeasureSQLite(p.c, func() { p.index(t, data, transcript.Cursor{}, 1) })
	if per := float64(c.SQLitePages) / n; per > bound {
		t.Errorf("indexing %d messages fetched %.2f SQLite pages per message, bound %.1f: a new index on the write path? (%s)", n, per, bound, c)
	} else {
		t.Logf("%.2f pages per message (bound %.1f)", per, bound)
	}
}

// Re-parsing a session (a rewritten file, a parser upgrade) is linear in
// its messages: every row is touched, and the digest is recounted once
// (absent rows retire at the end), not over the whole conversation per
// batch.
func TestReparseSessionScalesLinearly(t *testing.T) {
	perfguard.AssertScaling(t, perfguard.Linear, 1000, 8, func(t testing.TB, n int) perfguard.Cost {
		p := newPerfIndex(t)
		data := perfguard.ClaudeTranscript(n)
		p.index(t, data, transcript.Cursor{}, 1)
		return perfguard.MeasureSQLite(p.c, func() { p.index(t, data, transcript.Cursor{}, 2) })
	})
}

// Appending lines to a session costs the same however long the session
// is: no recount of the conversation, no rescan of its rows.
func TestAppendToSessionIsConstant(t *testing.T) {
	const added = 40
	perfguard.AssertScaling(t, perfguard.Constant, 500, 8, func(t testing.TB, n int) perfguard.Cost {
		p := newPerfIndex(t)
		s := perfguard.ClaudeSession{}
		cur := p.index(t, s.Lines(0, n), transcript.Cursor{}, 1)
		data := s.Lines(0, n+added)
		return perfguard.MeasureSQLite(p.c, func() { p.index(t, data, cur, 1) })
	})
}

// The write path's per-row and per-conversation queries use indexes
// (perf-guards.md #7): head lookups, the digest's counts, link
// resolution and supersede.
func TestApplyQueryPlans(t *testing.T) {
	p := newPerfIndex(t)
	p.index(t, perfguard.ClaudeTranscript(50), transcript.Cursor{}, 1)
	db := p.s.DB()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`SELECT ` + headCols + ` FROM messages WHERE conversation_id = ? AND native_id = ? AND part = ? AND superseded_by IS NULL`, []any{1, "x", 0}},
		{`SELECT ` + headCols + ` FROM messages WHERE source_id = ? AND ifnull(locator, byte_offset) = ? AND part = ? AND native_id IS NULL AND superseded_by IS NULL`, []any{1, 10, 0}},
		{`SELECT id FROM conversations WHERE device_id = ? AND agent = ? AND session_id = ?`, []any{"d", "claude", "s"}},
		{`SELECT agent FROM sources WHERE id = ?`, []any{1}},
		{`SELECT count(*) FROM conversations WHERE device_id = ? AND agent = ? AND parent_session_id = ? AND id <> ?`, []any{"d", "claude", "s", 1}},
		{`SELECT count(*) FROM messages WHERE conversation_id = ? AND tool_call_id = ? AND is_error = 1 AND superseded = 0 AND on_active_path IS NOT 0`, []any{1, "t"}},
		{`SELECT id FROM messages WHERE source_id = ? AND source_generation < ? AND superseded = 0`, []any{1, 2}},
		// resolveLinks, per conversation per batch.
		{resolveParentSQL, []any{1}},
		{resolveChildrenSQL, []any{1, 1}},
		{resolveSpawnSQL, []any{1, 1}},
		// refreshDigest, per conversation per batch.
		{refreshDigestSQL, []any{1}},
		{parentSubagentsSQL, []any{1}},
	} {
		perfguard.AssertSQLitePlan(t, db, nil, q.sql, q.args...)
	}
}

// readPage is what the local backend's read does for one page: find the
// focus (a session's first row, or a row by id), its neighbours, and the
// header's conversation with its message count.
func (p *perfIndex) readPage(t testing.TB, session string, id int64) int {
	t.Helper()
	ctx := context.Background()
	var focus *Row
	var err error
	if session != "" {
		focus, err = p.s.FirstMessage(ctx, session)
	} else {
		var rows []*Row
		rows, err = p.s.Messages(ctx, []int64{id})
		if len(rows) == 1 {
			focus = rows[0]
		}
	}
	if err != nil || focus == nil {
		t.Fatalf("focus: %v %v", focus, err)
	}
	if _, err := p.s.Context(ctx, focus.ID, 1, 20, Filter{}); err != nil {
		t.Fatal(err)
	}
	cs, err := p.s.ListConversations(ctx, ListOptions{Filter: Filter{Conversations: []int64{focus.ConversationID}}, IncludeDeleted: true, Limit: 1})
	if err != nil || len(cs) != 1 {
		t.Fatalf("conversation: %v %v", cs, err)
	}
	return cs[0].Messages
}

// A read of one page costs the same whatever the session's length: the
// focus (a row in the middle, or a bare session address's first row),
// its neighbours and the header's message count. The sessions are 1000
// and 8000 rows: an index-only count over messages_default reads few
// pages per row and passed at 500.
func TestReadPageIsConstant(t *testing.T) {
	for _, at := range []string{"message", "session"} {
		t.Run(at, func(t *testing.T) {
			perfguard.AssertScaling(t, perfguard.Constant, 1000, 8, func(t testing.TB, n int) perfguard.Cost {
				p := newPerfIndex(t)
				p.index(t, perfguard.ClaudeTranscript(n), transcript.Cursor{}, 1)
				var mid, live int64
				var session string
				if err := p.s.DB().QueryRow(`SELECT (SELECT id FROM messages ORDER BY ordinal LIMIT 1 OFFSET ?),
					(SELECT count(*) FROM messages WHERE superseded = 0 AND on_active_path IS NOT 0),
					(SELECT session_id FROM conversations)`, n/2).Scan(&mid, &live, &session); err != nil {
					t.Fatal(err)
				}
				if at == "message" {
					session = ""
				}
				p.readPage(t, session, mid) // open the read connection
				var got int
				cost := perfguard.MeasureSQLite(p.c, func() { got = p.readPage(t, session, mid) })
				if int64(got) != live {
					t.Fatalf("header count %d, want %d live rows", got, live)
				}
				return cost
			})
		})
	}
}

// A bare session address's first row is found through indexes. The one
// sort is over the session's conversations (one per device and agent,
// and their first rows), not its messages.
func TestFirstLivePlan(t *testing.T) {
	p := newPerfIndex(t)
	p.index(t, perfguard.ClaudeTranscript(50), transcript.Cursor{}, 1)
	plan, err := perfguard.SQLitePlan(p.s.DB(), firstLiveSQL, "s")
	if err != nil {
		t.Fatal(err)
	}
	bad := slices.DeleteFunc(perfguard.SQLiteFullScans(plan, nil), func(s string) bool { return s == "USE TEMP B-TREE FOR ORDER BY" })
	if len(bad) > 0 || !slices.ContainsFunc(plan, func(s string) bool { return strings.Contains(s, "USING INDEX messages_default") }) {
		t.Fatalf("unbounded plan %v:\n%s", bad, strings.Join(plan, "\n"))
	}
}

// The redaction and reconcile lookups use indexes: copies by text hash
// (messages_sha, which the first --all-copies redaction builds), records
// within a conversation, sessions, the marker.
func TestRedactionQueryPlans(t *testing.T) {
	p := newPerfIndex(t)
	p.index(t, perfguard.ClaudeTranscript(50), transcript.Cursor{}, 1)
	db := p.s.DB()
	if shaIndexExists(t, db) {
		t.Fatal("messages_sha exists before any redaction")
	}
	var session string
	var ordinal int64
	if err := db.QueryRow(`SELECT c.session_id, m.ordinal FROM messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE m.kind = 'user' ORDER BY m.ordinal LIMIT 1`).Scan(&session, &ordinal); err != nil {
		t.Fatal(err)
	}
	if _, err := p.s.RedactMessage(context.Background(), LocalRedaction{Session: session, Ordinal: ordinal, AllCopies: true}); err != nil {
		t.Fatal(err)
	}
	if !shaIndexExists(t, db) {
		t.Fatal("an --all-copies redaction did not build messages_sha")
	}
	sha := make([]byte, 32)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{targetByIDSQL, []any{1}},
		{targetBySHASQL, []any{sha}},
		{targetByNativeSQL, []any{1, "x"}},
		{targetNoNativeSQL, []any{1}},
		{sessionConvsSQL, []any{"s"}},
		{reconciledSQL, nil},
	} {
		perfguard.AssertSQLitePlan(t, db, nil, q.sql, q.args...)
	}
}

// Opening an index whose sidecar the rows already reflect costs the same
// however many rows it holds: the reconcile reads its marker and stops,
// without looking up a single row.
func TestOpenWithReconciledRedactionsIsConstant(t *testing.T) {
	perfguard.AssertScaling(t, perfguard.Constant, 500, 8, func(t testing.TB, n int) perfguard.Cost {
		p := newPerfIndex(t)
		p.index(t, perfguard.ClaudeTranscript(n), transcript.Cursor{}, 1)
		var session string
		var ordinal int64
		if err := p.s.DB().QueryRow(`SELECT c.session_id, m.ordinal FROM messages m JOIN conversations c ON c.id = m.conversation_id
			WHERE m.kind = 'user' ORDER BY m.ordinal LIMIT 1`).Scan(&session, &ordinal); err != nil {
			t.Fatal(err)
		}
		if _, err := p.s.RedactMessage(context.Background(), LocalRedaction{Session: session, Ordinal: ordinal, AllCopies: true}); err != nil {
			t.Fatal(err)
		}
		path := p.s.Path()
		if err := p.s.Close(); err != nil {
			t.Fatal(err)
		}
		var s *Store
		p.c.Reset()
		cost := perfguard.MeasureSQLite(p.c, func() {
			var err error
			if s, err = Open(path, Options{}); err != nil {
				t.Fatal(err)
			}
		})
		p.s = s // closed by newPerfIndex's cleanup
		for q := range p.c.BySQL() {
			if strings.Contains(q, "FROM messages") {
				t.Fatalf("open with nothing pending queried messages: %s", q)
			}
		}
		return cost
	})
}

// shaIndexExists reports whether the index has messages_sha.
func shaIndexExists(t testing.TB, db *sql.DB) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = 'messages_sha'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// An index that carries messages_sha from before it left the schema
// loses it at Open when no redaction is recorded, so its writes get
// cheaper without a reindex. With redactions recorded it is kept.
func TestOpenDropsUnusedSHAIndex(t *testing.T) {
	for _, redacted := range []bool{false, true} {
		p := newPerfIndex(t)
		p.index(t, perfguard.ClaudeTranscript(50), transcript.Cursor{}, 1)
		ctx := context.Background()
		if redacted {
			var session string
			var ordinal int64
			if err := p.s.DB().QueryRow(`SELECT c.session_id, m.ordinal FROM messages m JOIN conversations c ON c.id = m.conversation_id
				WHERE m.kind = 'user' ORDER BY m.ordinal LIMIT 1`).Scan(&session, &ordinal); err != nil {
				t.Fatal(err)
			}
			if _, err := p.s.RedactMessage(ctx, LocalRedaction{Session: session, Ordinal: ordinal}); err != nil {
				t.Fatal(err)
			}
		}
		if err := p.s.writeWait(ctx, func(w *writeTx) error { return w.ensureSHAIndex() }); err != nil {
			t.Fatal(err)
		}
		path := p.s.Path()
		if err := p.s.Close(); err != nil {
			t.Fatal(err)
		}
		s, err := Open(path, Options{})
		if err != nil {
			t.Fatal(err)
		}
		p.s = s // closed by newPerfIndex's cleanup
		if got := shaIndexExists(t, s.DB()); got != redacted {
			t.Errorf("redactions recorded %v: messages_sha exists after Open = %v", redacted, got)
		}
	}
}
