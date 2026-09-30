//go:build unix

package localindex

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Git files that are not small regular files (a FIFO, say, which a
// crafted .git file can point at) are ignored, not read: reading a FIFO
// blocks the agent.
func TestResolveRepoSkipsNonRegularGitFiles(t *testing.T) {
	base := realTemp(t)
	dir, gitdir := filepath.Join(base, "w"), filepath.Join(base, "gd")
	os.MkdirAll(dir, 0o755)
	os.MkdirAll(gitdir, 0o755)
	os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o600)
	for _, name := range []string{"commondir", "config"} {
		if err := syscall.Mkfifo(filepath.Join(gitdir, name), 0o600); err != nil {
			t.Skip("no mkfifo:", err)
		}
	}
	done := make(chan Repo, 1)
	go func() { done <- ResolveRepo(dir) }()
	select {
	case r := <-done:
		if r.Worktree != dir || r.Remote != "" {
			t.Errorf("ResolveRepo = %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ResolveRepo blocked reading a FIFO")
	}
}
