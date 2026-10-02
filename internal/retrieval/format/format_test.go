package format

import (
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestFiltersRoundTrip(t *testing.T) {
	f := Filters{Agent: "codex", Repo: "/src/flopwire", RepoRoots: []string{"/src/flopwire", "/src/flopwire-wt, with a comma"}, Device: "laptop-a", User: "gary@example.test", Kinds: []string{"user", "tool_result"},
		ExcludeKinds: []string{"thinking"}, Tools: []string{"Bash", "exec_command"}, Session: "0b7e2c1a",
		Since: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Until: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC),
		ExcludeSubagents: true, IncludeSuperseded: true, IncludeBranches: true, ExcludeConversation: "c", ExcludeSession: "s", Limit: 7,
		Branch: "feat/*", Sort: SortOldest, ExcludeLive: true, Live: []string{"s1", "s2"}}
	got, err := ParseFilters(f.Values())
	if err != nil || !reflect.DeepEqual(got, f) {
		t.Fatalf("round trip:\n got %+v\nwant %+v (%v)", got, f, err)
	}
	now := time.Now()
	for s, want := range map[string]time.Time{"24h": now.Add(-24 * time.Hour), "7d": now.AddDate(0, 0, -7), "2w": now.AddDate(0, 0, -14),
		"2026-09-01": time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		// The form hits print, so an agent can paste it back.
		"2026-09-23 10:00Z":         time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC),
		"2026-09-23T10:00:00-04:00": time.Date(2026, 9, 23, 14, 0, 0, 0, time.UTC)} {
		if ts, err := ParseTime(s, now); err != nil || !ts.Equal(want) {
			t.Fatalf("time %q: %v %v", s, ts, err)
		}
	}
	if _, err := ParseFilters(map[string][]string{"limit": {"-1"}}); err == nil {
		t.Fatal("negative limit accepted")
	}
	if _, err := ParseFilters(map[string][]string{"repo_root": {"relative/x"}}); err == nil {
		t.Fatal("relative repo_root accepted")
	}
	if _, err := ParseFilters(map[string][]string{"repo_root": make([]string, MaxRepoRoots+1)}); err == nil {
		t.Fatal("too many repo_root values accepted")
	}
	q := GrepQuery{Pattern: "a|b", CaseSensitive: true, Mode: ModeCount, Offset: 3, Limit: 4, MaxPerSession: 2, Before: 1, After: 2, OnlyMatching: true, Multiline: true}
	if got, err := ParseGrepQuery(q.Values(f.Values())); err != nil || got != q {
		t.Fatalf("grep query round trip: %+v %v", got, err)
	}
	r := ReadQuery{Address: "abc/1:2", Before: 1, After: 2, MaxChars: 10, LineOffset: 3}
	if got, err := ParseReadQuery(r.Values(f.Values())); err != nil || got != r {
		t.Fatalf("read query round trip: %+v %v", got, err)
	}
	r = ReadQuery{Address: "abc", Outline: true, Cursor: "5.x", Limit: 9}
	if got, err := ParseReadQuery(r.Values(url.Values{})); err != nil || got != r {
		t.Fatalf("outline query round trip: %+v %v", got, err)
	}
	if _, err := ParseFilters(url.Values{"sort": {"sideways"}}); err == nil {
		t.Fatal("bad sort accepted")
	}
	for _, tc := range []struct{ tool, in, want string }{{"grep", "", SortNewest}, {"search", "", SortRelevance}, {"sessions", "", SortNewest}, {"search", "oldest", SortOldest}} {
		if got, err := SortFor(tc.tool, tc.in); err != nil || got != tc.want {
			t.Errorf("SortFor(%s, %q) = %q %v", tc.tool, tc.in, got, err)
		}
	}
	if _, err := SortFor("sessions", SortRelevance); err == nil {
		t.Fatal("sessions accepted relevance")
	}
	if got := BranchMatch(`feat/*_x%`); got != `feat/%\_x\%` {
		t.Fatalf("BranchMatch: %q", got)
	}
}

