package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/api"
	"github.com/flopwire/flopwire/internal/bus"
	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// busServer is a server with Postgres and the message bus, an admin, and
// two members (gary and alex) with login sessions and enrolled devices.
type busServer struct {
	url        string
	s          store.Store
	admin      string
	gary, alex member
}

func newBusServer(t *testing.T) busServer {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), pgtest.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = store.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	s := store.NewPostgres(pool, nil, "")
	srv := newServer(t, s, api.Config{Bus: &bus.Store{Pool: pool}})
	b := busServer{url: srv.URL, s: s, admin: seedAdmin(t, s)}
	b.gary, b.alex = newMember(t, b.url, b.admin, "gary@example.test"), newMember(t, b.url, b.admin, "alex@example.test")
	return b
}

// busCall sends a bus request and decodes the answer into out (when not
// nil); it returns the status and the problem code.
func busCall(t *testing.T, method, url, token string, body, out any) (int, string) {
	t.Helper()
	var raw []byte
	if body != nil {
		raw = mustJSON(t, body)
	}
	res := request(t, method, url, raw, merge(bearer(token), map[string]string{"Content-Type": "application/json"}))
	defer res.Body.Close()
	var all json.RawMessage
	decodeResponse(t, res, &all)
	var p struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(all, &p)
	if out != nil {
		if err := json.Unmarshal(all, out); err != nil {
			t.Fatal(err)
		}
	}
	return res.StatusCode, p.Code
}

func presence(id, agent string, busy bool) busproto.PollRequest {
	return busproto.PollRequest{Sessions: []busproto.PresenceSession{{SessionID: id, Agent: agent, Repo: "/src/api", Branch: "main", Busy: busy}}}
}

// One server for both, since enrolling members is slow (argon2 under
// -race); the two use separate sessions and audit actions.
func TestBusOverHTTP(t *testing.T) {
	b := newBusServer(t)
	t.Run("credentials", func(t *testing.T) { testBusCredentials(t, b) })
	t.Run("send poll ack", func(t *testing.T) { testBusSendPollAck(t, b) })
}

