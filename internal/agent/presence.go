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
//   - Codex: a running Codex holds the flock on
//     thread-writer-locks/<thread>.lock (codexWriter probes it as Codex's
//     own cleanup does, under its coordination lock). The file outlives
//     the writer; a released lock is not held. When the probe cannot tell,
//     the file's existence counts, as before. Busy or idle is the
//     rollout's last task event: task_started (busy), task_complete or
//     turn_aborted (idle).
//   - Devin: session_locks/<session>.lock names a running process named
//     devin. Lock files outlive their sessions, so a Devin session is live
//     only on that evidence, however recently it wrote. Busy or idle is the
//     session's last hook event (hookTurns): Devin's store has no
//     read-only signal that a turn runs.
//
// A session that ended is not live (devicebus.Observe, End): its
// registry entry names a process that is gone (a Claude session file of a
// dead or reused pid, a released Codex writer lock, a Devin lock of a dead
// or non-devin pid), an entry it had is missing for a second, or its
// SessionEnd hook ran. Idleness never ends a session.
//
// A session the path rules keep off the server is marked Withheld: the
// poll does not report it (devicebus).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/flopwire/flopwire/internal/devicebus"
	"github.com/flopwire/flopwire/internal/fsprobe"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/redact"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/local"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/flopwire/flopwire/internal/transcript/codex"
)

// harnessLive is what the harness registries say: sessions held open, with
// the last write the registry knows (zero: unknown, MarkLive then uses the
// transcript's), the Claude sessions running a turn, and the evidence of
// which sessions ended (reg, for devicebus.Observe).
type harnessLive struct {
	at   map[string]time.Time
	busy map[string]bool
	reg  devicebus.Registry
}

// procStartLayout is Claude Code's procStart: ps's lstart in UTC.
const procStartLayout = time.ANSIC

