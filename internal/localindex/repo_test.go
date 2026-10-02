package localindex

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRun runs git in dir, failing the test on error.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func realTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// ResolveRepo reads the git files: a linked worktree resolves to its own
// root, the main checkout's root and the origin remote, as git reports
// them; a worktree of a bare repository resolves to the bare directory.
func TestResolveRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	base := realTemp(t)
	main := filepath.Join(base, "app")
	os.MkdirAll(filepath.Join(main, "src"), 0o755)
	gitRun(t, main, "init", "-q")
	gitRun(t, main, "remote", "add", "upstream", "https://github.com/other/app.git")
	gitRun(t, main, "remote", "add", "origin", "git@github.com:Acme/App.git")
	gitRun(t, main, "commit", "-q", "--allow-empty", "-m", "init")
	wt := filepath.Join(base, "app-fix-thing")
	gitRun(t, main, "worktree", "add", "-q", wt)

	bare := filepath.Join(base, "lib.git")
	gitRun(t, base, "clone", "-q", "--bare", main, bare)
	gitRun(t, bare, "remote", "set-url", "origin", "https://gitlab.com/acme/lib.git")
	bwt := filepath.Join(base, "lib-main")
	gitRun(t, bare, "worktree", "add", "-q", bwt)

	cases := []struct {
		cwd  string
		want Repo
	}{
		{filepath.Join(main, "src"), Repo{main, main, "github.com/Acme/App"}},
		{wt, Repo{wt, main, "github.com/Acme/App"}},
		{filepath.Join(wt, "gone", "deeper"), Repo{wt, main, "github.com/Acme/App"}}, // a directory that does not exist
		{bwt, Repo{bwt, bare, "gitlab.com/acme/lib"}},
		{base, Repo{}},
		{"", Repo{}},
	}
	for _, c := range cases {
		if got := ResolveRepo(c.cwd); got != c.want {
			t.Errorf("ResolveRepo(%s) = %+v, want %+v", c.cwd, got, c.want)
		}
	}
	// Agrees with git.
	for _, dir := range []string{wt, bwt} {
		top := gitRun(t, dir, "rev-parse", "--show-toplevel")
		if got := ResolveRepo(dir).Worktree; got != top {
			t.Errorf("worktree %s, git says %s", got, top)
		}
	}
}

func TestRemoteURLFallsBackToFirstRemote(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config")
	os.WriteFile(cfg, []byte("[core]\n\tbare = false\n[remote \"fork\"]\n\turl = git@github.com:me/x.git\n\tfetch = +refs/heads/*:refs/remotes/fork/*\n"), 0o600)
	if got := remoteURL(cfg); got != "git@github.com:me/x.git" {
		t.Fatalf("remote %q", got)
	}
}

// Worktrees lists a repository's live linked worktrees from its git
// files, for a main checkout and for a bare repository; RepoName drops a
// bare directory's ".git" and leading dot.
func TestWorktreesAndRepoName(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	base := realTemp(t)
	main := filepath.Join(base, "app")
	os.MkdirAll(main, 0o755)
	gitRun(t, main, "init", "-q")
	gitRun(t, main, "commit", "-q", "--allow-empty", "-m", "init")
	wt := filepath.Join(base, "app-x")
	gitRun(t, main, "worktree", "add", "-q", wt)
	bare := filepath.Join(base, ".lib.git")
	gitRun(t, base, "clone", "-q", "--bare", main, bare)
	bwt := filepath.Join(base, "lib")
	gitRun(t, bare, "worktree", "add", "-q", bwt)
	if got := Worktrees(main); len(got) != 1 || got[0] != wt {
		t.Fatalf("Worktrees(main) = %v", got)
	}
	if got := Worktrees(bare); len(got) != 1 || got[0] != bwt {
		t.Fatalf("Worktrees(bare) = %v", got)
	}
	if got := Worktrees(base); got != nil {
		t.Fatalf("Worktrees(not a repository) = %v", got)
	}
	if !IsBare(bare) || IsBare(main) || IsBare(base) {
		t.Fatal("IsBare")
	}
	for _, c := range [][3]string{{bare, "", "lib"}, {"/x/lib.git", "", "lib"}, {main, "", "app"}, {main, "github.com/acme/web", "web"}, {"/x/.git", "", ".git"}} {
		if got := RepoName(c[0], c[1]); got != c[2] {
			t.Errorf("RepoName(%s, %s) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}
