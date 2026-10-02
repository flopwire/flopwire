package perfguard

import (
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/fsprobe"
)

func TestCountFSCountsClaimedPaths(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	for _, d := range []string{root, other} {
		if err := os.MkdirAll(filepath.Join(d, "a", "b"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "a", "f.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c := CountFS(t, root)
	got := c.Measure(func() {
		for _, d := range []string{root, other} {
			_, _ = fsprobe.Stat(filepath.Join(d, "a", "f.txt"))
			_, _ = fsprobe.ReadFile(filepath.Join(d, "a", "f.txt"))
			_ = fsprobe.WalkDir(d, func(string, fs.DirEntry, error) error { return nil })
		}
	})
	// Under root: one stat and the walk's stat of root; one open; the walk
	// lists root, a and a/b. Nothing under other counts.
	if want := (FSCost{Stats: 2, Lists: 3, Opens: 1}); got != want {
		t.Fatalf("cost %s, want %s", got, want)
	}
	if o := c.Opened(); !slices.Equal(o, []string{filepath.Join(root, "a", "f.txt")}) {
		t.Fatalf("opened %v", o)
	}
	if l := c.Listed(); !maps.Equal(l, map[string]int64{root: 1, filepath.Join(root, "a"): 1, filepath.Join(root, "a", "b"): 1}) {
		t.Fatalf("listed %v", l)
	}
	// A skipped directory is not listed.
	got = c.Measure(func() {
		_ = fsprobe.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if p != root && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		})
	})
	if want := (FSCost{Stats: 1, Lists: 1}); got != want {
		t.Fatalf("skipping walk cost %s, want %s", got, want)
	}
}

func TestAssertScalingGatesFSCost(t *testing.T) {
	r := &recorder{TB: t}
	AssertScaling(r, Linear, 100, 8, func(_ testing.TB, n int) Cost {
		return Cost{Statements: -1, FS: FSCost{Stats: int64(n * n)}} // a stat per file pair
	})
	if !strings.Contains(r.failed(), "fs stats") {
		t.Fatalf("quadratic stats not caught: %q", r.failed())
	}
	r = &recorder{TB: t}
	AssertScaling(r, Constant, 100, 8, func(_ testing.TB, n int) Cost {
		return Cost{Statements: -1, FS: FSCost{Lists: int64(n)}} // a listing per directory where none was new
	})
	if !strings.Contains(r.failed(), "fs lists") {
		t.Fatalf("linear listings not caught by Constant: %q", r.failed())
	}
	AssertScaling(t, Linear, 100, 8, func(_ testing.TB, n int) Cost {
		return Cost{Statements: -1, FS: FSCost{Stats: int64(3*n + 5), Lists: int64(n)}}
	})
}
