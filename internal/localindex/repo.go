package localindex

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/flopwire/flopwire/internal/provenance"
)

// FindRepoRoot returns the nearest ancestor of cwd (inclusive) holding a
// .git entry (directory, or file for worktrees and submodules), or "" when
// there is none or cwd is empty. It only stats; it never runs git.
func FindRepoRoot(cwd string) string {
	if cwd == "" || !filepath.IsAbs(cwd) {
		return ""
	}
	dir := filepath.Clean(cwd)
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// RepoRootCache memoizes FindRepoRoot. A conversation's repo_root is
// resolved once, when the conversation is first seen (spec §4.2).
type RepoRootCache struct {
	mu sync.Mutex
	m  map[string]string
}

func NewRepoRootCache() *RepoRootCache { return &RepoRootCache{m: map[string]string{}} }

func (c *RepoRootCache) Resolve(cwd string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.m[cwd]; ok {
		return r
	}
	r := FindRepoRoot(cwd)
	c.m[cwd] = r
	return r
}

// Repo is where a directory sits in git, read from the files git keeps
// (.git, gitdir, commondir, config) without running git.
type Repo struct {
	Worktree string // the working tree root (git rev-parse --show-toplevel)
	Main     string // the main checkout root; for a bare repository with worktrees, the common git directory
	Remote   string // the origin remote (else the first remote), normalized to host/owner/name
}

// ResolveRepo finds the repository holding cwd: the nearest ancestor with
// a .git entry is the worktree root. A .git file ("gitdir: ...", a linked
// worktree or a submodule) leads to the worktree's git directory, whose
// commondir file leads to the repository's common directory. The main
// checkout is the common directory's parent when it is named .git, else
// the common directory itself (a bare repository). The remote comes from
// the common directory's config. A cwd that no longer exists resolves
// through whatever ancestor still does.
func ResolveRepo(cwd string) Repo {
	root := FindRepoRoot(cwd)
	if root == "" {
		return Repo{}
	}
	r := Repo{Worktree: root, Main: root}
	dotgit := filepath.Join(root, ".git")
	fi, err := os.Stat(dotgit)
	if err != nil {
		return r
	}
	gitdir := dotgit
	if !fi.IsDir() {
		b, err := readGitFile(dotgit)
		if err != nil {
			return r
		}
		line, _, _ := strings.Cut(string(b), "\n")
		p, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
		if !ok {
			return r
		}
		gitdir = absFrom(root, strings.TrimSpace(p))
	}
	common := gitdir
	if b, err := readGitFile(filepath.Join(gitdir, "commondir")); err == nil {
		common = absFrom(gitdir, strings.TrimSpace(string(b)))
	}
	if filepath.Base(common) == ".git" {
		r.Main = filepath.Dir(common)
	} else if common != gitdir || !fi.IsDir() {
		r.Main = common
	}
	if raw := remoteURL(filepath.Join(common, "config")); raw != "" {
		if n, err := provenance.NormalizeRemote(raw); err == nil {
			r.Remote = n
		}
	}
	return r
}

// RemoteOf returns the normalized remote (origin, else the first) of the
// main checkout or bare repository at main, "" when it has none.
func RemoteOf(main string) string {
	for _, cfg := range []string{filepath.Join(main, ".git", "config"), filepath.Join(main, "config")} {
		if raw := remoteURL(cfg); raw != "" {
			if n, err := provenance.NormalizeRemote(raw); err == nil {
				return n
			}
			return ""
		}
	}
	return ""
}

// maxGitFile bounds the git files ResolveRepo reads.
const maxGitFile = 1 << 20

// readGitFile reads a git file that is a regular file of at most
// maxGitFile bytes. A .git file may point anywhere: a FIFO would block
// the reader and a device would never end.
func readGitFile(p string) ([]byte, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxGitFile {
		return nil, fmt.Errorf("localindex: %s is not a small regular file", p)
	}
	return os.ReadFile(p)
}

func absFrom(base, p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	return filepath.Clean(p)
}

// remoteURL reads the url of [remote "origin"] from a git config file,
// else of the first remote section.
func remoteURL(config string) string {
	b, err := readGitFile(config)
	if err != nil {
		return ""
	}
	var first, origin, section string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			section = ""
			h := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			if name, ok := strings.CutPrefix(h, "remote "); ok || strings.HasPrefix(strings.ToLower(h), "remote ") {
				if !ok {
					name = h[len("remote "):]
				}
				section = strings.Trim(strings.TrimSpace(name), `"`)
			}
			continue
		}
		if section == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "url") {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		if section == "origin" && origin == "" {
			origin = v
		}
		if first == "" {
			first = v
		}
	}
	if origin != "" {
		return origin
	}
	return first
}

// maxWorktrees bounds the linked worktrees Worktrees lists.
const maxWorktrees = 4096

// Worktrees lists the live linked worktrees of the repository whose main
// checkout (or bare repository) is main, from the gitdir files git keeps
// under <common>/worktrees/<name>/gitdir. It reads files only; a missing
// or unreadable entry is left out. Paths are as git wrote them (git
// writes them with symlinks resolved).
func Worktrees(main string) []string {
	if main == "" {
		return nil
	}
	common := filepath.Join(main, ".git")
	if fi, err := os.Stat(common); err != nil || !fi.IsDir() {
		if !IsBare(main) {
			return nil
		}
		common = main
	}
	ents, err := os.ReadDir(filepath.Join(common, "worktrees"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if len(out) == maxWorktrees {
			break
		}
		b, err := readGitFile(filepath.Join(common, "worktrees", e.Name(), "gitdir"))
		if err != nil {
			continue
		}
		p := strings.TrimSpace(string(b))
		if !filepath.IsAbs(p) || filepath.Base(p) != ".git" {
			continue
		}
		out = append(out, filepath.Dir(filepath.Clean(p)))
	}
	return out
}

// IsBare reports whether dir looks like a bare git repository: a HEAD
// file and an objects directory, and no .git entry of its own.
func IsBare(dir string) bool {
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
		return false
	}
	if fi, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil || !fi.Mode().IsRegular() {
		return false
	}
	fi, err := os.Stat(filepath.Join(dir, "objects"))
	return err == nil && fi.IsDir()
}

// RepoName is a repository's short name: the last element of its remote
// (host/owner/name) when it has one, else of its main checkout. A bare
// repository's directory drops ".git" and a leading dot, so
// ~/Code/.app.git and ~/Code/app.git are both "app".
func RepoName(main, remote string) string {
	if remote != "" {
		return remote[strings.LastIndexByte(remote, '/')+1:]
	}
	if main == "" {
		return ""
	}
	n := filepath.Base(filepath.Clean(main))
	if s, ok := strings.CutSuffix(n, ".git"); ok && s != "" {
		n = strings.TrimPrefix(s, ".")
		if n == "" {
			n = s
		}
	}
	return n
}