func TestAddresses(t *testing.T) {
	for in, want := range map[string]Addr{
		"0b7e2c1a/28672:14":         {Kind: AddrMessage, Session: "0b7e2c1a", Ordinal: 28672, Line: 14},
		"agent-a1/0":                {Kind: AddrMessage, Session: "agent-a1", Ordinal: 0},
		"0b7e2c1a":                  {Kind: AddrBare, Token: "0b7e2c1a"},
		"12345":                     {Kind: AddrBare, Token: "12345"},
		"/home/u/.claude/x.jsonl:8": {Kind: AddrPath, Path: "/home/u/.claude/x.jsonl", PathNo: 8},
		"~/x/y.jsonl:3":             {Kind: AddrPath, Path: "~/x/y.jsonl", PathNo: 3},
	} {
		if got, err := ParseAddress(in); err != nil || got != want {
			t.Errorf("%q: %+v %v", in, got, err)
		}
	}
	for _, in := range []string{"", "abc/x", "abc/1:0", "/p/x.jsonl", "/p/x.jsonl:y", "/1"} {
		if _, err := ParseAddress(in); err == nil {
			t.Errorf("%q parsed", in)
		}
	}
	if got := ShortPrefix("019a0000-0000-7000-8000-0000000000a1", []string{"019a0000-0000-7000-8000-0000000000a1", "019a0000-0000-7000-8000-0000000000a2"}); got != "019a0000-0000-7000-8000-0000000000a1" {
		t.Errorf("shared prefix: %s", got)
	}
	if got := ShortPrefix("01a0e857-e229", []string{"01a0e857-e229", "01a0e8ff"}); got != "01a0e857" {
		t.Errorf("unique at 8: %s", got)
	}
	if got := ShortPrefix("01a0e857-e229", []string{"01a0e857-f000"}); got != "01a0e857-e" {
		t.Errorf("extend: %s", got)
	}
	if got := ShortPrefix("abc", nil); got != "abc" {
		t.Errorf("short id: %s", got)
	}
}

func TestCutAndClean(t *testing.T) {
	text := "one\ntwo\nthree\nfour\n"
	if e := Cut(text, 0, 0); e.Text != "one\ntwo\nthree\nfour" || e.From != 1 || e.To != 4 || e.Lines != 4 || e.Clipped {
		t.Fatalf("whole: %+v", e)
	}
	if e := Cut(text, 2, 9); e.Text != "two\nthree" || e.From != 2 || e.To != 3 || !e.Clipped {
		t.Fatalf("window: %+v", e)
	}
	if e := Cut("héllo world", 1, 2); e.Text != "h" || !e.Clipped {
		t.Fatalf("long first line: %+v", e)
	}
	if e := Cut(text, 99, 0); e.From != 4 {
		t.Fatalf("past the end: %+v", e)
	}
	// D17: escape sequences are shown, not executed; tab and newline stay.
	in := "a\x1b[31mred\x1b]52;c;ZXZpbA==\x07\tb\r\nc\x7f\u009b‮\xff"
	if got := Clean(in); got != "a␛[31mred␛]52;c;ZXZpbA==␇\tb\nc␡���" {
		t.Fatalf("clean: %q", got)
	}
	// Every Unicode Bidi_Control character is neutralized: the marks
	// (U+061C, U+200E, U+200F), embeddings and overrides (U+202A-U+202E)
	// and isolates (U+2066-U+2069). Neighbouring characters stay.
	for _, r := range []rune{0x061c, 0x200e, 0x200f, 0x202a, 0x202b, 0x202c, 0x202d, 0x202e, 0x2066, 0x2067, 0x2068, 0x2069} {
		if got := Clean("x" + string(r) + "y"); got != "x\ufffdy" {
			t.Errorf("clean U+%04X: %q", r, got)
		}
	}
	for _, s := range []string{"\u200d", "\u2065", "\u2070", "\u061b", "\u200c", "\u2029"} {
		if got := Clean(s); got != s {
			t.Errorf("clean changed %q to %q", s, got)
		}
	}
	if Clean("plain text\n") != "plain text\n" {
		t.Fatal("clean changed plain text")
	}
	if got := ClipAround(strings.Repeat("a", 50)+"X"+strings.Repeat("b", 50), 50, 20); !strings.HasPrefix(got, "…") || !strings.Contains(got, "X") || !strings.HasSuffix(got, "…") {
		t.Fatalf("clip: %q", got)
	}
}

