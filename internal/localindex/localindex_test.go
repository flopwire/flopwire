package localindex

import (
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

var ctx = context.Background()

func openTest(t *testing.T, detail Detail) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "index.db"), Options{TokDetail: detail, TriDetail: detail, RepoRoot: func(cwd string) string {
		if strings.HasPrefix(cwd, "/repo/a") {
			return "/repo/a"
		}
		return ""
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func source(t *testing.T, s *Store, agent transcript.Agent, path string) SourceState {
	t.Helper()
	st, err := s.EnsureSource(ctx, transcript.Source{Agent: agent, Path: path, FileID: transcript.FileID{Dev: 1, Ino: 42},
		StorageKind: transcript.StorageJSONLAppend, Parser: string(agent) + "@1"})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

var t0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func msg(session, id string, off int64, kind transcript.Kind, text string) *transcript.Message {
	m := &transcript.Message{SessionID: session, NativeID: id, Ordinal: transcript.OrdinalAt(off, 0), Kind: kind,
		TS: t0.Add(time.Duration(off) * time.Second), LineNo: off + 1, ByteOffset: off * 100, ByteLen: 100, Parser: "claude@1"}
	m.Text, m.FullLen, m.ContentSHA = text, len(text), sha256.Sum256([]byte(text))
	return m
}

func apply(t *testing.T, s *Store, b Batch) BatchResult {
	t.Helper()
	res, err := s.ApplyBatch(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

type rowState struct {
	text       string
	version    int
	superseded bool
	supGen     *int64
	onPath     *bool
}

func rowsOf(t *testing.T, s *Store, nativeID string) []rowState {
	t.Helper()
	rows, err := s.DB().Query(`SELECT text, version, superseded, superseded_in_generation, on_active_path FROM messages WHERE native_id = ? ORDER BY version`, nativeID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []rowState
	for rows.Next() {
		var r rowState
		var blob []byte
		var onPath *int64
		if err := rows.Scan(&blob, &r.version, &r.superseded, &r.supGen, &onPath); err != nil {
			t.Fatal(err)
		}
		r.text, _ = decompress(blob)
		if onPath != nil {
			b := *onPath != 0
			r.onPath = &b
		}
		out = append(out, r)
	}
	return out
}

func findIDs(t *testing.T, s *Store, pattern string, o FindOptions) []string {
	t.Helper()
	hits, err := s.Find(ctx, pattern, o)
	if err != nil && !errors.Is(err, ErrScanLimit) {
		t.Fatal(err)
	}
	var ids []string
	for _, h := range hits {
		ids = append(ids, h.NativeID)
	}
	return ids
}

func searchIDs(t *testing.T, s *Store, q string, o SearchOptions) []string {
	t.Helper()
	hits, err := s.Search(ctx, q, o)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, h := range hits {
		ids = append(ids, h.NativeID)
	}
	return ids
}

func eq(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func TestUpsertRules(t *testing.T) {
	for _, detail := range []Detail{DetailColumn, DetailFull} {
		t.Run(string(detail), func(t *testing.T) {
			s := openTest(t, detail)
			src := source(t, s, transcript.AgentClaude, "/h/s1.jsonl")
			conv := &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "s1", Cwd: "/repo/a/sub", Title: "t"}

			// Insert.
			res := apply(t, s, Batch{SourceID: src.ID, NewGeneration: &transcript.Generation{Generation: 1},
				Conversations: []*transcript.Conversation{conv},
				Messages: []*transcript.Message{
					msg("s1", "u1", 0, transcript.KindUser, "please fix the flaky alpha test"),
					msg("s1", "a1", 1, transcript.KindAssistant, "Looking at"),
				}})
			if res.Inserted != 2 {
				t.Fatalf("inserted %d", res.Inserted)
			}

			// Same content again: touched, no new rows.
			res = apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{msg("s1", "u1", 0, transcript.KindUser, "please fix the flaky alpha test")}})
			if res.Touched != 1 || res.Inserted != 0 {
				t.Fatalf("repeat: %+v", res)
			}

			// Prefix growth replaces in place and re-indexes.
			res = apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{msg("s1", "a1", 1, transcript.KindAssistant, "Looking at internal/foo.go:42 now")}})
			if res.Grown != 1 {
				t.Fatalf("grow: %+v", res)
			}
			if r := rowsOf(t, s, "a1"); len(r) != 1 || r[0].text != "Looking at internal/foo.go:42 now" || r[0].version != 1 {
				t.Fatalf("after growth: %+v", r)
			}
			eq(t, "search grown", searchIDs(t, s, "internal/foo.go", SearchOptions{}), []string{"a1"})
			eq(t, "find grown", findIDs(t, s, "foo.go:42", FindOptions{}), []string{"a1"})

			// Non-prefix change: old version superseded, new version live.
			res = apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{msg("s1", "a1", 1, transcript.KindAssistant, "Rewritten reply about beta")}})
			if res.Versioned != 1 {
				t.Fatalf("version: %+v", res)
			}
			r := rowsOf(t, s, "a1")
			if len(r) != 2 || !r[0].superseded || r[1].superseded || r[1].version != 2 || r[1].text != "Rewritten reply about beta" {
				t.Fatalf("versions: %+v", r)
			}
			eq(t, "default search hides old version", searchIDs(t, s, "internal/foo.go", SearchOptions{}), nil)
			eq(t, "include_superseded shows it", searchIDs(t, s, "internal/foo.go", SearchOptions{Filter: Filter{IncludeSuperseded: true}}), []string{"a1"})
			eq(t, "new version searchable", searchIDs(t, s, "beta", SearchOptions{}), []string{"a1"})

			// New generation without u1: SupersedeAbsent marks it.
			apply(t, s, Batch{SourceID: src.ID, NewGeneration: &transcript.Generation{Generation: 2},
				Messages: []*transcript.Message{msg("s1", "a1", 1, transcript.KindAssistant, "Rewritten reply about beta")}})
			n, err := s.SupersedeAbsent(ctx, src.ID, 2)
			if err != nil || n != 1 {
				t.Fatalf("supersede absent: n=%d err=%v", n, err)
			}
			if r := rowsOf(t, s, "u1"); !r[0].superseded || r[0].supGen == nil || *r[0].supGen != 2 {
				t.Fatalf("u1 after absence: %+v", r)
			}
			eq(t, "absent hidden", searchIDs(t, s, "flaky", SearchOptions{}), nil)
			eq(t, "absent kept", searchIDs(t, s, "flaky", SearchOptions{Filter: Filter{IncludeSuperseded: true}}), []string{"u1"})
			eq(t, "absent find kept", findIDs(t, s, "FLAKY alpha", FindOptions{Filter: Filter{IncludeSuperseded: true}}), []string{"u1"})

			// Re-appears unchanged in generation 3: live again, same row.
			apply(t, s, Batch{SourceID: src.ID, NewGeneration: &transcript.Generation{Generation: 3},
				Messages: []*transcript.Message{msg("s1", "u1", 0, transcript.KindUser, "please fix the flaky alpha test"), msg("s1", "a1", 1, transcript.KindAssistant, "Rewritten reply about beta")}})
			if n, _ := s.SupersedeAbsent(ctx, src.ID, 3); n != 0 {
				t.Fatalf("nothing should be absent, got %d", n)
			}
			if r := rowsOf(t, s, "u1"); len(r) != 1 || r[0].superseded || r[0].supGen != nil {
				t.Fatalf("u1 re-appeared: %+v", r)
			}
			eq(t, "re-appeared", searchIDs(t, s, "flaky", SearchOptions{}), []string{"u1"})

			// Conversation facts and repo root.
			convs, err := s.ListConversations(ctx, ListOptions{})
			if err != nil || len(convs) != 1 {
				t.Fatalf("list: %v %v", convs, err)
			}
			c := convs[0]
			if c.RepoRoot != "/repo/a" || c.Title != "t" || c.Messages != 2 || !c.StartedAt.Equal(t0) || !c.LastActivityAt.Equal(t0.Add(time.Second)) {
				t.Fatalf("conversation: %+v", c)
			}
			checkFTS(t, s)
		})
	}
}

