package format

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/digest"
	"github.com/flopwire/flopwire/internal/transcript"
)

// The header line is bounded whatever the session holds: a 50KB first
// prompt, 500 edited files, 30 PRs.
func TestHeaderIsBounded(t *testing.T) {
	msgs := []*transcript.Message{{Ordinal: 1, Kind: transcript.KindUser, Text: strings.Repeat("refactor the upload path ", 2000)}}
	for i := range 500 {
		msgs = append(msgs, &transcript.Message{Ordinal: int64(10 + i), Kind: transcript.KindToolCall, ToolName: "Edit", ToolCallID: fmt.Sprint(i),
			Text: fmt.Sprintf(`{"file_path":"/r/pkg/file%03d.go"}`, i)})
	}
	for i := range 30 {
		msgs = append(msgs, &transcript.Message{Ordinal: int64(1000 + i), Kind: transcript.KindAssistant, Text: fmt.Sprintf("see https://github.com/o/r/pull/%d", 100+i)})
	}
	b := digest.Update(nil, digest.Conv{RepoRoot: "/r"}, msgs, digest.Counts{Failed: 7})
	last := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	c := &ConversationInfo{Address: "0b7e2c1a", Agent: "claude", User: "gary@example.test", Device: "laptop", Repo: "/r",
		Branches: []string{"feat/" + strings.Repeat("long-branch-name-", 10)}, LastActivityAt: &last, Digest: ParseDigest(b)}
	h := header(c, last)
	if len(h) > MaxHeader+10 || !strings.Contains(h, `intent="refactor the upload path`) || !strings.Contains(h, " files=25+ ") ||
		!strings.Contains(h, " pr=#100 prs=10+ ") || !strings.Contains(h, " failed=7") || !strings.Contains(h, " ended=2026-09-23 ") {
		t.Fatalf("header (%d bytes): %s", len(h), h)
	}
}

