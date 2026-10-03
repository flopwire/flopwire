package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/pathpolicy"
)

// #102: a session whose working directory is gone, under a home
// directory that is itself a git repository (dotfiles), does not take
// the home repository's identity. It used to: ResolveRepo walked up to
// the home repository, whose remote replaced the one the session
// recorded, so --repo <the deleted directory> named the whole home
// directory. The placement keeps the directory and the recorded remote,
// and the recovery pass looks for its checkout by evidence.
func TestResolveDeletedCwdUnderDotfilesHome(t *testing.T) {
	home := t.TempDir()
	gitRun(t, home, "init", "-q")
	gitRun(t, home, "remote", "add", "origin", "git@github.com:me/dotfiles.git")
	gone := filepath.Join(home, "code", "app-wt")
	a := &Agent{}
	got := a.resolve(gone, "github.com/acme/app")
	want := pathpolicy.Placement{Cwd: gone, Remote: "github.com/acme/app"}
	if got != want {
		t.Fatalf("deleted cwd under a dotfiles home: %+v, want %+v", got, want)
	}
	if got := a.resolve(gone, ""); got != (pathpolicy.Placement{Cwd: gone}) {
		t.Fatalf("deleted cwd, no recorded remote: %+v", got)
	}
	// A directory that exists is placed by git as before.
	live := filepath.Join(home, "notes")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := a.resolve(live, ""); got.Main != home || got.Remote != "github.com/me/dotfiles" {
		t.Fatalf("live cwd under the home repository: %+v", got)
	}
}
