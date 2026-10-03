package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/retrieval/local"
	"github.com/flopwire/flopwire/internal/transcript"
)

// Issue #107 end to end, through a real device agent without a server.

const (
	e2eCodexParent = "01a0ffac-8aba-7f33-8d1d-a5b896775739"
	e2eCodexChild  = "01a0ffac-b452-7aa0-94ce-27c32af88461"
)

// writeCodexRollout writes a Codex rollout; parent "" is a root thread,
// else a thread_spawn subagent of parent, as Codex 0.160.0 writes it
// (session_meta.session_id is the root thread).
func writeCodexRollout(t *testing.T, home, id, parent, cwd, prompt string) string {
	t.Helper()
	at := time.Now().UTC().Add(-time.Minute)
	dir := filepath.Join(home, ".codex", "sessions", at.Format("2006"), at.Format("01"), at.Format("02"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ts := func(d time.Duration) string { return at.Add(d).Format("2006-01-02T15:04:05.000Z") }
	meta := map[string]any{"session_id": id, "id": id, "timestamp": ts(0), "cwd": cwd, "originator": "codex_exec", "cli_version": "0.160.0",
		"source": "exec", "thread_source": "user", "model_provider": "openai"}
	if parent != "" {
		meta["session_id"], meta["parent_thread_id"], meta["forked_from_id"], meta["thread_source"] = parent, parent, parent, "subagent"
		meta["source"] = map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": parent, "depth": 1, "agent_path": "/root/child"}}}
	}
	rec := func(ord int, typ string, payload any) string {
		b, _ := json.Marshal(map[string]any{"timestamp": ts(time.Duration(ord) * time.Second), "ordinal": ord, "type": typ, "payload": payload})
		return string(b)
	}
	lines := []string{
		rec(0, "session_meta", meta),
		rec(1, "event_msg", map[string]any{"type": "task_started", "turn_id": id[:8] + "-turn"}),
		rec(2, "response_item", map[string]any{"type": "message", "id": "msg_" + id[:8], "role": "user", "content": []map[string]any{{"type": "input_text", "text": prompt}}}),
	}
	path := filepath.Join(dir, fmt.Sprintf("rollout-%s-%s.jsonl", at.Format("2006-01-02T15-04-05"), id))
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// busJSONAs runs a bus command as caller, with JSON output.
func busJSONAs(t *testing.T, sock string, caller local.Caller, args ...string) (map[string]any, error) {
	t.Helper()
	asCaller(t, &caller)
	var out, errOut strings.Builder
	err := busCmd(t.Context(), args[0], append([]string{"--socket", sock}, args[1:]...), strings.NewReader(""), &out, &errOut)
	if err != nil {
		return nil, fmt.Errorf("%v: %s", err, errOut.String())
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out.String()), &m); err != nil {
		return nil, fmt.Errorf("not JSON: %q", out.String())
	}
	return m, nil
}

func codexHookIn(session, transcriptPath, agentID, event string) string {
	m := map[string]any{"session_id": session, "transcript_path": transcriptPath, "cwd": "/tmp/e2e-api", "hook_event_name": event,
		"turn_id": "01a0ffac-b4a1-7a70-b748-2c8f65c0a21b", "model": "gpt-x", "permission_mode": "default"}
	if event == evPostToolUse {
		m["tool_name"], m["tool_input"], m["tool_response"], m["tool_use_id"] = "Bash", map[string]any{"command": "ls"}, "A\n", "call-1"
	}
	if agentID != "" {
		m["agent_id"], m["agent_type"] = agentID, "default"
	}
	return hookJSON(m)
}

// A message for a session waits through its subagent's hooks (a
// PostToolUse, a Stop, a SubagentStop, even a SessionEnd carrying
// agent_id): none takes it, none ends the session, and the session's own
// next hook prints it once.
func TestSubagentHooksEndToEndLocal(t *testing.T) {
	sock := hookE2E(t)
	if _, err := busCLI(t, sock, e2eA, "", "send", "e2e0bbbb", "--intent", "request", "--", "for B itself"); err != nil {
		t.Fatal(err)
	}
	sub := func(event string) string {
		var m map[string]any
		json.Unmarshal([]byte(hookFor(e2eB, event)), &m)
		m["agent_id"], m["agent_type"] = claudeAgentID, "general-purpose"
		if event == evPostToolUse {
			m["tool_name"], m["tool_input"], m["tool_response"] = "Bash", map[string]any{"command": "ls"}, map[string]any{"stdout": "a\n"}
		}
		return hookJSON(m)
	}
	for _, ev := range []string{evPostToolUse, "Stop", "SubagentStop", evSessionEnd, evPostToolUse} {
		if out := runHooks(t, sock, sub(ev), 1)[0]; out != "" {
			t.Fatalf("subagent %s printed: %s", ev, out)
		}
	}
	if out, err := busCLI(t, sock, e2eA, "", "inbox", "--sent"); err != nil || !strings.Contains(out, "request  queued") {
		t.Fatalf("after the subagent's hooks, the sender's inbox:\n%s %v", out, err)
	}
	if out, err := busCLI(t, sock, e2eA, "", "peers"); err != nil || !strings.Contains(out, "e2e0bbbb") {
		t.Fatalf("a subagent's SessionEnd ended the session: %q %v", out, err)
	}
	c := contextOf(t, runHooks(t, sock, hookFor(e2eB, evPostToolUse), 1)[0])
	if strings.Count(c, "<flopwire-message ") != 1 || !strings.Contains(c, "for B itself") {
		t.Fatalf("the session's own hook:\n%s", c)
	}
	if out, err := busCLI(t, sock, e2eA, "", "inbox", "--sent"); err != nil || !strings.Contains(out, "request  delivered") {
		t.Fatalf("sender's inbox:\n%s %v", out, err)
	}
}

// A Codex subagent's send goes out as its root session, as a Claude Code
// subagent's does (the caller rule finds the parent's process), so the
// reply reaches the session and not the subagent, which no hook delivers
// to. Before, the subagent's thread was refused: it is not a session.
func TestSubagentSendGoesOutAsTheSession(t *testing.T) {
	home := t.TempDir()
	writeClaudeSession(t, filepath.Join(home, ".claude", "projects"), e2eA, "/tmp/e2e-api", "refactor client pagination")
	parentPath := writeCodexRollout(t, home, e2eCodexParent, "", "/tmp/e2e-api", "review the api")
	childPath := writeCodexRollout(t, home, e2eCodexChild, e2eCodexParent, "/tmp/e2e-api", "run the tests")
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "flopwire", "config.json"))
	t.Setenv("FLOPWIRE_INDEX", filepath.Join(t.TempDir(), "index.db"))
	t.Setenv(client.EnvToken, "")
	t.Setenv(client.EnvServer, "")
	sock := filepath.Join(shortSockDir(t), "a.sock")
	startAgent(t, home, sock, "--no-sync")
	waitPeer(t, sock, e2eA, e2eCodexParent)

	child := local.Caller{Agent: transcript.AgentCodex, SessionID: e2eCodexChild, Rule: "codex-env"}
	r, err := busJSONAs(t, sock, child, "send", e2eA[:8], "--intent", "request", "--", "the tests pass")
	if err != nil {
		t.Fatalf("a subagent's send: %v", err)
	}
	if from, _ := r["from"].(map[string]any); from["session"] != e2eCodexParent {
		t.Fatalf("receipt from %v, want the root session %s", r["from"], e2eCodexParent)
	}
	id, _ := r["id"].(string)
	c := contextOf(t, runHooks(t, sock, hookFor(e2eA, evPostToolUse), 1)[0])
	if !strings.Contains(c, `from="`+e2eCodexParent+`"`) || !strings.Contains(c, "the tests pass") {
		t.Fatalf("recipient's context:\n%s", c)
	}
	// The subagent's inbox is its session's: the sent message is there.
	if in, err := busJSONAs(t, sock, child, "inbox", "--sent"); err != nil || in["session"] != e2eCodexParent || !strings.Contains(fmt.Sprint(in["messages"]), id) {
		t.Fatalf("subagent's inbox: %v %v", in, err)
	}
	// The reply goes to the session; the subagent's hook takes nothing, the
	// session's own hook prints it.
	if _, err := busCLI(t, sock, e2eA, "", "send", e2eCodexParent, "--reply-to", id, "--", "thanks, merging"); err != nil {
		t.Fatal(err)
	}
	if out := runHooks(t, sock, codexHookIn(e2eCodexParent, childPath, e2eCodexChild, evPostToolUse), 1)[0]; out != "" {
		t.Fatalf("the subagent's hook printed: %s", out)
	}
	if c := contextOf(t, runHooks(t, sock, codexHookIn(e2eCodexParent, parentPath, "", evPostToolUse), 1)[0]); !strings.Contains(c, "thanks, merging") {
		t.Fatalf("the session's own hook:\n%s", c)
	}
}
