package local

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/grep"
	"github.com/flopwire/flopwire/internal/retrieval/local/localtest"
	"github.com/flopwire/flopwire/internal/transcript"
)

var ctx = context.Background()

// oracle indexes a copy of testdata/oracle/home and returns the backend
// and the copy's path.
func oracle(t *testing.T) (*Backend, string) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	if err := os.CopyFS(home, os.DirFS("../../../testdata/oracle/home")); err != nil {
		t.Fatal(err)
	}
	s, err := localindex.Open(filepath.Join(t.TempDir(), "index.db"), localindex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := localtest.IndexHome(ctx, s, home); err != nil {
		t.Fatal(err)
	}
	return &Backend{Store: s}, home
}

func hitIDs(hs []format.Hit) string {
	var ids []string
	for _, h := range hs {
		ids = append(ids, h.MessageID)
	}
	return strings.Join(ids, ",")
}

func grepFor(t *testing.T, b *Backend, spec grep.Spec, f format.Filters, set ...func(*format.GrepQuery)) *format.Page {
	t.Helper()
	var q format.GrepQuery
	if err := spec.Query(&q); err != nil {
		t.Fatal(err)
	}
	for _, s := range set {
		s(&q)
	}
	page, err := b.Grep(ctx, q, f)
	if err != nil {
		t.Fatalf("grep %+v: %v", spec, err)
	}
	return page
}

func pat(p string) grep.Spec { return grep.Spec{Patterns: []string{p}} }

