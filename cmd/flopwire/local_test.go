package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/local"
	"github.com/flopwire/flopwire/internal/retrieval/local/localtest"
)

var update = flag.Bool("update", false, "rewrite golden files")

// oracleIndex indexes a copy of testdata/oracle/home into FLOPWIRE_INDEX and
// returns the copy's path.
func oracleIndex(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	if err := os.CopyFS(home, os.DirFS("../../testdata/oracle/home")); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(t.TempDir(), "index.db")
	s, err := localindex.Open(db, localindex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := localtest.IndexHome(t.Context(), s, home); err != nil {
		t.Fatal(err)
	}
	s.Close()
	t.Setenv("FLOPWIRE_INDEX", db)
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "none.json"))
	// A fixed calling session: the Codex "why is the build red?" thread.
	t.Setenv("FLOPWIRE_SESSION_ID", "019a0000-0000-7000-8000-00000000c0de")
	return home
}

// TestLocalCLIGolden runs the verbs against the oracle fixtures and
// compares their output with testdata/golden (go test -run Golden -update
// rewrites them).
func TestLocalCLIGolden(t *testing.T) {
	home := oracleIndex(t)
	cases := []struct {
		name string
		fn   func([]string) error
		args []string
	}{
		{"grep", nil, []string{"grep", "setTimeout"}},
		{"grep_find_alias", nil, []string{"find", "-F", "setTimeout(500)"}},
		{"grep_regex_context", nil, []string{"grep", `^exit (code|status) \d$`, "-B", "1"}},
		{"grep_self_included", nil, []string{"grep", `chi\.New(Router|Mux)`, "--include-self"}},
		{"grep_filters", nil, []string{"grep", "retr", "--agent", "claude", "--kind", "user,assistant", "--exclude-subagents", "--since", "2026-09-23T10:30:00Z"}},
		{"grep_json", nil, []string{"grep", "--json", "exit status"}},
		{"grep_unindexed", nil, []string{"grep", `4.4`, "--limit", "2"}},
		{"grep_files", nil, []string{"grep", "-l", "retr"}},
		{"grep_count", nil, []string{"grep", "-c", "retr"}},
		{"grep_paged", nil, []string{"grep", "retr", "--limit", "2", "--offset", "2"}},
		{"grep_multi", nil, []string{"grep", "-w", "-e", "flaky", "-e", "backoff", "-m", "1"}},
		{"grep_session", nil, []string{"grep", "retr", "--session", "0b7e2c1a-0000-4000-8000-000000000002"}},
		{"grep_error_flag", nil, []string{"grep", "-F", "exit code 1"}},
		{"grep_max_bytes", nil, []string{"grep", "retr", "--session", "0b7e2c1a-0000-4000-8000-000000000002", "--max-bytes", "600"}},
		{"sessions_max_bytes", nil, []string{"sessions", "--max-bytes", "300"}},
		{"search", nil, []string{"search", "exponential backoff", "--limit", "3"}},
		{"search_any_term", nil, []string{"search", "how", "did", "we", "handle", "the", "exponential", "tokenizer?"}},
		{"search_phrase", nil, []string{"search", `"use exponential"`}},
		{"sessions", nil, []string{"sessions"}},
		{"sessions_glob", nil, []string{"sessions", "flak", "--agent", "claude"}},
		{"read_session", nil, []string{"read", "0b7e2c1a-0000-4000-8000-000000000001", "--max-chars", "400"}},
		{"read_path", nil, []string{"read", "$HOME/.claude/projects/-tmp-oracle-alpha/0b7e2c1a-0000-4000-8000-000000000001.jsonl:8", "-B", "1", "-A", "1"}},
		{"raw_provenance", nil, []string{"raw", "1", "1", "236", "344"}},
		{"grep_flat", nil, []string{"grep", "retr", "--limit", "3", "--no-heading"}},
		{"grep_only_matching", nil, []string{"grep", "-o", `retr\w*`, "--limit", "3"}},
		{"grep_multiline", nil, []string{"grep", "-U", `upload\.test\.ts:9 timeout\n.*exit`}},
		{"grep_branch", nil, []string{"grep", "retr", "--branch", "fix/*", "--exclude-subagents"}},
		{"grep_oldest", nil, []string{"grep", "retr", "--sort", "oldest", "--limit", "3"}},
		{"grep_relevance", nil, []string{"grep", "retr", "--sort", "relevance", "--limit", "3"}},
		{"grep_self_session", nil, []string{"grep", "chi", "--session", "self"}},
		{"search_newest", nil, []string{"search", "retry", "--sort", "newest", "--limit", "3"}},
		{"sessions_branch", nil, []string{"sessions", "--branch", "fix/*", "--sort", "oldest"}},
		{"read_outline", nil, []string{"read", "0b7e2c1a-0000-4000-8000-000000000002", "--outline"}},
		{"read_outline_paged", nil, []string{"read", "019a0000-0000-7000-8000-0000000000a1", "--outline", "--limit", "3", "--cursor", "3239936.56"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := make([]string, len(c.args))
			for i, a := range c.args {
				args[i] = strings.ReplaceAll(a, "$HOME", home)
			}
			out := captureStdout(t, func() error { return run(t.Context(), args) })
			out = strings.ReplaceAll(out, home, "$HOME")
			golden := filepath.Join("testdata", "golden", c.name+".txt")
			if *update {
				os.MkdirAll(filepath.Dir(golden), 0o755)
				if err := os.WriteFile(golden, []byte(out), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if out != string(want) {
				t.Errorf("%s: output differs from %s:\n%s", c.name, golden, out)
			}
		})
	}
}

// TestLocalMCPRoundTrip speaks JSON-RPC to the MCP server over the local
// index.
func TestLocalMCPRoundTrip(t *testing.T) {
	oracleIndex(t)
	r, err := openRetriever(false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	reqs := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"flopwire_grep","arguments":{"pattern":"chi\\.New(Router|Mux)"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"flopwire_grep","arguments":{"pattern":"chi.New","fixed_strings":true,"include_self":true,"agent":"codex","limit":10,"format":"json"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"flopwire_search","arguments":{"query":"backoff","kind":"nope"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"flopwire_grep","arguments":{"pattern":"x.y","limit":1}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"nope"}`,
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"flopwire_grep","arguments":{"pattern":"x","bogus":1}}}`,
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"flopwire_read","arguments":{"address":"zzzzzzzz/1"}}}`,
	}
	var out strings.Builder
	if err := serveMCP(t.Context(), r, strings.NewReader(strings.Join(reqs, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	got := mcpResponses(t, out.String())
	if len(got) != 9 {
		t.Fatalf("want 9 responses (none for the notification), got %d:\n%s", len(got), out.String())
	}
	if got[1].Result.ServerInfo["name"] != "flopwire" || !strings.Contains(got[1].Result.Instructions, "SESSION/ORDINAL") || len(got[2].Result.Tools) != 7 {
		t.Fatalf("initialize/list: %+v %+v", got[1], got[2])
	}
	text := func(id int) string { return got[id].Result.Content[0].Text }
	// The caller's own session (97, 98) and its subagent (100) are left
	// out, and the answer says so.
	if strings.Contains(text(3), "c0de") && !strings.Contains(text(3), "left out your own session 019a0000-0000-7000-8000-00000000c0de") ||
		strings.Count("\n"+text(3), "\n## ") != 1 || !strings.Contains(text(3), "include_self=true") {
		t.Fatalf("self exclusion: %s", text(3))
	}
	var page format.Page
	if err := json.Unmarshal([]byte(text(4)), &page); err != nil || len(page.Hits) != 4 || page.Total != 4 {
		t.Fatalf("include_self json: %s %v", text(4), err)
	}
	if !got[5].Result.IsError || !strings.Contains(text(5), "unknown kind") || strings.Contains(text(5), "retrieval:") {
		t.Fatalf("bad kind: %+v", got[5])
	}
	if !strings.Contains(text(6), "unindexed") {
		t.Fatalf("unindexed note: %s", text(6))
	}
	if got[7].Error == nil || got[7].Error.Code != -32601 {
		t.Fatalf("unknown method: %+v", got[7])
	}
	if !got[8].Result.IsError || !strings.Contains(text(8), `unknown argument "bogus"`) {
		t.Fatalf("unknown argument: %+v", got[8])
	}
	if !got[9].Result.IsError || !strings.Contains(text(9), "flopwire_grep") {
		t.Fatalf("read of a missing address: %+v", got[9])
	}
}

type mcpResp struct {
	ID     int `json:"id"`
	Result struct {
		Tools        []map[string]any  `json:"tools"`
		IsError      bool              `json:"isError"`
		Instructions string            `json:"instructions"`
		ServerInfo   map[string]string `json:"serverInfo"`
		Content      []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func mcpResponses(t *testing.T, out string) map[int]mcpResp {
	t.Helper()
	got := map[int]mcpResp{}
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		var r mcpResp
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("bad response line %q: %v", sc.Text(), err)
		}
		got[r.ID] = r
	}
	return got
}

// Every address grep, search and sessions print (text and JSON, CLI and
// MCP) round-trips through read: a message address reads that message
// with the addressed line marked, a session address reads the session.
func TestAddressesRoundTripThroughRead(t *testing.T) {
	oracleIndex(t)
	lineAddr := regexp.MustCompile(`^([^\s/\[]+/\d+)(?::(\d+))?[:-]`)
	sessAddr := regexp.MustCompile(`^([^\s/\[:]+)(?:  |:\d+$)`)
	// The grouped layout: a "## SESSION ..." header, then ORDINAL:LINE
	// (ORDINAL-LINE- for context) lines whose address is SESSION/ORDINAL.
	header := regexp.MustCompile(`^## ([^\s]+)`)
	grouped := regexp.MustCompile(`^(\d+)[:-](\d+)[ :-]`)
	type addr struct{ msg, line, session string }
	var addrs []addr
	collect := func(out string) {
		cur := ""
		for _, l := range strings.Split(out, "\n") {
			if m := header.FindStringSubmatch(l); m != nil {
				cur = m[1]
				addrs = append(addrs, addr{session: cur})
			} else if m := grouped.FindStringSubmatch(l); m != nil && cur != "" {
				addrs = append(addrs, addr{msg: cur + "/" + m[1], line: m[2]})
			} else if m := lineAddr.FindStringSubmatch(l); m != nil {
				addrs = append(addrs, addr{msg: m[1], line: m[2]})
			} else if m := sessAddr.FindStringSubmatch(l); m != nil {
				addrs = append(addrs, addr{session: m[1]})
			}
		}
	}
	for _, args := range [][]string{
		{"grep", "--include-self", "retr", "--limit", "50"},
		{"grep", "--include-self", `^exit`, "-C", "2"},
		{"grep", "--include-self", "-l", "e"},
		{"grep", "--include-self", "-c", "chi"},
		{"search", "--include-self", "retry backoff test"},
		{"search", "--include-self", "timers"},
		{"sessions", "--include-self", "--limit", "100"},
	} {
		collect(captureStdout(t, func() error { return run(t.Context(), args) }))
	}
	r, err := openRetriever(false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	for _, call := range []struct {
		tool string
		args map[string]any
	}{
		{"flopwire_grep", map[string]any{"pattern": "test", "include_self": true, "limit": float64(50)}},
		{"flopwire_search", map[string]any{"query": "upload retries", "include_self": true}},
		{"flopwire_sessions", map[string]any{"include_self": true}},
	} {
		text, err := mcpCall(t.Context(), r, call.tool, call.args)
		if err != nil {
			t.Fatal(err)
		}
		collect(text)
	}
	var jsonPage format.Page
	out := captureStdout(t, func() error { return run(t.Context(), []string{"grep", "--include-self", "--json", "the"}) })
	if err := json.Unmarshal([]byte(out), &jsonPage); err != nil {
		t.Fatal(err)
	}
	for _, h := range jsonPage.Hits {
		addrs = append(addrs, addr{msg: h.Address, line: strconv.Itoa(h.Lines[0].N)})
	}
	var nSess, nLine int
	for _, a := range addrs {
		if a.session != "" {
			nSess++
		} else if a.line != "" {
			nLine++
		}
	}
	if nSess < 20 || nLine < 40 {
		t.Fatalf("collected %d session and %d line addresses of %d", nSess, nLine, len(addrs))
	}
	seen := map[addr]bool{}
	for _, a := range addrs {
		if seen[a] {
			continue
		}
		seen[a] = true
		target := a.msg
		if a.session != "" {
			target = a.session
		} else if a.line != "" {
			target += ":" + a.line
		}
		out := captureStdout(t, func() error { return run(t.Context(), []string{"read", target}) })
		switch {
		case a.session != "":
			if !strings.Contains(out, ">> "+a.session+"/") {
				t.Errorf("read %s: no focus in that session:\n%s", target, out)
			}
		case !strings.Contains(out, ">> "+a.msg+"  "):
			t.Errorf("read %s: focus is not that message:\n%s", target, out)
		case a.line != "" && !regexp.MustCompile(`(?m)^>\s*`+a.line+`  `).MatchString(out):
			t.Errorf("read %s: line %s not marked:\n%s", target, a.line, out)
		}
		// JSON reads name the same message.
		var cx format.Context
		js := captureStdout(t, func() error { return run(t.Context(), []string{"read", "--json", target}) })
		if err := json.Unmarshal([]byte(js), &cx); err != nil || len(cx.Messages) == 0 {
			t.Fatalf("read --json %s: %v", target, err)
		}
		if a.msg != "" {
			for _, m := range cx.Messages {
				if m.ID == cx.Focus && m.Address != a.msg {
					t.Errorf("read --json %s: focus address %s", target, m.Address)
				}
			}
		}
	}
	// A path:line address from a hit's provenance reads the same message.
	h := jsonPage.Hits[0]
	out = captureStdout(t, func() error {
		return run(t.Context(), []string{"read", fmt.Sprintf("%s:%d", h.Provenance.Path, h.Provenance.LineNo)})
	})
	if !strings.Contains(out, ">> "+h.Address+"  ") && !strings.Contains(out, "   "+h.Address+"  ") {
		t.Errorf("read by path: %s not shown:\n%s", h.Address, out)
	}
	// Raw bytes of an address are its transcript record.
	rawOut := captureStdout(t, func() error { return run(t.Context(), []string{"read", "--raw", h.Address}) })
	if !strings.HasPrefix(rawOut, "{") || !strings.HasSuffix(strings.TrimSpace(rawOut), "}") {
		t.Errorf("read --raw: %q", rawOut)
	}
	// Ambiguous and unknown prefixes say so.
	if err := run(t.Context(), []string{"read", "019a0000/1"}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("ambiguous prefix: %v", err)
	}
	if err := run(t.Context(), []string{"read", "nosuchsession/1"}); err == nil || !strings.Contains(err.Error(), "no session starts with") {
		t.Errorf("unknown prefix: %v", err)
	}
}

// --session self and read self name the calling session by exact
// evidence only; without it the call fails and says how to name it.
// --exclude-live leaves out the sessions this machine's harnesses hold
// open, and headers mark them live.
func TestSelfAndLive(t *testing.T) {
	oracleIndex(t)
	r, err := openRetriever(false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	call := func(verb string, args ...string) (string, error) {
		t.Helper()
		o, err := parseArgs(verb, args)
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		err = runTool(t.Context(), r, o, &b, format.Style{}, selfCLI)
		return b.String(), err
	}
	out, err := call("read", "self", "--outline")
	if err != nil || !strings.Contains(out, "019a0000-0000-7000-8000-00000000c0de") || !strings.Contains(out, "# intent: \"why is the build red?\"") {
		t.Fatalf("read self --outline: %v\n%s", err, out)
	}
	r.caller = func(context.Context) (local.Caller, bool) { return local.Caller{}, false }
	if _, err := call("grep", "chi", "--session", "self"); err == nil || !strings.Contains(err.Error(), "FLOPWIRE_SESSION_ID") {
		t.Fatalf("--session self without a caller: %v", err)
	}
	if _, err := mcpCall(t.Context(), r, "flopwire_search", map[string]any{"query": "chi", "session": "self"}); err == nil ||
		!strings.Contains(err.Error(), "MCP server's environment") {
		t.Fatalf("MCP session=self without a caller: %v", err)
	}
	// Self is the caller's exact id, never a prefix: a caller whose id
	// only prefixes another session reads nothing of it.
	r.caller = func(context.Context) (local.Caller, bool) {
		return local.Caller{SessionID: "agent-a", Rule: "env"}, true
	}
	for _, args := range [][]string{{"grep", "e", "--session", "self"}, {"read", "self"}, {"read", "self", "--outline"}, {"read", "self", "--raw"}} {
		if out, err := call(args[0], args[1:]...); err == nil || !strings.Contains(err.Error(), "not indexed yet") {
			t.Fatalf("%v with a caller id that prefixes another session: %v\n%s", args, err, out)
		}
	}
	// The Codex "add a health check endpoint" session is held open; it
	// wrote 30 minutes ago.
	const open = "019a0000-0000-7000-8000-0000000000a1"
	setActivity := func(sid string, at time.Time) {
		t.Helper()
		db, err := sql.Open("sqlite", os.Getenv("FLOPWIRE_INDEX"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(`UPDATE conversations SET last_activity_at=? WHERE session_id=?`, at.UnixMilli(), sid); err != nil {
			t.Fatal(err)
		}
	}
	setActivity(open, time.Now().Add(-30*time.Minute))
	r.live = func(bool) map[string]time.Time { return map[string]time.Time{open: time.Now()} }
	out, err = call("sessions", "--repo", "oracle-beta", "--agent", "codex")
	if err != nil || !strings.Contains(out, open+"  codex  live, ") {
		t.Fatalf("live header: %v\n%s", err, out)
	}
	out, err = call("sessions", "--repo", "oracle-beta", "--agent", "codex", "--exclude-live")
	if err != nil || strings.Contains(out, open) || !strings.Contains(out, "[no sessions]") {
		t.Fatalf("--exclude-live: %v\n%s", err, out)
	}
	if out, err = call("grep", "health", "--exclude-live", "--include-self"); err != nil || strings.Contains(out, open) {
		t.Fatalf("grep --exclude-live: %v\n%s", err, out)
	}
	// Held open but idle for two hours (a Devin lock: no write time), it
	// is neither live nor left out.
	setActivity(open, time.Now().Add(-2*time.Hour))
	r.live = func(bool) map[string]time.Time { return map[string]time.Time{open: {}} }
	out, err = call("sessions", "--repo", "oracle-beta", "--agent", "codex", "--exclude-live")
	if err != nil || !strings.Contains(out, open+"  codex  ended ") {
		t.Fatalf("--exclude-live with an idle held session: %v\n%s", err, out)
	}
}
