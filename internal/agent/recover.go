package agent

// Deleted worktrees. A session the agent first sees after its worktree
// was deleted names a directory that is gone, so git cannot say which
// main checkout it belonged to, and a path rule on that checkout misses
// it. A background pass (never on the indexing path) looks for the
// checkout among the repositories this device already knows, the main
// checkouts of stored placements, and records a main checkout only when
// exactly one repository fits:
//
//   - remote: the session recorded its git remote (Codex does); the one
//     local checkout whose origin is that remote.
//   - branch: a branch the session recorded (Claude's gitBranch, Codex's
//     git.branch; not main, master or HEAD) is a local or remote-tracking
//     branch of the repository, or a "checkout: moving from X to Y" in its
//     HEAD reflogs.
//   - worktree-add: a session in a live checkout ran `git worktree add`
//     naming the directory (by path, else by its last element).
//   - commit: a commit the session printed ("[branch abc1234]") is in the
//     repository's object store.
//
// The first applies to a session with a remote; the other three, taken
// together, to one without. The result is stored as the placement's main
// checkout and that checkout's remote. When more than one repository fits
// (with those of earlier checks), all are stored as candidates and the
// rules apply with each (fail closed): a transcript can claim anything,
// so a bogus claim may only tighten. Until the pass has looked at a
// session once, its placement is not settled: it is not uploaded, and not
// indexed while a deny rule could apply. A session not found is looked at
// again a day later, by the cheap signals only. Lookups per repository
// (refs, reflogs, objects) are made once per pass. The scan of live
// sessions for `git worktree add` reads every live transcript, so it runs
// only for sessions looked at for the first time (in practice the first
// pass on a device), and is cached per transcript by size and
// modification time for the life of the process.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/localindex"
)

const (
	// recoverEvery: a session whose checkout was not found is looked for
	// again after this.
	recoverEvery = 24 * time.Hour
	// recoverRescan: Run starts a pass at least this often, for sessions
	// due again, and at every sweep after a new session needs one.
	recoverRescan = time.Hour
	// recoverReadMax bounds how much of one transcript the pass reads.
	recoverReadMax = 256 << 20
)

// needsRecovery reports whether a placement is of a directory that is
// gone (or of a remote alone) with no main checkout known.
func needsRecovery(p placed) bool {
	if p.pl.Main != "" || p.how == localindex.PlacedByNone {
		return false
	}
	if p.pl.Cwd == "" {
		return p.pl.Remote != ""
	}
	_, err := os.Stat(p.pl.Cwd)
	return errors.Is(err, os.ErrNotExist)
}

// placeEntry is a stored placement and its session's transcript ("" when
// not tracked).
type placeEntry struct {
	key  placeKey
	p    placed
	path string
}

// recoverResult is what a pass did.
type recoverResult struct {
	Checked   int            // sessions looked at
	First     int            // ... for the first time (their placement was not settled)
	Found     map[string]int // by method: remote, branch, worktree-add, commit
	Ambiguous int            // more than one repository fitted
	NewCands  int            // ... and that added a candidate
	Repos     int            // candidate repositories
	Took      time.Duration
}

// replacePlace stores next as key's placement if it still is old.
func (a *Agent) replacePlace(key placeKey, old, next placed) bool {
	a.mu.Lock()
	cur, ok := a.places[key]
	if !ok || cur != old {
		a.mu.Unlock()
		return false
	}
	a.places[key] = next
	a.mu.Unlock()
	a.storePlace(key, next)
	return true
}

// recoverAndEnforce runs a pass and, when it settled or changed any
// placement, enforces the rules again with them. It reports whether it
// did.
func (a *Agent) recoverAndEnforce(ctx context.Context) bool {
	r := a.recoverPass(ctx)
	n := 0
	for _, v := range r.Found {
		n += v
	}
	if r.Checked > 0 {
		a.log.Info("agent: looked for the checkouts of deleted worktrees", "sessions", r.Checked, "found", r.Found,
			"ambiguous", r.Ambiguous, "repositories", r.Repos, "took", r.Took.Round(time.Millisecond))
	}
	if r.First == 0 && n == 0 && r.NewCands == 0 || ctx.Err() != nil || !a.policy().pol.HasRules() {
		return false // nothing changed that a rule could see
	}
	a.polMu.Lock()
	a.enforce(ctx, a.policy().pol)
	a.polMu.Unlock()
	return true
}

