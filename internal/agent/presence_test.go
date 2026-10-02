package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicebus"
	"github.com/flopwire/flopwire/internal/transcript"
)

// indexed is a top-level session in the index and its last write.
type indexed struct {
	agent, id string
	last      time.Time
}

func (f *fixture) topSessions() []indexed {
	f.t.Helper()
	rows, err := f.store.DB().Query(`SELECT agent, session_id, last_activity_at FROM conversations
		WHERE depth = 0 AND deleted_in_generation IS NULL AND last_activity_at IS NOT NULL ORDER BY session_id`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []indexed
	for rows.Next() {
		var s indexed
		var ms int64
		if err := rows.Scan(&s.agent, &s.id, &ms); err != nil {
			f.t.Fatal(err)
		}
		s.last = time.UnixMilli(ms)
		out = append(out, s)
	}
	return out
}

func (f *fixture) pick(agent string) indexed {
	f.t.Helper()
	for _, s := range f.topSessions() {
		if s.agent == agent {
			return s
		}
	}
	f.t.Fatalf("no %s session in the fixture", agent)
	return indexed{}
}

// alphaLast is the alpha session's last write.
func (f *fixture) alphaLast() time.Time {
	f.t.Helper()
	for _, s := range f.topSessions() {
		if s.id == alphaID {
			return s.last
		}
	}
	f.t.Fatal("alpha not indexed")
	return time.Time{}
}

func (f *fixture) presence(at time.Time) map[string]devicebus.Session {
	f.t.Helper()
	f.a.now = func() time.Time { return at }
	all, err := f.a.BusPresence(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[string]devicebus.Session{}
	for _, s := range all {
		out[s.SessionID] = s
	}
	return out
}

func writeFile(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Presence is what `sessions` prints as live: written in the last ten
// minutes, or held open by its harness and written within the hour. The
// harness registries are synthetic files here; pids are stubbed.
func TestPresenceFromHarnessRegistries(t *testing.T) {
	devinPath, _ := buildDevin(t)
	f := newFixture(t, devinPath)
	f.once()
	alive := map[int]bool{}
	started := map[int]time.Time{}
	names := map[int]string{}
	f.a.pidAlive = func(pid int) bool { return alive[pid] }
	f.a.procName = func(pid int) string { return names[pid] }
	f.a.procStart = func(pid int) (time.Time, bool) { t, ok := started[pid]; return t, ok }

	cl, cx, dv := f.pick("claude"), f.pick("codex"), f.pick("devin")

	// Written five minutes ago: live with no registry, idle.
	p := f.presence(cl.last.Add(5 * time.Minute))
	s, ok := p[cl.id]
	if !ok || s.Busy || s.Agent != "claude" {
		t.Fatalf("recently written Claude session: %+v %v", s, ok)
	}
	// Thirty minutes later, with nothing holding it open: ended.
	if _, ok := f.presence(cl.last.Add(30 * time.Minute))[cl.id]; ok {
		t.Fatal("a quiet session without a harness registry is live")
	}

	// Claude: a session file of a running process, started when procStart
	// says, with status busy.
	sessions := filepath.Join(f.home, ".claude", "sessions")
	start := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	writeFile(t, filepath.Join(sessions, "4242.json"), fmt.Sprintf(`{"pid":4242,"sessionId":%q,"status":"busy","procStart":%q,"updatedAt":0}`,
		cl.id, start.Format(time.ANSIC)))
	alive[4242], started[4242] = true, start
	s, ok = f.presence(cl.last.Add(30 * time.Minute))[cl.id]
	if !ok || !s.Busy {
		t.Fatalf("Claude session held open and busy: %+v %v", s, ok)
	}
	// An hour and more without a write: not live, held open or not.
	if _, ok := f.presence(cl.last.Add(61 * time.Minute))[cl.id]; ok {
		t.Fatal("held open past LiveCap")
	}
	// The pid was reused by another process: the file does not count.
	started[4242] = start.Add(time.Hour)
	if _, ok := f.presence(cl.last.Add(30 * time.Minute))[cl.id]; ok {
		t.Fatal("a reused pid kept the session live")
	}
	alive[4242] = false
	started[4242] = start

	// Codex: a writer lock (looked at, never locked) and the rollout's last
	// task event.
	writeFile(t, filepath.Join(f.home, ".codex", "thread-writer-locks", cx.id+".lock"), "")
	rollout := f.a.transcriptsBySession()[placeKey{transcript.AgentCodex, cx.id}].path
	appendFile(t, rollout, `{"timestamp":"2026-09-23T11:00:00.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t9"}}`+"\n")
	s, ok = f.presence(cx.last.Add(30 * time.Minute))[cx.id]
	if !ok || !s.Busy {
		t.Fatalf("Codex session mid-turn: %+v %v", s, ok)
	}
	appendFile(t, rollout, `{"timestamp":"2026-09-23T11:00:01.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t9"}}`+"\n")
	if s := f.presence(cx.last.Add(30 * time.Minute))[cx.id]; s.Busy {
		t.Fatal("Codex session busy after task_complete")
	}

	// Devin: live only while a session lock names a running devin process,
	// even when it wrote a minute ago (the lock outlives the session).
	if _, ok := f.presence(dv.last.Add(time.Minute))[dv.id]; ok {
		t.Fatal("a Devin session without a lock is live")
	}
	lock := filepath.Join(filepath.Dir(devinPath), "session_locks", dv.id+".lock")
	writeFile(t, lock, "5151\n")
	if _, ok := f.presence(dv.last.Add(time.Minute))[dv.id]; ok {
		t.Fatal("Devin lock of a dead pid counted")
	}
	alive[5151], names[5151] = true, "zsh"
	if _, ok := f.presence(dv.last.Add(time.Minute))[dv.id]; ok {
		t.Fatal("Devin lock naming a reused pid (not devin) counted")
	}
	names[5151] = "devin"
	s, ok = f.presence(dv.last.Add(30 * time.Minute))[dv.id]
	if !ok || s.Busy {
		t.Fatalf("Devin session held open: %+v %v", s, ok)
	}
	// The registries were only read.
	if fi, err := os.Stat(lock); err != nil || fi.Size() != 5 {
		t.Fatalf("lock file changed: %v", err)
	}
}

// Devin's busy or idle is its last hook event, as `flopwire hook` hands it
// to the agent with each flush: a prompt or a tool call means a turn runs,
// Stop means it ended. A busy mark lapses after hookBusyCap.
func TestPresenceDevinBusyFromHookEvents(t *testing.T) {
	devinPath, _ := buildDevin(t)
	f := newFixture(t, devinPath)
	f.once()
	f.a.pidAlive = func(pid int) bool { return pid == 5151 }
	f.a.procName = func(int) string { return "devin" }
	dv := f.pick("devin")
	writeFile(t, filepath.Join(filepath.Dir(devinPath), "session_locks", dv.id+".lock"), "5151\n")
	at := dv.last.Add(time.Minute)
	f.a.now = func() time.Time { return at }
	busy := func() bool {
		t.Helper()
		s, ok := f.presence(at)[dv.id]
		if !ok {
			t.Fatal("Devin session not live")
		}
		return s.Busy
	}
	if busy() {
		t.Fatal("busy before any hook event")
	}
	for _, c := range []struct {
		event string
		busy  bool
	}{
		{"SessionStart", false},
		{"UserPromptSubmit", true},
		{"PostToolUse", true},
		{"Notification", true}, // not a turn event: no change
		{"Stop", false},
		{"PostToolUse", true},
		{"SessionEnd", false},
	} {
		f.a.noteHookEvent(dv.id, c.event)
		if got := busy(); got != c.busy {
			t.Fatalf("after %s: busy %v, want %v", c.event, got, c.busy)
		}
	}
	f.a.noteHookEvent(dv.id, "PostToolUse")
	f.a.noteHookEvent("", "PostToolUse") // no session: ignored
	at = at.Add(hookBusyCap + time.Second)
	if busy() {
		t.Fatal("busy past hookBusyCap with no hook event")
	}
}

// A session the path rules keep off the server is marked withheld, so the
// poll does not report it; the others are not.
func TestPresenceWithheldByPathRule(t *testing.T) {
	f := newFixture(t, "-")
	f.cfg.UserRuleList = []string{"local /tmp/oracle-alpha"}
	f.a = New(f.store, f.cfg)
	f.once()
	// Alpha and a Codex session of the fixture were written within five
	// minutes of this.
	f.a.now = func() time.Time { return f.alphaLast().Add(5 * time.Minute) }
	all, err := f.a.BusPresence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var withheld, open int
	for _, s := range all {
		if s.Withheld != strings.HasPrefix(s.Repo, "/tmp/oracle-alpha") {
			t.Fatalf("withheld is not the local rule's verdict: %+v", s)
		}
		if s.Withheld {
			withheld++
		} else {
			open++
		}
	}
	if !slices.ContainsFunc(all, func(s devicebus.Session) bool { return s.SessionID == alphaID }) || withheld == 0 || open == 0 {
		t.Fatalf("presence: %+v", all)
	}
}

// Known sessions carry the path rules' verdict too, so a withheld session
// that is not live is still kept off the server (devicebus notWithheld).
func TestKnownWithheldByPathRule(t *testing.T) {
	f := newFixture(t, "-")
	f.cfg.UserRuleList = []string{"local /tmp/oracle-alpha"}
	f.a = New(f.store, f.cfg)
	f.once()
	var withheld, open int
	for _, s := range f.topSessions() {
		known, err := f.a.BusKnown(ctx, s.id)
		if err != nil {
			t.Fatal(err)
		}
		if len(known) == 0 {
			t.Fatalf("session %s not known", s.id)
		}
		k := known[0]
		if k.Withheld != strings.HasPrefix(k.Repo, "/tmp/oracle-alpha") {
			t.Fatalf("withheld is not the local rule's verdict: %+v", k)
		}
		if k.Withheld {
			withheld++
		} else {
			open++
		}
	}
	if withheld == 0 || open == 0 {
		t.Fatalf("withheld %d, open %d", withheld, open)
	}
}
