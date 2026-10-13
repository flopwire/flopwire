package agent

import (
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/transcript"
)

func TestControlCallerContextUsesExactLocalPlacement(t *testing.T) {
	a := &Agent{places: map[placeKey]placed{
		{transcript.AgentCodex, "parent"}:  {pl: pathpolicy.Placement{Cwd: filepath.Join(t.TempDir(), "parent")}},
		{transcript.AgentCodex, "child"}:   {pl: pathpolicy.Placement{Cwd: filepath.Join(t.TempDir(), "child-worktree")}},
		{transcript.AgentClaude, "parent"}: {pl: pathpolicy.Placement{Cwd: filepath.Join(t.TempDir(), "claude")}},
	}}
	for key, p := range a.places {
		got := ask(t, a, Request{Op: "caller-context", Agent: string(key.agent), Session: key.session})
		if !got.OK || got.Cwd != p.pl.Cwd {
			t.Fatalf("%v: %+v", key, got)
		}
	}
	for _, req := range []Request{{Op: "caller-context", Session: "parent"}, {Op: "caller-context", Agent: "codex", Session: "par"}, {Op: "caller-context", Agent: "codex", Session: "unknown"}} {
		if got := ask(t, a, req); got.OK || got.Cwd != "" {
			t.Fatalf("guessed missing context: %+v", got)
		}
	}
}

func TestControlCallerContextLoadsSyncOnlyPlacement(t *testing.T) {
	store, err := localindex.Open(filepath.Join(t.TempDir(), "index.db"), localindex.Options{SyncOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cwd := filepath.Join(t.TempDir(), "child-worktree")
	if err := store.SavePlacement(t.Context(), localindex.Placement{Agent: transcript.AgentCodex, SessionID: "child", Placement: pathpolicy.Placement{Cwd: cwd}}); err != nil {
		t.Fatal(err)
	}
	a := New(store, Config{})
	if err := a.loadPlaces(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := ask(t, a, Request{Op: "caller-context", Agent: "codex", Session: "child"}); !got.OK || got.Cwd != cwd {
		t.Fatalf("sync-only persisted placement: %+v", got)
	}
}