func TestGrepSearchAndFilters(t *testing.T) {
	b, home := oracle(t)
	page := grepFor(t, b, grep.Spec{Patterns: []string{"setTimeout(500)"}, Fixed: true}, format.Filters{})
	// 13 (the subagent's reply) has the text of 5 (the Task result that
	// returned it): one hit, the newer, with a copy (decision D6).
	if hitIDs(page.Hits) != "5" || page.Hits[0].Copies != 1 || page.Total != 1 || !page.Exact || len(page.Notes) != 0 {
		t.Fatalf("substring: %s %+v", hitIDs(page.Hits), page)
	}
	h := page.Hits[0]
	if h.Agent != "claude" || h.Kind != "tool_result" || h.ToolName != "Task" || len(h.Lines) != 1 || h.Lines[0].N != 1 || !h.Lines[0].Match ||
		h.Provenance.LineNo != 6 || *h.Provenance.ByteOffset != 2485 || h.Provenance.ByteLen != 684 || h.Provenance.Generation != 1 ||
		!strings.HasPrefix(h.Provenance.Path, home) || h.Title != "why does the login test flake?" || h.TS == nil ||
		h.Address != format.MessageAddress(h.SessionID, h.Ordinal) || h.Repo != "/tmp/oracle-alpha" {
		t.Fatalf("hit %+v", h)
	}
	// Regex, smart case, grep line semantics, newest message first (D7).
	page = grepFor(t, b, pat(`^exit (code|status) \d$`), format.Filters{})
	if hitIDs(page.Hits) != "24,7,97,63" || page.Hits[0].Lines[0].N != 2 || page.Hits[0].Lines[0].Text != "exit code 2" {
		t.Fatalf("regex: %s %+v", hitIDs(page.Hits), page.Hits[0].Lines)
	}
	if page = grepFor(t, b, pat(`CHI\.New(Router|Mux)`), format.Filters{}); len(page.Hits) != 0 {
		t.Fatalf("smart case: an uppercase pattern matches case: %s", hitIDs(page.Hits))
	}
	if page = grepFor(t, b, grep.Spec{Patterns: []string{`CHI\.New(Router|Mux)`}, IgnoreCase: true}, format.Filters{}); len(page.Hits) != 4 {
		t.Fatalf("-i: %s", hitIDs(page.Hits))
	}
	// Filters: agent, kind, subagents, tool, excluded kinds, repo by name
	// and glob.
	for _, tc := range []struct {
		name string
		f    format.Filters
		want string
	}{
		{"agent+kind", format.Filters{Agent: "codex", Kinds: []string{"assistant"}}, "98,100"},
		{"exclude subagents", format.Filters{Agent: "codex", Kinds: []string{"assistant"}, ExcludeSubagents: true}, "98"},
		{"tool", format.Filters{Tools: []string{"EXEC_COMMAND"}}, "97,59"},
		{"exclude kind", format.Filters{Agent: "codex", ExcludeKinds: []string{"tool_result", "assistant"}}, ""},
		{"repo name", format.Filters{Repo: "oracle-beta"}, "59"},
		{"repo glob", format.Filters{Repo: "oracle-b*"}, "59"},
		{"repo path", format.Filters{Repo: "/tmp/oracle-alpha"}, "98,97,100"},
	} {
		if got := hitIDs(grepFor(t, b, grep.Spec{Patterns: []string{"chi.New"}, Fixed: true}, tc.f).Hits); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
	if _, err := b.Grep(ctx, format.GrepQuery{Pattern: "x", Fixed: true}, format.Filters{Kinds: []string{"bogus"}}); !errors.Is(err, format.ErrBadRequest) {
		t.Fatalf("bad kind: %v", err)
	}
	// Unindexed patterns say so.
	if page = grepFor(t, b, grep.Spec{Patterns: []string{"go"}, Fixed: true}, format.Filters{}); len(page.Notes) == 0 || !strings.HasPrefix(page.Notes[0], "unindexed") {
		t.Fatalf("short pattern: %+v", page)
	}
	if page = grepFor(t, b, pat(`4.4`), format.Filters{}); !strings.HasPrefix(page.Notes[0], "unindexed") || len(page.Hits) == 0 {
		t.Fatalf("unindexed regex: %+v", page)
	}
	// Paging: offsets walk the same order, and the footer totals hold.
	all := grepFor(t, b, pat("retr"), format.Filters{}, func(q *format.GrepQuery) { q.Limit = 100 })
	var paged []string
	for off := 0; ; {
		p := grepFor(t, b, pat("retr"), format.Filters{}, func(q *format.GrepQuery) { q.Limit, q.Offset = 3, off })
		if p.Total != all.Total || p.TotalSessions != all.TotalSessions {
			t.Fatalf("totals differ across pages: %+v vs %+v", p, all)
		}
		paged = append(paged, hitIDs(p.Hits))
		if p.Next == 0 {
			break
		}
		off = p.Next
	}
	if strings.Join(paged, ",") != hitIDs(all.Hits) || all.Total != len(all.Hits) {
		t.Fatalf("pages %s, all %s", strings.Join(paged, ","), hitIDs(all.Hits))
	}
}

func TestSearch(t *testing.T) {
	b, _ := oracle(t)
	search := func(q string, f format.Filters) *format.Page {
		t.Helper()
		p, err := b.Search(ctx, format.SearchQuery{Query: q}, f)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := search("exponential backoff", format.Filters{Limit: 3})
	if len(p.Hits) == 0 || p.Hits[0].Snippet == "" || p.Hits[0].Title == "" || p.Hits[0].Address == "" || len(p.Notes) != 0 {
		t.Fatalf("search: %+v", p)
	}
	// Every term matching nothing: an any-term retry without stopwords,
	// and a note.
	p = search("how did we handle the exponential tokenizer?", format.Filters{})
	if len(p.Hits) == 0 || len(p.Notes) != 1 || !strings.Contains(p.Notes[0], "any of: handle exponential tokenizer") {
		t.Fatalf("any-term retry: %+v", p)
	}
	// A quoted phrase must appear as written.
	if p = search(`"use exponential"`, format.Filters{}); len(p.Hits) != 1 || !strings.Contains(p.Hits[0].Snippet, "use exponential") {
		t.Fatalf("phrase: %+v", p)
	}
	if p = search(`"exponential use"`, format.Filters{}); len(p.Hits) != 0 {
		t.Fatalf("phrase out of order: %+v", p)
	}
	// D2: over the rank cap, the note says so.
	defer localindex.SetSearchRankCap(2)()
	if p = search("retry", format.Filters{}); len(p.Notes) != 1 || !regexp.MustCompile(`^ranked newest 2 of \d+ matches — add terms or filters$`).MatchString(p.Notes[0]) {
		t.Fatalf("rank cap note: %+v", p.Notes)
	}
}

// Self-session exclusion drops the session and every subagent below it.
func TestExcludeSession(t *testing.T) {
	b, _ := oracle(t)
	lim := func(q *format.GrepQuery) { q.Limit = 100 }
	all := grepFor(t, b, pat("retr"), format.Filters{}, lim).Hits
	mine := grepFor(t, b, pat("retr"), format.Filters{ExcludeSession: "0b7e2c1a-0000-4000-8000-000000000002"}, lim).Hits
	if len(all) == 0 || len(mine) != 0 {
		t.Fatalf("exclude beta session: all=%s left=%s", hitIDs(all), hitIDs(mine))
	}
	mine = grepFor(t, b, pat("retr"), format.Filters{ExcludeSession: "agent-b000000000000002"}, lim).Hits
	for _, h := range mine {
		if h.ConversationID == "4" || h.ConversationID == "5" {
			t.Fatalf("subagent tree not excluded: %+v", h)
		}
	}
	if len(mine) == 0 {
		t.Fatal("excluded too much")
	}
	s, err := b.Sessions(ctx, "", "", format.Filters{ExcludeSession: "0b7e2c1a-0000-4000-8000-000000000002", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range s.Sessions {
		if c.SessionID == "0b7e2c1a-0000-4000-8000-000000000002" {
			t.Fatal("sessions lists the excluded session")
		}
	}
}

func TestReadAndRaw(t *testing.T) {
	b, home := oracle(t)
	read := func(q format.ReadQuery) *format.Context {
		t.Helper()
		cx, err := b.Read(ctx, q, format.Filters{})
		if err != nil {
			t.Fatalf("read %+v: %v", q, err)
		}
		return cx
	}
	hit := grepFor(t, b, pat("expected 200"), format.Filters{}).Hits[0]
	cx := read(format.ReadQuery{Address: hit.Address + ":2", Before: 2, After: 1})
	if cx.Focus != "7" || cx.Line != 2 || len(cx.Messages) != 4 || cx.Messages[0].ID != "5" || cx.Messages[3].ID != "8" ||
		cx.Conversation.SessionID != "0b7e2c1a-0000-4000-8000-000000000001" || !cx.MoreBefore || !cx.MoreAfter {
		t.Fatalf("read %+v", cx)
	}
	// The same message by id, by path:line, and a session from its start.
	if cx = read(format.ReadQuery{Address: "7"}); cx.Focus != "7" || len(cx.Messages) != 1 {
		t.Fatalf("by id: %+v", cx)
	}
	if cx = read(format.ReadQuery{Address: fmt.Sprintf("%s:%d", hit.Provenance.Path, hit.Provenance.LineNo)}); cx.Focus != "7" {
		t.Fatalf("by path: %+v", cx)
	}
	if cx = read(format.ReadQuery{Address: "0b7e2c1a-0000-4000-8000-000000000001"}); cx.Messages[0].ID != cx.Focus || len(cx.Messages) != 9 || cx.MoreBefore || cx.MoreAfter {
		t.Fatalf("session: %+v", cx)
	}
	for addr, want := range map[string]error{"0b7e2c1a/1": format.ErrBadRequest, "nosuch/1": format.ErrNotFound,
		hit.SessionID + "/12345": format.ErrNotFound, "/nowhere.jsonl:3": format.ErrNotFound, "9999": format.ErrNotFound} {
		if _, err := b.Read(ctx, format.ReadQuery{Address: addr}, format.Filters{}); !errors.Is(err, want) {
			t.Errorf("read %s: %v, want %v", addr, err, want)
		}
	}
	// Long text is cut at the budget and says which lines it shows.
	cx = read(format.ReadQuery{Address: hit.Address, MaxChars: 10})
	if m := cx.Messages[0]; m.Text != "tests/logi" || !m.Clipped || m.LineFrom != 1 || m.LineTo != 1 || m.Lines != 2 {
		t.Fatalf("cut: %+v", m)
	}
	if m := read(format.ReadQuery{Address: hit.Address, LineOffset: 2}).Messages[0]; m.Text != "exit code 1" || m.LineFrom != 2 {
		t.Fatalf("line offset: %+v", m)
	}
	// raw: the record's exact JSONL line.
	data, err := b.RawAt(ctx, hit.Address)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".claude/projects/-tmp-oracle-alpha/0b7e2c1a-0000-4000-8000-000000000001.jsonl")
	file, _ := os.ReadFile(path)
	lines := strings.SplitAfter(string(file), "\n")
	if string(data) != lines[8] {
		t.Fatalf("raw = %q\nwant %q", data, lines[8])
	}
	if _, err := b.Raw(ctx, "1", 2, 0, 10); !errors.Is(err, ErrNotLocal) {
		t.Fatalf("old generation without a server: %v", err)
	}
	// V8: file gone: the server serves its latest copy, found by this
	// device's path and file id, never by a text search.
	os.Remove(path)
	if _, err := b.RawAt(ctx, hit.Address); !errors.Is(err, ErrNotLocal) {
		t.Fatalf("gone without a server: %v", err)
	}
	remote := &fakeRemote{data: []byte(lines[8])}
	b.Remote = remote
	data, err = b.RawAt(ctx, hit.Address)
	if err != nil || string(data) != lines[8] || remote.got != path+"|-1|3980|605" || !strings.Contains(remote.fileID, ":") {
		t.Fatalf("server fallback: %q %v %+v", data, err, remote)
	}
}

type fakeRemote struct {
	data        []byte
	got, fileID string
}

func (f *fakeRemote) RawByPath(_ context.Context, path, fileID string, gen, off, n int64) ([]byte, error) {
	f.got, f.fileID = fmt.Sprintf("%s|%d|%d|%d", path, gen, off, n), fileID
	return f.data, nil
}

// addRows indexes extra messages into one new Claude session.
func addRows(t *testing.T, b *Backend, session string, msgs ...*transcript.Message) {
	t.Helper()
	st, err := b.Store.EnsureSource(ctx, transcript.Source{Agent: transcript.AgentClaude, Path: "/synthetic/" + session + ".jsonl",
		FileID: transcript.FileID{Dev: 9, Ino: 9}, StorageKind: transcript.StorageJSONLAppend, Parser: "claude@1"})
	if err != nil {
		t.Fatal(err)
	}
	batch := localindex.Batch{SourceID: st.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: session, Cwd: "/tmp/big"}}}
	base := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	for i, m := range msgs {
		m.SessionID, m.NativeID, m.Ordinal, m.TS, m.LineNo, m.ByteOffset, m.ByteLen, m.Parser = session, fmt.Sprintf("n%d", i), int64(i)<<12, base.Add(time.Duration(i)*time.Second), int64(i+1), int64(i)*100, 100, "claude@1"
		m.FullLen, m.ContentSHA = len(m.Text), sha256.Sum256([]byte(m.Text))
		batch.Messages = append(batch.Messages, m)
	}
	if _, err := b.Store.ApplyBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
}

// Ranked search puts prompts and replies above tool output that matches
// as well: the tool result here is newer and ties on bm25.
func TestSearchRanksConversationAboveToolOutput(t *testing.T) {
	b, _ := oracle(t)
	addRows(t, b, "rank-session",
		&transcript.Message{Kind: transcript.KindAssistant, Text: "zanzibar deploy ya"},
		&transcript.Message{Kind: transcript.KindToolResult, ToolName: "Bash", Text: "zanzibar deploy ok"})
	p, err := b.Search(ctx, format.SearchQuery{Query: "zanzibar deploy"}, format.Filters{})
	if err != nil || len(p.Hits) != 2 || p.Hits[0].Kind != "assistant" {
		t.Fatalf("ranking: %+v %v", p, err)
	}
	// bm25 really scores (a contentless token table at detail=column
	// scored every row 0, so "ranked" meant newest row first).
	if p.Hits[0].Score >= 0 || p.Hits[1].Score >= 0 {
		t.Fatalf("bm25 scores %v, %v", p.Hits[0].Score, p.Hits[1].Score)
	}
	addRows(t, b, "rank-session-2",
		&transcript.Message{Kind: transcript.KindAssistant, Text: "quokka"},
		&transcript.Message{Kind: transcript.KindAssistant, Text: "quokka " + strings.Repeat("filler words here ", 50)})
	if p, err = b.Search(ctx, format.SearchQuery{Query: "quokka"}, format.Filters{}); err != nil || len(p.Hits) != 2 || p.Hits[0].Snippet != "quokka" {
		t.Fatalf("bm25 prefers the short match: %+v %v", p, err)
	}
}

// Paging through a ranked search shows every hit once: the kind boost
// reorders the scored list, so a page must not depend on how long a list
// the query happened to read. Here 70 tool results outscore one assistant
// reply on raw bm25, but the boost puts the reply first.
func TestSearchPagesStableUnderKindBoost(t *testing.T) {
	b, _ := oracle(t)
	var msgs []*transcript.Message
	msgs = append(msgs, &transcript.Message{Kind: transcript.KindAssistant, Text: "kiwi a b c d"})
	for i := range 70 {
		msgs = append(msgs, &transcript.Message{Kind: transcript.KindToolResult, ToolName: "Bash", Text: fmt.Sprintf("kiwi a b t%d", i)})
	}
	addRows(t, b, "boost-pages", msgs...)
	// A bare word ranks candidates from the token index; a phrase ranks
	// the rows holding it.
	for _, query := range []string{"kiwi", `"kiwi a"`} {
		pagesOnce(t, b, query)
	}
}

func pagesOnce(t *testing.T, b *Backend, query string) {
	t.Helper()
	seen := map[string]int{}
	for off := 0; ; off += 20 {
		p, err := b.Search(ctx, format.SearchQuery{Query: query, Limit: 20, Offset: off}, format.Filters{})
		if err != nil {
			t.Fatal(err)
		}
		if off == 0 && (len(p.Hits) == 0 || p.Hits[0].Kind != "assistant") {
			t.Fatalf("%s: the reply does not rank first: %+v", query, p.Hits)
		}
		for _, h := range p.Hits {
			seen[h.Address]++
		}
		if len(p.Hits) < 20 || off > 200 {
			break
		}
	}
	for a, n := range seen {
		if n != 1 {
			t.Errorf("%s: %s shown %d times", query, a, n)
		}
	}
	if len(seen) != 71 {
		t.Fatalf("%s: pages show %d distinct hits, want 71", query, len(seen))
	}
}

// A ranked page whose hits exactly fill the limit offers no next page
// when nothing follows (the server reads one row past the page too).
func TestSearchNextOnlyWhenMore(t *testing.T) {
	b, _ := oracle(t)
	addRows(t, b, "next-page",
		&transcript.Message{Kind: transcript.KindAssistant, Text: "wombat one"},
		&transcript.Message{Kind: transcript.KindAssistant, Text: "wombat two"})
	p, err := b.Search(ctx, format.SearchQuery{Query: "wombat", Limit: 2}, format.Filters{})
	if err != nil || len(p.Hits) != 2 || p.Next != 0 {
		t.Fatalf("exact page: %d hits, next %d, %v", len(p.Hits), p.Next, err)
	}
	if p, err = b.Search(ctx, format.SearchQuery{Query: "wombat", Limit: 1}, format.Filters{}); err != nil || len(p.Hits) != 1 || p.Next != 1 {
		t.Fatalf("short page: %d hits, next %d, %v", len(p.Hits), p.Next, err)
	}
}

// Parsers will store tool output up to about 1MB a row: grep prints only
// the matching lines of such a row, clipped, and read windows it by line
// and character budget with line numbers.
func TestLargeToolResult(t *testing.T) {
	b, _ := oracle(t)
	var big strings.Builder
	for i := 1; big.Len() < 600<<10; i++ {
		if i == 5000 {
			fmt.Fprintf(&big, "line %d: panic: quetzal overflow in the middle %s\n", i, strings.Repeat("z", 2000))
			continue
		}
		fmt.Fprintf(&big, "line %d: ok upload chunk\n", i)
	}
	addRows(t, b, "big-session", &transcript.Message{Kind: transcript.KindToolResult, ToolName: "Bash", Text: big.String()})
	page := grepFor(t, b, pat("quetzal"), format.Filters{}, func(q *format.GrepQuery) { q.Before, q.After = 1, 1 })
	if len(page.Hits) != 1 {
		t.Fatalf("grep: %+v", page)
	}
	h := page.Hits[0]
	var text strings.Builder
	_ = format.WriteGrep(&text, page, format.ModeContent, format.Style{})
	if len(h.Lines) != 3 || h.Lines[1].N != 5000 || !h.Lines[1].Match || len(h.Lines[1].Text) > 400 || text.Len() > 1500 {
		t.Fatalf("grep printed %d bytes: %+v", text.Len(), h.Lines)
	}
	read := func(q format.ReadQuery) (*format.Context, string) {
		t.Helper()
		cx, err := b.Read(ctx, q, format.Filters{})
		if err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		_ = format.WriteRead(&out, cx, format.Style{})
		return cx, out.String()
	}
	cx, out := read(format.ReadQuery{Address: h.Address + ":5000"})
	m := cx.Messages[0]
	if m.LineFrom != 4995 || m.Lines < 20000 || !m.Clipped || len(m.Text) > format.DefaultMaxChars || len(out) > format.DefaultMaxChars+900 ||
		!strings.Contains(out, ">") || !regexp.MustCompile(`(?m)^> 5000  line 5000: panic`).MatchString(out) ||
		!strings.Contains(out, fmt.Sprintf("more: flopwire read %s --line-offset %d", h.Address, m.LineTo+1)) {
		t.Fatalf("read at a line (%d bytes): %+v\n%s", len(out), m, out)
	}
	// The addressed line is longer than a small budget: it is cut inside.
	cx, _ = read(format.ReadQuery{Address: h.Address, LineOffset: 5000, MaxChars: 100})
	if m = cx.Messages[0]; m.LineFrom != 5000 || m.LineTo != 5000 || len(m.Text) != 100 {
		t.Fatalf("cut line: %+v", m)
	}
	cx, _ = read(format.ReadQuery{Address: h.Address, LineOffset: 20000, MaxChars: 1000})
	if m = cx.Messages[0]; m.LineFrom != 20000 || !strings.HasPrefix(m.Text, "line 20000: ") {
		t.Fatalf("line offset: %+v", m)
	}
}

// Printed session prefixes stay unique when many sessions share a long
// prefix (Claude subagents: agent-a86…), more than any lookup window.
func TestAddressPrefixesAmongManyNeighbours(t *testing.T) {
	b, _ := oracle(t)
	for i := range 150 {
		addRows(t, b, fmt.Sprintf("agent-a86%03x%x", i*7, i), &transcript.Message{Kind: transcript.KindAssistant, Text: fmt.Sprintf("wombat %d", i)})
	}
	page := grepFor(t, b, pat("wombat"), format.Filters{}, func(q *format.GrepQuery) { q.Limit = 200 })
	if len(page.Hits) != 150 {
		t.Fatalf("%d hits", len(page.Hits))
	}
	for _, h := range page.Hits {
		cx, err := b.Read(ctx, format.ReadQuery{Address: h.Address}, format.Filters{})
		if err != nil || cx.Focus != h.MessageID {
			t.Fatalf("read %s: %v", h.Address, err)
		}
	}
}

// Sessions and outlines page by cursor: walking one entry at a time gives
// the single page's entries, in order, both sorts.
func TestCursorPaging(t *testing.T) {
	b, _ := oracle(t)
	for _, sort := range []string{format.SortNewest, format.SortOldest} {
		all, err := b.Sessions(ctx, "", "", format.Filters{Limit: 100, Sort: sort})
		if err != nil || all.HasMore || len(all.Sessions) < 3 {
			t.Fatalf("sessions: %v %+v", err, all)
		}
		var got []string
		cursor := ""
		for range 100 {
			page, err := b.Sessions(ctx, "", cursor, format.Filters{Limit: 1, Sort: sort})
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range page.Sessions {
				got = append(got, c.ID)
			}
			if !page.HasMore {
				break
			}
			cursor = page.Next
		}
		var want []string
		for _, c := range all.Sessions {
			want = append(want, c.ID)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: walked %v, want %v", sort, got, want)
		}
	}
	q := format.ReadQuery{Address: "0b7e2c1a-0000-4000-8000-000000000001", Outline: true}
	whole, err := b.Read(ctx, q, format.Filters{})
	if err != nil || whole.OutlineMore || len(whole.Outline) < 2 {
		t.Fatalf("outline: %v %+v", err, whole)
	}
	var got []format.OutlineEntry
	for q.Limit = 1; ; {
		page, err := b.Read(ctx, q, format.Filters{})
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, page.Outline...)
		if !page.OutlineMore {
			break
		}
		q.Cursor = page.OutlineNext
	}
	if fmt.Sprint(got) != fmt.Sprint(whole.Outline) {
		t.Fatalf("outline walk %+v\nwant %+v", got, whole.Outline)
	}
	for _, bad := range []string{"x", "1.y"} {
		if _, err := b.Sessions(ctx, "", bad, format.Filters{}); !errors.Is(err, format.ErrBadRequest) {
			t.Errorf("sessions cursor %q: %v", bad, err)
		}
		if _, err := b.Read(ctx, format.ReadQuery{Address: q.Address, Outline: true, Cursor: bad}, format.Filters{}); !errors.Is(err, format.ErrBadRequest) {
			t.Errorf("outline cursor %q: %v", bad, err)
		}
	}
}
