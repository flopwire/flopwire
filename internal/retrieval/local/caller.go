package local

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
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
//  3. codex-rollout: an ancestor process is named codex and holds exactly
//     one <codex home>/sessions/**/rollout-*-<uuid>.jsonl open; the uuid is
//     the thread id. A Codex process holding several (subagent threads, an
//     app server) or none (codex exec --ephemeral) identifies none (C4),
//     and the walk and the later rules stop there: an exact match on the
//     calling session is the only reason to exclude. Codex starts MCP
//     servers with a scrubbed environment, so there is no variable to read.
//  4. claude-env: CLAUDE_CODE_SESSION_ID, which Claude Code sets for MCP
//     servers and tool subprocesses (verified on 2.1.284).
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
		if strings.Contains(strings.ToLower(filepath.Base(name)), "codex") && d.OpenFiles != nil {
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
	if s := env("CLAUDE_CODE_SESSION_ID"); s != "" {
		return Caller{Agent: transcript.AgentClaude, SessionID: s, Rule: "claude-env"}, true
	}
	return Caller{}, false
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
