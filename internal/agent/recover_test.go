package agent

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/transcript"
)

// claudeRecord writes one Claude user record with the given extra fields.
func claudeRecord(sid, cwd, extra, text string, n int) string {
	return fmt.Sprintf(`{"parentUuid":null,"isSidechain":false,"userType":"external","cwd":%q,"sessionId":%q,%s"version":"2.1.0","type":"user","message":{"role":"user","content":%q},"uuid":"c2000000-0000-4000-8000-%012d","timestamp":"2026-09-23T11:00:00.000Z"}`+"\n",
		cwd, sid, extra, text, n)
}

// writeSession writes a Claude transcript of lines in cwd's project folder.
func (f *fixture) writeSession(sid, cwd string, lines ...string) string {
	f.t.Helper()
	dir := filepath.Join(f.cfg.ClaudeProjects, encodeClaudeName(cwd))
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, sid+".jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "")), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return p
}

type storedPlacement struct {
	how, main, remote string
	checked           int64
}

func (f *fixture) placement(sid string) storedPlacement {
	f.t.Helper()
	var p storedPlacement
	f.store.DB().QueryRow(`SELECT how, ifnull(main_root, ''), ifnull(remote, ''), ifnull(checked_at, 0) FROM placements WHERE session_id = ?`, sid).
		Scan(&p.how, &p.main, &p.remote, &p.checked)
	return p
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

const (
	liveSid = "0b7e2c1a-0000-4000-8000-000000000101"
	goneSid = "0b7e2c1a-0000-4000-8000-000000000102"
)

// deletedWorktreeSession removes the worktree wt (keeping its branch
// unless dropBranch) and writes, only then, a session that ran in it: the
// agent first sees it after the delete. main gets a live session so the
// agent knows the checkout.
func (f *fixture) deletedWorktreeSession(main, wt string, dropBranch bool, lines ...string) string {
	f.t.Helper()
	branch := gitOutput(f.t, wt, "branch", "--show-current")
	if err := os.RemoveAll(wt); err != nil {
		f.t.Fatal(err)
	}
	gitRun(f.t, main, "worktree", "prune")
	if dropBranch {
		gitRun(f.t, main, "branch", "-D", branch)
	}
	f.claudeSessionAt(liveSid, main, "live session in the main checkout")
	return f.writeSession(goneSid, filepath.Join(wt, "src"), lines...)
}

// A session first seen after its worktree was deleted is placed in the
// main checkout whose refs hold the branch it recorded, and a rule on the
// main checkout covers it.
func TestRecoverDeletedWorktreeByBranch(t *testing.T) {
	main, wt := repoWithWorktree(t)
	f, _, _ := rulesFixture(t, "-", "deny "+main)
	cwd := filepath.Join(wt, "src")
	p := f.deletedWorktreeSession(main, wt, false,
		claudeRecord(goneSid, cwd, `"gitBranch":"app-fix-agent",`, "secret work in a deleted worktree", 1))
	f.once()
	if n := f.sessions(goneSid); n != 0 {
		t.Errorf("a deleted worktree's session under a denied main checkout was indexed (%d rows)", n)
	}
	if _, ok := f.rec.spec(p); ok || f.a.allowUpload(devicesync.SourceSpec{Path: p, Agent: transcript.AgentClaude, SessionKey: goneSid}) {
		t.Error("a deleted worktree's session under a denied main checkout reached sync")
	}
	got := f.placement(goneSid)
	if got.how != localindex.PlacedByBranch || got.main != main || got.remote != "github.com/acme/app" || got.checked == 0 {
		t.Errorf("stored placement %+v", got)
	}
	if c := f.a.placementCounts(); c[localindex.PlacedByBranch] != 1 {
		t.Errorf("status counts %v", c)
	}
}

// Two repositories know the branch: no main checkout is recorded, and the
// deny rule on one of the candidates covers the session (fail closed).
func TestRecoverAmbiguousBranch(t *testing.T) {
	main, wt := repoWithWorktree(t)
	other := filepath.Join(filepath.Dir(main), "other")
	os.MkdirAll(other, 0o755)
	gitRun(t, other, "init", "-q")
	gitRun(t, other, "commit", "-q", "--allow-empty", "-m", "init")
	gitRun(t, other, "branch", "app-fix-agent")
	f, _, _ := rulesFixture(t, "-", "deny "+main)
	f.claudeSessionAt("0b7e2c1a-0000-4000-8000-000000000103", other, "live session in the other repo")
	cwd := filepath.Join(wt, "src")
	p := f.deletedWorktreeSession(main, wt, false, claudeRecord(goneSid, cwd, `"gitBranch":"app-fix-agent",`, "ambiguous work", 1))
	f.once()
	got := f.placement(goneSid)
	if got.main != "" || got.how != localindex.PlacedByCwd || got.checked == 0 {
		t.Errorf("an ambiguous branch placed the session: %+v", got)
	}
	if n := f.sessions(goneSid); n != 0 {
		t.Errorf("a session with a denied candidate was indexed (%d rows)", n)
	}
	if _, ok := f.rec.spec(p); ok {
		t.Error("a session with a denied candidate was handed to sync")
	}
}

// With the branch deleted too, the `git worktree add` a live session ran
// names the repository.
func TestRecoverDeletedWorktreeByWorktreeAdd(t *testing.T) {
	main, _ := repoWithWorktree(t)
	f, _, _ := rulesFixture(t, "-", "local "+main)
	wt := filepath.Join(filepath.Dir(main), "app-wt2")
	gitRun(t, main, "worktree", "add", "-q", "-b", "wt2-branch", wt)
	// The live session that made it: a Bash tool call, JSON-escaped.
	add := fmt.Sprintf(`{"type":"assistant","cwd":%q,"sessionId":%q,"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"cd %s && git worktree add ../app-wt2 -b wt2-branch && echo \"done\""}}]},"uuid":"c3000000-0000-4000-8000-000000000001","timestamp":"2026-09-23T11:00:00.000Z"}`+"\n",
		main, liveSid, main)
	p := f.deletedWorktreeSession(main, wt, true, claudeRecord(goneSid, filepath.Join(wt, "src"), `"gitBranch":"wt2-branch",`, "work in wt2", 1))
	appendFile(t, f.claudeSessionAt(liveSid, main, "live session in the main checkout"), add)
	f.once()
	got := f.placement(goneSid)
	if got.how != localindex.PlacedByWorktreeAdd || got.main != main {
		t.Errorf("stored placement %+v", got)
	}
	if f.sessions(goneSid) == 0 {
		t.Error("a session under a local rule was not indexed")
	}
	if _, ok := f.rec.spec(p); ok {
		t.Error("a session under a local rule on its main checkout was handed to sync")
	}
}

// With no branch and no worktree add, a commit the session printed names
// the repository.
func TestRecoverDeletedWorktreeByCommit(t *testing.T) {
	main, wt := repoWithWorktree(t)
	gitRun(t, wt, "commit", "-q", "--allow-empty", "-m", "wip")
	hash := gitOutput(t, wt, "rev-parse", "--short=9", "HEAD")
	f, _, _ := rulesFixture(t, "-", "deny "+main)
	p := f.deletedWorktreeSession(main, wt, true,
		claudeRecord(goneSid, filepath.Join(wt, "src"), "", "[app-fix-agent "+hash+"] wip\n 1 file changed", 1))
	f.once()
	got := f.placement(goneSid)
	if got.how != localindex.PlacedByCommit || got.main != main {
		t.Errorf("stored placement %+v", got)
	}
	if f.sessions(goneSid) != 0 {
		t.Error("a session under a denied main checkout was indexed")
	}
	if _, ok := f.rec.spec(p); ok {
		t.Error("a session under a denied main checkout was handed to sync")
	}
}

// A session placed only by its remote also matches path rules on the one
// local checkout with that origin.
func TestRemoteLinksToLocalCheckout(t *testing.T) {
	main, _ := repoWithWorktree(t) // origin git@github.com:acme/app.git
	const id = "38b23afb-b0b1-4327-ba69-e38377dc11b1"
	f, _, _ := rulesFixture(t, "-", "deny "+main)
	f.claudeSessionAt(liveSid, main, "live session in the main checkout")
	p := f.oldCodexRollout(id, `{"id":"`+id+`","timestamp":"2025-08-09T15:19:15.009Z","instructions":null,"git":{"commit_hash":"d14c","branch":"x","repository_url":"https://github.com/acme/app.git"}}`, "old rollout in a deleted worktree")
	f.once()
	if n := f.count(`SELECT count(*) FROM conversations WHERE session_id = ?`, id); n != 0 {
		t.Error("a rollout whose remote is a denied local checkout's origin was indexed")
	}
	if _, ok := f.rec.spec(p); ok || f.a.allowUpload(devicesync.SourceSpec{Path: p, Agent: transcript.AgentCodex, SessionKey: id}) {
		t.Error("a rollout whose remote is a denied local checkout's origin reached sync")
	}
	if got := f.placement(id); got.how != localindex.PlacedByRemote || got.main != main {
		t.Errorf("stored placement %+v", got)
	}

	// One that named a directory, gone now (a deleted worktree): linked
	// the same way, and the link stays (the placement is final).
	const id2 = "38b23afb-b0b1-4327-ba69-e38377dc11b3"
	gone := filepath.Join(filepath.Dir(main), "app-wt-gone")
	p2 := f.oldCodexRollout(id2, `{"id":"`+id2+`","timestamp":"2025-08-09T15:19:15.009Z","instructions":null,"git":{"repository_url":"git@github.com:acme/app.git"}}`,
		"<environment_context><cwd>"+gone+"</cwd></environment_context>")
	f.once()
	f.once()
	if got := f.placement(id2); got.how != localindex.PlacedByRemote || got.main != main {
		t.Errorf("stored placement of a rollout in a deleted worktree %+v", got)
	}
	if n := f.count(`SELECT count(*) FROM conversations WHERE session_id = ?`, id2); n != 0 {
		t.Error("a rollout in a deleted worktree of a denied checkout was indexed")
	}
	if f.a.allowUpload(devicesync.SourceSpec{Path: p2, Agent: transcript.AgentCodex, SessionKey: id2}) {
		t.Error("a rollout in a deleted worktree of a denied checkout may upload")
	}
}

// Two local checkouts share the origin: the remote links to neither, and
// the deny rule on one of them covers the rollout (fail closed).
func TestRemoteLinkAmbiguous(t *testing.T) {
	main, _ := repoWithWorktree(t)
	clone := filepath.Join(filepath.Dir(main), "app-clone")
	os.MkdirAll(clone, 0o755)
	gitRun(t, clone, "init", "-q")
	gitRun(t, clone, "remote", "add", "origin", "https://github.com/acme/app")
	const id = "38b23afb-b0b1-4327-ba69-e38377dc11b2"
	f, _, _ := rulesFixture(t, "-", "deny "+main)
	f.claudeSessionAt(liveSid, main, "live session in the main checkout")
	f.claudeSessionAt("0b7e2c1a-0000-4000-8000-000000000104", clone, "live session in the clone")
	f.oldCodexRollout(id, `{"id":"`+id+`","timestamp":"2025-08-09T15:19:15.009Z","instructions":null,"git":{"repository_url":"https://github.com/acme/app.git"}}`, "ambiguous rollout")
	f.once()
	if got := f.placement(id); got.main != "" || got.checked == 0 {
		t.Errorf("stored placement %+v", got)
	}
	if n := f.count(`SELECT count(*) FROM conversations WHERE session_id = ?`, id); n != 0 {
		t.Errorf("a rollout with a denied candidate was indexed (%d rows)", n)
	}
}

// A session in a gone directory waits for the recovery pass before it may
// upload; with no signal it stays as it was and then uploads.
func TestDeletedDirWaitsForRecoveryPass(t *testing.T) {
	main, _ := repoWithWorktree(t)
	f, _, _ := rulesFixture(t, "-", "deny "+main)
	f.claudeSessionAt(liveSid, main, "live session in the main checkout")
	gone := filepath.Join(realDir(t), "tmp.x1y2z3", "scratch")
	p := f.writeSession(goneSid, gone, claudeRecord(goneSid, gone, "", "scratch work", 1))
	spec := devicesync.SourceSpec{Path: p, Agent: transcript.AgentClaude, SessionKey: goneSid}
	if f.a.allowUpload(spec) {
		t.Error("a session in a gone directory may upload before the recovery pass looked at it")
	}
	f.once()
	got := f.placement(goneSid)
	if got.how != localindex.PlacedByCwd || got.main != "" || got.checked == 0 {
		t.Errorf("stored placement %+v", got)
	}
	if !f.a.allowUpload(spec) {
		t.Error("a checked session outside every rule may not upload")
	}
	if _, ok := f.rec.spec(p); !ok {
		t.Error("a checked session outside every rule was not handed to sync")
	}
	// Checked: a restart does not hold it again.
	f.restart()
	if !f.a.allowUpload(spec) {
		t.Error("a checked session was held again after a restart")
	}
}

// Rules match the symlink-resolved path too: /tmp is /private/tmp on
// macOS, and agents record either.
func TestRulesMatchResolvedSymlinks(t *testing.T) {
	base := realDir(t)
	real := filepath.Join(base, "real")
	link := filepath.Join(base, "link")
	for _, d := range []string{"proj1", "proj2"} {
		os.MkdirAll(filepath.Join(real, d), 0o755)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	f, _, _ := rulesFixture(t, "-", "deny "+filepath.Join(link, "proj1"), "deny "+filepath.Join(real, "proj2"))
	s1, s2 := "0b7e2c1a-0000-4000-8000-000000000111", "0b7e2c1a-0000-4000-8000-000000000112"
	f.claudeSessionAt(s1, filepath.Join(real, "proj1"), "recorded physical, rule on the link")
	f.claudeSessionAt(s2, filepath.Join(link, "proj2"), "recorded through the link, rule physical")
	f.once()
	for _, s := range []string{s1, s2} {
		if n := f.sessions(s); n != 0 {
			t.Errorf("%s: a session under a denied directory reached through a symlink was indexed", s)
		}
	}
}

// A working directory that is not absolute names nothing: the session is
// unplaceable, not placed.
func TestRelativeCwdIsUnplaceable(t *testing.T) {
	f, _, _ := rulesFixture(t, "-")
	f.cfg.Unplaceable = "exclude"
	f.restart()
	p := f.writeSession(goneSid, ".", claudeRecord(goneSid, ".", "", "dot cwd", 1))
	old := time.Now().Add(-time.Hour)
	os.Chtimes(p, old, old)
	f.once()
	if n := f.sessions(goneSid); n != 0 {
		t.Errorf("a session with cwd \".\" was indexed under unplaceable=exclude (%d rows)", n)
	}
	if got := f.placement(goneSid); got.how != localindex.PlacedByNone {
		t.Errorf("placed by %q", got.how)
	}
}

// A session first placed by a fallback that later names its directory is
// placed again from it, though its verdict was worked out before.
func TestFallbackReplacedWhenCwdAppears(t *testing.T) {
	base := realDir(t)
	open, secret := filepath.Join(base, "open"), filepath.Join(base, "secret")
	os.MkdirAll(open, 0o755)
	os.MkdirAll(secret, 0o755)
	f, _, _ := rulesFixture(t, "-", "deny "+secret)
	p := f.writeSession(goneSid, open, `{"type":"ai-title","title":"a title","sessionId":"`+goneSid+`"}`+"\n")
	old := time.Now().Add(-time.Hour)
	os.Chtimes(p, old, old)
	f.once()
	if got := f.placement(goneSid); got.how != localindex.PlacedByFolder {
		t.Fatalf("placed by %q", got.how)
	}
	f.rec = newRecorder()
	f.cfg.Sync = f.rec
	// Same agent: the verdict from the fallback is cached on the target.
	f.a.cfg.Sync = f.rec
	appendFile(t, p, claudeRecord(goneSid, secret, "", "secret line", 2))
	f.once()
	if n := f.sessions(goneSid); n != 0 {
		t.Errorf("a line naming a denied directory was indexed (%d rows)", n)
	}
	if _, ok := f.rec.spec(p); ok {
		t.Error("a session that named a denied directory was handed to sync")
	}
	if got := f.placement(goneSid); got.how != localindex.PlacedByCwd {
		t.Errorf("placed by %q", got.how)
	}
}

// A rule change resolves stored placements again when their directory
// still exists: a remote added since is seen.
func TestRuleChangeSeesNewRemote(t *testing.T) {
	needGit(t)
	dir := filepath.Join(realDir(t), "proj")
	os.MkdirAll(dir, 0o755)
	gitRun(t, dir, "init", "-q")
	f, file, _ := rulesFixture(t, "-")
	f.claudeSessionAt(liveSid, dir, "before the remote")
	f.once()
	if f.sessions(liveSid) == 0 {
		t.Fatal("fixture session not indexed")
	}
	gitRun(t, dir, "remote", "add", "origin", "git@github.com:acme/proj.git")
	writeRules(t, file, "deny repo:github.com/acme/*")
	f.once()
	if n := f.sessions(liveSid); n != 0 {
		t.Errorf("a session whose repo gained a denied remote was not purged (%d rows)", n)
	}
	if got := f.placement(liveSid); got.remote != "github.com/acme/proj" {
		t.Errorf("stored remote %q", got.remote)
	}
}

func TestWorktreeAddPath(t *testing.T) {
	for args, want := range map[string]string{
		"../wt -b feat":             "../wt",
		"-b feat ../wt":             "../wt",
		"-B feat --detach /abs/wt ": "/abs/wt",
		"--reason why -f ~/wt":      "~/wt",
		"'../quoted' main":          "../quoted",
		"$HOME/Code/x":              "$HOME/Code/x",
		"$WT":                       "",
		"<path> <branch>":           "",
		"-b only":                   "",
	} {
		if got := worktreeAddPath(args); got != want {
			t.Errorf("worktreeAddPath(%q) = %q, want %q", args, got, want)
		}
	}
}

func TestScanWorktreeAdds(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	body := `{"input":{"command":"git -C /r/app worktree add ../app-a -b a && ls"}}` + "\n" +
		`{"arguments":"{\"command\":[\"bash\",\"-lc\",\"git worktree add /w/app-b\"]}"}` + "\n" +
		`{"text":"use git worktree add to make one"}` + "\n"
	os.WriteFile(p, []byte(body), 0o600)
	got := scanWorktreeAdds(p, bufio.NewReaderSize(nil, 1<<20))
	want := []wtAdd{{"/r/app", "../app-a"}, {"", "/w/app-b"}, {"", "to"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A directory deleted and made again as another repository does not move
// the sessions that ran in the old one: a rule change re-resolves only to
// fill in what was unknown, so the deny rule on the old remote still
// covers them.
func TestRuleChangeKeepsPlacementOfReusedDirectory(t *testing.T) {
	main, wt := repoWithWorktree(t)
	f, file, _ := rulesFixture(t, "-", "deny repo:github.com/acme/app")
	cwd := filepath.Join(wt, "src")
	p := f.claudeSessionAt(wtSession, cwd, "secret work")
	f.once()
	if n := f.sessions(wtSession); n != 0 {
		t.Fatalf("fixture: a denied session was indexed (%d rows)", n)
	}
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	gitRun(t, main, "worktree", "prune")
	os.MkdirAll(cwd, 0o755)
	gitRun(t, wt, "init", "-q")
	gitRun(t, wt, "remote", "add", "origin", "git@github.com:other/pub.git")
	writeRules(t, file, "deny repo:github.com/acme/app", "local /nowhere/else")
	f.once()
	if got := f.placement(wtSession); got.main != main || got.remote != "github.com/acme/app" {
		t.Errorf("the rule change moved the session to the directory's new repository: %+v", got)
	}
	if n := f.sessions(wtSession); n != 0 {
		t.Errorf("a session of the denied repository was indexed after its directory was reused (%d rows)", n)
	}
	if f.a.allowUpload(devicesync.SourceSpec{Path: p, Agent: transcript.AgentClaude, SessionKey: wtSession}) {
		t.Error("a session of the denied repository may upload after its directory was reused")
	}
}

// A `git -C DIR worktree add` in a live transcript names DIR's repository
// only when the device already knows it: text in a transcript cannot add
// a repository to the candidates (and have git run in it).
func TestWorktreeAddIgnoresUnknownRepository(t *testing.T) {
	main, _ := repoWithWorktree(t)
	stranger := filepath.Join(filepath.Dir(main), "stranger")
	os.MkdirAll(stranger, 0o755)
	gitRun(t, stranger, "init", "-q")
	f, _, _ := rulesFixture(t, "-", "local /nowhere/else")
	add := fmt.Sprintf(`{"type":"assistant","cwd":%q,"sessionId":%q,"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"git -C %s worktree add ../stranger-wt"}}]},"uuid":"c3000000-0000-4000-8000-000000000001","timestamp":"2026-09-23T11:00:00.000Z"}`+"\n",
		main, liveSid, stranger)
	appendFile(t, f.claudeSessionAt(liveSid, main, "live session in the main checkout"), add)
	f.writeSession(goneSid, filepath.Join(filepath.Dir(main), "stranger-wt", "src"),
		claudeRecord(goneSid, filepath.Join(filepath.Dir(main), "stranger-wt", "src"), "", "work", 1))
	f.once()
	if got := f.placement(goneSid); got.main != "" || got.checked == 0 {
		t.Errorf("a repository only a transcript named became a candidate: %+v", got)
	}
}

// A transcript can claim anything. A bogus `git worktree add` naming a
// denied repository, next to a correct branch match to an allowed one,
// leaves both as candidates, and the rules apply with each: the session
// is treated as denied.
func TestAmbiguousRecoveryFailsClosed(t *testing.T) {
	main, wt := repoWithWorktree(t)
	secret := filepath.Join(filepath.Dir(main), "secret")
	os.MkdirAll(secret, 0o755)
	gitRun(t, secret, "init", "-q")
	f, _, _ := rulesFixture(t, "-", "deny "+secret)
	f.claudeSessionAt("0b7e2c1a-0000-4000-8000-000000000103", secret, "live session in the denied repo")
	cwd := filepath.Join(wt, "src")
	p := f.deletedWorktreeSession(main, wt, false, claudeRecord(goneSid, cwd, `"gitBranch":"app-fix-agent",`, "work in the allowed repo", 1))
	// The bogus claim: text in the allowed repo's live session says the
	// denied repository made the directory.
	bogus := fmt.Sprintf(`{"type":"assistant","cwd":%q,"sessionId":%q,"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"git -C %s worktree add ../app-fix-agent"}}]},"uuid":"c3000000-0000-4000-8000-000000000001","timestamp":"2026-09-23T11:00:00.000Z"}`+"\n",
		main, liveSid, secret)
	appendFile(t, filepath.Join(f.cfg.ClaudeProjects, encodeClaudeName(main), liveSid+".jsonl"), bogus)
	f.once()
	var cands string
	f.store.DB().QueryRow(`SELECT ifnull(candidates, '') FROM placements WHERE session_id = ?`, goneSid).Scan(&cands)
	got := f.placement(goneSid)
	if got.main != "" || got.checked == 0 || !strings.Contains(cands, main+"\t") || !strings.Contains(cands, secret+"\t") {
		t.Errorf("stored placement %+v, candidates %q", got, cands)
	}
	if n := f.sessions(goneSid); n != 0 {
		t.Errorf("a session with a denied candidate was indexed (%d rows)", n)
	}
	if _, ok := f.rec.spec(p); ok || f.a.allowUpload(devicesync.SourceSpec{Path: p, Agent: transcript.AgentClaude, SessionKey: goneSid}) {
		t.Error("a session with a denied candidate reached sync")
	}
	// A restart keeps the candidates, and a later check keeps them too.
	f.restart()
	f.once()
	if f.a.allowUpload(devicesync.SourceSpec{Path: p, Agent: transcript.AgentClaude, SessionKey: goneSid}) {
		t.Error("after a restart, a session with a denied candidate may upload")
	}
}

// foldPaths finds a lowercase path as the disk spells it, which a
// case-sensitive file system needs to resolve a rule's symlinks.
func TestFoldPaths(t *testing.T) {
	base := realDir(t)
	dir := filepath.Join(base, "Mixed", "Case")
	os.MkdirAll(dir, 0o755)
	low := strings.ToLower(filepath.Join(dir, "gone", "Deeper"))
	got := foldPaths(low)
	want := filepath.Join(dir, strings.ToLower(filepath.Join("gone", "Deeper")))
	if !slices.Contains(got, want) {
		t.Errorf("foldPaths(%q) = %q, want it to hold %q", low, got, want)
	}
}
