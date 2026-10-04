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
		{"notes", ".", "plain"}, // outside git: the directory
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

// #102 review: the lists a --repo filter carries do not grow with the
// repository's worktrees (they are matched by the main checkout and
// remote their placements record), and a list past the request cap is an
// error that names it, never a silent cut.
func TestRepoListsStaySmall(t *testing.T) {
	var dirs []localindex.RepoDir
	for i := range 3 * format.MaxRepoRoots {
		dirs = append(dirs, localindex.RepoDir{Dir: fmt.Sprintf("/w/web-wt-%04d", i), Main: "/w/web", Remote: "github.com/acme/web"})
	}
	r, err := ExpandRepo("web", dirs, true)
	if err != nil || len(r.Roots) > 2 || !slices.Equal(r.Mains, []string{"/w/web"}) || !slices.Equal(r.Remotes, []string{"github.com/acme/web"}) {
		t.Fatalf("--server --repo web over %d worktrees: %+v %v", len(dirs), r, err)
	}
	dirs = dirs[:0]
	for i := range format.MaxRepoRoots + 1 {
		dirs = append(dirs, localindex.RepoDir{Dir: fmt.Sprintf("/c/web-%04d", i), Main: fmt.Sprintf("/c/web-%04d", i), Remote: "github.com/acme/web"})
	}
	if _, err := ExpandRepo("web", dirs, true); !errors.Is(err, format.ErrBadRequest) || !strings.Contains(err.Error(), "at most 256") {
		t.Fatalf("%d clones of one remote: %v", len(dirs), err)
	}
	if r, err := ExpandRepo("web", dirs, false); err != nil || len(r.Mains) != len(dirs) {
		t.Fatalf("locally, %d clones: %d mains %v", len(dirs), len(r.Mains), err)
	}
	// Directories past the local query's cap are refused, not cut.
	st, err := localindex.Open(filepath.Join(t.TempDir(), "index.db"), localindex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var roots []string
	for i := range localindex.MaxRepos + 1 {
		roots = append(roots, fmt.Sprintf("/d/%04d", i))
	}
	if _, err := (&Backend{Store: st}).Sessions(ctx, "", "", format.Filters{RepoRoots: roots}); !errors.Is(err, format.ErrBadRequest) || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("%d roots: %v", len(roots), err)
	}
}

// #102: a name that fits no repository the device knows is an error
// that says so. It used to fall back to matching the last element of a
// session's directory, so "app-api" (a worktree's own name) found that
// worktree's session, and a name could merge two unrelated directories.
func TestRepoUnknownNameIsAnError(t *testing.T) {
	w := newRepoWorld(t)
	for _, name := range []string{"app-api", "notes", "nosuch"} {
		_, err := w.sessions(t, format.Filters{Repo: name})
		if !errors.Is(err, format.ErrBadRequest) || !strings.Contains(err.Error(), "names no repository") {
			t.Errorf("--repo %s: %v", name, err)
		}
	}
	// Its path still names the directory.
	if got, err := w.sessions(t, format.Filters{Repo: filepath.Join(w.base, "notes")}); err != nil || got != "plain" {
		t.Fatalf("--repo <notes path>: %q %v", got, err)
	}
}

// #102: for the server, a name that fits two local repositories is the
// same error as locally: the server must never merge two repositories of
// one name. It used to send the name, which the server matched against
// the last element of every session's directory on every device.
func TestServerRepoAmbiguousNameIsAnError(t *testing.T) {
	w := newRepoWorld(t)
	dirs, err := w.b.Store.RepoDirs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ExpandRepo("app", dirs, true)
	if !errors.Is(err, format.ErrBadRequest) || !strings.Contains(err.Error(), "names 2 repositories") {
		t.Fatalf("--server --repo app: %+v %v", r, err)
	}
}

