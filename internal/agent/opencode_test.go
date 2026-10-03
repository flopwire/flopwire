package agent

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicebus"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/opencode"
	"github.com/flopwire/flopwire/internal/transcript/opencode/opencodetest"
)

// The agent indexes opencode's store beside Devin's: its sessions are
// searchable under agent opencode, handed to sync as per-session exports,
// and a deleted session's rows are superseded.
func TestOpencodeStore(t *testing.T) {
	oc := opencodetest.New(t, "")
	const ses = "ses_synthetic00000000000001"
	oc.Session(ses, "", "/work/opencode-demo", "Tune the cache", opencodetest.T0)
	oc.Prompt(ses, opencodetest.T0, "why is the parrot cache cold")

	f := newFixture(t, "-")
	f.cfg.OpencodeDB = oc.Path
	f.a = New(f.store, f.cfg)
	f.once()
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	hits, err := f.store.Find(ctx, "parrot cache", localindex.FindOptions{Filter: localindex.Filter{Agents: []transcript.Agent{transcript.AgentOpencode}}})
	if err != nil || len(hits) != 1 {
		t.Fatalf("opencode hits = %d, %v", len(hits), err)
	}
	spec, ok := f.rec.spec(opencode.ExportPath(oc.Path, ses))
	if !ok || spec.Agent != transcript.AgentOpencode || spec.Parser != opencode.ExportFormat || !spec.Export {
		t.Fatalf("export spec = %+v, %v", spec, ok)
	}

	// Unchanged store: the stat gate skips the parse.
	polls := f.a.opencode.polls.Load()
	f.a.opencode.last = time.Time{}
	f.a.pollStore(ctx, &f.a.opencode, false, false)
	if f.a.opencode.polls.Load() != polls {
		t.Error("unchanged store re-parsed")
	}

	oc.Exec(`DELETE FROM part`)
	oc.Exec(`DELETE FROM message`)
	oc.Exec(`DELETE FROM session`)
	f.a.opencode.last = time.Time{}
	f.a.pollStore(ctx, &f.a.opencode, false, false)
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	live := `SELECT count(*) FROM messages m JOIN conversations c ON c.id = m.conversation_id WHERE c.session_id = ? AND m.superseded = 0`
	if n := f.count(live, ses); n != 0 {
		t.Errorf("%d rows of the deleted session still live", n)
	}
}

// opencode: a message the plugin delivered (part metadata flopwire.id)
// gives a read sighting; the same wrapper typed as a prompt does not.
func TestReadSightingsOpencode(t *testing.T) {
	oc := opencodetest.New(t, "")
	const ses = "ses_synthetic00000000000002"
	t0 := opencodetest.T0
	oc.Session(ses, "", "/work/opencode-demo", "Receipts", t0)
	oc.Prompt(ses, t0, "hello")
	f := newFixture(t, "-")
	f.cfg.OpencodeDB = oc.Path
	f.a = New(f.store, f.cfg)
	l := captureReads(f)
	f.once()
	if r := l.take(); len(r) != 0 {
		t.Fatalf("sightings before delivery: %+v", r)
	}
	deliver := func(ms int64, text, metadata string) {
		msg, part := opencodetest.ID("msg", ms, 1), opencodetest.ID("prt", ms, 2)
		oc.Message(msg, ses, ms, `{"role":"user"}`)
		oc.Part(part, msg, ses, ms, `{"type":"text","text":`+jsonStr(text)+metadata+`}`)
	}
	deliver(t0+5000, hookText(false, "mhook1000000000"), `,"metadata":{"flopwire":{"id":"mhook1000000000"}}`)
	deliver(t0+6000, hookText(false, "muser0000000000"), ``)
	f.a.pollStore(ctx, &f.a.opencode, true, true)
	got := l.take()
	if !slices.Equal(readIDs(got), []string{"mhook1000000000"}) {
		t.Fatalf("sightings %v, want the delivered one", readIDs(got))
	}
	if r := got[0]; r.Session != ses || r.Agent != "opencode" || !r.At.Equal(time.UnixMilli(t0+5000)) {
		t.Fatalf("sighting %+v", r)
	}
}

