package local

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/flopwire/flopwire/internal/transcript"
)

// Caller is the agent session a retrieval call runs inside. Its own rows
// (the grep call and the output it gets back are indexed within about a
// second) are left out of grep, search and sessions by default (decision
// D4); include_self turns that off.
type Caller struct {
	Agent     transcript.Agent
	SessionID string
	Rule      string // which rule identified it (see Detector)
}

// Detector identifies the calling session by exact evidence only
// (decision D4). The rules, first match wins:
//
//  1. env: FLOPWIRE_SESSION_ID (with optional FLOPWIRE_AGENT), for harnesses
//     that can pass it in their MCP server config.
//  2. claude-sessions: an ancestor process has a Claude Code session file,
//     <claude config dir>/sessions/<pid>.json; its sessionId is the live
//     session (it follows /clear, which the environment does not).
//  3. devin-lock: an ancestor process named devin has the pid exactly one
//     <devin dir>/session_locks/<session>.lock names. Devin sets no
//     variable for its children; its hooks' and MCP servers' parent is the
//     `devin acp` process, which holds the lock (probes 2026-10-01). Lock
//     files outlive their sessions and their pids are reused, so a lock
//     naming a process that is not devin does not count, and a devin pid
//     two locks name identifies none.
//  4. codex-env, codex-rollout: an ancestor process is named codex. Its
//     shell children carry CODEX_THREAD_ID, the thread id (codex-env).
//     Without it (an MCP server: Codex scrubs their environment), the
//     process must hold exactly one <codex home>/sessions/**/
//     rollout-*-<uuid>.jsonl open; the uuid is the thread id
//     (codex-rollout). A Codex process holding several (subagent threads,
//     an app server) or none (codex exec --ephemeral) identifies none
//     (C4), and the walk and the later rules stop there: an exact match on
//     the calling session is the only reason to exclude. An MCP server
//     under Codex gets its thread id per call from the request's _meta
//     instead (cmd/flopwire mcp).
//  5. claude-env, codex-env: CLAUDE_CODE_SESSION_ID, which Claude Code sets
//     for MCP servers and tool subprocesses (verified on 2.1.284), or
//     CODEX_THREAD_ID, when the walk found no harness (a process table
//     that cannot be read). With both set, the caller is unknown: either
//     may be inherited from a harness further up.
//
// There is no guess from the working directory or recent activity: a
// wrong guess hides someone's live session. The ancestor walk starts at
// the parent process and climbs at most eight levels, so wrappers (sh -c,
// npx, uv) in between are fine.
type Detector struct {
	Getenv    func(string) string
	Home      string
	Pid       int                                            // the process to start from; default os.Getppid()
	Proc      func(pid int) (ppid int, name string, ok bool) // default: the OS process table
	OpenFiles func(pid int) []string                         // default: lsof or /proc
	// For Live: whether a pid runs, and the files codex processes hold
	// open. Defaults: the OS.
	PidAlive   func(pid int) bool
	CodexFiles func() []string
}

// NewDetector returns a Detector over the real process and environment.
func NewDetector() *Detector {
	home, _ := os.UserHomeDir()
	return &Detector{Getenv: os.Getenv, Home: home, Pid: os.Getppid(), Proc: procInfo, OpenFiles: openFiles}
}

var rolloutRe = regexp.MustCompile(`rollout-.*-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`)

