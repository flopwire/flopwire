package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/busrender"
)

const opencodeSID = "ses_synthetic00000000000001"

func opencodeIn(event string, extra map[string]any) string {
	m := map[string]any{"hook_event_name": event, "harness": "opencode", "session_id": opencodeSID}
	for k, v := range extra {
		m[k] = v
	}
	return hookJSON(m)
}

// The plugin's delivery: the hook prints each pending message as its own
// wrapper text and confirms nothing; the plugin's Confirm confirms. The
// flush names the harness and no transcript path.
func TestHookOpencodeDeliverThenConfirm(t *testing.T) {
	fa := newHookAgent(t)
	fa.msgs = []busproto.Envelope{testEnvelope("m1", "first", busproto.IntentInform), testEnvelope("m2", "second", busproto.IntentRequest)}
	fa.resp.Instruct = true
	out, _ := runHook(t, fa.sock, opencodeIn(evPostToolUse, map[string]any{"tool_name": "bash"}), nil)
	var got opencodeOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if !got.Instruction || len(got.Messages) != 2 {
		t.Fatalf("output: %+v", got)
	}
	for i, id := range []string{"m1", "m2"} {
		m := got.Messages[i]
		if m.ID != id || !strings.HasPrefix(m.Text, `<flopwire-message id="`+id+`"`) || strings.Count(m.Text, "<flopwire-message ") != 1 {
			t.Fatalf("message %d: %+v", i, m)
		}
	}
	if strings.Contains(out, "flopwire-instructions") {
		t.Fatal("the standing instruction went into the delivered text; it belongs in the system prompt")
	}
	if c := fa.requests("confirm"); len(c) != 0 {
		t.Fatalf("the hook confirmed before the plugin delivered: %+v", c)
	}
	f := waitFlush(t, fa, 1)
	if f[0].Agent != "opencode" || f[0].Path != "" || f[0].Session != opencodeSID {
		t.Fatalf("flush: %+v", f[0])
	}

	out, _ = runHook(t, fa.sock, opencodeIn(evConfirm, map[string]any{"ids": []string{"m1", "m2"}, "instruction": true}), nil)
	if out != "" {
		t.Fatalf("Confirm printed %q", out)
	}
	if c := fa.requests("confirm"); len(c) != 1 || c[0].Session != opencodeSID || strings.Join(c[0].IDs, ",") != "m1,m2" || !c[0].Instruction {
		t.Fatalf("confirm: %+v", c)
	}
}

// A subagent's event (agent_id: its child session) delivers nothing.
func TestHookOpencodeSubagentTakesNothing(t *testing.T) {
	fa := newHookAgent(t)
	fa.msgs = []busproto.Envelope{testEnvelope("m1", "first", busproto.IntentInform)}
	out, _ := runHook(t, fa.sock, opencodeIn(evPostToolUse, map[string]any{"agent_id": "ses_child000000000000000001"}), nil)
	if out != "" || len(fa.requests("pending")) != 0 {
		t.Fatalf("a subagent's hook took messages: %q %+v", out, fa.requests("pending"))
	}
}

// Hello gives the plugin the instruction, the tools and the registry
// directory, and asks the agent nothing.
func TestHookOpencodeHello(t *testing.T) {
	fa := newHookAgent(t)
	t.Setenv("FLOPWIRE_CONFIG", "/cfg/flopwire/config.json")
	out, _ := runHook(t, fa.sock, opencodeIn(evHello, nil), nil)
	var h opencodeHello
	if err := json.Unmarshal([]byte(out), &h); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if h.Instruction != busrender.StandingInstruction || h.Registry != "/cfg/flopwire/opencode" || len(h.Tools) != len(mcpTools()) || !strings.Contains(h.MCP, "flopwire_send") {
		t.Fatalf("hello: %+v", h)
	}
	if len(fa.requests("")) != 0 || len(fa.reqs) != 0 {
		t.Fatalf("Hello asked the agent: %+v", fa.reqs)
	}
}
