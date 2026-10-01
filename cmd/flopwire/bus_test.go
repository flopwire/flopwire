package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/local"
	"github.com/flopwire/flopwire/internal/transcript"
)

// fakeAgent is a control socket that answers bus requests from a script
// and records them. Everything it holds is synthetic.
type fakeAgent struct {
	sock   string
	mu     sync.Mutex
	reqs   []agent.Request
	answer func(agent.Request) agent.Response
}

func startFakeAgent(t *testing.T, answer func(agent.Request) agent.Response) *fakeAgent {
	t.Helper()
	fa := &fakeAgent{sock: filepath.Join(shortSockDir(t), "a.sock"), answer: answer}
	ln, err := net.Listen("unix", fa.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, err := bufio.NewReader(c).ReadBytes('\n')
				if err != nil {
					return
				}
				var req agent.Request
				if json.Unmarshal(line, &req) != nil {
					return
				}
				fa.mu.Lock()
				fa.reqs = append(fa.reqs, req)
				ans := fa.answer
				fa.mu.Unlock()
				b, _ := json.Marshal(ans(req))
				c.Write(append(b, '\n'))
			}()
		}
	}()
	return fa
}

func (fa *fakeAgent) requests() []agent.Request {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	return append([]agent.Request{}, fa.reqs...)
}

func refused(be busproto.Error) agent.Response {
	return agent.Response{Error: be.Error(), BusError: &be}
}

const (
	selfID  = "0b7e2c1a-0000-4000-8000-0000000000aa"
	peerID  = "4c19e0d2-0000-4000-8000-0000000000bb"
	alexID  = "0b7e2c1a-0000-4000-8000-0000000000cc" // shares selfID's first 8 characters
	t0Stamp = "2026-10-01T14:02:11Z"
)

var t0, _ = time.Parse(time.RFC3339, t0Stamp)

// asCaller makes the CLI's detector find c (or none).
func asCaller(t *testing.T, c *local.Caller) {
	t.Helper()
	prev, prevRetry := busDetect, busRetry
	busDetect = func() func(context.Context) (local.Caller, bool) {
		return func(context.Context) (local.Caller, bool) {
			if c == nil {
				return local.Caller{}, false
			}
			return *c, true
		}
	}
	busRetry = 10 * time.Millisecond
	t.Cleanup(func() { busDetect, busRetry = prev, prevRetry })
}

var claudeSelf = &local.Caller{Agent: transcript.AgentClaude, SessionID: selfID, Rule: "test"}

// cli runs a bus verb against the fake agent and returns stdout.
func cli(t *testing.T, fa *fakeAgent, stdin string, args ...string) (string, error) {
	t.Helper()
	var out strings.Builder
	err := busCmd(t.Context(), args[0], append([]string{"--socket", fa.sock}, args[1:]...), strings.NewReader(stdin), &out)
	return out.String(), err
}

