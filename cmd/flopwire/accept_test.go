package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/api"
	"github.com/flopwire/flopwire/internal/bus"
	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/devicebus"
	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// acceptRun runs an accept verb with a terminal (or not), the password
// typed at the prompt, and cfg as the client configuration.
func acceptRun(t *testing.T, terminal bool, cfg client.Config, cfgErr error, typed string, verb string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut strings.Builder
	err = acceptCmd(t.Context(), verb, args, acceptIO{password: func() (string, error) { return strings.TrimSuffix(typed, "\n"), nil }, out: &out, errOut: &errOut,
		terminal: func() bool { return terminal }, load: func() (client.Config, error) { return cfg, cfgErr }})
	return out.String(), errOut.String(), err
}

func errorCode(t *testing.T, stderr string) string {
	t.Helper()
	var e errorJSON
	// The JSON error follows the statement and prompt, when accept wrote
	// them.
	if i := strings.LastIndex(stderr, `{"kind":"error"`); i > 0 {
		stderr = stderr[i:]
	}
	if err := json.Unmarshal([]byte(stderr), &e); err != nil || e.Kind != "error" || e.Error == nil {
		t.Fatalf("not a JSON error: %q", stderr)
	}
	return e.Error.Code
}

// Without a terminal, every accept verb refuses before it reads anything
// from the server, says why, and points to the console.
func TestAcceptVerbsRequireATerminal(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	t.Cleanup(srv.Close)
	cfg := client.Config{Server: srv.URL, Token: "device-token", SessionToken: "session-token", DeviceID: "d1"}
	for _, c := range [][]string{{"accept", "alex@example.test"}, {"revoke", "alex@example.test"}, {"accepts"}} {
		_, stderr, err := acceptRun(t, false, cfg, nil, "accept\n", c[0], c[1:]...)
		if !errors.Is(err, errReported) || errorCode(t, stderr) != codeTerminalRequired || !strings.Contains(stderr, srv.URL+"/#messages") {
			t.Fatalf("%v without a terminal: %v %s", c, err, stderr)
		}
		_, _, err = acceptRun(t, false, cfg, nil, "accept\n", c[0], append([]string{"--text"}, c[1:]...)...)
		if err == nil || !strings.Contains(err.Error(), "needs a person at a terminal") || !strings.Contains(err.Error(), "web console at "+srv.URL+"/#messages") {
			t.Fatalf("%v --text without a terminal: %v", c, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("the server was called %d times without a terminal", hits.Load())
	}
}

// With no server there is one person, so nothing to accept.
func TestAcceptVerbsLocalOnly(t *testing.T) {
	out, _, err := acceptRun(t, true, client.Config{}, os.ErrNotExist, "", "accepts", "--text")
	if err != nil || !strings.Contains(out, "messaging is local to this device") {
		t.Fatalf("accepts local: %q %v", out, err)
	}
	out, _, err = acceptRun(t, true, client.Config{}, os.ErrNotExist, "", "accepts")
	if err != nil || !strings.Contains(out, `"local":true`) {
		t.Fatalf("accepts local JSON: %q %v", out, err)
	}
	for _, verb := range []string{"accept", "revoke"} {
		_, stderr, err := acceptRun(t, true, client.Config{}, os.ErrNotExist, "accept\n", verb, "alex")
		if !errors.Is(err, errReported) || errorCode(t, stderr) != codeLocalOnly {
			t.Fatalf("%s local: %v %s", verb, err, stderr)
		}
	}
}

// acceptServer is the real API and bus with gary (sender, a device with a
// live session) and alex (recipient, a login session only).
type acceptServer struct {
	url               string
	pool              *pgxpool.Pool
	gary, alex        string // login sessions
	garyDevice        string
	garyID, alexID    string
	adminID, adminTok string
}

func newAcceptServer(t *testing.T) acceptServer {
	t.Helper()
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
	s := acceptServer{url: srv.URL, pool: pool}
	s.adminID, s.adminTok = seedAdmin(t, pg)
	member := func(email string) (string, string) {
		invite := apiCall(t, srv.URL, s.adminTok, "POST", "/v1/admin/invites", map[string]string{"email": email})
		out := apiCall(t, srv.URL, "", "POST", "/v1/invites/claim", map[string]string{"code": invite["code"].(string), "name": email, "password": adminPassword})
		return out["token"].(string), out["user"].(map[string]any)["id"].(string)
	}
	s.gary, s.garyID = member("gary@example.test")
	s.alex, s.alexID = member("alex@example.test")
	s.garyDevice = apiCall(t, srv.URL, s.gary, "POST", "/v1/devices", map[string]string{"name": "mac", "platform": "darwin"})["token"].(string)
	apiCall(t, srv.URL, s.garyDevice, "POST", busproto.PathPoll, busproto.PollRequest{Sessions: []busproto.PresenceSession{{SessionID: "gary-1111", Agent: "claude", Repo: "/Users/gary/src/api", Branch: "main"}}})
	return s
}

// send sends from gary's session as gary's device.
func (s acceptServer) send(t *testing.T, to, body string) busproto.SendResponse {
	t.Helper()
	var out busproto.SendResponse
	if err := (client.HTTP{Server: s.url, Token: s.garyDevice}).JSON(context.Background(), "POST", busproto.PathSend, busproto.SendRequest{FromSession: "gary-1111", To: to, Body: body}, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAcceptVerbsAgainstTheServer(t *testing.T) {
	s := newAcceptServer(t)
	held := s.send(t, "@alex", "Can you review the pagination change?\nDetails in the PR.\x1b[31m")
	if held.State != busproto.StateHeld {
		t.Fatalf("send %+v", held)
	}
	alex := client.Config{Server: s.url, Token: "alex-device-unused", SessionToken: s.alex, DeviceID: "d-alex"}

	// The list shows the held sender with a preview, as text and JSON.
	out, _, err := acceptRun(t, true, alex, nil, "", "accepts", "--text")
	if err != nil || !strings.Contains(out, "held: 1 message from 1 person you have not accepted") || !strings.Contains(out, "gary@example.test  1 held") ||
		!strings.Contains(out, `claude api@main  inform  "Can you review the pagination change?"`) || strings.Contains(out, "Details in the PR") ||
		!strings.Contains(out, "accepted: nobody") || !strings.Contains(out, "console: "+s.url+"/#messages") {
		t.Fatalf("accepts --text:\n%s %v", out, err)
	}
	out, _, err = acceptRun(t, true, alex, nil, "", "accepts")
	var list acceptsJSON
	if err != nil || json.Unmarshal([]byte(out), &list) != nil || list.Kind != "accepts" || len(list.Held) != 1 || list.Held[0].Messages[0].ID != held.ID {
		t.Fatalf("accepts JSON: %s %v", out, err)
	}

	// accept states what accepting means and needs the person's password:
	// nothing typed cancels, and the word "accept" (or any wrong password)
	// does not accept. The saved login session alone never does.
	_, stderr, err := acceptRun(t, true, alex, nil, "\n", "accept", "gary")
	if !errors.Is(err, errReported) || !strings.Contains(strings.Join(strings.Fields(stderr), " "), acceptStatement) ||
		!strings.Contains(stderr, "gary@example.test has 1 held message") || !strings.Contains(stderr, "type your Flopwire password") {
		t.Fatalf("accept unconfirmed: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, `"code":"`+codeNotConfirmed+`"`) {
		t.Fatalf("unconfirmed code: %s", stderr)
	}
	_, stderr, err = acceptRun(t, true, alex, nil, "accept\n", "accept", "gary")
	if !errors.Is(err, errReported) || errorCode(t, stderr) != busproto.CodePasswordRequired {
		t.Fatalf("accept with a wrong password: %v\n%s", err, stderr)
	}
	var n int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM bus_accepts`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("accepted without the password: %d %v", n, err)
	}
	out, _, err = acceptRun(t, true, alex, nil, adminPassword+"\n", "accept", "--text", "gary")
	if err != nil || !strings.HasPrefix(out, "accepted gary@example.test: 1 held message released to your sessions") {
		t.Fatalf("accept: %q %v", out, err)
	}
	out, _, err = acceptRun(t, true, alex, nil, "", "accepts", "--text")
	if err != nil || !strings.Contains(out, "held: nothing") || !strings.Contains(out, "accepted: 1\n  gary@example.test  since ") {
		t.Fatalf("accepts after accept:\n%s %v", out, err)
	}
	// revoke is one step.
	queued := s.send(t, "@alex", "a second question")
	if queued.State != busproto.StateQueued {
		t.Fatalf("send after accept %+v", queued)
	}
	out, _, err = acceptRun(t, true, alex, nil, "", "revoke", "gary@example.test")
	var rev acceptJSON
	if err != nil || json.Unmarshal([]byte(out), &rev) != nil || rev.Kind != "revoke" || rev.Reheld != 2 || rev.Accepted {
		// Two: the released message (not delivered: alex has no device)
		// and the second one.
		t.Fatalf("revoke: %s %v", out, err)
	}

	// No login session (a device that never logged in), an expired one, a
	// minted token: login_required, pointing at login and the console.
	for name, cfg := range map[string]client.Config{
		"no session":      {Server: s.url, Token: "device-token", DeviceID: "d-alex"},
		"expired session": {Server: s.url, Token: "device-token", SessionToken: "not-a-session", DeviceID: "d-alex"},
		"device token":    {Server: s.url, Token: s.garyDevice, SessionToken: s.garyDevice, DeviceID: "d-gary"},
		"minted token":    {Server: s.url, Token: "minted", FromEnv: true},
	} {
		_, stderr, err := acceptRun(t, true, cfg, nil, "accept\n", "accept", "gary")
		if !errors.Is(err, errReported) || errorCode(t, stderr) != codeLoginRequired || !strings.Contains(stderr, "flopwire login") {
			t.Errorf("%s: %v %s", name, err, stderr)
		}
	}
	// An unknown person is a refusal, not an accept.
	_, stderr, err = acceptRun(t, true, alex, nil, adminPassword+"\n", "accept", "nobody@example.test")
	if !errors.Is(err, errReported) || errorCode(t, stderr) != busproto.CodeUnknownRecipient {
		t.Fatalf("unknown: %v %s", err, stderr)
	}
}

// e2eWait polls cond until it holds or 30 seconds pass.
func e2eWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A real cross-user exchange: gary's session messages alex's session
// through the real server handlers; alex's device runs the real agent and
// hook. Held: alex's device does not receive it, the hook gives the model
// nothing and alex (the person) one notice. Alex accepts on a terminal:
// the message is delivered once, sender="teammate". Alex revokes: a
// message queued before the revoke is held again before any hook prints
// it, and gary's next message is held. Gary's receipts and inbox say
// held, then delivered, then held, and nothing else about alex.
func TestAcceptEndToEndCrossUser(t *testing.T) {
	s := newAcceptServer(t)
	ctx := t.Context()
	enrolled := apiCall(t, s.url, s.alex, "POST", "/v1/devices", map[string]string{"name": "alex-mac", "platform": "darwin"})
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "flopwire", "config.json"))
	t.Setenv("FLOPWIRE_INDEX", filepath.Join(t.TempDir(), "index.db"))
	t.Setenv(client.EnvToken, "")
	t.Setenv(client.EnvServer, "")
	if err := client.Save(client.Config{Server: s.url, Token: enrolled["token"].(string), SessionToken: s.alex, DeviceID: enrolled["device"].(map[string]any)["id"].(string)}); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	writeClaudeSession(t, filepath.Join(home, ".claude", "projects"), e2eB, "/tmp/e2e-api", "add cursor to list endpoint")
	sock := filepath.Join(shortSockDir(t), "alex.sock")
	startAgent(t, home, sock)

	gary := client.HTTP{Server: s.url, Token: s.garyDevice}
	garyPoll := func() {
		apiCall(t, s.url, s.garyDevice, "POST", busproto.PathPoll, busproto.PollRequest{Sessions: []busproto.PresenceSession{{SessionID: "gary-1111", Agent: "claude", Repo: "/Users/gary/src/api", Branch: "main"}}})
	}
	e2eWait(t, "alex's session in presence", func() bool {
		garyPoll()
		var peers busproto.PeersResponse
		return gary.JSON(ctx, "GET", busproto.PathPeers+"?session=gary-1111", nil, &peers) == nil && len(peers.Peers) == 1 && peers.Peers[0].Session == e2eB
	})
	send := func(body string) busproto.SendResponse {
		t.Helper()
		var out busproto.SendResponse
		if err := gary.JSON(ctx, "POST", busproto.PathSend, busproto.SendRequest{FromSession: "gary-1111", To: "e2e0bbbb", Body: body}, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	sentState := func(id string) busproto.State {
		t.Helper()
		var in busproto.InboxResponse
		if err := gary.JSON(ctx, "GET", busproto.PathInbox+"?session=gary-1111&sent=true", nil, &in); err != nil {
			t.Fatal(err)
		}
		for _, m := range in.Messages {
			if m.ID == id {
				return m.State
			}
		}
		return ""
	}
	status := func() devicebus.Status {
		resp, err := agent.Call(ctx, sock, agent.Request{Op: "status"})
		if err != nil || resp.Bus == nil {
			return devicebus.Status{}
		}
		return *resp.Bus
	}
	hook := func(event string) hookOutput {
		t.Helper()
		var out, errOut strings.Builder
		hookCmd(ctx, []string{"--socket", sock}, strings.NewReader(hookFor(e2eB, event)), &out, &errOut, func(string) string { return "" })
		if out.Len() == 0 {
			return hookOutput{}
		}
		var o hookOutput
		if err := json.Unmarshal([]byte(out.String()), &o); err != nil {
			t.Fatalf("hook output %q", out.String())
		}
		return o
	}
	noModelMention := func(o hookOutput, step string) {
		t.Helper()
		if c := o.HookSpecificOutput.AdditionalContext; strings.Contains(c, "gary") || strings.Contains(strings.ToLower(c), "held") {
			t.Fatalf("%s: model context mentions the held message:\n%s", step, c)
		}
	}

	// 1. Held. The sender's receipt and inbox say held, with the reason.
	first := send("Can you switch the client to the cursor? It is in main now.")
	if first.State != busproto.StateHeld || first.Sender != busproto.SenderTeammate || first.To.Session != e2eB || arrival(first) != arriveAccepted ||
		!strings.Contains(sendOutcome(first), "alex has not accepted messages from you") {
		t.Fatalf("held receipt %+v: %s", first, sendOutcome(first))
	}
	if st := sentState(first.ID); st != busproto.StateHeld {
		t.Fatalf("sender's inbox: %s", st)
	}
	e2eWait(t, "the device to learn of the held sender", func() bool { return status().Held == 1 })
	if st := status(); st.Pending != 0 || len(st.HeldSenders) != 1 || st.HeldSenders[0].User != "gary@example.test" {
		t.Fatalf("status while held %+v", st)
	}
	o := hook(evUserPromptSubmit)
	noModelMention(o, "held")
	if o.HookSpecificOutput.AdditionalContext != "" || !strings.Contains(o.SystemMessage, "1 message from gary@example.test (1) is held until you accept the sender") ||
		!strings.Contains(o.SystemMessage, s.url+"/#messages") {
		t.Fatalf("held notice %+v", o)
	}
	for _, ev := range []string{evUserPromptSubmit, evPostToolUse} {
		if o := hook(ev); o != (hookOutput{}) {
			t.Fatalf("%s after the notice: %+v", ev, o)
		}
	}

	// 2. Alex accepts on a terminal with the login session.
	out, _, err := acceptRun(t, true, mustLoad(t), nil, adminPassword+"\n", "accept", "--text", "gary@example.test")
	if err != nil || !strings.HasPrefix(out, "accepted gary@example.test: 1 held message released") {
		t.Fatalf("accept: %q %v", out, err)
	}
	var got string
	e2eWait(t, "delivery after the accept", func() bool {
		o := hook(evPostToolUse)
		got = o.HookSpecificOutput.AdditionalContext
		return got != ""
	})
	if strings.Count(got, "<flopwire-message ") != 1 || !strings.Contains(got, `id="`+first.ID+`"`) || !strings.Contains(got, `sender="teammate"`) ||
		!strings.Contains(got, `user="gary@example.test"`) || !strings.Contains(got, "Can you switch the client to the cursor?") {
		t.Fatalf("delivered context:\n%s", got)
	}
	if o := hook(evPostToolUse); o != (hookOutput{}) {
		t.Fatalf("delivered twice: %+v", o)
	}
	e2eWait(t, "the delivery receipt", func() bool { return sentState(first.ID) == busproto.StateDelivered })

	// 3. A message queued before the revoke reaches alex's device but no
	// hook yet; the revoke holds it again and the device drops it.
	second := send("Also: please bump the client version.")
	if second.State != busproto.StateQueued {
		t.Fatalf("send after accept %+v", second)
	}
	e2eWait(t, "the second message in the device's inbox", func() bool { return status().Pending == 1 })
	out, _, err = acceptRun(t, true, mustLoad(t), nil, "", "revoke", "--text", "gary")
	if err != nil || !strings.HasPrefix(out, "revoked gary@example.test: 1 undelivered message held again") {
		t.Fatalf("revoke: %q %v", out, err)
	}
	// At once: the revoke wakes the device's poll (not its 25-second
	// timeout).
	revoked := time.Now()
	e2eWait(t, "the device to drop the re-held message", func() bool { st := status(); return st.Pending == 0 && st.Held == 1 })
	if d := time.Since(revoked); d > 10*time.Second {
		t.Fatalf("the device dropped the re-held message after %s", d)
	}
	for _, ev := range []string{evUserPromptSubmit, evPostToolUse} {
		o := hook(ev)
		noModelMention(o, "after revoke")
		if o.HookSpecificOutput.AdditionalContext != "" {
			t.Fatalf("%s after revoke delivered: %+v", ev, o)
		}
	}
	if st := sentState(second.ID); st != busproto.StateHeld {
		t.Fatalf("re-held message in the sender's inbox: %s", st)
	}
	third := send("Ping: did you see the version bump request?")
	if third.State != busproto.StateHeld || arrival(third) != arriveAccepted {
		t.Fatalf("send after revoke %+v", third)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action IN ('bus.accept','bus.revoke','bus.held') AND actor_id=$1`, s.alexID).Scan(&n); err != nil || n < 3 {
		t.Fatalf("accept, revoke and held reads audited: %d %v", n, err)
	}
}

func mustLoad(t *testing.T) client.Config {
	t.Helper()
	cfg, err := client.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// The console states what accepting means in the same words as the CLI.
func TestAcceptStatementMatchesTheConsole(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "messaging.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `"`+acceptStatement+`"`) {
		t.Fatalf("web/src/messaging.ts does not hold the CLI's statement:\n%s", acceptStatement)
	}
}
