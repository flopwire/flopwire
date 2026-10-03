package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/retrieval/local"
)

// sendReceiptKeys are the receipt's field names, in order; next is last
// and only on a request.
var sendReceiptKeys = []string{"kind", "id", "thread_id", "state", "to", "sender", "intent", "sent", "expires_at", "from", "arrives", "outcome", "next"}

// receiptKeys is the top-level field names of one JSON object, in order.
func receiptKeys(t *testing.T, line string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(line))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %q", line)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

// A request's receipt carries next, what to do until the reply comes, by
// situation (issue #83); inform and done carry none. The JSON stays one
// compact line with the same field names, --text adds the same sentence
// to the outcome line, and MCP answers the CLI's JSON byte for byte.
func TestSendReceiptNext(t *testing.T) {
	asCaller(t, claudeSelf)
	exp := t0.Add(24 * time.Hour)
	busy := busproto.Recipient{Session: peerID, Agent: "codex", User: "gary@example.test", Repo: "/src/api", Branch: "main", Live: true, Busy: true}
	idle := busy
	idle.Busy = false
	ended := busy
	ended.Live, ended.Busy = false, false
	var to busproto.Recipient
	var state busproto.State
	fa := startFakeAgent(t, func(r agent.Request) agent.Response {
		intent, _ := busproto.ParseIntent(r.Send.Intent)
		return agent.Response{OK: true, Sent: &busproto.SendResponse{ID: "m01", ThreadID: "m01", State: state, To: to, Sender: busproto.SenderOwn,
			Intent: intent, Sent: t0, ExpiresAt: exp}}
	})
	r := &retriever{caller: func(context.Context) (local.Caller, bool) { return *claudeSelf, true }, busSocket: fa.sock}
	for _, c := range []struct {
		name   string
		intent string
		to     busproto.Recipient
		state  busproto.State
		next   string
	}{
		{"busy", "request", busy, busproto.StateQueued, nextBusy},
		{"busy @user", "request", busproto.Recipient{User: "alex@example.test", Repo: "api", Live: true, Busy: true}, busproto.StateQueued, nextBusy},
		{"idle", "request", idle, busproto.StateQueued, nextNoWait},
		{"held", "request", busproto.Recipient{User: "sam@example.test", Repo: "api", Live: true, Busy: true}, busproto.StateHeld, nextNoWait},
		{"queued @user", "request", busproto.Recipient{User: "alex@example.test", Repo: "api"}, busproto.StateQueued, nextNoWait},
		{"not running", "request", ended, busproto.StateQueued, nextNoWait},
		{"cloud", "request", busproto.Recipient{Session: "session_01AbCd", Agent: "claude", User: "gary@example.test", Live: true, Busy: true, Cloud: true}, busproto.StateQueued, nextCloud},
		{"inform", "inform", busy, busproto.StateQueued, ""},
		{"inform by default", "", busy, busproto.StateQueued, ""},
		{"done", "done", busy, busproto.StateQueued, ""},
	} {
		to, state = c.to, c.state
		args := []string{"send", "4c19e0d2"}
		mcpArgs := map[string]any{"to": "4c19e0d2", "message": "Which cursor field does the client send?"}
		if c.intent != "" {
			args = append(args, "--intent", c.intent)
			mcpArgs["intent"] = c.intent
		}
		args = append(args, "--", "Which cursor field does the client send?")

		out, stderr, err := cliJSON(t, fa, "", args...)
		var rc sendJSON
		if err != nil || stderr != "" || strings.Count(out, "\n") != 1 || json.Unmarshal([]byte(out), &rc) != nil {
			t.Fatalf("%s: %q %q %v", c.name, out, stderr, err)
		}
		if rc.Next != c.next {
			t.Errorf("%s: next %q, want %q", c.name, rc.Next, c.next)
		}
		want := sendReceiptKeys
		if c.next == "" {
			want = sendReceiptKeys[:len(sendReceiptKeys)-1]
		}
		if got := receiptKeys(t, out); !slices.Equal(got, want) {
			t.Errorf("%s: fields %v, want %v", c.name, got, want)
		}
		if strings.Contains(rc.Outcome, rc.Next) && rc.Next != "" {
			t.Errorf("%s: outcome repeats next: %q", c.name, rc.Outcome)
		}
		if len(rc.Next) > 200 {
			t.Errorf("%s: next is %d bytes", c.name, len(rc.Next))
		}

		// --text: the outcome, then next.
		text, err := cli(t, fa, "", args...)
		wantText := rc.Outcome + "\n"
		if c.next != "" {
			wantText = rc.Outcome + ". " + c.next + "\n"
		}
		if err != nil || text != wantText {
			t.Errorf("%s --text:\n got %q\nwant %q", c.name, text, wantText)
		}

		// MCP: the CLI's JSON, and its text form the CLI's line.
		got, err := mcpCall(t.Context(), r, "flopwire_send", mcpArgs)
		if err != nil || got != strings.TrimRight(out, "\n") {
			t.Errorf("%s over MCP:\n got %s\nwant %s %v", c.name, got, out, err)
		}
		mcpArgs["format"] = "text"
		if got, err := mcpCall(t.Context(), r, "flopwire_send", mcpArgs); err != nil || got != strings.TrimRight(wantText, "\n") {
			t.Errorf("%s over MCP as text: %q %v", c.name, got, err)
		}
	}
}

