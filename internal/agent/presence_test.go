package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/flopwire/flopwire/internal/devicebus"
	"github.com/flopwire/flopwire/internal/redact"
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

// endedFixture is presence with a local bus on the test's clock.
func endedFixture(t *testing.T) (*fixture, *time.Time) {
	t.Helper()
	devinPath, _ := buildDevin(t)
	f := newFixture(t, devinPath)
	clock := new(time.Time)
	b, err := devicebus.Open(filepath.Join(t.TempDir(), "bus.db"), devicebus.Config{User: "gary", Logger: f.cfg.Logger, Now: func() time.Time { return *clock }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	f.cfg.Bus = b
	f.a = New(f.store, f.cfg)
	f.once()
	return f, clock
}

// present is presence at a time on both clocks: is the session live?
func (f *fixture) present(clock *time.Time, at time.Time, id string) bool {
	f.t.Helper()
	*clock = at
	_, ok := f.presence(at)[id]
	return ok
}

// An ended session drops out of presence at once, however recently it
// wrote (#82): a Claude session file of a dead process, a released Codex
// writer lock, a Devin lock of a dead pid, a SessionEnd hook; a Claude
// session file that is gone ends it a second later. Idleness does not.
func TestPresenceEndedSessions(t *testing.T) {
	f, clock := endedFixture(t)
	alive := map[int]bool{}
	started := map[int]time.Time{}
	f.a.pidAlive = func(pid int) bool { return alive[pid] }
	f.a.procName = func(int) string { return "devin" }
	f.a.procStart = func(pid int) (time.Time, bool) { t, ok := started[pid]; return t, ok }
	cl, cx, dv := f.pick("claude"), f.pick("codex"), f.pick("devin")

	// Claude: held by a running process, written a minute ago; killed (its
	// file stays, naming a dead pid): ended at the next read.
	at := cl.last.Add(time.Minute)
	sessions := filepath.Join(f.home, ".claude", "sessions")
	start := cl.last.Add(-time.Hour).UTC().Truncate(time.Second)
	file := filepath.Join(sessions, "4242.json")
	writeFile(t, file, fmt.Sprintf(`{"pid":4242,"sessionId":%q,"status":"idle","procStart":%q}`, cl.id, start.Format(time.ANSIC)))
	alive[4242], started[4242] = true, start
	if !f.present(clock, at, cl.id) {
		t.Fatal("held Claude session not live")
	}
	// Idle for 50 minutes, still held: live.
	if !f.present(clock, cl.last.Add(50*time.Minute), cl.id) {
		t.Fatal("an idle held session ended")
	}
	alive[4242] = false
	if f.present(clock, at.Add(time.Second), cl.id) {
		t.Fatal("a killed Claude session is live")
	}
	// Resumed by a new process: live again. (The dead process's file goes:
	// left there, it would end the session at once when the new process
	// exits, which is the next case.)
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sessions, "4343.json"), fmt.Sprintf(`{"pid":4343,"sessionId":%q,"status":"idle","procStart":%q}`, cl.id, at.Add(2*time.Second).UTC().Format(time.ANSIC)))
	alive[4343], started[4343] = true, at.Add(2*time.Second).UTC().Truncate(time.Second)
	if !f.present(clock, at.Add(3*time.Second), cl.id) {
		t.Fatal("a resumed Claude session is not live")
	}
	// A clean exit removes the file: ended a second later.
	if err := os.Remove(filepath.Join(sessions, "4343.json")); err != nil {
		t.Fatal(err)
	}
	if !f.present(clock, at.Add(4*time.Second), cl.id) {
		t.Fatal("ended at the first read without its file")
	}
	if f.present(clock, at.Add(6*time.Second), cl.id) {
		t.Fatal("a Claude session whose file is gone is live")
	}

	// Codex: the writer's flock, not the file.
	locks := filepath.Join(f.home, ".codex", "thread-writer-locks")
	writeFile(t, filepath.Join(locks, ".coordination.lock"), "")
	lockPath := filepath.Join(locks, cx.id+".lock")
	writeFile(t, lockPath, "")
	w, err := os.Open(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(w.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	at = cx.last.Add(30 * time.Minute)
	if !f.present(clock, at, cx.id) {
		t.Fatal("a Codex session whose writer holds the lock is not live")
	}
	w.Close() // the writer exits; the file stays
	if f.present(clock, at.Add(time.Second), cx.id) || f.present(clock, cx.last.Add(time.Minute), cx.id) {
		t.Fatal("a Codex session whose writer is gone is live")
	}
	if fi, err := os.Stat(lockPath); err != nil || fi.Size() != 0 {
		t.Fatalf("the lock file changed: %v", err)
	}

	// Devin: a SessionEnd hook ends it while its devin still runs; a later
	// hook of it (a resume) revives it.
	writeFile(t, filepath.Join(filepath.Dir(f.cfg.DevinDB), "session_locks", dv.id+".lock"), "5151\n")
	alive[5151], started[5151] = true, dv.last.Add(-time.Hour)
	at = dv.last.Add(time.Minute)
	if !f.present(clock, at, dv.id) {
		t.Fatal("held Devin session not live")
	}
	if r := ask(t, f.a, Request{Op: "flush", Session: dv.id, Agent: "devin", Event: "SessionEnd", HookStart: at.UnixMilli()}); !r.OK && r.Error == "" {
		t.Fatal(r.Error)
	}
	if f.present(clock, at.Add(time.Second), dv.id) {
		t.Fatal("live after its SessionEnd hook")
	}
	ask(t, f.a, Request{Op: "flush", Session: dv.id, Event: "Stop", HookStart: at.Add(-time.Second).UnixMilli()})
	if f.present(clock, at.Add(2*time.Second), dv.id) {
		t.Fatal("a Stop hook from before the end revived the session")
	}
	ask(t, f.a, Request{Op: "flush", Session: dv.id, Event: "UserPromptSubmit", HookStart: at.Add(time.Minute).UnixMilli()})
	if !f.present(clock, at.Add(time.Minute+time.Second), dv.id) {
		t.Fatal("a hook after the end did not revive the session")
	}
	alive[5151] = false
	if f.present(clock, at.Add(2*time.Minute), dv.id) {
		t.Fatal("a Devin session of a dead pid is live")
	}
}

// A send's refs and recipient prefix are checked against the path rules
// (devicebus namesNoWithheld, issue #71): an address, prefix or path of a
// withheld session (or its subagent) names it, whether the rule keeps it
// local or denies it (never indexed); the others name none.
func TestBusWithheldNamesWithheldSessions(t *testing.T) {
	for _, rule := range []string{"local /tmp/oracle-alpha", "deny /tmp/oracle-alpha"} {
		t.Run(strings.Fields(rule)[0], func(t *testing.T) {
			f := newFixture(t, "-")
			f.cfg.UserRuleList = []string{rule}
			f.a = New(f.store, f.cfg)
			f.once()
			var sub, open string
			var codex []*target
			f.a.mu.Lock()
			for _, tg := range f.a.targets {
				if tg.kind == kindTranscript && tg.root == alphaID {
					sub = tg.src.SessionKey
				}
				if tg.kind == kindTranscript && tg.src.Agent == transcript.AgentCodex {
					codex = append(codex, tg)
				}
			}
			f.a.mu.Unlock()
			for _, tg := range codex {
				if f.a.uploadable(tg) {
					open = tg.src.SessionKey
				}
			}
			if sub == "" || open == "" {
				t.Fatalf("subagent %q, open session %q", sub, open)
			}
			for ref, want := range map[string]bool{
				alphaID:                 true,
				alphaID[:8]:             true, // also orphanID's prefix: either may be meant
				alphaID[:13] + "/3":     true,
				alphaID + "/3:2":        true,
				sub:                     true,
				sub + "/1":              true,
				f.path(alphaRel) + ":1": true,
				"~/" + alphaRel + ":1":  true, // the fixture's home is not $HOME, but the id is in it
				// A ref is free text: any form that carries the id (or a
				// prefix of it at the start) names the session.
				alphaID + ":3":      true,
				alphaID + "/latest": true,
				"see " + alphaID:    true,
				alphaID[:8] + ":3":  true,
				f.path(alphaRel):    true,
				f.path(".claude/projects/-tmp-oracle-alpha/"+alphaID+"/tool-results/x.txt") + ":1": true,
				open + ":3":                   false,
				open:                          false,
				open + "/4":                   false,
				orphanID:                      false,
				"not an address of a session": false,
			} {
				id, err := f.a.BusWithheld(ctx, ref)
				if err != nil {
					t.Fatal(err)
				}
				if (id != "") != want {
					t.Fatalf("%s: withheld %q, want %v", ref, id, want)
				}
			}
		})
	}
	// Without rules nothing is withheld.
	f := newFixture(t, "-")
	f.once()
	if id, err := f.a.BusWithheld(ctx, alphaID); id != "" || err != nil {
		t.Fatalf("no rules: %q %v", id, err)
	}
}

// A title cut at 100 runes through a secret is redacted before the cut:
// the redactor does not match a token's first characters alone, so
// redacting the cut leaked them (issue #71). Presence reports the title so.
func TestBusTitleRedactsBeforeTheCut(t *testing.T) {
	f := newFixture(t, "-")
	const sid = "0b7e2c1a-0000-4000-8000-0000000000e1"
	token := "ghp_" + strings.Repeat("Ab3dEf6hIj", 4)[:36]
	prompt := strings.Repeat("deploy ", 12) + "with " + token + " then check the logs\nsecond line"
	if at := strings.Index(prompt, token); at >= titleCut || at+len(token) <= titleCut {
		t.Fatalf("the token spans %d..%d, not the cut at %d", at, at+len(token), titleCut)
	}
	f.writeSession(sid, "/tmp/oracle-title", claudeRecord(sid, "/tmp/oracle-title", "", prompt, 1))
	f.once()
	known, err := f.a.BusKnown(ctx, sid)
	if err != nil || len(known) != 1 {
		t.Fatalf("known: %+v %v", known, err)
	}
	s := known[0]
	leak := token[:titleCut-strings.Index(prompt, token)] // what the cut kept of it
	if !strings.Contains(s.Title, leak) || utf8.RuneCountInString(s.Title) != titleCut {
		t.Fatalf("the index's title is not the raw cut: %q", s.Title)
	}
	if masked, _ := redact.Redact([]byte(s.Title)); !strings.Contains(string(masked), leak) {
		t.Fatalf("the redactor matches the cut token, so this tests nothing: %q", masked)
	}
	got := f.a.busTitle(ctx, s)
	if strings.Contains(got, token[4:8]) || !strings.HasPrefix(got, "deploy deploy") || utf8.RuneCountInString(got) > titleCut {
		t.Fatalf("bus title %q", got)
	}
	// Presence carries it.
	f.a.now = func() time.Time { return time.Date(2026, 9, 23, 11, 1, 0, 0, time.UTC) }
	all, err := f.a.BusPresence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(all, func(p devicebus.Session) bool { return p.SessionID == sid })
	if i < 0 || all[i].Title != got {
		t.Fatalf("presence: %+v", all)
	}
	// A cut title whose line is not at hand loses its last word.
	s.Title = strings.Repeat("word ", 19) + "ghp_Ab3dE"
	if got := f.a.busTitle(ctx, s); got != strings.Repeat("word ", 18)+"word …" {
		t.Fatalf("no line: %q", got)
	}
	// A short title is no cut.
	s.Title = "fix the flaky upload test"
	if got := f.a.busTitle(ctx, s); got != s.Title {
		t.Fatalf("short title: %q", got)
	}
}
