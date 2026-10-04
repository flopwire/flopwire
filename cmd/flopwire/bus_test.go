package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/devicebus"
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
	// --repo reads the local index's placements: never this machine's.
	t.Setenv("FLOPWIRE_INDEX", filepath.Join(t.TempDir(), "missing.db"))
	// ... and so does the client config with the path rules beside it,
	// unless the test set its own.
	if _, set := os.LookupEnv("FLOPWIRE_CONFIG"); !set {
		t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	}
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
				if req.Op == "root" {
					// Every command first asks which session a caller belongs
					// to (busClient.self); these callers are sessions.
					c.Write([]byte(`{"ok":true}` + "\n"))
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

// cli runs a bus verb with --text against the fake agent and returns
// stdout; a failure is the readable error.
func cli(t *testing.T, fa *fakeAgent, stdin string, args ...string) (string, error) {
	t.Helper()
	var out, errOut strings.Builder
	err := busCmd(t.Context(), args[0], append([]string{"--socket", fa.sock, "--text"}, args[1:]...), strings.NewReader(stdin), &out, &errOut)
	if errOut.Len() > 0 {
		t.Fatalf("--text wrote to stderr: %s", errOut.String())
	}
	return out.String(), err
}

// cliJSON runs a bus verb in the default (JSON) mode and returns stdout
// and stderr.
func cliJSON(t *testing.T, fa *fakeAgent, stdin string, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut strings.Builder
	err := busCmd(t.Context(), args[0], append([]string{"--socket", fa.sock}, args[1:]...), strings.NewReader(stdin), &out, &errOut)
	return out.String(), errOut.String(), err
}

// jsonErr decodes the JSON error a failed command wrote to stderr.
func jsonErr(t *testing.T, stderr string, err error) busErr {
	t.Helper()
	var e struct {
		Kind  string `json:"kind"`
		Error busErr `json:"error"`
	}
	if !errors.Is(err, errReported) || strings.Count(stderr, "\n") != 1 || json.Unmarshal([]byte(stderr), &e) != nil || e.Kind != "error" || e.Error.Code == "" {
		t.Fatalf("want one JSON error on stderr and errReported; got %q %v", stderr, err)
	}
	return e.Error
}

// mcpContent is the text content of a JSON-RPC tools/call response, and
// whether it is an error.
func mcpContent(t *testing.T, resp string) (string, bool, map[string]any) {
	t.Helper()
	var r struct {
		Result struct {
			IsError    bool             `json:"isError"`
			Content    []map[string]any `json:"content"`
			Structured map[string]any   `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(resp), &r); err != nil || len(r.Result.Content) != 1 {
		t.Fatalf("response %s: %v", resp, err)
	}
	text, _ := r.Result.Content[0]["text"].(string)
	return text, r.Result.IsError, r.Result.Structured
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
		{busproto.SendResponse{ID: "m7f42", State: busproto.StateQueued, ExpiresAt: exp,
			To: busproto.Recipient{Session: "session_01AbCd", Agent: "claude", User: "gary@example.test", Repo: "acme/api", Branch: "claude/fix", Live: true, Busy: true, Cloud: true}},
			"sent m7f42 to session_01AbCd (gary claude cloud api@claude/fix): running, pushed now and read at its next tool call; a cloud session cannot reply"},
		{busproto.SendResponse{ID: "m7f43", State: busproto.StateQueued, ExpiresAt: exp,
			To: busproto.Recipient{Session: "devin-0a1b2c3d", Agent: "devin", User: "gary@example.test", Live: true, Cloud: true}},
			"sent m7f43 to devin-0a1b2c3d (gary devin cloud -): not running a turn, pushed when it next runs one; a cloud session cannot reply; expires 2026-10-02T14:02Z"},
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
	// The default is the JSON receipt; --json is accepted and changes
	// nothing.
	for _, extra := range [][]string{nil, {"--json"}} {
		out, stderr, err := cliJSON(t, fa, "", append(append([]string{"send", "@alex"}, extra...), "--", "hi")...)
		var rc sendJSON
		if err != nil || stderr != "" || strings.Count(out, "\n") != 1 || json.Unmarshal([]byte(out), &rc) != nil || rc.Kind != "send_receipt" || rc.ID != "m01" ||
			rc.State != busproto.StateQueued || rc.To.Session != peerID || rc.Arrives != arriveNextToolCall || rc.From.Session != selfID || rc.From.Agent != "claude" ||
			rc.Outcome != "sent m01 to 4c19e0d2 (gary codex api@main): busy, arrives at its next tool call" {
			t.Fatalf("json %v: %q %q %v", extra, out, stderr, err)
		}
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
			[]string{"refused (unknown_recipient)", "Fix: find the session from history (flopwire sessions --repo R --branch B --json), check it is live with flopwire peers --session ID", "Example: flopwire send @alex"}},
		{busproto.Error{Status: 409, Code: busproto.CodeAmbiguousRecipient, Detail: "0b7e2c1a matches 2 sessions; use a longer prefix", Candidates: []busproto.Candidate{
			{Session: selfID, Agent: "claude", User: "gary@example.test", Repo: "/src/api", Branch: "main", Title: "refactor client pagination", Live: true},
			{Session: alexID, Agent: "codex", User: "alex@example.test", Repo: "/src/web", Title: "old"}}},
			[]string{"refused (ambiguous_recipient): 0b7e2c1a matches 2 sessions", "candidates:\n  0b7e2c1a-0000-4000-8000-0000000000a  gary  claude  live  api@main  \"refactor client pagination\"\n  0b7e2c1a-0000-4000-8000-0000000000c  alex  codex  ended  web",
				`Example: flopwire send 0b7e2c1a-0000-4000-8000-0000000000a -- "TEXT"`}},
		{busproto.Error{Status: 404, Code: busproto.CodeNotFound, Detail: "reply_to m5: no such message sent or received by you"},
			[]string{"refused (not_found)", "Example: flopwire inbox lists them"}},
		{busproto.Error{Status: 403, Code: busproto.CodeSessionNotOnDevice, Detail: "session x is kept off the server by a path rule; it cannot use messaging"},
			[]string{"refused (session_not_on_device)", "its transcripts stay on this device"}},
		{busproto.Error{Status: 403, Code: busproto.CodeWithheldSession, Detail: `ref "5ec2e7aa/3" names session 5ec2e7aa-1, which a path rule keeps off the server; nothing about it may reach the team server`},
			[]string{"refused (withheld_session): ref \"5ec2e7aa/3\"", "Fix: send it without that ref", `Example: flopwire send 0b7e2c1a -- "TEXT"`}},
		{busproto.Error{Status: 403, Code: busproto.CodeSessionNotOnDevice, Detail: "session x is not indexed on this device yet (a new session); retry in a few seconds"},
			[]string{"refused (session_not_on_device): session x is not indexed", "Fix: the device agent has not indexed this session yet (a new session); retry"}},
		{busproto.Error{Status: 403, Code: busproto.CodeWithheldRepo, Detail: `repo "/src/client" is kept off the server by a path rule; nothing about it may reach the team server`},
			[]string{"refused (withheld_repo): repo \"/src/client\"", "Fix: leave out --repo", `Example: flopwire send 0b7e2c1a -- "TEXT"`}},
		{busproto.Error{Status: 400, Code: busproto.CodeBadRequest, Detail: "a ref is an archive address of at most 512 bytes"},
			[]string{"refused (bad_request): a ref is", "Fix: check the arguments", `Example: flopwire send 0b7e2c1a -- "Heads-up`}},
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
		text, isErr, _ := mcpContent(t, resp)
		var je struct {
			Kind  string `json:"kind"`
			Error busErr `json:"error"`
		}
		if !isErr || json.Unmarshal([]byte(text), &je) != nil || je.Kind != "error" || je.Error.Code != c.be.Code || !je.Error.Refused || je.Error.Detail != c.be.Detail ||
			je.Error.Fix == "" || je.Error.Example == "" || je.Error.MessageID != c.be.MessageID || len(je.Error.Candidates) != len(c.be.Candidates) || strings.Contains(text, "flopwire inbox --sent") {
			t.Errorf("%s over MCP: %s", c.be.Code, text)
		}
		// format=text: the readable error.
		resp = mcpRoundTrip(t, r, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"flopwire_send","arguments":{"to":"0b7e2c1a","message":"hi","format":"text"}}}`)
		if text, isErr, _ := mcpContent(t, resp); !isErr || !strings.HasPrefix(text, "refused ("+c.be.Code+")") {
			t.Errorf("%s over MCP as text: %s", c.be.Code, text)
		}
		// The CLI default: the same JSON error on stderr, exit 1.
		_, stderr, err := cliJSON(t, fa, "", "send", "0b7e2c1a", "--", "hi")
		if e := jsonErr(t, stderr, err); e.Code != c.be.Code || e.Detail != c.be.Detail || !e.Refused {
			t.Errorf("%s JSON: %+v", c.be.Code, e)
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
		var errOut strings.Builder
		err := busCmd(t.Context(), args[0], append([]string{"--socket", sock, "--text"}, args[1:]...), strings.NewReader(""), &out, &errOut)
		if err == nil || !strings.Contains(err.Error(), "device agent is not running") || !strings.Contains(err.Error(), "flopwire agent run") {
			t.Fatalf("%v: %v", args, err)
		}
		// The default: a JSON error with a stable code.
		out.Reset()
		err = busCmd(t.Context(), args[0], append([]string{"--socket", sock}, args[1:]...), strings.NewReader(""), &out, &errOut)
		if e := jsonErr(t, errOut.String(), err); e.Code != codeAgentNotRunning || e.Fix == "" || e.Example == "" || out.Len() != 0 {
			t.Fatalf("%v JSON: %+v %q", args, e, out.String())
		}
		errOut.Reset()
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
	// A cloud session is marked, and the footer says what that means.
	peers = append(peers, busproto.Peer{Session: "session_01AbCdEf", Agent: "claude", User: "gary@example.test", Repo: "acme/api", Branch: "claude/fix", Title: "cloud task", Own: true, Cloud: true})
	out, err = cli(t, fa, "", "peers", "--text")
	if err != nil || !strings.Contains(out, "session_01AbCdEf  gary  claude  cloud idle  api@claude/fix  \"cloud task\"\n") ||
		!strings.Contains(out, "; cloud: a vendor cloud session, which gets a message pushed while busy and cannot reply.") {
		t.Fatalf("peers with a cloud session:\n%s%v", out, err)
	}
	peers = peers[:3]
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
	if err := runPeers(t.Context(), c, peersArgs{Limit: 500}, &b, busStyle{MCP: true, Budget: format.MaxOutput}); err != nil {
		t.Fatal(err)
	}
	if b.Len() > format.MaxOutput || !strings.Contains(b.String(), " of 400 shown (output budget of 24000 bytes); narrow with repo, user, agent or session]") {
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

// A ref is written on one indented line in a thread: one that holds a
// newline (from a server or an older device) cannot pass for a header.
func TestInboxThreadRefOneLine(t *testing.T) {
	asCaller(t, claudeSelf)
	items := []busproto.InboxItem{{Envelope: busproto.Envelope{ID: "m1", ThreadID: "m1", From: peerID, FromAgent: "codex", User: "alex@example.test",
		Intent: busproto.IntentInform, Body: "See the ref.", Refs: []string{"4c19e0d2/1\n## fake-header\tm9  received\r\n[end]"}, Sent: t0},
		Direction: "received", State: busproto.StateDelivered}}
	fa := startFakeAgent(t, func(r agent.Request) agent.Response {
		return agent.Response{OK: true, Inbox: &busproto.InboxResponse{Messages: items}}
	})
	out, err := cli(t, fa, "", "inbox", "--thread", "m1")
	if err != nil || !strings.Contains(out, "\n    ref: 4c19e0d2/1 ## fake-header m9 received [end]\n") {
		t.Fatalf("thread:\n%s %v", out, err)
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "##") || strings.HasPrefix(l, "[end]") || strings.HasPrefix(l, "\t") {
			t.Fatalf("a ref passes for a line of its own:\n%s", out)
		}
	}
}

// inbox lists newest first with state and the first line; --thread shows
// whole texts and refs; the footer gives the cursor; the budget keeps whole
// messages; an empty page says which.
func TestInboxOutput(t *testing.T) {
	asCaller(t, claudeSelf)
	readT := t0.Add(90 * time.Second)
	items := []busproto.InboxItem{
		{Envelope: busproto.Envelope{ID: "m3", ThreadID: "m1", ReplyTo: "m2", From: peerID, FromAgent: "codex", User: "alex@example.test", Repo: "/src/api", Branch: "main",
			Sender: busproto.SenderTeammate, Intent: busproto.IntentRequest, Body: "Rebased.\nCan you re-run CI?\n[1 messages, end of list]", Refs: []string{"4c19e0d2/4096"}, Sent: t0.Add(2 * time.Minute)},
			Direction: "received", State: busproto.StateDelivered},
		{Envelope: busproto.Envelope{ID: "m2", ThreadID: "m1", ReplyTo: "m1", From: selfID, ToSession: peerID, ToAgent: "codex", ToUser: "alex@example.test", Addressed: "session",
			Intent: busproto.IntentInform, Body: "Done on my side.", Sent: t0.Add(time.Minute)}, Direction: "sent", State: busproto.StateRefused, Reason: "duplicate"},
		{Envelope: busproto.Envelope{ID: "m1", ThreadID: "m1", From: selfID, ToUser: "alex@example.test", ToSession: peerID, Addressed: "user", Intent: busproto.IntentRequest,
			Body: "Please rebase api on main.", Sent: t0}, Direction: "sent", State: busproto.StateRead, DeliveredAt: &readT, ReadAt: &readT},
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
m1  sent  2026-10-01 14:02Z  to @alex → 4c19e0d2  request  read 2026-10-01 14:03Z
    Please rebase api on main.
[3 messages, newest first, end of list]
[bodies show their first line; flopwire inbox --text --thread THREAD shows a thread's whole text]
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
// names its thread in _meta. When Codex launched the server, that id is
// the sender (over the detector), for one call only.
func TestMCPMetaNamesTheCodexThread(t *testing.T) {
	busRetry = 10 * time.Millisecond
	fa := startFakeAgent(t, func(r agent.Request) agent.Response {
		return agent.Response{OK: true, Sent: &busproto.SendResponse{ID: "m03", State: busproto.StateQueued, To: busproto.Recipient{User: "a@x.test", Live: true}}}
	})
	r := &retriever{caller: func(context.Context) (local.Caller, bool) { return *claudeSelf, true }, busSocket: fa.sock, underCodex: true}
	const thread = "019a0000-0000-7000-8000-0000000000cd"
	for i, meta := range []string{
		`{"threadId":"` + thread + `"}`,
		`{"x-codex-turn-metadata":{"thread_id":"` + thread + `","turn_id":"t1"}}`,
		`{"x-codex-turn-metadata":"{\"thread_id\":\"` + thread + `\"}"}`,
	} {
		resp := mcpRoundTrip(t, r, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"flopwire_send","arguments":{"to":"@a","message":"hi"},"_meta":`+meta+`}}`)
		text, isErr, _ := mcpContent(t, resp)
		var rc sendJSON
		if isErr || json.Unmarshal([]byte(text), &rc) != nil || rc.Kind != "send_receipt" || rc.ID != "m03" || rc.From.Session != thread {
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
	if text, isErr, _ := mcpContent(t, resp); !isErr || !strings.Contains(text, `unknown argument \"body\"; it takes to, message`) || !strings.Contains(text, `"code":"bad_request"`) {
		t.Fatalf("unknown argument: %s", text)
	}
	resp = mcpRoundTrip(t, r, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"flopwire_inbox","arguments":{"limit":"5"}}}`)
	if text, isErr, _ := mcpContent(t, resp); !isErr || !strings.Contains(text, "limit wants an integer") {
		t.Fatalf("bad type: %s", text)
	}
	// The instructions carry the messaging guidance.
	resp = mcpRoundTrip(t, r, `{"jsonrpc":"2.0","id":5,"method":"initialize","params":{}}`)
	if !strings.Contains(resp, "do not poll flopwire_peers") {
		t.Fatalf("instructions: %s", resp)
	}
}

// Any MCP client can put a Codex thread id in _meta. Unless Codex launched
// the server, it is ignored and the detector names the sender (issue #71).
func TestMCPMetaIgnoredWithoutCodex(t *testing.T) {
	busRetry = 10 * time.Millisecond
	fa := startFakeAgent(t, func(r agent.Request) agent.Response {
		return agent.Response{OK: true, Sent: &busproto.SendResponse{ID: "m03", State: busproto.StateQueued, To: busproto.Recipient{User: "a@x.test", Live: true}}}
	})
	r := &retriever{caller: func(context.Context) (local.Caller, bool) { return *claudeSelf, true }, busSocket: fa.sock}
	const thread = "019a0000-0000-7000-8000-0000000000cd"
	for i, meta := range []string{
		`{"threadId":"` + thread + `"}`,
		`{"x-codex-turn-metadata":{"thread_id":"` + thread + `"}}`,
	} {
		resp := mcpRoundTrip(t, r, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"flopwire_send","arguments":{"to":"@a","message":"hi"},"_meta":`+meta+`}}`)
		if _, isErr, _ := mcpContent(t, resp); isErr {
			t.Fatalf("meta %d: %s", i, resp)
		}
		if s := fa.requests()[i].Send; s.FromSession != selfID || s.FromAgent != "claude" {
			t.Fatalf("meta %d: sender %+v, want the detected Claude session", i, s)
		}
	}
	// Nor does a detector that finds nothing fall back to it.
	r.caller = func(context.Context) (local.Caller, bool) { return local.Caller{}, false }
	resp := mcpRoundTrip(t, r, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"flopwire_send","arguments":{"to":"@a","message":"hi"},"_meta":{"threadId":"`+thread+`"}}}`)
	if text, isErr, _ := mcpContent(t, resp); !isErr || !strings.Contains(text, codeNoCaller) || len(fa.requests()) != 2 {
		t.Fatalf("no caller: %s", text)
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
	text, err := mcpCall(t.Context(), r, "flopwire_peers", map[string]any{"repo": "api", "format": "text"})
	if err != nil || !strings.HasPrefix(text, "4c19e0d2  gary  codex  live idle") {
		t.Fatalf("peers: %q %v", text, err)
	}
	text, err = mcpCall(t.Context(), r, "flopwire_peers", map[string]any{"repo": "api"})
	var pj peersJSON
	if err != nil || json.Unmarshal([]byte(text), &pj) != nil || pj.Kind != "peers" || len(pj.Peers) != 1 || pj.Peers[0].Session != peerID {
		t.Fatalf("peers json: %q %v", text, err)
	}
	text, err = mcpCall(t.Context(), r, "flopwire_inbox", map[string]any{"sent": true})
	if err != nil || text != `{"kind":"inbox","session":"`+selfID+`","messages":[],"more":false}` {
		t.Fatalf("inbox json: %q %v", text, err)
	}
	if _, err := mcpCall(t.Context(), r, "flopwire_nope", nil); err == nil || !strings.Contains(err.Error(), "flopwire_inbox, flopwire_peers, flopwire_read") {
		t.Fatalf("unknown tool: %v", err)
	}
}

// peers --repo naming a repo the path rules keep off the server is
// refused with what to do instead.
func TestPeersWithheldRepoRefused(t *testing.T) {
	asCaller(t, claudeSelf)
	fa := startFakeAgent(t, func(agent.Request) agent.Response {
		return refused(busproto.Error{Status: 403, Code: busproto.CodeWithheldRepo, Detail: `repo "client" is kept off the server by a path rule; nothing about it may reach the team server`})
	})
	_, err := cli(t, fa, "", "peers", "--repo", "client")
	if err == nil || !strings.Contains(err.Error(), "refused (withheld_repo)") || !strings.Contains(err.Error(), "Fix: leave out --repo") {
		t.Fatalf("peers: %v", err)
	}
}

// A caller the agent will not name to the server (a path rule, or not
// seen yet) still gets peers: the agent is asked without the session, and
// the caller is left out here.
func TestPeersWithoutNamingAWithheldCaller(t *testing.T) {
	asCaller(t, claudeSelf)
	fa := startFakeAgent(t, func(r agent.Request) agent.Response {
		if r.Peers.Session != "" {
			return refused(busproto.Error{Status: 403, Code: busproto.CodeSessionNotOnDevice, Detail: "session x is kept off the server by a path rule; it cannot use messaging"})
		}
		return agent.Response{OK: true, Peers: &busproto.PeersResponse{Peers: []busproto.Peer{{Session: selfID, Agent: "claude", User: "g@x.test", Own: true}, {Session: peerID, Agent: "codex", User: "g@x.test", Own: true}}}}
	})
	out, err := cli(t, fa, "", "peers")
	if err != nil || strings.Contains(out, "0b7e2c1a") || !strings.HasPrefix(out, "4c19e0d2  g  codex") {
		t.Fatalf("peers: %q %v", out, err)
	}
}

// awkward is text with spaces, quotes, backslashes, Unicode and a newline.
const awkward = "say \"hi\" to O'Brien — naïve café 🚀\\path\twith tab\nsecond line"

// Every verb prints one compact JSON object by default, in busproto's
// field names with full session ids; strings with spaces, quotes and
// Unicode round-trip exactly; the fields say whether more follows.
func TestBusJSONDefaultRoundTrips(t *testing.T) {
	asCaller(t, claudeSelf)
	seen := t0.Add(-time.Second).UTC()
	peer := busproto.Peer{Session: peerID, Agent: "codex", User: "gary@example.test", UserID: "u-1", UserName: "Gary \"G\" Ü", Device: "mac mini",
		Repo: "/src/my repo \"x\"/ünï", Branch: "feat/naïve space", Title: awkward, Busy: true, Own: true, SeenAt: seen}
	other := busproto.Peer{Session: "9d00e0d2-0000-4000-8000-000000000001", Agent: "claude", User: "alex@example.test", SeenAt: seen}
	item := busproto.InboxItem{Envelope: busproto.Envelope{ID: "m3", ThreadID: "m1", ReplyTo: "m1", From: peerID, FromAgent: "codex", User: "gary@example.test",
		UserID: "u-1", Repo: peer.Repo, Branch: peer.Branch, Sender: busproto.SenderOwn, Intent: busproto.IntentRequest, Body: awkward, Refs: []string{"4c19e0d2/4096:2"},
		Sent: t0, ExpiresAt: t0.Add(24 * time.Hour), ToSession: selfID, ToAgent: "claude", ToUser: "gary@example.test", ToUserID: "u-1", Addressed: "session", Seq: 7},
		Direction: "received", State: busproto.StateDelivered}
	sent := busproto.SendResponse{ID: "m9", ThreadID: "m1", State: busproto.StateQueued, Sender: busproto.SenderOwn, Intent: busproto.IntentInform, Sent: t0, ExpiresAt: t0.Add(24 * time.Hour),
		To: busproto.Recipient{Session: peerID, Agent: "codex", User: "gary@example.test", UserID: "u-1", Repo: peer.Repo, Branch: peer.Branch, Live: true}}
	fa := startFakeAgent(t, func(r agent.Request) agent.Response {
		switch r.Op {
		case "peers":
			return agent.Response{OK: true, Peers: &busproto.PeersResponse{Peers: []busproto.Peer{other, peer}}}
		case "send":
			return agent.Response{OK: true, Sent: &sent}
		}
		return agent.Response{OK: true, Inbox: &busproto.InboxResponse{Messages: []busproto.InboxItem{item}, Next: t0Stamp + "|m3"}}
	})

	out, stderr, err := cliJSON(t, fa, "", "peers", "--limit", "1")
	var pj peersJSON
	if err != nil || stderr != "" || strings.Count(out, "\n") != 1 || json.Unmarshal([]byte(out), &pj) != nil {
		t.Fatalf("peers: %q %q %v", out, stderr, err)
	}
	if pj.Kind != "peers" || len(pj.Peers) != 1 || pj.Total != 2 || !pj.More || pj.Limit != 1 || pj.Caller == nil || pj.Caller.Session != selfID || !strings.Contains(pj.Hint, "--limit") {
		t.Fatalf("peers fields: %s", out)
	}
	if got := pj.Peers[0]; got != peer {
		t.Fatalf("peer did not round-trip:\n got %+v\nwant %+v", got, peer)
	}
	// --session narrows to one exact session, by its full id or a prefix.
	out, _, _ = cliJSON(t, fa, "", "peers", "--session", peerID)
	if json.Unmarshal([]byte(out), &pj) != nil || len(pj.Peers) != 1 || pj.Peers[0].Session != peerID || pj.More {
		t.Fatalf("peers --session: %s", out)
	}
	out, _, _ = cliJSON(t, fa, "", "peers", "--session", "ffff")
	if out != `{"kind":"peers","peers":[],"total":0,"more":false,"limit":50,"caller":{"session":"`+selfID+`","agent":"claude"}}`+"\n" {
		t.Fatalf("peers --session not live: %s", out)
	}

	out, stderr, err = cliJSON(t, fa, "", "send", peerID, "--", awkward)
	var rc sendJSON
	if err != nil || stderr != "" || json.Unmarshal([]byte(out), &rc) != nil || rc.Kind != "send_receipt" || rc.Arrives != arriveNextPrompt || rc.To != sent.To || !rc.ExpiresAt.Equal(sent.ExpiresAt) {
		t.Fatalf("send: %q %q %v", out, stderr, err)
	}
	if body := fa.requests()[len(fa.requests())-1].Send.Body; body != awkward {
		t.Fatalf("body changed on the way: %q", body)
	}

	out, stderr, err = cliJSON(t, fa, "", "inbox", "--limit", "1")
	var ij struct {
		Kind     string `json:"kind"`
		Session  string `json:"session"`
		Messages []struct {
			busproto.InboxItem
			IsReply bool `json:"is_reply"`
		} `json:"messages"`
		More bool   `json:"more"`
		Next string `json:"next"`
	}
	if err != nil || stderr != "" || json.Unmarshal([]byte(out), &ij) != nil || ij.Kind != "inbox" || ij.Session != selfID || len(ij.Messages) != 1 || !ij.More || ij.Next != t0Stamp+"|m3" {
		t.Fatalf("inbox: %q %q %v", out, stderr, err)
	}
	got := ij.Messages[0]
	if !got.IsReply || got.Body != awkward || got.Repo != item.Repo || got.Branch != item.Branch || got.Direction != "received" || got.State != busproto.StateDelivered ||
		got.ID != item.ID || got.From != peerID || !got.Sent.Equal(item.Sent) || strings.Join(got.Refs, ",") != "4c19e0d2/4096:2" {
		t.Fatalf("inbox entry did not round-trip: %+v", got)
	}
	if q := fa.requests()[len(fa.requests())-1].Inbox; q.Limit != 1 {
		t.Fatalf("inbox query %+v", q)
	}
	// The default page is bounded below the wire's 50.
	cliJSON(t, fa, "", "inbox")
	if q := fa.requests()[len(fa.requests())-1].Inbox; q.Limit != inboxDefaultLimit {
		t.Fatalf("default inbox limit %d", q.Limit)
	}
	// No caller: a JSON error with its code, nothing on stdout.
	asCaller(t, nil)
	out, stderr, err = cliJSON(t, fa, "", "inbox")
	if e := jsonErr(t, stderr, err); e.Code != codeNoCaller || out != "" || e.Refused {
		t.Fatalf("no caller: %+v %q", e, out)
	}
	// A usage error too.
	_, stderr, err = cliJSON(t, fa, "", "inbox", "--limit", "999")
	if e := jsonErr(t, stderr, err); e.Code != busproto.CodeBadRequest || e.Example == "" {
		t.Fatalf("usage: %+v", e)
	}
}

// The MCP bus tools answer one text block, the CLI's compact JSON with
// its named fields, and no structuredContent (Claude Code would show the
// model only that); a large inbox stays within the 24000-byte budget in
// whole messages, and its fields say how to read on.
func TestMCPBusJSONAndBudget(t *testing.T) {
	var items []busproto.InboxItem
	for i := range 200 {
		items = append(items, busproto.InboxItem{Envelope: busproto.Envelope{ID: fmt.Sprintf("m%03d", i), ThreadID: "t", From: peerID, User: "a@x.test",
			Body: strings.Repeat("é", 1900), Sent: t0.Add(-time.Duration(i) * time.Minute)}, Direction: "received", State: busproto.StateQueued})
	}
	fa := startFakeAgent(t, func(r agent.Request) agent.Response {
		switch r.Op {
		case "peers":
			return agent.Response{OK: true, Peers: &busproto.PeersResponse{Peers: []busproto.Peer{{Session: peerID, Agent: "codex", User: "g@x.test", Own: true}}}}
		case "send":
			return agent.Response{OK: true, Sent: &busproto.SendResponse{ID: "m5", ThreadID: "m5", State: busproto.StateHeld, To: busproto.Recipient{User: "s@x.test"}}}
		}
		return agent.Response{OK: true, Inbox: &busproto.InboxResponse{Messages: items[:r.Inbox.Limit], Next: "x|y"}}
	})
	r := &retriever{caller: func(context.Context) (local.Caller, bool) { return *claudeSelf, true }, busSocket: fa.sock}
	required := map[string][]string{
		"flopwire_peers": {"kind", "peers", "total", "more", "limit"},
		"flopwire_send":  {"kind", "id", "thread_id", "state", "to", "arrives", "outcome"},
		"flopwire_inbox": {"kind", "session", "messages", "more"},
	}
	for name, args := range map[string]string{
		"flopwire_peers": `{}`,
		"flopwire_send":  `{"to":"@s","message":"hi"}`,
		"flopwire_inbox": `{"limit":200}`,
	} {
		resp := mcpRoundTrip(t, r, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`)
		text, isErr, structured := mcpContent(t, resp)
		var obj map[string]any
		dec := json.NewDecoder(strings.NewReader(text))
		if isErr || structured != nil || strings.Contains(resp, "structuredContent") || dec.Decode(&obj) != nil || dec.More() {
			t.Fatalf("%s: want one text block holding one JSON document: %s", name, resp)
		}
		for _, k := range required[name] {
			if _, ok := obj[k]; !ok {
				t.Errorf("%s: JSON lacks %q", name, k)
			}
		}
		structured = obj
		if len(text) > format.MaxOutput {
			t.Errorf("%s: %d bytes, over the budget", name, len(text))
		}
		if name == "flopwire_inbox" {
			msgs := structured["messages"].([]any)
			last := msgs[len(msgs)-1].(map[string]any)
			if structured["more"] != true || len(msgs) == 0 || len(msgs) >= 200 || !strings.HasSuffix(structured["next"].(string), "|"+last["id"].(string)) ||
				!strings.Contains(structured["hint"].(string), "output budget of 24000 bytes reached") {
				t.Errorf("inbox budget: %d messages, more %v, next %v, hint %v", len(msgs), structured["more"], structured["next"], structured["hint"])
			}
		}
	}
}

