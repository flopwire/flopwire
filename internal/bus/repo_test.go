package bus

import "testing"

// RepoMatches: a path covers its root and what is under it, a name the
// last element, and the checkout roots a device expanded --repo to cover
// a linked worktree of the same repository.
func TestRepoMatches(t *testing.T) {
	roots := []string{"/src/app", "/src/app-api"}
	for _, c := range []struct {
		filter string
		roots  []string
		repo   string
		want   bool
	}{
		{"", nil, "/src/x", true},
		{"/src/app", nil, "/src/app/sub", true},
		{"/src/app", nil, "/src/app-api", false},
		{"/src/app", roots, "/src/app-api", true},
		{"/src/app", roots, "/src/app-apix", false},
		{"/src/app", roots, "/other/app", false},
		{"app", nil, "/other/app", true},
		{"app", roots, "/src/app-api", true},
		{"", roots, "/src/app-api/x", true},
		{"", roots, "/elsewhere", false},
	} {
		if got := RepoMatches(c.filter, c.roots, c.repo); got != c.want {
			t.Errorf("RepoMatches(%q, %v, %q) = %v", c.filter, c.roots, c.repo, got)
		}
	}
}