func TestQueryHelpers(t *testing.T) {
	w, p := SplitQuery(`retry "exponential backoff" in upload "unclosed phrase`)
	if strings.Join(w, ",") != "retry,in,upload" || strings.Join(p, ",") != "exponential backoff,unclosed phrase" {
		t.Fatalf("split: %q %q", w, p)
	}
	if got := DropStopwords([]string{"how", "did", "we", "handle", "the", "tokenizer?"}); strings.Join(got, ",") != "handle,tokenizer" {
		t.Fatalf("stopwords: %q", got)
	}
	if got := DropStopwords([]string{"the", "a"}); len(got) != 2 {
		t.Fatalf("all stopwords: %q", got)
	}
	for in, want := range map[string][2]string{
		"/src/flopwire/": {"/src/flopwire", ""},
		"flopwire":       {"", `%/flopwire`},
		"team*":          {"", `%/team%`},
		"/src/*/web":     {"", `/src/%/web`},
		"my_repo":        {"", `%/my\_repo`},
	} {
		if p, l := RepoMatch(in); p != want[0] || l != want[1] {
			t.Errorf("repo %q: %q %q", in, p, l)
		}
	}
	if GlobLike("flaky") != "%flaky%" || GlobLike("0b7e*") != "0b7e%" || GlobLike("") != "" {
		t.Fatal("glob")
	}
}

// D17: a session id comes from transcript data, so the addresses, headers
// and "sub of" parts that print it are cleaned like any other text.
func TestRenderCleansSessionIDs(t *testing.T) {
	evil := "sess\x1b]52;c;ZXZpbA==\x07"
	addr := MessageAddress(evil, 1)
	info := ConversationInfo{Address: evil, SessionID: evil, ParentSession: evil, Agent: "claude"}
	var b strings.Builder
	if err := WriteGrep(&b, &Page{Hits: []Hit{{Address: addr, SessionID: evil, Agent: "claude", Kind: "user", Lines: []Line{{N: 1, Text: "x", Match: true}}}}}, "", Style{}); err != nil {
		t.Fatal(err)
	}
	if err := WriteGrep(&b, &Page{Sessions: []ConversationInfo{info}}, ModeSessions, Style{}); err != nil {
		t.Fatal(err)
	}
	if err := WriteSessions(&b, &Sessions{Sessions: []ConversationInfo{info}}, Style{}); err != nil {
		t.Fatal(err)
	}
	if err := WriteRead(&b, &Context{Conversation: info, Messages: []Message{{Address: addr, Kind: "user", Text: "x"}}}, Style{}); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(b.String(), "\x1b\x07") {
		t.Fatalf("raw control characters: %q", b.String())
	}
}

// An answer to an agent stays under the budget: whole hits until the
// next would pass it, and the footer says the budget ended the page and
// gives the exact next offset. What qualifies the hits comes before them.
func TestBudgetEndsPageWithNextOffset(t *testing.T) {
	ts := time.Date(2026, 9, 29, 12, 8, 0, 0, time.UTC)
	p := &Page{Offset: 20, Next: 70, Total: 90, TotalSessions: 3, Exact: true, Notes: []string{"no message has every term"}, Excluded: "left out your own session s"}
	for i := range 50 {
		p.Hits = append(p.Hits, Hit{Address: fmt.Sprintf("sess/%d", i), Agent: "codex", Kind: "tool_result", TS: &ts, IsError: i == 0,
			Lines: []Line{{N: 1, Text: strings.Repeat("x", 300), Match: true}}, Snippet: strings.Repeat("y", 300)})
	}
	st := Style{MCP: true, Budget: 2000, Flat: true}
	var b strings.Builder
	if err := WriteGrep(&b, p, ModeContent, st); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	shown := strings.Count(out, "\nsess/") + strings.Count(out[:5], "sess/")
	if len(out) > 2000+300 || shown == 0 || shown >= 50 || lines[0] != "[no message has every term]" ||
		!strings.Contains(out, fmt.Sprintf("[showing 21-%d of 90 hits in 3 sessions; output budget of 2000 bytes reached; next: offset=%d]", 20+shown, 20+shown)) ||
		lines[len(lines)-1] != "[left out your own session s]" || !strings.Contains(lines[1], "2026-09-29 12:08Z") || !strings.Contains(lines[1], " error]") {
		t.Fatalf("grep under a budget (%d hits shown):\n%s", shown, out)
	}
	// No budget: every hit and the backend's next offset.
	b.Reset()
	if err := WriteGrep(&b, p, ModeContent, Style{MCP: true, Flat: true}); err != nil || !strings.Contains(b.String(), "[showing 21-70 of 90 hits in 3 sessions; next: offset=70]") {
		t.Fatalf("grep without a budget: %v\n%s", err, b.String())
	}
	b.Reset()
	if err := WriteSearch(&b, p, st); err != nil || len(b.String()) > 2300 || !regexp.MustCompile(`output budget of 2000 bytes reached; next: offset=\d+\]`).MatchString(b.String()) {
		t.Fatalf("search under a budget: %v\n%s", err, b.String())
	}
	// The budget cuts the sessions page; the next page starts after the
	// last session shown, not after the last one fetched.
	s := &Sessions{HasMore: true, Next: "fetched-page-end"}
	for i := range 400 {
		at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Add(-time.Duration(i) * time.Microsecond)
		s.Sessions = append(s.Sessions, ConversationInfo{Address: fmt.Sprintf("s%03d", i), ID: fmt.Sprintf("c%03d", i), LastActivityAt: &at,
			Agent: "claude", Title: strings.Repeat("t", 80), Messages: 3})
	}
	b.Reset()
	err := WriteSessions(&b, s, Style{Budget: 2000})
	m := regexp.MustCompile(`\[(\d+) sessions shown, more follow; output budget of 2000 bytes reached; next: --cursor (\S+)\]`).FindStringSubmatch(b.String())
	if err != nil || len(b.String()) > 2300 || m == nil {
		t.Fatalf("sessions under a budget: %v\n%s", err, b.String())
	}
	if shown, _ := strconv.Atoi(m[1]); shown == 0 || m[2] != SessionCursor(s.Sessions[shown-1]) || !strings.Contains(b.String(), s.Sessions[shown-1].Address) {
		t.Fatalf("sessions cursor %q is not after the last shown of %s:\n%s", m[2], m[1], b.String())
	}
	// Without the budget the page's own cursor is printed; the last page
	// says so.
	b.Reset()
	s.Sessions = s.Sessions[:2]
	if err := WriteSessions(&b, s, Style{MCP: true}); err != nil || !strings.Contains(b.String(), "[2 sessions shown, more follow; next: cursor=fetched-page-end]") {
		t.Fatalf("sessions page: %v\n%s", err, b.String())
	}
	b.Reset()
	s.HasMore, s.Next = false, ""
	if err := WriteSessions(&b, s, Style{}); err != nil || !strings.Contains(b.String(), "[2 sessions, end of list]") {
		t.Fatalf("last sessions page: %v\n%s", err, b.String())
	}
}