func TestShortIntent(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Fix the parser. Then add tests for it and more.", "Fix the parser."},
		{"short one", "short one"},
		{"review the upload retry change and check that every call site uses the new backoff helper please", "review the upload retry change and check that every call site uses the new…"},
		{strings.Repeat("x", 200), strings.Repeat("x", 80) + "…"},
	} {
		if got := shortIntent(tc.in, 80); got != tc.want {
			t.Errorf("%q: %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestGroupedLayout(t *testing.T) {
	ts := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	recent := ts.Add(-4 * time.Minute)
	infoA := ConversationInfo{Address: "aaaa1111", SessionID: "aaaa1111-0000", Agent: "claude", Repo: "/x/flopwire", Branches: []string{"main"}, LastActivityAt: &recent, Live: true,
		Digest: &digest.Digest{Intent: "fix the flaky test", FilesEdited: []string{"a.go", "b.go"}, PRs: []string{"o/r#43", "o/r#44"}, Commits: []string{"abc1234"}, Failed: 2}}
	infoB := ConversationInfo{Address: "bbbb2222", SessionID: "bbbb2222-0000", Agent: "codex", Repo: "/x/other", LastActivityAt: &ts} // no digest yet
	hit := func(s, addr string, n int, kind, tool string) Hit {
		return Hit{Address: addr, SessionID: s, Agent: "claude", Kind: kind, ToolName: tool, TS: &ts, Lines: []Line{{N: n, Text: "match " + addr, Match: true}}}
	}
	p := &Page{Hits: []Hit{hit(infoA.SessionID, "aaaa1111/10", 1, "user", ""), hit(infoA.SessionID, "aaaa1111/20", 3, "tool_result", "Bash"), hit(infoB.SessionID, "bbbb2222/5", 2, "assistant", ""),
		hit(infoA.SessionID, "aaaa1111/30", 1, "assistant", "")}, Total: 4, TotalSessions: 2, Exact: true, SessionInfo: []ConversationInfo{infoA, infoB}}
	var b strings.Builder
	st := Style{Now: func() time.Time { return ts }}
	if err := WriteGrep(&b, p, ModeContent, st); err != nil {
		t.Fatal(err)
	}
	want := `## aaaa1111-0000 agent=claude live=4m repo=flopwire branch=main files=2 pr=#43 prs=2 commits=1 failed=2 intent="fix the flaky test"
10:1 user: match aaaa1111/10
20:3 tool_result/Bash: match aaaa1111/20
## bbbb2222-0000 agent=codex ended=2026-09-29 repo=other
5:2 assistant: match bbbb2222/5
## aaaa1111-0000
30:1 assistant: match aaaa1111/30
[4 hits in 2 sessions]
`
	if b.String() != want {
		t.Fatalf("grouped grep:\n%s\nwant:\n%s", b.String(), want)
	}
	// A page cut by the budget starts the next page with a full header.
	p.Offset, p.Hits = 1, p.Hits[1:]
	b.Reset()
	if err := WriteGrep(&b, p, ModeContent, Style{Now: st.Now, Budget: 200}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(b.String(), "## aaaa1111-0000 agent=claude live=4m") || !strings.Contains(b.String(), "next: --offset") {
		t.Fatalf("paged grouped grep:\n%s", b.String())
	}
	// Search groups the same way.
	for i := range p.Hits {
		p.Hits[i].Snippet, p.Hits[i].TextLine = "snippet", 2
	}
	b.Reset()
	if err := WriteSearch(&b, p, st); err != nil || !strings.Contains(b.String(), "## bbbb2222-0000 agent=codex ended=2026-09-29 repo=other\n5:2 assistant: snippet\n") {
		t.Fatalf("grouped search: %v\n%s", err, b.String())
	}
}

func TestOutlineRendering(t *testing.T) {
	ts := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	cx := &Context{Conversation: ConversationInfo{Address: "aaaa1111", SessionID: "aaaa1111-full", Agent: "claude", Repo: "/x/flopwire",
		Digest: &digest.Digest{Intent: "fix it", FilesEdited: []string{"a.go"}, PRs: []string{"o/r#43"}, Tokens: &digest.Tokens{Input: 1200, Output: 3_400_000}}},
		OutlineMore: true, OutlineNext: "page-end"}
	for i := range 40 {
		e := OutlineEntry{Address: fmt.Sprintf("aaaa1111/%d", i), ID: fmt.Sprintf("m%d", i), Ordinal: int64(i), TS: &ts, Kind: "tool_call", Tool: "Bash", Text: "go test ./..."}
		if i%10 == 0 {
			e.Kind, e.Tool, e.Text = "user", "", "run the tests"
		}
		if i == 3 {
			e.Error, e.Subagents = true, []string{"agent-1"}
		}
		cx.Outline = append(cx.Outline, e)
	}
	var b strings.Builder
	if err := WriteRead(&b, cx, Style{MCP: true, Budget: 900}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"# intent: \"fix it\"", "# files edited (1): a.go", "# PRs: o/r#43", "# tokens: input 1.2k, output 3.4M",
		"aaaa1111/0  2026-09-29 12:00Z  user: run the tests", "  aaaa1111/3  Bash(go test ./...)  error  → sub agent-1",
		"output budget of 900 bytes reached; next: flopwire_read address=aaaa1111 outline=true cursor="} {
		if !strings.Contains(out, want) {
			t.Errorf("outline lacks %q:\n%s", want, out)
		}
	}
	// The cursor is the last entry shown: its line is there, the next
	// entry's is not.
	if m := regexp.MustCompile(`outline: (\d+) entries shown, more follow;.* cursor=(\d+)\.m(\d+)\]`).FindStringSubmatch(out); m == nil || m[2] != m[3] ||
		!strings.Contains(out, "aaaa1111/"+m[2]+" ") || strings.Contains(out, fmt.Sprintf("aaaa1111/%d ", atoi(m[2])+1)) || m[1] != fmt.Sprint(atoi(m[2])+1) {
		t.Errorf("outline cursor is not after the last entry shown: %q\n%s", m, out)
	}
	if len(out) > 1100 {
		t.Errorf("outline is %d bytes under a 900-byte budget", len(out))
	}
}

// The header never cuts the session address, which read takes back with
// the hit's ORDINAL:LINE; other fields go first to keep the bound.
func TestHeaderKeepsTheAddress(t *testing.T) {
	last := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	d := &digest.Digest{Intent: "refactor the upload path", FilesEdited: make([]string, 25), FilesMore: true,
		PRs: []string{"o/r#10000", "o/r#10001"}, More: []string{"prs", "commits"}, Commits: make([]string, 20), Failed: 1234}
	addr := "devin-" + strings.Repeat("0123456789abcdef", 3) // 54 bytes
	c := &ConversationInfo{Address: addr, Agent: "claude", User: "someone.long@example-company.test", Device: "a-long-device-name",
		Repo: "/r/some-long-repository-name", Branches: []string{"feat/" + strings.Repeat("long-branch-", 8), "main"}, LastActivityAt: &last, Digest: d}
	h := header(c, last)
	if !strings.HasPrefix(h, "## "+addr+" ") || len(h) > MaxHeader+3 || !strings.Contains(h, " failed=1234") {
		t.Fatalf("header (%d bytes): %s", len(h), h)
	}
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// Every header leads with the full session id, not the short address
// (the decision on #55): grep and search headers, the short header that
// reopens a session, a header without a session description, grep -l
// rows and sessions --text rows. A short prefix unique today may not be
// tomorrow; read takes the full id back with the hit's ORDINAL:LINE.
func TestHeadersLeadWithTheFullSessionID(t *testing.T) {
	ts := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	a := ConversationInfo{Address: "aaaa1111", SessionID: "aaaa1111-0000-4000-8000-000000000001", Agent: "claude", LastActivityAt: &ts, Hits: 2}
	hit := func(s, addr string) Hit {
		return Hit{Address: addr, SessionID: s, Agent: "codex", Kind: "user", TS: &ts, Lines: []Line{{N: 1, Text: "x", Match: true}}}
	}
	b := "bbbb2222-0000-4000-8000-000000000002" // no session description
	p := &Page{Hits: []Hit{hit(a.SessionID, "aaaa1111/1"), hit(b, "bbbb2222/2"), hit(a.SessionID, "aaaa1111/3")}, Total: 3, TotalSessions: 2, Exact: true,
		SessionInfo: []ConversationInfo{a}}
	st := Style{Now: func() time.Time { return ts }}
	var out strings.Builder
	if err := WriteGrep(&out, p, ModeContent, st); err != nil {
		t.Fatal(err)
	}
	var heads []string
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(l, "## ") {
			heads = append(heads, strings.Fields(l)[1])
		}
	}
	if want := []string{a.SessionID, b, a.SessionID}; !slices.Equal(heads, want) {
		t.Fatalf("grep header ids %v, want %v:\n%s", heads, want, out.String())
	}
	out.Reset()
	if err := WriteGrep(&out, &Page{Sessions: []ConversationInfo{a}, Total: 2, TotalSessions: 1, Exact: true}, ModeSessions, st); err != nil || !strings.HasPrefix(out.String(), a.SessionID+" ") {
		t.Fatalf("grep -l: %v %s", err, out.String())
	}
	out.Reset()
	if err := WriteSessions(&out, &Sessions{Sessions: []ConversationInfo{a}}, st); err != nil || !strings.HasPrefix(out.String(), a.SessionID+" ") {
		t.Fatalf("sessions --text: %v %s", err, out.String())
	}
}
