package agent

import (
	"slices"
	"testing"
	"time"

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
