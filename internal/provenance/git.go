// Package provenance resolves a transcript's repository identity from local
// Git state: the normalized remote (upstream, then origin, then the first
// remote), the HEAD commit, and an explicit override. The device agent (B2)
// uses it to fill conversations.repo_root.
//
// Ported from the CASS-era ingestion hardening (#7, hardening/ingestion).
package provenance

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type GitIdentity struct {
	Project string
	Commit  string
	Remote  string
}

type Runner func(context.Context, string, ...string) ([]byte, error)

func ResolveGit(ctx context.Context, sourcePath, override string, run Runner) (GitIdentity, error) {
	if run == nil {
		run = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, "git", args...)
			cmd.Dir = dir
			return cmd.Output()
		}
	}
	dir := filepath.Dir(sourcePath)
	rootRaw, err := run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		if override != "" {
			return GitIdentity{Project: override}, nil
		}
		return GitIdentity{}, nil // no remote/repository is explicitly unassigned
	}
	root := strings.TrimSpace(string(rootRaw))
	commitRaw, _ := run(ctx, root, "rev-parse", "HEAD")
	remotesRaw, err := run(ctx, root, "remote")
	if err != nil {
		return GitIdentity{}, fmt.Errorf("list git remotes: %w", err)
	}
	remotes := strings.Fields(string(remotesRaw))
	sort.Strings(remotes)
	chosen := ""
	for _, preferred := range []string{"upstream", "origin"} {
		for _, remote := range remotes {
			if remote == preferred {
				chosen = remote
				break
			}
		}
		if chosen != "" {
			break
		}
	}
	if chosen == "" && len(remotes) > 0 {
		chosen = remotes[0]
	}
	identity := GitIdentity{Project: override, Commit: strings.TrimSpace(string(commitRaw)), Remote: chosen}
	if override != "" || chosen == "" {
		return identity, nil
	}
	remoteRaw, err := run(ctx, root, "remote", "get-url", chosen)
	if err != nil {
		return GitIdentity{}, fmt.Errorf("resolve git remote %s: %w", chosen, err)
	}
	identity.Project, err = NormalizeRemote(strings.TrimSpace(string(remoteRaw)))
	if err != nil {
		return GitIdentity{}, err
	}
	return identity, nil
}

func NormalizeRemote(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	var host, path string
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			return "", fmt.Errorf("invalid git remote %q", raw)
		}
		host, path = strings.ToLower(u.Hostname()), u.Path
	} else {
		at := strings.LastIndex(raw, "@")
		colon := strings.Index(raw[max(0, at+1):], ":")
		if colon < 0 {
			return "", errors.New("git remote must be an SSH or HTTP(S) URL")
		}
		colon += max(0, at+1)
		host, path = strings.ToLower(raw[max(0, at+1):colon]), raw[colon+1:]
	}
	path = strings.Trim(strings.TrimSuffix(path, ".git"), "/")
	if host == "" || path == "" || strings.Contains(path, "..") {
		return "", fmt.Errorf("invalid git remote %q", raw)
	}
	return host + "/" + path, nil
}
