package local

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/retrieval/format"
)

// ExpandRepo resolves a --repo argument to what the retrieval filters
// match (Filters.Repo and Filters.RepoRoots). One repository is one repo
// however it is checked out: its identity is its main checkout (the bare
// repository for a bare-backed layout), compared with symlinks resolved,
// and, when it has one, its normalized remote. dirs are the placements
// the local index holds (localindex.Store.RepoDirs); nil knows none.
//
//   - A path (absolute, ".", "./x", "../x") names the repository holding
//     it. repo is its worktree root (the path itself outside git), and
//     roots are every checkout of that repository: the main checkout,
//     the live linked worktrees git lists, and the directories of every
//     placement whose main checkout or remote is the repository's. A
//     path that no longer exists names the repository of the placements
//     at or above it when they agree on one.
//   - A short form (a name such as "app", or owner/name, or
//     host/owner/name) names the one repository the placements know by
//     that name or remote. Two or more is an error that lists them.
//     None leaves the name to match the last element of a session's
//     directory, as before. team keeps the name in repo as well, for the
//     server, which matches it across the team.
//   - A glob passes through.
func ExpandRepo(arg string, dirs []localindex.RepoDir, team bool) (repo string, roots []string, err error) {
	return expandRepo(arg, dirs, team, nil)
}

// ServerRepo is ExpandRepo for a request to the server, without the
// checkouts the path rules keep off it: uploads reports whether a
// session placed at a directory may reach the server
// (agent.Uploads). The server audits every query, so a checkout under a
// local or deny rule is never named in one. A root is kept only when
// every main checkout and remote of the repository allows it; a rule on
// the main checkout therefore leaves only the argument itself (repo).
func ServerRepo(arg string, dirs []localindex.RepoDir, uploads func(pathpolicy.Placement) bool) (repo string, roots []string, err error) {
	return expandRepo(arg, dirs, true, uploads)
}

func expandRepo(arg string, dirs []localindex.RepoDir, team bool, uploads func(pathpolicy.Placement) bool) (repo string, roots []string, err error) {
	arg = strings.TrimSpace(arg)
	if arg == "" || strings.ContainsAny(arg, "*?[") {
		return arg, nil, nil
	}
	x := &expander{canon: map[string]string{}, dirs: dirs, uploads: uploads}
	if isPathArg(arg) {
		abs, err := filepath.Abs(arg)
		if err != nil {
			return arg, nil, nil
		}
		repo, roots = x.path(abs)
		if team {
			roots = wireRoots(roots)
		}
		return repo, roots, nil
	}
	groups := x.named(arg)
	switch len(groups) {
	case 0:
		return arg, nil, nil
	case 1:
		roots = x.rootsOf(groups[0], nil, "")
		if team {
			return arg, wireRoots(roots), nil
		}
		return "", roots, nil
	}
	labels := make([]string, 0, len(groups))
	for _, g := range groups {
		labels = append(labels, g.label())
	}
	slices.Sort(labels)
	return arg, nil, fmt.Errorf("%w: repo %q names %d repositories: %s; pass its path or owner/name", format.ErrBadRequest, arg, len(groups), strings.Join(labels, "; "))
}

// isPathArg reports whether a --repo argument is a path.
func isPathArg(s string) bool {
	return filepath.IsAbs(s) || s == "." || s == ".." || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../")
}

type expander struct {
	canon map[string]string
	dirs  []localindex.RepoDir
	// uploads, for a request to the server, keeps only the roots the
	// path rules let reach it (ServerRepo); nil keeps every root.
	uploads func(pathpolicy.Placement) bool
}

// real is p with symlinks resolved, or p cleaned when it does not exist.
func (x *expander) real(p string) string {
	if p == "" {
		return ""
	}
	if r, ok := x.canon[p]; ok {
		return r
	}
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		r = filepath.Clean(p)
	}
	x.canon[p] = r
	return r
}

// ident is one repository: its main checkouts (resolved) and remotes.
type ident struct {
	mains   map[string]bool
	remotes map[string]bool
}

func newIdent() *ident { return &ident{mains: map[string]bool{}, remotes: map[string]bool{}} }

