package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/digest"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/transcript"
)

// sessions answers JSON by default; it is main's sessions --json, field
// for field, with the kind and the paging fields always present.
func TestSessionsJSONKeepsMainFields(t *testing.T) {
	read := func(path string) (format.Sessions, map[string]any) {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var s format.Sessions
		var m map[string]any
		if json.Unmarshal(b, &s) != nil || json.Unmarshal(b, &m) != nil {
			t.Fatalf("%s is not JSON", path)
		}
		return s, m
	}
	main, _ := read("testdata/sessions_json_main.json")
	main.Scope = &format.Scope{Kind: "local"}
	now, raw := read("testdata/golden/sessions_json.txt")
	if !reflect.DeepEqual(main, now) || len(now.Sessions) == 0 {
		t.Fatalf("sessions JSON differs from main's --json:\nmain %+v\nnow  %+v", main, now)
	}
	if raw["kind"] != "sessions" || raw["has_more"] != false {
		t.Fatalf("kind and has_more: %v %v", raw["kind"], raw["has_more"])
	}
	b, _ := os.ReadFile("testdata/golden/sessions_json.txt")
	if d, _ := os.ReadFile("testdata/golden/sessions_glob.txt"); strings.Count(string(b), "\n") != 1 || !strings.HasPrefix(string(d), `{"scope":{"kind":"local"},"kind":"sessions",`) {
		t.Fatalf("sessions JSON is not one compact line, or not the default")
	}
	for _, c := range now.Sessions {
		if len(c.SessionID) != 36 && !strings.HasPrefix(c.SessionID, "agent-") {
			t.Errorf("session_id %q is not a full id", c.SessionID)
		}
	}
}

// --json and --text are accepted by all seven agent-facing verbs; each is
// a no-op where it is already the default, and --text wins over --json.
func TestJSONAndTextFlagsOnEveryVerb(t *testing.T) {
	for _, verb := range []string{"grep", "search", "sessions", "read", "peers", "send", "inbox"} {
		for _, f := range []string{"--json", "--text"} {
			if o, err := parseArgs(verb, []string{f, "x"}); err != nil || !o.on[f[2:]] {
				t.Errorf("%s %s: %v", verb, f, err)
			}
		}
	}
	oracleIndex(t)
	cli := func(args ...string) string {
		t.Helper()
		return withoutCoverageObservation(captureStdout(t, func() error { return run(t.Context(), args) }))
	}
	if a, b := cli("sessions"), cli("sessions", "--json"); a != b || !strings.HasPrefix(a, `{"scope":{"kind":"local"},"kind":"sessions",`) {
		t.Fatalf("sessions --json is not the default:\n%s\n%s", a, b)
	}
	if out := cli("sessions", "--text", "--json"); !strings.HasPrefix(out, "[scope: local device]\n0b7e2c1a-0000-4000-8000-000000000002 agent=claude ") {
		t.Fatalf("sessions --text --json: %s", out)
	}
	if a, b := cli("grep", "retr"), cli("grep", "retr", "--text"); a != b || !strings.HasPrefix(a, "[scope: local device]\n## 0b7e2c1a-") {
		t.Fatalf("grep --text is not the default:\n%s\n%s", a, b)
	}
	if out := cli("search", "backoff", "--json", "--text"); !strings.HasPrefix(out, "[scope: local device]\n## 0b7e2c1a-") {
		t.Fatalf("search --json --text: %s", out)
	}
}

