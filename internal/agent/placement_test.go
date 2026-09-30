package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/transcript"
)

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
}

// realDir is a temp dir with symlinks resolved (git records real paths).
func realDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// repoWithWorktree makes a main checkout (origin git@github.com:acme/app)
// and a linked worktree of it, and returns both roots.
func repoWithWorktree(t *testing.T) (main, wt string) {
	t.Helper()
	needGit(t)
	base := realDir(t)
	main = filepath.Join(base, "app")
	os.MkdirAll(main, 0o755)
	gitRun(t, main, "init", "-q")
	gitRun(t, main, "remote", "add", "origin", "git@github.com:acme/app.git")
	gitRun(t, main, "commit", "-q", "--allow-empty", "-m", "init")
	wt = filepath.Join(base, "app-fix-agent")
	gitRun(t, main, "worktree", "add", "-q", wt)
	os.MkdirAll(filepath.Join(wt, "src"), 0o755)
	return main, wt
}

const wtSession = "0b7e2c1a-0000-4000-8000-0000000000d1"

// claudeSessionAt writes a Claude session whose transcript names cwd, in
// the project folder Claude would use, and returns its path.
func (f *fixture) claudeSessionAt(sid, cwd, text string) string {
	f.t.Helper()
	dir := filepath.Join(f.cfg.ClaudeProjects, encodeClaudeName(cwd))
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, sid+".jsonl")
	if err := os.WriteFile(p, []byte(claudeLine(sid, cwd, text, 1)), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func claudeLine(sid, cwd, text string, n int) string {
	return fmt.Sprintf(`{"parentUuid":null,"isSidechain":false,"userType":"external","cwd":%q,"sessionId":%q,"version":"2.1.0","type":"user","message":{"role":"user","content":%q},"uuid":"c1000000-0000-4000-8000-%012d","timestamp":"2026-09-23T11:00:00.000Z"}`+"\n",
		cwd, sid, text, n)
}

func (f *fixture) sessions(sid string) int {
	f.t.Helper()
	return f.count(`SELECT count(*) FROM messages m JOIN conversations c ON c.id = m.conversation_id WHERE c.session_id = ?`, sid)
}

// A rule on the main checkout covers sessions in its linked worktrees.
func TestMainCheckoutRuleCoversWorktree(t *testing.T) {
	main, wt := repoWithWorktree(t)
	f, _, _ := rulesFixture(t, "-", "deny "+main)
	p := f.claudeSessionAt(wtSession, filepath.Join(wt, "src"), "worktree needle")
	f.once()
	if n := f.sessions(wtSession); n != 0 {
		t.Errorf("a session in a worktree of a denied checkout was indexed (%d rows)", n)
	}
	if _, ok := f.rec.spec(p); ok || f.a.allowUpload(devicesync.SourceSpec{Path: p, Agent: transcript.AgentClaude, SessionKey: wtSession}) {
		t.Error("a session in a worktree of a denied checkout reached sync")
	}
}

// The placement is stored when the session is first seen: after its
// worktree (and even the main checkout) is deleted and the agent
// restarts, rule changes are still decided against it, by path and by
// remote.
func TestDeletedWorktreeKeepsStoredPlacement(t *testing.T) {
	// "upload": no rule and no setting applies when the session is first
	// seen; its placement is recorded anyway.
	for _, unplaceable := range []string{"", "upload"} {
		t.Run("unplaceable="+unplaceable, func(t *testing.T) { deletedWorktree(t, unplaceable) })
	}
}

func deletedWorktree(t *testing.T, unplaceable string) {
	main, wt := repoWithWorktree(t)
	f, file, _ := rulesFixture(t, "-")
	f.cfg.Unplaceable = unplaceable
	f.restart()
	p := f.claudeSessionAt(wtSession, filepath.Join(wt, "src"), "worktree needle")
	f.once()
	if f.sessions(wtSession) == 0 {
		t.Fatal("fixture session not indexed")
	}
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	gitRun(t, main, "worktree", "prune")
	f.restart()

	// A deny rule on the main checkout, added after the delete.
	writeRules(t, file, "deny "+main)
	f.once()
	if n := f.sessions(wtSession); n != 0 {
		t.Errorf("deleted worktree's session not purged by a rule on its main checkout (%d rows)", n)
	}
	appendFile(t, p, claudeLine(wtSession, filepath.Join(wt, "src"), "later line", 2))
	f.once()
	if n := f.sessions(wtSession); n != 0 {
		t.Errorf("a line appended after the delete was indexed (%d rows)", n)
	}

	// With the main checkout gone too, a repo rule still matches the
	// stored remote; loosening deny to local indexes the session again.
	if err := os.RemoveAll(main); err != nil {
		t.Fatal(err)
	}
	f.restart()
	writeRules(t, file, "local repo:github.com/acme/*")
	f.rec = newRecorder()
	f.cfg.Sync = f.rec
	f.restart()
	f.once()
	if f.sessions(wtSession) == 0 {
		t.Error("session not indexed again after its deny rule became local")
	}
	if _, ok := f.rec.spec(p); ok {
		t.Error("a session under a local repo rule was handed to sync")
	}
	var how, remote, mainRoot string
	f.store.DB().QueryRow(`SELECT how, ifnull(remote, ''), ifnull(main_root, '') FROM placements WHERE session_id = ?`, wtSession).Scan(&how, &remote, &mainRoot)
	if how != localindex.PlacedByWorktree || remote != "github.com/acme/app" || mainRoot != main {
		t.Errorf("stored placement %s %s %s", how, remote, mainRoot)
	}
}

// A repo rule matches the origin remote, whether it is an ssh or an https
// URL.
func TestRepoRuleMatchesSSHAndHTTPSRemotes(t *testing.T) {
	needGit(t)
	base := realDir(t)
	remotes := map[string]string{"ssh": "git@github.com:Acme/one.git", "https": "https://github.com/acme/two.git", "other": "https://github.com/else/three"}
	f, _, _ := rulesFixture(t, "-", "deny repo:github.com/acme/*")
	sids := map[string]string{}
	i := 0
	for name, url := range remotes {
		dir := filepath.Join(base, name)
		os.MkdirAll(dir, 0o755)
		gitRun(t, dir, "init", "-q")
		gitRun(t, dir, "remote", "add", "origin", url)
		i++
		sids[name] = fmt.Sprintf("0b7e2c1a-0000-4000-8000-0000000000e%d", i)
		f.claudeSessionAt(sids[name], dir, "remote needle "+name)
	}
	f.once()
	for name, sid := range sids {
		n := f.sessions(sid)
		if name == "other" && n == 0 {
			t.Errorf("%s: an unmatched remote was not indexed", name)
		}
		if name != "other" && n != 0 {
			t.Errorf("%s: a session in a denied repo was indexed", name)
		}
	}
}

// oldCodexRollout writes an August 2025 style rollout (a header line with
// git but no cwd) whose mtime is past cwdWait.
func (f *fixture) oldCodexRollout(id, header, text string) string {
	f.t.Helper()
	dir := filepath.Join(f.cfg.CodexHome, "sessions", "2025", "08", "09")
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, "rollout-2025-08-09T15-19-15-"+id+".jsonl")
	body := header + "\n" + `{"record_type":"state"}` + "\n" +
		fmt.Sprintf(`{"type":"message","id":null,"role":"user","content":[{"type":"input_text","text":%q}]}`, text) + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		f.t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(p, old, old)
	return p
}