// read under a budget keeps the focus and its nearest neighbours, and the
// hints name the address to go on from, as a call an agent can make.
func TestReadBudgetKeepsFocus(t *testing.T) {
	cx := &Context{Focus: "m5", Conversation: ConversationInfo{Agent: "claude", SessionID: "sess", Messages: 11}}
	for i := range 11 {
		cx.Messages = append(cx.Messages, Message{ID: fmt.Sprintf("m%d", i), Address: fmt.Sprintf("sess/%d", i), Kind: "assistant", Text: strings.Repeat("z", 400)})
	}
	var b strings.Builder
	if err := WriteRead(&b, cx, Style{MCP: true, Budget: 1700}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, ">> sess/5") || strings.Contains(out, "sess/0 ") || strings.Contains(out, "sess/10 ") ||
		!regexp.MustCompile(`\[earlier messages: showing [12] of 5 messages before; output budget of 1700 bytes reached; next: flopwire_read address=sess/([34]) messages_before=([34])\]`).MatchString(out) ||
		!regexp.MustCompile(`\[later messages: showing [12] of 5 messages after; output budget of 1700 bytes reached; next: flopwire_read address=sess/([67]) messages_after=([34])\]`).MatchString(out) || strings.Contains(out, "sess/0\n") {
		t.Fatalf("read under a budget:\n%s", out)
	}
}

// read's budget counts what the focus renders, line-number prefixes and
// clip hint included: many short lines, or one long line, never render
// past it, and the hint reads on from the first line left out.
func TestReadBudgetCountsLinePrefixes(t *testing.T) {
	for _, c := range []struct {
		name, text string
		budget     int
	}{
		{"short lines", strings.Repeat("x\n", 12000), MaxOutput},
		{"one long line", strings.Repeat("é", 15000), 2000},
	} {
		lines := strings.Count(c.text, "\n") + 1
		cx := &Context{Focus: "m1", Conversation: ConversationInfo{Agent: "claude", SessionID: "sess", Messages: 3}}
		for i := range 3 {
			m := Message{ID: fmt.Sprintf("m%d", i), Address: fmt.Sprintf("sess/%d", i), Kind: "assistant", Text: "neighbour"}
			if i == 1 {
				m.Text, m.LineFrom, m.LineTo, m.Lines = c.text, 1, lines, lines
			}
			cx.Messages = append(cx.Messages, m)
		}
		var b strings.Builder
		if err := WriteRead(&b, cx, Style{MCP: true, Budget: c.budget}); err != nil {
			t.Fatal(err)
		}
		out := b.String()
		m := regexp.MustCompile(`\[lines 1-(\d+) of \d+; more: flopwire_read address=sess/1 line_offset=(\d+)\]`).FindStringSubmatch(out)
		if len(out) > c.budget || !strings.Contains(out, ">> sess/1") || !utf8.ValidString(out) || !strings.Contains(out, "[lines 1-") {
			t.Fatalf("%s: %d bytes, budget %d:\n%.400s", c.name, len(out), c.budget, out)
		}
		if lines == 1 {
			continue // a single line is cut inside, as Cut does
		}
		if m == nil {
			t.Fatalf("%s: no read-on hint:\n%.400s", c.name, out)
		}
		to, _ := strconv.Atoi(m[1])
		next, _ := strconv.Atoi(m[2])
		if next != to+1 || !strings.Contains(out, fmt.Sprintf("%5d  ", to)) {
			t.Fatalf("%s: shows to %d, reads on at %d", c.name, to, next)
		}
	}
}

