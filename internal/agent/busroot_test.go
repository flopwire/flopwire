package agent

import "testing"

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