// An old Codex rollout that names no directory is placed by the git
// remote it recorded.
func TestCodexOldRolloutPlacedByRemote(t *testing.T) {
	const id = "38b23afb-b0b1-4327-ba69-e38377dc11a1"
	f, _, _ := rulesFixture(t, "-", "deny repo:github.com/acme/secret")
	p := f.oldCodexRollout(id, `{"id":"`+id+`","timestamp":"2025-08-09T15:19:15.009Z","instructions":null,"git":{"commit_hash":"d14c","branch":"main","repository_url":"https://github.com/Acme/secret.git"}}`, "old rollout needle")
	f.once()
	if n := f.count(`SELECT count(*) FROM conversations WHERE session_id = ?`, id); n != 0 {
		t.Errorf("an old rollout in a denied repo was indexed")
	}
	if f.a.allowUpload(devicesync.SourceSpec{Path: p, Agent: transcript.AgentCodex, SessionKey: id}) {
		t.Error("an old rollout in a denied repo may upload")
	}
	var how string
	f.store.DB().QueryRow(`SELECT how FROM placements WHERE session_id = ?`, id).Scan(&how)
	if how != localindex.PlacedByRemote {
		t.Errorf("placed by %q", how)
	}
}

// A young transcript that names no directory waits cwdWait to be placed.
// Once the wait is over the next sweep must hand it to sync, even though
// the file has not changed since (an idle new file is never re-gated).
func TestYoungUnplacedTranscriptUploadsAfterCwdWait(t *testing.T) {
	saved := cwdWait
	t.Cleanup(func() { cwdWait = saved })
	cwdWait = time.Hour // every copied fixture is young
	f := newFixture(t, "-")
	legacy := f.path(".codex/sessions/2025/08/20/rollout-2025-08-20T12-00-00-019a0000-0000-7000-8000-0000000000a7.jsonl")
	f.once()
	if _, ok := f.rec.spec(legacy); ok {
		t.Fatal("a young rollout without a directory was handed to sync before cwdWait")
	}
	cwdWait = 0 // the wait is over; the file has not changed
	f.once()
	if _, ok := f.rec.spec(legacy); !ok {
		t.Fatal("the rollout was never handed to sync after cwdWait")
	}
}