// maybeRecover starts a background pass when one is due and none runs:
// a placement saved since the last pass may need one, or (unless onlyNew)
// the last pass is recoverRescan old. done gets a value when the pass
// changed anything.
func (a *Agent) maybeRecover(ctx context.Context, done chan<- struct{}, onlyNew bool) {
	a.mu.Lock()
	due := a.recoverDue || !onlyNew && time.Since(a.recoverAt) >= recoverRescan
	a.mu.Unlock()
	if !due || !a.recoverRunning.CompareAndSwap(false, true) {
		return
	}
	a.bgWG.Add(1)
	go func() {
		defer a.bgWG.Done()
		defer a.recoverRunning.Store(false)
		if a.recoverAndEnforce(ctx) {
			select {
			case done <- struct{}{}:
			default:
			}
		}
	}()
}

// recoverPass looks for the main checkout of every placement that needs
// one and is due.
func (a *Agent) recoverPass(ctx context.Context) recoverResult {
	a.recoverMu.Lock()
	defer a.recoverMu.Unlock()
	t0 := time.Now()
	res := recoverResult{Found: map[string]int{}}
	type entry = placeEntry
	a.mu.Lock()
	a.recoverDue, a.recoverAt = false, t0
	paths := make(map[placeKey]string, len(a.targets))
	for _, t := range a.targets {
		if t.kind == kindTranscript {
			k, p := t.placeKeyOf()
			paths[k] = p
		}
	}
	all := make([]entry, 0, len(a.places))
	for k, p := range a.places {
		all = append(all, entry{k, p, paths[k]})
	}
	a.mu.Unlock()

	now := t0.UnixMilli()
	var due []entry
	mains := map[string]bool{}
	for _, e := range all {
		if e.p.pl.Main != "" {
			mains[e.p.pl.Main] = true
			continue
		}
		if (e.p.checked == 0 || now-e.p.checked >= recoverEvery.Milliseconds()) && needsRecovery(e.p) {
			due = append(due, e)
		}
	}
	if len(due) == 0 {
		return res
	}
	rc := newRepoCache(ctx, mains)
	res.Repos = len(rc.repos)
	var wt *wtIndex // built on first need
	for _, e := range due {
		if ctx.Err() != nil {
			break
		}
		next := e.p
		next.checked = now
		var repos []string
		how := ""
		if e.p.pl.Remote != "" {
			repos, how = rc.byRemote[e.p.pl.Remote], localindex.PlacedByRemote
		} else if len(rc.repos) > 0 {
			a.waitQuiet(ctx)
			sig := scanSignals(e.path)
			// The `git worktree add` that made the directory precedes the
			// session, so it is looked for once, at the first check: the
			// scan reads every live session's transcript.
			first := e.p.checked == 0
			if first && e.p.pl.Cwd != "" && wt == nil {
				wt = a.buildWtIndex(ctx, rc, all)
			}
			var wtRepos map[string]bool
			if first {
				wtRepos = wt.lookup(e.p.pl.Cwd)
			}
			sets := []struct {
				how   string
				repos map[string]bool
			}{
				{localindex.PlacedByBranch, rc.withBranch(sig.branches)},
				{localindex.PlacedByWorktreeAdd, wtRepos},
				{localindex.PlacedByCommit, rc.withCommit(sig.commits)},
			}
			union := map[string]bool{}
			for _, s := range sets {
				if len(s.repos) > 0 && how == "" {
					how = s.how
				}
				for r := range s.repos {
					union[r] = true
				}
			}
			for r := range union {
				repos = append(repos, r)
			}
		}
		// An earlier ambiguous check's candidates stay candidates: the
		// worktree-add scan runs only once, and a claim (true or bogus)
		// may only tighten.
		cs := candidates(e.p.cands)
		for _, r := range repos {
			cs = append(cs, candidate{r, rc.remote[r]})
		}
		merged := candidates(encodeCandidates(cs))
		switch {
		case len(merged) == 1:
			next.pl.Main, next.how, next.cands = merged[0].main, how, ""
			if how != localindex.PlacedByRemote {
				next.pl.Remote = merged[0].remote
			}
			res.Found[how]++
		case len(merged) > 1:
			// Not one main checkout: the rules apply with each.
			next.cands = encodeCandidates(merged)
			res.Ambiguous++
			if next.cands != e.p.cands {
				res.NewCands++
			}
		}
		if a.replacePlace(e.key, e.p, next) {
			res.Checked++
			if e.p.checked == 0 {
				res.First++
			}
		}
	}
	res.Took = time.Since(t0)
	return res
}

