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

// Codex shell children carry CODEX_THREAD_ID; Devin's children are found
// by the lock file naming their ancestor's pid. All fixtures synthetic.
func TestDetectorCodexEnvAndDevinLock(t *testing.T) {
	home := t.TempDir()
	locks := filepath.Join(home, ".local", "share", "devin", "cli", "session_locks")
	os.MkdirAll(locks, 0o755)
	os.WriteFile(filepath.Join(locks, "devin-sess-1.lock"), []byte("700\n"), 0o644)
	// A stale lock and a live one naming the same pid: unknown.
	os.WriteFile(filepath.Join(locks, "devin-old-a.lock"), []byte("900"), 0o644)
	os.WriteFile(filepath.Join(locks, "devin-old-b.lock"), []byte("900"), 0o644)
	// 100 (sh) -> 200 (codex); 600 (sh) -> 650 (node) -> 700 (devin acp);
	// 800 (sh) -> 900 (devin acp, two locks); 1000 (sh) -> 1100 (claude, no session file)
	procs := map[int]struct {
		ppid int
		name string
	}{100: {200, "sh"}, 200: {1, "codex"}, 600: {650, "sh"}, 650: {700, "node"}, 700: {1, "devin"}, 800: {900, "sh"}, 900: {1, "devin"},
		1000: {1100, "sh"}, 1100: {1, "claude"}}
	proc := func(pid int) (int, string, bool) { p, ok := procs[pid]; return p.ppid, p.name, ok }
	env := map[string]string{}
	d := &Detector{Getenv: func(k string) string { return env[k] }, Home: home, Proc: proc, OpenFiles: func(int) []string { return nil }}
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
	// A Codex process without a rollout and no variable: none.
	check(100, "", "", "")
	env["CODEX_THREAD_ID"] = "019a0000-0000-7000-8000-0000000000cd"
	check(100, transcript.AgentCodex, "019a0000-0000-7000-8000-0000000000cd", "codex-env")
	check(600, transcript.AgentDevin, "devin-sess-1", "devin-lock")
	check(800, "", "", "")
	// No harness in the walk: the variable alone identifies Codex, but not
	// when Claude's is set as well.
	d.Proc = nil
	check(1000, transcript.AgentCodex, "019a0000-0000-7000-8000-0000000000cd", "codex-env")
	env["CLAUDE_CODE_SESSION_ID"] = "claude-env"
	check(1000, "", "", "")
	// The explicit override still wins.
	env["FLOPWIRE_SESSION_ID"] = "explicit"
	check(1000, "", "explicit", "env")
}

// Devin lock files outlive their sessions, so the pid a stale lock names
// is reused by unrelated processes. A lock counts only when the process at
// its pid is a devin process: a reused pid must not make a Claude Code
// shell, or a plain terminal, send as that old Devin session, nor (two
// stale locks naming one reused pid) hide the real caller.
func TestDetectorStaleDevinLockOnReusedPid(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude", "sessions"), 0o755)
	os.WriteFile(filepath.Join(home, ".claude", "sessions", "300.json"), []byte(`{"pid":300,"sessionId":"claude-live"}`), 0o644)
	locks := filepath.Join(home, ".local", "share", "devin", "cli", "session_locks")
	os.MkdirAll(locks, 0o755)
	os.WriteFile(filepath.Join(locks, "old-devin-a.lock"), []byte("100"), 0o644) // reused by a Claude Code shell
	os.WriteFile(filepath.Join(locks, "old-devin-b.lock"), []byte("500"), 0o644) // reused by a terminal's zsh
	os.WriteFile(filepath.Join(locks, "old-devin-c.lock"), []byte("200"), 0o644) // two stale locks on one reused pid
	os.WriteFile(filepath.Join(locks, "old-devin-d.lock"), []byte("200"), 0o644)
	// 100 (sh) -> 300 (claude); 400 (flopwire's parent sh) -> 500 (zsh) -> 600 (tmux);
	// 150 (sh) -> 200 (bash) -> 300 (claude)
	procs := map[int]struct {
		ppid int
		name string
	}{100: {300, "sh"}, 300: {1, "claude"}, 400: {500, "sh"}, 500: {600, "zsh"}, 600: {1, "tmux"}, 150: {200, "sh"}, 200: {300, "bash"}}
	proc := func(pid int) (int, string, bool) { p, ok := procs[pid]; return p.ppid, p.name, ok }
	d := &Detector{Getenv: func(string) string { return "" }, Home: home, Proc: proc}
	for _, tc := range []struct {
		pid     int
		session string
	}{{100, "claude-live"}, {400, ""}, {150, "claude-live"}} {
		d.Pid = tc.pid
		c, ok := d.Detect(context.Background())
		if c.SessionID != tc.session || ok != (tc.session != "") {
			t.Errorf("pid %d: got %+v %v, want %q", tc.pid, c, ok, tc.session)
		}
	}
}

