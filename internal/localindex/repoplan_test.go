package localindex

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/transcript"
)

// #125 review: an index of the previous layout (12) is rebuilt with the
// placements indexes a --repo filter needs, its placements carried, and
// the filter's plan finds a repository's sessions through those indexes,
// not by scanning placements.
func TestRepoFilterUsesPlacementIndexes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := Placement{Agent: transcript.AgentClaude, SessionID: "s1", How: PlacedByWorktree,
		Placement: pathpolicy.Placement{Cwd: "/w/app-wt", Worktree: "/w/app-wt", Main: "/w/app", Remote: "github.com/acme/app"}}
	if err := s.SavePlacement(ctx, want); err != nil {
		t.Fatal(err)
	}
	s.write(ctx, func(w *writeTx) error {
		for _, q := range []string{`DROP INDEX placements_main`, `DROP INDEX placements_remote`, `PRAGMA user_version = 12`} {
			if _, err := w.tx.Exec(q); err != nil {
				return err
			}
		}
		return nil
	})
	s.Close()
	s, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, err := s.Placements(ctx); err != nil || len(got) != 1 || got[0] != want {
		t.Fatalf("placements after the rebuild: %+v %v", got, err)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name IN ('placements_main','placements_remote')`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("placements indexes after the rebuild: %d %v", n, err)
	}
	f := Filter{RepoMains: []string{"/w/app"}, RepoRemotes: []string{"github.com/acme/app"}, IncludeSuperseded: true, IncludeBranches: true}
	where, args := f.where()
	rows, err := s.DB().Query("EXPLAIN QUERY PLAN SELECT c.id FROM conversations c WHERE "+where, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var d string
		if err := rows.Scan(&id, &parent, &unused, &d); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, d)
	}
	all := strings.Join(plan, "\n")
	for _, use := range []string{"USING INDEX placements_main (main_root=?)", "USING INDEX placements_remote (remote=?)"} {
		if !strings.Contains(all, use) {
			t.Fatalf("plan does not use %s:\n%s", use, all)
		}
	}
	if strings.Contains(all, "SCAN p") || strings.Contains(all, "SCAN rc") {
		t.Fatalf("plan scans placements or conversations:\n%s", all)
	}
}
