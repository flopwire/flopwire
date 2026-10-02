package agent

// Presence for the message bus (notes/message-bus/plan.md §4): the device's
// live sessions with their harness, repo, branch, and busy or idle.
//
// Live is what `flopwire sessions` prints as live (local.MarkLive): written
// within format.LiveWindow, or held open by its harness and written within
// local.LiveCap. The evidence that a harness holds a session open is read
// here, read-only:
//
//   - Claude Code: <claude config dir>/sessions/<pid>.json whose pid runs
//     and started when procStart says (a reused pid does not count). Its
//     status "busy" means a turn is running.
//   - Codex: thread-writer-locks/<thread>.lock exists. The file is only
//     looked at, never locked: a try-lock can break a writer that is
//     starting. A lock left by a killed Codex stays until LiveCap passes
//     without a write. Busy or idle is the rollout's last task event:
//     task_started (busy), task_complete or turn_aborted (idle).
//   - Devin: session_locks/<session>.lock names a running process named
//     devin. Lock files outlive their sessions, so a Devin session is live
//     only on that evidence, however recently it wrote. Busy or idle is the
//     session's last hook event (hookTurns): Devin's store has no
//     read-only signal that a turn runs.
//
// A session the path rules keep off the server is marked Withheld: the
// poll does not report it (devicebus).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/flopwire/flopwire/internal/devicebus"
	"github.com/flopwire/flopwire/internal/fsprobe"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/local"
	"github.com/flopwire/flopwire/internal/transcript"
)

// harnessLive is what the harness registries say: sessions held open, with
// the last write the registry knows (zero: unknown, MarkLive then uses the
// transcript's), and the Claude sessions running a turn.
type harnessLive struct {
	at   map[string]time.Time
	busy map[string]bool
}

// procStartLayout is Claude Code's procStart: ps's lstart in UTC.
const procStartLayout = time.ANSIC

// registries reads the harness registries.
func (a *Agent) registries() harnessLive {
	h := harnessLive{at: map[string]time.Time{}, busy: map[string]bool{}}
	add := func(id string, at time.Time) {
		if prev, ok := h.at[id]; !ok || at.After(prev) {
			h.at[id] = at
		}
	}
	files, _ := fsprobe.Glob(filepath.Join(filepath.Dir(a.cfg.ClaudeProjects), "sessions", "*.json"))
	for _, f := range files {
		pid, err := strconv.Atoi(strings.TrimSuffix(filepath.Base(f), ".json"))
		if err != nil || pid <= 1 || !a.pidAlive(pid) {
			continue
		}
		b, err := fsprobe.ReadFile(f)
		if err != nil {
			continue
		}
		var v struct {
			SessionID string `json:"sessionId"`
			Status    string `json:"status"`
			ProcStart string `json:"procStart"`
			UpdatedAt int64  `json:"updatedAt"`
		}
		if json.Unmarshal(b, &v) != nil || v.SessionID == "" || !a.sameProcess(pid, v.ProcStart) {
			continue
		}
		var at time.Time
		if v.UpdatedAt > 0 {
			at = time.UnixMilli(v.UpdatedAt)
		}
		add(v.SessionID, at)
		h.busy[v.SessionID] = h.busy[v.SessionID] || v.Status == "busy"
	}
	locks, _ := fsprobe.Glob(filepath.Join(a.cfg.CodexHome, "thread-writer-locks", "*.lock"))
	for _, f := range locks {
		add(strings.TrimSuffix(filepath.Base(f), ".lock"), time.Time{})
	}
	if a.devin.path != "" {
		locks, _ := fsprobe.Glob(filepath.Join(filepath.Dir(a.devin.path), "session_locks", "*.lock"))
		for _, f := range locks {
			b, err := fsprobe.ReadFile(f)
			if err != nil {
				continue
			}
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 && a.pidAlive(pid) && local.IsDevinProcess(a.procName(pid)) {
				add(strings.TrimSuffix(filepath.Base(f), ".lock"), time.Time{})
			}
		}
	}
	return h
}

// sameProcess reports whether pid is the process a Claude session file
// describes: its start time matches procStart (within 2s, read as UTC or
// local time). A file without procStart, or a platform that cannot tell,
// takes the pid alone.
func (a *Agent) sameProcess(pid int, procStart string) bool {
	if procStart == "" {
		return true
	}
	started, ok := a.procStart(pid)
	if !ok {
		return true
	}
	for _, loc := range []*time.Location{time.UTC, time.Local} {
		if t, err := time.ParseInLocation(procStartLayout, procStart, loc); err == nil {
			if d := started.Sub(t); d > -2*time.Second && d < 2*time.Second {
				return true
			}
		}
	}
	return false
}

// rolloutState caches codexBusy by the file's size and change time:
// presence is checked every two seconds and most rollouts did not move.
type rolloutState struct {
	mu sync.Mutex
	m  map[string]rolloutBusy
}

type rolloutBusy struct {
	size int64
	mod  time.Time
	busy bool
}

// busy is codexBusy for path, read again only when the file changed. keep
// names the paths still live; the rest are forgotten.
func (r *rolloutState) busy(path string, keep map[string]bool) bool {
	fi, err := fsprobe.Stat(path)
	if err != nil {
		return false
	}
	r.mu.Lock()
	for p := range r.m {
		if !keep[p] {
			delete(r.m, p)
		}
	}
	c, ok := r.m[path]
	r.mu.Unlock()
	if ok && c.size == fi.Size() && c.mod.Equal(fi.ModTime()) {
		return c.busy
	}
	b := codexBusy(path)
	r.mu.Lock()
	if r.m == nil {
		r.m = map[string]rolloutBusy{}
	}
	r.m[path] = rolloutBusy{size: fi.Size(), mod: fi.ModTime(), busy: b}
	r.mu.Unlock()
	return b
}

