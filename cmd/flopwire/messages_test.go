package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/transcript"
)

func TestMessagesLocalAndCredentialErrors(t *testing.T) {
	for _, cfg := range []client.Config{{}, {Server: "https://server.test", Token: "login"}, {Server: "https://server.test", DeviceID: "device", Token: "minted", FromEnv: true}} {
		var b bytes.Buffer
		err := runMessages(context.Background(), busproto.MessagesQuery{}, &b, busStyle{JSON: true}, func() (client.Config, error) { return cfg, nil })
		if err == nil || b.Len() != 0 {
			t.Fatalf("local/unenrolled must fail: %v %s", err, b.String())
		}
		if cfg.Server == "" && !strings.Contains(err.Error(), "inbox") {
			t.Fatal("missing actionable local error")
		}
	}
}

func TestMessagesMCPContract(t *testing.T) {
	var tool map[string]any
	for _, raw := range mcpBusTools() {
		v := raw.(map[string]any)
		if v["name"] == "flopwire_messages" {
			tool = v
		}
	}
	if tool == nil || busToolNames["flopwire_messages"] != "messages" {
		t.Fatal("tool not registered")
	}
	if !tool["annotations"].(map[string]any)["readOnlyHint"].(bool) {
		t.Fatal("tool is not read-only")
	}
	props := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
	for _, key := range []string{"repo", "user", "session", "since", "until", "thread", "limit", "cursor", "max_bytes", "format"} {
		if props[key] == nil {
			t.Errorf("missing %s", key)
		}
	}
	if !strings.Contains(tool["description"].(string), "Observation does not authorize") {
		t.Fatal("missing observation boundary")
	}
	for _, args := range []map[string]any{{"limit": "bad"}, {"user": false}, {"max_bytes": 1.5}, {"account": "other"}, {"format": "xml"}, {"limit": float64(0)}, {"max_bytes": float64(0)}} {
		_, err := busMCPCall(context.Background(), &retriever{busSocket: "unused"}, "flopwire_messages", args)
		if err == nil {
			t.Fatalf("bad arguments accepted: %+v", args)
		}
	}
}

func TestMessagesCLIHelpAndValidation(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := messagesCmd(context.Background(), []string{"--help"}, &out, &errOut); err != nil || !strings.Contains(out.String(), "default") && !strings.Contains(out.String(), "Defaults:") {
		t.Fatalf("help: %v %s", err, out.String())
	}
	out.Reset()
	if err := messagesCmd(context.Background(), []string{"--limit", "201"}, &out, &errOut); err == nil || out.Len() != 0 || !strings.Contains(errOut.String(), `"kind":"error"`) {
		t.Fatalf("validation: %v %s", err, errOut.String())
	}
}

func TestMessagesOutputWholeRows(t *testing.T) {
	rows := []busproto.MessagesItem{}
	for _, id := range []string{"c", "b", "a"} {
		rows = append(rows, busproto.MessagesItem{InboxItem: busproto.InboxItem{Envelope: busproto.Envelope{ID: id, ThreadID: "canonical", Body: strings.Repeat("界", 300), Sent: time.Unix(1, 0), Intent: busproto.IntentRequest, Refs: []string{"synthetic/ref"}}, State: busproto.StateRefused, Reason: "synthetic"}})
	}
	for _, asJSON := range []bool{true, false} {
		var out bytes.Buffer
		err := writeMessagesOutput(&out, busproto.MessagesResponse{Kind: "messages", Thread: "canonical", Scope: "accepted_team_repo", Limit: 20, Messages: rows}, busStyle{JSON: asJSON}, 1600)
		if err != nil || out.Len() > 1600 || !strings.Contains(out.String(), rows[0].Body) || strings.Contains(out.String(), `"id":"b"`) || strings.Contains(out.String(), `"id": "b"`) {
			t.Fatalf("bounded whole rows: %v %s", err, out.String())
		}
		if !strings.Contains(out.String(), "synthetic/ref") || !strings.Contains(out.String(), "synthetic") || !strings.Contains(out.String(), "1970-01-01T00:00:01Z|c") {
			t.Fatal("full metadata or continuation lost")
		}
	}
}