// #102: for the server, a name the device knows becomes that
// repository's checkouts and remotes, and no name: the server matches
// the remote on every device, at any path. A name the device does not
// know passes through for the server to resolve.
func TestServerRepoSendsRemotes(t *testing.T) {
	w := newRepoWorld(t)
	dirs, err := w.b.Store.RepoDirs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p := func(s string) string { return filepath.Join(w.base, s) }
	real := func(s string) string {
		r, err := filepath.EvalSymlinks(s)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, arg := range []string{"web", "acme/web", p("web-fix")} {
		r, err := ExpandRepo(arg, dirs, true)
		if err != nil || !slices.Equal(r.Remotes, []string{"github.com/acme/web"}) || !slices.Contains(r.Mains, real(p("elsewhere/web2"))) || !slices.Contains(r.Mains, real(p("web"))) {
			t.Fatalf("--server --repo %s: %+v %v", arg, r, err)
		}
		if !strings.HasPrefix(arg, "/") && r.Repo != "" {
			t.Fatalf("--server --repo %s sends the name %q", arg, r.Repo)
		}
	}
	if r, err := ExpandRepo(p("app"), dirs, true); err != nil || len(r.Remotes) != 0 || !slices.Contains(r.Mains, real(p("app"))) || r.Repo != p("app") {
		t.Fatalf("--server --repo <app>: %+v %v", r, err)
	}
	if r, err := ExpandRepo("teammates-repo", dirs, true); err != nil || r.Repo != "teammates-repo" || r.Roots != nil || r.Remotes != nil {
		t.Fatalf("--server --repo teammates-repo: %+v %v", r, err)
	}
	// A path rule on the repository keeps its remote off the request.
	deny := func(pl pathpolicy.Placement) bool { return pl.Remote != "github.com/acme/web" }
	if r, err := ServerRepo("web", dirs, deny); err != nil || r.Remotes != nil || r.Mains != nil {
		t.Fatalf("withheld remote: %+v %v", r, err)
	}
}

// #102: on a case-insensitive volume (macOS by default), /p/App and
// /p/app are one directory, so placements that spell one main checkout
// two ways are one repository: --repo by name is not ambiguous, and by
// either path finds both sessions. Skipped where the volume holding the
// temporary directory compares case exactly.
func TestRepoMainCaseInsensitive(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	base := t.TempDir()
	app := filepath.Join(base, "App")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "app")); err != nil {
		t.Skip("the volume compares case exactly")
	}
	git(t, app, "init", "-q")
	lower := filepath.Join(base, "app")
	dirs := []localindex.RepoDir{{Dir: app, Main: app}, {Dir: lower, Main: lower}}
	r, err := ExpandRepo("app", dirs, false)
	if err != nil || !slices.Contains(r.Mains, app) || !slices.Contains(r.Mains, lower) {
		t.Fatalf("--repo app: %+v %v", r, err)
	}
	if r, err := ExpandRepo(lower, dirs, false); err != nil || !slices.Contains(r.Mains, app) {
		t.Fatalf("--repo %s: %+v %v", lower, r, err)
	}
}

// #102: --repo with a deleted directory under a home directory that is a
// git repository (dotfiles) names that directory, not the home
// repository: a placement above it whose directory still exists is
// another repository, not the deleted one's. It used to name the home
// repository, so the filter matched every session under home.
func TestRepoDeletedDirUnderDotfilesHome(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	git(t, home, "init", "-q")
	git(t, home, "remote", "add", "origin", "git@github.com:me/dotfiles.git")
	gone := filepath.Join(home, "code", "app-wt")
	dirs := []localindex.RepoDir{
		{Dir: home, Main: home, Remote: "github.com/me/dotfiles"},
		{Dir: gone}, // placed with no repository: its directory was gone
	}
	r, err := ExpandRepo(gone, dirs, false)
	if err != nil || r.Repo != gone || slices.Contains(r.Roots, home) || slices.Contains(r.Mains, home) || len(r.Remotes) != 0 {
		t.Fatalf("--repo <deleted dir under home>: %+v %v", r, err)
	}
	// A deleted worktree is still named by a placement recorded at it,
	// and a directory under it by the placement above it, also gone.
	wt := filepath.Join(home, "gone-wt")
	dirs = append(dirs, localindex.RepoDir{Dir: wt, Main: "/p/app", Remote: "github.com/acme/app"})
	for _, arg := range []string{wt, filepath.Join(wt, "sub")} {
		if r, err := ExpandRepo(arg, dirs, false); err != nil || !slices.Contains(r.Mains, "/p/app") || slices.Contains(r.Mains, home) {
			t.Fatalf("--repo %s: %+v %v", arg, r, err)
		}
	}
}
