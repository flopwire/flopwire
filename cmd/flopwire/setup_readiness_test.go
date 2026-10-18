package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/retrieval/local"
	"github.com/flopwire/flopwire/internal/transcript"
)

// Check reports installation prerequisites, not the calling harness's tool
// authorization. Exercise its report alongside real MCP JSON-RPC operations:
// untrusted Codex hooks and a stopped collector do not prevent archive reads.
func TestSetupCheckLocalRetrievalWithStoppedAgentAndUntrustedHooks(t *testing.T) {
	oracleIndex(t)
	index := os.Getenv("FLOPWIRE_INDEX")
	f := newCodexFixture(t, false)
	t.Setenv("FLOPWIRE_INDEX", index)
	t.Setenv("FLOPWIRE_SOCKET", filepath.Join(f.dir, "stopped.sock"))
	t.Setenv(client.EnvServer, "")
	if _, _, err := f.run(); err != nil {
		t.Fatal(err)
	}
	f.codexCalls()
	rep, _, err := f.run("--check")
	if err != nil || !rep.OK || rep.Agent.Running || rep.Index.State != indexIndexed || rep.SearchScope.Kind != "local" {
		t.Fatalf("check: %+v, %v", rep, err)
	}
	h := f.codex(rep)
	if !h.Installed || !h.Enabled || h.HookTrust == nil || len(h.HookTrust.NeedReview) == 0 {
		t.Fatalf("expected installed plugin awaiting hook trust: %+v", h)
	}
	for _, call := range f.codexCalls() {
		if strings.HasPrefix(call, "app-server") {
			t.Fatal("check started the mutating app server")
		}
	}
	r, err := openRetriever(false, index)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	got := readinessMCPCall(t, r, "flopwire_grep", map[string]any{"pattern": "setTimeout", "include_self": true})
	if got.IsError || !strings.Contains(got.text(), "setTimeout") {
		t.Fatalf("local retrieval failed: %+v", got)
	}
	got = readinessMCPCall(t, r, "flopwire_peers", nil)
	if !got.IsError || !strings.Contains(got.text(), codeAgentNotRunning) {
		t.Fatalf("stopped agent should block live messaging: %+v", got)
	}
}

// Shared retrieval reaches the authenticated server even when the collector
// is stopped and this device has never created a local index. An empty result
// is a successful query, not evidence that setup or permissions are broken.
func TestSetupCheckSharedRetrievalWithoutLocalIndex(t *testing.T) {
	f := newSetupFixture(t, false)
	t.Setenv("FLOPWIRE_SOCKET", filepath.Join(f.dir, "stopped.sock"))
	t.Setenv(client.EnvServer, "")
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/sync/coverage" {
			http.NotFound(w, req)
			return
		}
		if req.URL.Path != "/v1/search" || req.Header.Get("Authorization") != "Bearer synthetic-device-token" {
			t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hits":[]}`))
	}))
	defer srv.Close()
	if err := client.Save(client.Config{Server: srv.URL, Token: "synthetic-device-token", DeviceID: "synthetic-device"}); err != nil {
		t.Fatal(err)
	}
	rep, _, err := f.run("--check")
	if err != nil || !rep.OK || rep.Agent.Running || rep.Index.State != indexMissing || rep.SearchScope.Kind != "shared" {
		t.Fatalf("check: %+v, %v", rep, err)
	}
	if requests.Load() != 0 {
		t.Fatal("setup check unexpectedly queried shared history")
	}
	if rep.CheckNote != setupCheckNote {
		t.Fatalf("check must distinguish inspection from session verification: %q", rep.CheckNote)
	}
	r, err := openRetriever(true, "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	got := readinessMCPCall(t, r, "flopwire_search", map[string]any{"query": "synthetic absent phrase"})
	if got.IsError || requests.Load() != 1 || !strings.Contains(got.text(), "shared") {
		t.Fatalf("shared retrieval: %+v; requests=%d", got, requests.Load())
	}
	if _, err := os.Stat(rep.Index.Path); !os.IsNotExist(err) {
		t.Fatalf("shared retrieval created or used a local index: %v", err)
	}
}

// Hook trust and tool execution are separate paths. The test drives the MCP
// protocol and the actual local bus/hook, without granting a harness approval
// or claiming that a direct MCP connection proves harness tool permission.
func TestSetupCheckUntrustedHooksDoNotDisableMCPMessaging(t *testing.T) {
	f := newCodexFixture(t, false)
	if _, _, err := f.run(); err != nil {
		t.Fatal(err)
	}
	sock := hookE2E(t)
	t.Setenv("FLOPWIRE_SOCKET", sock)
	rep, _, err := f.run("--check")
	if err != nil || !rep.OK || !rep.Agent.Running || len(f.codex(rep).HookTrust.NeedReview) == 0 {
		t.Fatalf("check: %+v, %v", rep, err)
	}
	r := &retriever{busSocket: sock, caller: func(context.Context) (local.Caller, bool) {
		return local.Caller{Agent: transcript.AgentClaude, SessionID: e2eA, Rule: "synthetic test caller"}, true
	}}
	const text = "Synthetic setup readiness test: MCP is independent of hook trust."
	got := readinessMCPCall(t, r, "flopwire_send", map[string]any{"to": e2eB, "message": text})
	if got.IsError || !strings.Contains(got.text(), "send_receipt") {
		t.Fatalf("MCP send: %+v", got)
	}
	outputs := runHooks(t, sock, hookFor(e2eB, evSessionStart), 1)
	if len(outputs) != 1 || !strings.Contains(contextOf(t, outputs[0]), text) {
		t.Fatalf("message did not reach receiver context: %q", outputs)
	}
	// Completing a direct synthetic round trip cannot verify the caller's
	// normal harness connection or its authorization policy.
	after, _, err := f.run("--check")
	if err != nil || after.CheckNote != setupCheckNote || len(f.codex(after).HookTrust.NeedReview) == 0 {
		t.Fatalf("check overstated runtime evidence: %+v, %v", after, err)
	}
}

type readinessMCPResult struct {
	IsError bool `json:"isError"`
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
}

func (r readinessMCPResult) text() string {
	var b strings.Builder
	for _, c := range r.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}

func readinessMCPCall(t *testing.T, r *retriever, name string, args map[string]any) readinessMCPResult {
	t.Helper()
	input, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := serveMCP(t.Context(), r, bytes.NewReader(append(input, '\n')), &output); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Result readinessMCPResult `json:"result"`
		Error  json.RawMessage    `json:"error"`
	}
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatalf("MCP response: %v: %s", err, &output)
	}
	if len(response.Error) != 0 {
		t.Fatalf("MCP protocol error: %s", &output)
	}
	return response.Result
}
