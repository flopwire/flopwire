package perfguard

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/flopwire/flopwire/internal/fsprobe"
)

// FSCost is the file system calls an operation made through fsprobe on
// the paths an FSCounter claims.
type FSCost struct {
	Stats, Lists, Opens int64
}

func (c FSCost) String() string {
	return fmt.Sprintf("stats=%d lists=%d opens=%d", c.Stats, c.Lists, c.Opens)
}

// FSCounter counts fsprobe calls on paths under its root, per operation
// and per path. It is safe for concurrent use.
type FSCounter struct {
	root string

	mu     sync.Mutex
	cost   FSCost
	opened map[string]int64
	listed map[string]int64
}

var fsCounters struct {
	sync.Mutex
	m []*FSCounter
}

// CountFS returns a counter of every fsprobe call on root or a path below
// it (normally the test's t.TempDir()) from now on. It is dropped when the
// test ends.
func CountFS(t testing.TB, root string) *FSCounter {
	t.Helper()
	if root == "" {
		t.Fatal("perfguard: CountFS needs a nonempty root")
	}
	c := &FSCounter{root: strings.TrimSuffix(root, "/")}
	fsCounters.Lock()
	fsCounters.m = append(fsCounters.m, c)
	if len(fsCounters.m) == 1 {
		fsprobe.SetHook(fsNote)
	}
	fsCounters.Unlock()
	t.Cleanup(func() {
		fsCounters.Lock()
		defer fsCounters.Unlock()
		fsCounters.m = slices.DeleteFunc(fsCounters.m, func(x *FSCounter) bool { return x == c })
		if len(fsCounters.m) == 0 {
			fsprobe.SetHook(nil)
		}
	})
	return c
}

func fsNote(op fsprobe.Op, path string) {
	fsCounters.Lock()
	var best *FSCounter
	for _, c := range fsCounters.m {
		if (path == c.root || strings.HasPrefix(path, c.root+"/")) && (best == nil || len(c.root) > len(best.root)) {
			best = c
		}
	}
	fsCounters.Unlock()
	if best != nil {
		best.note(op, path)
	}
}

func (c *FSCounter) note(op fsprobe.Op, path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch op {
	case fsprobe.OpStat:
		c.cost.Stats++
	case fsprobe.OpList:
		c.cost.Lists++
		if c.listed == nil {
			c.listed = map[string]int64{}
		}
		c.listed[path]++
	case fsprobe.OpOpen:
		c.cost.Opens++
		if c.opened == nil {
			c.opened = map[string]int64{}
		}
		c.opened[path]++
	}
}

// Measure runs fn and returns the calls it caused. Calls made by other
// goroutines on the counted paths while fn runs count too.
func (c *FSCounter) Measure(fn func()) FSCost {
	c.Reset()
	fn()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cost
}

// Reset sets every count to zero.
func (c *FSCounter) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cost, c.opened, c.listed = FSCost{}, nil, nil
}

// Opened returns the paths opened since the last Reset, sorted.
func (c *FSCounter) Opened() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Sorted(maps.Keys(c.opened))
}

// Listed returns the directories listed since the last Reset, sorted.
func (c *FSCounter) Listed() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Sorted(maps.Keys(c.listed))
}
