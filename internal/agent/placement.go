package agent

// Placement: where a session ran, for path rules (D18). For a session the
// agent resolves, once, when it first sees it:
//
//   - the working directory the transcript names (the first "cwd",
//     "payload.cwd" or <cwd> tag);
//   - the git worktree root and main checkout root holding it, and the
//     origin remote, read from the git files (localindex.ResolveRepo);
//   - the remote the transcript itself recorded (Codex session_meta
//     git.repository_url), used when git gives none, for example because
//     the directory is gone.
//
// When the transcript names no directory, the Claude project folder name
// (~/.claude/projects/-Users-me-Code-app) stands for it; a Codex rollout
// that recorded only its remote is placed by that. The result is stored
// (localindex placements), so rules keep matching a session after its
// worktree is deleted, and a rule change re-evaluates the stored
// placement (and resolves again those whose directory still exists). A
// session first seen after its worktree was deleted has no main checkout;
// a background pass looks for it (recover.go). A session with nothing at
// all is unplaceable and gets the unplaceable setting.
//
// A session may name more directories later (Claude records a cwd on
// every line; Codex a cwd per turn and in <cwd> tags). The parsers list
// them (transcript.Conversation.OtherCwds); each is resolved when first
// seen and added to the session's placement set (placed.others), and the
// rules apply the most restrictive verdict across the set. A directory
// that tightens the verdict stops further uploads and, for deny, purges
// the session's rows (widen).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/flopwire/flopwire/internal/fsprobe"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/provenance"
	"github.com/flopwire/flopwire/internal/transcript"
)

// placeKey identifies a session's placement, as conversations do.
type placeKey struct {
	agent   transcript.Agent
	session string
}

// placed is a stored placement, how it was found, and when the recovery
// pass (recover.go) last looked for its deleted worktree's main checkout
// (unix ms, 0 never).
type placed struct {
	pl      pathpolicy.Placement
	how     string
	checked int64
	// cands are the repositories an ambiguous recovery found, as stored
	// (localindex.Placement.Candidates). Rules are applied with each: a
	// transcript can claim anything, so a bogus claim may only tighten.
	cands string
	// others are the other directories the session named, each resolved
	// when first seen (localindex.Placement.OtherCwds). They only tighten.
	others string
}

// final reports whether the placement started from a directory the
// transcript named (then a later line cannot improve it). A session
// whose main checkout was linked through its remote but that named a
// directory is final too.
func (p placed) final() bool {
	return localindex.PlacedFromCwd(p.how) || p.how == localindex.PlacedByRemote && p.pl.Cwd != ""
}

// cwdWait is how long a transcript that has not named its directory yet
// is left to do so before a fallback places it. A live session names it
// in its first lines; a file this old without one never will.
var cwdWait = 2 * time.Minute // a var for tests

// placeOf returns the placement of the session key, whose transcript is
// at path (it may be gone). A placement found from the transcript's
// working directory is final; one from a fallback is replaced when the
// transcript later names its directory. A working directory that is not
// absolute (".") names nothing. known is false while a young transcript
// has not named its directory yet.
func (a *Agent) placeOf(key placeKey, path string) (placed, bool) {
	a.mu.Lock()
	p, stored := a.places[key]
	a.mu.Unlock()
	if stored && p.final() {
		return p, true
	}
	h := scanHints(path)
	if h.cwd != "" && filepath.IsAbs(h.cwd) {
		pl := a.resolve(h.cwd, h.remote)
		return a.savePlace(key, placed{pl: pl, how: cwdHow(pl)}), true
	}
	if stored {
		return p, true
	}
	if h.state == hintsPending {
		if fi, err := fsprobe.Stat(path); err == nil && time.Since(fi.ModTime()) < cwdWait {
			return placed{}, false
		}
	}
	pl, how := a.fallback(key.agent, path, h.remote)
	return a.savePlace(key, placed{pl: pl, how: how}), true
}

