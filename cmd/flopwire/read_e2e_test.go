package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/retrieval/local"
	"github.com/flopwire/flopwire/internal/transcript"
)

// appendLine appends one JSONL record to a transcript, as the harness does.
func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

// flushNow has the agent index the transcript and waits for it.
func flushNow(t *testing.T, sock, path string) {
	t.Helper()
	if resp, err := agent.Call(t.Context(), sock, agent.Request{Op: "flush", Path: path}); err != nil || !resp.OK {
		t.Fatalf("flush: %+v %v", resp, err)
	}
}

// Read receipts through the real device agent with no server: a message
// the hook printed reads as delivered until the recipient's transcript
// records it as hook context (Claude Code's hook_additional_context
// attachment), then as read, at that record's time. The same wrapper in a
// tool result (the recipient read another transcript) does not count.
func TestReadReceiptEndToEndLocal(t *testing.T) {
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
	pathB := filepath.Join(projects, "-tmp-e2e-api", e2eB+".jsonl")

	if _, err := busCLI(t, sock, e2eA, "", "send", "e2e0bbbb", "--", "Heads-up: the cursor changed."); err != nil {
		t.Fatal(err)
	}
	in := hookJSON(map[string]any{"session_id": e2eB, "hook_event_name": evPostToolUse, "transcript_path": pathB, "cwd": "/tmp/e2e-api"})
	printed := contextOf(t, runHooks(t, sock, in, 1)[0])
	if !strings.Contains(printed, "<flopwire-message id=") {
		t.Fatalf("hook printed %q", printed)
	}
	sentState := func() string {
		t.Helper()
		out, err := busCLI(t, sock, e2eA, "", "inbox", "--sent")
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := sentState(); !strings.Contains(out, "inform  delivered\n") {
		t.Fatalf("before the transcript records it:\n%s", out)
	}

	rec := func(uuid, parent string, at time.Time, body string) string {
		return fmt.Sprintf(`{"parentUuid":%q,"isSidechain":false,"userType":"external","cwd":"/tmp/e2e-api","sessionId":%q,"version":"2.1.287","gitBranch":"main",%s,"uuid":%q,"timestamp":%q}`,
			parent, e2eB, body, uuid, at.UTC().Format("2006-01-02T15:04:05.000Z"))
	}
	str := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	// The recipient read another session's transcript holding the same
	// wrapper: a tool result, not a sighting.
	now := time.Now().UTC().Truncate(time.Millisecond)
	appendLine(t, pathB, rec("rr-t1", e2eB[:8]+"-a1", now, `"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":`+str(printed)+`}]}`))
	flushNow(t, sock, pathB)
	if out := sentState(); !strings.Contains(out, "inform  delivered\n") {
		t.Fatalf("a tool result marked it read:\n%s", out)
	}
	// The harness records the hook's context.
	seen := time.Now().UTC().Truncate(time.Millisecond) // after the delivery, not in the future
	appendLine(t, pathB, rec("rr-h1", "rr-t1", seen, `"type":"attachment","attachment":{"type":"hook_additional_context","content":[`+str(printed)+`],"hookName":"PostToolUse:Bash","toolUseID":"tu1","hookEvent":"PostToolUse"}`))
	flushNow(t, sock, pathB)
	want := "inform  read " + seen.Format("2006-01-02 15:04Z") + "\n"
	if out := sentState(); !strings.Contains(out, want) {
		t.Fatalf("after the transcript records it, want %q:\n%s", want, out)
	}
	// The recipient's inbox says the same, with read_at in the JSON.
	asCaller(t, &local.Caller{Agent: transcript.AgentClaude, SessionID: e2eB, Rule: "test"})
	var raw, errOut strings.Builder
	if err := busCmd(t.Context(), "inbox", []string{"--socket", sock}, strings.NewReader(""), &raw, &errOut); err != nil {
		t.Fatalf("inbox: %v %s", err, errOut.String())
	}
	var got inboxJSON
	if err := json.Unmarshal([]byte(raw.String()), &got); err != nil || len(got.Messages) != 1 {
		t.Fatalf("inbox JSON %s %v", raw.String(), err)
	}
	if m := got.Messages[0]; m.State != "read" || m.ReadAt == nil || !m.ReadAt.Equal(seen) || m.Direction != "received" {
		t.Fatalf("recipient sees %+v", m)
	}
}
