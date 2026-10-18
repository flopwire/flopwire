package pathpolicy

import "testing"

func subtreePolicy(t *testing.T, admin, user []string) Policy {
	t.Helper()
	a, err := ParseRules(admin)
	if err != nil {
		t.Fatal(err)
	}
	u, err := ParseRules(user)
	if err != nil {
		t.Fatal(err)
	}
	return Policy{Admin: a, User: u, Unplaceable: Local}
}

func TestSubtreePathIntersection(t *testing.T) {
	cases := []struct {
		rule, root string
		want       bool
	}{
		{"/work/private", "/work", true},
		{"/work/private", "/", true},
		{"/", "/work", true},
		{"/work/private", "/work/public", false},
		{"/work/private", "/work/private/deep", true},
		{"/work/private", "/work/private-copy", false},
		{"/other/private", "/work/private", false},
		{"/other/private", "/work", false},
		{"private", "/work/public", true},
		{"/work/*/secret", "/work/client", true},
		{"/work/*/secret", "/work/client/public", false},
		{"/work/**/secret", "/work/client/public", true},
		{"/work/client?/secret", "/work/client1", true},
		{"/work/client?/secret", "/work/client12", false},
		{"/work/client[ab]/secret", "/work/clientb", true},
		{"/work/client[ab]/secret", "/work/clientc", false},
		{"/work/**/secret", "/unrelated/work", false},
		{"/work/**", "/work", true},
		{"/work/private/", "/WORK/", true},
		{"/work/secret[", "/work", false},
	}
	for _, tc := range cases {
		t.Run(tc.rule+"_"+tc.root, func(t *testing.T) {
			p := subtreePolicy(t, nil, []string{tc.rule})
			d := p.DecideSubtree(Placement{Cwd: tc.root})
			if (d.Decision.Mode == Deny) != tc.want || d.RepoScopeUnknown {
				t.Fatalf("decision: %+v", d)
			}
		})
	}
}

func TestSubtreeDoesNotBroadenWorktreeOrMain(t *testing.T) {
	p := subtreePolicy(t, nil, []string{"/repo/secret", "/main/secret"})
	d := p.DecideSubtree(Placement{Cwd: "/repo/public", Worktree: "/repo", Main: "/main"})
	if d.Decision.Mode != Allow {
		t.Fatalf("selected subtree broadened: %+v", d)
	}
	p = subtreePolicy(t, nil, []string{"local:/main"})
	if d = p.DecideSubtree(Placement{Cwd: "/repo/public", Worktree: "/repo", Main: "/main"}); d.Decision.Mode != Local {
		t.Fatalf("direct main match lost: %+v", d)
	}
}

func TestSubtreeAdminFloorAndTies(t *testing.T) {
	p := subtreePolicy(t, []string{"local:/work/private"}, []string{"allow:/work/**"})
	d := p.DecideSubtree(Placement{Cwd: "/work"})
	if d.Decision.Mode != Local || !d.Decision.Admin {
		t.Fatal(d)
	}
	p = subtreePolicy(t, []string{"/work/deep/private"}, []string{"/work"})
	d = p.DecideSubtree(Placement{Cwd: "/work"})
	if d.Decision.Mode != Deny || !d.Decision.Admin || d.Decision.Rule.Pattern != "/work/deep/private" {
		t.Fatal(d)
	}
	p = subtreePolicy(t, []string{"local:/work/private"}, []string{"/work/deep/secret"})
	d = p.DecideSubtree(Placement{Cwd: "/work"})
	if d.Decision.Mode != Deny || d.Decision.Admin {
		t.Fatal(d)
	}
}

func TestSubtreeRepoUnknownHoldsLocalAndKnownDenyWins(t *testing.T) {
	p := subtreePolicy(t, []string{"repo:github.com/private/*"}, nil)
	d := p.DecideSubtree(Placement{Cwd: "/work", Remote: "github.com/public/repo"})
	if d.Decision.Mode != Local || !d.RepoScopeUnknown || d.Decision.Rule.Pattern != "" || !d.Decision.Admin {
		t.Fatalf("invented unrelated repo deny: %+v", d)
	}
	d = p.DecideSubtree(Placement{Cwd: "/work", Remote: "github.com/private/repo"})
	if d.Decision.Mode != Deny || d.RepoScopeUnknown || !d.Decision.Rule.Repo {
		t.Fatal(d)
	}
	p = subtreePolicy(t, nil, []string{"repo:github.com/private/*", "/work/secret"})
	d = p.DecideSubtree(Placement{Cwd: "/work"})
	if d.Decision.Mode != Deny || !d.RepoScopeUnknown || d.Decision.Rule.Repo {
		t.Fatalf("known path deny weakened: %+v", d)
	}
	p = subtreePolicy(t, nil, []string{"allow:repo:github.com/public/*"})
	d = p.DecideSubtree(Placement{Cwd: "/work"})
	if d.Decision.Mode != Allow || d.RepoScopeUnknown {
		t.Fatal(d)
	}
}

func TestSubtreePhysicalAliasAndUnplaced(t *testing.T) {
	p := subtreePolicy(t, nil, []string{"/physical/secret"})
	if p.DecideSubtree(Placement{Cwd: "/logical"}).Decision.Mode != Allow || p.DecideSubtree(Placement{Cwd: "/physical"}).Decision.Mode != Deny {
		t.Fatal("caller aliases not independently evaluated")
	}
	d := p.DecideSubtree(Placement{})
	if !d.Decision.Unplaceable || d.Decision.Mode != Local || d.RepoScopeUnknown {
		t.Fatal(d)
	}
}
