package format

import (
	"errors"
	"strings"
	"testing"
)

func TestChangeQueryValidationAndCursorBinding(t *testing.T) {
	full := strings.Repeat("ab", 20)
	q, err := NormalizeChange(ChangeQuery{Commits: []string{strings.ToUpper(full), full}}, Filters{Repo: "/repo"})
	if err != nil || len(q.Commits) != 1 || q.Commits[0] != full || q.Limit != 20 {
		t.Fatalf("normalize %+v %v", q, err)
	}
	for _, bad := range []ChangeQuery{{}, {Commits: []string{"abcdef0"}}, {Commits: []string{strings.Repeat("z", 40)}}, {Commits: []string{full}, Limit: 101}} {
		if _, err := NormalizeChange(bad, Filters{Repo: "/repo"}); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("accepted %+v %v", bad, err)
		}
	}
	if _, err := NormalizeChange(q, Filters{}); !errors.Is(err, ErrBadRequest) {
		t.Fatal("missing repo accepted")
	}
	f := Filters{Repo: "/repo", Owner: "owner"}
	scope := ChangeScope(q, f)
	f.Owner = "other"
	if scope == ChangeScope(q, f) {
		t.Fatal("owner omitted from cursor scope")
	}
	secret := []byte("test-secret")
	c := ChangeCursor{Scope: scope, IndexID: "index", Revision: 2, Cutoff: 9, After: 4}
	token := SignChangeCursor(c, secret)
	got, err := VerifyChangeCursor(token, secret)
	if err != nil || got != c {
		t.Fatalf("roundtrip %+v %v", got, err)
	}
	if _, err := VerifyChangeCursor(token, []byte("different")); !errors.Is(err, ErrBadRequest) {
		t.Fatal("wrong secret accepted")
	}
	forged := c
	forged.Cutoff = 100
	if _, err := VerifyChangeCursor(EncodeChangeCursor(forged)+"."+strings.Split(token, ".")[1], secret); !errors.Is(err, ErrBadRequest) {
		t.Fatal("forged cutoff accepted")
	}
}
func TestBoundedChangeMatchKeepsEvidenceAddresses(t *testing.T) {
	m := ChangeMatch{Session: ConversationInfo{Title: strings.Repeat("x", 100000)}, CommandAddress: "123", ResultAddress: "124", InputCommits: []string{strings.Repeat("a", 40)}}
	bounded, size := BoundedChangeMatch(m)
	if bounded.CommandAddress != m.CommandAddress || bounded.ResultAddress != m.ResultAddress || size > ChangePageBudget || len(bounded.Session.Title) > 520 {
		t.Fatalf("bound failed size=%d", size)
	}
}
