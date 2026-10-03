package bus

import "testing"

// RepoMatches: a path covers its root and what is under it, a name the
// repository's name, the checkout roots a device expanded --repo to
// cover a linked worktree of the same repository, and the remotes it
// resolved to cover every checkout of it on any device (#102).
func TestRepoMatches(t *testing.T) {
	roots := []string{"/src/app", "/src/app-api"}
	web := []string{"github.com/acme/web"}
	for _, c := range []struct {
		filter        string
		roots, remote []string
		repo, rem     string
		want          bool
	}{
		{"", nil, nil, "/src/x", "", true},
		{"/src/app", nil, nil, "/src/app/sub", "", true},
		{"/src/app", nil, nil, "/src/app-api", "", false},
		{"/src/app", roots, nil, "/src/app-api", "", true},
		{"/src/app", roots, nil, "/src/app-apix", "", false},
		{"/src/app", roots, nil, "/other/app", "", false},
		{"app", nil, nil, "/other/app", "", true},
		{"app", roots, nil, "/src/app-api", "", true},
		{"", roots, nil, "/src/app-api/x", "", true},
		{"", roots, nil, "/elsewhere", "", false},
		// Another device's checkout of the same remote, at another path.
		{"", nil, web, "/home/bob/code/web-local", "github.com/acme/web", true},
		// Another repository of the same name, by remote: apart.
		{"", nil, web, "/home/bob/web", "github.com/other/web", false},
		{"", nil, web, "/home/bob/web", "", false},
		// A name is the remote's name when there is one.
		{"web", nil, nil, "/home/bob/web-local", "github.com/acme/web", true},
		{"web-local", nil, nil, "/home/bob/web-local", "github.com/acme/web", false},
		{"acme/web", nil, nil, "/x/y", "github.com/acme/web", true},
		{"acme/web", nil, nil, "/x/web", "", false},
	} {
		if got := RepoMatches(c.filter, c.roots, nil, c.remote, c.repo, "", c.rem); got != c.want {
			t.Errorf("RepoMatches(%q, %v, %v, %q, %q) = %v", c.filter, c.roots, c.remote, c.repo, c.rem, got)
		}
	}
	// A main checkout matches a session placed in it, whatever worktree
	// root it reports (#102 review).
	if !RepoMatches("", nil, []string{"/src/app"}, nil, "/src/app-wt-0999", "/src/app", "") ||
		RepoMatches("", nil, []string{"/src/app"}, nil, "/src/other", "/src/other", "") {
		t.Fatal("mains")
	}
}

// #102: an @user message routes by the sending session's remote when it
// has one, so a recipient session in another repository of the same
// name is not on it; a session in a checkout of the same remote at
// another path is.
func TestRouteRepo(t *testing.T) {
	for _, c := range []struct{ repo, fromRepo, fromRemote, want string }{
		{"", "/p/web", "github.com/acme/web", "github.com/acme/web"},
		{"", "/p/app", "", "app"},
		{"*", "/p/web", "github.com/acme/web", ""},
		{"/q/lib", "/p/web", "github.com/acme/web", "lib"},
		{"github.com/acme/lib", "/p/web", "", "github.com/acme/lib"},
		{"lib", "/p/web", "", "lib"},
	} {
		if got := RouteRepo(c.repo, c.fromRepo, c.fromRemote); got != c.want {
			t.Errorf("RouteRepo(%q, %q, %q) = %q, want %q", c.repo, c.fromRepo, c.fromRemote, got, c.want)
		}
	}
	to := RouteRepo("", "/p/web", "github.com/acme/web")
	same := liveSession{id: "a", repo: "/home/bob/web-local", remote: "github.com/acme/web"}
	other := liveSession{id: "b", repo: "/home/bob/web", remote: "github.com/other/web"}
	all := []liveSession{same, other}
	if !eligible(to, same, all) || eligible(to, other, all) {
		t.Fatalf("routing by remote: same %v, other %v", eligible(to, same, all), eligible(to, other, all))
	}
}
