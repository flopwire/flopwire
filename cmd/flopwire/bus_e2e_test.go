package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/api"
	"github.com/flopwire/flopwire/internal/bus"
	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/pgtest"
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
	var out strings.Builder
	err := busCmd(t.Context(), args[0], append([]string{"--socket", sock}, args[1:]...), strings.NewReader(stdin), &out)
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
	text, err = mcpCall(t.Context(), r, "flopwire_inbox", map[string]any{"thread": id})
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
	apiCall(t, srv.URL, alex, "POST", busproto.PathAccepts, busproto.AcceptRequest{Sender: "gary@example.test"})
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