// registries reads the harness registries.
func (a *Agent) registries() harnessLive {
	h := harnessLive{at: map[string]time.Time{}, busy: map[string]bool{},
		reg: devicebus.Registry{Held: map[devicebus.Ref]devicebus.Holder{}, Read: map[string]bool{}}}
	add := func(id string, at time.Time) {
		if prev, ok := h.at[id]; !ok || at.After(prev) {
			h.at[id] = at
		}
	}
	dirRead := func(dir string) bool {
		fi, err := fsprobe.Stat(dir)
		return err == nil && fi.IsDir()
	}
	claude := string(transcript.AgentClaude)
	sessions := filepath.Join(filepath.Dir(a.cfg.ClaudeProjects), "sessions")
	h.reg.Read[claude] = dirRead(sessions)
	files, _ := fsprobe.Glob(filepath.Join(sessions, "*.json"))
	for _, f := range files {
		pid, err := strconv.Atoi(strings.TrimSuffix(filepath.Base(f), ".json"))
		if err != nil || pid <= 1 {
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
		if json.Unmarshal(b, &v) != nil || v.SessionID == "" {
			continue
		}
		ref := devicebus.Ref{Agent: claude, Session: v.SessionID}
		if !a.pidAlive(pid) || !a.sameProcess(pid, v.ProcStart) {
			// The file of a process that is gone (killed: a clean exit
			// removes it), or of a pid now reused.
			h.reg.Gone = append(h.reg.Gone, ref)
			continue
		}
		var at time.Time
		if v.UpdatedAt > 0 {
			at = time.UnixMilli(v.UpdatedAt)
		}
		add(v.SessionID, at)
		h.busy[v.SessionID] = h.busy[v.SessionID] || v.Status == "busy"
		started, _ := a.procStart(pid)
		h.reg.Held[ref] = devicebus.Holder{ID: fmt.Sprintf("pid:%d:%s", pid, v.ProcStart), Start: started}
	}
	codex := string(transcript.AgentCodex)
	writers := filepath.Join(a.cfg.CodexHome, "thread-writer-locks")
	h.reg.Read[codex] = dirRead(writers)
	locks, _ := fsprobe.Glob(filepath.Join(writers, "*.lock"))
	for _, f := range locks {
		id := strings.TrimSuffix(filepath.Base(f), ".lock")
		if id == "" || strings.HasPrefix(id, ".") {
			continue // .coordination.lock
		}
		ref := devicebus.Ref{Agent: codex, Session: id}
		switch a.codexWriter(f) {
		case codexHeld:
			add(id, time.Time{})
			h.reg.Held[ref] = devicebus.Holder{ID: "lock"}
		case codexReleased:
			h.reg.Gone = append(h.reg.Gone, ref)
		default:
			// Unknown this time: live as the file's existence says, and
			// neither held nor ended for Observe.
			add(id, time.Time{})
			h.reg.Unknown = append(h.reg.Unknown, ref)
		}
	}
	if a.devin.path != "" {
		devin := string(transcript.AgentDevin)
		dir := filepath.Join(filepath.Dir(a.devin.path), "session_locks")
		h.reg.Read[devin] = dirRead(dir)
		locks, _ := fsprobe.Glob(filepath.Join(dir, "*.lock"))
		for _, f := range locks {
			id := strings.TrimSuffix(filepath.Base(f), ".lock")
			ref := devicebus.Ref{Agent: devin, Session: id}
			b, err := fsprobe.ReadFile(f)
			if err != nil {
				h.reg.Unknown = append(h.reg.Unknown, ref)
				continue
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			switch {
			case err != nil || pid <= 1:
				h.reg.Unknown = append(h.reg.Unknown, ref)
			case a.pidAlive(pid) && local.IsDevinProcess(a.procName(pid)):
				add(id, time.Time{})
				started, _ := a.procStart(pid)
				h.reg.Held[ref] = devicebus.Holder{ID: fmt.Sprintf("pid:%d", pid), Start: started}
			default:
				h.reg.Gone = append(h.reg.Gone, ref) // a dead pid, or reused by another program
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
	// The bus records what the registries say and answers which sessions
	// ended (a dead or missing entry, a SessionEnd hook); they are not
	// live, however recently they wrote. Without an answer none is left
	// out: presence must not empty on a read error.
	var ended map[devicebus.Ref]bool
	if a.cfg.Bus != nil {
		if ended, err = a.cfg.Bus.Observe(ctx, reg.reg); err != nil {
			a.log.Warn("agent: ended sessions", "err", err)
		}
	}
	paths := a.transcriptsBySession()
	var out []devicebus.Session
	rollouts := map[string]bool{}
	for _, s := range all {
		if ended[devicebus.Ref{Agent: s.Agent, Session: s.SessionID}] {
			continue
		}
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
		if !s.Withheld {
			s.Title = a.busTitle(ctx, s)
		}
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

// titleCut is where the parsers cut a title taken from a message's first
// line (claude.TitleRunes, codex.TitleRunes).
const titleCut = min(claude.TitleRunes, codex.TitleRunes)

// titleLeading is how many of a session's first messages busTitle looks
// through for the one its title was cut from: Codex skips injected
// context before it.
const titleLeading = 20

type busTitleKey struct{ agent, session, title string }

// busTitle is a session's title as presence reports it to the server.
// The parsers cut a title from the first prompt's first line at titleCut
// runes, and a cut can split a secret so the redactor no longer matches
// what is left of it (issue #71). So a title that may be a cut is rebuilt
// from the line it was cut from: the whole line redacted, then cut. When
// no such line is found (the index does not hold it yet, a local
// redaction hid it, or the title came from elsewhere), the title is
// redacted and its last word, which may be the start of a secret, is
// dropped. A shorter title is no cut and is returned as it is; the bus
// redacts every title it reports (devicebus serverPresence).
func (a *Agent) busTitle(ctx context.Context, s devicebus.Session) string {
	if utf8.RuneCountInString(s.Title) < titleCut {
		return s.Title
	}
	key := busTitleKey{s.Agent, s.SessionID, s.Title}
	a.mu.Lock()
	t, ok := a.busTitles[key]
	a.mu.Unlock()
	if ok {
		return t
	}
	t = redactedCut(s.Title)
	rows, err := a.store.LeadingMessages(ctx, s.SessionID, s.Agent, titleLeading)
	if err != nil {
		return t
	}
	for _, r := range rows {
		line := strings.TrimSpace(r.Text)
		if i := strings.IndexByte(line, '\n'); i >= 0 {
			line = line[:i]
		}
		if cutTitle(line) == strings.TrimSpace(s.Title) {
			masked, _ := redact.Redact([]byte(line))
			t = cutTitle(string(masked))
			break
		}
	}
	a.mu.Lock()
	if a.busTitles == nil || len(a.busTitles) >= 1024 {
		a.busTitles = map[busTitleKey]string{}
	}
	a.busTitles[key] = t
	a.mu.Unlock()
	return t
}

// cutTitle cuts a first line as the parsers do.
func cutTitle(line string) string {
	if r := []rune(line); len(r) > titleCut {
		line = string(r[:titleCut])
	}
	return strings.TrimSpace(line)
}

// redactedCut is a cut title whose line is not at hand: redacted, less its
// last word.
func redactedCut(title string) string {
	masked, _ := redact.Redact([]byte(title))
	t := strings.TrimRightFunc(string(masked), unicode.IsSpace)
	i := strings.LastIndexFunc(t, unicode.IsSpace)
	if i < 0 {
		return ""
	}
	return strings.TrimRightFunc(t[:i], unicode.IsSpace) + " …"
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

// BusWithheld names a session the path rules keep off the server that ref
// would tell the server about, "" when there is none (issue #71). ref is
// a send's archive address (SESSION/ORDINAL[:LINE], SESSION, a message id,
// /path/file.jsonl:LINE) or its recipient, a session id prefix. A prefix
// is checked against every transcript the agent tracks (subagents
// included; a denied one is never indexed, so the index cannot tell) and
// every Devin session, and any match that may not reach the server counts:
// the prefix alone could tell the server which. A path is the transcript
// it names. A message id or anything else matches no session id.
func (a *Agent) BusWithheld(ctx context.Context, ref string) (string, error) {
	addr, err := format.ParseAddress(ref)
	if err != nil {
		return "", nil
	}
	pv := a.policy()
	if pv.pol.Empty() {
		return "", nil
	}
	var prefix string
	switch addr.Kind {
	case format.AddrPath:
		p := addr.Path
		if strings.HasPrefix(p, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				p = filepath.Join(home, p[2:])
			}
		}
		a.mu.Lock()
		t := a.targets[filepath.Clean(p)]
		a.mu.Unlock()
		if t != nil && t.kind == kindTranscript && !a.uploadable(t) {
			if t.src.SessionKey != "" {
				return t.src.SessionKey, nil
			}
			return p, nil
		}
		return "", nil
	case format.AddrMessage:
		prefix = addr.Session
	default:
		prefix = addr.Token
	}
	if prefix == "" {
		return "", nil
	}
	a.mu.Lock()
	var match []*target
	for _, t := range a.targets {
		if t.kind == kindTranscript && t.src.SessionKey != "" && strings.HasPrefix(t.src.SessionKey, prefix) {
			match = append(match, t)
		}
	}
	a.mu.Unlock()
	for _, t := range match {
		if !a.uploadable(t) {
			return t.src.SessionKey, nil
		}
	}
	if a.devin.path != "" {
		a.loadDevinModes(ctx, pv)
		a.mu.Lock()
		defer a.mu.Unlock()
		for session, m := range a.devinModes {
			if strings.HasPrefix(session, prefix) && m != pathpolicy.Allow {
				return session, nil
			}
		}
	}
	return "", nil
}

// BusRoot is the top-level session a subagent's session belongs to, or
// session itself (issue #107). A subagent is not a session a message can
// be addressed to: peers lists top-level sessions only, and its hooks
// carry the parent's id. So a subagent sends, and reads its inbox, as its
// session, and a reply reaches the session. Claude Code subagents already
// call as their session (the caller rule finds the parent's process);
// a Codex subagent's shell and MCP calls name its own thread
// (CODEX_THREAD_ID, _meta.threadId), which the index links to its parent
// (session_meta.source.subagent). A thread the index does not hold yet is
// indexed first, by session id.
func (a *Agent) BusRoot(ctx context.Context, agent, session string) string {
	if session == "" {
		return ""
	}
	root, found := a.busParent(ctx, agent, session)
	if !found {
		if _, err := a.FlushPath(ctx, "", session); err == nil {
			root, _ = a.busParent(ctx, agent, session)
		}
	}
	return root
}

// busParent climbs the session's parent links (at most eight) to a
// top-level session. found reports whether the index holds the session.
func (a *Agent) busParent(ctx context.Context, agent, session string) (root string, found bool) {
	root = session
	for i := range 8 {
		var parent string
		var depth int
		err := a.store.DB().QueryRowContext(ctx, `SELECT depth, COALESCE(parent_session_id, '') FROM conversations
			WHERE session_id = ? AND (? = '' OR agent = ?) AND deleted_in_generation IS NULL ORDER BY depth LIMIT 1`, root, agent, agent).Scan(&depth, &parent)
		if err != nil {
			return root, i > 0
		}
		if depth == 0 || parent == "" || parent == root {
			return root, true
		}
		root = parent
	}
	return root, true
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