// cwdHow is how a placement resolved from a named directory was found.
func cwdHow(pl pathpolicy.Placement) string {
	if pl.Worktree != "" && pl.Main != "" && pl.Worktree != pl.Main {
		return localindex.PlacedByWorktree
	}
	return localindex.PlacedByCwd
}

// settled reports whether rules can be applied to a placement now. While
// any rule is in force, a session whose directory is gone and whose main
// checkout is unknown waits for the recovery pass to look for it once
// (recover.go): deciding before could upload a session that a rule on its
// main checkout covers.
func settled(pol pathpolicy.Policy, p placed) bool {
	return !pol.HasRules() || p.checked != 0 || !needsRecovery(p)
}

// decide applies the rules to a placement, to it with each candidate
// repository of an ambiguous recovery as its main checkout, and to each
// other directory the session named, and keeps the most restrictive
// verdict.
func (a *Agent) decide(pol pathpolicy.Policy, p placed) pathpolicy.Decision {
	d := a.decideOne(pol, p)
	for _, o := range decodeOthers(p.others) {
		if d2 := a.decideOne(pol, placed{pl: o, how: cwdHow(o)}); d2.Mode > d.Mode {
			d = d2
		}
	}
	for _, c := range candidates(p.cands) {
		q := p
		q.pl.Main = c.main
		if q.pl.Remote == "" {
			q.pl.Remote = c.remote
		}
		if d2 := a.decideOne(pol, q); d2.Mode > d.Mode {
			d = d2
		}
	}
	return d
}

// candidate is one repository of an ambiguous recovery.
type candidate struct{ main, remote string }

// candidates decodes placed.cands.
func candidates(s string) []candidate {
	var out []candidate
	for _, l := range strings.Split(s, "\n") {
		if m, r, _ := strings.Cut(l, "\t"); m != "" {
			out = append(out, candidate{m, r})
		}
	}
	return out
}

