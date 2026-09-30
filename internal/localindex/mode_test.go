package localindex

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/transcript"
)

func shardFiles(t *testing.T, path string) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if n := e.Name(); strings.HasPrefix(n, filepath.Base(path)+"-tok") || strings.HasPrefix(n, filepath.Base(path)+"-tri") {
			out = append(out, n)
		}
	}
	return out
}

func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A sync-only index keeps sources, watermarks, conversations and
// placements but no message rows and no FTS shards; read-only opens fail
// with ErrSyncOnly; switching modes rebuilds the index, keeping
// placements.
func TestSyncOnlyModeAndSwitch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	pl := Placement{Agent: transcript.AgentClaude, SessionID: "s1", How: PlacedByCwd,
		Placement: pathpolicy.Placement{Cwd: "/w/app"}}
	batch := func(s *Store) {
		src := source(t, s, transcript.AgentClaude, "/p/a.jsonl")
		wm := transcript.Watermark{Offset: 300, LineNo: 3}
		b := Batch{SourceID: src.ID, Generation: 1, NewGeneration: &transcript.Generation{Generation: 1, Size: 300},
			Conversations: []*transcript.Conversation{{SessionID: "s1", Cwd: "/w/app"}},
			Messages:      []*transcript.Message{msg("s1", "m1", 0, transcript.KindUser, "needle one"), msg("s1", "m2", 1, transcript.KindAssistant, "needle two")},
			Watermark:     &wm}
		apply(t, s, b)
		if err := s.Sync(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// Sync-only from the start.
	s, err := Open(path, Options{SyncOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	batch(s)
	if err := s.SavePlacement(ctx, pl); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s, "messages"); n != 0 {
		t.Fatalf("sync-only: %d message rows", n)
	}
	if n := countRows(t, s, "conversations"); n != 1 {
		t.Fatalf("sync-only: %d conversations", n)
	}
	srcs, err := s.ListSources(ctx, "", "")
	if err != nil || len(srcs) != 1 || srcs[0].Watermark == nil || srcs[0].Watermark.Offset != 300 {
		t.Fatalf("sync-only: sources %+v err %v", srcs, err)
	}
	if fs := shardFiles(t, path); len(fs) != 0 {
		t.Fatalf("sync-only: shard files %v", fs)
	}
	s.Close()
	if _, err := Open(path, Options{ReadOnly: true}); !errors.Is(err, ErrSyncOnly) || err.Error() != "this device is sync-only; use --server" {
		t.Fatalf("read-only open of a sync-only index: %v", err)
	}

	// Reopened sync-only: nothing rebuilt.
	s, err = Open(path, Options{SyncOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s, "sources"); n != 1 {
		t.Fatalf("sync-only reopen dropped sources: %d", n)
	}
	s.Close()

	// Switch to full: the index is rebuilt empty (the agent re-parses every
	// source from the start), placements are kept, shards exist.
	s, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s, "sources"); n != 0 {
		t.Fatalf("switch to full kept %d sources: nothing would be re-parsed", n)
	}
	if got, err := s.Placements(ctx); err != nil || len(got) != 1 || got[0].SessionID != "s1" {
		t.Fatalf("switch to full: placements %+v %v", got, err)
	}
	batch(s)
	if n := countRows(t, s, "messages"); n != 2 {
		t.Fatalf("full: %d message rows", n)
	}
	if ids := findIDs(t, s, "needle", FindOptions{}); len(ids) != 2 {
		t.Fatalf("full: find %v", ids)
	}
	if fs := shardFiles(t, path); len(fs) == 0 {
		t.Fatal("full: no shard files")
	}
	s.Close()
	ro, err := Open(path, Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("read-only open of a full index: %v", err)
	}
	ro.Close()

	// Back to sync-only: message rows and shards are dropped, placements kept.
	s, err = Open(path, Options{SyncOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if n := countRows(t, s, "messages"); n != 0 {
		t.Fatalf("switch to sync-only kept %d message rows", n)
	}
	if fs := shardFiles(t, path); len(fs) != 0 {
		t.Fatalf("switch to sync-only kept shard files %v", fs)
	}
	if got, err := s.Placements(ctx); err != nil || len(got) != 1 {
		t.Fatalf("switch to sync-only: placements %+v %v", got, err)
	}
}