// Every time printed is UTC, whatever zone the agent stamped it in.
func TestBusTimesAreUTC(t *testing.T) {
	asCaller(t, claudeSelf)
	ny := time.FixedZone("EDT", -4*3600)
	fa := startFakeAgent(t, func(r agent.Request) agent.Response {
		switch r.Op {
		case "peers":
			return agent.Response{OK: true, Peers: &busproto.PeersResponse{Peers: []busproto.Peer{{Session: peerID, SeenAt: t0.In(ny)}}}}
		case "send":
			return agent.Response{OK: true, Sent: &busproto.SendResponse{ID: "m1", Sent: t0.In(ny), ExpiresAt: t0.In(ny), To: busproto.Recipient{User: "a@x.test"}}}
		}
		d := t0.In(ny)
		return agent.Response{OK: true, Inbox: &busproto.InboxResponse{Messages: []busproto.InboxItem{{Envelope: busproto.Envelope{ID: "m1", Sent: d, ExpiresAt: d}, DeliveredAt: &d}}}}
	})
	for _, args := range [][]string{{"peers"}, {"send", "@a", "--", "hi"}, {"inbox"}} {
		out, _, err := cliJSON(t, fa, "", args...)
		if err != nil || strings.Contains(out, "-04:00") || !strings.Contains(out, "T14:02:11Z") {
			t.Fatalf("%v: %s %v", args, out, err)
		}
	}
}