func TestLocatorKeyAndParts(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentCodex, "/h/r.jsonl")
	// No native id: keyed by (source, byte offset, part). Two parts share a line.
	m1 := msg("c1", "", 5, transcript.KindToolCall, "exec ls -la")
	m2 := msg("c1", "", 5, transcript.KindToolResult, "total 0")
	m2.Part = 1
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{m1, m2}})
	m2b := msg("c1", "", 5, transcript.KindToolResult, "total 0\ndrwxr-xr-x")
	m2b.Part = 1
	res := apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{m1, m2b}})
	if res.Touched != 1 || res.Grown != 1 || res.Inserted != 0 {
		t.Fatalf("locator upsert: %+v", res)
	}
	// Locator-keyed rows (SQLite sources).
	l := &transcript.Message{SessionID: "c1", Locator: "rowid:9", Kind: transcript.KindUser, Parser: "x@1", Text: "hello there"}
	l.ContentSHA = sha256.Sum256([]byte(l.Text))
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{l}})
	res = apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{l}})
	if res.Touched != 1 {
		t.Fatalf("locator row: %+v", res)
	}
	var n int
	s.DB().QueryRow(`SELECT count(*) FROM messages`).Scan(&n)
	if n != 3 {
		t.Fatalf("rows = %d, want 3", n)
	}
	checkFTS(t, s)
}