// encodeCandidates is the placed.cands of cs, sorted and without
// duplicates.
func encodeCandidates(cs []candidate) string {
	set := map[string]bool{}
	for _, c := range cs {
		set[c.main+"\t"+c.remote] = true
	}
	lines := make([]string, 0, len(set))
	for l := range set {
		lines = append(lines, l)
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

// decodeOthers decodes placed.others.
func decodeOthers(s string) []pathpolicy.Placement {
	var out []pathpolicy.Placement
	for _, l := range strings.Split(s, "\n") {
		f := strings.Split(l, "\t")
		if len(f) == 4 && f[0] != "" {
			out = append(out, pathpolicy.Placement{Cwd: f[0], Worktree: f[1], Main: f[2], Remote: f[3]})
		}
	}
	return out
}

// encodeOthers is the placed.others of ps.
func encodeOthers(ps []pathpolicy.Placement) string {
	lines := make([]string, 0, len(ps))
	for _, p := range ps {
		lines = append(lines, p.Cwd+"\t"+p.Worktree+"\t"+p.Main+"\t"+p.Remote)
	}
	return strings.Join(lines, "\n")
}

// mergeOthers is the union of two placed.others, each directory once (the
// first resolution kept), in order of appearance.
func mergeOthers(a, b string) string {
	if a == "" || a == b {
		return b
	}
	if b == "" {
		return a
	}
	all := decodeOthers(a)
	seen := map[string]bool{}
	for _, o := range all {
		seen[o.Cwd] = true
	}
	for _, o := range decodeOthers(b) {
		if !seen[o.Cwd] {
			seen[o.Cwd] = true
			all = append(all, o)
		}
	}
	return encodeOthers(all)
}

// addCwds adds the directories a session named to its placement set
// (placed.others), resolving each new one, and returns the placement and
// whether the set grew. Directories that are not absolute, or that are the
// placement's own, are ignored. path is the transcript that places the
// session (placeOf).
func (a *Agent) addCwds(key placeKey, path string, cwds []string) (placed, bool) {
	p, ok := a.storedPlace(key)
	if !ok {
		if p, ok = a.placeOf(key, path); !ok {
			p, _ = a.storedPlace(key) // still young: extend what is stored, if anything
		}
	}
	have := map[string]bool{p.pl.Cwd: true}
	for _, o := range decodeOthers(p.others) {
		have[o.Cwd] = true
	}
	var add []pathpolicy.Placement
	for _, c := range cwds {
		if c == "" || !filepath.IsAbs(c) || have[c] || strings.ContainsAny(c, "\t\n") {
			continue
		}
		have[c] = true
		add = append(add, a.resolve(c, ""))
	}
	if len(add) == 0 {
		return p, false
	}
	np := p
	np.others = mergeOthers(p.others, encodeOthers(add))
	if !ok {
		// Never placed: the first directory places it, as a transcript
		// naming it on its first line would.
		np = placed{pl: add[0], how: cwdHow(add[0]), others: encodeOthers(add[1:])}
	}
	return a.savePlace(key, np), true
}

// decideOne applies the rules to a placement, as recorded and with
// symlinks resolved (on macOS /tmp is /private/tmp; agents record either),
// and keeps the more restrictive verdict.
func (a *Agent) decideOne(pol pathpolicy.Policy, p placed) pathpolicy.Decision {
	d := decidePlaced(pol, p)
	rp := p
	rp.pl.Cwd, rp.pl.Worktree, rp.pl.Main = a.physical(p.pl.Cwd), a.physical(p.pl.Worktree), a.physical(p.pl.Main)
	if rp.pl != p.pl {
		if d2 := decidePlaced(pol, rp); d2.Mode > d.Mode {
			d = d2
		}
	}
	return d
}

// physical is physicalPath, memoized until the rules change.
func (a *Agent) physical(p string) string {
	if p == "" {
		return ""
	}
	a.mu.Lock()
	r, ok := a.phys[p]
	a.mu.Unlock()
	if ok {
		return r
	}
	r = physicalPath(p)
	a.mu.Lock()
	if len(a.phys) > 1<<16 {
		clear(a.phys)
	}
	a.phys[p] = r
	a.mu.Unlock()
	return r
}

// physicalPath resolves the symlinks of an absolute path: those of its
// longest existing ancestor, with the rest appended (the directory may be
// gone). Anything else is returned as is.
func physicalPath(p string) string {
	if !filepath.IsAbs(p) {
		return p
	}
	p = filepath.Clean(p)
	rest := ""
	for dir := p; ; {
		if r, err := fsprobe.EvalSymlinks(dir); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return p
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}

// decidePlaced applies the rules to a placement. A placement decoded from
// a Claude project folder name is also checked against the name itself
// (pathpolicy.Policy.DecideFolder).
func decidePlaced(pol pathpolicy.Policy, p placed) pathpolicy.Decision {
	d := pol.Decide(p.pl)
	if p.how != localindex.PlacedByFolder || p.pl.Cwd == "" {
		return d
	}
	return pol.DecideFolder(d, encodeClaudeName(p.pl.Cwd))
}

// fallback places a session whose transcript names no directory: a
// Claude session by its project folder, a Codex session by the remote it
// recorded.
func (a *Agent) fallback(agent transcript.Agent, path, remote string) (pathpolicy.Placement, string) {
	if agent == transcript.AgentClaude {
		if cwd := a.claudeFolderCwd(path); cwd != "" {
			return a.resolve(cwd, remote), localindex.PlacedByFolder
		}
	}
	if remote != "" {
		return pathpolicy.Placement{Remote: remote}, localindex.PlacedByRemote
	}
	return pathpolicy.Placement{}, localindex.PlacedByNone
}

// resolve places a working directory: its repo from git, and the remote
// the transcript recorded when git has none. The git files are read each
// time (not memoized per process): a session is placed once, and a remote
// or worktree added while the agent runs must cover the next session.
func (a *Agent) resolve(cwd, remote string) pathpolicy.Placement {
	r := localindex.ResolveRepo(cwd)
	pl := pathpolicy.Placement{Cwd: cwd, Worktree: r.Worktree, Main: r.Main, Remote: r.Remote}
	if pl.Remote == "" {
		pl.Remote = remote
	}
	return pl
}

// savePlace records a placement in memory and in the index. An unchanged
// placement keeps when it was last checked, and every placement keeps the
// candidates of an ambiguous recovery (they only tighten).
func (a *Agent) savePlace(key placeKey, p placed) placed {
	a.mu.Lock()
	old, ok := a.places[key]
	if ok && old.cands != "" {
		p.cands = encodeCandidates(append(candidates(old.cands), candidates(p.cands)...))
	}
	if ok {
		p.others = mergeOthers(old.others, p.others)
	}
	if ok && p.checked == 0 && old.pl == p.pl && old.how == p.how {
		p.checked = old.checked
	}
	a.places[key] = p
	if p.checked == 0 && p.pl.Main == "" {
		a.recoverDue = true
	}
	a.mu.Unlock()
	if ok && old == p {
		return p
	}
	a.storePlace(key, p)
	return p
}

// storePlace writes a placement to the index.
func (a *Agent) storePlace(key placeKey, p placed) {
	if err := a.store.SavePlacement(context.Background(), localindex.Placement{Agent: key.agent, SessionID: key.session,
		Placement: p.pl, How: p.how, CheckedAt: p.checked, Candidates: p.cands, OtherCwds: p.others}); err != nil {
		a.log.Warn("agent: saving a session placement", "session", key.session, "err", err)
	}
}

// loadPlaces reads the stored placements. Called from New and load.
func (a *Agent) loadPlaces(ctx context.Context) error {
	ps, err := a.store.Placements(ctx)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range ps {
		a.places[placeKey{p.Agent, p.SessionID}] = placed{p.Placement, p.How, p.CheckedAt, p.Candidates, p.OtherCwds}
	}
	a.placesOK = true
	return nil
}

// storedPlace returns the stored placement of a session, if any.
func (a *Agent) storedPlace(key placeKey) (placed, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.places[key]
	return p, ok
}

// claudeFolderCwd decodes the Claude project folder holding path
// (<projects>/<folder>/...) back to the directory it names.
func (a *Agent) claudeFolderCwd(path string) string {
	rel, err := filepath.Rel(a.cfg.ClaudeProjects, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return ""
	}
	folder, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
	a.mu.Lock()
	cwd, ok := a.folders[folder]
	a.mu.Unlock()
	if ok {
		return cwd
	}
	cwd = decodeClaudeFolder(folder)
	a.mu.Lock()
	a.folders[folder] = cwd
	a.mu.Unlock()
	return cwd
}

// encodeClaudeName is how Claude Code names a project folder.
func encodeClaudeName(s string) string { return pathpolicy.ClaudeFolderName(s) }

// decodeClaudeFolder turns a Claude project folder name back into a path.
// The encoding is lossy ('/', '.', '-' and more all became '-'), so it
// walks the filesystem from the root, taking at each level the entries
// whose encoded name is a prefix of what is left, and returns the first
// complete existing path. Failing that, it extends the longest existing
// prefix with the rest read naively ('-' as '/'). A name that does not
// start with '-' (not an absolute path) decodes to "".
func decodeClaudeFolder(name string) string {
	if !strings.HasPrefix(name, "-") || len(name) < 2 {
		return ""
	}
	budget := 256 // directory listings
	bestDir, bestRest := "/", name[1:]
	var walk func(dir, rest string) string
	walk = func(dir, rest string) string {
		if budget <= 0 {
			return ""
		}
		budget--
		ents, err := fsprobe.ReadDir(dir)
		if err != nil {
			return ""
		}
		type cand struct{ name, enc string }
		var cands []cand
		for _, e := range ents {
			enc := encodeClaudeName(e.Name())
			if rest == enc || strings.HasPrefix(rest, enc+"-") {
				cands = append(cands, cand{e.Name(), enc})
			}
		}
		// Longer names first: "my-app" before "my" when both fit.
		sort.Slice(cands, func(i, j int) bool { return len(cands[i].enc) > len(cands[j].enc) })
		for _, c := range cands {
			p := filepath.Join(dir, c.name)
			if rest == c.enc {
				return p
			}
			left := rest[len(c.enc)+1:]
			if fi, err := fsprobe.Stat(p); err != nil || !fi.IsDir() {
				continue
			}
			if len(left) < len(bestRest) {
				bestDir, bestRest = p, left
			}
			if got := walk(p, left); got != "" {
				return got
			}
		}
		return ""
	}
	if got := walk("/", name[1:]); got != "" {
		return got
	}
	return filepath.Join(bestDir, strings.ReplaceAll(bestRest, "-", "/"))
}

// hintsState says how far a transcript scan got.
type hintsState uint8

const (
	hintsPending hintsState = iota // unreadable, or ended before a directory and before cwdScanLines lines
	hintsNone                      // cwdScanLines lines and no directory: the transcript has none
	hintsFound
)

type hints struct {
	cwd    string
	remote string // normalized
	state  hintsState
}

// hintReaders hold scanHints' buffers. Codex session_meta lines (with the
// remote) run to about 23KB.
var hintReaders = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, 128<<10) }}