// A Devin session is held elsewhere when its lock names a running devin
// that is not an ancestor of the asking process: the hooks of a `devin -r`
// Devin is about to refuse.
func TestDevinHeldElsewhere(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "sessions.db")
	os.MkdirAll(filepath.Join(dir, "session_locks"), 0o755)
	lock := func(session, pid string) {
		os.WriteFile(filepath.Join(dir, "session_locks", session+".lock"), []byte(pid+"\n"), 0o644)
	}
	lock("live", "200")  // held by devin 200
	lock("ended", "999") // dead pid
	lock("reused", "500")
	// 100 (sh, the hook's parent) -> 200 (devin, the holder); 300 (sh) ->
	// 400 (devin -r, refused); 500 (zsh, a reused pid).
	procs := map[int]struct {
		ppid int
		name string
	}{100: {200, "sh"}, 200: {1, "devin"}, 300: {400, "sh"}, 400: {1, "devin"}, 500: {1, "zsh"}}
	proc := func(pid int) (int, string, bool) { p, ok := procs[pid]; return p.ppid, p.name, ok }
	d := &Detector{Proc: proc}
	for _, c := range []struct {
		pid     int
		session string
		want    bool
	}{
		{100, "live", false}, // the holder's own hook
		{300, "live", true},  // a refused resume
		{300, "ended", false},
		{300, "reused", false},
		{300, "no-lock", false},
		{300, "../live", false},
	} {
		d.Pid = c.pid
		if got := d.DevinHeldElsewhere(db, c.session); got != c.want {
			t.Errorf("pid %d session %q: %v, want %v", c.pid, c.session, got, c.want)
		}
	}
	d.Proc = nil // no process table: cannot tell
	if d.DevinHeldElsewhere(db, "live") {
		t.Error("held elsewhere without a process table")
	}
}

// A Claude session is held elsewhere when a session file names it and a
// running process that is not the asking process or one of its ancestors:
// the SessionEnd of a `claude -p -r ID` while another process runs ID.
func TestClaudeHeldElsewhere(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sessions"), 0o755)
	file := func(pid int, session string) {
		os.WriteFile(filepath.Join(dir, "sessions", itoa(pid)+".json"), []byte(`{"pid":`+itoa(pid)+`,"sessionId":"`+session+`"}`), 0o644)
	}
	file(200, "live")  // claude 200 runs live
	file(999, "ended") // a dead pid
	// 100 (sh, the hook's parent) -> 200 (claude, the holder); 300 (sh) ->
	// 400 (claude -p -r live, no session file).
	procs := map[int]struct {
		ppid int
		name string
	}{100: {200, "sh"}, 200: {1, "2.1.288"}, 300: {400, "sh"}, 400: {1, "2.1.288"}}
	proc := func(pid int) (int, string, bool) { p, ok := procs[pid]; return p.ppid, p.name, ok }
	d := &Detector{Proc: proc}
	for _, c := range []struct {
		pid     int
		session string
		want    bool
	}{
		{100, "live", false}, // the holder's own hook
		{300, "live", true},  // the second process's hook
		{300, "ended", false},
		{300, "no-file", false},
	} {
		d.Pid = c.pid
		if got := d.ClaudeHeldElsewhere(dir, c.session); got != c.want {
			t.Errorf("pid %d session %q: %v, want %v", c.pid, c.session, got, c.want)
		}
	}
	d.Proc = nil
	if d.ClaudeHeldElsewhere(dir, "live") {
		t.Error("held elsewhere without a process table")
	}
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

// UnderCodex: the nearest harness above the process is Codex. Claude Code
// (its session file) or Devin first, no harness, or an unreadable process
// table is not.
func TestDetectorUnderCodex(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude", "sessions"), 0o755)
	os.WriteFile(filepath.Join(home, ".claude", "sessions", "300.json"), []byte(`{"pid":300,"sessionId":"claude-live"}`), 0o644)
	procs := map[int]struct {
		ppid int
		name string
	}{
		100: {500, "codex"},                                     // Codex launched the server directly
		110: {111, "sh"}, 111: {112, "npx"}, 112: {500, "node"}, // through wrappers
		500: {300, "/opt/homebrew/bin/codex"},  // a Codex run from a Claude Code shell is still Codex
		200: {300, "node"}, 300: {1, "claude"}, // Claude Code
		400: {401, "sh"}, 401: {1, "devin"}, // Devin
		600: {601, "sh"}, 601: {1, "launchd"}, // no harness
		700: {701, "sh"}, // 701 is not in the table
	}
	proc := func(pid int) (int, string, bool) { p, ok := procs[pid]; return p.ppid, p.name, ok }
	d := &Detector{Getenv: func(string) string { return "" }, Home: home, Proc: proc}
	for pid, want := range map[int]bool{100: true, 110: true, 500: true, 200: false, 300: false, 400: false, 600: false, 700: false, 0: false} {
		d.Pid = pid
		if got := d.UnderCodex(); got != want {
			t.Errorf("pid %d: under Codex %v, want %v", pid, got, want)
		}
	}
	d.Proc, d.Pid = nil, 100
	if d.UnderCodex() {
		t.Error("no process table: under Codex")
	}
}