func TestActivePathAndTombstone(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentDevin, "/h/devin.db")
	var ms []*transcript.Message
	for i, id := range []string{"n1", "n2", "n3", "n4"} {
		ms = append(ms, msg("d1", id, int64(i), transcript.KindAssistant, "devin node "+id+" gamma"))
	}
	ms[3].OnActivePath = transcript.BoolPtr(false) // parser-provided
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: ms})
	eq(t, "parser off-path hidden", searchIDs(t, s, "gamma", SearchOptions{}), []string{"n3", "n2", "n1"})

	// The header's message count (ListConversations, from the digest)
	// follows the rows' path and supersession.
	count := func(want int) {
		t.Helper()
		convs, err := s.ListConversations(ctx, ListOptions{IncludeDeleted: true})
		if err != nil || len(convs) != 1 || convs[0].Messages != want {
			t.Fatalf("message count: %+v %v, want %d", convs, err, want)
		}
	}
	count(3)
	n, err := s.SetActivePath(ctx, transcript.AgentDevin, "d1", []string{"n1", "n3", "n4"})
	if err != nil || n != 4 { // n1,n2,n3 from NULL, n4 false->true
		t.Fatalf("set active path: n=%d err=%v", n, err)
	}
	count(3)
	if _, err := s.SetActivePath(ctx, transcript.AgentDevin, "d1", []string{"n1"}); err != nil {
		t.Fatal(err)
	}
	count(1)
	if _, err := s.SetActivePath(ctx, transcript.AgentDevin, "d1", []string{"n1", "n3", "n4"}); err != nil {
		t.Fatal(err)
	}
	count(3)
	eq(t, "branches hidden", searchIDs(t, s, "gamma", SearchOptions{}), []string{"n4", "n3", "n1"})
	eq(t, "include_branches", searchIDs(t, s, "gamma", SearchOptions{Filter: Filter{IncludeBranches: true}}), []string{"n4", "n3", "n2", "n1"})
	eq(t, "find hides branches", findIDs(t, s, "node n2", FindOptions{}), nil)
	eq(t, "find include_branches", findIDs(t, s, "node n2", FindOptions{Filter: Filter{IncludeBranches: true}}), []string{"n2"})

	// Flip: n2 back on path, n4 off.
	if _, err := s.SetActivePath(ctx, transcript.AgentDevin, "d1", []string{"n1", "n2", "n3"}); err != nil {
		t.Fatal(err)
	}
	eq(t, "flipped", searchIDs(t, s, "gamma", SearchOptions{}), []string{"n3", "n2", "n1"})
	// A later touch without a pointer keeps the flag.
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{msg("d1", "n4", 3, transcript.KindAssistant, "devin node n4 gamma")}})
	if r := rowsOf(t, s, "n4"); r[0].onPath == nil || *r[0].onPath {
		t.Fatalf("n4 flag lost: %+v", r)
	}

	// Session deleted: every row superseded, conversation hidden, rows kept.
	if err := s.TombstoneConversation(ctx, transcript.AgentDevin, "d1", 2); err != nil {
		t.Fatal(err)
	}
	count(0)
	eq(t, "tombstoned", searchIDs(t, s, "gamma", SearchOptions{}), nil)
	eq(t, "tombstoned kept", searchIDs(t, s, "gamma", SearchOptions{Filter: Filter{IncludeSuperseded: true, IncludeBranches: true}}), []string{"n4", "n3", "n2", "n1"})
	if convs, _ := s.ListConversations(ctx, ListOptions{}); len(convs) != 0 {
		t.Fatalf("deleted conversation listed: %+v", convs)
	}
	if convs, _ := s.ListConversations(ctx, ListOptions{IncludeDeleted: true}); len(convs) != 1 || !convs[0].Deleted {
		t.Fatalf("deleted conversation: %+v", convs)
	}
	checkFTS(t, s)
}

