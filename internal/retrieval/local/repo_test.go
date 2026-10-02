package local

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/local/localtest"
	"github.com/flopwire/flopwire/internal/transcript"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// repoWorld is a set of synthetic repositories in every layout, each with
// Claude Code sessions indexed and placed as the device agent places them.
type repoWorld struct {
	b    *Backend
	base string // t.TempDir(), not symlink-resolved (on macOS /var is /private/var)
	ids  map[string]string
}

// session ids by name; the name is also the session's grep marker.
var worldSessions = []struct{ name, dir string }{
	{"appmain", "app"},
	{"appsub", "app/sub"},
	{"appwt", "app-api"},
	{"appgone", "app-old"},
	{"otherapp", "other/app"},
	{"webwt", "web-fix"},
	{"webclone", "elsewhere/web2"},
	{"libmain", "lib"},
	{"libwt", "lib-feat"},
	{"plain", "notes"},
}

func newRepoWorld(t *testing.T) *repoWorld {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	base := t.TempDir()
	p := func(s string) string { return filepath.Join(base, s) }
	for _, d := range []string{"app/sub", "other/app", "web", "elsewhere", "notes"} {
		if err := os.MkdirAll(p(d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// app: no remote, a linked worktree, a worktree deleted later.
	git(t, p("app"), "init", "-q")
	git(t, p("app"), "commit", "-q", "--allow-empty", "-m", "init")
	git(t, p("app"), "worktree", "add", "-q", "-b", "api-cursors", p("app-api"))
	git(t, p("app"), "worktree", "add", "-q", "-b", "old", p("app-old"))
	// other/app: a different repository with the same name, no remote.
	git(t, p("other/app"), "init", "-q")
	// web: a remote, a linked worktree, and a second clone of the same remote.
	git(t, p("web"), "init", "-q")
	git(t, p("web"), "remote", "add", "origin", "git@github.com:acme/web.git")
	git(t, p("web"), "commit", "-q", "--allow-empty", "-m", "init")
	git(t, p("web"), "worktree", "add", "-q", "-b", "fix", p("web-fix"))
	git(t, p("elsewhere"), "clone", "-q", p("web"), p("elsewhere/web2"))
	git(t, p("elsewhere/web2"), "remote", "set-url", "origin", "https://github.com/acme/web")
	// lib: the layout this repository uses, a bare repository and worktrees
	// beside it, with no main checkout.
	git(t, base, "clone", "-q", "--bare", p("app"), p(".lib.git"))
	git(t, p(".lib.git"), "remote", "remove", "origin")
	git(t, p(".lib.git"), "worktree", "add", "-q", p("lib"))
	git(t, p(".lib.git"), "worktree", "add", "-q", "-b", "feat", p("lib-feat"))

	home := filepath.Join(t.TempDir(), "home")
	w := &repoWorld{base: base, ids: map[string]string{}}
	for i, s := range worldSessions {
		id := fmt.Sprintf("0b7e2c1a-0000-4000-8000-%012d", i+1)
		w.ids[s.name] = id
		cwd := p(s.dir)
		var lines []byte
		for j, m := range []map[string]any{
			{"type": "user", "message": map[string]any{"role": "user", "content": "zebra " + s.name}},
			{"type": "assistant", "message": map[string]any{"id": "msg" + s.name, "type": "message", "role": "assistant", "model": "m", "content": []map[string]any{{"type": "text", "text": "ok " + s.name}}}},
		} {
			m["cwd"], m["sessionId"], m["uuid"] = cwd, id, fmt.Sprintf("d1000000-0000-4000-8000-%06d%06d", i, j)
			m["timestamp"] = fmt.Sprintf("2026-09-23T11:%02d:%02d.000Z", i, j)
			if j > 0 {
				m["parentUuid"] = fmt.Sprintf("d1000000-0000-4000-8000-%06d%06d", i, j-1)
			}
			b, _ := json.Marshal(m)
			lines = append(append(lines, b...), '\n')
		}
		dir := filepath.Join(home, ".claude", "projects", fmt.Sprintf("-p-%d", i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), lines, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := localindex.Open(filepath.Join(t.TempDir(), "index.db"), localindex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := localtest.IndexHome(ctx, s, home); err != nil {
		t.Fatal(err)
	}
	// Place each session as the agent does when it first sees it.
	for _, x := range worldSessions {
		cwd := p(x.dir)
		r := localindex.ResolveRepo(cwd)
		how := localindex.PlacedByCwd
		if r.Worktree != r.Main {
			how = localindex.PlacedByWorktree
		}
		pl := localindex.Placement{Agent: transcript.AgentClaude, SessionID: w.ids[x.name], How: how,
			Placement: pathpolicy.Placement{Cwd: cwd, Worktree: r.Worktree, Main: r.Main, Remote: r.Remote}}
		if err := s.SavePlacement(ctx, pl); err != nil {
			t.Fatal(err)
		}
	}
	// The app-old worktree is deleted after its session was placed.
	git(t, p("app"), "worktree", "remove", "--force", p("app-old"))
	w.b = &Backend{Store: s}
	return w
}

// sessions lists the names of the sessions f keeps.
func (w *repoWorld) sessions(t *testing.T, f format.Filters) (string, error) {
	t.Helper()
	got, err := w.b.Sessions(ctx, "", "", f)
	if err != nil {
		return "", err
	}
	names := map[string]string{}
	for k, v := range w.ids {
		names[v] = k
	}
	var out []string
	for _, c := range got.Sessions {
		out = append(out, names[c.SessionID])
	}
	slices.Sort(out)
	return strings.Join(out, ","), nil
}

const (
	appRepo = "appgone,appmain,appsub,appwt"
	webRepo = "webclone,webwt"
	libRepo = "libmain,libwt"
)

// #81: --repo . from a repository's main checkout finds the sessions in
// its linked worktrees, with no remote, and from every checkout and
// subdirectory alike; a deleted worktree's session stays in.
func TestRepoMatchesEveryCheckout(t *testing.T) {
	w := newRepoWorld(t)
	p := func(s string) string { return filepath.Join(w.base, s) }
	for _, tc := range []struct{ cwd, repo, want string }{
		{"app", ".", appRepo},
		{"app-api", ".", appRepo},
		{"app/sub", ".", appRepo},
		{"app/sub", "..", appRepo},
		{"notes", p("app"), appRepo},
		{"notes", p("app-api"), appRepo},
		{"notes", p("app-old"), appRepo}, // deleted: named by its placement
		{"other/app", ".", "otherapp"},
		{"web-fix", ".", webRepo},
		{"web", ".", webRepo},
		{"notes", "acme/web", webRepo},
		{"notes", "github.com/acme/web", webRepo},
		{"notes", "web", webRepo},
		{"lib", ".", libRepo},
		{"lib-feat", ".", libRepo},
		{"notes", p(".lib.git"), libRepo},
		{"notes", "lib", libRepo},
		{"notes", ".", "plain"},       // outside git: the directory
		{"notes", "app-api", "appwt"}, // a worktree's own name, no repository by it: the directory name, as before
	} {
		t.Run(tc.cwd+" "+tc.repo, func(t *testing.T) {
			t.Chdir(p(tc.cwd))
			got, err := w.sessions(t, format.Filters{Repo: tc.repo})
			if err != nil || got != tc.want {
				t.Fatalf("--repo %s from %s: %q %v, want %q", tc.repo, tc.cwd, got, err, tc.want)
			}
		})
	}
}

// Two repositories with the same name never merge: by path each is its
// own, and the bare name is an error that lists both.
func TestRepoSameNameDoesNotMerge(t *testing.T) {
	w := newRepoWorld(t)
	_, err := w.sessions(t, format.Filters{Repo: "app"})
	if !errors.Is(err, format.ErrBadRequest) || !strings.Contains(err.Error(), "names 2 repositories") ||
		!strings.Contains(err.Error(), filepath.Join("other", "app")) {
		t.Fatalf("ambiguous name: %v", err)
	}
	if got, err := w.sessions(t, format.Filters{Repo: filepath.Join(w.base, "other", "app")}); err != nil || got != "otherapp" {
		t.Fatalf("other/app by path: %q %v", got, err)
	}
}

// grep's repo filter is the same as sessions'.
func TestRepoGrepFilter(t *testing.T) {
	w := newRepoWorld(t)
	t.Chdir(filepath.Join(w.base, "app"))
	page := grepFor(t, w.b, pat("zebra"), format.Filters{Repo: "."})
	var got []string
	for _, h := range page.Hits {
		got = append(got, strings.TrimPrefix(h.Lines[0].Text, "zebra "))
	}
	slices.Sort(got)
	if strings.Join(got, ",") != appRepo {
		t.Fatalf("grep --repo .: %v", got)
	}
}

func TestSymlinkPrefixes(t *testing.T) {
	for _, c := range []struct{ l, r, lp, rp string }{
		{"/tmp/x/app", "/private/tmp/x/app", "", "/private"},
		{"/var/f/app", "/private/var/f/app", "", "/private"},
		{"/home/me/app", "/home/me/app", "", ""},
		{"/link/app", "/data/real/app", "/link", "/data/real"},
	} {
		if lp, rp := symlinkPrefixes(c.l, c.r); lp != c.lp || rp != c.rp {
			t.Errorf("symlinkPrefixes(%s, %s) = %q %q", c.l, c.r, lp, rp)
		}
	}
}

// Roots sent to the server fit format.MaxRepoRoots: nested roots go
// first, then the last ones; the first (the argument's checkouts) stay.
func TestWireRoots(t *testing.T) {
	var roots []string
	for i := range format.MaxRepoRoots + 10 {
		roots = append(roots, fmt.Sprintf("/w/r%03d", i))
	}
	roots = append([]string{"/w/r000/sub"}, roots...)
	got := wireRoots(roots)
	if len(got) != format.MaxRepoRoots || got[0] != "/w/r000" || slices.Contains(got, "/w/r000/sub") {
		t.Fatalf("wireRoots: %d, first %v", len(got), got[:2])
	}
	if short := []string{"/a", "/a/b"}; !slices.Equal(wireRoots(short), short) {
		t.Fatal("a list that fits is kept as it is")
	}
}

// For the server a bare name that fits two local repositories is not an
// error: the server matches the name across the team, as it did before
// --repo knew repositories, and the request carries both repositories'
// checkouts.
func TestServerRepoAmbiguousNameKeepsTheName(t *testing.T) {
	w := newRepoWorld(t)
	dirs, err := w.b.Store.RepoDirs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	repo, roots, err := ExpandRepo("app", dirs, true)
	if err != nil || repo != "app" || !slices.Contains(roots, filepath.Join(w.base, "app-api")) || !slices.Contains(roots, filepath.Join(w.base, "other", "app")) {
		t.Fatalf("--server --repo app: %q %v %v", repo, roots, err)
	}
}
