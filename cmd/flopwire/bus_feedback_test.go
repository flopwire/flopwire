package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/retrieval/local"
)

// Recovery must preserve explicit retry semantics instead of suggesting a new send.
func TestRetryRefusalRecoveryRendered(t *testing.T) {
	asCaller(t, claudeSelf)
	for _, tc := range []struct{ code, detail, want string }{
		{busproto.CodeNotFound, "retry_of: source not found", "verify retry_of in your sent inbox"},
		{busproto.CodeNotFound, "retry_of: successor no longer available", "verify retry_of in your sent inbox"},
		{busproto.CodeBadRequest, "retry_of: sender, recipient and message content must match the source", "preserving its exact recipient, message, intent, refs and reply_to"},
		{busproto.CodeBadRequest, "retry_of: source must be undelivered with reason session_ended", "retry only an undelivered/session_ended message"},
	} {
		t.Run(tc.detail, func(t *testing.T) {
			fa := startFakeAgent(t, func(agent.Request) agent.Response { return refused(busproto.Error{Code: tc.code, Detail: tc.detail}) })
			_, err := cli(t, fa, "", "send", "0b7e2c1a", "--retry-of", "m1", "--", "hi")
			if err == nil {
				t.Fatal("expected refusal")
			}
			check := func(text, example string) {
				t.Helper()
				if !strings.Contains(text, tc.want) || !strings.Contains(text, example) || strings.Contains(text, "reply_to takes") || strings.Contains(text, "Heads-up") {
					t.Fatalf("misleading recovery: %s", text)
				}
			}
			check(err.Error(), "flopwire inbox --sent")
			r := &retriever{caller: func(context.Context) (local.Caller, bool) { return *claudeSelf, true }, busSocket: fa.sock}
			for _, mode := range []string{"json", "text"} {
				response := mcpRoundTrip(t, r, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"flopwire_send","arguments":{"to":"0b7e2c1a","message":"hi","retry_of":"m1","format":"`+mode+`"}}}`)
				text, isErr, _ := mcpContent(t, response)
				if !isErr {
					t.Fatal("expected MCP error")
				}
				check(text, "flopwire_inbox sent=true")
			}
		})
	}
}

func TestDiscoveryAndDuplicateRecoveryRendered(t *testing.T) {
	for _, mcp := range []bool{false, true} {
		st := busStyle{MCP: mcp}
		text := refusal(&busproto.Error{Code: busproto.CodeUnknownRecipient}, busproto.SendRequest{}, st).Error()
		if strings.Contains(text, "branch") || !strings.Contains(text, "verify its command/result evidence") {
			t.Fatalf("discovery recovery: %s", text)
		}
		text = refusal(&busproto.Error{Code: busproto.CodeDuplicate}, busproto.SendRequest{}, st).Error()
		if strings.Contains(text, "on its way") || !strings.Contains(text, "check its state") {
			t.Fatalf("duplicate recovery: %s", text)
		}
	}
	var out bytes.Buffer
	if err := writePeers(&out, peersJSON{}, peersArgs{Session: "abcd"}, busStyle{}); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "no visible live session starts with abcd") || !strings.Contains(text, "new send receipt") || strings.Contains(text, "waits until") || strings.Contains(text, "it ended") {
		t.Fatalf("presence absence overclaims: %s", text)
	}
}
