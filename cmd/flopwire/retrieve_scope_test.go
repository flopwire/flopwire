package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/retrieval/format"
)

func TestRetrievalScopeSelection(t *testing.T) {
	t.Setenv(client.EnvToken, "")
	t.Setenv(client.EnvServer, "")
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	check := func(server, local bool, index string, want bool, wantErr bool) {
		t.Helper()
		got, err := retrievalServer(server, local, index)
		if got != want || (err != nil) != wantErr {
			t.Fatalf("server=%v local=%v index=%q: got=%v err=%v", server, local, index, got, err)
		}
	}
	check(false, false, "", false, false) // no enrollment
	c := client.Config{Server: "https://shared.example.test", Token: "session"}
	if err := client.Save(c); err != nil {
		t.Fatal(err)
	}
	check(false, false, "", false, false) // login alone is not enrollment
	c.DeviceID, c.Token = "device", "device-token"
	if err := client.Save(c); err != nil {
		t.Fatal(err)
	}
	check(false, false, "", true, false)
	check(false, true, "", false, false)
	check(false, false, "scratch.sqlite", false, false)
	check(true, false, "scratch.sqlite", true, false)
	check(true, true, "", false, true)
	p, _ := client.Path()
	if err := os.WriteFile(p, []byte("broken config"), 0600); err != nil {
		t.Fatal(err)
	}
	check(false, false, "", false, true)
	check(false, true, "", false, false)
	t.Setenv(client.EnvToken, "minted-token")
	t.Setenv(client.EnvServer, "https://sandbox.example.test")
	check(false, false, "", true, false)
	t.Setenv(client.EnvServer, "")
	check(false, false, "", false, true)
}

func TestEnrolledCLIUsesSharedScopeWithoutLocalFallback(t *testing.T) {
	oracleIndex(t) // a populated local index must not hide a shared outage
	t.Setenv(client.EnvToken, "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/search" {
			t.Errorf("path=%s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer device-token" {
			t.Error("wrong credential")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"hits":[]}`))
	}))
	if err := client.Save(client.Config{Server: srv.URL, Token: "device-token", DeviceID: "device"}); err != nil {
		t.Fatal(err)
	}
	o, err := parseArgs("search", []string{"backoff", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if err := runToolCmd(t.Context(), "search", o, &out, &stderr, true); err != nil {
		t.Fatal(err)
	}
	var answer struct{ Scope struct{ Kind, Server string } }
	if err := json.Unmarshal(out.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	if answer.Scope.Kind != "shared" || answer.Scope.Server != srv.URL {
		t.Fatalf("scope=%+v", answer.Scope)
	}
	srv.Close()
	out.Reset()
	if err := runToolCmd(t.Context(), "search", o, &out, &stderr, true); err == nil {
		t.Fatal("shared outage silently fell back")
	}
	if out.Len() != 0 {
		t.Fatal("outage returned local results")
	}
	o.on["local"] = true
	if err := runToolCmd(t.Context(), "search", o, &out, &stderr, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"kind": "local"`) {
		t.Fatalf("missing local scope: %s", out.String())
	}
}

func TestSharedMCPScopeIsVisibleInInstructionsAndAnswers(t *testing.T) {
	r := &retriever{backend: &blockingBackend{}, scope: &format.Scope{Kind: "shared", Server: "https://shared.example.test"}}
	if note := r.mcpInstructions(); !strings.Contains(note, "[scope: shared server https://shared.example.test]") || !strings.Contains(note, "never fall back") {
		t.Fatalf("instructions=%s", note)
	}
	for _, tool := range []string{"flopwire_search", "flopwire_sessions", "flopwire_read"} {
		args := map[string]any{"format": "json"}
		if tool == "flopwire_search" {
			args["query"] = "backoff"
		}
		if tool == "flopwire_read" {
			args["address"] = "session/1"
		}
		text, err := mcpCall(t.Context(), r, tool, args)
		if err != nil {
			t.Fatal(err)
		}
		var answer struct{ Scope struct{ Kind, Server string } }
		if err := json.Unmarshal([]byte(text), &answer); err != nil {
			t.Fatalf("%s: %v: %s", tool, err, text)
		}
		if answer.Scope.Kind != "shared" || answer.Scope.Server != r.scope.Server {
			t.Fatalf("%s scope=%+v", tool, answer.Scope)
		}
	}
}
