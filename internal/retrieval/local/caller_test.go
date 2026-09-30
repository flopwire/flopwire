package local

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

func TestDetectorRules(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude", "sessions"), 0o755)
	os.WriteFile(filepath.Join(home, ".claude", "sessions", "300.json"), []byte(`{"pid":300,"sessionId":"claude-live"}`), 0o644)
	// 100 (sh) -> 200 (node wrapper) -> 300 (claude); 400 (sh) -> 500 (codex)
	procs := map[int]struct {
		ppid int
		name string
	}{100: {200, "sh"}, 200: {300, "node"}, 300: {1, "claude"}, 400: {500, "sh"}, 500: {1, "codex"}, 600: {700, "sh"}, 700: {1, "codex"},
		// codex exec --ephemeral run from a Claude Code shell: the codex
		// process holds no rollout, and its parent is a live Claude session.
		800: {900, "sh"}, 900: {300, "codex"}}
	proc := func(pid int) (int, string, bool) { p, ok := procs[pid]; return p.ppid, p.name, ok }
	codexRollout := filepath.Join(home, ".codex", "sessions", "2026", "09", "28", "rollout-2026-09-28T10-07-48-01a0e857-e229-7a82-9cac-d6c507842d91.jsonl")
	files := func(pid int) []string {
		switch pid {
		case 500: // the same rollout open twice is still one thread
			return []string{"/dev/null", filepath.Join(home, ".codex", "logs_2.sqlite"), codexRollout, codexRollout}
		case 700: // an app server holding two threads' rollouts
			return []string{codexRollout, strings.Replace(codexRollout, "d6c507842d91", "d6c507842d92", 1)}
		}
		return nil
	}
	env := map[string]string{}
	d := &Detector{Getenv: func(k string) string { return env[k] }, Home: home, Proc: proc, OpenFiles: files}
	check := func(pid int, agent transcript.Agent, session, rule string) {
		t.Helper()
		d.Pid = pid
		c, ok := d.Detect(context.Background())
		if session == "" {
			if ok {
				t.Fatalf("pid %d: detected %+v", pid, c)
			}
			return
		}
		if !ok || c.Agent != agent || c.SessionID != session || c.Rule != rule {
			t.Fatalf("pid %d: got %+v %v, want %s %s %s", pid, c, ok, agent, session, rule)
		}
	}
	check(100, transcript.AgentClaude, "claude-live", "claude-sessions")
	check(400, transcript.AgentCodex, "01a0e857-e229-7a82-9cac-d6c507842d91", "codex-rollout")
	check(999, "", "", "")
	// C4: a Codex process holding several rollouts identifies none.
	check(600, "", "", "")
	// A Codex process with no rollout identifies none; the walk must not
	// climb past it to the Claude session that launched it.
	check(800, "", "", "")
	env["CLAUDE_CODE_SESSION_ID"] = "claude-env"
	check(999, transcript.AgentClaude, "claude-env", "claude-env")
	check(800, "", "", "")
	check(600, "", "", "")
	env["FLOPWIRE_SESSION_ID"], env["FLOPWIRE_AGENT"] = "explicit", "devin"
	check(100, transcript.AgentDevin, "explicit", "env")
}

// TestDetectLive prints what the detector finds for the process running
// the test (run it from an agent's shell tool).
func TestDetectLive(t *testing.T) {
	if os.Getenv("FLOPWIRE_LIVE_CALLER") == "" {
		t.Skip("set FLOPWIRE_LIVE_CALLER=1")
	}
	d := NewDetector()
	d.Pid = os.Getpid()
	if p, err := strconv.Atoi(os.Getenv("FLOPWIRE_LIVE_PID")); err == nil {
		d.Pid = p
	}
	t0 := time.Now()
	c, ok := d.Detect(context.Background())
	t.Logf("caller %+v found=%v in %s", c, ok, time.Since(t0))
}
