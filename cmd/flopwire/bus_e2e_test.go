package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/api"
	"github.com/flopwire/flopwire/internal/bus"
	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/local"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/jackc/pgx/v5/pgxpool"
)

// writeClaudeSession writes a synthetic Claude Code transcript for id in
// cwd, last written a minute ago, so the agent reports it live.
func writeClaudeSession(t *testing.T, projects, id, cwd, prompt string) {
	t.Helper()
	writeClaudeSessionAt(t, projects, id, cwd, prompt, time.Now().UTC().Add(-time.Minute))
}

func writeClaudeSessionAt(t *testing.T, projects, id, cwd, prompt string, at time.Time) {
	t.Helper()
	dir := filepath.Join(projects, strings.ReplaceAll(cwd, "/", "-"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ts := func(d time.Duration) string { return at.Add(d).Format("2006-01-02T15:04:05.000Z") }
	lines := []string{
		fmt.Sprintf(`{"parentUuid":null,"isSidechain":false,"userType":"external","cwd":%q,"sessionId":%q,"version":"2.1.0","gitBranch":"main","type":"user","message":{"role":"user","content":%q},"uuid":"%s-u1","timestamp":%q}`, cwd, id, prompt, id[:8], ts(0)),
		fmt.Sprintf(`{"parentUuid":"%s-u1","isSidechain":false,"userType":"external","cwd":%q,"sessionId":%q,"version":"2.1.0","gitBranch":"main","type":"assistant","message":{"id":"msg_%s","type":"message","role":"assistant","model":"claude-opus-4-1","content":[{"type":"text","text":"On it."}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}},"requestId":"req_%s","uuid":"%s-a1","timestamp":%q}`, id[:8], cwd, id, id[:8], id[:8], id[:8], ts(time.Second)),
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// startAgent runs `flopwire agent run` on home's transcripts until the test
// ends, and waits until its peers list both sessions as seen from caller.
func startAgent(t *testing.T, home, sock string, extra ...string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runAgent(ctx, append([]string{"--socket", sock, "--claude-projects", filepath.Join(home, ".claude", "projects"),
			"--codex-home", filepath.Join(home, ".codex"), "--devin-db", "-", "--mem-limit", "0", "--gc-percent", "100"}, extra...))
		done <- err
	}()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(60 * time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("agent stopped: %v", err)
		default:
		}
		if _, err := agent.Call(ctx, sock, agent.Request{Op: "ping"}); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("agent never answered")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// busCLI runs a bus verb as session (a Claude session) against sock.
func busCLI(t *testing.T, sock, session, stdin string, args ...string) (string, error) {
	t.Helper()
	asCaller(t, &local.Caller{Agent: transcript.AgentClaude, SessionID: session, Rule: "test"})
	var out, errOut strings.Builder
	err := busCmd(t.Context(), args[0], append([]string{"--socket", sock, "--text"}, args[1:]...), strings.NewReader(stdin), &out, &errOut)
	return out.String(), err
}

// waitPeer waits until session's peers list other.
func waitPeer(t *testing.T, sock, session, other string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		out, err := busCLI(t, sock, session, "", "peers")
		if err == nil && strings.Contains(out, other[:8]) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("peers never listed %s: %q %v", other, out, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// osUser is the name the agent uses for this device's person without a
// server (devicebus: the OS account).
func osUser(t *testing.T) string {
	u, err := user.Current()
	if err != nil || u.Username == "" {
		t.Skip("no OS user name")
	}
	return u.Username
}

const (
	e2eA = "e2e0aaaa-0000-4000-8000-000000000001"
	e2eB = "e2e0bbbb-0000-4000-8000-000000000002"
)

// Two sessions on one device, through the real device agent with no
// server: peers lists the other one, send queues the message, the hook's
// pending returns it once, and the sender's inbox shows it delivered. The
// MCP tools reach the same agent.
func TestBusEndToEndLocal(t *testing.T) {
	home := t.TempDir()
	projects := filepath.Join(home, ".claude", "projects")
	writeClaudeSession(t, projects, e2eA, "/tmp/e2e-api", "refactor client pagination")
	writeClaudeSession(t, projects, e2eB, "/tmp/e2e-api", "add cursor to list endpoint")
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "flopwire", "config.json"))
	t.Setenv("FLOPWIRE_INDEX", filepath.Join(t.TempDir(), "index.db"))
	t.Setenv(client.EnvToken, "")
	t.Setenv(client.EnvServer, "")
	sock := filepath.Join(shortSockDir(t), "a.sock")
	startAgent(t, home, sock, "--no-sync")
	waitPeer(t, sock, e2eA, e2eB)

	out, err := busCLI(t, sock, e2eA, "", "peers")
	if err != nil || strings.Contains(out, "e2e0aaaa") || !strings.Contains(out, `e2e0bbbb  `) || !strings.Contains(out, `claude  live idle  e2e-api@main  "add cursor to list endpoint"`) {
		t.Fatalf("peers:\n%s %v", out, err)
	}
	out, err = busCLI(t, sock, e2eA, "Heads-up: the list endpoint returns a cursor now.\nUse it for page 2.\n", "send", "e2e0bbbb", "--intent", "request", "--", "-")
	if err != nil || !strings.HasPrefix(out, "sent m") || !strings.HasSuffix(out, " to e2e0bbbb ("+osUser(t)+" claude e2e-api@main): idle, arrives with its human's next prompt\n") {
		t.Fatalf("send: %q %v", out, err)
	}
	id := strings.Fields(out)[1]
	// The same text again within ten minutes is dropped, and says so.
	if _, err := busCLI(t, sock, e2eA, "Heads-up: the list endpoint returns a cursor now.\nUse it for page 2.", "send", "e2e0bbbb", "--intent", "request", "--", "-"); err == nil ||
		!strings.Contains(err.Error(), "refused (duplicate)") || !strings.Contains(err.Error(), "Fix: do not resend") {
		t.Fatalf("duplicate: %v", err)
	}
	// The hook stream's side: pending returns it to the recipient once.
	resp, err := agent.Call(t.Context(), sock, agent.Request{Op: "pending", Session: e2eB})
	if err != nil || len(resp.Messages) != 1 || resp.Messages[0].ID != id || resp.Messages[0].From != e2eA ||
		resp.Messages[0].Body != "Heads-up: the list endpoint returns a cursor now.\nUse it for page 2." || resp.Messages[0].Intent != busproto.IntentRequest {
		t.Fatalf("pending: %+v %v", resp.Messages, err)
	}
	if again, _ := agent.Call(t.Context(), sock, agent.Request{Op: "pending", Session: e2eB}); len(again.Messages) != 0 {
		t.Fatalf("delivered twice: %+v", again.Messages)
	}
	out, err = busCLI(t, sock, e2eA, "", "inbox", "--sent")
	if err != nil || !strings.Contains(out, id+"  sent  ") || !strings.Contains(out, "request  delivered") || !strings.Contains(out, "refused (duplicate)") {
		t.Fatalf("sender's inbox:\n%s %v", out, err)
	}
	// The recipient replies over MCP, as the same agent's other session.
	r := &retriever{caller: func(context.Context) (local.Caller, bool) {
		return local.Caller{Agent: transcript.AgentClaude, SessionID: e2eB, Rule: "test"}, true
	}, busSocket: sock}
	text, err := mcpCall(t.Context(), r, "flopwire_send", map[string]any{"to": "e2e0aaaa", "message": "Thanks, switching now.", "intent": "done", "reply_to": id})
	if err != nil || !strings.Contains(text, " to e2e0aaaa (") {
		t.Fatalf("mcp send: %q %v", text, err)
	}
	text, err = mcpCall(t.Context(), r, "flopwire_inbox", map[string]any{"thread": id, "format": "text"})
	if err != nil || !strings.Contains(text, "    Thanks, switching now.") || !strings.Contains(text, "    Use it for page 2.") || !strings.Contains(text, "[2 messages in thread "+id) {
		t.Fatalf("mcp thread:\n%s %v", text, err)
	}
	// A done message must not be answered.
	_, err = busCLI(t, sock, e2eA, "", "send", "e2e0bbbb", "--reply-to", strings.Fields(text)[0], "--", "ok")
	if err == nil || !strings.Contains(err.Error(), "refused (reply_to_done)") {
		t.Fatalf("reply to done: %v", err)
	}
	// An unknown prefix is refused with a fix.
	if _, err := busCLI(t, sock, e2eA, "", "send", "ffff0000", "--", "hi"); err == nil || !strings.Contains(err.Error(), "refused (unknown_recipient)") {
		t.Fatalf("unknown recipient: %v", err)
	}
}

// The same through the real server handlers and Postgres: the agent polls
// the server with its presence, the send goes to the server, the
// recipient's pending gets it from the poll, and the receipt reaches the
// sender's inbox. An @user message to a person with no live session is
// queued with its expiry.
func TestBusEndToEndServer(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, pgtest.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	pg := store.NewPostgres(pool, nil, "")
	srv := httptest.NewServer(api.New(pg, api.Config{Logger: quiet, Bus: &bus.Store{Pool: pool}}).Handler(nil))
	t.Cleanup(srv.Close)
	_, admin := seedAdmin(t, pg)
	member := func(email string) string {
		invite := apiCall(t, srv.URL, admin, "POST", "/v1/admin/invites", map[string]string{"email": email})
		return apiCall(t, srv.URL, "", "POST", "/v1/invites/claim", map[string]string{"code": invite["code"].(string), "name": email, "password": adminPassword})["token"].(string)
	}
	gary := member("gary@example.test")
	alex := member("alex@example.test")
	enrolled := apiCall(t, srv.URL, gary, "POST", "/v1/devices", map[string]string{"name": "mac", "platform": "darwin"})
	configPath := filepath.Join(t.TempDir(), "flopwire", "config.json")
	t.Setenv("FLOPWIRE_CONFIG", configPath)
	t.Setenv("FLOPWIRE_INDEX", filepath.Join(t.TempDir(), "index.db"))
	t.Setenv(client.EnvToken, "")
	t.Setenv(client.EnvServer, "")
	if err := client.Save(client.Config{Server: srv.URL, Token: enrolled["token"].(string), SessionToken: gary, DeviceID: enrolled["device"].(map[string]any)["id"].(string)}); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	projects := filepath.Join(home, ".claude", "projects")
	writeClaudeSession(t, projects, e2eA, "/tmp/e2e-api", "refactor client pagination")
	writeClaudeSession(t, projects, e2eB, "/tmp/e2e-api", "add cursor to list endpoint")
	// A session a local path rule keeps on this device, quiet for two
	// hours, so it is not in presence either.
	const secret = "e2e0cccc-0000-4000-8000-000000000003"
	writeClaudeSessionAt(t, projects, secret, "/tmp/e2e-secret", "rotate the client's keys", time.Now().UTC().Add(-2*time.Hour))
	if err := os.WriteFile(filepath.Join(filepath.Dir(configPath), "path-rules"), []byte("local /tmp/e2e-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(shortSockDir(t), "a.sock")
	startAgent(t, home, sock)
	waitPeer(t, sock, e2eA, e2eB)

	out, err := busCLI(t, sock, e2eA, "", "send", "e2e0bbbb", "--", "Heads-up: the list endpoint returns a cursor now.")
	if err != nil || !strings.HasPrefix(out, "sent m") || !strings.HasSuffix(out, " to e2e0bbbb (gary claude e2e-api@main): idle, arrives with its human's next prompt\n") {
		t.Fatalf("send: %q %v", out, err)
	}
	id := strings.Fields(out)[1]
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := agent.Call(t.Context(), sock, agent.Request{Op: "pending", Session: e2eB})
		if err == nil && len(resp.Messages) == 1 {
			if e := resp.Messages[0]; e.ID != id || e.From != e2eA || e.User != "gary@example.test" || e.Sender != busproto.SenderOwn {
				t.Fatalf("delivered: %+v", e)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never delivered: %+v %v", resp, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	for {
		out, err = busCLI(t, sock, e2eA, "", "inbox", "--sent")
		if err == nil && strings.Contains(out, id+"  sent  ") && strings.Contains(out, "inform  delivered") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("receipt never reached the sender's inbox:\n%s %v", out, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Another person has not accepted gary yet: held. Once accepted, a
	// message to them with no live session is queued until it expires.
	out, err = busCLI(t, sock, e2eA, "", "send", "@alex", "--", "Who owns the pager this week?")
	if err != nil || !strings.HasPrefix(out, "held m") || !strings.Contains(out, " for @alex: alex has not accepted messages from you; expires ") {
		t.Fatalf("held: %q %v", out, err)
	}
	apiCall(t, srv.URL, alex, "POST", busproto.PathAccepts, busproto.AcceptRequest{Sender: "gary@example.test", Password: adminPassword})
	out, err = busCLI(t, sock, e2eA, "", "send", "@alex", "--", "Second question: is the pager rota in the wiki?")
	if err != nil || !strings.HasPrefix(out, "queued m") || !strings.Contains(out, " for @alex: no live session on e2e-api; expires ") {
		t.Fatalf("@user send: %q %v", out, err)
	}
	if _, err := busCLI(t, sock, e2eA, "", "send", "@nobody", "--", "hi"); err == nil || !strings.Contains(err.Error(), "refused (unknown_recipient): no person matches @nobody") {
		t.Fatalf("unknown person: %v", err)
	}
	// The withheld session that is not live: refused on the device; its id
	// and text never reach the server.
	if _, err := busCLI(t, sock, secret, "", "send", "e2e0bbbb", "--", "the client's key is in ~/keys"); err == nil ||
		!strings.Contains(err.Error(), "refused (session_not_on_device)") || !strings.Contains(err.Error(), "path rule") {
		t.Fatalf("withheld sender: %v", err)
	}
	if _, err := busCLI(t, sock, secret, "", "inbox"); err == nil || !strings.Contains(err.Error(), "path rule") {
		t.Fatalf("withheld inbox: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM bus_messages WHERE from_session=$1 OR body LIKE '%client''s key%'`, secret).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the withheld session's send reached the server: %d %v", n, err)
	}
}

// writeCommitSession writes a synthetic Claude Code session that commits
// on branchA (a successful git commit tool call), then switches to
// branchB; its last record is at last. Its first prompt, and so its
// title, is prompt.
func writeCommitSession(t *testing.T, projects, id, cwd, branchA, branchB, sha, prompt string, last time.Time) {
	t.Helper()
	dir := filepath.Join(projects, strings.ReplaceAll(cwd, "/", "-"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ts := func(back time.Duration) string { return last.Add(-back).UTC().Format("2006-01-02T15:04:05.000Z") }
	p := id[:8]
	rec := func(uuid, parent, branch, typ, message, extra string, back time.Duration) string {
		par := "null"
		if parent != "" {
			par = fmt.Sprintf("%q", p+parent)
		}
		return fmt.Sprintf(`{"parentUuid":%s,"isSidechain":false,"userType":"external","cwd":%q,"sessionId":%q,"version":"2.1.0","gitBranch":%q,"type":%q,"message":%s,%s"uuid":%q,"timestamp":%q}`,
			par, cwd, id, branch, typ, message, extra, p+uuid, ts(back))
	}
	lines := []string{
		rec("-u1", "", branchA, "user", `{"role":"user","content":`+string(must(json.Marshal(prompt)))+`}`, "", 4*time.Minute),
		rec("-a1", "-u1", branchA, "assistant", `{"id":"msg_`+p+`1","type":"message","role":"assistant","model":"claude-opus-4-1","content":[{"type":"tool_use","id":"toolu_`+p+`","name":"Bash","input":{"command":"git commit -am \"add cursor\""}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`, `"requestId":"req_`+p+`1",`, 3*time.Minute),
		rec("-u2", "-a1", branchA, "user", `{"role":"user","content":[{"tool_use_id":"toolu_`+p+`","type":"tool_result","content":"[`+branchA+` `+sha+`] add cursor\n 1 file changed, 4 insertions(+)","is_error":false}]}`, `"toolUseResult":{"stdout":"[`+branchA+` `+sha+`] add cursor","stderr":"","interrupted":false},`, 3*time.Minute-time.Second),
		rec("-u3", "-u2", branchB, "user", `{"role":"user","content":"now start the docs on `+branchB+`"}`, "", time.Minute),
		rec("-a2", "-u3", branchB, "assistant", `{"id":"msg_`+p+`2","type":"message","role":"assistant","model":"claude-opus-4-1","content":[{"type":"text","text":"Switched."}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`, `"requestId":"req_`+p+`2",`, 0),
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// History, then presence (issue #55): a session that committed on
// feat-a and then switched to feat-b is found by its history on feat-a,
// and its presence row carries the same full session id (on feat-b). A
// session that ended is still in history but is not reported live, and a
// message to it waits: nothing wakes it.
func TestBusHistoryToPresence(t *testing.T) {
	home := t.TempDir()
	projects := filepath.Join(home, ".claude", "projects")
	const (
		repo    = "/tmp/e2e-hist"
		moved   = "e2e1dddd-0000-4000-8000-000000000004"
		ended   = "e2e1eeee-0000-4000-8000-000000000005"
		movedSH = "1a2b3c4"
		endedSH = "5d6e7f8"
	)
	writeClaudeSession(t, projects, e2eA, repo, "review the list endpoint")
	// A title with spaces, quotes, field look-alikes and Unicode.
	const title = `fix "the" list endpoint — café ☕  agent: codex  session: x`
	writeCommitSession(t, projects, moved, repo, "feat-a", "feat-b", movedSH, title, time.Now().Add(-30*time.Second))
	writeCommitSession(t, projects, ended, repo, "feat-a", "feat-a", endedSH, "add a cursor to the list endpoint", time.Now().Add(-3*time.Hour))
	index := filepath.Join(t.TempDir(), "index.db")
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "flopwire", "config.json"))
	t.Setenv("FLOPWIRE_INDEX", index)
	t.Setenv(client.EnvToken, "")
	t.Setenv(client.EnvServer, "")
	sock := filepath.Join(shortSockDir(t), "a.sock")
	startAgent(t, home, sock, "--no-sync")
	waitPeer(t, sock, e2eA, moved)

	// 1. History: who committed on feat-a in this repo? sessions answers
	// concise JSON by default; the match is by field name: session_id,
	// repo and commits.
	r, err := openRetriever(false, index)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	o, err := parseArgs("sessions", []string{"--repo", repo, "--branch", "feat-a"})
	if err != nil {
		t.Fatal(err)
	}
	var hist bytes.Buffer
	if err := runTool(t.Context(), r, o, &hist, format.Style{}, selfCLI); err != nil {
		t.Fatal(err)
	}
	type histSession struct {
		SessionID string   `json:"session_id"`
		Title     string   `json:"title"`
		Repo      string   `json:"repo"`
		Branches  []string `json:"branches"`
		Live      bool     `json:"live"`
		Commits   []string `json:"commits"`
	}
	var hj struct {
		Kind     string        `json:"kind"`
		Sessions []histSession `json:"sessions"`
		HasMore  *bool         `json:"has_more"`
	}
	if err := json.Unmarshal(hist.Bytes(), &hj); err != nil || hj.Kind != "sessions" || hj.HasMore == nil || *hj.HasMore {
		t.Fatalf("history is not the sessions JSON: %v\n%s", err, hist.String())
	}
	bySHA := func(sha string) histSession {
		for _, c := range hj.Sessions {
			if slices.Contains(c.Commits, sha) {
				return c
			}
		}
		t.Fatalf("no session in history committed %s:\n%s", sha, hist.String())
		return histSession{}
	}
	m, e := bySHA(movedSH), bySHA(endedSH)
	if len(hj.Sessions) != 2 || m.SessionID != moved || e.SessionID != ended {
		t.Fatalf("history on feat-a: %s", hist.String())
	}
	if m.Title != title || m.Repo != repo || !slices.Equal(m.Branches, []string{"feat-a", "feat-b"}) || !m.Live {
		t.Fatalf("moved session in history: %+v", m)
	}
	if e.Live {
		t.Fatalf("ended session in history: %+v", e)
	}
	// The MCP tool answers the same JSON by default.
	text, err := mcpCall(t.Context(), r, "flopwire_sessions", map[string]any{"repo": repo, "branch": "feat-a"})
	var mj struct {
		Sessions []histSession `json:"sessions"`
	}
	if err != nil || json.Unmarshal([]byte(text), &mj) != nil || len(mj.Sessions) != 2 || !slices.ContainsFunc(mj.Sessions, func(c histSession) bool {
		return c.SessionID == moved && slices.Contains(c.Commits, movedSH) && c.Title == title
	}) {
		t.Fatalf("flopwire_sessions: %s %v", text, err)
	}

	// 2. Presence: peers answers JSON by default; the session field of its
	// row is the same full id, live, on its current branch.
	out, stderr, err := busJSON(t, sock, e2eA, "peers", "--session", m.SessionID)
	var pj struct {
		Peers []struct {
			Session string `json:"session"`
			Branch  string `json:"branch"`
			Repo    string `json:"repo"`
			Title   string `json:"title"`
		} `json:"peers"`
	}
	if err != nil || json.Unmarshal([]byte(out), &pj) != nil || len(pj.Peers) != 1 {
		t.Fatalf("peers --session %s: %q %q %v", moved, out, stderr, err)
	}
	if p := pj.Peers[0]; p.Session != m.SessionID || p.Branch != "feat-b" || p.Repo != repo || p.Title != m.Title {
		t.Fatalf("presence row: %+v", p)
	}
	// A peer filter by its branch from history would miss it: the
	// session is no longer on feat-a.
	if out, _, _ := busJSON(t, sock, e2eA, "peers", "--repo", repo); strings.Contains(out, `"branch":"feat-a"`) {
		t.Fatalf("a live session reported on feat-a: %s", out)
	}
	// The ended session is not live.
	out, _, err = busJSON(t, sock, e2eA, "peers", "--session", e.SessionID)
	if err != nil || !strings.HasPrefix(out, `{"kind":"peers","peers":[],"total":0,"more":false,`) {
		t.Fatalf("ended session in presence: %s %v", out, err)
	}

	// 3. Send to the full id from history.
	out, _, err = busJSON(t, sock, e2eA, "send", m.SessionID, "--intent", "request", "--", "You committed "+movedSH+" on feat-a: does the cursor survive a page reload?")
	var rc sendJSON
	if err != nil || json.Unmarshal([]byte(out), &rc) != nil || rc.To.Session != moved || !rc.To.Live || rc.Arrives != arriveNextPrompt {
		t.Fatalf("send to the moved session: %s %v", out, err)
	}
	// To the ended one it waits; nothing wakes it, and it stays not live.
	out, _, err = busJSON(t, sock, e2eA, "send", e.SessionID, "--", "Your commit "+endedSH+" needs a follow-up.")
	if err != nil || json.Unmarshal([]byte(out), &rc) != nil || rc.To.Session != ended || rc.To.Live || rc.Arrives != arriveIfResumed || rc.State != busproto.StateQueued {
		t.Fatalf("send to the ended session: %s %v", out, err)
	}
	if out, _, _ := busJSON(t, sock, e2eA, "peers", "--session", ended); !strings.Contains(out, `"peers":[]`) {
		t.Fatalf("a message woke the ended session: %s", out)
	}
}

// busJSON runs a bus verb in the default JSON mode as session.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func busJSON(t *testing.T, sock, session string, args ...string) (string, string, error) {
	t.Helper()
	asCaller(t, &local.Caller{Agent: transcript.AgentClaude, SessionID: session, Rule: "test"})
	var out, errOut strings.Builder
	err := busCmd(t.Context(), args[0], append([]string{"--socket", sock}, args[1:]...), strings.NewReader(""), &out, &errOut)
	return out.String(), errOut.String(), err
}