// "-" turns the opencode store off even when the environment names one:
// nothing is indexed, watched or handed to sync.
func TestOpencodeDisabled(t *testing.T) {
	oc := opencodetest.New(t, "")
	const ses = "ses_synthetic00000000000003"
	oc.Session(ses, "", "/work/opencode-demo", "Off", opencodetest.T0)
	oc.Prompt(ses, opencodetest.T0, "the disabled parrot")
	t.Setenv(opencode.EnvDB, oc.Path)
	f := newFixture(t, "-")
	if f.cfg.OpencodeDB != "-" {
		t.Fatalf("fixture opencode db = %q", f.cfg.OpencodeDB)
	}
	f.a = New(f.store, f.cfg)
	f.once()
	f.a.pollStores(ctx, true, true)
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT count(*) FROM conversations WHERE agent = 'opencode'`); n != 0 {
		t.Fatalf("%d opencode conversations indexed with the store off", n)
	}
	if _, ok := f.rec.spec(opencode.ExportPath(oc.Path, ses)); ok {
		t.Fatal("an opencode export was handed to sync with the store off")
	}
	if f.a.storeOf(transcript.AgentOpencode) != nil {
		t.Fatal("the opencode store is polled")
	}
	for _, d := range f.a.watchDirs(time.Now()) {
		if d == filepath.Dir(oc.Path) {
			t.Fatal("the opencode directory is watched")
		}
	}
}

// opencode presence comes from the plugin's registry: a session its
// running opencode names is live and busy as its last plugin event says;
// a session the file no longer names (deleted in opencode) or a file of a
// dead or reused pid is not live, however recently the session wrote.
func TestPresenceOpencode(t *testing.T) {
	oc := opencodetest.New(t, "")
	const a, b = "ses_synthetic0000000000000A", "ses_synthetic0000000000000B"
	t0 := opencodetest.T0
	for i, s := range []string{a, b} {
		oc.Session(s, "", "/work/opencode-demo", "Presence", t0)
		oc.Prompt(s, t0+int64(i), "hello from "+s)
	}
	f := newFixture(t, "-")
	f.cfg.OpencodeDB = oc.Path
	f.cfg.OpencodeRegistry = filepath.Join(t.TempDir(), "opencode")
	clock := new(time.Time)
	bus, err := devicebus.Open(filepath.Join(t.TempDir(), "bus.db"), devicebus.Config{User: "gary", Logger: f.cfg.Logger, Now: func() time.Time { return *clock }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bus.Close() })
	f.cfg.Bus = bus
	f.a = New(f.store, f.cfg)
	f.once()
	start := time.UnixMilli(t0 - 3600_000)
	alive, name := true, "opencode"
	f.a.pidAlive = func(pid int) bool { return pid == 6161 && alive }
	f.a.procName = func(int) string { return name }
	f.a.procStart = func(int) (time.Time, bool) { return start, true }
	reg := filepath.Join(f.cfg.OpencodeRegistry, "6161.json")
	write := func(sessions ...string) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"pid": 6161, "started": start.UnixMilli(), "sessions": sessions})
		writeFile(t, reg, string(b))
	}
	at := time.UnixMilli(t0).Add(time.Minute)

	write(a, b)
	if !f.present(clock, at, a) || !f.present(clock, at, b) {
		t.Fatal("sessions the registry names are not live")
	}
	ask(t, f.a, Request{Op: "flush", Session: a, Agent: "opencode", Event: "PreToolUse", HookStart: at.UnixMilli()})
	if s := f.presence(at)[a]; !s.Busy {
		t.Fatalf("busy after a PreToolUse: %+v", s)
	}
	// B deleted in opencode: the plugin drops it from the file.
	write(a)
	if f.present(clock, at.Add(time.Second), b) {
		t.Fatal("a session the registry no longer names is live")
	}
	if !f.present(clock, at.Add(time.Second), a) {
		t.Fatal("the remaining session is not live")
	}
	// The pid now runs another program.
	name = "zsh"
	if f.present(clock, at.Add(2*time.Second), a) {
		t.Fatal("a session of a reused pid is live")
	}
}

// The plugin records performance.timeOrigin as its process's start. In the
// TUI the plugin runs in a worker that starts after the process (0.6-0.8s
// measured on opencode 1.18.30, more on a loaded machine), so the process
// can have started well before the recorded time; its sessions are live.
// A pid that started after the recorded time is another process that
// reused it.
func TestPresenceOpencodeWorkerStart(t *testing.T) {
	oc := opencodetest.New(t, "")
	const a = "ses_synthetic0000000000000A"
	t0 := opencodetest.T0
	oc.Session(a, "", "/work/opencode-demo", "Presence", t0)
	oc.Prompt(a, t0, "hello")
	f := newFixture(t, "-")
	f.cfg.OpencodeDB = oc.Path
	f.cfg.OpencodeRegistry = filepath.Join(t.TempDir(), "opencode")
	clock := new(time.Time)
	bus, err := devicebus.Open(filepath.Join(t.TempDir(), "bus.db"), devicebus.Config{User: "gary", Logger: f.cfg.Logger, Now: func() time.Time { return *clock }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bus.Close() })
	f.cfg.Bus = bus
	f.a = New(f.store, f.cfg)
	f.once()
	recorded := time.UnixMilli(t0 - 3600_000)
	start := recorded.Add(-5 * time.Second) // the worker started 5s after the process
	f.a.pidAlive = func(pid int) bool { return pid == 6161 }
	f.a.procName = func(int) string { return "opencode" }
	f.a.procStart = func(int) (time.Time, bool) { return start, true }
	b, _ := json.Marshal(map[string]any{"pid": 6161, "started": recorded.UnixMilli(), "sessions": []string{a}})
	writeFile(t, filepath.Join(f.cfg.OpencodeRegistry, "6161.json"), string(b))
	at := time.UnixMilli(t0).Add(time.Minute)
	if !f.present(clock, at, a) {
		t.Fatal("a session of an opencode whose plugin worker started 5s after the process is not live")
	}
	start = recorded.Add(10 * time.Second) // a later process reused the pid
	if f.present(clock, at.Add(time.Second), a) {
		t.Fatal("a session of a pid that started after the plugin recorded its start is live")
	}
}
