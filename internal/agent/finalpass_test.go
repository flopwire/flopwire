package agent

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/localindex"
)

// Sync-only x strict placement x withhold: on a sync-only device (no
// message rows), a session already uploaded that later names a denied
// directory is still withheld: the server deletes it, nothing is owed,
// and the session is not handed to sync again.
func TestSyncOnlyDenyMidSessionWithholds(t *testing.T) {
	s := newWithholdServer(t)
	f, _, _ := rulesFixture(t, "-", "deny /tmp/oracle-secret")
	f.store.Close()
	store, err := localindex.Open(filepath.Join(t.TempDir(), "index.db"), localindex.Options{DeferCommit: true, SyncOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	f.store = store
	f.cfg.Withhold = WithholdSession(s.url, s.token, s.h.Client())
	f.a = New(f.store, f.cfg)
	f.once()
	if n := f.count(`SELECT count(*) FROM messages`); n != 0 {
		t.Fatalf("sync-only index holds %d message rows", n)
	}
	s.uploadAlpha(f, true)
	if n := s.alphaConvs(); n < 2 {
		t.Fatalf("server holds %d conversations of the alpha session", n)
	}
	f.rec.mu.Lock()
	for p := range f.rec.notify {
		delete(f.rec.notify, p)
	}
	f.rec.mu.Unlock()

	appendFile(t, f.path(alphaRel), claudeUserAt("/tmp/oracle-secret/x", "c9000000-0000-4000-8000-000000000021", "denied later, sync-only"))
	f.once()

	if n := s.alphaConvs(); n != 0 {
		t.Errorf("server still holds %d conversations of a session denied mid-session", n)
	}
	if w := f.withhold(); w != "" {
		t.Errorf("deletion still owed: %q", w)
	}
	if n := s.count(`SELECT count(*) FROM audit_events WHERE action='conversation.withheld'`); n != 1 {
		t.Errorf("conversation.withheld audit events: %d", n)
	}
	// The next pass (the verdict is known before any file of the session
	// is looked at) hands nothing of it to sync.
	f.rec.mu.Lock()
	for p := range f.rec.notify {
		delete(f.rec.notify, p)
	}
	f.rec.mu.Unlock()
	waitRacy()
	f.once()
	f.rec.mu.Lock()
	defer f.rec.mu.Unlock()
	for p := range f.rec.notify {
		if strings.Contains(p, alphaID) {
			t.Errorf("the denied session was handed to sync again: %s", p)
		}
	}
}
