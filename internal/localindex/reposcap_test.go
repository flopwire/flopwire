package localindex

import (
	"fmt"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

// #102: a repo filter over more checkout roots than one SQLite statement
// can bind (6 variables a root; SQLite's limit is 32766, so 5462 roots)
// is capped at MaxRepos, the first roots kept, instead of failing with
// "too many SQL variables". A repository whose placements name thousands
// of directories expands to that many roots (local.ExpandRepo).
func TestRepoFilterPastTheCap(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/h/c.jsonl")
	apply(t, s, Batch{SourceID: src.ID, Generation: 1,
		Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "s", Cwd: "/repo/a/sub"}},
		Messages:      []*transcript.Message{msg("s", "m1", 0, transcript.KindUser, "omega")}})
	for _, n := range []int{MaxRepos + 1, 32766/6 + 1} {
		repos := []string{"/repo/a"}
		for i := 1; i < n; i++ {
			repos = append(repos, fmt.Sprintf("/wt/r%05d", i))
		}
		eq(t, fmt.Sprintf("%d roots", n), searchIDs(t, s, "omega", SearchOptions{Filter: Filter{Repos: repos}}), []string{"m1"})
	}
}