func TestPurgeAndFTSConsistency(t *testing.T) {
	s := openTest(t, DetailFull)
	src := source(t, s, transcript.AgentClaude, "/h/p.jsonl")
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{
		msg("keep", "k1", 0, transcript.KindUser, "shared delta words"),
		msg("gone", "g1", 1, transcript.KindUser, "shared delta words and more"),
		msg("gone", "g2", 2, transcript.KindUser, ""), // empty text: no FTS rows
	}})
	if err := s.PurgeConversation(ctx, transcript.AgentClaude, "gone"); err != nil {
		t.Fatal(err)
	}
	eq(t, "purged search", searchIDs(t, s, "delta", SearchOptions{Filter: Filter{IncludeSuperseded: true}}), []string{"k1"})
	eq(t, "purged find", findIDs(t, s, "delta words", FindOptions{}), []string{"k1"})
	checkFTS(t, s)
}

// checkFTS asserts that both FTS tables index exactly the rows with text,
// with each row's current text: every stored row is found by a distinctive
// token of its text, and the FTS row counts match.
func checkFTS(t *testing.T, s *Store) {
	t.Helper()
	var withText, tok, tri int
	s.DB().QueryRow(`SELECT count(*) FROM messages WHERE text_len > 0`).Scan(&withText)
	// fts5vocab-free count: the docsize shadow table holds one row per document.
	s.DB().QueryRow(`SELECT count(*) FROM fts_tok_docsize`).Scan(&tok)
	for _, sh := range s.tri {
		var n int
		s.DB().QueryRow(`SELECT count(*) FROM ` + sh.schema + `.fts_tri_docsize`).Scan(&n)
		tri += n
	}
	if tok != withText || tri != withText {
		t.Fatalf("fts rows tok=%d tri=%d, messages with text=%d", tok, tri, withText)
	}
	rows, _ := s.DB().Query(`SELECT id, text FROM messages WHERE text_len > 0`)
	defer rows.Close()
	for rows.Next() {
		var id int64
		var blob []byte
		rows.Scan(&id, &blob)
		text, _ := decompress(blob)
		var got int
		if runeLen(text) >= 3 {
			part := s.tri[id%int64(len(s.tri))].schema
			if err := s.DB().QueryRow(`SELECT count(*) FROM `+part+`.fts_tri WHERE fts_tri MATCH ? AND rowid = ?`, s.substringExpr(text), id).Scan(&got); err != nil || got != 1 {
				t.Errorf("row %d text %q not in fts_tri (%v)", id, text, err)
			}
		}
		var toks []string
		tokenize(text, func(s string) { toks = append(toks, ftsString(s)) })
		if len(toks) > 0 {
			if err := s.DB().QueryRow(`SELECT count(*) FROM fts_tok WHERE fts_tok MATCH ? AND rowid = ?`, strings.Join(toks, " AND "), id).Scan(&got); err != nil || got != 1 {
				t.Errorf("row %d text %q not in fts_tok (%v)", id, text, err)
			}
		}
	}
}

func TestFindModes(t *testing.T) {
	for _, d := range []Detail{DetailColumn, DetailFull} {
		t.Run(string(d), func(t *testing.T) { testFindModes(t, openTest(t, d)) })
	}
}

func testFindModes(t *testing.T, s *Store) {
	src := source(t, s, transcript.AgentClaude, "/h/f.jsonl")
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{
		msg("f", "m1", 0, transcript.KindToolResult, "ok\npanic: runtime error at internal/foo.go:42\nexit status 2"),
		msg("f", "m2", 1, transcript.KindAssistant, "The ERROR in Internal/Foo.go:42 is fixed"),
		msg("f", "m3", 2, transcript.KindUser, "use a := b; x"),
		msg("f", "m4", 3, transcript.KindUser, "Ünïcode naïve café test"),
	}})
	eq(t, "path, insensitive", findIDs(t, s, "internal/foo.go:42", FindOptions{}), []string{"m2", "m1"})
	eq(t, "path, sensitive", findIDs(t, s, "internal/foo.go:42", FindOptions{CaseSensitive: true}), []string{"m1"})
	eq(t, "mixed case, sensitive", findIDs(t, s, "ERROR", FindOptions{CaseSensitive: true}), []string{"m2"})
	eq(t, "mixed case, insensitive", findIDs(t, s, "eRrOr", FindOptions{}), []string{"m2", "m1"})
	eq(t, "short pattern scan", findIDs(t, s, ":=", FindOptions{}), []string{"m3"})
	eq(t, "one char", findIDs(t, s, ";", FindOptions{CaseSensitive: true}), []string{"m3"})
	eq(t, "unicode", findIDs(t, s, "NAÏVE", FindOptions{}), []string{"m4"})
	eq(t, "long pattern", findIDs(t, s, "panic: runtime error at internal/foo.go:42\nexit status 2", FindOptions{}), []string{"m1"})
	eq(t, "trigrams present, substring absent", findIDs(t, s, "foo.go:42 is fixed at", FindOptions{}), nil)
	eq(t, "kind filter", findIDs(t, s, "foo.go", FindOptions{Filter: Filter{Kinds: []transcript.Kind{transcript.KindToolResult}}}), []string{"m1"})

	hits, err := s.Find(ctx, "foo.go:42", FindOptions{Filter: Filter{Kinds: []transcript.Kind{transcript.KindToolResult}}})
	if err != nil || len(hits) != 1 || len(hits[0].Matches) != 1 {
		t.Fatalf("hits: %+v %v", hits, err)
	}
	if lm := hits[0].Matches[0]; lm.Line != 2 || lm.Text != "panic: runtime error at internal/foo.go:42" || lm.Col != 33 {
		t.Fatalf("line match: %+v", lm)
	}
	// Short-pattern scan bound.
	_, err = s.Find(ctx, "zz", FindOptions{MaxScan: 2})
	if !errors.Is(err, ErrScanLimit) {
		t.Fatalf("scan limit: %v", err)
	}
	// Limit.
	if ids := findIDs(t, s, "o", FindOptions{Limit: 2}); len(ids) != 2 {
		t.Fatalf("limit: %v", ids)
	}
}