// In JSON mode (sessions by default, any retrieval verb with --json) a
// failure is one JSON object on stderr with a stable code, a fix and an
// example, and nothing on stdout; in text mode it is the one-line error.
// Over MCP the same holds for the call's isError text.
func TestRetrievalJSONErrors(t *testing.T) {
	oracleIndex(t)
	cmd := func(args ...string) (string, string, error) {
		var out, stderr strings.Builder
		err := toolCmdIO(t.Context(), args[0], args[1:], &out, &stderr)
		return out.String(), stderr.String(), err
	}
	for _, c := range []struct {
		args []string
		code string
	}{
		{[]string{"sessions", "--sort", "relevance"}, busproto.CodeBadRequest},
		{[]string{"sessions", "--bogus"}, busproto.CodeBadRequest},
		{[]string{"sessions", "--limit", "x"}, busproto.CodeBadRequest},
		{[]string{"sessions", "a", "b"}, busproto.CodeBadRequest},
		{[]string{"read", "--json", "nosuchsession/1"}, busproto.CodeNotFound},
		{[]string{"grep", "--json", "("}, busproto.CodeBadRequest},
		{[]string{"grep", "--json"}, busproto.CodeBadRequest},
	} {
		out, stderr, err := cmd(c.args...)
		if e := jsonErr(t, stderr, err); e.Code != c.code || e.Detail == "" || e.Fix == "" || e.Example == "" || out != "" || strings.Contains(e.Detail, "retrieval:") {
			t.Errorf("%v: %+v %q", c.args, e, out)
		}
	}
	for _, args := range [][]string{{"sessions", "--text", "--sort", "relevance"}, {"sessions", "--text", "--bogus"}, {"grep", "("}, {"read", "nosuchsession/1"}} {
		_, stderr, err := cmd(args...)
		if err == nil || errors.Is(err, errReported) || strings.HasPrefix(stderr, "{") || strings.HasPrefix(err.Error(), "{") {
			t.Errorf("%v: text error: %v %q", args, err, stderr)
		}
	}

	r, err := openRetriever(false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	for _, c := range []struct {
		tool string
		args string
		code string // "" for a text error
	}{
		{"flopwire_sessions", `{"bogus":1}`, busproto.CodeBadRequest},
		{"flopwire_sessions", `{"sort":"relevance"}`, busproto.CodeBadRequest},
		{"flopwire_sessions", `{"sort":"relevance","format":"text"}`, ""},
		{"flopwire_read", `{"address":"nosuchsession/1"}`, ""},
		{"flopwire_read", `{"address":"nosuchsession/1","format":"json"}`, busproto.CodeNotFound},
		{"flopwire_grep", `{"pattern":"(","format":"json"}`, busproto.CodeBadRequest},
		{"flopwire_grep", `{"pattern":"("}`, ""},
	} {
		text, isErr, structured := mcpContent(t, mcpRoundTrip(t, r, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+c.tool+`","arguments":`+c.args+`}}`))
		var e errorJSON
		switch {
		case !isErr || structured != nil:
			t.Errorf("%s %s: not an error result: %q", c.tool, c.args, text)
		case c.code == "" && strings.HasPrefix(text, "{"):
			t.Errorf("%s %s: want a text error: %s", c.tool, c.args, text)
		case c.code != "" && (json.Unmarshal([]byte(text), &e) != nil || e.Kind != "error" || e.Error.Code != c.code || e.Error.Example == "" || strings.Contains(e.Error.Example, "flopwire sessions")):
			t.Errorf("%s %s: want a JSON %s error: %s", c.tool, c.args, c.code, text)
		}
	}
}

// No tool declares an outputSchema, and every result is one text block
// with no structuredContent: Claude Code shows a model only
// structuredContent when a result has it, and Codex shows both (#84). In
// JSON mode (sessions by default, format="json" elsewhere) the text is
// one compact JSON document with the named fields; in text mode it is
// not JSON. read raw=true answers the record's bytes whatever format says.
func TestMCPRetrievalOneTextBlock(t *testing.T) {
	for _, tl := range mcpTools() {
		tm := tl.(map[string]any)
		if _, ok := tm["outputSchema"]; ok {
			t.Errorf("%s declares an outputSchema", tm["name"])
		}
	}
	oracleIndex(t)
	r, err := openRetriever(false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	for _, c := range []struct {
		tool, args string
		fields     []string // a JSON answer's fields; nil for text
	}{
		{"flopwire_grep", `{"pattern":"retr","include_self":true}`, nil},
		{"flopwire_grep", `{"pattern":"retr","output_mode":"sessions"}`, nil},
		{"flopwire_grep", `{"pattern":"chi","output_mode":"count","format":"json"}`, []string{"kind", "hits", "sessions", "total"}},
		{"flopwire_grep", `{"pattern":"exit","context":2,"format":"json","include_self":true}`, []string{"kind", "hits", "session_info", "total"}},
		{"flopwire_search", `{"query":"retry backoff"}`, nil},
		{"flopwire_search", `{"query":"timers","format":"json"}`, []string{"kind", "hits"}},
		{"flopwire_sessions", `{"include_self":true}`, []string{"kind", "sessions", "has_more"}},
		{"flopwire_sessions", `{"include_self":true,"detail":true}`, []string{"kind", "sessions", "has_more"}},
		{"flopwire_sessions", `{"branch":"fix/*","format":"text"}`, nil},
		{"flopwire_read", `{"address":"0b7e2c1a-0000-4000-8000-000000000001/13578240:1","messages_before":1,"messages_after":1}`, nil},
		{"flopwire_read", `{"address":"0b7e2c1a-0000-4000-8000-000000000002","outline":true,"format":"json"}`, []string{"kind", "conversation", "outline"}},
		{"flopwire_read", `{"address":"0b7e2c1a-0000-4000-8000-000000000001/13578240","raw":true,"format":"json"}`, []string{"uuid", "sessionId"}},
	} {
		resp := mcpRoundTrip(t, r, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+c.tool+`","arguments":`+c.args+`}}`)
		text, isErr, structured := mcpContent(t, resp)
		if isErr || structured != nil || strings.Contains(resp, "structuredContent") {
			t.Fatalf("%s %s: %s", c.tool, c.args, resp)
		}
		if len(text) > format.MaxOutput {
			t.Errorf("%s %s: %d bytes, over the budget", c.tool, c.args, len(text))
		}
		var obj map[string]any
		dec := json.NewDecoder(strings.NewReader(text))
		isJSON := dec.Decode(&obj) == nil && !dec.More()
		if isJSON != (c.fields != nil) {
			t.Errorf("%s %s: JSON %v, want %v:\n%.300s", c.tool, c.args, isJSON, c.fields != nil, text)
		}
		for _, k := range c.fields {
			if _, ok := obj[k]; !ok {
				t.Errorf("%s %s: no %q in %.300s", c.tool, c.args, k, text)
			}
		}
	}
}

// bigBackend answers every call with an answer far past the budget.
type bigBackend struct {
	page     *format.Page
	sessions *format.Sessions
	cx       *format.Context
	raw      []byte
}

func (b *bigBackend) Grep(context.Context, format.GrepQuery, format.Filters) (*format.Page, error) {
	p := *b.page
	return &p, nil
}
func (b *bigBackend) Search(context.Context, format.SearchQuery, format.Filters) (*format.Page, error) {
	p := *b.page
	return &p, nil
}
func (b *bigBackend) Sessions(context.Context, string, string, format.Filters) (*format.Sessions, error) {
	s := *b.sessions
	return &s, nil
}
func (b *bigBackend) Read(context.Context, format.ReadQuery, format.Filters) (*format.Context, error) {
	c := *b.cx
	return &c, nil
}
func (b *bigBackend) RawAt(context.Context, string) ([]byte, error) { return b.raw, nil }
func (b *bigBackend) Raw(context.Context, string, int64, int64, int64) ([]byte, error) {
	return b.raw, nil
}

// JSON answers (format="json", and sessions) stay within the budget in
// whole objects, whatever
// the backend returns, and its fields say where the next page starts: the
// offset after the last hit kept, the cursor after the last session or
// outline entry kept, the line_offset after the last line of a focus that
// alone passes the budget.
func TestJSONAnswerBudget(t *testing.T) {
	ts := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	call := func(b *bigBackend, tool, args string) map[string]any {
		t.Helper()
		r := &retriever{backend: b}
		// The text answer stays within the budget too: its entries do; the
		// notes and footer around them may pass it a little (format's
		// tests allow the same), and one hit past the budget is not cut
		// (format's units keep the first whole), a deferred item.
		resp := mcpRoundTrip(t, r, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tool+`","arguments":`+withFormat(args, "text")+`}}`)
		if text, isErr, _ := mcpContent(t, resp); isErr || len(text) > format.MaxOutput+500 && !strings.Contains(args, `"z"`) {
			t.Fatalf("%s %s text: %d bytes", tool, args, len(text))
		}
		resp = mcpRoundTrip(t, r, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tool+`","arguments":`+withFormat(args, "json")+`}}`)
		text, isErr, structured := mcpContent(t, resp)
		var s map[string]any
		if isErr || structured != nil || json.Unmarshal([]byte(text), &s) != nil {
			t.Fatalf("%s: %.300s", tool, resp)
		}
		if len(text) > format.MaxOutput {
			t.Fatalf("%s %s: %d bytes", tool, args, len(text))
		}
		return s
	}
	info := func(i int) format.ConversationInfo {
		at := ts.Add(-time.Duration(i) * time.Minute)
		return format.ConversationInfo{Address: fmt.Sprintf("s%03d", i), ID: fmt.Sprintf("c%03d", i), SessionID: fmt.Sprintf("s%03d-full", i), Agent: "claude",
			Title: strings.Repeat("t", 300), LastActivityAt: &at, Messages: 3}
	}

	// grep and search: whole hits, the next offset after the last kept,
	// and only the kept hits' sessions described.
	page := &format.Page{Offset: 40, Total: 900, TotalSessions: 50, Exact: true, Next: 540}
	for i := range 500 {
		var lines []format.Line
		for n := range 10 {
			lines = append(lines, format.Line{N: n + 1, Text: strings.Repeat("x", 300), Match: true})
		}
		page.Hits = append(page.Hits, format.Hit{Address: fmt.Sprintf("s%03d/%d", i%50, i), SessionID: fmt.Sprintf("s%03d-full", i%50), Agent: "codex", Kind: "tool_result",
			TS: &ts, Lines: lines, Snippet: strings.Repeat("y", 300), Score: 1.5})
	}
	for i := range 50 {
		page.SessionInfo = append(page.SessionInfo, info(i))
	}
	for tool, args := range map[string]string{"flopwire_grep": `{"pattern":"x"}`, "flopwire_search": `{"query":"x"}`} {
		s := call(&bigBackend{page: page}, tool, args)
		hits := s["hits"].([]any)
		if len(hits) == 0 || len(hits) >= 500 || s["next_offset"] != float64(40+len(hits)) || !strings.Contains(s["hint"].(string), fmt.Sprintf("next: offset=%d", 40+len(hits))) {
			t.Fatalf("%s: %d hits, next %v, hint %v", tool, len(hits), s["next_offset"], s["hint"])
		}
		kept := map[string]bool{}
		for _, h := range hits {
			kept[h.(map[string]any)["session_id"].(string)] = true
		}
		for _, c := range s["session_info"].([]any) {
			if !kept[c.(map[string]any)["session_id"].(string)] {
				t.Fatalf("%s: session_info describes a session with no hit kept", tool)
			}
		}
		if len(s["session_info"].([]any)) != len(kept) {
			t.Fatalf("%s: session_info %d, sessions kept %d", tool, len(s["session_info"].([]any)), len(kept))
		}
	}
	// One hit alone past the budget keeps its first lines and counts the
	// matching lines left out.
	var lines []format.Line
	for n := range 400 {
		lines = append(lines, format.Line{N: n + 1, Text: strings.Repeat("z", 290), Match: n%2 == 0})
	}
	one := &format.Page{Hits: []format.Hit{{Address: "s/1", SessionID: "s", Agent: "claude", Kind: "user", Lines: lines, MoreLines: 3}}, Total: 1, Exact: true}
	s := call(&bigBackend{page: one}, "flopwire_grep", `{"pattern":"z"}`)
	h := s["hits"].([]any)[0].(map[string]any)
	kept := h["lines"].([]any)
	left := 0
	for _, l := range lines[len(kept):] {
		if l.Match {
			left++
		}
	}
	if len(kept) == 0 || len(kept) >= 400 || h["more_lines"] != float64(3+left) || !strings.Contains(s["hint"].(string), "flopwire_read address=s/1") {
		t.Fatalf("one big hit: %d lines kept, more_lines %v, hint %v", len(kept), h["more_lines"], s["hint"])
	}
	// grep's sessions mode: whole sessions, the next offset.
	ls := &format.Page{Total: 2000, TotalSessions: 400, Exact: true}
	for i := range 400 {
		c := info(i)
		c.Hits = 5
		ls.Sessions = append(ls.Sessions, c)
	}
	s = call(&bigBackend{page: ls}, "flopwire_grep", `{"pattern":"x","output_mode":"sessions"}`)
	if n := len(s["sessions"].([]any)); n == 0 || n >= 400 || s["next_offset"] != float64(n) {
		t.Fatalf("grep sessions mode: %d sessions, next %v", n, s["next_offset"])
	}

	// sessions: whole sessions, the cursor after the last one kept.
	ss := &format.Sessions{HasMore: true, Next: "fetched-page-end"}
	for i := range 400 {
		ss.Sessions = append(ss.Sessions, info(i))
	}
	s = call(&bigBackend{sessions: ss}, "flopwire_sessions", `{}`)
	got := s["sessions"].([]any)
	if len(got) == 0 || len(got) >= 400 || s["has_more"] != true || s["next_cursor"] != format.SessionCursor(ss.Sessions[len(got)-1]) ||
		!strings.Contains(s["hint"].(string), "output budget of 24000 bytes reached; next: cursor=") {
		t.Fatalf("sessions: %d kept, has_more %v, next %v, hint %v", len(got), s["has_more"], s["next_cursor"], s["hint"])
	}
	// The concise default: a page of 20 busy sessions (20 commits, 25
	// files, long title and intent, every digest list full) fits the
	// budget whole, with room to spare; --detail does not.
	busy := &format.Sessions{}
	for i := range 20 {
		c := info(i)
		c.SessionID, c.Repo, c.User, c.Branches = fmt.Sprintf("%08x-0000-4000-8000-000000000000", i), "/src/some-repository", "someone@example.test", []string{"feat/a-long-branch-name", "main"}
		d := &digest.Digest{Intent: strings.Repeat("i", 300), Failed: 3, Last: strings.Repeat("l", 160), Tools: map[string]int{"Bash": 40, "Edit": 30, "Read": 90, "Grep": 12}}
		for k := range 25 {
			d.FilesEdited = append(d.FilesEdited, fmt.Sprintf("internal/pkg%02d/file.go", k))
		}
		for k := range 20 {
			d.Commits = append(d.Commits, fmt.Sprintf("%07x", k*977))
		}
		for k := range 10 {
			d.PRs = append(d.PRs, fmt.Sprintf("org/repo#%d", 100+k))
		}
		c.Digest = d
		busy.Sessions = append(busy.Sessions, c)
	}
	s = call(&bigBackend{sessions: busy}, "flopwire_sessions", `{}`)
	if n := len(s["sessions"].([]any)); n != 20 || s["has_more"] != false || jsonSize(s) > 20000 {
		t.Fatalf("20 busy sessions: %d kept, %d bytes", n, jsonSize(s))
	}
	t.Logf("20 busy sessions, concise: %d bytes", jsonSize(s))
	if s = call(&bigBackend{sessions: busy}, "flopwire_sessions", `{"detail":true}`); len(s["sessions"].([]any)) == 20 {
		t.Fatalf("20 busy sessions with detail fit the budget: %d bytes", jsonSize(s))
	}
	// Under the budget the backend's own cursor stays, and the hint says
	// how to use it.
	small := &format.Sessions{HasMore: true, Next: "fetched-page-end", Sessions: ss.Sessions[:2]}
	if s = call(&bigBackend{sessions: small}, "flopwire_sessions", `{}`); s["next_cursor"] != "fetched-page-end" || s["has_more"] != true ||
		s["hint"] != "pass next_cursor as cursor=C for the next page" {
		t.Fatalf("small sessions page: %v", s)
	}

	// read: the focus and its nearest neighbours.
	cx := &format.Context{Focus: "m20", Conversation: info(0)}
	for i := range 41 {
		cx.Messages = append(cx.Messages, format.Message{ID: fmt.Sprintf("m%d", i), Address: fmt.Sprintf("s000/%d", i), Kind: "assistant", Text: strings.Repeat("w", 3000), TextLen: 3000})
	}
	s = call(&bigBackend{cx: cx}, "flopwire_read", `{"address":"s000/20","messages_before":20,"messages_after":20}`)
	var ids []string
	for _, m := range s["messages"].([]any) {
		ids = append(ids, m.(map[string]any)["id"].(string))
	}
	if !slices.Contains(ids, "m20") || len(ids) >= 41 || len(ids) < 3 || !slices.Contains(ids, "m19") || !slices.Contains(ids, "m21") ||
		s["more_before"] != true || s["more_after"] != true || !regexp.MustCompile(`^output budget of 24000 bytes reached; showing \d+ of 20 messages before, next: flopwire_read address=s000/\d+ messages_before=\d+; showing \d+ of 20 messages after, next: flopwire_read address=s000/\d+ messages_after=\d+$`).MatchString(s["hint"].(string)) {
		t.Fatalf("read neighbours: %v %v %v %v", ids, s["more_before"], s["more_after"], s["hint"])
	}
	// A focus that alone passes the budget keeps its first lines and says
	// where to read on; one long line is cut at a character boundary.
	for _, text := range []string{strings.Repeat("line of text\n", 5000), strings.Repeat("é", 30000)} {
		lines := strings.Count(text, "\n") + 1
		big := &format.Context{Focus: "m0", Conversation: info(0), Messages: []format.Message{{ID: "m0", Address: "s000/0", Kind: "user", Text: text, TextLen: len(text),
			Lines: lines, LineFrom: 1, LineTo: lines}}}
		s = call(&bigBackend{cx: big}, "flopwire_read", `{"address":"s000/0"}`)
		m := s["messages"].([]any)[0].(map[string]any)
		if m["clipped"] != true || !utf8.ValidString(m["text"].(string)) || len(m["text"].(string)) == 0 || len(m["text"].(string)) >= len(text) {
			t.Fatalf("big focus: %v %v", m["clipped"], len(m["text"].(string)))
		}
		if lines > 1 && (m["line_to"] != float64(strings.Count(m["text"].(string), "\n")+1) || !strings.Contains(s["hint"].(string), fmt.Sprintf("line_offset=%d", int(m["line_to"].(float64))+1))) {
			t.Fatalf("big focus lines: line_to %v, hint %v", m["line_to"], s["hint"])
		}
	}
	// An outline: whole entries, the cursor after the last one kept.
	ol := &format.Context{Conversation: info(0), Outline: []format.OutlineEntry{}}
	for i := range 2000 {
		ol.Outline = append(ol.Outline, format.OutlineEntry{Address: fmt.Sprintf("s000/%d", i), ID: fmt.Sprintf("m%d", i), Ordinal: int64(i), Kind: "tool_call", Tool: "Bash", Text: strings.Repeat("v", 100)})
	}
	s = call(&bigBackend{cx: ol}, "flopwire_read", `{"address":"s000","outline":true}`)
	if n := len(s["outline"].([]any)); n == 0 || n >= 2000 || s["outline_more"] != true || s["outline_next"] != format.OutlineCursor(ol.Outline[n-1]) {
		t.Fatalf("outline: %d kept, more %v, next %v", n, s["outline_more"], s["outline_next"])
	}
}

// withFormat adds format f to a JSON object of arguments.
func withFormat(args, f string) string {
	if args == "{}" {
		return `{"format":"` + f + `"}`
	}
	return `{"format":"` + f + `",` + args[1:]
}

// Issue #80: a commit the transcript never showed the sha of (git commit
// -q) is in sessions' concise row and its --detail, without its call id
// and without a made-up sha, and the text header counts it.
func TestSessionsShowCommitsWithoutSHA(t *testing.T) {
	t0 := time.Date(2026, 10, 2, 0, 36, 24, 0, time.UTC)
	cmd := func(ord int64, id, c string) *transcript.Message {
		a, _ := json.Marshal(map[string]string{"command": c})
		return &transcript.Message{Ordinal: ord, Kind: transcript.KindToolCall, ToolName: "Bash", ToolCallID: id, Text: string(a), TS: t0}
	}
	result := func(ord int64, id, out string) *transcript.Message {
		return &transcript.Message{Ordinal: ord, Kind: transcript.KindToolResult, ToolName: "Bash", ToolCallID: id, Text: out, TS: t0.Add(2 * time.Second)}
	}
	msgs := []*transcript.Message{cmd(1, "c1", `git add a.go && git commit -q -m "Switch GET /users to cursor pagination"`), result(2, "c1", ""),
		cmd(3, "c2", `git commit -m "Add tests"`), result(4, "c2", "[api-cursors 650a939] Add tests\n")}
	b := digest.Update(nil, digest.Conv{Cwd: "/r", RepoRoot: "/r", Branches: []string{"api-cursors"}}, msgs, digest.Counts{})
	last := time.Date(2026, 10, 2, 0, 40, 0, 0, time.UTC)
	c := format.ConversationInfo{SessionID: "b5dd812f-0000-4000-8000-000000000001", Agent: "claude", Repo: "/r", Branches: []string{"api-cursors"},
		LastActivityAt: &last, Digest: format.ParseDigest(b)}
	s := &format.Sessions{Sessions: []format.ConversationInfo{c}}
	for _, detail := range []bool{false, true} {
		out, err := json.Marshal(boundSessions(s, 24000, false, detail))
		if err != nil {
			t.Fatal(err)
		}
		j := string(out)
		want := `"commits_no_sha":[{"subject":"Switch GET /users to cursor pagination","branch":"api-cursors","at":"2026-10-02T00:36:26Z"}]`
		if !strings.Contains(j, `"commits":["650a939"]`) || !strings.Contains(j, want) || strings.Contains(j, `"id":"c1"`) {
			t.Fatalf("detail=%v: %s", detail, j)
		}
	}
	var text strings.Builder
	if err := format.WriteSessions(&text, s, format.Style{Now: func() time.Time { return last }}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), " commits=2") {
		t.Fatalf("text: %s", text.String())
	}
}