// A flag the parser rejects is an error like any other: in JSON mode (the
// default) one JSON object on stderr with a stable code, nothing on
// stdout; with --text, readable text.
func TestBusParseErrorsAreJSON(t *testing.T) {
	asCaller(t, claudeSelf)
	fa := startFakeAgent(t, func(agent.Request) agent.Response { return agent.Response{OK: true} })
	for _, args := range [][]string{{"peers", "--bogus"}, {"send", "@a", "--intent"}, {"inbox", "--sent=maybe"}, {"inbox", "-Z"}} {
		out, stderr, err := cliJSON(t, fa, "", args...)
		if e := jsonErr(t, stderr, err); e.Code != busproto.CodeBadRequest || e.Detail == "" || out != "" {
			t.Fatalf("%v: %+v %q", args, e, out)
		}
	}
	_, stderr, err := cliJSON(t, fa, "", "peers", "--text", "--bogus")
	if err == nil || errors.Is(err, errReported) || stderr != "" {
		t.Fatalf("--text parse error: %q %v", stderr, err)
	}
	if len(fa.requests()) != 0 {
		t.Fatalf("requests: %+v", fa.requests())
	}
}

// What an agent reads about delivery is what flopwire hook does: a
// message arrives through the hook inside a running turn or with the
// human's next prompt, never by waking a session; inbox is for checking a
// sent message and re-reading, and the fallback where the hook is not set
// up. Nothing says the hook is missing.
func TestBusTextDescribesDelivery(t *testing.T) {
	raw, _ := json.Marshal(mcpTools())
	texts := map[string]string{"help peers": toolHelp["peers"], "help send": toolHelp["send"], "help inbox": toolHelp["inbox"], "instructions": mcpInstructions, "tools": string(raw)}
	for name, s := range texts {
		if strings.Contains(s, "not built") {
			t.Errorf("%s says the hook is not built", name)
		}
	}
	for _, name := range []string{"help send", "help inbox", "instructions", "tools"} {
		s := strings.Join(strings.Fields(texts[name]), " ")
		if !strings.Contains(s, "flopwire hook") || !strings.Contains(s, "next prompt") {
			t.Errorf("%s does not say how a message arrives: %s", name, s)
		}
	}
	for _, name := range []string{"help inbox", "tools"} {
		s := strings.Join(strings.Fields(texts[name]), " ")
		if !strings.Contains(s, "check a sent message's state or re-read a thread") || !strings.Contains(s, "where the hook is not set up") {
			t.Errorf("%s does not say what inbox is for", name)
		}
	}
	if !strings.Contains(mcpInstructions, "A message never starts a turn") {
		t.Error("instructions do not say a message never starts a turn")
	}
}

