package pathpolicy

import (
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/provenance"
)

func TestParseRule(t *testing.T) {
	tests := []struct {
		raw  string
		want Rule
	}{
		{"/Users/Gary/Personal", Rule{Mode: Deny, Pattern: "/users/gary/personal"}},
		{"deny ~/personal", Rule{Mode: Deny, Pattern: "~/personal"}},
		{"LOCAL:~/clients/acme/**", Rule{Mode: Local, Pattern: "~/clients/acme/**"}},
		{"allow\t/work", Rule{Mode: Allow, Pattern: "/work"}},
		{`local C:\Work\Acme\`, Rule{Mode: Local, Pattern: "c:/work/acme/"}},
		{"secret-client", Rule{Mode: Deny, Pattern: "secret-client"}},
		{"repo:GitHub.com/Acme/*", Rule{Mode: Deny, Pattern: "github.com/acme/*", Repo: true}},
		{"local repo:https://git@github.com/acme/web.git", Rule{Mode: Local, Pattern: "github.com/acme/web", Repo: true}},
		{"local:repo:git@github.com:acme/web.git", Rule{Mode: Local, Pattern: "github.com/acme/web", Repo: true}},
	}
	for _, tt := range tests {
		got, err := ParseRule(tt.raw)
		if err != nil || got != tt.want {
			t.Errorf("ParseRule(%q) = %+v, %v; want %+v", tt.raw, got, err, tt.want)
		}
	}
	for _, bad := range []string{"", "  ", "deny", "local: ", "repo:", "deny repo:/"} {
		if _, err := ParseRule(bad); err == nil {
			t.Errorf("ParseRule(%q) accepted", bad)
		}
	}
}

func TestMatch(t *testing.T) {
	tests := []struct {
		pattern, path string
		want          bool
	}{
		{"/users/gary/personal", "/Users/gary/Personal", true},
		{"/users/gary/personal", "/users/gary/personal/notes/x", true},
		{"/users/gary/personal/", "/users/gary/personal", true},
		{"/users/gary/personal", "/users/gary/personality", false},
		{"/users/gary/personal", "/users/gary", false},
		{"/users/*/personal", "/users/ann/personal/a", true},
		{"/users/*/personal", "/users/ann/b/personal", false},
		{"/users/**/personal", "/users/ann/b/personal", true},
		{"/code/acme-*", "/code/acme-web/src", true},
		{"/code/acme-*", "/code/acme", false},
		{"secret-client", "/home/x/secret-client/app", true},
		{"secret-client", "/home/x/secret-clients", false},
		{"**", "/anything", true},
		{`C:\Work`, `c:\work\app`, true},
		{"/users/gary/work/../personal", "/users/gary/personal", true},
		{"/x", "", false},
	}
	for _, tt := range tests {
		if got := Match(tt.pattern, tt.path); got != tt.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
		}
	}
}

func TestExpandHome(t *testing.T) {
	r, _ := ParseRule("deny ~/Personal")
	if got := r.ExpandHome("/Users/Gary"); got.Pattern != "/users/gary/personal" {
		t.Fatalf("expanded %q", got.Pattern)
	}
	r, _ = ParseRule("deny ~other/x")
	if got := r.ExpandHome("/Users/Gary"); got.Pattern != "~other/x" {
		t.Fatalf("expanded %q", got.Pattern)
	}
}

// The most restrictive rule wins, so an admin rule is a floor: a user
// rule can tighten it but not loosen it.
func TestDecideMostRestrictiveAndAdminFloor(t *testing.T) {
	rules := func(lines ...string) []Rule {
		rs, err := ParseRules(lines)
		if err != nil {
			t.Fatal(err)
		}
		return rs
	}
	p := Policy{
		Admin: rules("local /work/acme"),
		User:  rules("allow /work/**", "deny /work/acme/secret", "local /home/me/**"),
	}
	cases := []struct {
		paths []string
		want  Mode
		admin bool
	}{
		{[]string{"/work/acme/web"}, Local, true},           // user allow cannot loosen the admin floor
		{[]string{"/work/acme/secret/x"}, Deny, false},      // user tightens
		{[]string{"/work/other"}, Allow, false},             // allow is the default anyway
		{[]string{"/home/me/p"}, Local, false},              // user-only rule
		{[]string{"/tmp/x", "/work/acme"}, Local, true},     // the repo root matches
		{[]string{"", "/elsewhere"}, Allow, false},          // an empty path is skipped
		{[]string{"/home/me/p", "/work/acme"}, Local, true}, // tie: the admin rule is reported
	}
	for _, c := range cases {
		pl := Placement{Cwd: c.paths[0]}
		if len(c.paths) > 1 {
			pl.Main = c.paths[1]
		}
		d := p.Decide(pl)
		if d.Mode != c.want || d.Mode != Allow && d.Admin != c.admin {
			t.Errorf("Decide(%q) = %+v, want %v admin=%v", c.paths, d, c.want, c.admin)
		}
	}
	if !(Policy{}).Empty() || p.Empty() || (Policy{Unplaceable: Local}).Empty() {
		t.Error("Empty")
	}
}

func TestNormalizeRulesDeduplicatesEquivalentRules(t *testing.T) {
	got := NormalizeRules([]string{` /Personal/ `, `\personal\`, "", ".", "deny:/personal/", "LOCAL /Acme", "local:/acme"})
	if strings.Join(got, ",") != "/personal/,local:/acme" {
		t.Fatalf("rules=%#v", got)
	}
}

func TestParseRulesSkipsCommentsAndReportsBadLines(t *testing.T) {
	rs, err := ParseRules([]string{"# comment", "", "deny /a", "local:"})
	if len(rs) != 1 || rs[0] != (Rule{Mode: Deny, Pattern: "/a"}) {
		t.Fatalf("rules %+v", rs)
	}
	if err == nil || !strings.Contains(err.Error(), "local:") {
		t.Fatalf("err %v", err)
	}
}

// A repo rule matches the normalized origin remote, whether the remote is
// https or ssh, in any case; path rules match the worktree and main
// checkout roots as well as the working directory.
func TestDecideRepoRulesAndPlacement(t *testing.T) {
	rules, err := ParseRules([]string{"deny repo:github.com/acme/*", "local /code/main", "local repo:gitlab.com/me/notes"})
	if err != nil {
		t.Fatal(err)
	}
	p := Policy{User: rules}
	cases := []struct {
		pl   Placement
		want Mode
	}{
		{Placement{Cwd: "/x", Remote: "github.com/Acme/Web"}, Deny},
		{Placement{Cwd: "/x", Remote: "github.com/acme"}, Allow}, // above the pattern
		{Placement{Cwd: "/x", Remote: "github.com/other/web"}, Allow},
		{Placement{Cwd: "/x", Remote: "gitlab.com/me/notes"}, Local},
		{Placement{Cwd: "/wt/fix/src", Worktree: "/wt/fix", Main: "/code/main"}, Local},
		{Placement{Cwd: "/code/main"}, Local},
		{Placement{Remote: "github.com/acme/web"}, Deny}, // a remote alone places a session
	}
	for _, c := range cases {
		if got := p.Mode(c.pl); got != c.want {
			t.Errorf("Mode(%+v) = %v, want %v", c.pl, got, c.want)
		}
	}
	for _, remote := range []string{"https://github.com/acme/web.git", "git@github.com:Acme/web.git", "ssh://git@github.com/acme/web"} {
		n, err := provenance.NormalizeRemote(remote)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.Mode(Placement{Remote: n}); got != Deny {
			t.Errorf("remote %s (%s): %v", remote, n, got)
		}
	}
	if MatchRepo("github.com/acme/*", "/code/acme") || rules[0].Matches("/github.com/acme/x") {
		t.Error("a repo rule matched a path")
	}
}

// A session with nothing known about its place gets the unplaceable
// setting: the user's (default local), with the admin's as a floor.
func TestUnplaceable(t *testing.T) {
	for _, s := range []string{"upload", "LOCAL", " exclude "} {
		if _, ok := ParseUnplaceable(s); !ok {
			t.Errorf("ParseUnplaceable(%q) failed", s)
		}
	}
	if _, ok := ParseUnplaceable("deny"); ok {
		t.Error("deny is not an unplaceable setting")
	}
	d := Policy{Unplaceable: Deny, UnplaceableAdmin: true}.Decide(Placement{})
	if d.Mode != Deny || !d.Unplaceable || !d.Admin || !strings.Contains(d.Reason(), "exclude") {
		t.Fatalf("decision %+v %s", d, d.Reason())
	}
	if d := (Policy{Unplaceable: Local}).Decide(Placement{Cwd: "/x"}); d.Mode != Allow || d.Unplaceable {
		t.Fatalf("placed session: %+v", d)
	}
}