// A session with no directory and no remote gets the unplaceable setting:
// the user's (default local), with the admin's as a floor.
func TestUnplaceableSetting(t *testing.T) {
	const id = "38b23afb-b0b1-4327-ba69-e38377dc11a2"
	cases := []struct {
		user, admin      string
		indexed, uploads bool
	}{
		{"", "", true, false}, // default local
		{"local", "", true, false},
		{"upload", "", true, true},
		{"exclude", "", false, false},
		{"upload", "local", true, false},    // the admin floor wins
		{"upload", "exclude", false, false}, // the admin floor wins
		{"exclude", "upload", false, false}, // the user may be stricter
	}
	for _, c := range cases {
		t.Run(c.user+"/"+c.admin, func(t *testing.T) {
			f, _, _ := rulesFixture(t, "-")
			f.cfg.Unplaceable = c.user
			if c.admin != "" {
				admin := c.admin
				f.cfg.AdminRules = func(context.Context) (AdminPolicy, error) {
					return AdminPolicy{Rules: []string{}, Unplaceable: admin}, nil
				}
			}
			f.restart()
			p := f.oldCodexRollout(id, `{"id":"`+id+`","timestamp":"2025-08-09T15:19:15.009Z","instructions":null}`, "unplaceable needle")
			f.once()
			if got := f.count(`SELECT count(*) FROM conversations WHERE session_id = ?`, id) > 0; got != c.indexed {
				t.Errorf("indexed %v, want %v", got, c.indexed)
			}
			if _, got := f.rec.spec(p); got != c.uploads {
				t.Errorf("handed to sync %v, want %v", got, c.uploads)
			}
			if got := f.a.allowUpload(devicesync.SourceSpec{Path: p, Agent: transcript.AgentCodex, SessionKey: id}); got != c.uploads {
				t.Errorf("sync filter %v, want %v", got, c.uploads)
			}
		})
	}
}

// decodeClaudeFolder: the folder name is lossy ('/', '.', '-' and '_' all
// became '-'); the filesystem decides, and a missing tail is decoded
// naively.
func TestDecodeClaudeFolder(t *testing.T) {
	base := realDir(t)
	for _, d := range []string{"my-app.v2/sub_dir", "my/app", "my-app", "x/y-z"} {
		os.MkdirAll(filepath.Join(base, d), 0o755)
	}
	for _, want := range []string{
		filepath.Join(base, "my-app.v2", "sub_dir"),
		filepath.Join(base, "my-app"), // "my/app" exists too: the longer name wins
		filepath.Join(base, "x", "y-z"),
		filepath.Join(base, ".hidden-x"),
	} {
		if want == filepath.Join(base, ".hidden-x") {
			os.MkdirAll(want, 0o755)
		}
		if got := decodeClaudeFolder(encodeClaudeName(want)); got != want {
			t.Errorf("decode(%s) = %s, want %s", encodeClaudeName(want), got, want)
		}
	}
	if got := decodeClaudeFolder(encodeClaudeName(base) + "-gone-dir"); got != filepath.Join(base, "gone", "dir") {
		t.Errorf("naive tail: %s", got)
	}
	if got := decodeClaudeFolder("not-absolute"); got != "" {
		t.Errorf("relative folder decoded to %q", got)
	}
}

