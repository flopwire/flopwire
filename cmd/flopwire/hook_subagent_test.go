package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/flopwire/flopwire/internal/busproto"
)

// Issue #107. A hook that runs inside a subagent carries its parent
// session's id. Messages are addressed to sessions, and a subagent is not
// one, so such a hook takes nothing: no message, no standing instruction,
// no busy/idle or ended event. It still flushes the subagent's transcript.
//
// The inputs below are the shapes the probes recorded (2026-10-03):
// Claude Code 2.1.288, Codex 0.160.0, Devin 3000.11.1.

const (
	claudeAgentID = "a260631108a9ffbc9"
	codexChildID  = "01a0ffac-b452-7aa0-94ce-27c32af88461"
)

var codexChildTranscript = "/Users/u/.codex/sessions/2026/10/01/rollout-2026-10-01T12-56-03-" + codexChildID + ".jsonl"

// claudeSubagentIn is a Claude Code hook inside a subagent: the parent's
// session_id and transcript_path, with agent_id and agent_type.
func claudeSubagentIn(event, transcriptPath string) string {
	var m map[string]any
	json.Unmarshal([]byte(claudeIn(event)), &m)
	m["transcript_path"] = transcriptPath
	m["agent_id"], m["agent_type"] = claudeAgentID, "general-purpose"
	return hookJSON(m)
}

// codexSubagentIn is a Codex hook inside a spawned agent: the root
// thread's session_id, the child's rollout and turn, and agent_id.
func codexSubagentIn(event string) string {
	var m map[string]any
	json.Unmarshal([]byte(codexIn(event)), &m)
	m["transcript_path"] = codexChildTranscript
	m["agent_id"], m["agent_type"] = codexChildID, "default"
	return hookJSON(m)
}

