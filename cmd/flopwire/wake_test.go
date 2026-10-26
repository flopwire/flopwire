package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/retrieval/local"
)

func TestExplicitWakeCLIAndMCP(t *testing.T) {
	asCaller(t, claudeSelf)
	fa := startFakeAgent(t, func(r agent.Request) agent.Response {
		return agent.Response{OK: true, Sent: &busproto.SendResponse{ID: "mwake", ThreadID: "mwake", State: busproto.StateQueued, WakeRequested: r.Send.Wake, Intent: busproto.IntentInform, Sent: t0, ExpiresAt: t0.Add(time.Hour), To: busproto.Recipient{Session: peerID, Agent: "codex", User: "gary@example.test", Live: true, WakeSupported: true}}}
	})
	out, err := cli(t, fa, "", "send", peerID, "--wake", "--", "synthetic wake")
	if err != nil || !strings.Contains(out, "may start a new turn and consume usage") {
		t.Fatalf("CLI warning: %q %v", out, err)
	}
	request := fa.requests()[0]
	if request.Op != "wake-send-v1" || !request.Send.Wake {
		t.Fatalf("CLI request: %+v", request)
	}
	r := &retriever{caller: func(context.Context) (local.Caller, bool) { return *claudeSelf, true }, busSocket: fa.sock}
	raw, err := mcpCall(t.Context(), r, "flopwire_send", map[string]any{"to": peerID, "message": "synthetic wake", "wake": true})
	var receipt sendJSON
	if err != nil || json.Unmarshal([]byte(raw), &receipt) != nil || !receipt.WakeRequested || receipt.Arrives != "wake_requested" || !strings.Contains(receipt.Outcome, "not confirmed") {
		t.Fatalf("MCP receipt: %q %v", raw, err)
	}
	request = fa.requests()[1]
	if request.Op != "wake-send-v1" || !request.Send.Wake {
		t.Fatalf("MCP request: %+v", request)
	}
	_, err = cli(t, fa, "", "send", peerID, "--", "ordinary")
	if err != nil {
		t.Fatal(err)
	}
	request = fa.requests()[2]
	if request.Op != "send" || request.Send.Wake {
		t.Fatal("ordinary send requested wake")
	}
}