func testBusCredentials(t *testing.T, b busServer) {
	if st, _ := busCall(t, "POST", b.url+busproto.PathPoll, b.gary.device, presence("gary-1111", "claude", true), nil); st != 200 {
		t.Fatalf("device poll %d", st)
	}
	// A login session is not a device: no bus traffic.
	for _, c := range []struct{ method, path string }{
		{"POST", busproto.PathSend}, {"POST", busproto.PathPoll}, {"POST", busproto.PathClaim}, {"POST", busproto.PathAck},
		{"GET", busproto.PathPeers}, {"GET", busproto.PathInbox + "?session=gary-1111"},
	} {
		if st, code := busCall(t, c.method, b.url+c.path, b.gary.session, map[string]any{}, nil); st != 403 || code != busproto.CodeDeviceRequired {
			t.Errorf("%s %s with a login session: %d %s", c.method, c.path, st, code)
		}
	}
	// A device token cannot accept or revoke: an agent could use it.
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"GET", busproto.PathAccepts, nil},
		{"POST", busproto.PathAccepts, busproto.AcceptRequest{Sender: "alex", Password: memberPassword}},
		{"DELETE", busproto.PathAccepts + "/alex", nil},
	} {
		if st, code := busCall(t, c.method, b.url+c.path, b.gary.device, c.body, nil); st != 403 || code != busproto.CodeLoginRequired {
			t.Errorf("%s %s with a device token: %d %s", c.method, c.path, st, code)
		}
	}
	// A minted token (a sandbox) is not an enrolled device.
	_, minted := mint(t, b.url, b.gary.session, map[string]any{"label": "ci", "scopes": []string{"upload", "read"}})
	if st, code := busCall(t, "POST", b.url+busproto.PathPoll, minted["token"].(string), presence("ci-1", "claude", true), nil); st != 403 || code != busproto.CodeDeviceRequired {
		t.Errorf("minted token poll: %d %s", st, code)
	}
	var acc busproto.AcceptResponse
	if st, _ := busCall(t, "POST", b.url+busproto.PathAccepts, b.gary.session, busproto.AcceptRequest{Sender: "alex", Password: memberPassword}, &acc); st != 200 || !acc.Accepted || acc.User != "alex@example.test" {
		t.Fatalf("accept with a login session: %d %+v", st, acc)
	}
	var list busproto.AcceptsResponse
	if st, _ := busCall(t, "GET", b.url+busproto.PathAccepts, b.gary.session, nil, &list); st != 200 || len(list.Accepted) != 1 {
		t.Fatalf("accepts: %d %+v", st, list)
	}
	if st, _ := busCall(t, "DELETE", b.url+busproto.PathAccepts+"/alex@example.test", b.gary.session, nil, &acc); st != 200 || acc.Accepted {
		t.Fatalf("revoke: %d %+v", st, acc)
	}
	// alex's device cannot send as gary's session.
	busCall(t, "POST", b.url+busproto.PathPoll, b.alex.device, presence("alex-2222", "codex", false), nil)
	if st, code := busCall(t, "POST", b.url+busproto.PathSend, b.alex.device, busproto.SendRequest{FromSession: "gary-1111", To: "alex-2222", Body: "spoof"}, nil); st != 403 || code != busproto.CodeSessionNotOnDevice {
		t.Fatalf("send as another device's session: %d %s", st, code)
	}
	failed := 0
	for _, e := range auditMeta(t, b.s, "authorization.failed") {
		switch e.Metadata["reason"] {
		case busproto.CodeDeviceRequired, busproto.CodeLoginRequired, busproto.CodeSessionNotOnDevice:
			failed++
		}
	}
	if failed != 11 {
		t.Fatalf("authorization failures audited: %d", failed)
	}
	if len(auditMeta(t, b.s, "bus.accept")) != 1 || len(auditMeta(t, b.s, "bus.revoke")) != 1 {
		t.Fatal("accept and revoke audited")
	}
}

func TestBusWithoutBackendAnswers501(t *testing.T) {
	s := store.NewMemory()
	srv := newServer(t, s, api.Config{})
	admin := seedAdmin(t, s)
	device := postJSON(t, srv.URL+"/v1/devices", map[string]string{"name": "mac", "platform": "darwin"}, bearer(admin))["token"].(string)
	if st, _ := busCall(t, "POST", srv.URL+busproto.PathPoll, device, presence("s-1", "claude", true), nil); st != 501 {
		t.Fatalf("poll without a bus: %d", st)
	}
}

