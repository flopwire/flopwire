package main

import (
	"context"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"strings"
	"testing"
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
