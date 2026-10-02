package main

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/localindex"
)

// On a sync-only device the local verbs fail with a pointer to --server.
func TestLocalVerbsOnSyncOnlyIndex(t *testing.T) {
	db := filepath.Join(t.TempDir(), "index.db")
	s, err := localindex.Open(db, localindex.Options{SyncOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	t.Setenv("FLOPWIRE_INDEX", db)
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "none.json"))
	for _, args := range [][]string{{"grep", "x"}, {"search", "x"}, {"read", "0b7e2c1a"}, {"sessions", "--text"}} {
		err := run(t.Context(), args)
		if err == nil || err.Error() != "this device is sync-only; use --server" {
			t.Errorf("%v: %v", args, err)
		}
	}
	// In JSON mode (sessions by default, any verb with --json) the error
	// is a JSON object on stderr, with a code and the fix.
	for _, args := range [][]string{{"sessions"}, {"grep", "--json", "x"}} {
		var out, stderr strings.Builder
		err := toolCmdIO(t.Context(), args[0], args[1:], &out, &stderr)
		var e errorJSON
		if !errors.Is(err, errReported) || out.Len() != 0 || json.Unmarshal([]byte(stderr.String()), &e) != nil || e.Kind != "error" ||
			e.Error.Code != codeSyncOnly || e.Error.Detail != "this device is sync-only; use --server" || !strings.Contains(e.Error.Example, "--server") {
			t.Errorf("%v: %v %q", args, err, stderr.String())
		}
	}
}

func TestAgentRunSyncOnlyFlags(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("FLOPWIRE_CONFIG", cfgPath)
	t.Setenv("FLOPWIRE_INDEX", filepath.Join(t.TempDir(), "index.db"))
	if _, err := runAgent(t.Context(), []string{"--sync-only", "--no-sync", "--once"}); err == nil || !strings.Contains(err.Error(), "opposites") {
		t.Errorf("--sync-only --no-sync: %v", err)
	}
	if _, err := runAgent(t.Context(), []string{"--sync-only", "--once"}); err == nil || !strings.Contains(err.Error(), "needs a server") {
		t.Errorf("--sync-only without a server: %v", err)
	}
	if err := os.WriteFile(cfgPath, []byte(`{"server":"https://x.example","token":"t","mode":"bogus"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runAgent(t.Context(), []string{"--once"}); err == nil || !strings.Contains(err.Error(), `mode "bogus"`) {
		t.Errorf("bad mode: %v", err)
	}
}

func TestResolveSyncOnly(t *testing.T) {
	for _, c := range []struct {
		args []string
		mode string
		want bool
	}{
		{nil, "", false},
		{nil, "full", false},
		{nil, "sync-only", true},
		{[]string{"--sync-only"}, "", true},
		{[]string{"--sync-only=false"}, "sync-only", false}, // the flag wins
	} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		v := fs.Bool("sync-only", false, "")
		if err := fs.Parse(c.args); err != nil {
			t.Fatal(err)
		}
		if err := resolveSyncOnly(fs, v, client.Config{Mode: c.mode}, nil); err != nil || *v != c.want {
			t.Errorf("%v mode %q: %v %v", c.args, c.mode, *v, err)
		}
	}
}

// On a sync-only device the MCP server still starts: its instructions say
// the device is sync-only and point to --server, and a tool call answers
// with the same error.
func TestMCPOnSyncOnlyIndex(t *testing.T) {
	db := filepath.Join(t.TempDir(), "index.db")
	s, err := localindex.Open(db, localindex.Options{SyncOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	t.Setenv("FLOPWIRE_INDEX", db)
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "none.json"))
	dir := t.TempDir()
	in := filepath.Join(dir, "in")
	req := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"flopwire_grep","arguments":{"pattern":"x"}}}` + "\n"
	if err := os.WriteFile(in, []byte(req), 0o600); err != nil {
		t.Fatal(err)
	}
	fin, err := os.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	defer fin.Close()
	fout, err := os.Create(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer fout.Close()
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = fin, fout
	err = run(t.Context(), []string{"mcp"})
	os.Stdin, os.Stdout = oldIn, oldOut
	if err != nil {
		t.Fatalf("mcp on a sync-only device: %v", err)
	}
	out, _ := os.ReadFile(fout.Name())
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		t.Fatalf("mcp answered %d lines:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[0], `"instructions":"This device is sync-only`) || !strings.Contains(lines[0], "--server") {
		t.Errorf("instructions do not say the device is sync-only: %s", lines[0])
	}
	if !strings.Contains(lines[1], "this device is sync-only; use --server") || !strings.Contains(lines[1], `"isError":true`) {
		t.Errorf("tool call on a sync-only device: %s", lines[1])
	}
}