// When the budget leaves out neighbours read fetched, the hint says how
// many it shows of how many and gives the exact call that reads the rest
// from the last one shown (agent-ux §7: 2 of 150 came back silently).
func TestReadBudgetSaysNeighboursCut(t *testing.T) {
	cx := &Context{Focus: "m0", Conversation: ConversationInfo{Agent: "codex", SessionID: "sess", Messages: 151}, MoreAfter: true}
	for i := range 151 {
		n := 400
		if i == 0 {
			n = 20000 // a long system prompt first
		}
		cx.Messages = append(cx.Messages, Message{ID: fmt.Sprintf("m%d", i), Address: fmt.Sprintf("sess/%d", i), Kind: "assistant", Text: strings.Repeat("z", n)})
	}
	for _, c := range []struct {
		st   Style
		want string
	}{
		{Style{MCP: true, Budget: MaxOutput}, `\[later messages: showing (\d+) of 150 messages after; output budget of 24000 bytes reached; next: flopwire_read address=sess/(\d+) messages_after=(\d+)\]\n$`},
		{Style{Budget: MaxOutput}, `\[later messages: showing (\d+) of 150 messages after; output budget of 24000 bytes reached; next: flopwire read sess/(\d+) --messages-after (\d+)\]\n$`},
	} {
		var b strings.Builder
		if err := WriteRead(&b, cx, c.st); err != nil {
			t.Fatal(err)
		}
		out := b.String()
		m := regexp.MustCompile(c.want).FindStringSubmatch(out)
		if m == nil || len(out) > MaxOutput {
			t.Fatalf("cut neighbours (%d bytes):\n%s", len(out), out[max(len(out)-400, 0):])
		}
		shown, _ := strconv.Atoi(m[1])
		last, _ := strconv.Atoi(m[2])
		rest, _ := strconv.Atoi(m[3])
		if shown < 1 || shown >= 150 || last != shown || rest != 150-shown || strings.Count(out, "\n   sess/") != shown || strings.Contains(out, "earlier messages") {
			t.Fatalf("shown %d, last sess/%d, rest %d:\n%s", shown, last, rest, out[max(len(out)-400, 0):])
		}
	}
	// Both sides cut: each says so.
	cx.Focus = "m75"
	cx.Messages[0].Text = "short"
	var b strings.Builder
	if err := WriteRead(&b, cx, Style{MCP: true, Budget: 6000}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !regexp.MustCompile(`^# .*\n   \[earlier messages: showing (\d+) of 75 messages before; output budget of 6000 bytes reached; next: flopwire_read address=sess/\d+ messages_before=\d+\]\n`).MatchString(out) ||
		!regexp.MustCompile(`\[later messages: showing \d+ of 75 messages after; output budget of 6000 bytes reached; next: flopwire_read address=sess/\d+ messages_after=\d+\]\n$`).MatchString(out) || len(out) > 6000 {
		t.Fatalf("both sides cut (%d bytes):\n%s", len(out), out)
	}
	// Nothing cut: the plain hint, no count.
	cx.Messages = cx.Messages[70:81]
	b.Reset()
	if err := WriteRead(&b, cx, Style{MCP: true, Budget: MaxOutput}); err != nil {
		t.Fatal(err)
	}
	if out := b.String(); !strings.Contains(out, "   [later messages: flopwire_read address=sess/80 messages_after=10]\n") || strings.Contains(out, "showing") {
		t.Fatalf("nothing cut:\n%s", out)
	}
}
