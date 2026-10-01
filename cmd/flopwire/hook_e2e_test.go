package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/busrender"
	"github.com/flopwire/flopwire/internal/client"
)

// hookE2E is a real device agent with no server and two live Claude
// sessions, A and B, on one repo.
func hookE2E(t *testing.T) (sock string) {
	t.Helper()
	home := t.TempDir()
	projects := filepath.Join(home, ".claude", "projects")
	writeClaudeSession(t, projects, e2eA, "/tmp/e2e-api", "refactor client pagination")
	writeClaudeSession(t, projects, e2eB, "/tmp/e2e-api", "add cursor to list endpoint")
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "flopwire", "config.json"))
	t.Setenv("FLOPWIRE_INDEX", filepath.Join(t.TempDir(), "index.db"))
	t.Setenv(client.EnvToken, "")
	t.Setenv(client.EnvServer, "")
	sock = filepath.Join(shortSockDir(t), "a.sock")
	startAgent(t, home, sock, "--no-sync")
	waitPeer(t, sock, e2eA, e2eB)
	return sock
}

func hookFor(session, event string) string {
	return hookJSON(map[string]any{"session_id": session, "hook_event_name": event, "source": "startup",
		"transcript_path": "/tmp/none/" + session + ".jsonl", "cwd": "/tmp/e2e-api"})
}

// runHooks runs n hooks at once for one input and returns their outputs.
func runHooks(t *testing.T, sock, in string, n int) []string {
	t.Helper()
	outs := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			var out, errOut strings.Builder
			hookCmd(t.Context(), []string{"--socket", sock}, strings.NewReader(in), &out, &errOut, func(string) string { return "" })
			outs[i] = out.String()
		})
	}
	wg.Wait()
	return outs
}

func contextOf(t *testing.T, out string) string {
	t.Helper()
	if out == "" {
		return ""
	}
	var o hookOutput
	if err := json.Unmarshal([]byte(out), &o); err != nil {
		t.Fatalf("not hook JSON: %q", out)
	}
	return o.HookSpecificOutput.AdditionalContext
}

// Through the real device agent: a sent message reaches the recipient's
// hook exactly once across two concurrent hook invocations (two hook
// configs for one event), the standing instruction too, and a ref to a
// message the local index holds carries an excerpt.
func TestHookEndToEndLocal(t *testing.T) {
	prev := agent.ExcerptBudget
	agent.ExcerptBudget = 10 * time.Second // the race detector on a loaded machine
	t.Cleanup(func() { agent.ExcerptBudget = prev })
	sock := hookE2E(t)
	if _, err := busCLI(t, sock, e2eA, "", "send", "e2e0bbbb", "--intent", "request", "--ref", e2eA+"/0", "--", "Can you rebase api on main?"); err != nil {
		t.Fatal(err)
	}
	// Both hooks may print: one may take the instruction and the other the
	// message. The harness passes both to the model together; each part
	// appears exactly once.
	outs := runHooks(t, sock, hookFor(e2eB, evSessionStart), 2)
	var c string
	for _, o := range outs {
		c += contextOf(t, o) + "\n"
	}
	if strings.Count(c, busrender.StandingInstruction) != 1 || strings.Count(c, "<flopwire-message ") != 1 ||
		!strings.Contains(c, `from="`+e2eA+`"`) || !strings.Contains(c, `sender="own" intent="request"`) ||
		!strings.Contains(c, ">\nCan you rebase api on main?\n") || !strings.Contains(c, "\nReply with the flopwire_send tool: to=\""+e2eA+"\"") {
		t.Fatalf("context:\n%s", c)
	}
	if !strings.Contains(c, `<flopwire-ref address="`+e2eA+`/0">user: refactor client pagination</flopwire-ref>`) {
		t.Fatalf("ref excerpt:\n%s", c)
	}
	// Nothing left: both later hooks print nothing.
	for _, o := range runHooks(t, sock, hookFor(e2eB, evPostToolUse), 2) {
		if o != "" {
			t.Fatalf("delivered twice: %q", o)
		}
	}
	// The sender's inbox shows it delivered.
	if out, err := busCLI(t, sock, e2eA, "", "inbox", "--sent"); err != nil || !strings.Contains(out, "request  delivered") {
		t.Fatalf("sender's inbox:\n%s %v", out, err)
	}
}

// More messages than one hook call carries: the oldest HookMessages print,
// the rest stay queued and print at the next call, each exactly once.
func TestHookEndToEndCap(t *testing.T) {
	sock := hookE2E(t)
	const n = busrender.HookMessages + 2
	for i := range n {
		if _, err := busCLI(t, sock, e2eA, "", "send", "e2e0bbbb", "--", fmt.Sprintf("note %d", i)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond) // distinct sent times keep the order observable
	}
	first := contextOf(t, runHooks(t, sock, hookFor(e2eB, evPostToolUse), 1)[0])
	if strings.Count(first, "<flopwire-message ") != busrender.HookMessages || !strings.Contains(first, "note 0\n") || strings.Contains(first, fmt.Sprintf("note %d\n", n-1)) {
		t.Fatalf("first call:\n%s", first)
	}
	resp, err := agent.Call(t.Context(), sock, agent.Request{Op: "status"})
	if err != nil || resp.Bus == nil || resp.Bus.Pending != 2 {
		t.Fatalf("pending after the first call: %+v %v", resp.Bus, err)
	}
	second := contextOf(t, runHooks(t, sock, hookFor(e2eB, evUserPromptSubmit), 1)[0])
	if strings.Count(second, "<flopwire-message ") != 2 || !strings.Contains(second, fmt.Sprintf("note %d\n", n-1)) {
		t.Fatalf("second call:\n%s", second)
	}
}