// waitQuiet yields to indexing: it waits (up to 2s) while jobs are queued
// or running.
func (a *Agent) waitQuiet(ctx context.Context) {
	for range 40 {
		a.mu.Lock()
		busy := a.busy > 0 || a.urgent.len()+a.normal.len() > 0
		a.mu.Unlock()
		if !busy || ctx.Err() != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// repoCache holds the candidate repositories and, per pass, what was
// looked up in each.
type repoCache struct {
	ctx      context.Context
	repos    []string            // main checkouts that exist, one per physical path
	remote   map[string]string   // repo -> normalized remote
	byRemote map[string][]string // normalized remote -> repos
	phys     map[string]string   // physical path -> repo
	git      bool                // git runs here
	branches map[string]map[string]bool
	objects  map[string]map[string]bool // repo -> hash -> is a commit there
}

func newRepoCache(ctx context.Context, mains map[string]bool) *repoCache {
	rc := &repoCache{ctx: ctx, remote: map[string]string{}, byRemote: map[string][]string{}, phys: map[string]string{},
		objects: map[string]map[string]bool{}}
	_, err := exec.LookPath("git")
	rc.git = err == nil
	for m := range mains {
		rc.add(m)
	}
	return rc
}

// add makes the main checkout m a candidate when it exists and is not one
// already known under another path.
func (rc *repoCache) add(m string) {
	fi, err := os.Stat(m)
	if err != nil || !fi.IsDir() {
		return
	}
	p := physicalPath(m)
	if _, ok := rc.phys[p]; ok {
		return
	}
	rc.phys[p] = m
	rc.repos = append(rc.repos, m)
	if r := localindex.RemoteOf(m); r != "" {
		rc.remote[m] = r
		rc.byRemote[r] = append(rc.byRemote[r], m)
	}
}

// known returns the candidate's name for the main checkout m, "" when m
// is not a candidate.
func (rc *repoCache) known(m string) string {
	if m == "" {
		return ""
	}
	return rc.phys[physicalPath(m)]
}

// gitOut runs git in dir with the options a background lookup wants.
func (rc *repoCache) gitOut(dir string, stdin io.Reader, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(rc.ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.fsmonitor=false"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	cmd.Stdin = stdin
	return cmd.Output()
}

// excludedBranches name no worktree.
var excludedBranches = map[string]bool{"main": true, "master": true, "HEAD": true}

// withBranch returns the repositories knowing any of the branches.
func (rc *repoCache) withBranch(branches []string) map[string]bool {
	if len(branches) == 0 {
		return nil
	}
	if rc.branches == nil {
		rc.branches = map[string]map[string]bool{}
		for _, r := range rc.repos {
			rc.branches[r] = rc.branchNames(r)
		}
	}
	out := map[string]bool{}
	for _, r := range rc.repos {
		for _, b := range branches {
			if rc.branches[r][b] {
				out[r] = true
			}
		}
	}
	return out
}

// branchNames reads a repository's local and remote-tracking branches and
// the branches its HEAD reflogs moved between.
func (rc *repoCache) branchNames(repo string) map[string]bool {
	names := map[string]bool{}
	if rc.git {
		out, _ := rc.gitOut(repo, nil, "for-each-ref", "--format=%(refname)", "refs/heads", "refs/remotes")
		for _, l := range strings.Split(string(out), "\n") {
			if n, ok := strings.CutPrefix(l, "refs/heads/"); ok {
				names[n] = true
			} else if n, ok := strings.CutPrefix(l, "refs/remotes/"); ok {
				if _, b, ok := strings.Cut(n, "/"); ok && b != "HEAD" {
					names[b] = true
				}
			}
		}
	}
	common := filepath.Join(repo, ".git")
	if fi, err := os.Stat(common); err != nil || !fi.IsDir() {
		common = repo // a bare repository
	}
	logs := []string{filepath.Join(common, "logs", "HEAD")}
	if wts, err := os.ReadDir(filepath.Join(common, "worktrees")); err == nil {
		for _, w := range wts {
			logs = append(logs, filepath.Join(common, "worktrees", w.Name(), "logs", "HEAD"))
		}
	}
	for _, l := range logs {
		reflogBranches(l, names)
	}
	return names
}

// reflogBranches adds the X and Y of every "checkout: moving from X to Y"
// in a reflog file.
func reflogBranches(path string, names map[string]bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return
	}
	sc := bufio.NewScanner(io.LimitReader(f, 64<<20))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		_, msg, ok := strings.Cut(sc.Text(), "\tcheckout: moving from ")
		if !ok {
			continue
		}
		from, to, ok := strings.Cut(msg, " to ")
		if !ok {
			continue
		}
		names[from] = true
		names[strings.TrimSpace(to)] = true
	}
}

// withCommit returns the repositories holding any of the commits.
func (rc *repoCache) withCommit(hashes []string) map[string]bool {
	if len(hashes) == 0 || !rc.git {
		return nil
	}
	out := map[string]bool{}
	for _, r := range rc.repos {
		have := rc.objects[r]
		if have == nil {
			have = map[string]bool{}
			rc.objects[r] = have
		}
		var ask []string
		for _, h := range hashes {
			if _, ok := have[h]; !ok {
				ask = append(ask, h)
			}
		}
		if len(ask) > 0 {
			got, _ := rc.gitOut(r, strings.NewReader(strings.Join(ask, "\n")+"\n"), "cat-file", "--batch-check")
			lines := strings.Split(string(got), "\n")
			for i, h := range ask {
				f := []string(nil)
				if i < len(lines) {
					f = strings.Fields(lines[i])
				}
				have[h] = len(f) >= 2 && f[1] == "commit"
			}
		}
		for _, h := range hashes {
			if have[h] {
				out[r] = true
			}
		}
	}
	return out
}

// signals are what a transcript recorded that can name its repository.
type signals struct {
	branches []string
	commits  []string
}

var (
	claudeBranchRe = regexp.MustCompile(`"gitBranch":"([^"\\]{1,200})"`)
	codexBranchRe  = regexp.MustCompile(`"git":\{[^{}]{0,1000}?"branch":"([^"\\]{1,200})"`)
	commitRe       = regexp.MustCompile(`\[[\w./-]{1,100} ([0-9a-f]{7,40})\]`)
)

// maxCommits bounds the commits taken from one transcript.
const maxCommits = 20

// scanSignals reads the branches and printed commits of a transcript.
func scanSignals(path string) signals {
	var s signals
	if path == "" {
		return s
	}
	f, err := os.Open(path)
	if err != nil {
		return s
	}
	defer f.Close()
	seenB, seenC := map[string]bool{}, map[string]bool{}
	br := bufio.NewReaderSize(io.LimitReader(f, recoverReadMax), 1<<20)
	for {
		line, err := br.ReadSlice('\n')
		if len(line) > 0 {
			for _, re := range []*regexp.Regexp{claudeBranchRe, codexBranchRe} {
				if !bytes.Contains(line, []byte(`ranch":"`)) {
					break
				}
				for _, m := range re.FindAllSubmatch(line, -1) {
					b := string(m[1])
					if !excludedBranches[b] && !seenB[b] {
						seenB[b] = true
						s.branches = append(s.branches, b)
					}
				}
			}
			if len(s.commits) < maxCommits && bytes.IndexByte(line, '[') >= 0 {
				for _, m := range commitRe.FindAllSubmatch(line, -1) {
					c := string(m[1])
					if !seenC[c] && len(s.commits) < maxCommits {
						seenC[c] = true
						s.commits = append(s.commits, c)
					}
				}
			}
		}
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return s
		}
	}
}

