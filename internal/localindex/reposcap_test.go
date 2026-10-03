package localindex

import (
	"fmt"
	"testing"

	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/transcript"
)

// #102: a repo filter of MaxRepos directories runs (each is one OR term,
// and SQLite refuses an expression deeper than 1000), and one of
// thousands of main checkouts and remotes runs too: they are bound as
// JSON arrays and matched through the placements' indexes.
func TestRepoFilterAtTheCap(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/h/c.jsonl")
	apply(t, s, Batch{SourceID: src.ID, Generation: 1,
		Conversations: []*transcript.Conversation{
			{Agent: transcript.AgentClaude, SessionID: "s", Cwd: "/repo/a/sub"},
			{Agent: transcript.AgentClaude, SessionID: "p", Cwd: "/w/gone-wt"}},
		Messages: []*transcript.Message{msg("s", "m1", 0, transcript.KindUser, "omega"), msg("p", "m2", 1, transcript.KindUser, "omega")}})
	if err := s.SavePlacement(ctx, Placement{Agent: transcript.AgentClaude, SessionID: "p", How: PlacedByWorktree}); err != nil {
		t.Fatal(err)
	}
	repos := []string{"/repo/a"}
	for i := 1; i < MaxRepos; i++ {
		repos = append(repos, fmt.Sprintf("/wt/r%05d", i))
	}
	eq(t, "MaxRepos roots", searchIDs(t, s, "omega", SearchOptions{Filter: Filter{Repos: repos}}), []string{"m1"})
	var mains []string
	for i := range 5000 {
		mains = append(mains, fmt.Sprintf("/c/web-%05d", i))
	}
	if err := s.SavePlacement(ctx, Placement{Agent: transcript.AgentClaude, SessionID: "p", How: PlacedByWorktree,
		Placement: pathpolicy.Placement{Cwd: "/w/gone-wt", Worktree: "/w/gone-wt", Main: "/c/web-04999", Remote: "github.com/acme/web"}}); err != nil {
		t.Fatal(err)
	}
	eq(t, "5000 mains", searchIDs(t, s, "omega", SearchOptions{Filter: Filter{RepoMains: mains}}), []string{"m2"})
	eq(t, "by remote", searchIDs(t, s, "omega", SearchOptions{Filter: Filter{RepoRemotes: []string{"github.com/acme/web"}}}), []string{"m2"})
	eq(t, "another remote", searchIDs(t, s, "omega", SearchOptions{Filter: Filter{RepoRemotes: []string{"github.com/acme/other"}}}), nil)
}