// The outcome line of every kind of send, as plan §3 prints them, plus
// the cases the plan leaves out and the redaction note.
func TestSendOutcomeLines(t *testing.T) {
	exp := t0.Add(24 * time.Hour)
	for _, c := range []struct {
		r    busproto.SendResponse
		want string
	}{
		{busproto.SendResponse{ID: "m7f3a", State: busproto.StateQueued, ExpiresAt: exp,
			To: busproto.Recipient{Session: "0b7e2c1a-1111", Agent: "claude", User: "alex@example.test", Repo: "/src/api", Branch: "main", Live: true, Busy: true}},
			"sent m7f3a to 0b7e2c1a (alex claude api@main): busy, arrives at its next tool call"},
		{busproto.SendResponse{ID: "m7f3b", State: busproto.StateQueued, ExpiresAt: exp,
			To: busproto.Recipient{Session: "4c19e0d2-2222", Agent: "codex", User: "gary@example.test", Repo: "/home/g/api", Branch: "main", Live: true}},
			"sent m7f3b to 4c19e0d2 (gary codex api@main): idle, arrives with its human's next prompt"},
		{busproto.SendResponse{ID: "m7f3c", State: busproto.StateHeld, ExpiresAt: exp, To: busproto.Recipient{User: "sam@example.test", Repo: "api", Live: true}},
			"held m7f3c for @sam: sam has not accepted messages from you; expires 2026-10-02T14:02Z"},
		{busproto.SendResponse{ID: "m7f3d", State: busproto.StateQueued, ExpiresAt: exp, To: busproto.Recipient{User: "alex@example.test", Repo: "api"}},
			"queued m7f3d for @alex: no live session on api; expires 2026-10-02T14:02Z"},
		{busproto.SendResponse{ID: "m7f3e", State: busproto.StateQueued, ExpiresAt: exp, To: busproto.Recipient{User: "alex@example.test", Repo: "api", Live: true, Busy: true}},
			"sent m7f3e to @alex: a busy session on api takes it, arrives at its next tool call"},
		{busproto.SendResponse{ID: "m7f3f", State: busproto.StateQueued, ExpiresAt: exp, To: busproto.Recipient{User: "alex@example.test", Live: true}},
			"sent m7f3f to @alex: an idle session takes it, arrives with its human's next prompt"},
		{busproto.SendResponse{ID: "m7f40", State: busproto.StateQueued, ExpiresAt: exp,
			To: busproto.Recipient{Session: "4c19e0d2-2222", Agent: "codex", User: "gary@example.test", Repo: "/home/g/api"}},
			"sent m7f40 to 4c19e0d2 (gary codex api): not running, arrives only if it resumes; expires 2026-10-02T14:02Z"},
		{busproto.SendResponse{ID: "m7f41", State: busproto.StateQueued, ExpiresAt: exp, Redactions: map[string]int{"github-token": 2, "aws-key": 1},
			To: busproto.Recipient{Session: "4c19e0d2-2222", Agent: "codex", User: "gary@example.test", Repo: "/src/api", Branch: "main", Live: true, Busy: true}},
			"sent m7f41 to 4c19e0d2 (gary codex api@main): busy, arrives at its next tool call; 3 secrets masked before it left this device (aws-key, github-token)"},
	} {
		if got := sendOutcome(c.r); got != c.want {
			t.Errorf("outcome\n got %s\nwant %s", got, c.want)
		}
	}
}

// send names the calling session as the sender, reads "-" from stdin, and
// prints one line; --json prints the response.
func TestSendCLI(t *testing.T) {
	asCaller(t, claudeSelf)
	fa := startFakeAgent(t, func(r agent.Request) agent.Response {
		return agent.Response{OK: true, Sent: &busproto.SendResponse{ID: "m01", State: busproto.StateQueued, ExpiresAt: t0,
			To: busproto.Recipient{Session: peerID, Agent: "codex", User: "gary@example.test", Repo: "/src/api", Branch: "main", Live: true, Busy: true}}}
	})
	out, err := cli(t, fa, "Heads-up: pagination is changing.\nUse the cursor.\n", "send", "4c19e0d2", "--intent", "request", "--ref", "4c19e0d2/28672", "--ref", "x/1", "--reply-to", "m00", "--", "-")
	if err != nil || out != "sent m01 to 4c19e0d2 (gary codex api@main): busy, arrives at its next tool call\n" {
		t.Fatalf("send: %q %v", out, err)
	}
	r := fa.requests()[0]
	if r.Op != "send" || r.Send.FromSession != selfID || r.Send.FromAgent != "claude" || r.Send.To != "4c19e0d2" || r.Send.Intent != "request" ||
		r.Send.ReplyTo != "m00" || strings.Join(r.Send.Refs, ",") != "4c19e0d2/28672,x/1" || r.Send.Body != "Heads-up: pagination is changing.\nUse the cursor." {
		t.Fatalf("request: %+v", r.Send)
	}
	// Text as arguments, without --.
	if _, err := cli(t, fa, "", "send", "@alex", "two", "words"); err != nil || fa.requests()[1].Send.Body != "two words" {
		t.Fatalf("argument text: %v %+v", err, fa.requests()[1].Send)
	}
	out, err = cli(t, fa, "", "send", "@alex", "--json", "--", "hi")
	var resp busproto.SendResponse
	if err != nil || json.Unmarshal([]byte(out), &resp) != nil || resp.ID != "m01" {
		t.Fatalf("json: %q %v", out, err)
	}
	// Caught before the agent: empty text, an oversized body, a bad intent.
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"send", "@alex", "--", ""}, "the message text is empty"},
		{[]string{"send", "@alex", "--", strings.Repeat("x", busproto.MaxBodyBytes+1)}, "the cap is 4000"},
		{[]string{"send", "@alex", "--intent", "ask", "--", "hi"}, "want request"},
		{[]string{"send"}, "Usage: flopwire send <to>"},
	} {
		n := len(fa.requests())
		if _, err := cli(t, fa, "", c.args...); err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "xample") {
			t.Errorf("%v: %v", c.args, err)
		}
		if len(fa.requests()) != n {
			t.Errorf("%v reached the agent", c.args)
		}
	}
}