// codexBusy reads the end of a rollout for its last task event: a turn is
// running after task_started until task_complete or turn_aborted.
func codexBusy(path string) bool {
	const tail = 256 << 10
	f, err := fsprobe.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	off := max(fi.Size()-tail, 0)
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return false
	}
	lines := bytes.Split(buf, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		l := lines[i]
		if !bytes.Contains(l, []byte(`"event_msg"`)) {
			continue
		}
		var rec struct {
			Type    string `json:"type"`
			Payload struct {
				Type string `json:"type"`
			} `json:"payload"`
		}
		if json.Unmarshal(l, &rec) != nil || rec.Type != "event_msg" {
			continue
		}
		switch rec.Payload.Type {
		case "task_started":
			return true
		case "task_complete", "turn_aborted":
			return false
		}
	}
	return false
}

// presenceSQL is every top-level session written since $1 (unix ms).
const presenceSQL = `SELECT agent, session_id, COALESCE(repo_root, cwd, ''), COALESCE(branches, ''), COALESCE(title, ''), last_activity_at
	FROM conversations WHERE depth = 0 AND deleted_in_generation IS NULL AND last_activity_at >= ?`

// knownSQL is the top-level sessions whose id is in [$1, $2): a prefix.
const knownSQL = `SELECT agent, session_id, COALESCE(repo_root, cwd, ''), COALESCE(branches, ''), COALESCE(title, ''), COALESCE(last_activity_at, 0)
	FROM conversations WHERE depth = 0 AND deleted_in_generation IS NULL AND session_id >= ? AND session_id < ? ORDER BY session_id LIMIT 50`

func (a *Agent) sessionRows(ctx context.Context, q string, args ...any) ([]devicebus.Session, error) {
	rows, err := a.store.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []devicebus.Session
	for rows.Next() {
		var s devicebus.Session
		var branches string
		var last int64
		if err := rows.Scan(&s.Agent, &s.SessionID, &s.Repo, &branches, &s.Title, &last); err != nil {
			return nil, err
		}
		var bs []string
		if json.Unmarshal([]byte(branches), &bs) == nil && len(bs) > 0 {
			s.Branch = bs[len(bs)-1]
		}
		if last > 0 {
			s.LastActive = time.UnixMilli(last)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// BusPresence lists the device's live sessions (see the file comment).
func (a *Agent) BusPresence(ctx context.Context) ([]devicebus.Session, error) {
	now := a.now()
	all, err := a.sessionRows(ctx, presenceSQL, now.Add(-local.LiveCap).UnixMilli())
	if err != nil {
		return nil, err
	}
	reg := a.registries()
	paths := a.transcriptsBySession()
	var out []devicebus.Session
	rollouts := map[string]bool{}
	for _, s := range all {
		if transcript.Agent(s.Agent) == transcript.AgentDevin {
			if _, held := reg.at[s.SessionID]; !held {
				continue // its lock is gone or names no running devin: ended
			}
		}
		last := s.LastActive
		info := format.ConversationInfo{SessionID: s.SessionID, LastActivityAt: &last}
		local.MarkLive(&info, reg.at, now)
		if !info.Live {
			continue
		}
		key := placeKey{transcript.Agent(s.Agent), s.SessionID}
		if t := paths[key]; t != nil && key.agent == transcript.AgentCodex {
			rollouts[t.path] = true
		}
		s.Withheld = !a.reportable(ctx, key, paths[key])
		out = append(out, s)
	}
	for i, s := range out {
		key := placeKey{transcript.Agent(s.Agent), s.SessionID}
		switch key.agent {
		case transcript.AgentClaude:
			out[i].Busy = reg.busy[s.SessionID]
		case transcript.AgentCodex:
			if t := paths[key]; t != nil {
				out[i].Busy = a.rollouts.busy(t.path, rollouts)
			}
		case transcript.AgentDevin:
			out[i].Busy = a.hookBusy(s.SessionID)
		}
	}
	return out, nil
}

// BusKnown lists the device's top-level sessions whose id starts with
// prefix, live or not: what a send without a server may address, and what
// the bus checks the path rules against for a session that is not live.
// Withheld is set as BusPresence sets it.
func (a *Agent) BusKnown(ctx context.Context, prefix string) ([]devicebus.Session, error) {
	out, err := a.sessionRows(ctx, knownSQL, prefix, prefix+"\U0010FFFF")
	if err != nil || len(out) == 0 {
		return out, err
	}
	paths := a.transcriptsBySession()
	for i, s := range out {
		key := placeKey{transcript.Agent(s.Agent), s.SessionID}
		out[i].Withheld = !a.reportable(ctx, key, paths[key])
	}
	return out, nil
}

// transcriptsBySession maps each session to its main transcript.
func (a *Agent) transcriptsBySession() map[placeKey]*target {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[placeKey]*target{}
	for _, t := range a.targets {
		if t.kind == kindTranscript && t.root == "" && t.src.SessionKey != "" {
			out[placeKey{t.src.Agent, t.src.SessionKey}] = t
		}
	}
	return out
}

// reportable reports whether the path rules let the session reach the
// server. A session not placed yet is not reported.
func (a *Agent) reportable(ctx context.Context, key placeKey, t *target) bool {
	if key.agent == transcript.AgentDevin {
		return a.devin.path != "" && a.devinMode(ctx, key.session) == pathpolicy.Allow
	}
	return t != nil && a.uploadable(t)
}