// A Claude transcript that never names its directory (metadata only) and
// an orphaned session's companions are placed by the project folder
// name, a dash-containing directory included; a companion of an allowed
// folder still uploads.
func TestClaudeFolderFallback(t *testing.T) {
	base := realDir(t)
	secret := filepath.Join(base, "client-acme", "web")
	open := filepath.Join(base, "open-src")
	os.MkdirAll(secret, 0o755)
	os.MkdirAll(open, 0o755)
	f, _, _ := rulesFixture(t, "-", "deny "+filepath.Join(base, "client-acme"))

	// A metadata-only transcript in the denied folder.
	metaSid := "0b7e2c1a-0000-4000-8000-0000000000f1"
	dir := filepath.Join(f.cfg.ClaudeProjects, encodeClaudeName(secret))
	os.MkdirAll(dir, 0o755)
	meta := filepath.Join(dir, metaSid+".jsonl")
	os.WriteFile(meta, []byte(`{"type":"ai-title","title":"secret title","sessionId":"`+metaSid+`"}`+"\n"), 0o600)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(meta, old, old)

	// Orphaned sessions (tool-results, no transcript) in each folder.
	orphan := func(cwd, sid string) string {
		p := filepath.Join(f.cfg.ClaudeProjects, encodeClaudeName(cwd), sid, "tool-results", "out01.txt")
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("tool output\n"), 0o600)
		return p
	}
	denied := orphan(secret, "0b7e2c1a-0000-4000-8000-0000000000f2")
	allowed := orphan(open, "0b7e2c1a-0000-4000-8000-0000000000f3")
	f.once()

	if f.a.allowUpload(devicesync.SourceSpec{Path: meta, Agent: transcript.AgentClaude, SessionKey: metaSid}) {
		t.Error("a metadata-only transcript in a denied folder may upload")
	}
	if n := f.count(`SELECT count(*) FROM conversations WHERE session_id = ?`, metaSid); n != 0 {
		t.Error("a metadata-only transcript in a denied folder was indexed")
	}
	if _, ok := f.rec.spec(denied); ok {
		t.Error("an orphaned companion in a denied folder was handed to sync")
	}
	if _, ok := f.rec.spec(allowed); !ok {
		t.Error("an orphaned companion in an allowed folder was not handed to sync")
	}
	var how, cwd string
	f.store.DB().QueryRow(`SELECT how, cwd FROM placements WHERE session_id = ?`, metaSid).Scan(&how, &cwd)
	if how != localindex.PlacedByFolder || cwd != secret {
		t.Errorf("placed by %q at %q", how, cwd)
	}
}

// The sync scheduler runs its filter as soon as the agent is built, before
// the first pass loads anything: a queued flush of a session whose
// worktree is gone must be decided against the stored placement, and that
// placement must not be replaced by a weaker one read from the transcript.
func TestSyncFilterBeforeLoadUsesStoredPlacement(t *testing.T) {
	main, wt := repoWithWorktree(t)
	f, file, _ := rulesFixture(t, "-")
	f.cfg.Unplaceable = "upload"
	f.restart()
	p := f.claudeSessionAt(wtSession, filepath.Join(wt, "src"), "worktree needle")
	f.once()
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	gitRun(t, main, "worktree", "prune")
	writeRules(t, file, "local "+main)
	f.restart() // New installs the filter; the scheduler may call it at once
	spec := devicesync.SourceSpec{Path: p, Agent: transcript.AgentClaude, SessionKey: wtSession}
	if f.a.allowUpload(spec) {
		t.Error("a queued flush of a session under a local rule on its main checkout may upload after a restart")
	}
	var mainRoot string
	f.store.DB().QueryRow(`SELECT ifnull(main_root, '') FROM placements WHERE session_id = ?`, wtSession).Scan(&mainRoot)
	if mainRoot != main {
		t.Errorf("stored main checkout %q, want %q", mainRoot, main)
	}
}

