package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/local/localtest"
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
		if r.URL.Path == "/v1/sync/coverage" {
			http.NotFound(w, r)
			return
		}
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

func TestEnrolledLocalReadContinuationsStayLocal(t *testing.T) {
	home := oracleIndex(t)
	t.Setenv(client.EnvToken, "")
	t.Setenv(client.EnvServer, "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("local continuation queried shared server: %s", r.URL.Path)
		http.NotFound(w, r)
	}))
	defer srv.Close()
	if err := client.Save(client.Config{Server: srv.URL, Token: "device-token", DeviceID: "device"}); err != nil {
		t.Fatal(err)
	}
	// This path must remain one shell argument, with no substitutions.
	index := filepath.Join(t.TempDir(), "archive ' $(printf injected) `printf injected`.db")
	store, err := localindex.Open(index, localindex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := localtest.IndexHome(t.Context(), store, home); err != nil {
		store.Close()
		t.Fatal(err)
	}
	store.Close()
	for _, source := range [][]string{{"--local"}, {"--index", index}, {"--local", "--index", index}} {
		args := append([]string{"0b7e2c1a-0000-4000-8000-000000000001/10178560", "--messages-before", "0", "--messages-after", "0"}, source...)
		var out, stderr bytes.Buffer
		if err := toolCmdIO(t.Context(), "read", args, &out, &stderr); err != nil {
			t.Fatal(err)
		}
		followed := 0
		for _, line := range strings.Split(out.String(), "\n") {
			start := strings.Index(line, "flopwire read ")
			if start < 0 {
				continue
			}
			hint := strings.TrimSuffix(line[start:], "]")
			// Interpret the printed command as a shell would, but replace
			// flopwire with an argument recorder. Execute those arguments
			// through the real CLI entry point below.
			recorded, err := exec.Command("/bin/sh", "-c", "flopwire() { printf '%s\\000' \"$@\"; }\n"+hint).Output()
			if err != nil {
				t.Fatalf("hint %q: %v", hint, err)
			}
			argv := strings.Split(strings.TrimSuffix(string(recorded), "\x00"), "\x00")
			if argv[0] != "read" {
				t.Fatalf("hint arguments: %q", argv)
			}
			if len(source) > 1 {
				found := false
				for i := 0; i+1 < len(argv); i++ {
					found = found || argv[i] == "--index" && argv[i+1] == index
				}
				if !found {
					t.Fatalf("hint lost exact index argument: %q", argv)
				}
			}
			var next bytes.Buffer
			if err := toolCmdIO(t.Context(), "read", argv[1:], &next, &stderr); err != nil {
				t.Fatalf("follow %q: %v", hint, err)
			}
			if !strings.HasPrefix(next.String(), "[scope: local device]\n") {
				t.Fatalf("continuation lost local scope: %s", next.String())
			}
			followed++
		}
		if followed != 2 {
			t.Fatalf("wanted earlier/later continuations, got %d: %s", followed, out.String())
		}
	}
}
