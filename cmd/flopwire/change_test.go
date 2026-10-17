package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/local"
)

type changeCaptureBackend struct {
	backend
	q format.ChangeQuery
	f format.Filters
}

func (b *changeCaptureBackend) Change(_ context.Context, q format.ChangeQuery, f format.Filters) (*format.Changes, error) {
	b.q, b.f = q, f
	return &format.Changes{Kind: "changes", Matches: []format.ChangeMatch{}}, nil
}
func TestChangeMCPInputsAndSelf(t *testing.T) {
	b := &changeCaptureBackend{}
	r := &retriever{backend: b}
	hash := strings.Repeat("a", 40)
	out, err := mcpCall(t.Context(), r, "flopwire_change", map[string]any{"repo": "example/repo", "commits": []any{hash}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"kind":"changes"`) || b.q.Limit != 20 || len(b.q.Commits) != 1 {
		t.Fatalf("output %s query %#v", out, b.q)
	}
	if b.f.Repo != "example/repo" || b.f.ExcludeConversation != "" || b.f.ExcludeSession != "" || b.f.ExcludeSubagents {
		t.Fatalf("filters %#v", b.f)
	}
	for _, args := range []map[string]any{
		{"repo": "r", "commits": []any{"abc"}},
		{"repo": "r", "commits": []any{hash}, "limit": 101.0},
		{"commits": []any{hash}},
		{"repo": "r", "commits": []any{hash}, "bogus": true},
		{"repo": "r", "commits": hash},
		{"repo": "r", "commits": []any{7}},
	} {
		if _, err := mcpCall(t.Context(), r, "flopwire_change", args); err == nil {
			t.Fatalf("accepted %#v", args)
		}
	}
}

func TestChangeMCPExpandedRepositoryScope(t *testing.T) {
	b := &changeCaptureBackend{}
	expanded := local.Repo{Roots: []string{"/src/repo"}, Mains: []string{"/src/repo"}, Remotes: []string{"github.com/example/repo"}}
	r := &retriever{backend: b, teamRepo: func(_ context.Context, repo string) (local.Repo, error) {
		if repo != "example/repo" {
			t.Fatalf("repo %q", repo)
		}
		return expanded, nil
	}}
	args := map[string]any{"repo": "example/repo", "commits": []any{strings.Repeat("a", 40)}}
	if _, err := mcpCall(t.Context(), r, "flopwire_change", args); err != nil {
		t.Fatal(err)
	}
	want := format.Filters{RepoRoots: expanded.Roots, RepoMains: expanded.Mains, RepoRemotes: expanded.Remotes}
	if !reflect.DeepEqual(b.f, want) {
		t.Fatalf("scope changed: got %#v want %#v", b.f, want)
	}
	sentinel := errors.New("ambiguous repository")
	r.teamRepo = func(context.Context, string) (local.Repo, error) { return local.Repo{}, sentinel }
	b.q = format.ChangeQuery{}
	if _, err := mcpCall(t.Context(), r, "flopwire_change", args); !errors.Is(err, sentinel) {
		t.Fatalf("resolver error lost: %v", err)
	}
	if len(b.q.Commits) != 0 {
		t.Fatal("called backend after resolution failed")
	}
	r.teamRepo = func(context.Context, string) (local.Repo, error) { return local.Repo{}, nil }
	if _, err := mcpCall(t.Context(), r, "flopwire_change", args); err == nil {
		t.Fatal("accepted empty resolved scope")
	}
}

// Exercise public command dispatch, not just the shared handler: old scripts and
// new instructions must reach the same lookup without requiring a server.
func TestTraceCLIAndChangeAlias(t *testing.T) {
	oracleIndex(t)
	hash := strings.Repeat("a", 40)
	var baseline string
	for _, verb := range []string{"trace", "change"} {
		t.Run(verb, func(t *testing.T) {
			output := captureStdout(t, func() error {
				return run(t.Context(), []string{verb, "--local", "--repo", "/tmp/oracle-alpha", hash})
			})
			var page format.Changes
			if err := json.Unmarshal([]byte(output), &page); err != nil || page.Kind != "changes" {
				t.Fatalf("invalid lookup response: %s (%v)", output, err)
			}
			if baseline == "" {
				baseline = output
			} else if output != baseline {
				t.Fatalf("alias changed response: got %s want %s", output, baseline)
			}
			if err := run(t.Context(), []string{verb, "--local", "--repo", "/tmp/oracle-alpha", "abc"}); err == nil {
				t.Fatal("accepted abbreviated input commit")
			}
		})
	}
}