// Detect returns the calling session, if any rule finds one.
func (d *Detector) Detect(ctx context.Context) (Caller, bool) {
	env := d.Getenv
	if env == nil {
		env = os.Getenv
	}
	if s := env("FLOPWIRE_SESSION_ID"); s != "" {
		return Caller{Agent: transcript.Agent(env("FLOPWIRE_AGENT")), SessionID: s, Rule: "env"}, true
	}
	claudeDir := env("CLAUDE_CONFIG_DIR")
	if claudeDir == "" {
		claudeDir = filepath.Join(d.Home, ".claude")
	}
	codexHome := env("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(d.Home, ".codex")
	}
	devinDB := env("FLOPWIRE_DEVIN_DB")
	if devinDB == "" {
		devinDB = filepath.Join(d.Home, ".local", "share", "devin", "cli", "sessions.db")
	}
	var devin map[int][]string // pid -> sessions whose lock names it; read once
	pid := d.Pid
	for range 8 {
		if pid <= 1 || d.Proc == nil {
			break
		}
		if id := claudeSessionFile(filepath.Join(claudeDir, "sessions", itoa(pid)+".json")); id != "" {
			return Caller{Agent: transcript.AgentClaude, SessionID: id, Rule: "claude-sessions"}, true
		}
		ppid, name, ok := d.Proc(pid)
		if !ok {
			break
		}
		// A lock counts only when its pid is a devin process now: stale
		// locks name pids the OS has since given to other processes.
		if IsDevinProcess(name) {
			if devin == nil {
				devin = devinLocks(filepath.Join(filepath.Dir(devinDB), "session_locks"))
			}
			if ids := devin[pid]; len(ids) == 1 {
				return Caller{Agent: transcript.AgentDevin, SessionID: ids[0], Rule: "devin-lock"}, true
			} else if len(ids) > 1 {
				return Caller{}, false
			}
		}
		if strings.Contains(strings.ToLower(filepath.Base(name)), "codex") {
			if id := env("CODEX_THREAD_ID"); id != "" {
				return Caller{Agent: transcript.AgentCodex, SessionID: id, Rule: "codex-env"}, true
			}
			if d.OpenFiles == nil {
				return Caller{}, false
			}
			threads := map[string]bool{}
			for _, f := range d.OpenFiles(pid) {
				if !strings.HasPrefix(f, codexHome+string(filepath.Separator)) {
					continue
				}
				if m := rolloutRe.FindStringSubmatch(f); m != nil {
					threads[m[1]] = true
				}
			}
			if len(threads) == 1 {
				for id := range threads {
					return Caller{Agent: transcript.AgentCodex, SessionID: id, Rule: "codex-rollout"}, true
				}
			}
			// No rollout (codex exec --ephemeral) or several: the caller is
			// this Codex process and its thread is unknown. Identify none;
			// climbing further would find the session that launched Codex.
			return Caller{}, false
		}
		pid = ppid
	}
	cl, cx := env("CLAUDE_CODE_SESSION_ID"), env("CODEX_THREAD_ID")
	switch {
	case cl != "" && cx != "":
		return Caller{}, false
	case cl != "":
		return Caller{Agent: transcript.AgentClaude, SessionID: cl, Rule: "claude-env"}, true
	case cx != "":
		return Caller{Agent: transcript.AgentCodex, SessionID: cx, Rule: "codex-env"}, true
	}
	return Caller{}, false
}

// DevinHeldElsewhere reports whether the Devin session's lock (in
// session_locks beside devinDB) names a running devin process that is not
// d.Pid or one of its ancestors: another devin process holds the session.
// `devin -r ID` on a live session runs its SessionStart hooks before Devin
// refuses the session (probes 2026-10-01), so such a hook is not the
// session's own. When the process table cannot tell, it reports false.
func (d *Detector) DevinHeldElsewhere(devinDB, session string) bool {
	if d.Proc == nil || session == "" || strings.ContainsAny(session, `/\`) {
		return false
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(devinDB), "session_locks", session+".lock"))
	if err != nil {
		return false
	}
	holder, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || holder <= 1 {
		return false
	}
	if _, name, ok := d.Proc(holder); !ok || !IsDevinProcess(name) {
		return false // the session ended: a dead pid, or one the OS reused
	}
	pid := d.Pid
	for range 8 {
		if pid <= 1 {
			break
		}
		if pid == holder {
			return false
		}
		ppid, _, ok := d.Proc(pid)
		if !ok {
			return false
		}
		pid = ppid
	}
	return true
}

// devinLocks maps each pid a Devin session lock names to the sessions
// naming it. The files are only read.
func devinLocks(dir string) map[int][]string {
	out := map[int][]string{}
	locks, _ := filepath.Glob(filepath.Join(dir, "*.lock"))
	for _, f := range locks {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 {
			out[pid] = append(out[pid], strings.TrimSuffix(filepath.Base(f), ".lock"))
		}
	}
	return out
}

func claudeSessionFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var v struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(b, &v) != nil {
		return ""
	}
	return v.SessionID
}

func itoa(n int) string { return id(int64(n)) }