func (id *ident) add(x *expander, main, remote string) {
	if main != "" {
		id.mains[x.real(main)] = true
	}
	if remote != "" {
		id.remotes[remote] = true
	}
}

func (id *ident) has(x *expander, d localindex.RepoDir) bool {
	return d.Main != "" && id.mains[x.real(d.Main)] || d.Remote != "" && id.remotes[d.Remote]
}

func (id *ident) empty() bool { return len(id.mains)+len(id.remotes) == 0 }

func (id *ident) label() string {
	mains := slices.Sorted(mapKeys(id.mains))
	remotes := slices.Sorted(mapKeys(id.remotes))
	switch {
	case len(mains) == 0:
		return strings.Join(remotes, ", ")
	case len(remotes) == 0:
		return strings.Join(mains, ", ")
	}
	return strings.Join(mains, ", ") + " (" + strings.Join(remotes, ", ") + ")"
}

func mapKeys(m map[string]bool) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// path resolves a path argument.
func (x *expander) path(abs string) (string, []string) {
	abs = filepath.Clean(abs)
	id := newIdent()
	repo := abs
	var seeds []string
	if _, err := os.Stat(abs); err == nil {
		if r := localindex.ResolveRepo(abs); r.Worktree != "" {
			repo = r.Worktree
			seeds = append(seeds, r.Worktree, r.Main)
			id.add(x, r.Main, r.Remote)
		} else if localindex.IsBare(abs) {
			seeds = append(seeds, abs)
			id.add(x, abs, localindex.RemoteOf(abs))
		}
	}
	if id.empty() {
		// Outside git, or gone: a deleted main checkout or bare
		// repository is still named by its path, and a deleted worktree
		// by the placements recorded at or above it, when they agree.
		seeds = append(seeds, abs)
		id.add(x, abs, "")
		ra := x.real(abs)
		var found []*ident
		for _, d := range x.dirs {
			if rd := x.real(d.Dir); d.Dir != "" && (ra == rd || strings.HasPrefix(ra, rd+"/")) {
				one := newIdent()
				one.add(x, d.Main, d.Remote)
				found = append(found, one)
			}
		}
		if g := x.group(found); len(g) == 1 {
			for m := range g[0].mains {
				id.mains[m] = true
			}
			for r := range g[0].remotes {
				id.remotes[r] = true
			}
		}
	}
	return repo, x.rootsOf(id, seeds, abs)
}

// rootsOf lists the checkout roots of repository id: seeds first, then
// the live linked worktrees of each main checkout, then the directories
// of the placements in id (closing over the main checkouts and remotes
// they add: a clone of the same remote is the same repository). Each
// root also appears with its symlinks resolved and, when arg reached its
// directory through a symlink (/tmp for /private/tmp), as reached that
// way.
func (x *expander) rootsOf(id *ident, seeds []string, arg string) []string {
	var roots []string
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && filepath.IsAbs(p) && !seen[p] {
			seen[p] = true
			roots = append(roots, p)
		}
	}
	for _, s := range seeds {
		add(s)
	}
	used := make([]bool, len(x.dirs))
	for changed := true; changed; {
		changed = false
		for i, d := range x.dirs {
			if used[i] || !id.has(x, d) {
				continue
			}
			used[i], changed = true, true
			n := len(id.mains) + len(id.remotes)
			id.add(x, d.Main, d.Remote)
			changed = changed || n != len(id.mains)+len(id.remotes)
		}
	}
	for _, m := range slices.Sorted(mapKeys(id.mains)) {
		add(m)
		for _, w := range localindex.Worktrees(m) {
			add(w)
		}
	}
	for i, d := range x.dirs {
		if used[i] {
			add(d.Dir)
			add(d.Main)
		}
	}
	lp, rp := symlinkPrefixes(arg, x.real(arg))
	for _, r := range slices.Clone(roots) {
		add(x.real(r))
		if rp != "" && (r == rp || strings.HasPrefix(r, rp+"/")) {
			add(lp + r[len(rp):])
		}
	}
	if x.uploads != nil {
		roots = slices.DeleteFunc(roots, func(r string) bool { return !x.allowed(id, r) })
	}
	return roots
}