func TestSearchFilters(t *testing.T) {
	s := openTest(t, DetailColumn)
	cl := source(t, s, transcript.AgentClaude, "/h/c.jsonl")
	cx := source(t, s, transcript.AgentCodex, "/h/x.jsonl")
	sub := source(t, s, transcript.AgentClaude, "/h/c/subagents/agent-1.jsonl")
	parentCall := msg("parent", "tc", 1, transcript.KindToolCall, "Task: investigate omega")
	parentCall.ToolCallID = "toolu_1"
	parentCall.ToolName = "Task"
	apply(t, s, Batch{SourceID: cl.ID, Generation: 1,
		Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "parent", Cwd: "/repo/a"}},
		Messages:      []*transcript.Message{msg("parent", "p1", 0, transcript.KindUser, "omega release notes"), parentCall}})
	// Child arrives first under a different source, then links.
	apply(t, s, Batch{SourceID: sub.ID, Generation: 1,
		Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "child", Cwd: "/repo/a", ParentSessionID: "parent", SpawnedByToolCallID: "toolu_1", Depth: 1}},
		Messages:      []*transcript.Message{msg("child", "c1", 2, transcript.KindAssistant, "omega found in subagent")}})
	apply(t, s, Batch{SourceID: cx.ID, Generation: 1,
		Conversations: []*transcript.Conversation{{Agent: transcript.AgentCodex, SessionID: "cx", Cwd: "/elsewhere"}},
		Messages:      []*transcript.Message{msg("cx", "x1", 3, transcript.KindToolResult, "omega from codex tool output")}})

	all := searchIDs(t, s, "omega", SearchOptions{})
	if len(all) != 4 {
		t.Fatalf("all: %v", all)
	}
	eq(t, "agent", searchIDs(t, s, "omega", SearchOptions{Filter: Filter{Agents: []transcript.Agent{transcript.AgentCodex}}}), []string{"x1"})
	if got := searchIDs(t, s, "omega", SearchOptions{Filter: Filter{Repos: []string{"/repo/a"}}}); len(got) != 3 {
		t.Fatalf("repo: %v", got)
	}
	eq(t, "repo prefix does not match sibling", searchIDs(t, s, "omega", SearchOptions{Filter: Filter{Repos: []string{"/rep"}}}), nil)
	if got := searchIDs(t, s, "omega", SearchOptions{Filter: Filter{ExcludeSubagents: true}}); len(got) != 3 || strings.Contains(strings.Join(got, ","), "c1") {
		t.Fatalf("exclude subagents: %v", got)
	}
	if got := searchIDs(t, s, "omega", SearchOptions{Filter: Filter{ExcludeSessions: []string{"parent"}}}); len(got) != 2 {
		t.Fatalf("exclude session: %v", got)
	}
	eq(t, "kind", searchIDs(t, s, "omega", SearchOptions{Filter: Filter{Kinds: []transcript.Kind{transcript.KindUser}}}), []string{"p1"})
	eq(t, "time range", searchIDs(t, s, "omega", SearchOptions{Filter: Filter{Since: t0.Add(2 * time.Second), Until: t0.Add(3 * time.Second)}}), []string{"c1"})
	eq(t, "device", searchIDs(t, s, "omega", SearchOptions{Filter: Filter{Devices: []string{"other"}}}), nil)
	eq(t, "AND semantics", searchIDs(t, s, "omega codex", SearchOptions{}), []string{"x1"})
	if got := searchIDs(t, s, "subagent codex", SearchOptions{AnyTerm: true}); len(got) != 2 {
		t.Fatalf("OR: %v", got)
	}
	eq(t, "prefix", searchIDs(t, s, "subag*", SearchOptions{}), []string{"c1"})
	eq(t, "sentence punctuation", searchIDs(t, s, "notes.", SearchOptions{}), []string{"p1"})

	hits, _ := s.Search(ctx, "codex", SearchOptions{})
	if len(hits) != 1 || !strings.Contains(hits[0].Snippet, "codex") || len(hits[0].Highlights) != 1 {
		t.Fatalf("snippet: %+v", hits)
	}
	if h := hits[0]; h.Snippet[h.Highlights[0][0]:h.Highlights[0][1]] != "codex" || h.SourcePath != "/h/x.jsonl" || h.Agent != transcript.AgentCodex {
		t.Fatalf("highlight/provenance: %+v", h)
	}

	// Subagent link resolved even though the child arrived first.
	convs, _ := s.ListConversations(ctx, ListOptions{Filter: Filter{Agents: []transcript.Agent{transcript.AgentClaude}}})
	var child ConversationRow
	for _, c := range convs {
		if c.SessionID == "child" {
			child = c
		}
	}
	if child.ParentConversationID == 0 || child.SpawnedByMessageID == 0 || child.Depth != 1 {
		t.Fatalf("child links: %+v", child)
	}
}

