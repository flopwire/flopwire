package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/retrieval/format"
)

// TestMCPWithoutIndex: on a device where the agent has never run, flopwire
// mcp still starts and answers. A harness would otherwise mark the server
// failed (Claude Code then skips it for 15 minutes). Retrieval tools return
// a short "no index yet" error result; the messaging tools answer from the
// agent (here: not running) rather than taking the server down.
func TestMCPWithoutIndex(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLOPWIRE_INDEX", filepath.Join(dir, "missing", "index.db"))
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(dir, "fw", "config.json"))
	in := filepath.Join(dir, "in")
	req := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"flopwire_grep","arguments":{"pattern":"x"}}}` + "\n" +
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"flopwire_peers","arguments":{}}}` + "\n"
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
		t.Fatalf("mcp without an index exited: %v", err)
	}
	out, _ := os.ReadFile(fout.Name())
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 4 {
		t.Fatalf("mcp answered %d lines, want 4:\n%s", len(lines), out)
	}
	// Tool calls are answered concurrently: order the answers by id.
	byID := make([]string, 4)
	for _, l := range lines {
		var r struct {
			ID int `json:"id"`
		}
		if json.Unmarshal([]byte(l), &r) != nil || r.ID < 1 || r.ID > 4 || byID[r.ID-1] != "" {
			t.Fatalf("answer without a request id 1-4: %s", l)
		}
		byID[r.ID-1] = l
	}
	lines = byID
	if !strings.Contains(lines[0], `"serverInfo"`) {
		t.Errorf("initialize: %s", lines[0])
	}
	for _, tool := range []string{"flopwire_grep", "flopwire_send", "flopwire_peers"} {
		if !strings.Contains(lines[1], `"`+tool+`"`) {
			t.Errorf("tools/list lacks %s: %s", tool, lines[1])
		}
	}
	if !strings.Contains(lines[2], `"isError":true`) || !strings.Contains(lines[2], "no index yet") || !strings.Contains(lines[2], "flopwire agent run") {
		t.Errorf("grep without an index: want an isError result saying no index yet; got %s", lines[2])
	}
	if !strings.Contains(lines[3], `"isError":true`) || !strings.Contains(lines[3], codeAgentNotRunning) {
		t.Errorf("peers with no agent: want the agent_not_running error result; got %s", lines[3])
	}
}

// TestLazyIndexOpensOnceItExists: a server that started before the index
// existed serves it as soon as the agent has built it, without a restart.
func TestLazyIndexOpensOnceItExists(t *testing.T) {
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "none.json"))
	db := filepath.Join(t.TempDir(), "index.db")
	b := &lazyIndexBackend{path: db}
	defer b.Close()
	if _, err := b.Sessions(t.Context(), "", "", format.Filters{}); err == nil || !strings.Contains(err.Error(), "no index yet") {
		t.Fatalf("before the index exists: %v", err)
	}
	s, err := localindex.Open(db, localindex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := b.Sessions(t.Context(), "", "", format.Filters{}); err != nil {
		t.Fatalf("after the index exists: %v", err)
	}
}
