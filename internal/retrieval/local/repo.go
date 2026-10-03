package local

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/retrieval/format"
)

// Repo is what a --repo argument resolves to: the retrieval filters
// Filters.Repo, RepoRoots, RepoMains and RepoRemotes. Every list is
// small, whatever the number of checkouts: a session is matched by the
// repository its placement records (its main checkout or remote), not by
// enumerating the directories it may have run in (#102).
type Repo struct {
	// Repo is a path matched as a prefix (the argument's worktree root,
	// or the path itself outside git), a glob, or, for the server, a
	// name the device does not know. "" for a resolved name.
	Repo string
	// Roots are directories matched as prefixes: the argument's worktree
	// root and the repository's main checkouts, also with symlinks
	// resolved and as the argument reached them.
	Roots []string
	// Mains are the repository's main checkouts, as placements recorded
	// them and resolved: a session placed in one matches.
	Mains []string
	// Remotes are its normalized remotes: a session placed in a checkout
	// of one matches, on any device.
	Remotes []string
}

// ExpandRepo resolves a --repo argument. One repository is one repo
// however it is checked out: its identity is its main checkout (the bare
// repository for a bare-backed layout), compared with symlinks resolved
// and, where the file system ignores case, in the case it has on disk,
// and, when it has one, its normalized remote. dirs are the placements
// the local index holds (localindex.Store.RepoDirs); nil knows none.
//
//   - A path (absolute, ".", "./x", "../x") names the repository holding
//     it: every session placed in it (its main checkouts and remotes,
//     closed over the placements: a clone of the same remote is the same
//     repository), and every session under its worktree root or main
//     checkout. A path that no longer exists names the repository of the
//     placements at or above it when they agree on one.
//   - A short form (a name such as "app", or owner/name, or
//     host/owner/name) names the one repository the placements know by
//     that name or remote; Repo is then "". None, or two or more, is an
//     error that says so: a name never falls back to the last element of
//     a session's directory, which would merge two repositories of one
//     name. For the server (team), a name the device does not know
//     passes through as Repo, and the server resolves it among the
//     uploaded placements (retrieval.resolveFilterRepo).
//   - A glob passes through.
//
// For the server each list must fit format.MaxRepoRoots; one that does
// not is an error that names the cap, never a silent cut.
func ExpandRepo(arg string, dirs []localindex.RepoDir, team bool) (Repo, error) {
	return expandRepo(arg, dirs, team, nil)
}

// ServerRepo is ExpandRepo for a request to the server, without the
// checkouts the path rules keep off it: uploads reports whether a
// session placed at a directory may reach the server
// (agent.Uploads). The server audits every query, so a checkout under a
// local or deny rule is never named in one. A root or main checkout is
// kept only when every main checkout and remote of the repository allows
// it; a rule on the main checkout therefore leaves only the argument
// itself (Repo), and no remote.
func ServerRepo(arg string, dirs []localindex.RepoDir, uploads func(pathpolicy.Placement) bool) (Repo, error) {
	return expandRepo(arg, dirs, true, uploads)
}

func expandRepo(arg string, dirs []localindex.RepoDir, team bool, uploads func(pathpolicy.Placement) bool) (Repo, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" || strings.ContainsAny(arg, "*?[") {
		return Repo{Repo: arg}, nil
	}
	x := &expander{canon: map[string]string{}, mainc: map[string]string{}, dirs: dirs, uploads: uploads}
	var out Repo
	if isPathArg(arg) {
		abs, err := filepath.Abs(arg)
		if err != nil {
			return Repo{Repo: arg}, nil
		}
		out = x.path(abs)
	} else {
		groups := x.named(arg)
		switch len(groups) {
		case 0:
			if team {
				return Repo{Repo: arg}, nil
			}
			return Repo{Repo: arg}, fmt.Errorf("%w: repo %q names no repository this device has a session in; pass its path, owner/name, or a glob", format.ErrBadRequest, arg)
		case 1:
			out = x.expand(groups[0], nil, "")
		default:
			labels := make([]string, 0, len(groups))
			for _, g := range groups {
				labels = append(labels, g.label())
			}
			slices.Sort(labels)
			return Repo{Repo: arg}, fmt.Errorf("%w: repo %q names %d repositories: %s; pass its path or owner/name", format.ErrBadRequest, arg, len(groups), strings.Join(labels, "; "))
		}
	}
	if team {
		for _, l := range [][]string{out.Roots, out.Mains, out.Remotes} {
			if len(l) > format.MaxRepoRoots {
				return Repo{Repo: arg}, fmt.Errorf("%w: repo %q has %d main checkouts, remotes or roots; a request carries at most %d of each; pass a narrower path", format.ErrBadRequest, arg, len(l), format.MaxRepoRoots)
			}
		}
	}
	return out, nil
}

