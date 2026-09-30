package provenance

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestNormalizeRemoteEquivalent(t *testing.T) {
	for _, raw := range []string{"git@GitHub.com:Acme/widgets.git", "https://github.com/Acme/widgets.git", "ssh://git@github.com/Acme/widgets.git"} {
		got, err := NormalizeRemote(raw)
		if err != nil || got != "github.com/Acme/widgets" {
			t.Fatalf("NormalizeRemote(%q) = %q, %v", raw, got, err)
		}
	}
}

func TestResolveGitRemotePrecedenceAndOverride(t *testing.T) {
	responses := map[string]string{"rev-parse --show-toplevel": "/repo\n", "rev-parse HEAD": "abc123\n", "remote": "zeta\norigin\nupstream\n", "remote get-url upstream": "git@github.com:acme/team.git\n"}
	run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		v, ok := responses[join(args)]
		if !ok {
			return nil, errors.New("unexpected")
		}
		return []byte(v), nil
	}
	got, err := ResolveGit(context.Background(), "/repo/a.jsonl", "", run)
	if err != nil || !reflect.DeepEqual(got, GitIdentity{Project: "github.com/acme/team", Commit: "abc123", Remote: "upstream"}) {
		t.Fatalf("got %#v %v", got, err)
	}
	got, err = ResolveGit(context.Background(), "/repo/a.jsonl", "local/project", run)
	if err != nil || got.Project != "local/project" {
		t.Fatalf("override got %#v %v", got, err)
	}
}

func join(v []string) string {
	var out string
	for i, s := range v {
		if i > 0 {
			out += " "
		}
		out += s
	}
	return out
}
