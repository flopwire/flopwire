package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/flopwire/flopwire/internal/api"
	"github.com/flopwire/flopwire/internal/bus"
	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// acceptRun runs an accept verb with a terminal (or not), the typed
// input, and cfg as the client configuration.
func acceptRun(t *testing.T, terminal bool, cfg client.Config, cfgErr error, typed string, verb string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut strings.Builder
	err = acceptCmd(t.Context(), verb, args, acceptIO{in: strings.NewReader(typed), out: &out, errOut: &errOut,
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

	// accept states what accepting means and needs the word typed.
	_, stderr, err := acceptRun(t, true, alex, nil, "yes\n", "accept", "gary")
	if !errors.Is(err, errReported) || !strings.Contains(strings.Join(strings.Fields(stderr), " "), acceptStatement) ||
		!strings.Contains(stderr, "gary@example.test has 1 held message") || !strings.Contains(stderr, "Type accept to confirm") {
		t.Fatalf("accept unconfirmed: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, `"code":"`+codeNotConfirmed+`"`) {
		t.Fatalf("unconfirmed code: %s", stderr)
	}
	var n int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM bus_accepts`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("accepted without confirmation: %d %v", n, err)
	}
	out, _, err = acceptRun(t, true, alex, nil, "accept\n", "accept", "--text", "gary")
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
	_, stderr, err = acceptRun(t, true, alex, nil, "accept\n", "accept", "nobody@example.test")
	if !errors.Is(err, errReported) || errorCode(t, stderr) != busproto.CodeUnknownRecipient {
		t.Fatalf("unknown: %v %s", err, stderr)
	}
}
