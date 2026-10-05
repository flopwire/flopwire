//go:build darwin

package local

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// An lsof that is slow (a sandbox, a process with many descriptors) is cut
// off. What it printed before is a partial list: a lock file missing from
// it would read as closed, so a cut-off list is unknown (nil), as is an
// lsof that is missing or fails.
func TestOpenFilesCutOffIsUnknown(t *testing.T) {
	// The real lsof lists a file this process holds open.
	held, err := os.CreateTemp(t.TempDir(), "held-*.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	want, _ := filepath.EvalSymlinks(held.Name())
	var found bool
	for _, f := range OpenFiles(context.Background(), os.Getpid()) {
		found = found || f == want || f == held.Name()
	}
	if !found {
		t.Fatalf("lsof does not list %s", held.Name())
	}
	dir := t.TempDir()
	lsof := filepath.Join(dir, "lsof")
	if err := os.WriteFile(lsof, []byte("#!/bin/sh\nprintf 'p1\\nn/x/session_locks/a.lock\\n'\nexec /bin/sleep 10\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if got := OpenFiles(ctx, os.Getpid()); got != nil {
		t.Fatalf("a cut-off lsof gave %q, want unknown", got)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("OpenFiles took %s past its deadline", d)
	}
	t.Setenv("PATH", t.TempDir()) // no lsof at all
	if got := OpenFiles(context.Background(), os.Getpid()); got != nil {
		t.Fatalf("without lsof: %q", got)
	}
}