func testBusSendPollAck(t *testing.T, b busServer) {
	busCall(t, "POST", b.url+busproto.PathPoll, b.gary.device, presence("gary-1111", "claude", true), nil)
	alexPresence := presence("alex-2222", "codex", false)
	var first busproto.PollResponse
	busCall(t, "POST", b.url+busproto.PathPoll, b.alex.device, alexPresence, &first)
	if st, _ := busCall(t, "POST", b.url+busproto.PathAccepts, b.alex.session, busproto.AcceptRequest{Sender: "gary", Password: memberPassword}, nil); st != 200 {
		t.Fatal("accept")
	}
	// The accept moved alex's generation: the next poll answers at once.
	busCall(t, "POST", b.url+busproto.PathPoll, b.alex.device, alexPresence, &first)
	// alex's device waits; gary's send wakes it.
	type polled struct {
		st  int
		out busproto.PollResponse
	}
	done := make(chan polled, 1)
	go func() {
		req := alexPresence
		req.Cursor, req.Gen, req.WaitSeconds = first.Cursor, first.Gen, 20
		var out busproto.PollResponse
		st, _ := busCall(t, "POST", b.url+busproto.PathPoll, b.alex.device, req, &out)
		done <- polled{st, out}
	}()
	time.Sleep(200 * time.Millisecond)
	var sent busproto.SendResponse
	st, _ := busCall(t, "POST", b.url+busproto.PathSend, b.gary.device, busproto.SendRequest{FromSession: "gary-1111", To: "alex", Body: "hello alex", Intent: "request"}, &sent)
	if st != 201 || sent.State != busproto.StateQueued || sent.To.Session != "alex-2222" || sent.To.User != "alex@example.test" || !sent.To.Live || sent.Sender != "teammate" {
		t.Fatalf("send %d %+v", st, sent)
	}
	var got polled
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("long poll not woken by the send")
	}
	if got.st != 200 || len(got.out.Messages) != 1 || got.out.Messages[0].ID != sent.ID || got.out.Messages[0].User != "gary@example.test" || got.out.Messages[0].Intent != "request" {
		t.Fatalf("woken poll %d %+v", got.st, got.out)
	}
	var ack busproto.AckResponse
	if st, _ := busCall(t, "POST", b.url+busproto.PathAck, b.alex.device, busproto.AckRequest{IDs: []string{sent.ID}}, &ack); st != 200 || len(ack.Acked) != 1 {
		t.Fatalf("ack %d %+v", st, ack)
	}
	// Refusals carry their code and the stored message's id.
	var refused busproto.Error
	if st, code := busCall(t, "POST", b.url+busproto.PathSend, b.gary.device, busproto.SendRequest{FromSession: "gary-1111", To: "alex-2", Body: "hello alex"}, &refused); st != 409 || code != busproto.CodeDuplicate || refused.MessageID == "" {
		t.Fatalf("duplicate %d %+v", st, refused)
	}
	var unknown busproto.Error
	if st, code := busCall(t, "POST", b.url+busproto.PathSend, b.gary.device, busproto.SendRequest{FromSession: "gary-1111", To: "nobody", Body: "x"}, &unknown); st != 404 || code != busproto.CodeUnknownRecipient {
		t.Fatalf("unknown %d %+v", st, unknown)
	}
	var peers busproto.PeersResponse
	if st, _ := busCall(t, "GET", b.url+busproto.PathPeers+"?session=gary-1111", b.gary.device, nil, &peers); st != 200 || len(peers.Peers) != 1 || peers.Peers[0].Session != "alex-2222" {
		t.Fatalf("peers %d %+v", st, peers)
	}
	var inbox busproto.InboxResponse
	if st, _ := busCall(t, "GET", b.url+busproto.PathInbox+"?session=gary-1111&sent=true", b.gary.device, nil, &inbox); st != 200 || len(inbox.Messages) != 2 ||
		inbox.Messages[0].State != busproto.StateRefused || inbox.Messages[1].State != busproto.StateDelivered {
		t.Fatalf("inbox %d %+v", st, inbox)
	}
	if st, _ := busCall(t, "GET", b.url+busproto.PathInbox+"?session=gary-1111&limit=x", b.gary.device, nil, nil); st != 400 {
		t.Fatalf("bad limit %d", st)
	}
	for action, want := range map[string]int{"bus.send": 2, "bus.poll": 1, "bus.deliver": 1, "bus.peers": 1, "bus.inbox": 1} {
		if n := len(auditMeta(t, b.s, action)); n != want {
			t.Errorf("%s audited %d times, want %d", action, n, want)
		}
	}
	for _, e := range auditMeta(t, b.s, "bus.send") {
		if strings.Contains(strings.ToLower(e.TargetID+jsonString(t, e.Metadata)), "hello alex") {
			t.Fatalf("audit holds the body: %+v", e)
		}
	}
	// A device that gives up on its poll leaves the server healthy.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var now busproto.PollResponse
	busCall(t, "POST", b.url+busproto.PathPoll, b.alex.device, busproto.PollRequest{Sessions: []busproto.PresenceSession{}}, &now)
	req, _ := http.NewRequestWithContext(ctx, "POST", b.url+busproto.PathPoll, strings.NewReader(fmt.Sprintf(`{"sessions":[],"cursor":999999,"gen":%d,"wait_seconds":20}`, now.Gen)))
	req.Header.Set("Authorization", "Bearer "+b.alex.device)
	if res, err := http.DefaultClient.Do(req); err == nil {
		res.Body.Close()
		t.Fatalf("poll answered before its wait: %d", res.StatusCode)
	}
}