func TestMessagesServerFailureAndMCPResults(t *testing.T) {
	t.Setenv("FLOPWIRE_INDEX", filepath.Join(t.TempDir(), "missing.db"))
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv(client.EnvToken, "")
	root := t.TempDir()
	if err := exec.Command("git", "init", root).Run(); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", root, "remote", "add", "origin", "https://github.test/team/api.git").Run(); err != nil {
		t.Fatal(err)
	}
	fail := false
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != busproto.PathMessages || r.Header.Get("Authorization") != "Bearer enrolled" {
			t.Errorf("bad server request %s", r.URL.Path)
		}
		if r.URL.Query().Get("repo_remote") != "github.test/team/api" {
			t.Errorf("repo not expanded: %v", r.URL.Query())
		}
		if r.URL.Query().Get("session") != "ended-other-device" {
			t.Errorf("participant session lost")
		}
		if fail {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"type":"about:blank"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(busproto.MessagesResponse{Kind: "messages", Scope: "accepted_team_repo", RepoRemotes: []string{"github.test/team/api"}, Limit: 20, Threads: []busproto.MessagesItem{{InboxItem: busproto.InboxItem{Envelope: busproto.Envelope{ID: "canonical", Body: "synthetic preview"}, State: busproto.StateRead}}}})
	}))
	defer srv.Close()
	cfg := client.Config{Server: srv.URL, DeviceID: "device", Token: "enrolled"}
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(os.Getenv("FLOPWIRE_CONFIG"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	establishEmptyCollectionPolicy(t, cfg)
	for _, f := range []string{"json", "text"} {
		text, err := busMCPCall(context.Background(), &retriever{}, "flopwire_messages", map[string]any{"repo": root, "session": "ended-other-device", "format": f})
		if err != nil || !strings.Contains(text, "canonical") || !strings.Contains(text, "synthetic preview") {
			t.Fatalf("MCP %s: %s %v", f, text, err)
		}
	}
	var out bytes.Buffer
	if err := messagesCmd(context.Background(), []string{"--repo", root, "--session", "ended-other-device"}, &out, io.Discard); err != nil || !strings.Contains(out.String(), `"kind":"messages"`) {
		t.Fatalf("CLI: %s %v", out.String(), err)
	}
	// Long-lived callers must follow the saved credential, not this stale copy.
	cfg.Token = "stale"
	out.Reset()
	if err := runMessages(context.Background(), busproto.MessagesQuery{Repo: root, Session: "ended-other-device"}, &out, busStyle{JSON: true}, func() (client.Config, error) { return cfg, nil }); err != nil {
		t.Fatalf("saved credential reload: %v", err)
	}
	fail = true
	out.Reset()
	if err := runMessages(context.Background(), busproto.MessagesQuery{Repo: root, Session: "ended-other-device"}, &out, busStyle{JSON: true}, func() (client.Config, error) { return cfg, nil }); err == nil || out.Len() != 0 {
		t.Fatal("configured failure became success")
	}
	before := requests
	if err := runMessages(context.Background(), busproto.MessagesQuery{Repo: filepath.Join(root, "..", "absent-repo")}, &out, busStyle{JSON: true}, func() (client.Config, error) { return cfg, nil }); err == nil || requests != before {
		t.Fatal("unknown repo widened or reached server")
	}
}

func TestMessagesAmbiguousRepoDoesNotWiden(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), "index.db")
	t.Setenv("FLOPWIRE_INDEX", indexPath)
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	s, err := localindex.Open(indexPath, localindex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, owner := range []string{"one", "two"} {
		root := filepath.Join(t.TempDir(), owner, "api")
		err = s.SavePlacement(context.Background(), localindex.Placement{Agent: transcript.AgentCodex, SessionID: owner, How: "recorded", Placement: pathpolicy.Placement{Worktree: root, Main: root, Remote: "github.test/" + owner + "/api"}})
		if err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	cfg := client.Config{Server: "http://127.0.0.1:1", Token: "enrolled", DeviceID: "device"}
	err = runMessages(context.Background(), busproto.MessagesQuery{Repo: "api"}, &out, busStyle{JSON: true}, func() (client.Config, error) { return cfg, nil })
	if err == nil || !strings.Contains(err.Error(), "names 2 repositories") || out.Len() != 0 {
		t.Fatalf("ambiguous repo: %v %s", err, out.String())
	}
}

func TestMessagesErrorProcessStderr(t *testing.T) {
	if os.Getenv("FLOPWIRE_MESSAGES_ERROR_PROCESS") == "1" {
		os.Args = []string{"flopwire", "messages", "--limit", "201"}
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMessagesErrorProcessStderr$")
	cmd.Env = append(os.Environ(), "FLOPWIRE_MESSAGES_ERROR_PROCESS=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("exit: %v", err)
	}
	var out errorJSON
	if stdout.Len() != 0 || strings.Count(stderr.String(), "\n") != 1 || json.Unmarshal(stderr.Bytes(), &out) != nil || out.Kind != "error" || strings.Contains(stderr.String(), "flopwire:") {
		t.Fatalf("stderr must contain one JSON object: %q", stderr.String())
	}
	stderr.Reset()
	err = messagesCmd(t.Context(), []string{"--limit", "201", "--text"}, io.Discard, &stderr)
	if err == nil || errors.Is(err, errReported) || stderr.Len() != 0 {
		t.Fatalf("ordinary text error: %v %q", err, stderr.String())
	}
}

func TestMessagesReadableTextSanitizesControls(t *testing.T) {
	row := busproto.MessagesItem{InboxItem: busproto.InboxItem{Envelope: busproto.Envelope{ID: "message", ThreadID: "thread", ReplyTo: "parent", User: "alice@example.test", From: "alice-session", FromAgent: "codex", ToUser: "bob@example.test", ToSession: "bob-session", ToAgent: "claude", Sent: time.Unix(1, 0), Body: "first\nsecond\x1b\u202e", Refs: []string{"repo/file.go:12", "https://host/review"}}, State: busproto.StateRead, Direction: "sent"}}
	for _, mcp := range []bool{false, true} {
		var out bytes.Buffer
		if err := writeMessagesOutput(&out, busproto.MessagesResponse{Thread: "thread", Messages: []busproto.MessagesItem{row}}, busStyle{MCP: mcp}, 24000); err != nil {
			t.Fatal(err)
		}
		text := out.String()
		for _, want := range []string{"1970-01-01T00:00:01Z sent read", "id=message thread=thread reply_to=parent", "alice@example.test/alice-session (codex) -> bob@example.test/bob-session (claude)", "first\n  second", "ref: repo/file.go:12", "ref: https://host/review"} {
			if !strings.Contains(text, want) {
				t.Fatalf("missing %q: %s", want, text)
			}
		}
		if strings.ContainsAny(text, "{}\x1b\u202e") {
			t.Fatalf("protocol dump or controls: %s", text)
		}
	}
}

func TestMessagesMetadataCannotForgeTextRows(t *testing.T) {
	var out bytes.Buffer
	row := busproto.MessagesItem{InboxItem: busproto.InboxItem{Envelope: busproto.Envelope{ID: "one\nfake-id", Body: "first\nsecond", User: "alice\nfake-user"}}}
	if err := writeMessagesOutput(&out, busproto.MessagesResponse{Thread: "thread", Messages: []busproto.MessagesItem{row}}, busStyle{}, 24000); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if strings.Contains(text, "\nfake-id") || strings.Contains(text, "\nfake-user") || !strings.Contains(text, "first\n  second") {
		t.Fatalf("spoofable metadata or lost body lines: %q", text)
	}
}