// A refused request has no next: the error is unchanged.
func TestSendRefusalHasNoNext(t *testing.T) {
	asCaller(t, claudeSelf)
	fa := startFakeAgent(t, func(agent.Request) agent.Response {
		return refused(busproto.Error{Status: 409, Code: busproto.CodeRecipientFull, Detail: "the recipient has 50 undelivered messages; the limit is 50"})
	})
	out, stderr, err := cliJSON(t, fa, "", "send", "4c19e0d2", "--intent", "request", "--", "hi")
	if e := jsonErr(t, stderr, err); out != "" || e.Code != busproto.CodeRecipientFull || strings.Contains(stderr, `"next"`) || strings.Contains(stderr, nextNoWait) {
		t.Fatalf("refusal: %q %q", out, stderr)
	}
	if _, err := cli(t, fa, "", "send", "4c19e0d2", "--intent", "request", "--", "hi"); err == nil || strings.Contains(err.Error(), nextNoWait) || strings.Contains(err.Error(), nextBusy) {
		t.Fatalf("refusal --text: %v", err)
	}
	r := &retriever{caller: func(context.Context) (local.Caller, bool) { return *claudeSelf, true }, busSocket: fa.sock}
	if _, err := mcpCall(t.Context(), r, "flopwire_send", map[string]any{"to": "4c19e0d2", "message": "hi", "intent": "request"}); err == nil || strings.Contains(err.Error(), `"next"`) {
		t.Fatalf("refusal over MCP: %v", err)
	}
}

// Through the real device agent with no server: a request to a busy
// session (a Claude session file of a running process with status busy)
// gets the busy next, one to an idle session the no-wait next, and an
// inform none.
func TestSendReceiptNextEndToEndLocal(t *testing.T) {
	const busyID = "e2e0dddd-0000-4000-8000-000000000004"
	home := t.TempDir()
	projects := filepath.Join(home, ".claude", "projects")
	writeClaudeSession(t, projects, e2eA, "/tmp/e2e-api", "refactor client pagination")
	writeClaudeSession(t, projects, e2eB, "/tmp/e2e-api", "add cursor to list endpoint")
	writeClaudeSession(t, projects, busyID, "/tmp/e2e-api", "run the check suite")
	sessions := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	if err := os.WriteFile(filepath.Join(sessions, fmt.Sprintf("%d.json", pid)), fmt.Appendf(nil, `{"pid":%d,"sessionId":%q,"status":"busy","updatedAt":0}`, pid, busyID), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "flopwire", "config.json"))
	t.Setenv("FLOPWIRE_INDEX", filepath.Join(t.TempDir(), "index.db"))
	t.Setenv(client.EnvToken, "")
	t.Setenv(client.EnvServer, "")
	sock := filepath.Join(shortSockDir(t), "a.sock")
	startAgent(t, home, sock, "--no-sync")
	waitPeer(t, sock, e2eA, e2eB)
	deadline := time.Now().Add(60 * time.Second)
	for {
		out, err := busCLI(t, sock, e2eA, "", "peers", "--session", busyID)
		if err == nil && strings.Contains(out, "live busy") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the busy session never showed busy: %q %v", out, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	send := func(args ...string) sendJSON {
		t.Helper()
		var out, errOut strings.Builder
		if err := busCmd(t.Context(), "send", append([]string{"--socket", sock}, args...), strings.NewReader(""), &out, &errOut); err != nil || errOut.Len() > 0 {
			t.Fatalf("send %v: %v %s", args, err, errOut.String())
		}
		var rc sendJSON
		if strings.Count(out.String(), "\n") != 1 || json.Unmarshal([]byte(out.String()), &rc) != nil || rc.Kind != "send_receipt" {
			t.Fatalf("send %v: %q", args, out.String())
		}
		return rc
	}
	if rc := send("e2e0dddd", "--intent", "request", "--", "Which stage is failing?"); rc.Arrives != arriveNextToolCall || rc.Next != nextBusy {
		t.Fatalf("request to a busy session: %+v", rc)
	}
	if rc := send("e2e0bbbb", "--intent", "request", "--", "Which cursor field does the client send?"); rc.Arrives != arriveNextPrompt || rc.Next != nextNoWait {
		t.Fatalf("request to an idle session: %+v", rc)
	}
	if rc := send("e2e0dddd", "--", "Heads-up: stage 3 is slow."); rc.Arrives != arriveNextToolCall || rc.Next != "" {
		t.Fatalf("inform to a busy session: %+v", rc)
	}
}