func jsonString(t *testing.T, v any) string { return string(mustJSON(t, v)) }

// The held list and the accept routes, by credential: only a person's own
// login session reads or changes their acceptances, and no route names
// another person, so another member and an administrator see and change
// only their own.
func TestBusAcceptAuthorizationMatrix(t *testing.T) {
	b := newBusServer(t)
	busCall(t, "POST", b.url+busproto.PathPoll, b.gary.device, presence("gary-1111", "claude", true), nil)
	busCall(t, "POST", b.url+busproto.PathPoll, b.alex.device, presence("alex-2222", "codex", false), nil)
	var sent busproto.SendResponse
	if st, _ := busCall(t, "POST", b.url+busproto.PathSend, b.gary.device, busproto.SendRequest{FromSession: "gary-1111", To: "alex-2222", Body: "secret plan: rotate the keys"}, &sent); st != 201 || sent.State != busproto.StateHeld {
		t.Fatalf("send %d %+v", st, sent)
	}
	_, minted := mint(t, b.url, b.alex.session, map[string]any{"label": "ci", "scopes": []string{"upload", "read"}})
	svc := postJSON(t, b.url+"/v1/admin/service-accounts", map[string]string{"name": "collector"}, bearer(b.admin))
	refused := map[string]string{
		"recipient's device token": b.alex.device,
		"minted token":             minted["token"].(string),
		"service account":          svc["token"].(string),
	}
	routes := []struct {
		method, path string
		body         any
	}{
		{"GET", busproto.PathHeld, nil},
		{"GET", busproto.PathAccepts, nil},
		{"POST", busproto.PathAccepts, busproto.AcceptRequest{Sender: "gary", Password: memberPassword}},
		{"DELETE", busproto.PathAccepts + "/gary", nil},
	}
	for name, token := range refused {
		for _, r := range routes {
			var raw json.RawMessage
			st, code := busCall(t, r.method, b.url+r.path, token, r.body, &raw)
			if st != 403 || (code != busproto.CodeLoginRequired && code != "") || strings.Contains(string(raw), "secret plan") {
				t.Errorf("%s %s %s: %d %s %s", name, r.method, r.path, st, code, raw)
			}
		}
	}
	// Another member and an administrator see their own (empty) held list,
	// never alex's, and their accept changes only their own acceptances.
	for name, token := range map[string]string{"another member": b.gary.session, "administrator": b.admin} {
		var held busproto.HeldResponse
		if st, _ := busCall(t, "GET", b.url+busproto.PathHeld, token, nil, &held); st != 200 || len(held.Senders) != 0 {
			t.Errorf("%s held: %d %+v", name, st, held)
		}
	}
	if st, _ := busCall(t, "POST", b.url+busproto.PathAccepts, b.admin, busproto.AcceptRequest{Sender: "gary", Password: "correct horse battery staple"}, nil); st != 200 {
		t.Fatalf("admin accepts for themself: %d", st)
	}
	var acc busproto.AcceptsResponse
	if busCall(t, "GET", b.url+busproto.PathAccepts, b.alex.session, nil, &acc); len(acc.Accepted) != 0 || len(acc.Held) != 1 {
		t.Fatalf("the admin's accept changed alex's: %+v", acc)
	}
	// The recipient's own login session sees the preview, and accepts.
	var held busproto.HeldResponse
	if st, _ := busCall(t, "GET", b.url+busproto.PathHeld, b.alex.session, nil, &held); st != 200 || len(held.Senders) != 1 ||
		held.Senders[0].User != "gary@example.test" || held.Senders[0].Messages[0].ID != sent.ID || held.Senders[0].Messages[0].Preview != "secret plan: rotate the keys" {
		t.Fatalf("recipient held: %d %+v", st, held)
	}
	var out busproto.AcceptResponse
	if st, _ := busCall(t, "POST", b.url+busproto.PathAccepts, b.alex.session, busproto.AcceptRequest{Sender: "gary", Password: memberPassword}, &out); st != 200 || out.Released != 1 {
		t.Fatalf("recipient accept: %d %+v", st, out)
	}
	// Every read is audited with what it showed.
	heldReads := auditMeta(t, b.s, "bus.held")
	if len(heldReads) != 3 {
		t.Fatalf("bus.held audited %d times", len(heldReads))
	}
	found := false
	for _, e := range heldReads {
		if ids, _ := e.Metadata["result_ids"].([]any); len(ids) == 1 && ids[0] == sent.ID {
			found = true
		}
	}
	if !found || len(auditMeta(t, b.s, "bus.accepts")) != 1 || len(auditMeta(t, b.s, "bus.accept")) != 2 {
		t.Fatalf("audit: held %+v, accepts %d, accept %d", heldReads, len(auditMeta(t, b.s, "bus.accepts")), len(auditMeta(t, b.s, "bus.accept")))
	}
}