// writeDevinStore writes a Devin sessions.db holding the session's own
// assistant nodes with these tool calls (name:id), in order.
func writeDevinStore(t *testing.T, path string, calls ...string) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, main_chain_id INTEGER);
		CREATE TABLE message_nodes (row_id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, node_id INTEGER NOT NULL,
		  parent_node_id INTEGER, chat_message TEXT NOT NULL, created_at INTEGER NOT NULL, metadata TEXT, UNIQUE(session_id, node_id));
		CREATE INDEX idx_message_nodes_session ON message_nodes(session_id);`); err != nil {
		t.Fatal(err)
	}
	devinNode(t, db, map[string]any{"message_id": "u0", "role": "user", "content": "do it"})
	for _, c := range calls {
		name, id, _ := strings.Cut(c, ":")
		devinNode(t, db, map[string]any{"message_id": "a-" + id, "role": "assistant", "content": "",
			"tool_calls": []map[string]any{{"id": id, "name": name, "arguments": "{}"}}})
	}
	return db
}

// devinNode appends a node to the session's conversation.
func devinNode(t *testing.T, db *sql.DB, msg map[string]any) {
	t.Helper()
	devinNodeOf(t, db, devinSID, msg)
}

func devinNodeOf(t *testing.T, db *sql.DB, session string, msg map[string]any) {
	t.Helper()
	b, _ := json.Marshal(msg)
	if _, err := db.Exec(`INSERT INTO message_nodes (session_id, node_id, parent_node_id, chat_message, created_at)
		SELECT ?, COALESCE(MAX(node_id), -1) + 1, MAX(node_id), ?, 1790996000 FROM message_nodes WHERE session_id = ?`, session, string(b), session); err != nil {
		t.Fatal(err)
	}
}

// devinHookEnv is devinEnv with a store in which the tool calls of
// devinIn ("call_1") and of claudeIn ("t1", the input of a hook Devin runs
// from .claude/settings.json) are their sessions' own.
func devinHookEnv(t *testing.T) map[string]string {
	t.Helper()
	env := maps.Clone(devinEnv)
	env["CHISEL_SESSION_DB"] = filepath.Join(t.TempDir(), "cli", "sessions.db")
	db := writeDevinStore(t, env["CHISEL_SESSION_DB"], "exec:call_1")
	devinNodeOf(t, db, claudeSID, map[string]any{"message_id": "a-t1", "role": "assistant", "content": "",
		"tool_calls": []map[string]any{{"id": "t1", "name": "Bash", "arguments": "{}"}}})
	return env
}

func devinToolIn(event, toolUseID string) string {
	var m map[string]any
	json.Unmarshal([]byte(devinIn(evPostToolUse)), &m)
	m["hook_event_name"], m["tool_use_id"] = event, toolUseID
	return hookJSON(m)
}

// assertTookNothing checks that a hook printed nothing, took nothing and
// confirmed nothing, and that its flush carried no event (and, when path
// is not "-", that transcript).
func assertTookNothing(t *testing.T, name string, fa *hookAgent, out, path string) {
	t.Helper()
	if out != "" {
		t.Fatalf("%s: printed into the subagent's context: %s", name, out)
	}
	if p := fa.requests("pending"); len(p) != 0 {
		t.Fatalf("%s: took the parent session's messages: %+v", name, p)
	}
	if c := fa.requests("confirm"); len(c) != 0 {
		t.Fatalf("%s: confirmed a delivery: %+v", name, c)
	}
	f := waitFlush(t, fa, 1)
	if f[0].Event != "" {
		t.Fatalf("%s: flush carries the event %q, a busy/idle or ended signal for the parent", name, f[0].Event)
	}
	if path != "-" && f[0].Path != path {
		t.Fatalf("%s: flush path %q, want the subagent's transcript %q", name, f[0].Path, path)
	}
}

func pendingForParent(fa *hookAgent) {
	fa.resp.Instruct = true
	fa.msgs = []busproto.Envelope{testEnvelope("m1", "for the parent session", busproto.IntentRequest)}
}

// The issue's reproduction: a PostToolUse hook inside a Claude Code
// subagent took the parent's pending message, printed it into the
// subagent's context and confirmed it delivered.
func TestHookClaudeSubagentTakesNothing(t *testing.T) {
	sub := strings.TrimSuffix(claudeTranscript, ".jsonl") + "/subagents/agent-" + claudeAgentID + ".jsonl"
	for _, ev := range []string{evPostToolUse, evUserPromptSubmit, evSessionStart, "Stop", "SubagentStop", evSessionEnd} {
		fa := newHookAgent(t)
		pendingForParent(fa)
		out, _ := runHook(t, fa.sock, claudeSubagentIn(ev, claudeTranscript), map[string]string{"CLAUDECODE": "1"})
		assertTookNothing(t, "claude subagent "+ev, fa, out, sub)
	}
	// The parent's own next hook gets it.
	fa := newHookAgent(t)
	pendingForParent(fa)
	if out, _ := runHook(t, fa.sock, claudeIn(evPostToolUse), map[string]string{"CLAUDECODE": "1"}); !strings.Contains(out, "for the parent session") {
		t.Fatalf("parent hook: %q", out)
	}
}

// A main thread run with `claude --agent NAME` carries agent_type and no
// agent_id: it is the session itself and gets its messages.
func TestHookClaudeAgentFlagIsTheSession(t *testing.T) {
	var m map[string]any
	json.Unmarshal([]byte(claudeIn(evPostToolUse)), &m)
	m["agent_type"] = "security-reviewer"
	fa := newHookAgent(t)
	pendingForParent(fa)
	if out, _ := runHook(t, fa.sock, hookJSON(m), nil); !strings.Contains(out, "for the parent session") {
		t.Fatalf("--agent main thread: %q", out)
	}
}

// The subagent's flush names its own transcript: <session>/subagents/
// agent-<id>.jsonl, or the workflows/<run>/ file that exists, or the
// agent_transcript_path SubagentStart/SubagentStop carry. A transcript
// path under a subagents/ directory is a subagent even without agent_id.
func TestHookClaudeSubagentTranscript(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, claudeSID+".jsonl")
	wf := filepath.Join(dir, claudeSID, "subagents", "workflows", "run1", "agent-"+claudeAgentID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(wf), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wf, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fa := newHookAgent(t)
	out, _ := runHook(t, fa.sock, claudeSubagentIn(evPostToolUse, parent), nil)
	assertTookNothing(t, "workflow subagent", fa, out, wf)

	direct := filepath.Join(dir, claudeSID, "subagents", "agent-"+claudeAgentID+".jsonl")
	if err := os.WriteFile(direct, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fa = newHookAgent(t)
	out, _ = runHook(t, fa.sock, claudeSubagentIn(evPostToolUse, parent), nil)
	assertTookNothing(t, "subagent", fa, out, direct)

	var m map[string]any
	json.Unmarshal([]byte(claudeSubagentIn("SubagentStop", parent)), &m)
	m["agent_transcript_path"] = "/elsewhere/agent-" + claudeAgentID + ".jsonl"
	fa = newHookAgent(t)
	out, _ = runHook(t, fa.sock, hookJSON(m), nil)
	assertTookNothing(t, "SubagentStop", fa, out, "/elsewhere/agent-"+claudeAgentID+".jsonl")

	json.Unmarshal([]byte(claudeIn(evPostToolUse)), &m)
	delete(m, "agent_id")
	m["transcript_path"] = direct
	fa = newHookAgent(t)
	pendingForParent(fa)
	out, _ = runHook(t, fa.sock, hookJSON(m), map[string]string{"CLAUDECODE": "1"})
	assertTookNothing(t, "subagent transcript without agent_id", fa, out, direct)
}

// A session's own transcript can sit under a directory named subagents
// (CLAUDE_CONFIG_DIR or a home there): it is not a subagent's, and its
// hook delivers. Only an agent-<id>.jsonl file under subagents/ is one.
func TestHookClaudeSubagentsDirAboveTheSession(t *testing.T) {
	var m map[string]any
	json.Unmarshal([]byte(claudeIn(evPostToolUse)), &m)
	m["transcript_path"] = "/work/subagents/.claude/projects/-src-api/" + claudeSID + ".jsonl"
	fa := newHookAgent(t)
	pendingForParent(fa)
	if out, _ := runHook(t, fa.sock, hookJSON(m), map[string]string{"CLAUDECODE": "1"}); !strings.Contains(out, "for the parent session") {
		t.Fatalf("session under a subagents/ directory: %q", out)
	}
}

// Codex runs hooks in spawned agents with the root thread's session_id,
// the child's own rollout and agent_id (codex 0.160.0).
func TestHookCodexSubagentTakesNothing(t *testing.T) {
	for _, ev := range []string{evPostToolUse, evUserPromptSubmit, evSessionStart, "Stop", "SubagentStop", evSessionEnd} {
		fa := newHookAgent(t)
		pendingForParent(fa)
		out, _ := runHook(t, fa.sock, codexSubagentIn(ev), nil)
		assertTookNothing(t, "codex subagent "+ev, fa, out, codexChildTranscript)
	}
	// Without agent_id, a rollout that is not the session's own thread is a
	// subagent's too: delivering there is delivering to another context.
	var m map[string]any
	json.Unmarshal([]byte(codexSubagentIn(evPostToolUse)), &m)
	delete(m, "agent_id")
	delete(m, "agent_type")
	fa := newHookAgent(t)
	pendingForParent(fa)
	out, _ := runHook(t, fa.sock, hookJSON(m), nil)
	assertTookNothing(t, "codex rollout of another thread", fa, out, codexChildTranscript)

	fa = newHookAgent(t)
	pendingForParent(fa)
	if out, _ := runHook(t, fa.sock, codexIn(evPostToolUse), nil); !strings.Contains(out, "for the parent session") {
		t.Fatalf("root thread hook: %q", out)
	}
}

// Devin runs a run_subagent subagent's hooks with the session's id and
// nothing else to tell them apart; its tool calls are not in the store
// while it runs, and it fires a Stop of its own before the run_subagent
// call returns (devin 3000.11.1).
func TestHookDevinSubagentTakesNothing(t *testing.T) {
	env := maps.Clone(devinEnv)
	env["CHISEL_SESSION_DB"] = filepath.Join(t.TempDir(), "cli", "sessions.db")
	db := writeDevinStore(t, env["CHISEL_SESSION_DB"], "exec:exec_0_be34b330#0c0a", "run_subagent:run_subagent_0_4b2c#dde4")

	// The parent's own tool call delivers.
	fa := newHookAgent(t)
	pendingForParent(fa)
	if out, _ := runHook(t, fa.sock, devinToolIn(evPostToolUse, "exec_0_be34b330#0c0a"), env); !strings.Contains(out, "for the parent session") {
		t.Fatalf("parent tool call: %q", out)
	}
	// The subagent's tool call (not in the store) takes nothing.
	fa = newHookAgent(t)
	pendingForParent(fa)
	out, _ := runHook(t, fa.sock, devinToolIn(evPostToolUse, "exec_0_8202fb5f#2c26"), env)
	assertTookNothing(t, "devin subagent PostToolUse", fa, out, "-")
	// The subagent's Stop while run_subagent runs: no idle signal.
	fa = newHookAgent(t)
	out, _ = runHook(t, fa.sock, devinIn("Stop"), env)
	assertTookNothing(t, "devin subagent Stop", fa, out, "-")
	// The run_subagent call's own PostToolUse is the parent's.
	fa = newHookAgent(t)
	pendingForParent(fa)
	if out, _ := runHook(t, fa.sock, devinToolIn(evPostToolUse, "run_subagent_0_4b2c#dde4"), env); !strings.Contains(out, "for the parent session") {
		t.Fatalf("run_subagent PostToolUse: %q", out)
	}
	// Once the subagent returned, the parent's Stop is the idle signal.
	devinNode(t, db, map[string]any{"message_id": "r1", "role": "tool", "content": "Subagent completed", "tool_call_id": "run_subagent_0_4b2c#dde4"})
	devinNode(t, db, map[string]any{"message_id": "a9", "role": "assistant", "content": "done", "tool_calls": []any{}})
	fa = newHookAgent(t)
	runHook(t, fa.sock, devinIn("Stop"), env)
	if f := waitFlush(t, fa, 1); f[0].Event != "Stop" {
		t.Fatalf("parent Stop: flush %+v", f[0])
	}
}

// Without a readable store a Devin PostToolUse cannot tell whose it is:
// it delivers nothing. UserPromptSubmit and SessionStart, which subagents
// do not fire, still deliver.
func TestHookDevinWithoutTheStoreDeliversOnlyAtPrompts(t *testing.T) {
	env := maps.Clone(devinEnv)
	env["CHISEL_SESSION_DB"] = filepath.Join(t.TempDir(), "missing", "sessions.db")
	for _, c := range []struct {
		ev      string
		deliver bool
	}{{evPostToolUse, false}, {evUserPromptSubmit, true}, {evSessionStart, true}} {
		fa := newHookAgent(t)
		pendingForParent(fa)
		out, errOut := runHook(t, fa.sock, devinIn(c.ev), env)
		if got := strings.Contains(out, "for the parent session"); got != c.deliver {
			t.Fatalf("%s without a store: delivered %v, want %v (%q)", c.ev, got, c.deliver, out)
		}
		// Delivery stopping because the store cannot be read is a fault,
		// not a subagent: it must say so where it can be found.
		if !c.deliver && !strings.Contains(errOut, "session store") {
			t.Fatalf("%s without a store: nothing on stderr says why nothing was delivered: %q", c.ev, errOut)
		}
	}
	// Stop without a store keeps its event: it never delivers, and a
	// session without a store has no subagent the hook could tell of.
	fa := newHookAgent(t)
	runHook(t, fa.sock, devinIn("Stop"), env)
	if f := waitFlush(t, fa, 1); f[0].Event != "Stop" {
		t.Fatalf("Stop without a store: %+v", f[0])
	}
}

func TestDevinStoreFixture(t *testing.T) {
	env := devinHookEnv(t)
	db, err := sql.Open("sqlite", "file:"+env["CHISEL_SESSION_DB"]+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM message_nodes WHERE session_id = ?`, devinSID).Scan(&n); err != nil || n != 2 {
		t.Fatal(fmt.Sprint(n, err))
	}
}