// A directory's git state is read when each session is placed, not once
// per process: a remote added while the agent runs covers the next
// session there.
func TestRemoteAddedWhileRunning(t *testing.T) {
	needGit(t)
	dir := filepath.Join(realDir(t), "proj")
	os.MkdirAll(dir, 0o755)
	gitRun(t, dir, "init", "-q")
	f, _, _ := rulesFixture(t, "-", "deny repo:github.com/acme/*")
	first := "0b7e2c1a-0000-4000-8000-0000000000a1"
	f.claudeSessionAt(first, dir, "before the remote")
	f.once()
	if f.sessions(first) == 0 {
		t.Fatal("a session in a repo with no remote was not indexed")
	}
	gitRun(t, dir, "remote", "add", "origin", "git@github.com:acme/proj.git")
	second := "0b7e2c1a-0000-4000-8000-0000000000a2"
	p := f.claudeSessionAt(second, dir, "after the remote")
	f.once()
	if n := f.sessions(second); n != 0 {
		t.Errorf("a session in a repo whose remote a deny rule covers was indexed (%d rows)", n)
	}
	if f.a.allowUpload(devicesync.SourceSpec{Path: p, Agent: transcript.AgentClaude, SessionKey: second}) {
		t.Error("a session in a denied repo may upload")
	}
}

// An orphaned session's folder whose directory is gone decodes naively,
// which loses dashes; a rule on a dash-named directory must still cover
// it.
func TestOrphanInDeletedDashedDirStaysDenied(t *testing.T) {
	base := realDir(t)
	secret := filepath.Join(base, "secret-proj") // never created: deleted
	f, _, _ := rulesFixture(t, "-", "deny "+secret)
	sid := "0b7e2c1a-0000-4000-8000-0000000000b1"
	c := filepath.Join(f.cfg.ClaudeProjects, encodeClaudeName(secret), sid, "tool-results", "out01.txt")
	os.MkdirAll(filepath.Dir(c), 0o755)
	os.WriteFile(c, []byte("secret tool output\n"), 0o600)
	f.once()
	if _, ok := f.rec.spec(c); ok {
		t.Error("an orphaned companion of a denied, deleted directory was handed to sync")
	}
	if f.a.allowUpload(devicesync.SourceSpec{Path: c, Agent: transcript.AgentClaude, SessionKey: sid,
		Parent: filepath.Join(f.cfg.ClaudeProjects, encodeClaudeName(secret), sid+".jsonl")}) {
		t.Error("the sync filter lets an orphaned companion of a denied, deleted directory upload")
	}
	if n := f.count(`SELECT count(*) FROM conversations WHERE session_id = ?`, sid); n != 0 {
		t.Error("an orphaned session of a denied, deleted directory was indexed")
	}
}

func TestFolderMatch(t *testing.T) {
	folder := strings.ToLower(encodeClaudeName("/Users/me/Code/secret-proj/web"))
	for pattern, want := range map[string]bool{
		"/users/me/code/secret-proj":   true,
		"/users/me/code/secret-proj/":  true,
		"/users/me/code/secret/proj":   true, // the name cannot tell them apart
		"/users/me/code/secret.proj":   true,
		"/users/me/code/*-proj":        true,
		"/users/me/**/web":             true,
		"secret-proj":                  true, // any depth
		"/users/me/code/secret-pro?":   true,
		"/users/me/code/secret-pro[j]": true,
		"/users/me/code/open":          false,
		"/users/me/code/secret-projx":  false,
		"/users/you":                   false,
		"web2":                         false,
	} {
		if got := pathpolicy.MatchClaudeFolder(pattern, folder); got != want {
			t.Errorf("folderMatch(%q, %q) = %v, want %v", pattern, folder, got, want)
		}
	}
}