// gone reports whether the directory p no longer exists.
func gone(p string) bool {
	_, err := os.Stat(p)
	return errors.Is(err, os.ErrNotExist)
}

// isPathArg reports whether a --repo argument is a path.
func isPathArg(s string) bool {
	return filepath.IsAbs(s) || s == "." || s == ".." || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../")
}

type expander struct {
	canon map[string]string
	mainc map[string]string // main checkout -> key (main)
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

// main is a main checkout as an identity compares it: symlinks resolved,
// and in the case the file system spells it, so /p/App and /p/app on a
// case-insensitive volume are one repository (diskCase).
func (x *expander) main(p string) string {
	if p == "" {
		return ""
	}
	if r, ok := x.mainc[p]; ok {
		return r
	}
	r := diskCase(x.real(p))
	x.mainc[p] = r
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
		id.mains[x.main(main)] = true
	}
	if remote != "" {
		id.remotes[remote] = true
	}
}

func (id *ident) has(x *expander, d localindex.RepoDir) bool {
	return d.Main != "" && id.mains[x.main(d.Main)] || d.Remote != "" && id.remotes[d.Remote]
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
func (x *expander) path(abs string) Repo {
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
		// by the placements recorded at or above it, when they agree. A
		// placement above it whose directory still exists is another
		// repository (a home directory kept in git, issue #102), not the
		// deleted one's.
		seeds = append(seeds, abs)
		id.add(x, abs, "")
		ra := x.real(abs)
		var found []*ident
		for _, d := range x.dirs {
			if rd := x.real(d.Dir); d.Dir != "" && (ra == rd || strings.HasPrefix(ra, rd+"/") && gone(d.Dir)) {
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
	out := x.expand(id, seeds, abs)
	out.Repo = repo
	return out
}

// expand closes repository id over the placements (the main checkouts
// and remotes they add: a clone of the same remote is the same
// repository) and lists what matches it. Roots are seeds (the
// argument's worktree root and main checkout) and the main checkouts,
// each also with its symlinks resolved and, when arg reached its
// directory through a symlink (/tmp for /private/tmp), as reached that
// way. Mains are the main checkouts as resolved and as every placement
// in id recorded them. The lists grow with the repository's main
// checkouts and remotes, not with its worktrees or sessions.
func (x *expander) expand(id *ident, seeds []string, arg string) Repo {
	var out Repo
	add := func(list *[]string, seen map[string]bool, p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			*list = append(*list, p)
		}
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
	seenM := map[string]bool{}
	for _, m := range slices.Sorted(mapKeys(id.mains)) {
		add(&out.Mains, seenM, m)
	}
	for _, s := range seeds {
		if id.mains[x.main(s)] {
			add(&out.Mains, seenM, s)
		}
	}
	for i, d := range x.dirs {
		if used[i] {
			add(&out.Mains, seenM, d.Main)
		}
	}
	seenR := map[string]bool{}
	for _, s := range seeds {
		if filepath.IsAbs(s) {
			add(&out.Roots, seenR, s)
		}
	}
	for _, m := range out.Mains {
		if filepath.IsAbs(m) {
			add(&out.Roots, seenR, m)
		}
	}
	lp, rp := symlinkPrefixes(arg, x.real(arg))
	for _, r := range slices.Clone(out.Roots) {
		add(&out.Roots, seenR, x.real(r))
		if rp != "" && (r == rp || strings.HasPrefix(r, rp+"/")) {
			add(&out.Roots, seenR, lp+r[len(rp):])
		}
	}
	out.Remotes = slices.Sorted(mapKeys(id.remotes))
	if x.uploads != nil {
		out.Roots = slices.DeleteFunc(out.Roots, func(r string) bool { return !x.allowed(id, r) })
		out.Mains = slices.DeleteFunc(out.Mains, func(m string) bool { return !x.allowed(id, m) })
		if len(out.Remotes) > 0 && !x.allowed(id, "") {
			out.Remotes = nil
		}
	}
	for _, l := range []*[]string{&out.Roots, &out.Mains, &out.Remotes} {
		if len(*l) == 0 {
			*l = nil
		}
	}
	return out
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
