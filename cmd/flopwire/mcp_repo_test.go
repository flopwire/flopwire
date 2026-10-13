package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/local"
)

type repoCaptureBackend struct {
	blockingBackend
	queries chan string
}

func (b *repoCaptureBackend) Sessions(_ context.Context, _, _ string, f format.Filters) (*format.Sessions, error) {
	b.queries <- f.Repo
	return &format.Sessions{}, nil
}

func TestMCPRepoUsesCallingThreadRatherThanPluginCwd(t *testing.T) {
	plugin := t.TempDir()
	t.Chdir(plugin)
	cwds := map[string]string{"thread-a": filepath.Join(t.TempDir(), "repo-a", "sub"), "thread-b": filepath.Join(t.TempDir(), "repo-b"), "child-a": filepath.Join(t.TempDir(), "child-worktree")}
	b := &repoCaptureBackend{queries: make(chan string, 3)}
	r := &retriever{backend: b, callerCwd: func(_ context.Context, c local.Caller) (string, error) { return cwds[c.SessionID], nil }}
	args := map[string]any{"repo": "."}
	var wg sync.WaitGroup
	for id := range cwds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := withMCPMeta(t.Context(), true, map[string]any{"threadId": id})
			if _, err := mcpCall(ctx, r, "flopwire_sessions", args); err != nil {
				t.Errorf("thread %s: %v", id, err)
			}
		}()
	}
	wg.Wait()
	close(b.queries)
	seen := map[string]bool{}
	for repo := range b.queries {
		seen[repo] = true
	}
	for _, cwd := range cwds {
		if !seen[cwd] {
			t.Errorf("caller directory %s not queried; got %v", cwd, seen)
		}
	}
	if args["repo"] != "." || seen[plugin] {
		t.Fatalf("mutated caller arguments or queried plugin: %v, %v", args, seen)
	}
	// CLI resolution still uses the shell/process cwd.
	if got := local.ResolveRepo("."); got != plugin {
		t.Fatalf("CLI repo = %s, want %s", got, plugin)
	}
}

func TestMCPRepoToolDefaultsAndRelativePaths(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "repo", "sub")
	r := &retriever{callerCwd: func(_ context.Context, _ local.Caller) (string, error) { return cwd, nil }}
	ctx := withMCPMeta(t.Context(), true, map[string]any{"threadId": "thread"})
	for _, tool := range []string{"flopwire_grep", "flopwire_search", "flopwire_sessions", "flopwire_messages", "flopwire_peers", "flopwire_send"} {
		for _, repo := range []string{".", "..", "./child", "../sibling"} {
			got, err := mcpRepoArgs(ctx, r, tool, map[string]any{"repo": repo})
			if err != nil || got["repo"] != filepath.Join(cwd, repo) {
				t.Fatalf("%s %s: %v, %v", tool, repo, got, err)
			}
		}
		got, err := mcpRepoArgs(ctx, r, tool, nil)
		if err != nil || tool == "flopwire_messages" && got["repo"] != cwd || tool != "flopwire_messages" && len(got) != 0 {
			t.Fatalf("%s default: %v, %v", tool, got, err)
		}
	}
}

func TestMCPRepoUnavailableNeverUsesProcessCwd(t *testing.T) {
	ctx := withMCPMeta(t.Context(), true, map[string]any{"threadId": "thread"})
	for _, r := range []*retriever{{}, {callerCwd: func(context.Context, local.Caller) (string, error) { return "", errors.New("not recorded") }}, {callerCwd: func(context.Context, local.Caller) (string, error) { return "relative", nil }}} {
		for _, tool := range []string{"flopwire_sessions", "flopwire_messages"} {
			_, err := mcpCall(ctx, r, tool, map[string]any{"repo": "."})
			if err == nil || !strings.Contains(err.Error(), "absolute path or repository name") {
				t.Fatalf("%s: %v", tool, err)
			}
		}
		for _, repo := range []string{"/absolute/repo", "owner/repo", "repo*"} {
			got, err := mcpRepoArgs(ctx, r, "flopwire_sessions", map[string]any{"repo": repo})
			if err != nil || got["repo"] != repo {
				t.Fatalf("explicit selector %s: %v, %v", repo, got, err)
			}
		}
	}
}