// wtIndex maps the directories `git worktree add` created, as live
// sessions ran it, to the repositories they ran it in.
type wtIndex struct {
	byPath map[string]map[string]bool // physical path -> repos
	byBase map[string]map[string]bool // last element -> repos
}

// wtScan is one transcript's `git worktree add` targets, cached by the
// file's size and modification time.
type wtScan struct {
	size, mod int64
	adds      []wtAdd
}

type wtAdd struct {
	dir  string // -C directory, as written ("" for none)
	path string // the new worktree's path, as written
}

// buildWtIndex scans the transcripts of sessions whose directory is a
// live checkout for `git worktree add`.
func (a *Agent) buildWtIndex(ctx context.Context, rc *repoCache, all []placeEntry) *wtIndex {
	idx := &wtIndex{byPath: map[string]map[string]bool{}, byBase: map[string]map[string]bool{}}
	home := a.cfg.Home
	live := map[string]bool{}
	var buf *bufio.Reader
	for _, e := range all {
		if ctx.Err() != nil {
			break
		}
		cwd := e.p.pl.Cwd
		if e.path == "" || e.p.pl.Main == "" || cwd == "" {
			continue
		}
		ok, seen := live[cwd]
		if !seen {
			fi, err := os.Stat(cwd)
			ok = err == nil && fi.IsDir()
			live[cwd] = ok
		}
		if !ok {
			continue
		}
		fi, err := os.Stat(e.path)
		if err != nil {
			continue
		}
		sc, hit := a.wtCache[e.path]
		if !hit || sc.size != fi.Size() || sc.mod != fi.ModTime().UnixNano() {
			a.waitQuiet(ctx)
			if buf == nil {
				buf = bufio.NewReaderSize(nil, 1<<20)
			}
			sc = wtScan{size: fi.Size(), mod: fi.ModTime().UnixNano(), adds: scanWorktreeAdds(e.path, buf)}
			a.wtCache[e.path] = sc
		}
		for _, add := range sc.adds {
			base, repo := cwd, e.p.pl.Main
			if add.dir != "" {
				// git -C DIR: DIR's repository, which must still exist
				// and be one the device knows. A transcript can say
				// anything: its text never adds a candidate.
				base = absUnder(cwd, expandHome(add.dir, home))
				repo = localindex.ResolveRepo(base).Main
			}
			repo = rc.known(repo)
			if repo == "" {
				continue
			}
			p := physicalPath(absUnder(base, expandHome(add.path, home)))
			addTo(idx.byPath, p, repo)
			if b := filepath.Base(p); len(b) >= 4 {
				addTo(idx.byBase, b, repo)
			}
		}
	}
	return idx
}