func TestParentArrivesAfterChild(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/h/c.jsonl")
	apply(t, s, Batch{SourceID: src.ID, Generation: 1,
		Conversations: []*transcript.Conversation{{SessionID: "child", ParentSessionID: "parent", SpawnedByToolCallID: "toolu_9", Depth: 1}}})
	call := msg("parent", "tc", 0, transcript.KindToolCall, "Task")
	call.ToolCallID = "toolu_9"
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{SessionID: "parent"}}, Messages: []*transcript.Message{call}})
	convs, _ := s.ListConversations(ctx, ListOptions{})
	for _, c := range convs {
		if c.SessionID == "child" && (c.ParentConversationID == 0 || c.SpawnedByMessageID == 0) {
			t.Fatalf("child not linked: %+v", c)
		}
	}
}

func TestContext(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/h/ctx.jsonl")
	var ms []*transcript.Message
	for i := range 6 {
		ms = append(ms, msg("c", "m"+itoa(int64(i)), int64(i), transcript.KindUser, "line "+itoa(int64(i))))
	}
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, NewGeneration: &transcript.Generation{Generation: 1}, Messages: ms})
	// m2 disappears in generation 2.
	apply(t, s, Batch{SourceID: src.ID, NewGeneration: &transcript.Generation{Generation: 2}, Messages: append(append([]*transcript.Message{}, ms[:2]...), ms[3:]...)})
	s.SupersedeAbsent(ctx, src.ID, 2)
	var target int64
	s.DB().QueryRow(`SELECT id FROM messages WHERE native_id = 'm3'`).Scan(&target)
	ids := func(rows []*Row) (out []string) {
		for _, r := range rows {
			out = append(out, r.NativeID)
		}
		return
	}
	rows, err := s.Context(ctx, target, 2, 1, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "context", ids(rows), []string{"m0", "m1", "m3", "m4"})
	rows, _ = s.Context(ctx, target, 2, 1, Filter{IncludeSuperseded: true})
	eq(t, "context incl superseded", ids(rows), []string{"m1", "m2", "m3", "m4"})
	rows, _ = s.Context(ctx, target, 10, 10, Filter{})
	eq(t, "context wide", ids(rows), []string{"m0", "m1", "m3", "m4", "m5"})
	if rows[2].Text != "line 3" {
		t.Fatalf("text: %q", rows[2].Text)
	}
}

func TestWatermarkAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "i.db")
	s, err := Open(path, Options{TokDetail: DetailFull})
	if err != nil {
		t.Fatal(err)
	}
	src := source(t, s, transcript.AgentClaude, "/h/w.jsonl")
	if src.Watermark != nil {
		t.Fatal("fresh source has a watermark")
	}
	wm := transcript.Watermark{Identity: transcript.Identity{ID: transcript.FileID{Dev: 1, Ino: 42}, Size: 900, CTime: 77},
		SampledAt: 88, Offset: 800, LineNo: 8, HeadLen: 800, AnchorLen: 800}
	wm.HeadHash[0], wm.AnchorSum[31] = 1, 2
	apply(t, s, Batch{SourceID: src.ID, NewGeneration: &transcript.Generation{Generation: 1, Size: 900}, Watermark: &wm, CursorState: []byte("state"),
		Messages: []*transcript.Message{msg("w", "a", 0, transcript.KindUser, "hi there")}})
	s.Close()

	s, err = Open(path, Options{TokDetail: DetailColumn}) // existing detail wins
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Details().Tok != DetailFull {
		t.Fatalf("details %+v", s.Details())
	}
	got, err := s.SourcesByPath(ctx, "/h/w.jsonl")
	if err != nil || len(got) != 1 {
		t.Fatalf("sources: %v %v", got, err)
	}
	if got[0].Watermark == nil || *got[0].Watermark != wm || string(got[0].CursorState) != "state" || got[0].Generation != 1 || got[0].Source.FileID != (transcript.FileID{Dev: 1, Ino: 42}) {
		t.Fatalf("watermark round trip: %+v", got[0])
	}
	if err := s.UpsertCompanion(ctx, Companion{SessionID: "w", Agent: transcript.AgentClaude, SourceID: src.ID, Path: "/h/w/tool-results/x.txt", Kind: "tool_result", Size: 5, MessageNativeID: "a"}); err != nil {
		t.Fatal(err)
	}
	var mid *int64
	s.DB().QueryRow(`SELECT message_id FROM companions`).Scan(&mid)
	if mid == nil {
		t.Fatal("companion not linked to message")
	}
}

func TestTokText(t *testing.T) {
	cases := map[string]string{
		"done.":                   "done ",
		"see ./scripts/x.sh now":  "see   scripts/x.sh now",
		"internal/ and --flag -v": "internal  and --flag -v",
		"a/b.go:42.":              "a/b.go:42 ",
		"-----":                   "     ",
	}
	for in, want := range cases {
		if got := tokText(in); got != want {
			t.Errorf("tokText(%q) = %q, want %q", in, got, want)
		}
	}
	expr, terms := tokQuery(`internal/foo.go:42 "quoted" pre*`, false)
	if expr != `"internal/foo.go" AND "42" AND "quoted" AND "pre" *` || len(terms) != 4 {
		t.Fatalf("tokQuery: %s %v", expr, terms)
	}
}

func TestTrigramQuery(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/h/r.jsonl")
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{
		msg("r", "r1", 0, transcript.KindUser, "func handleRequest(w http.ResponseWriter)"),
		msg("r", "r2", 1, transcript.KindUser, "func handleResponse()"),
		msg("r", "r3", 2, transcript.KindUser, "nothing relevant"),
	}})
	tri := func(ts ...string) *TrigramQuery { return &TrigramQuery{Op: TrigramAnd, Trigrams: ts} }
	// handle(Request|Response): AND(han, and, ndl, dle) AND OR(req.., res..)
	q := &TrigramQuery{Op: TrigramAnd, Trigrams: []string{"han", "and", "ndl", "dle"}, Sub: []*TrigramQuery{
		{Op: TrigramOr, Sub: []*TrigramQuery{tri("req", "equ"), tri("res", "esp")}},
	}}
	ids, err := s.CandidateIDs(ctx, q, Filter{}, 0)
	if err != nil || len(ids) != 2 {
		t.Fatalf("candidates: %v %v", ids, err)
	}
	if ids, _ := s.CandidateIDs(ctx, &TrigramQuery{Op: TrigramNone}, Filter{}, 0); ids != nil {
		t.Fatalf("none: %v", ids)
	}
	if _, err := s.CandidateIDs(ctx, &TrigramQuery{Op: TrigramAll}, Filter{}, 0); err == nil {
		t.Fatal("all should need a scan bound")
	}
	n := 0
	if _, err := s.Scan(ctx, &TrigramQuery{Op: TrigramOr, Sub: []*TrigramQuery{tri("zzz"), {Op: TrigramAll}}}, ScanOptions{MaxScan: 10}, func(*Row) bool { n++; return true }); err != nil || n != 3 {
		t.Fatalf("OR with All scans: n=%d err=%v", n, err)
	}
	n = 0
	s.Scan(ctx, &TrigramQuery{Op: TrigramAnd, Sub: []*TrigramQuery{tri("zzz"), {Op: TrigramNone}}}, ScanOptions{MaxScan: 10}, func(*Row) bool { n++; return true })
	if n != 0 {
		t.Fatalf("AND with None: %d", n)
	}
}

