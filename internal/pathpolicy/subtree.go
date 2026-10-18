package pathpolicy

import (
	"path"
	"strings"
)

// SubtreeDecision distinguishes a matching rule from a local-only hold because
// a selected folder may contain repositories whose remotes are not inventoried.
type SubtreeDecision struct {
	Decision         Decision
	RepoScopeUnknown bool
}

// DecideSubtree applies every path rule whose covered directories intersect
// Cwd or any possible descendant. Worktree and Main remain direct placement
// checks: selecting a subdirectory does not grant access to its siblings.
// Callers supply each logical/physical alias as its own Cwd placement.
//
// Restrictive repo rules that do not match the known remote may cover unseen
// nested repositories. This pure helper cannot inspect them, so they hold
// sharing at Local. An actual matching Deny still prevents local indexing.
func (p Policy) DecideSubtree(pl Placement) SubtreeDecision {
	d := p.Decide(pl)
	unknown := false
	unknownAdmin := false
	for _, set := range []struct {
		rules []Rule
		admin bool
	}{{p.Admin, true}, {p.User, false}} {
		for _, r := range set.rules {
			if r.Repo {
				if pl.Cwd != "" && r.Mode > Allow && !r.MatchesRemote(pl.Remote) {
					unknown = true
					unknownAdmin = unknownAdmin || set.admin
				}
				continue
			}
			if subtreeIntersects(r.Pattern, pl.Cwd) && (r.Mode > d.Mode || r.Mode == d.Mode && set.admin && !d.Admin) {
				d = Decision{Mode: r.Mode, Rule: r, Admin: set.admin}
			}
		}
	}
	if unknown && d.Mode < Local {
		d = Decision{Mode: Local, Admin: unknownAdmin}
	}
	return SubtreeDecision{Decision: d, RepoScopeUnknown: unknown}
}

// subtreeIntersects matches the fixed selected-root prefix against the rule's
// segment automaton. After the prefix, a valid remaining glob can be fulfilled
// by some descendant; no real directories are read or guessed.
func subtreeIntersects(pattern, root string) bool {
	pattern = Normalize(pattern)
	root = Normalize(root)
	if pattern != "/" {
		pattern = strings.TrimSuffix(pattern, "/")
	}
	if root != "/" {
		root = strings.TrimSuffix(root, "/")
	}
	if pattern == "" || root == "" {
		return false
	}
	if !strings.HasPrefix(pattern, "/") {
		pattern = "**/" + pattern
	}
	pat := append(strings.Split(pattern, "/"), "**")
	segs := strings.Split(root, "/")
	if pattern == "/" {
		pat = []string{"", "**"}
	}
	if root == "/" {
		segs = []string{""}
	}
	type state struct{ pattern, path int }
	seen := map[state]bool{}
	var visit func(int, int) bool
	visit = func(i, j int) bool {
		s := state{i, j}
		if seen[s] {
			return false
		}
		seen[s] = true
		if j == len(segs) {
			for _, seg := range pat[i:] {
				if seg == "**" {
					continue
				}
				if _, err := path.Match(seg, ""); err != nil {
					return false
				}
			}
			return true
		}
		if i == len(pat) {
			return false
		}
		if pat[i] == "**" {
			return visit(i+1, j) || visit(i, j+1)
		}
		matched, err := path.Match(pat[i], segs[j])
		return err == nil && matched && visit(i+1, j+1)
	}
	return visit(0, 0)
}