// cwdScanLines bounds how far into a transcript scanHints looks.
const cwdScanLines = 200

// scanHints reads what a JSONL transcript records about its place: the
// first "cwd" (Claude, every record), "payload.cwd" (Codex session_meta)
// or <cwd> tag (older Codex), and the git remote a Codex session recorded
// (git.repository_url on the first line of an old rollout, or
// payload.git.repository_url in session_meta), within its first
// cwdScanLines complete lines.
func scanHints(path string) hints {
	var h hints
	f, err := fsprobe.Open(path)
	if err != nil {
		return h
	}
	defer f.Close()
	br := hintReaders.Get().(*bufio.Reader)
	defer hintReaders.Put(br)
	br.Reset(f)
	for range cwdScanLines {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			for errors.Is(err, bufio.ErrBufferFull) { // a huge record: skip it
				_, err = br.ReadSlice('\n')
			}
			if err != nil {
				return h
			}
			continue
		}
		if errors.Is(err, io.EOF) || err != nil {
			return h
		}
		if h.remote == "" && bytes.Contains(line, []byte(`"repository_url"`)) {
			var rec struct {
				Git *struct {
					RepositoryURL string `json:"repository_url"`
				} `json:"git"`
				Payload struct {
					Git *struct {
						RepositoryURL string `json:"repository_url"`
					} `json:"git"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &rec) == nil {
				raw := ""
				if rec.Git != nil {
					raw = rec.Git.RepositoryURL
				} else if rec.Payload.Git != nil {
					raw = rec.Payload.Git.RepositoryURL
				}
				if n, err := provenance.NormalizeRemote(raw); err == nil {
					h.remote = n
				}
			}
		}
		if cwd := cwdTag(line); cwd != "" {
			h.cwd, h.state = cwd, hintsFound
			return h
		}
		if !bytes.Contains(line, []byte(`"cwd"`)) {
			continue
		}
		var rec struct {
			Cwd     string `json:"cwd"`
			Payload struct {
				Cwd string `json:"cwd"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		if rec.Cwd == "" {
			rec.Cwd = rec.Payload.Cwd
		}
		if rec.Cwd != "" {
			h.cwd, h.state = rec.Cwd, hintsFound
			return h
		}
	}
	h.state = hintsNone
	return h
}

// cwdTag reads the directory from an <environment_context> block's
// <cwd>...</cwd> inside a JSON string (older Codex rollouts have no
// session_meta cwd, only this), in either escaping of '<'.
func cwdTag(line []byte) string {
	for _, tag := range [][2]string{{"<cwd>", "</cwd>"}, {`\u003ccwd\u003e`, `\u003c/cwd\u003e`}} {
		i := bytes.Index(line, []byte(tag[0]))
		if i < 0 {
			continue
		}
		rest := line[i+len(tag[0]):]
		j := bytes.Index(rest, []byte(tag[1]))
		if j <= 0 {
			continue
		}
		var cwd string
		if json.Unmarshal(append(append([]byte{'"'}, rest[:j]...), '"'), &cwd) == nil {
			return strings.TrimSpace(cwd)
		}
	}
	return ""
}