func addTo(m map[string]map[string]bool, k, v string) {
	if m[k] == nil {
		m[k] = map[string]bool{}
	}
	m[k][v] = true
}

// lookup returns the repositories a `git worktree add` of the deleted
// directory holding cwd ran in: by the path of the directory or of a
// deleted ancestor, else by the last element of the topmost deleted
// ancestor.
func (w *wtIndex) lookup(cwd string) map[string]bool {
	if w == nil || cwd == "" {
		return nil
	}
	p := physicalPath(cwd)
	top := ""
	for dir := p; ; {
		if _, err := os.Stat(dir); err == nil {
			break
		}
		if r := w.byPath[dir]; len(r) > 0 {
			return r
		}
		top = dir
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if top == "" {
		return nil
	}
	return w.byBase[filepath.Base(top)]
}

func expandHome(p, home string) string {
	if p == "~" {
		return home
	}
	for _, pre := range []string{"~/", "$HOME/", "${HOME}/"} {
		if rest, ok := strings.CutPrefix(p, pre); ok && home != "" {
			return filepath.Join(home, rest)
		}
	}
	return p
}

func absUnder(base, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(base, p)
}

var (
	wtAddNeedle = []byte("worktree add")
	wtAddRe     = regexp.MustCompile(`(?:-C\s+([^\s"'\\;&|]+)\s+)?worktree\s+add\s+([^"\\;&|\n` + "`" + `()<>]{1,300})`)
)

// scanWorktreeAdds finds the `git worktree add` commands in a transcript
// and returns each one's -C directory and path argument. Text in JSON
// strings is escaped, so a quote or a backslash ends a command.
func scanWorktreeAdds(path string, br *bufio.Reader) []wtAdd {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	br.Reset(io.LimitReader(f, recoverReadMax))
	var out []wtAdd
	seen := map[wtAdd]bool{}
	for {
		line, err := br.ReadSlice('\n')
		for off := 0; ; {
			i := bytes.Index(line[off:], wtAddNeedle)
			if i < 0 {
				break
			}
			i += off
			lo := max(0, i-300)
			hi := min(len(line), i+len(wtAddNeedle)+320)
			for _, m := range wtAddRe.FindAllSubmatch(line[lo:hi], -1) {
				if p := worktreeAddPath(string(m[2])); p != "" {
					add := wtAdd{dir: string(m[1]), path: p}
					if !seen[add] {
						seen[add] = true
						out = append(out, add)
					}
				}
			}
			off = i + len(wtAddNeedle)
		}
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return out
		}
	}
}

// worktreeAddPath returns the path argument of `git worktree add ARGS`:
// the first word that is not an option or an option's value.
func worktreeAddPath(args string) string {
	fs := strings.Fields(args)
	for i := 0; i < len(fs); i++ {
		w := strings.Trim(fs[i], `'`)
		rest := w
		for _, pre := range []string{"$HOME/", "${HOME}/"} {
			rest = strings.TrimPrefix(rest, pre)
		}
		switch {
		case w == "-b" || w == "-B" || w == "--reason":
			i++
			continue
		case strings.HasPrefix(w, "-"):
			continue
		case w == "" || strings.ContainsAny(rest, "$*{}%<>"):
			return ""
		}
		return w
	}
	return ""
}
