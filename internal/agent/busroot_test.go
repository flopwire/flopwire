package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// A subagent's session resolves to the top-level session it belongs to
// (issue #107); a top-level session and an unknown id stand.
func TestBusRoot(t *testing.T) {
	f := newFixture(t, "-")
	f.once()
	for _, c := range []struct{ agent, session, want string }{
		{"claude", "agent-a1b2c3", alphaID},
		{"", "agent-a1b2c3", alphaID},
		{"claude", alphaID, alphaID},
		{"codex", "agent-a1b2c3", "agent-a1b2c3"}, // another harness's id
		{"claude", "0b7e2c1a-ffff-4000-8000-00000000ffff", "0b7e2c1a-ffff-4000-8000-00000000ffff"},
		{"claude", "", ""},
	} {
		if got := f.a.BusRoot(ctx, c.agent, c.session); got != c.want {
			t.Errorf("BusRoot(%q, %q) = %q, want %q", c.agent, c.session, got, c.want)
		}
	}
}

// A Codex subagent thread that _meta names before the agent has seen its
// rollout (a new file no pass has found yet) resolves to its parent, not
// to itself: the send then names a live session instead of being refused
// as not indexed (issue #71).
func TestBusRootNewCodexSubagent(t *testing.T) {
	f := newFixture(t, "-")
	f.once()
	const parent, child = "019a0000-0000-7000-8000-0000000000a2", "019a0000-0000-7000-8000-0000000000c8"
	rollout := fmt.Sprintf(".codex/sessions/2026/10/04/rollout-2026-10-04T14-05-00-%s.jsonl", child)
	if err := os.MkdirAll(filepath.Dir(f.path(rollout)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.path(rollout), []byte(`{"timestamp":"2026-10-04T14:05:00.000Z","ordinal":0,"type":"session_meta","payload":{"session_id":"`+parent+`","id":"`+child+`","timestamp":"2026-10-04T14:05:00.000Z","cwd":"/tmp/oracle-gamma","originator":"codex_exec","cli_version":"0.160.0","source":{"subagent":{"thread_spawn":{"parent_thread_id":"`+parent+`","depth":1}}},"thread_source":"subagent","parent_thread_id":"`+parent+`"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := f.a.BusRoot(ctx, "codex", child); got != parent {
		t.Fatalf("BusRoot(codex, new subagent) = %q, want %q", got, parent)
	}
}
