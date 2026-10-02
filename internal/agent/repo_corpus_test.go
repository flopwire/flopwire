package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/flopwire/flopwire/internal/transcript/codex"
)

// TestCorpusRepoKeys reads the directory every real Claude and Codex
// transcript names (read-only, the first lines only) and reports, in
// numbers only, how --repo groups the sessions: how many ran in a linked
// worktree or a bare-backed worktree (their repository's main checkout
// is another directory, so --repo from the main checkout now finds them),
// how many checkouts collapse into how many repositories, and how many
// repository names are shared by two or more repositories (a bare name
// is then ambiguous).
//
//	FLOPWIRE_CORPUS=1 go test -run TestCorpusRepoKeys -v ./internal/agent/
func TestCorpusRepoKeys(t *testing.T) {
	if os.Getenv("FLOPWIRE_CORPUS") == "" {
		t.Skip("set FLOPWIRE_CORPUS=1")
	}
	home, _ := os.UserHomeDir()
	var paths []string
	sessions, err := claude.Discover(claude.ProjectsRoot(os.Getenv, home))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		if s.Transcript != "" {
			paths = append(paths, s.Transcript)
		}
	}
	cs, err := codex.Discover(codex.Home())
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range cs {
		paths = append(paths, x.Path)
	}
	var named, inGit, moved, gone int
	checkouts, repos := map[string]bool{}, map[string]string{}
	for _, p := range paths {
		h := scanHints(p)
		if h.cwd == "" || !filepath.IsAbs(h.cwd) {
			continue
		}
		named++
		if _, err := os.Stat(h.cwd); err != nil {
			gone++
		}
		r := localindex.ResolveRepo(h.cwd)
		if r.Main == "" {
			continue
		}
		inGit++
		main, err := filepath.EvalSymlinks(r.Main)
		if err != nil {
			main = filepath.Clean(r.Main)
		}
		if wt, err := filepath.EvalSymlinks(r.Worktree); err == nil && wt != main || err != nil && r.Worktree != r.Main {
			moved++
		}
		checkouts[r.Worktree] = true
		repos[main] = localindex.RepoName(main, r.Remote)
	}
	byName := map[string]int{}
	for _, n := range repos {
		byName[n]++
	}
	shared := 0
	for _, n := range byName {
		if n > 1 {
			shared++
		}
	}
	t.Logf("transcripts %d, naming a directory %d (gone %d), in git %d", len(paths), named, gone, inGit)
	t.Logf("sessions whose checkout is not the main checkout (now matched from it): %d", moved)
	t.Logf("checkouts %d -> repositories %d; names shared by 2+ repositories: %d", len(checkouts), len(repos), shared)
}
