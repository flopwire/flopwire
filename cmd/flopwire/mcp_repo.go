package main

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/retrieval/local"
)

// Relative MCP paths belong to the calling session, not the server process:
// Codex starts the plugin server in its installed plugin directory. Never
// chdir here: one server can serve several calling threads concurrently.
func mcpRepoArgs(ctx context.Context, r *retriever, name string, args map[string]any) (map[string]any, error) {
	switch name {
	case "flopwire_grep", "flopwire_search", "flopwire_sessions", "flopwire_messages", "flopwire_peers", "flopwire_send":
	default:
		return args, nil
	}
	repo, ok := args["repo"].(string)
	if _, present := args["repo"]; present && !ok {
		return args, nil // preserve normal argument validation
	}
	if !ok && name != "flopwire_messages" {
		return args, nil
	}
	if name == "flopwire_messages" && repo == "" {
		repo = "."
	}
	repo = strings.TrimSpace(repo)
	if repo != "." && repo != ".." && !strings.HasPrefix(repo, "./") && !strings.HasPrefix(repo, "../") {
		return args, nil
	}
	c, known := r.whoCalls(ctx)
	if !known || r.callerCwd == nil {
		return args, fmt.Errorf("repo %q: cannot identify the calling session's directory; pass an absolute path or repository name", repo)
	}
	cwd, err := r.callerCwd(ctx, c)
	if err != nil || !filepath.IsAbs(cwd) {
		if err == nil {
			err = fmt.Errorf("no absolute directory recorded")
		}
		return args, fmt.Errorf("repo %q: calling session's directory unavailable (%v); pass an absolute path or repository name", repo, err)
	}
	out := maps.Clone(args)
	if out == nil {
		out = make(map[string]any)
	}
	out["repo"] = filepath.Join(cwd, repo)
	return out, nil
}

func agentCallerCwd(socket string) func(context.Context, local.Caller) (string, error) {
	return func(ctx context.Context, c local.Caller) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		resp, err := agent.Call(ctx, socket, agent.Request{Op: "caller-context", Agent: string(c.Agent), Session: c.SessionID})
		return resp.Cwd, err
	}
}