// allowed reports whether a session at root, on repository id, may reach
// the server under each of the repository's main checkouts and remotes.
func (x *expander) allowed(id *ident, root string) bool {
	mains, remotes := slices.Collect(mapKeys(id.mains)), slices.Collect(mapKeys(id.remotes))
	if len(mains) == 0 {
		mains = []string{""}
	}
	if len(remotes) == 0 {
		remotes = []string{""}
	}
	for _, m := range mains {
		for _, r := range remotes {
			if !x.uploads(pathpolicy.Placement{Cwd: root, Worktree: root, Main: m, Remote: r}) {
				return false
			}
		}
	}
	return true
}

// wireRoots fits roots to a request (format.MaxRepoRoots): a root under
// another one adds nothing and goes first; then the last ones go, so the
// argument's own checkouts and the live worktrees stay.
func wireRoots(roots []string) []string {
	if len(roots) <= format.MaxRepoRoots {
		return roots
	}
	var out []string
	for _, r := range roots {
		under := false
		for _, o := range roots {
			if o != r && strings.HasPrefix(r, strings.TrimSuffix(o, "/")+"/") {
				under = true
				break
			}
		}
		if !under {
			out = append(out, r)
		}
	}
	if len(out) > format.MaxRepoRoots {
		out = out[:format.MaxRepoRoots]
	}
	return out
}

// symlinkPrefixes returns the leading parts in which logical and real
// (logical with symlinks resolved) differ: for /tmp/x and /private/tmp/x,
// "" and "/private". Both are "" when they do not differ.
func symlinkPrefixes(logical, real string) (lp, rp string) {
	if logical == "" || logical == real {
		return "", ""
	}
	l := strings.Split(strings.Trim(logical, "/"), "/")
	r := strings.Split(strings.Trim(real, "/"), "/")
	n := 0
	for n < len(l) && n < len(r) && l[len(l)-1-n] == r[len(r)-1-n] {
		n++
	}
	lp = "/" + strings.Join(l[:len(l)-n], "/")
	rp = "/" + strings.Join(r[:len(r)-n], "/")
	if rp == "/" || lp == rp {
		return "", ""
	}
	return strings.TrimSuffix(lp, "/"), rp
}

// group merges identities that share a main checkout or remote.
func (x *expander) group(ids []*ident) []*ident {
	var out []*ident
	for _, id := range ids {
		if id.empty() {
			continue
		}
		merged := id
		keep := out[:0]
		for _, o := range out {
			if overlaps(o, merged) {
				for m := range o.mains {
					merged.mains[m] = true
				}
				for r := range o.remotes {
					merged.remotes[r] = true
				}
				continue
			}
			keep = append(keep, o)
		}
		out = append(keep, merged)
	}
	return out
}

func overlaps(a, b *ident) bool {
	for m := range a.mains {
		if b.mains[m] {
			return true
		}
	}
	for r := range a.remotes {
		if b.remotes[r] {
			return true
		}
	}
	return false
}

// named returns the repositories the placements know by name: the last
// element of a remote or main checkout (RepoName) without a slash;
// owner/name or host/owner/name, a remote that ends with it, or a main
// checkout whose path does, with one.
func (x *expander) named(name string) []*ident {
	name = strings.TrimSuffix(name, "/")
	var all []*ident
	for _, d := range x.dirs {
		one := newIdent()
		one.add(x, d.Main, d.Remote)
		all = append(all, one)
	}
	var out []*ident
	for _, g := range x.group(all) {
		if g.named(name) {
			out = append(out, g)
		}
	}
	return out
}

func (id *ident) named(name string) bool {
	slash := strings.Contains(name, "/")
	lower := strings.ToLower(name)
	for r := range id.remotes {
		if !slash && strings.EqualFold(localindex.RepoName("", r), name) ||
			slash && (strings.EqualFold(r, name) || strings.HasSuffix(strings.ToLower(r), "/"+lower)) {
			return true
		}
	}
	for m := range id.mains {
		if !slash && strings.EqualFold(localindex.RepoName(m, ""), name) ||
			slash && strings.HasSuffix(strings.ToLower(m), "/"+lower) {
			return true
		}
	}
	return false
}
