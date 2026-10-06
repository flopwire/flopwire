package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/flopwire/flopwire/internal/devicebus"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/redact"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/devin"
	"github.com/flopwire/flopwire/internal/transcript/opencode/opencodetest"
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

// Presence uses recency for uncertain evidence and keeps confirmed
// harness holders until a real end. The
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
	// A confirmed running holder stays live after transcript inactivity.
	if _, ok := f.presence(cl.last.Add(61 * time.Minute))[cl.id]; !ok {
		t.Fatal("confirmed holder disappeared past LiveCap")
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
	started[5151] = start
	s, ok = f.presence(dv.last.Add(30 * time.Minute))[dv.id]
	if !ok || s.Busy {
		t.Fatalf("Devin session held open: %+v %v", s, ok)
	}
	if _, ok := f.presence(dv.last.Add(2 * time.Hour))[dv.id]; !ok {
		t.Fatal("confirmed Devin holder disappeared past LiveCap")
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
		f.a.noteHookEvent("devin", dv.id, c.event, at)
		if got := busy(); got != c.busy {
			t.Fatalf("after %s: busy %v, want %v", c.event, got, c.busy)
		}
	}
	f.a.noteHookEvent("devin", dv.id, "PostToolUse", at)
	f.a.noteHookEvent("devin", "", "PostToolUse", at) // no session: ignored
	at = at.Add(hookBusyCap + time.Second)
	if busy() {
		t.Fatal("busy past hookBusyCap with no hook event")
	}
}

// One devin process can hold several sessions (an ACP client's
// session/new): each has a lock naming the same pid. A session the
// process let go (ACP session/delete closes its lock file and drops it
// from the store, and fires no SessionEnd) is not live, while the
// process's other session is (devin 3000.11.1, issue #60).
func TestPresenceDevinOneProcessSeveralSessions(t *testing.T) {
	devinPath, db := buildDevin(t)
	f := newFixture(t, devinPath)
	f.once()
	const pid = 5151
	lockDir := filepath.Join(filepath.Dir(devinPath), "session_locks")
	started := time.Now().Add(-time.Hour)
	f.a.pidAlive = func(p int) bool { return p == pid }
	f.a.procName = func(int) string { return "devin" }
	f.a.procStart = func(int) (time.Time, bool) { return started, true }
	var open []string
	lsofCalls := 0
	f.a.openFiles = func(_ context.Context, p int) []string {
		lsofCalls++
		if p != pid {
			return nil
		}
		return open
	}
	const running, ended = "devin-oracle-001", "devin-oracle-002"
	for _, id := range []string{running, ended} {
		writeFile(t, filepath.Join(lockDir, id+".lock"), fmt.Sprintf("%d\n", pid))
	}
	var last time.Time
	for _, s := range f.topSessions() {
		if s.id == ended {
			last = s.last
		}
	}
	at := last.Add(20 * time.Minute) // past LiveWindow: live only on the lock
	live := func() (bool, bool) {
		t.Helper()
		p := f.presence(at)
		_, r := p[running]
		_, e := p[ended]
		return r, e
	}
	// The process holds both lock files open: both live.
	open = []string{"/dev/null", filepath.Join(lockDir, running+".lock"), filepath.Join(lockDir, ended+".lock")}
	if r, e := live(); !r || !e {
		t.Fatalf("both sessions held open: running %v, ended %v", r, e)
	}
	// It closed one session's lock: only the other is live.
	open = []string{"/dev/null", filepath.Join(lockDir, running+".lock")}
	if r, e := live(); !r || e {
		t.Fatalf("one lock let go: running %v, ended %v", r, e)
	}
	// The process table cannot tell: the locks stand.
	open = nil
	if r, e := live(); !r || !e {
		t.Fatalf("open files unknown: running %v, ended %v", r, e)
	}
	// lsof hangs: the presence check does not wait past its budget, and
	// the locks stand.
	f.a.openFiles = func(ctx context.Context, _ int) []string {
		lsofCalls++
		<-ctx.Done()
		return nil
	}
	began := time.Now()
	if r, e := live(); !r || !e {
		t.Fatalf("open files cut off: running %v, ended %v", r, e)
	}
	if d := time.Since(began); d > devinOpenFilesBudget+time.Second {
		t.Fatalf("a hung lsof held presence for %s", d)
	}
	f.a.openFiles = func(_ context.Context, p int) []string {
		lsofCalls++
		if p != pid {
			return nil
		}
		return open
	}
	// The session was deleted from the store, its lock file kept.
	if _, err := db.Exec(`DELETE FROM sessions WHERE id = ?`, ended); err != nil {
		t.Fatal(err)
	}
	if r, e := live(); !r || e {
		t.Fatalf("deleted session: running %v, ended %v", r, e)
	}
	// A process named by one lock alone is not asked for its open files.
	lsofCalls = 0
	if err := os.Remove(filepath.Join(lockDir, ended+".lock")); err != nil {
		t.Fatal(err)
	}
	if r, _ := live(); !r || lsofCalls != 0 {
		t.Fatalf("single lock: running %v, open-file reads %d", r, lsofCalls)
	}
	// A devin that started after the lock was written reuses the pid of
	// the session's dead process.
	started = time.Now().Add(time.Minute)
	if r, _ := live(); r {
		t.Fatal("a lock older than its devin process counted")
	}
}