// The inbox budget counts the fields it adds after cutting (more, hint):
// whatever the page's size, the MCP answer stays within 24000 bytes.
func TestMCPInboxBudgetCountsTheHint(t *testing.T) {
	var body int
	fa := startFakeAgent(t, func(r agent.Request) agent.Response {
		var items []busproto.InboxItem
		for i := range 7 {
			items = append(items, busproto.InboxItem{Envelope: busproto.Envelope{ID: fmt.Sprintf("m%03d", i), ThreadID: "t", From: peerID,
				Body: strings.Repeat("a", body), Sent: t0}, Direction: "received", State: busproto.StateQueued})
		}
		return agent.Response{OK: true, Inbox: &busproto.InboxResponse{Messages: items, Next: "2026-10-01T14:02:11Z|m006"}}
	})
	r := &retriever{caller: func(context.Context) (local.Caller, bool) { return *claudeSelf, true }, busSocket: fa.sock}
	for body = 2900; body < 3200; body++ {
		resp := mcpRoundTrip(t, r, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"flopwire_inbox","arguments":{}}}`)
		text, isErr, _ := mcpContent(t, resp)
		var in inboxJSON
		if isErr || len(text) > format.MaxOutput || json.Unmarshal([]byte(text), &in) != nil || !in.More {
			t.Fatalf("body %d: %d bytes (budget %d), more %v", body, len(text), format.MaxOutput, in.More)
		}
	}
}

// peers --repo . from a main checkout names the repository: the request
// carries its checkout roots, so a peer in a linked worktree is on it,
// and the filter run here (the caller withheld) keeps it too (#81).
func TestPeersRepoCoversWorktrees(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	asCaller(t, claudeSelf)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main, wt, other := filepath.Join(base, "app"), filepath.Join(base, "app-api"), filepath.Join(base, "other", "app")
	os.MkdirAll(main, 0o755)
	os.MkdirAll(other, 0o755)
	for _, g := range []struct {
		dir  string
		args []string
	}{{main, []string{"init", "-q"}}, {main, []string{"commit", "-q", "--allow-empty", "-m", "init"}}, {main, []string{"worktree", "add", "-q", wt}}, {other, []string{"init", "-q"}}} {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, g.args...)...)
		cmd.Dir = g.dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", g.args, err, out)
		}
	}
	wtPeer, otherPeer := "9d00e0d4-0000-4000-8000-000000000001", "9d00e0d5-0000-4000-8000-000000000001"
	fa := startFakeAgent(t, func(r agent.Request) agent.Response {
		if r.Peers.Session != "" {
			return refused(busproto.Error{Status: 403, Code: busproto.CodeSessionNotOnDevice, Detail: "withheld"})
		}
		return agent.Response{OK: true, Peers: &busproto.PeersResponse{Peers: []busproto.Peer{
			{Session: wtPeer, Agent: "codex", User: "g@x.test", Repo: wt, Main: main},
			{Session: otherPeer, Agent: "codex", User: "g@x.test", Repo: other, Main: other},
		}}}
	})
	t.Chdir(main)
	out, stderr, err := cliJSON(t, fa, "", "peers", "--repo", ".")
	var pj peersJSON
	if err != nil || json.Unmarshal([]byte(out), &pj) != nil || len(pj.Peers) != 1 || pj.Peers[0].Session != wtPeer {
		t.Fatalf("peers --repo . from the main checkout: %q %q %v", out, stderr, err)
	}
	// The worktree is matched by its main checkout, which presence
	// reports, not by listing it (#102).
	if r := fa.requests()[0]; r.Peers.Repo != main || !slices.Contains(r.Peers.Mains, main) || slices.Contains(r.Peers.Mains, other) {
		t.Fatalf("the request names no main checkout: %+v", r.Peers)
	}
}

// With a server, peers --repo names only the checkouts the path rules let
// reach it: a worktree under a local rule is left out of the request,
// while the filter run here for a withheld caller still covers it.
func TestPeersRepoLeavesOutWithheldCheckouts(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	cfgDir := t.TempDir()
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(cfgDir, "config.json"))
	if err := client.Save(client.Config{Server: "https://flopwire.example.test", Token: "device"}); err != nil {
		t.Fatal(err)
	}
	asCaller(t, claudeSelf)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main, ok, secret := filepath.Join(base, "app"), filepath.Join(base, "app-ok"), filepath.Join(base, "app-secret")
	os.MkdirAll(main, 0o755)
	for _, args := range [][]string{{"init", "-q"}, {"commit", "-q", "--allow-empty", "-m", "init"}, {"worktree", "add", "-q", ok}, {"worktree", "add", "-q", secret}} {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = main
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "path-rules"), []byte("local "+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secretPeer := "9d00e0d6-0000-4000-8000-000000000001"
	withheld := false
	fa := startFakeAgent(t, func(r agent.Request) agent.Response {
		if withheld && r.Peers.Session != "" {
			return refused(busproto.Error{Status: 403, Code: busproto.CodeSessionNotOnDevice, Detail: "withheld"})
		}
		return agent.Response{OK: true, Peers: &busproto.PeersResponse{Peers: []busproto.Peer{{Session: secretPeer, Agent: "codex", User: "g@x.test", Repo: secret, Main: main}}}}
	})
	t.Chdir(main)
	if _, stderr, err := cliJSON(t, fa, "", "peers", "--repo", "."); err != nil {
		t.Fatalf("peers: %q %v", stderr, err)
	}
	if r := fa.requests()[0]; r.Peers.Repo != main || !slices.Contains(r.Peers.Mains, main) || slices.Contains(r.Peers.Roots, secret) || slices.Contains(r.Peers.Mains, secret) {
		t.Fatalf("the request to the server: %+v", r.Peers)
	}
	withheld = true
	out, stderr, err := cliJSON(t, fa, "", "peers", "--repo", ".")
	var pj peersJSON
	if err != nil || json.Unmarshal([]byte(out), &pj) != nil || len(pj.Peers) != 1 || pj.Peers[0].Session != secretPeer {
		t.Fatalf("the filter run here drops the withheld worktree's peer: %q %q %v", out, stderr, err)
	}
}

// When the agent will not name the caller (a path rule keeps its session
// off the server), the retry without its id must not carry its repo
// either: --repo . names the withheld repo. The repo filter then runs
// here, with the server's matching (a path and what is under it, or a
// repo name).
func TestPeersWithheldCallerKeepsItsRepoLocal(t *testing.T) {
	asCaller(t, claudeSelf)
	fa := startFakeAgent(t, func(r agent.Request) agent.Response {
		if r.Peers.Session != "" {
			return refused(busproto.Error{Status: 403, Code: busproto.CodeSessionNotOnDevice, Detail: "session x is kept off the server by a path rule; it cannot use messaging"})
		}
		return agent.Response{OK: true, Peers: &busproto.PeersResponse{Peers: []busproto.Peer{
			{Session: peerID, Agent: "codex", User: "g@x.test", Repo: "/work/oracle-alpha"},
			{Session: "9d00e0d2-0000-4000-8000-000000000001", Agent: "claude", User: "g@x.test", Repo: "/work/api"},
			{Session: "9d00e0d3-0000-4000-8000-000000000001", Agent: "claude", User: "g@x.test", Repo: "/other/oracle-alpha/sub"},
		}}}
	})
	for _, c := range []struct {
		repo string
		want []string
	}{
		{"/work/oracle-alpha", []string{peerID}},
		{"oracle-alpha", []string{peerID}},
		{"/other/oracle-alpha", []string{"9d00e0d3-0000-4000-8000-000000000001"}},
	} {
		out, stderr, err := cliJSON(t, fa, "", "peers", "--repo", c.repo)
		var pj peersJSON
		if err != nil || json.Unmarshal([]byte(out), &pj) != nil {
			t.Fatalf("%s: %q %q %v", c.repo, out, stderr, err)
		}
		var got []string
		for _, p := range pj.Peers {
			got = append(got, p.Session)
		}
		if !slices.Equal(got, c.want) || pj.Total != len(c.want) {
			t.Errorf("--repo %s: %v, want %v", c.repo, got, c.want)
		}
	}
	for _, r := range fa.requests() {
		if r.Peers.Session == "" && r.Peers.Repo != "" {
			t.Fatalf("the request without the caller's id still names a repo: %+v", r.Peers)
		}
	}
}

// TestBusSandboxBlockedConnect: a shell inside Codex's sandbox with the
// network off may not connect to the agent's socket. Codex marks such a
// shell with CODEX_SANDBOX_NETWORK_DISABLED=1 (both OSes) and
// CODEX_SANDBOX=seatbelt (macOS). That is not "the agent is not running":
// the error says so and points to the MCP tools, which run outside the
// sandbox. A socket the caller may not open (EACCES) stands in for the
// sandbox's EPERM here.
func TestBusSandboxBlockedConnect(t *testing.T) {
	for _, env := range [][2]string{{"CODEX_SANDBOX_NETWORK_DISABLED", "1"}, {"CODEX_SANDBOX", "seatbelt"}} {
		t.Run(env[0], func(t *testing.T) {
			t.Setenv("CODEX_SANDBOX_NETWORK_DISABLED", "")
			t.Setenv("CODEX_SANDBOX", "")
			t.Setenv(env[0], env[1])
			e := sendToUnopenableSocket(t)
			if e.Code != codeSandboxBlocked || !strings.Contains(e.Fix, "flopwire_send") || strings.Contains(e.Detail, "not running") {
				t.Fatalf("blocked connect: %+v", e)
			}
		})
	}
}

// TestBusPermissionDeniedConnect: outside any sandbox, a socket the
// caller may not open is a permissions problem, not sandbox_blocked: the
// MCP tools would fail the same way.
func TestBusPermissionDeniedConnect(t *testing.T) {
	t.Setenv("CODEX_SANDBOX_NETWORK_DISABLED", "")
	t.Setenv("CODEX_SANDBOX", "")
	e := sendToUnopenableSocket(t)
	if e.Code != codePermissionDenied || strings.Contains(e.Detail, "sandbox") || strings.Contains(e.Fix, "flopwire_send") {
		t.Fatalf("plain EACCES: %+v", e)
	}
}

// TestBusConnectRefusalCode: EPERM on connect comes from a sandbox
// (seatbelt, seccomp), never from file modes, so it is sandbox_blocked
// even without Codex's variables; EACCES is only with them.
func TestBusConnectRefusalCode(t *testing.T) {
	none := func(string) string { return "" }
	codex := func(k string) string {
		if k == "CODEX_SANDBOX_NETWORK_DISABLED" {
			return "1"
		}
		return ""
	}
	for _, c := range []struct {
		err    error
		getenv func(string) string
		want   string
	}{
		{syscall.EPERM, none, codeSandboxBlocked},
		{syscall.EPERM, codex, codeSandboxBlocked},
		{syscall.EACCES, codex, codeSandboxBlocked},
		{syscall.EACCES, none, codePermissionDenied},
		{errors.New("other"), codex, ""},
	} {
		if got := connectRefusal(c.err, c.getenv); got != c.want {
			t.Errorf("connectRefusal(%v, sandboxed=%v) = %q, want %q", c.err, c.getenv("CODEX_SANDBOX_NETWORK_DISABLED") != "", got, c.want)
		}
	}
}

// sendToUnopenableSocket runs flopwire send against a listening socket
// with mode 0 and returns its error.
func sendToUnopenableSocket(t *testing.T) busErr {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores socket permissions")
	}
	asCaller(t, claudeSelf)
	sock := filepath.Join(shortSockDir(t), "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	if err := os.Chmod(sock, 0); err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	err = busCmd(t.Context(), "send", []string{"--socket", sock, "@a", "--", "hi"}, strings.NewReader(""), &out, &errOut)
	return jsonErr(t, errOut.String(), err)
}

// One message at the body and ref caps, as a send leaves the device
// (devicebus.CleanText), fits the MCP output budget in every inbox form:
// what survives cleaning grows at most two-fold as JSON. A body of control
// characters grew six-fold, about 24 KB from 4,000 bytes (issue #71).
func TestInboxWorstCaseMessageFitsMCPBudget(t *testing.T) {
	asCaller(t, claudeSelf)
	fill := func(unit string, n int) string { return strings.Repeat(unit, n/len(unit)) }
	var item busproto.InboxItem
	fa := startFakeAgent(t, func(agent.Request) agent.Response {
		return agent.Response{OK: true, Inbox: &busproto.InboxResponse{Messages: []busproto.InboxItem{item}, Next: "x|m1"}}
	})
	r := &retriever{caller: func(context.Context) (local.Caller, bool) { return *claudeSelf, true }, busSocket: fa.sock}
	// "\n…x" is a body of empty lines (one character ends it, as the text
	// form trims trailing newlines): the thread text form indents each.
	for _, unit := range []string{"\x01", "\x1b", "\x7f", "\u0085", "\r", "\"", "\\", "\n", "\t", " ", "<", "é", "\u2028", "x\n", "\n…x"} {
		body := devicebus.CleanText(fill(unit, busproto.MaxBodyBytes))
		if unit == "\n…x" {
			body = strings.Repeat("\n", busproto.MaxBodyBytes-1) + "x"
		}
		if body == "" {
			continue // nothing left: refused as an empty body
		}
		var refs []string
		for range busproto.MaxRefs {
			refs = append(refs, devicebus.CleanRef(fill(unit, busproto.MaxRefBytes)))
		}
		item = busproto.InboxItem{Envelope: busproto.Envelope{ID: "m0123456789abcdef", ThreadID: "m0123456789abcdef", ReplyTo: "m0123456789abcdee",
			From: peerID, FromAgent: "codex", User: "alex@example.test", UserID: "u-2", Repo: "/src/web", Branch: "main",
			Sender: busproto.SenderTeammate, Intent: busproto.IntentRequest, Body: body, Refs: refs, Sent: t0, ExpiresAt: t0.Add(time.Hour),
			ToSession: selfID, ToAgent: "claude", ToUser: "gary@example.test", Addressed: "session"}, Direction: "received", State: busproto.StateRead}
		for _, args := range []map[string]any{{}, {"thread": "m0123456789abcdef"}, {"format": "text"}, {"thread": "m0123456789abcdef", "format": "text"}} {
			text, err := mcpCall(t.Context(), r, "flopwire_inbox", args)
			if err != nil {
				t.Fatalf("%q %v: %v", unit, args, err)
			}
			if len(text) > format.MaxOutput {
				t.Errorf("%q %v: %d bytes, over the %d-byte budget", unit, args, len(text), format.MaxOutput)
			}
			if !strings.Contains(text, "m0123456789abcdef") {
				t.Errorf("%q %v: the message is not shown: %.200s", unit, args, text)
			}
		}
	}
}
