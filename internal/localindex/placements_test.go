package localindex

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/transcript"
)

// Placements survive a schema bump: a session whose worktree is gone
// cannot be placed again from its transcript after the rebuild.
func TestPlacementsSurviveSchemaBump(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := Placement{Agent: transcript.AgentClaude, SessionID: "s1", How: PlacedByWorktree,
		Placement: pathpolicy.Placement{Cwd: "/w/app-wt/src", Worktree: "/w/app-wt", Main: "/w/app", Remote: "github.com/acme/app"}}
	if err := s.SavePlacement(ctx, want); err != nil {
		t.Fatal(err)
	}
	// The previous layout: no checked_at, an older version.
	s.write(ctx, func(w *writeTx) error {
		if _, err := w.tx.Exec(`ALTER TABLE placements DROP COLUMN checked_at`); err != nil {
			return err
		}
		_, err := w.tx.Exec(`PRAGMA user_version = 5`)
		return err
	})
	s.Close()

	s, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Placements(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("placements after the rebuild: %+v", got)
	}
	var v int
	s.DB().QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != schemaVersion {
		t.Fatalf("user_version %d", v)
	}
	var old int
	s.DB().QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'placements_old'`).Scan(&old)
	if old != 0 {
		t.Fatal("the set-aside table was left behind")
	}
}