// Devin fires no Stop hook for a turn the user interrupts; the store's
// marker ends the busy mark within the presence tick, not after
// hookBusyCap. A turn started after the interrupt stays busy.
func TestPresenceDevinInterruptedTurnIsIdle(t *testing.T) {
	devinPath, db := buildDevin(t)
	f := newFixture(t, devinPath)
	f.once()
	f.a.pidAlive = func(pid int) bool { return pid == 5151 }
	f.a.procName = func(int) string { return "devin" }
	f.a.procStart = func(int) (time.Time, bool) { return time.Now().Add(-time.Hour), true }
	dv := f.pick("devin")
	writeFile(t, filepath.Join(filepath.Dir(devinPath), "session_locks", dv.id+".lock"), "5151\n")
	at := dv.last.Add(time.Minute)
	var interruptLog bytes.Buffer
	f.a.log = slog.New(slog.NewTextHandler(&interruptLog, &slog.HandlerOptions{Level: slog.LevelDebug}))
	busy := func() bool {
		t.Helper()
		interruptLog.Reset()
		s, ok := f.presence(at)[dv.id]
		if !ok {
			t.Fatal("Devin session not live")
		}
		return s.Busy
	}
	waitIdle := func() {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for busy() {
			// A failed read deliberately retains busy until the next presence
			// tick. Retry only a witnessed deadline, never a successful read
			// that missed the marker or an unrelated store error.
			if !strings.Contains(interruptLog.String(), `msg="agent: devin interrupt"`) || !strings.Contains(interruptLog.String(), "context deadline exceeded") {
				t.Fatalf("busy after interrupt without a read deadline: %s", interruptLog.String())
			}
			if time.Now().After(deadline) {
				t.Fatalf("interrupt not observed within 2s: %s", interruptLog.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
		if marked, _ := f.a.hookBusy(transcript.AgentDevin, dv.id); marked {
			t.Fatal("idle presence did not clear Devin's hook state")
		}
		if marked, _ := f.a.hookBusy(transcript.AgentOpencode, dv.id); !marked {
			t.Fatal("Devin interrupt cleared another harness's hook state")
		}
	}
	node := func(id int, msg string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO message_nodes (session_id, node_id, parent_node_id, chat_message, created_at) VALUES (?, ?, NULL, ?, 0)`,
			dv.id, 1000+id, msg); err != nil {
			t.Fatal(err)
		}
	}
	hook := time.Date(2026, 10, 4, 20, 10, 19, 0, time.UTC)
	f.a.noteHookEvent("opencode", dv.id, "PostToolUse", hook)
	f.a.noteHookEvent("devin", dv.id, "PostToolUse", hook)
	if !busy() {
		t.Fatal("not busy after PostToolUse")
	}
	// Esc twice while the next tool ran: its result is the marker, no Stop.
	node(1, `{"role":"tool","content":"Canceled due to user interrupt","tool_call_id":"get_output_0#1","metadata":{"created_at":"2026-10-04T20:10:28.937439Z","extensions":{"chisel/tool_failure":{"reason":"Canceled"}}}}`)
	// Prove the timeout path: a marker exists, but an expired read must
	// retain busy state so an uncertain read cannot announce an idle turn.
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := devin.InterruptedSince(expired, devinPath, dv.id, hook); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired interrupt read: %v", err)
	}
	if !f.a.devinTurnBusy(expired, dv.id) {
		t.Fatal("failed interrupt read announced idle")
	}
	if !strings.Contains(interruptLog.String(), "context deadline exceeded") {
		t.Fatalf("interrupt deadline was not logged: %s", interruptLog.String())
	}
	if marked, start := f.a.hookBusy(transcript.AgentDevin, dv.id); !marked || !start.Equal(hook) {
		t.Fatal("failed interrupt read changed hook state")
	}
	waitIdle()
	// The next prompt starts a turn after the interrupt: busy.
	f.a.noteHookEvent("devin", dv.id, "UserPromptSubmit", hook.Add(30*time.Second))
	f.a.hookTurnEnded(transcript.AgentDevin, dv.id, hook) // late completion of the old read
	if !busy() {
		t.Fatal("a turn after the interrupt is not busy")
	}
	// Interrupted while the model answered.
	node(2, `{"role":"system","content":"[Response interrupted by user]","metadata":{"created_at":"2026-10-04T20:10:55.035344Z"}}`)
	waitIdle()
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
	// A cut with no word break left loses everything (a 150-character
	// token cut at 100).
	s.Title = string([]rune("ghp_" + strings.Repeat("Ab3dEf6hIj", 15))[:titleCut])
	if got := f.a.busTitle(ctx, s); got != "" {
		t.Fatalf("no word break: %q", got)
	}
	// A short title is no cut.
	s.Title = "fix the flaky upload test"
	if got := f.a.busTitle(ctx, s); got != s.Title {
		t.Fatalf("short title: %q", got)
	}
}

// A repo a bus request names is withheld when the path rules withhold
// its path, or, by name, when every session of the device on it is
// withheld (devicebus reposNotWithheld, issue #71).
func TestBusRepoWithheld(t *testing.T) {
	f := newFixture(t, "-")
	f.cfg.UserRuleList = []string{"local /tmp/oracle-alpha"}
	f.a = New(f.store, f.cfg)
	f.once()
	for repo, want := range map[string]bool{
		"/tmp/oracle-alpha":     true,
		"/tmp/oracle-alpha/":    true,
		"/tmp/oracle-alpha/src": true,
		"oracle-alpha":          true,
		"/tmp/oracle-beta":      false,
		"oracle-beta":           false,
		"no-such-repo":          false,
		"oracle-*":              false,
		"oracle_alpha":          false, // LIKE's _ is escaped
	} {
		got, err := f.a.BusRepoWithheld(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s: withheld %v, want %v", repo, got, want)
		}
	}
	f = newFixture(t, "-")
	f.once()
	if got, err := f.a.BusRepoWithheld(ctx, "/tmp/oracle-alpha"); got || err != nil {
		t.Fatalf("no rules: %v %v", got, err)
	}
}

// A repo name matches a session as --repo expansion does (local.named):
// by its checkout's name, its main checkout's name or its origin
// (name or owner/name, any case). An allowed session in a linked
// worktree of a repo by that name keeps the name open, and a withheld
// repo's origin name is withheld (issue #71).
func TestBusRepoWithheldByMainCheckoutAndRemote(t *testing.T) {
	f := newFixture(t, "-")
	f.cfg.UserRuleList = []string{"local /tmp/oracle-alpha"}
	f.a = New(f.store, f.cfg)
	const wt = "0b7e2c1a-0000-4000-8000-0000000000e2"
	f.writeSession(wt, "/tmp/work/oracle-alpha-wt", claudeRecord(wt, "/tmp/work/oracle-alpha-wt", "", "hello", 1))
	f.once()
	place := func(session, main, remote string) {
		t.Helper()
		pl := localindex.Placement{Agent: transcript.AgentClaude, SessionID: session, How: localindex.PlacedByWorktree}
		pl.Main, pl.Remote = main, remote
		if err := f.store.SavePlacement(ctx, pl); err != nil {
			t.Fatal(err)
		}
		if err := f.store.Sync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	place(alphaID, "/tmp/oracle-alpha", "github.com/acme/secret-svc")
	if got, err := f.a.BusRepoWithheld(ctx, "oracle-alpha"); !got || err != nil {
		t.Fatalf("only a withheld session on oracle-alpha: %v %v", got, err)
	}
	for _, name := range []string{"secret-svc", "acme/secret-svc", "Acme/Secret-Svc"} {
		if got, err := f.a.BusRepoWithheld(ctx, name); !got || err != nil {
			t.Errorf("%s, the withheld repo's origin: withheld %v %v", name, got, err)
		}
	}
	// An allowed linked worktree of another checkout named oracle-alpha.
	place(wt, "/tmp/work/oracle-alpha", "")
	if got, err := f.a.BusRepoWithheld(ctx, "oracle-alpha"); got || err != nil {
		t.Fatalf("an allowed worktree's main checkout is oracle-alpha: %v %v", got, err)
	}
}

// Review of #125: an @user send's repo resolves to a remote only when the
// path rules let every checkout and remote of the repository reach the
// server (local.ServerRepo), as peers --repo does. Here one session of
// oracle-alpha has no remote and is allowed, so the name is not
// withheld, but the repository's other session recorded the remote
// that a repo rule keeps local: that remote must not go to the server.
func TestBusRepoKeyLeavesOutAWithheldRemote(t *testing.T) {
	f := newFixture(t, "-")
	f.cfg.UserRuleList = []string{"local repo:github.com/acme/secret-svc"}
	f.a = New(f.store, f.cfg)
	const wt = "0b7e2c1a-0000-4000-8000-0000000000e3"
	f.writeSession(wt, "/tmp/work/oracle-alpha-wt", claudeRecord(wt, "/tmp/work/oracle-alpha-wt", "", "hello", 1))
	f.once()
	for _, p := range []localindex.Placement{
		{Agent: transcript.AgentClaude, SessionID: alphaID, How: localindex.PlacedByWorktree, Placement: pathpolicy.Placement{Cwd: "/tmp/oracle-alpha", Main: "/tmp/oracle-alpha"}},
		{Agent: transcript.AgentClaude, SessionID: wt, How: localindex.PlacedByWorktree, Placement: pathpolicy.Placement{Cwd: "/tmp/work/oracle-alpha-wt", Main: "/tmp/oracle-alpha", Remote: "github.com/acme/secret-svc"}},
	} {
		if err := f.store.SavePlacement(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	f.restart()
	f.once()
	if got, err := f.a.BusRepoWithheld(ctx, "oracle-alpha"); got || err != nil {
		t.Fatalf("an allowed session is on oracle-alpha: %v %v", got, err)
	}
	got, err := f.a.BusRepoKey(ctx, "oracle-alpha")
	if err != nil || strings.Contains(got, "secret-svc") {
		t.Fatalf("repo key %q %v: a withheld remote", got, err)
	}
}

// A session whose path rules are not decided yet (a young transcript that
// has not named its directory) is withheld and marked unplaced, so the bus
// waits for it and refuses it as not indexed yet, never as kept off the
// server by a path rule (issue #71). Once placed, it is neither.
func TestKnownUnplacedSession(t *testing.T) {
	const id = "019a0000-0000-7000-8000-0000000000a7"
	saved := cwdWait
	t.Cleanup(func() { cwdWait = saved })
	cwdWait = time.Hour // every copied fixture is young
	f := newFixture(t, "-")
	f.cfg.UserRuleList = []string{"local /tmp/oracle-alpha"}
	f.a = New(f.store, f.cfg)
	f.once()
	known, err := f.a.BusKnown(ctx, id)
	if err != nil || len(known) != 1 {
		t.Fatalf("known: %+v %v", known, err)
	}
	if !known[0].Withheld || !known[0].Unplaced {
		t.Fatalf("a session not placed yet: %+v", known[0])
	}
	cwdWait = 0 // the wait is over: placed by its fallback
	f.once()    // and looked for by the recovery pass
	known, _ = f.a.BusKnown(ctx, id)
	if len(known) != 1 || known[0].Unplaced {
		t.Fatalf("a placed session: %+v", known)
	}
	if err := f.a.BusPlace(ctx, "no-such-session"); err == nil {
		t.Fatal("placing an unknown session")
	}
}

func TestPresenceConfirmedCodexHolderBeyondCapAndIdleEvidence(t *testing.T) {
	f := newFixture(t, "-")
	f.once()
	cx := f.pick("codex")
	writeFile(t, filepath.Join(f.home, ".codex", "thread-writer-locks", cx.id+".lock"), "")
	rollout := f.a.transcriptsBySession()[placeKey{transcript.AgentCodex, cx.id}].path
	at := cx.last.Add(2 * time.Hour).UTC().Truncate(time.Millisecond)
	appendFile(t, rollout, fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"task_complete"}}`+"\n", at.Format(time.RFC3339Nano)))
	f.a.codexWriter = func(string) int { return codexHeld }
	s, ok := f.presence(at.Add(time.Hour))[cx.id]
	if !ok || s.Busy || !s.IdleKnown || !s.IdleSince.Equal(at) {
		t.Fatalf("held idle Codex: %+v %v", s, ok)
	}
	f.a.codexWriter = func(string) int { return codexUnknown }
	if _, ok := f.presence(at.Add(time.Hour))[cx.id]; ok {
		t.Fatal("uncertain lock bypassed LiveCap")
	}
	f.a.codexWriter = func(string) int { return codexReleased }
	if _, ok := f.presence(at.Add(time.Hour))[cx.id]; ok {
		t.Fatal("released writer held session live")
	}
}

func TestPresenceIdleSinceIsStopNotLastActivity(t *testing.T) {
	f := newFixture(t, "-")
	f.once()
	cl := f.pick("claude")
	f.a.pidAlive = func(int) bool { return true }
	start := cl.last.Add(-time.Hour).UTC().Truncate(time.Second)
	f.a.procStart = func(int) (time.Time, bool) { return start, true }
	writeFile(t, filepath.Join(f.home, ".claude", "sessions", "4242.json"), fmt.Sprintf(`{"sessionId":%q,"status":"idle","procStart":%q}`, cl.id, start.Format(time.ANSIC)))
	at := cl.last.Add(10 * time.Minute)
	f.a.now = func() time.Time { return at }
	if s := f.presence(at)[cl.id]; s.IdleKnown || !s.IdleSince.IsZero() {
		t.Fatal("invented idle time from activity/registry")
	}
	f.a.noteHookEvent("claude", cl.id, "Stop", at)
	later := at.Add(3 * time.Hour)
	if s := f.presence(later)[cl.id]; !s.IdleSince.Equal(at) || !s.IdleKnown {
		t.Fatalf("lost witnessed stop: %+v", s)
	}
	f.a.noteHookEvent("claude", cl.id, "UserPromptSubmit", later)
	f.a.noteHookEvent("claude", cl.id, "Stop", at) // delayed hook of the old turn
	if s := f.presence(later)[cl.id]; s.IdleKnown {
		t.Fatal("stale stop replaced newer turn")
	}
}

func TestPresenceIdleEvidenceDoesNotCrossHarnesses(t *testing.T) {
	f := newFixture(t, "-")
	f.once()
	cl := f.pick("claude")
	f.a.pidAlive = func(int) bool { return true }
	writeFile(t, filepath.Join(f.home, ".claude", "sessions", "4242.json"), fmt.Sprintf(`{"sessionId":%q,"status":"idle"}`, cl.id))
	at := cl.last.Add(time.Minute)
	f.a.now = func() time.Time { return at }
	f.a.noteHookEvent("opencode", cl.id, "Stop", at)
	if s := f.presence(at)[cl.id]; s.IdleKnown || !s.IdleSince.IsZero() {
		t.Fatal("other harness's Stop became Claude idle evidence")
	}
}

func TestPresenceUncertainHoldersStayBoundedWithoutEnding(t *testing.T) {
	for _, harness := range []string{"claude", "devin", "opencode"} {
		for _, missing := range []string{"process-start", "registry-start", "store", "open-files"} {
			if harness == "devin" && missing == "registry-start" || harness != "devin" && (missing == "store" || missing == "open-files") {
				continue
			}
			t.Run(harness+"/"+missing, func(t *testing.T) {
				devinPath, _ := buildDevin(t)
				f := newFixture(t, devinPath)
				pickHarness := harness
				if harness == "opencode" {
					oc := opencodetest.New(t, "")
					const id = "ses_synthetic0000000000000A"
					oc.Session(id, "", "/work/opencode-demo", "Presence", opencodetest.T0)
					oc.Prompt(id, opencodetest.T0, "hello")
					f.cfg.OpencodeDB = oc.Path
					f.cfg.OpencodeRegistry = filepath.Join(t.TempDir(), "opencode")
					f.a = New(f.store, f.cfg)
				}
				f.once()
				session := f.pick(pickHarness)
				start := session.last.Add(-time.Hour).UTC().Truncate(time.Second)
				f.a.pidAlive = func(int) bool { return true }
				f.a.procName = func(int) string { return harness }
				known := true
				f.a.procStart = func(int) (time.Time, bool) { return start, known }
				writeRegistry := func(withStart bool) {
					switch harness {
					case "claude":
						proof := start.Format(time.ANSIC)
						if !withStart {
							proof = ""
						}
						writeFile(t, filepath.Join(f.home, ".claude", "sessions", "4242.json"), fmt.Sprintf(`{"sessionId":%q,"procStart":%q,"status":"idle"}`, session.id, proof))
					case "devin":
						dir := filepath.Join(filepath.Dir(devinPath), "session_locks")
						writeFile(t, filepath.Join(dir, session.id+".lock"), "4242")
						if missing == "open-files" {
							writeFile(t, filepath.Join(dir, "second-session.lock"), "4242")
						}
						f.a.openFiles = func(context.Context, int) []string { return []string{filepath.Join(dir, session.id+".lock")} }
					case "opencode":
						ms := start.UnixMilli()
						if !withStart {
							ms = 0
						}
						writeFile(t, filepath.Join(f.cfg.OpencodeRegistry, "4242.json"), fmt.Sprintf(`{"started":%d,"sessions":[%q]}`, ms, session.id))
					}
				}
				writeRegistry(true)
				now := session.last.Add(3 * time.Hour)
				b, err := devicebus.Open(filepath.Join(t.TempDir(), "bus.db"), devicebus.Config{User: "gary", Now: func() time.Time { return now }})
				if err != nil {
					t.Fatal(err)
				}
				defer b.Close()
				f.a.cfg.Bus = b
				if _, ok := f.presence(now)[session.id]; !ok {
					t.Fatal("confirmed old holder disappeared")
				}
				switch missing {
				case "process-start":
					known = false
				case "registry-start":
					writeRegistry(false)
				case "store":
					f.a.devin.path = filepath.Join(filepath.Dir(devinPath), "missing.db")
				case "open-files":
					f.a.openFiles = func(context.Context, int) []string { return nil }
				}
				for i := 0; i < 2; i++ {
					now = now.Add(2 * time.Second)
					if _, ok := f.presence(now)[session.id]; ok {
						t.Fatal("uncertain old holder bypassed LiveCap")
					}
					reg := f.a.registries().reg
					ref := devicebus.Ref{Agent: harness, Session: session.id}
					if !slices.Contains(reg.Unknown, ref) {
						t.Fatalf("not unknown: %+v", reg)
					}
					ended, err := b.Observe(ctx, reg)
					if err != nil || ended[ref] {
						t.Fatalf("uncertainty ended lifecycle: %v %v", ended, err)
					}
				}
				// Returning to a recent clock exposes uncertain evidence, without a fake end.
				now = session.last.Add(5 * time.Minute)
				if _, ok := f.presence(now)[session.id]; !ok {
					t.Fatal("uncertain recent holder disappeared")
				}
			})
		}
	}
}
