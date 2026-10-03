package agent

import (
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
