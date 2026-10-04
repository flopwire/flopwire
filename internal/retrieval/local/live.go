package local

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/flopwire/flopwire/internal/retrieval/format"
)

// LiveCap bounds how long a harness holding a session open makes it live
// without the session writing: a Claude session file, a Devin lock or an
// open Codex rollout left idle this long no longer counts.
const LiveCap = time.Hour

// Live maps the sessions the harnesses on this machine hold open to their
// transcript's last write (zero when unknown). The evidence, exact only:
//
//   - Claude: <claude config dir>/sessions/<pid>.json of a running pid
//     (the same files the caller detector reads); the last write is the
//     later of the transcript's mtime and the file's updatedAt.
//   - Devin: session_locks/<session>.lock beside sessions.db, holding the
//     pid of a running process named devin (lock files outlive their
//     sessions, and the OS reuses their pids).
//   - Codex, with codex set: a rollout a process named codex holds open
//     (one lsof call); its mtime is the last write.
//
// Sessions whose last write is known and older than LiveCap are left out.
func (d *Detector) Live(codex bool) map[string]time.Time {
	env := d.Getenv
	if env == nil {
		env = os.Getenv
	}
	now := time.Now
	alive := d.PidAlive
	if alive == nil {
		alive = pidAlive
	}
	out := map[string]time.Time{}
	add := func(id string, at time.Time) {
		if id == "" || !at.IsZero() && now().Sub(at) > LiveCap {
			return
		}
		if prev, ok := out[id]; !ok || at.After(prev) {
			out[id] = at
		}
	}
	claudeDir := env("CLAUDE_CONFIG_DIR")
	if claudeDir == "" {
		claudeDir = filepath.Join(d.Home, ".claude")
	}
	files, _ := filepath.Glob(filepath.Join(claudeDir, "sessions", "*.json"))
	for _, f := range files {
		pid, err := strconv.Atoi(strings.TrimSuffix(filepath.Base(f), ".json"))
		if err != nil || !alive(pid) {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var v struct {
			SessionID string `json:"sessionId"`
			UpdatedAt int64  `json:"updatedAt"`
		}
		if json.Unmarshal(b, &v) != nil || v.SessionID == "" {
			continue
		}
		at := time.UnixMilli(v.UpdatedAt)
		if v.UpdatedAt == 0 {
			at = time.Time{}
		}
		if ts, _ := filepath.Glob(filepath.Join(claudeDir, "projects", "*", v.SessionID+".jsonl")); len(ts) > 0 {
			if st, err := os.Stat(ts[0]); err == nil && st.ModTime().After(at) {
				at = st.ModTime()
			}
		}
		if at.IsZero() {
			at = time.Unix(0, 1) // unknown: treat as old
		}
		add(v.SessionID, at)
	}
	devinDB := env("FLOPWIRE_DEVIN_DB")
	if devinDB == "" {
		devinDB = filepath.Join(d.Home, ".local", "share", "devin", "cli", "sessions.db")
	}
	locks, _ := filepath.Glob(filepath.Join(filepath.Dir(devinDB), "session_locks", "*.lock"))
	for _, f := range locks {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 && alive(pid) && d.isDevin(pid) {
			add(strings.TrimSuffix(filepath.Base(f), ".lock"), time.Time{})
		}
	}
	if codex {
		codexHome := env("CODEX_HOME")
		if codexHome == "" {
			codexHome = filepath.Join(d.Home, ".codex")
		}
		list := d.CodexFiles
		if list == nil {
			list = codexOpenFiles
		}
		for _, f := range list() {
			if !strings.HasPrefix(f, codexHome+string(filepath.Separator)) {
				continue
			}
			if m := rolloutRe.FindStringSubmatch(f); m != nil {
				if st, err := os.Stat(f); err == nil {
					add(m[1], st.ModTime())
				}
			}
		}
	}
	return out
}

// isDevin reports whether pid runs a program named devin. Without a process
// table to ask (d.Proc nil), it takes the pid alone.
func (d *Detector) isDevin(pid int) bool {
	if d.Proc == nil {
		return true
	}
	_, name, ok := d.Proc(pid)
	return ok && IsDevinProcess(name)
}

// IsOpencodeProcess reports whether a process name is opencode's (the
// opencode binary, or a launcher whose name holds it).
func IsOpencodeProcess(name string) bool {
	return strings.Contains(strings.ToLower(filepath.Base(name)), "opencode")
}

// IsDevinProcess reports whether a process name is Devin CLI's (devin, or
// a path ending in it).
func IsDevinProcess(name string) bool {
	return strings.Contains(strings.ToLower(filepath.Base(name)), "devin")
}

// ProcName is the name of the program pid runs, or "" when the process
// table does not have it.
func ProcName(pid int) string {
	_, name, ok := procInfo(pid)
	if !ok {
		return ""
	}
	return name
}

// MarkLive sets Live on a conversation: active within format.LiveWindow of
// now, or held open by its harness (live, from Detector.Live) and active
// within LiveCap.
func MarkLive(c *format.ConversationInfo, live map[string]time.Time, now time.Time) {
	if c.Live { // the server knew already
		return
	}
	last := c.LastActivityAt
	if last != nil && now.Sub(*last) < format.LiveWindow {
		c.Live = true
		return
	}
	at, ok := live[c.SessionID]
	if !ok {
		return
	}
	if at.IsZero() && last != nil {
		at = *last
	}
	c.Live = !at.IsZero() && now.Sub(at) <= LiveCap
}

// LiveReporter returns the sessions to report as live with each sync
// flush (sorted, at most max), detected at most every ttl.
func LiveReporter(d *Detector, ttl time.Duration, max int) func() []string {
	var mu sync.Mutex
	var at time.Time
	var ids []string
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		if at.IsZero() || time.Since(at) > ttl {
			ids = ids[:0:0]
			for id := range d.Live(true) {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			if len(ids) > max {
				ids = ids[:max]
			}
			at = time.Now()
		}
		return append([]string{}, ids...)
	}
}
