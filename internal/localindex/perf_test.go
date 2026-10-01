package localindex

import (
	"bytes"
	"context"
	"path/filepath"
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
