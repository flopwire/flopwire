package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

func TestAntigravityHookDelivery(t *testing.T) {
	f := newHookAgent(t)
	f.mu.Lock()
	f.msgs = []busproto.Envelope{{ID: "message-id", Body: "synthetic Antigravity delivery", Intent: busproto.IntentInform}}
	f.mu.Unlock()
	const id = "96323ff5-1947-4546-8118-be2e50fd50e9"
	path := filepath.Join(t.TempDir(), "brain", id, ".system_generated", "logs", "transcript.jsonl")
	run := func(event string, idle bool) string {
		t.Helper()
		raw := hookJSON(map[string]any{"conversationId": id, "transcriptPath": path, "fullyIdle": idle})
		var stdout, stderr strings.Builder
		args := []string{"--socket", f.sock, "--antigravity", event}
		// Launching agy from another harness must not misclassify its hook.
		env := func(string) string { return "inherited-harness-value" }
		if err := hookCmd(t.Context(), args, strings.NewReader(raw), &stdout, &stderr, env); err != nil || stderr.Len() != 0 {
			t.Fatalf("%s returned %v, stderr = %q", event, err, stderr.String())
		}
		return stdout.String()
	}

	if out := run("SessionStart", false); out != "" || len(f.requests("pending")) != 0 {
		t.Fatalf("SessionStart took a message: %s", out)
	}
	var output antigravityOutput
	if raw := run("PreInvocation", false); json.Unmarshal([]byte(raw), &output) != nil || len(output.InjectSteps) != 1 ||
		!strings.Contains(output.InjectSteps[0].SystemMessage.Text, "synthetic Antigravity delivery") {
		t.Fatalf("PreInvocation output = %s", raw)
	}
	pending := f.requests("pending")
	if len(pending) != 1 || pending[0].Session != id || pending[0].Agent != string(transcript.AgentAntigravity) || pending[0].Notice {
		t.Fatalf("pending = %+v", pending)
	}
	confirm := f.requests("confirm")
	if len(confirm) != 1 || len(confirm[0].IDs) != 1 || confirm[0].IDs[0] != "message-id" {
		t.Fatalf("confirm = %+v", confirm)
	}
	run("Stop", false)
	run("Stop", true)
	events := map[string]int{}
	for _, req := range waitFlush(t, f, 4) {
		if req.Path != path || req.Session != id || req.Agent != string(transcript.AgentAntigravity) {
			t.Fatalf("wrong flush request: %+v", req)
		}
		events[req.Event]++
	}
	for _, e := range []string{evSessionStart, evUserPromptSubmit, "", "Stop"} {
		if events[e] != 1 {
			t.Fatalf("flush events = %+v", events)
		}
	}
}