func TestConcurrentReadWrite(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/h/cc.jsonl")
	done := make(chan struct{})
	errs := make(chan error, 4)
	for range 4 {
		go func() {
			for {
				select {
				case <-done:
					errs <- nil
					return
				default:
				}
				if _, err := s.Search(ctx, "concurrent", SearchOptions{}); err != nil {
					errs <- err
					return
				}
				if _, err := s.Find(ctx, "rrent row", FindOptions{}); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	for i := range 200 {
		apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{msg("cc", "m"+itoa(int64(i)), int64(i), transcript.KindUser, "concurrent row "+itoa(int64(i)))}})
	}
	close(done)
	for range 4 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if ids := searchIDs(t, s, "concurrent", SearchOptions{Limit: 1000}); len(ids) != 200 {
		t.Fatalf("got %d", len(ids))
	}
}

// TestSameBatchChurn changes one key several times inside one batch, so
// the per-transaction FTS buffer sees insert, growth and versioning of the
// same row before it is flushed.
func TestSameBatchChurn(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/h/churn.jsonl")
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{
		msg("c", "old", 0, transcript.KindAssistant, "committed epsilon"),
	}})
	res := apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{
		msg("c", "new", 1, transcript.KindAssistant, "fresh"),
		msg("c", "new", 1, transcript.KindAssistant, "fresh zeta"),              // grows a row new in this tx
		msg("c", "new", 1, transcript.KindAssistant, "rewritten eta"),           // versions it
		msg("c", "old", 0, transcript.KindAssistant, "committed epsilon theta"), // grows a committed row
		msg("c", "old", 0, transcript.KindAssistant, "committed epsilon theta iota"),
	}})
	if res.Inserted != 1 || res.Grown != 3 || res.Versioned != 1 {
		t.Fatalf("result %+v", res)
	}
	eq(t, "grown committed", searchIDs(t, s, "iota", SearchOptions{}), []string{"old"})
	eq(t, "versioned", searchIDs(t, s, "eta", SearchOptions{}), []string{"new"})
	eq(t, "old version", searchIDs(t, s, "zeta", SearchOptions{}), nil)
	eq(t, "old version kept", searchIDs(t, s, "zeta", SearchOptions{Filter: Filter{IncludeSuperseded: true}}), []string{"new"})
	checkFTS(t, s)
}

// TestSinkSupersedeSession drives the Sink the way the Devin parser does:
// rows, then SupersedeSession, then a re-emit of the rows still present.
func TestSinkSupersedeSession(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentDevin, "/h/sessions.db")
	k := s.NewSink(ctx, src.ID, 1)
	k.BatchSize = 2
	k.Conversation(&transcript.Conversation{Agent: transcript.AgentDevin, SessionID: "d"})
	for i, id := range []string{"a", "b", "c"} {
		if err := k.Message(msg("d", id, int64(i), transcript.KindUser, "kappa "+id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := k.Flush(&transcript.Watermark{Offset: 7}, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	eq(t, "first parse", searchIDs(t, s, "kappa", SearchOptions{}), []string{"c", "b", "a"})

	k = s.NewSink(ctx, src.ID, 2)
	k.Message(msg("d", "x", 9, transcript.KindUser, "pending lambda")) // pending when the superseder fires
	if err := k.SupersedeSession(transcript.AgentDevin, "d"); err != nil {
		t.Fatal(err)
	}
	k.Message(msg("d", "a", 0, transcript.KindUser, "kappa a"))
	k.Message(msg("d", "c", 2, transcript.KindUser, "kappa c"))
	if err := k.Flush(nil, nil); err != nil {
		t.Fatal(err)
	}
	eq(t, "after reset", searchIDs(t, s, "kappa", SearchOptions{}), []string{"c", "a"})
	eq(t, "pending row superseded too", searchIDs(t, s, "lambda", SearchOptions{}), nil)
	if r := rowsOf(t, s, "b"); len(r) != 1 || !r[0].superseded || r[0].supGen == nil || *r[0].supGen != 2 {
		t.Fatalf("b: %+v", r)
	}
	if k.Result.Touched != 2 {
		t.Fatalf("result %+v", k.Result)
	}
	st, err := s.Source(ctx, src.ID)
	if err != nil || st.Watermark == nil || st.Watermark.Offset != 7 {
		t.Fatalf("watermark %+v %v", st.Watermark, err)
	}
}
