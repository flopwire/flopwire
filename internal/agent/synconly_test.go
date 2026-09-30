package agent

import (
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/localindex"
)

// A sync-only agent records sources and watermarks and hands every
// transcript to sync, but writes no message rows; restarting it in full
// mode builds the local index from the transcripts.
func TestSyncOnlyAgentAndSwitchToFull(t *testing.T) {
	home := t.TempDir()
	copyTree(t, oracleHome, home)
	dbPath := filepath.Join(t.TempDir(), "index.db")
	rec := newRecorder()
	cfg := Config{ClaudeProjects: filepath.Join(home, ".claude", "projects"), CodexHome: filepath.Join(home, ".codex"),
		DevinDB: "-", Workers: 3, Sync: rec, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	count := func(s *localindex.Store, q string) int {
		t.Helper()
		var n int
		if err := s.DB().QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	store, err := localindex.Open(dbPath, localindex.Options{DeferCommit: true, SyncOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := New(store, cfg).Once(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(store, `SELECT count(*) FROM messages`); n != 0 {
		t.Fatalf("sync-only: %d message rows", n)
	}
	wms := count(store, `SELECT count(*) FROM sources WHERE wm_offset IS NOT NULL`)
	if wms < 8 {
		t.Fatalf("sync-only: %d sources with a watermark", wms)
	}
	if n := count(store, `SELECT count(*) FROM conversations`); n == 0 {
		t.Fatal("sync-only: no conversations")
	}
	if n := count(store, `SELECT count(*) FROM placements`); n == 0 {
		t.Fatal("sync-only: no placements")
	}
	if _, ok := rec.spec(filepath.Join(home, alphaRel)); !ok {
		t.Fatal("sync-only: the Claude transcript was not handed to sync")
	}
	// A restart parses nothing that did not change.
	waitRacy()
	a := New(store, cfg)
	if err := a.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if n := a.Stats().Indexed.Load(); n != 0 {
		t.Fatalf("sync-only restart re-parsed %d sources", n)
	}
	store.Close()
	if _, err := localindex.Open(dbPath, localindex.Options{ReadOnly: true}); err != localindex.ErrSyncOnly {
		t.Fatalf("read-only open: %v", err)
	}

	// Switch to full: everything is parsed and searchable.
	store, err = localindex.Open(dbPath, localindex.Options{DeferCommit: true})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a = New(store, cfg)
	if err := a.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if n := a.Stats().Indexed.Load(); n < int64(wms) {
		t.Fatalf("switch to full parsed %d sources, want at least %d", n, wms)
	}
	for _, agent := range []string{"claude", "codex"} {
		var n int
		if err := store.DB().QueryRow(`SELECT count(*) FROM messages m JOIN sources s ON s.id = m.source_id WHERE s.agent = ?`, agent).Scan(&n); err != nil || n == 0 {
			t.Errorf("full: %d %s rows (%v)", n, agent, err)
		}
	}
	hits, err := store.Find(ctx, "oracle", localindex.FindOptions{})
	if err != nil || len(hits) == 0 {
		t.Fatalf("full: find: %d hits, %v", len(hits), err)
	}
}

// Path rules apply in sync-only mode as in full mode: a denied session and
// a local one are never handed to sync.
func TestSyncOnlyAppliesPathRules(t *testing.T) {
	f := newFixture(t, "-")
	f.store.Close()
	store, err := localindex.Open(filepath.Join(t.TempDir(), "index.db"), localindex.Options{DeferCommit: true, SyncOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	f.store = store
	f.cfg.UserRuleList = []string{"deny /tmp/oracle-alpha", "local:/tmp/oracle-beta"}
	f.a = New(store, f.cfg)
	f.once()
	f.rec.mu.Lock()
	defer f.rec.mu.Unlock()
	if len(f.rec.notify) == 0 {
		t.Fatal("nothing handed to sync")
	}
	for p := range f.rec.notify {
		if strings.Contains(p, "-tmp-oracle-alpha") || strings.Contains(p, "-tmp-oracle-beta") {
			t.Errorf("sync-only agent handed a %s session to sync", p)
		}
	}
}