// Every refusal prints its cause, the fix and a valid example; over MCP it
// is an isError result.
func TestSendRefusals(t *testing.T) {
	asCaller(t, claudeSelf)
	var be busproto.Error
	fa := startFakeAgent(t, func(agent.Request) agent.Response { return refused(be) })
	for _, c := range []struct {
		be   busproto.Error
		want []string
	}{
		{busproto.Error{Status: 429, Code: busproto.CodeThreadRate, Detail: "thread m1 had 8 messages in the last hour; the limit is 8", MessageID: "m9"},
			[]string{"refused (thread_rate): thread m1 had 8", "Fix: stop this exchange", "Example: flopwire inbox --thread", "The refused message m9 is listed in flopwire inbox --sent"}},
		{busproto.Error{Status: 429, Code: busproto.CodeSessionRate, Detail: "this session sent 30 messages in the last hour; the limit is 30"},
			[]string{"refused (session_rate)", "Fix: send less", "Example: flopwire inbox --sent"}},
		{busproto.Error{Status: 429, Code: busproto.CodeDeviceRate, Detail: "this device sent 120 messages in the last hour; the limit is 120"},
			[]string{"refused (device_rate)", "Fix: send less"}},
		{busproto.Error{Status: 429, Code: busproto.CodeUserRate, Detail: "you sent 300 messages in the last hour; the limit is 300"},
			[]string{"refused (user_rate)", "Fix: send less"}},
		{busproto.Error{Status: 409, Code: busproto.CodeDuplicate, Detail: "dropped: this session sent the same text to the same recipient in the last 10m0s"},
			[]string{"refused (duplicate)", "Fix: do not resend"}},
		{busproto.Error{Status: 409, Code: busproto.CodeRecipientFull, Detail: "the recipient has 50 undelivered messages; the limit is 50"},
			[]string{"refused (recipient_full)", "Fix: wait"}},
		{busproto.Error{Status: 409, Code: busproto.CodeReplyToDone, Detail: "m5 closed its thread (intent done); it must not be answered"},
			[]string{"refused (reply_to_done)", "Fix: do not answer it", `Example: flopwire send 0b7e2c1a -- "TEXT"`}},
		{busproto.Error{Status: 404, Code: busproto.CodeUnknownRecipient, Detail: "no session id starts with 0b7e2c1a; flopwire peers lists live sessions"},
			[]string{"refused (unknown_recipient)", "Fix: address a live session by an id prefix from flopwire peers", "Example: flopwire send @alex"}},
		{busproto.Error{Status: 409, Code: busproto.CodeAmbiguousRecipient, Detail: "0b7e2c1a matches 2 sessions; use a longer prefix", Candidates: []busproto.Candidate{
			{Session: selfID, Agent: "claude", User: "gary@example.test", Repo: "/src/api", Branch: "main", Title: "refactor client pagination", Live: true},
			{Session: alexID, Agent: "codex", User: "alex@example.test", Repo: "/src/web", Title: "old"}}},
			[]string{"refused (ambiguous_recipient): 0b7e2c1a matches 2 sessions", "candidates:\n  0b7e2c1a-0000-4000-8000-0000000000a  gary  claude  live  api@main  \"refactor client pagination\"\n  0b7e2c1a-0000-4000-8000-0000000000c  alex  codex  ended  web",
				`Example: flopwire send 0b7e2c1a-0000-4000-8000-0000000000a -- "TEXT"`}},
		{busproto.Error{Status: 404, Code: busproto.CodeNotFound, Detail: "reply_to m5: no such message sent or received by you"},
			[]string{"refused (not_found)", "Example: flopwire inbox lists them"}},
		{busproto.Error{Status: 403, Code: busproto.CodeSessionNotOnDevice, Detail: "session x is kept off the server by a path rule; it cannot use messaging"},
			[]string{"refused (session_not_on_device)", "its transcripts stay on this device"}},
		{busproto.Error{Status: 400, Code: busproto.CodeBadRequest, Detail: "a ref is an archive address of at most 512 bytes"},
			[]string{"refused (bad_request): a ref is", "Example: Usage: flopwire send"}},
	} {
		be = c.be
		_, err := cli(t, fa, "", "send", "0b7e2c1a", "--", "hi")
		for _, w := range c.want {
			if err == nil || !strings.Contains(err.Error(), w) {
				t.Errorf("%s: want %q in\n%v", c.be.Code, w, err)
			}
		}
		// The same over MCP: isError, and MCP-form hints.
		r := &retriever{caller: func(context.Context) (local.Caller, bool) { return *claudeSelf, true }, busSocket: fa.sock}
		resp := mcpRoundTrip(t, r, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"flopwire_send","arguments":{"to":"0b7e2c1a","message":"hi"}}}`)
		if !strings.Contains(resp, `"isError":true`) || !strings.Contains(resp, "refused ("+c.be.Code+")") || strings.Contains(resp, "flopwire inbox --sent") {
			t.Errorf("%s over MCP: %s", c.be.Code, resp)
		}
	}
}

// A session the agent has not seen yet is retried once; a path-rule
// refusal is not.
func TestSendRetriesASessionNotSeenYet(t *testing.T) {
	asCaller(t, claudeSelf)
	n := 0
	fa := startFakeAgent(t, func(r agent.Request) agent.Response {
		n++
		if n == 1 {
			return refused(busproto.Error{Status: 403, Code: busproto.CodeSessionNotOnDevice, Detail: "session x is not indexed on this device yet; try again in a few seconds"})
		}
		return agent.Response{OK: true, Sent: &busproto.SendResponse{ID: "m02", State: busproto.StateQueued, To: busproto.Recipient{User: "a@x.test", Live: true}}}
	})
	if out, err := cli(t, fa, "", "send", "@a", "--", "hi"); err != nil || !strings.HasPrefix(out, "sent m02") || len(fa.requests()) != 2 {
		t.Fatalf("retry: %q %v %d", out, err, len(fa.requests()))
	}
}

// Without an identified calling session, send and inbox refuse (a message
// always names its session); peers still lists, and says the caller may be
// among the rows.
func TestBusWithoutCaller(t *testing.T) {
	asCaller(t, nil)
	fa := startFakeAgent(t, func(r agent.Request) agent.Response {
		return agent.Response{OK: true, Peers: &busproto.PeersResponse{Peers: []busproto.Peer{{Session: peerID, Agent: "codex", User: "gary@example.test", Own: true}}}}
	})
	for _, verb := range []string{"send", "inbox"} {
		args := []string{verb}
		if verb == "send" {
			args = append(args, "@gary", "--", "hi")
		}
		if _, err := cli(t, fa, "", args...); err == nil || !strings.Contains(err.Error(), "cannot identify the calling session") || !strings.Contains(err.Error(), "FLOPWIRE_SESSION_ID") {
			t.Fatalf("%s without a caller: %v", verb, err)
		}
	}
	if len(fa.requests()) != 0 {
		t.Fatalf("an anonymous request reached the agent: %+v", fa.requests())
	}
	out, err := cli(t, fa, "", "peers")
	if err != nil || !strings.HasPrefix(out, "[your own session could not be identified") || !strings.Contains(out, "4c19e0d2  gary  codex") {
		t.Fatalf("peers without a caller: %q %v", out, err)
	}
	if q := fa.requests()[0].Peers; q.Session != "" {
		t.Fatalf("peers named a session: %+v", q)
	}
}

// With no agent on the socket, every verb says the agent is not running
// and how to start it.
func TestBusAgentNotRunning(t *testing.T) {
	asCaller(t, claudeSelf)
	sock := filepath.Join(shortSockDir(t), "none.sock")
	for _, args := range [][]string{{"peers"}, {"send", "@a", "--", "hi"}, {"inbox"}} {
		var out strings.Builder
		err := busCmd(t.Context(), args[0], append([]string{"--socket", sock}, args[1:]...), strings.NewReader(""), &out)
		if err == nil || !strings.Contains(err.Error(), "device agent is not running") || !strings.Contains(err.Error(), "flopwire agent run") {
			t.Fatalf("%v: %v", args, err)
		}
	}
	// An agent with messaging off says so.
	fa := startFakeAgent(t, func(agent.Request) agent.Response { return agent.Response{Error: "messaging is off in this agent"} })
	if _, err := cli(t, fa, "", "peers"); err == nil || !strings.Contains(err.Error(), "messaging off") {
		t.Fatalf("messaging off: %v", err)
	}
}

// peers prints the sessions header shape, the caller's own person first,
// the caller left out (by the agent, and here again), with a footer; the
// budget cuts whole rows and says how to narrow.
func TestPeersOutput(t *testing.T) {
	asCaller(t, claudeSelf)
	peers := []busproto.Peer{
		{Session: "9d00e0d2-0000-4000-8000-000000000001", Agent: "claude", User: "alex@example.test", Repo: "/src/api", Branch: "main", Title: "refactor client\npagination", Busy: true},
		{Session: peerID, Agent: "codex", User: "gary@example.test", Repo: "/home/g/api", Branch: "main", Title: "add cursor to list endpoint", Own: true},
		{Session: selfID, Agent: "claude", User: "gary@example.test", Own: true},
	}
	fa := startFakeAgent(t, func(agent.Request) agent.Response {
		return agent.Response{OK: true, Peers: &busproto.PeersResponse{Peers: peers}}
	})
	out, err := cli(t, fa, "", "peers", "--repo", "api", "--user", "@alex", "--agent", "claude")
	want := `4c19e0d2  gary  codex  live idle  api@main  "add cursor to list endpoint"
9d00e0d2  alex  claude  live busy  api@main  "refactor client pagination"
[2 live sessions (1 yours); busy: a message arrives at its next tool call; idle: with its human's next prompt. Address one by its first column, or a person as @user]
`
	if err != nil || out != want {
		t.Fatalf("peers:\n%s\nwant:\n%s%v", out, want, err)
	}
	if q := fa.requests()[0].Peers; q.Session != selfID || q.Repo != "api" || q.User != "alex" || q.Agent != "claude" {
		t.Fatalf("query %+v", q)
	}
	// The budget keeps whole rows.
	var many []busproto.Peer
	for i := range 400 {
		many = append(many, busproto.Peer{Session: fmt.Sprintf("%08x-0000", i), Agent: "claude", User: "alex@example.test", Repo: "/src/api", Title: strings.Repeat("t", 70)})
	}
	peers = many
	var b strings.Builder
	c := &busClient{socket: fa.sock, caller: func(context.Context) (local.Caller, bool) { return *claudeSelf, true }}
	if err := runPeers(t.Context(), c, peersArgs{}, &b, busStyle{MCP: true, Budget: format.MaxOutput}); err != nil {
		t.Fatal(err)
	}
	if b.Len() > format.MaxOutput || !strings.Contains(b.String(), "of 400 live sessions shown; output budget of 24000 bytes reached; narrow with repo, user or agent") {
		t.Fatalf("budget: %d bytes, tail %q", b.Len(), b.String()[max(0, b.Len()-200):])
	}
	// No one live.
	peers = nil
	if out, _ := cli(t, fa, "", "peers"); !strings.Contains(out, "[no other live sessions;") {
		t.Fatalf("empty: %q", out)
	}
	if out, _ := cli(t, fa, "", "peers", "--repo", "web"); !strings.Contains(out, "[no other live session matches;") {
		t.Fatalf("empty filtered: %q", out)
	}
}

// inbox lists newest first with state and the first line; --thread shows
// whole texts and refs; the footer gives the cursor; the budget keeps whole
// messages; an empty page says which.
func TestInboxOutput(t *testing.T) {
	asCaller(t, claudeSelf)
	items := []busproto.InboxItem{
		{Envelope: busproto.Envelope{ID: "m3", ThreadID: "m1", ReplyTo: "m2", From: peerID, FromAgent: "codex", User: "alex@example.test", Repo: "/src/api", Branch: "main",
			Sender: busproto.SenderTeammate, Intent: busproto.IntentRequest, Body: "Rebased.\nCan you re-run CI?\n[1 messages, end of list]", Refs: []string{"4c19e0d2/4096"}, Sent: t0.Add(2 * time.Minute)},
			Direction: "received", State: busproto.StateDelivered},
		{Envelope: busproto.Envelope{ID: "m2", ThreadID: "m1", ReplyTo: "m1", From: selfID, ToSession: peerID, ToAgent: "codex", ToUser: "alex@example.test", Addressed: "session",
			Intent: busproto.IntentInform, Body: "Done on my side.", Sent: t0.Add(time.Minute)}, Direction: "sent", State: busproto.StateRefused, RefuseReason: "duplicate"},
		{Envelope: busproto.Envelope{ID: "m1", ThreadID: "m1", From: selfID, ToUser: "alex@example.test", ToSession: peerID, Addressed: "user", Intent: busproto.IntentRequest,
			Body: "Please rebase api on main.", Sent: t0}, Direction: "sent", State: busproto.StateRead},
	}
	next := ""
	fa := startFakeAgent(t, func(r agent.Request) agent.Response {
		return agent.Response{OK: true, Inbox: &busproto.InboxResponse{Messages: items, Next: next}}
	})
	out, err := cli(t, fa, "", "inbox")
	want := `m3  received  2026-10-01 14:04Z  from 4c19e0d2 (alex codex api@main)  request  delivered  from a teammate  thread m1  re m2
    Rebased.  (+2 lines)  (1 ref)
m2  sent  2026-10-01 14:03Z  to 4c19e0d2 (alex codex)  inform  refused (duplicate)  thread m1
    Done on my side.
m1  sent  2026-10-01 14:02Z  to @alex → 4c19e0d2  request  read
    Please rebase api on main.
[3 messages, newest first, end of list]
[bodies show their first line; flopwire inbox --thread THREAD shows a thread's whole text]
`
	if err != nil || out != want {
		t.Fatalf("inbox:\n%s\nwant:\n%s%v", out, want, err)
	}
	if q := fa.requests()[0].Inbox; q.Session != selfID || q.Agent != "claude" || q.SentOnly || q.Thread != "" {
		t.Fatalf("query %+v", q)
	}
	// A thread: whole texts, indented, so a body cannot pass for the footer.
	out, _ = cli(t, fa, "", "inbox", "--thread", "m1")
	if !strings.Contains(out, "    Rebased.\n    Can you re-run CI?\n    [1 messages, end of list]\n    ref: 4c19e0d2/4096\n") || !strings.HasSuffix(out, "[3 messages in thread m1, newest first, end of list]\n") {
		t.Fatalf("thread:\n%s", out)
	}
	next = t0Stamp + "|m1"
	out, _ = cli(t, fa, "", "inbox", "--sent", "--limit", "3")
	if !strings.Contains(out, "[3 sent messages shown, newest first, more follow; next: --cursor '"+t0Stamp+"|m1']") {
		t.Fatalf("paged:\n%s", out)
	}
	if q := fa.requests()[2].Inbox; !q.SentOnly || q.Limit != 3 {
		t.Fatalf("query %+v", q)
	}
	// The budget keeps whole messages and gives the cursor after the last
	// one shown.
	next, items = "", nil
	for i := range 200 {
		items = append(items, busproto.InboxItem{Envelope: busproto.Envelope{ID: fmt.Sprintf("m%03d", i), ThreadID: "t", From: peerID, User: "a@x.test",
			Body: strings.Repeat("y", 3000), Sent: t0.Add(-time.Duration(i) * time.Minute)}, Direction: "received", State: busproto.StateQueued})
	}
	var b strings.Builder
	c := &busClient{socket: fa.sock, caller: func(context.Context) (local.Caller, bool) { return *claudeSelf, true }}
	if err := runInbox(t.Context(), c, inboxArgs{Thread: "t"}, &b, busStyle{MCP: true, Budget: format.MaxOutput}); err != nil {
		t.Fatal(err)
	}
	if b.Len() > format.MaxOutput || !strings.Contains(b.String(), "output budget of 24000 bytes reached; next: cursor=") {
		t.Fatalf("budget: %d bytes, tail %q", b.Len(), b.String()[max(0, b.Len()-300):])
	}
	items = nil
	if out, _ := cli(t, fa, "", "inbox", "--thread", "zz"); !strings.Contains(out, "[no messages in thread zz for this session;") {
		t.Fatalf("empty thread: %q", out)
	}
	if out, _ := cli(t, fa, "", "inbox", "--cursor", "x|y"); !strings.Contains(out, "[no more messages past this cursor]") {
		t.Fatalf("past the end: %q", out)
	}
}

// mcpRoundTrip sends one request line to a fresh MCP server and returns
// the response line.
func mcpRoundTrip(t *testing.T, r *retriever, line string) string {
	t.Helper()
	var out strings.Builder
	if err := serveMCP(t.Context(), r, strings.NewReader(line+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out.String())
}

// Codex starts MCP servers with a scrubbed environment; each tools/call
// names its thread in _meta. That id is the sender (over the detector),
// for one call only.
func TestMCPMetaNamesTheCodexThread(t *testing.T) {
	busRetry = 10 * time.Millisecond
	fa := startFakeAgent(t, func(r agent.Request) agent.Response {
		return agent.Response{OK: true, Sent: &busproto.SendResponse{ID: "m03", State: busproto.StateQueued, To: busproto.Recipient{User: "a@x.test", Live: true}}}
	})
	r := &retriever{caller: func(context.Context) (local.Caller, bool) { return *claudeSelf, true }, busSocket: fa.sock}
	const thread = "019a0000-0000-7000-8000-0000000000cd"
	for i, meta := range []string{
		`{"threadId":"` + thread + `"}`,
		`{"x-codex-turn-metadata":{"thread_id":"` + thread + `","turn_id":"t1"}}`,
		`{"x-codex-turn-metadata":"{\"thread_id\":\"` + thread + `\"}"}`,
	} {
		resp := mcpRoundTrip(t, r, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"flopwire_send","arguments":{"to":"@a","message":"hi"},"_meta":`+meta+`}}`)
		if strings.Contains(resp, "isError") || !strings.Contains(resp, "sent m03") {
			t.Fatalf("meta %d: %s", i, resp)
		}
		if s := fa.requests()[i].Send; s.FromSession != thread || s.FromAgent != "codex" {
			t.Fatalf("meta %d: sender %+v", i, s)
		}
	}
	// Without _meta (or with a malformed id) the detector decides.
	mcpRoundTrip(t, r, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"flopwire_send","arguments":{"to":"@a","message":"hi"},"_meta":{"threadId":"a b"}}}`)
	if s := fa.requests()[3].Send; s.FromSession != selfID || s.FromAgent != "claude" {
		t.Fatalf("no meta: sender %+v", s)
	}
	// An unknown argument names the ones the tool takes; a wrong type says
	// what it wants.
	resp := mcpRoundTrip(t, r, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"flopwire_send","arguments":{"to":"@a","body":"hi"}}}`)
	if !strings.Contains(resp, `"isError":true`) || !strings.Contains(resp, `unknown argument \"body\"; it takes to, message`) {
		t.Fatalf("unknown argument: %s", resp)
	}
	resp = mcpRoundTrip(t, r, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"flopwire_inbox","arguments":{"limit":"5"}}}`)
	if !strings.Contains(resp, `"isError":true`) || !strings.Contains(resp, "limit wants an integer") {
		t.Fatalf("bad type: %s", resp)
	}
	// The instructions carry the messaging guidance.
	resp = mcpRoundTrip(t, r, `{"jsonrpc":"2.0","id":5,"method":"initialize","params":{}}`)
	if !strings.Contains(resp, "do not poll flopwire_peers") {
		t.Fatalf("instructions: %s", resp)
	}
}

// The MCP side of peers and inbox: the same text as the CLI, with MCP
// hints.
func TestMCPPeersAndInbox(t *testing.T) {
	fa := startFakeAgent(t, func(r agent.Request) agent.Response {
		if r.Op == "peers" {
			return agent.Response{OK: true, Peers: &busproto.PeersResponse{Peers: []busproto.Peer{{Session: peerID, Agent: "codex", User: "gary@example.test", Own: true}}}}
		}
		return agent.Response{OK: true, Inbox: &busproto.InboxResponse{Messages: []busproto.InboxItem{}, Next: ""}}
	})
	r := &retriever{caller: func(context.Context) (local.Caller, bool) { return *claudeSelf, true }, busSocket: fa.sock}
	text, err := mcpCall(t.Context(), r, "flopwire_peers", map[string]any{"repo": "api"})
	if err != nil || !strings.HasPrefix(text, "4c19e0d2  gary  codex  live idle") {
		t.Fatalf("peers: %q %v", text, err)
	}
	text, err = mcpCall(t.Context(), r, "flopwire_inbox", map[string]any{"sent": true, "format": "json"})
	if err != nil || !strings.Contains(text, `"messages": []`) {
		t.Fatalf("inbox json: %q %v", text, err)
	}
	if _, err := mcpCall(t.Context(), r, "flopwire_nope", nil); err == nil || !strings.Contains(err.Error(), "flopwire_inbox, flopwire_peers, flopwire_read") {
		t.Fatalf("unknown tool: %v", err)
	}
}
