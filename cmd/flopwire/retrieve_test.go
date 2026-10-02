package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/retrieval/format"
)

// The CLI tools and MCP tools call the server's retrieval endpoints with
// the shared encodings and print the shared formats.
func TestRetrievalToolsAgainstServer(t *testing.T) {
	var queries []url.Values
	var paths []string
	off := int64(120)
	hit := format.Hit{Address: "019a0000/491520", MessageID: "m1", ConversationID: "c1", Agent: "codex", SessionID: "019a0000-0000-7000", Kind: "tool_result",
		User: "gary@example.test", Lines: []format.Line{{N: 3, Text: "upload.test.ts:9 timeout\x1b]52;c;evil\x07", Match: true}},
		Provenance: format.Provenance{SourceID: "s1", Path: "/r.jsonl", LineNo: 7, ByteOffset: &off, ByteLen: 40}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries, paths = append(queries, r.URL.Query()), append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/v1/grep", "/v1/search":
			_ = json.NewEncoder(w).Encode(format.Page{Hits: []format.Hit{hit}, Total: 1, TotalSessions: 1, Exact: true})
		case "/v1/sessions":
			_ = json.NewEncoder(w).Encode(format.Sessions{Sessions: []format.ConversationInfo{{Address: "019a0000", Agent: "codex", Title: "t", Messages: 3}}})
		case "/v1/read":
			_ = json.NewEncoder(w).Encode(format.Context{Focus: "m1", Messages: []format.Message{{ID: "m1", Address: "019a0000/491520", Kind: "tool_result", Text: "a\nb", LineFrom: 1, LineTo: 2, Lines: 2}}})
		case "/v1/raw":
			_, _ = w.Write([]byte(`{"raw":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("FLOPWIRE_INDEX", filepath.Join(t.TempDir(), "missing.db"))
	t.Setenv("FLOPWIRE_SESSION_ID", "self-session")
	if err := client.Save(client.Config{Server: srv.URL, Token: "device"}); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() error {
		return run(t.Context(), []string{"grep", "--server", `upload\.Test`, "--agent", "codex", "--since", "24h", "-C", "2", "-m", "3", "--offset", "5"})
	})
	q := queries[0]
	if paths[0] != "/v1/grep" || q.Get("pattern") != `upload\.Test` || q.Get("regex") != "true" || q.Get("case_sensitive") != "true" || q.Get("agent") != "codex" ||
		q.Get("since") == "" || q.Get("before") != "2" || q.Get("after") != "2" || q.Get("max_per_session") != "3" || q.Get("offset") != "5" {
		t.Fatalf("grep query %s %v", paths[0], q)
	}
	// Test stdin is not a terminal, so the calling session is left out.
	if q.Get("exclude_session") != "self-session" || !strings.Contains(out, "left out your own session self-session") {
		t.Fatalf("self exclusion: %v\n%s", q, out)
	}
	// D17: control characters from the transcript are shown, not sent to
	// the terminal.
	if !strings.HasPrefix(out, "## 019a0000 who=gary@example.test agent=codex\n491520:3 tool_result: upload.test.ts:9 timeout␛]52;c;evil␇\n") {
		t.Fatalf("grep output %q", out)
	}
	if err := run(t.Context(), []string{"search", "backoff"}); err == nil || !strings.Contains(err.Error(), "--server") {
		t.Fatalf("search without an index: %v", err)
	}

	c := &retriever{backend: client.HTTP{Server: srv.URL, Token: "device"}}
	text, err := mcpCall(t.Context(), c, "flopwire_search", map[string]any{"query": "backoff", "include_branches": true, "limit": float64(5), "offset": float64(5)})
	if err != nil || !strings.HasPrefix(text, "## 019a0000 who=gary@example.test agent=codex\n491520 ") {
		t.Fatalf("mcp search: %q %v", text, err)
	}
	q = queries[len(queries)-1]
	if q.Get("q") != "backoff" || q.Get("include_branches") != "true" || q.Get("limit") != "5" || q.Get("offset") != "5" {
		t.Fatalf("mcp search query %v", q)
	}
	if text, err = mcpCall(t.Context(), c, "flopwire_read", map[string]any{"address": "019a0000/491520:2", "before": float64(1)}); err != nil || !strings.Contains(text, ">> 019a0000/491520") {
		t.Fatalf("mcp read: %q %v", text, err)
	}
	if q = queries[len(queries)-1]; q.Get("address") != "019a0000/491520:2" || q.Get("before") != "1" {
		t.Fatalf("read query %v", q)
	}
	if text, err = mcpCall(t.Context(), c, "flopwire_read", map[string]any{"address": "019a0000/491520", "raw": true}); err != nil || text != `{"raw":true}` {
		t.Fatalf("mcp raw: %q %v", text, err)
	}
	if q = queries[len(queries)-1]; q.Get("address") != "019a0000/491520" {
		t.Fatalf("raw query %v", q)
	}
	if text, err = mcpCall(t.Context(), c, "flopwire_sessions", map[string]any{"glob": "team*", "agent": "codex"}); err != nil ||
		text != `{"kind":"sessions","sessions":[{"session_id":"","address":"019a0000","agent":"codex","live":false,"messages":3,"title":"t"}],"has_more":false}` {
		t.Fatalf("mcp sessions: %q %v", text, err)
	}
	if q = queries[len(queries)-1]; q.Get("glob") != "team*" || q.Get("agent") != "codex" {
		t.Fatalf("sessions query %v", q)
	}
	if len(mcpTools()) != 7 {
		t.Fatal("tool list")
	}
}

// The parser keeps grep's muscle memory: flags anywhere, grouped short
// flags, -C3, "--" before a dash-leading pattern, -e for one, -n and -r as
// no-ops, and one line naming the nearest flag for a typo.
func TestParseArgs(t *testing.T) {
	o, err := parseArgs("grep", []string{"-in", "--limit=5", "foo", "-C3", "--agent", "codex", "-r"})
	if err != nil || !o.on["ignore-case"] || o.vals["limit"] != "5" || o.vals["context"] != "3" || o.vals["agent"] != "codex" || strings.Join(o.pos, ",") != "foo" {
		t.Fatalf("parse: %+v %v", o, err)
	}
	if o, err = parseArgs("grep", []string{"--", "-v", "--limit"}); err != nil || strings.Join(o.pos, ",") != "-v,--limit" {
		t.Fatalf("dash-leading after --: %+v %v", o, err)
	}
	if o, err = parseArgs("grep", []string{"-e", "-v", "-e", "--x"}); err != nil || strings.Join(o.list["regexp"], ",") != "-v,--x" {
		t.Fatalf("-e: %+v %v", o, err)
	}
	if _, err = parseArgs("grep", []string{"--lmit", "5"}); err == nil || err.Error() != "grep: unknown flag --lmit; did you mean --limit? (flopwire grep --help)" {
		t.Fatalf("typo: %v", err)
	}
	if _, err = parseArgs("read", []string{"-l"}); err == nil || !strings.Contains(err.Error(), "unknown flag -l") {
		t.Fatalf("grep flag on read: %v", err)
	}
	if _, err = parseArgs("grep", []string{"foo", "--limit"}); err == nil || !strings.Contains(err.Error(), "needs a value") {
		t.Fatalf("missing value: %v", err)
	}
	o, _ = parseArgs("grep", []string{"foo", "--limit", "x"})
	if err := runTool(t.Context(), &retriever{}, o, os.Stdout, format.Style{}, selfCLI); err == nil || !strings.Contains(err.Error(), "--limit wants a number") {
		t.Fatalf("int flag: %v", err)
	}
}

// Every tool's help fits a screen and starts with three examples.
func TestToolHelp(t *testing.T) {
	for _, verb := range []string{"grep", "search", "sessions", "read"} {
		h := toolHelp[verb]
		lines := strings.Split(strings.TrimRight(h, "\n"), "\n")
		if len(lines) > 24 {
			t.Errorf("%s help is %d lines", verb, len(lines))
		}
		if len(lines) < 5 || lines[1] != "" || !strings.HasPrefix(lines[2], "  flopwire "+verb) || !strings.HasPrefix(lines[3], "  flopwire "+verb) || !strings.HasPrefix(lines[4], "  flopwire "+verb) {
			t.Errorf("%s help does not open with three examples:\n%s", verb, h)
		}
		for _, l := range lines {
			if len([]rune(l)) > 100 {
				t.Errorf("%s help line too wide: %q", verb, l)
			}
		}
		for _, f := range flagsOf(verb) {
			if f == "help" || f == "line-number" || f == "recursive" {
				continue
			}
			if !strings.Contains(h, "--"+f) && !strings.Contains(h, " "+shortOf(f)+" ") && !strings.Contains(h, shortOf(f)+"/") && !strings.Contains(h, "/"+shortOf(f)) {
				t.Errorf("%s help does not mention --%s", verb, f)
			}
		}
	}
}

func shortOf(long string) string {
	for _, d := range flagDefs {
		if d.long == long && d.short != 0 {
			return "-" + string(d.short)
		}
	}
	return "--" + long
}

func captureStdout(t *testing.T, fn func() error) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = f
	err = fn()
	os.Stdout = old
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f.Name())
	return string(b)
}

// D17: a tool's error line can echo transcript data (an ambiguous
// prefix lists session ids); it prints with control characters shown.
func TestShortErrorCleansControlCharacters(t *testing.T) {
	err := format.AmbiguousError("se", []string{"sess\x1b]52;c;ZXZpbA==\x07", "sess-2"})
	if got := shortError(err); strings.ContainsAny(got, "\x1b\x07") || !strings.Contains(got, "ambiguous") {
		t.Fatalf("%q", got)
	}
}

type rawBackend struct {
	backend
	data []byte
}

func (b rawBackend) RawAt(context.Context, string) ([]byte, error) { return b.data, nil }

// D17: read --raw shows control characters as pictures on a terminal,
// and stays byte-exact into a pipe or file, or with --json.
func TestReadRawCleansOnlyOnTerminal(t *testing.T) {
	record := []byte("{\"t\":\"a\x1b]52;c;ZXZpbA==\x07‮b\"}\n")
	r := &retriever{backend: rawBackend{data: record}}
	run := func(tty bool, args ...string) string {
		t.Helper()
		old := terminalOut
		terminalOut = func(io.Writer) bool { return tty }
		defer func() { terminalOut = old }()
		o, err := parseArgs("read", append(args, "sess/1"))
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		if err := runTool(t.Context(), r, o, &b, format.Style{}, selfCLI); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	if got := run(true, "--raw"); got != format.Clean(string(record)) || strings.ContainsAny(got, "\x1b\x07‮") {
		t.Fatalf("terminal: %q", got)
	}
	if got := run(false, "--raw"); got != string(record) {
		t.Fatalf("pipe: %q", got)
	}
	if got := run(true, "--raw", "--json"); got != string(record) {
		t.Fatalf("--json: %q", got)
	}
}

// The CLI prints everything by default, into a terminal or a pipe
// (ripgrep's convention); --max-bytes opts into the budget. MCP answers
// are always budgeted and take no max_bytes argument.
func TestCLIBudgetIsOptIn(t *testing.T) {
	for _, c := range []struct {
		args []string
		want int
	}{{nil, 0}, {[]string{"--max-bytes", "600"}, 600}, {[]string{"--max-bytes", "0"}, 0}} {
		o, err := parseArgs("grep", append(c.args, "x"))
		if err != nil {
			t.Fatal(err)
		}
		st, err := cliStyle(o)
		if err != nil || st.Budget != c.want || st.MCP {
			t.Fatalf("%v: %+v %v", c.args, st, err)
		}
	}
	o, err := parseArgs("grep", []string{"--max-bytes", "-1", "x"})
	if err == nil {
		if _, err = cliStyle(o); err == nil {
			t.Fatal("--max-bytes -1 accepted")
		}
	}
	if _, _, err := mcpOpts("flopwire_grep", map[string]any{"pattern": "x", "max_bytes": float64(10)}); err == nil || !strings.Contains(err.Error(), `unknown argument "max_bytes"`) {
		t.Fatalf("MCP max_bytes: %v", err)
	}
}

// A time a hit prints can be pasted back into --since, and the help
// shows that form.
func TestSincePrintedForm(t *testing.T) {
	o, err := parseArgs("grep", []string{"--since", "2026-09-23 10:00Z", "x"})
	if err != nil {
		t.Fatal(err)
	}
	f, err := o.filters()
	if err != nil || !f.Since.Equal(time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("since: %v %v", f.Since, err)
	}
	for _, v := range []string{"grep", "search", "sessions"} {
		if !strings.Contains(toolHelp[v], "'2026-09-23 10:00Z'") {
			t.Errorf("%s help has no printed-form --since example", v)
		}
	}
}