// A login session alone does not accept a sender: the session saved by
// `flopwire login` is readable by every process of the person's OS user,
// an agent included. Accepting needs the person's password, typed by
// them, with each attempt counted against the login rate limit.
func TestBusAcceptNeedsThePassword(t *testing.T) {
	b := newBusServer(t)
	busCall(t, "POST", b.url+busproto.PathPoll, b.gary.device, presence("gary-1111", "claude", true), nil)
	busCall(t, "POST", b.url+busproto.PathPoll, b.alex.device, presence("alex-2222", "codex", false), nil)
	var sent busproto.SendResponse
	if st, _ := busCall(t, "POST", b.url+busproto.PathSend, b.gary.device, busproto.SendRequest{FromSession: "gary-1111", To: "alex-2222", Body: "held"}, &sent); st != 201 || sent.State != busproto.StateHeld {
		t.Fatalf("send %d %+v", st, sent)
	}
	accepted := func() int {
		var acc busproto.AcceptsResponse
		busCall(t, "GET", b.url+busproto.PathAccepts, b.alex.session, nil, &acc)
		return len(acc.Accepted)
	}
	for name, pw := range map[string]string{"no password": "", "a wrong password": "guess"} {
		var raw json.RawMessage
		st, code := busCall(t, "POST", b.url+busproto.PathAccepts, b.alex.session, busproto.AcceptRequest{Sender: "gary", Password: pw}, &raw)
		if st != 403 || code != busproto.CodePasswordRequired || accepted() != 0 || strings.Contains(string(raw), "guess") {
			t.Fatalf("%s: %d %s %s, accepted %d", name, st, code, raw, accepted())
		}
	}
	var out busproto.AcceptResponse
	if st, _ := busCall(t, "POST", b.url+busproto.PathAccepts, b.alex.session, busproto.AcceptRequest{Sender: "gary", Password: memberPassword}, &out); st != 200 || out.Released != 1 || accepted() != 1 {
		t.Fatalf("accept with the password: %d %+v", st, out)
	}
	// The password never reaches the audit log.
	for _, e := range auditMeta(t, b.s, "authorization.failed") {
		if strings.Contains(fmt.Sprint(e.Metadata), "guess") {
			t.Fatalf("password in the audit: %+v", e.Metadata)
		}
	}
	// Guessing is limited like login: the attempts share its bucket.
	limited := false
	for range 12 {
		if st, _ := busCall(t, "POST", b.url+busproto.PathAccepts, b.alex.session, busproto.AcceptRequest{Sender: "gary", Password: "guess"}, nil); st == 429 {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("password guesses on accept are not rate limited")
	}
}
