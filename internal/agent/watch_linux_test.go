//go:build linux

package agent

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func newTestWatcher(t *testing.T) *watcher {
	t.Helper()
	w := newWatcher(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if w.fd < 0 {
		t.Skip("inotify unavailable")
	}
	return w
}

func nextEvent(t *testing.T, w *watcher, want func(watchEvent) bool) watchEvent {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-w.events:
			if want(ev) {
				return ev
			}
		case <-deadline:
			t.Fatal("no matching event")
		}
	}
}

// A write to a file in a watched directory arrives flagged modify; its
// creation does not.
func TestInotifyFlagsWrites(t *testing.T) {
	w := newTestWatcher(t)
	defer w.close()
	dir := t.TempDir()
	if got := w.set([]string{dir}); len(got) != 1 {
		t.Fatalf("added %v", got)
	}
	p := filepath.Join(dir, "a.jsonl")
	fh, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	if ev := nextEvent(t, w, func(e watchEvent) bool { return e.path == p }); ev.modify {
		t.Errorf("create flagged modify: %+v", ev)
	}
	if _, err := fh.WriteString("x\n"); err != nil {
		t.Fatal(err)
	}
	nextEvent(t, w, func(e watchEvent) bool { return e.path == p && e.modify })
}

// A watched directory that is deleted drops out of the watch set, so a
// directory recreated at the same path is watched again.
func TestInotifyForgetsRemovedDirs(t *testing.T) {
	w := newTestWatcher(t)
	defer w.close()
	dir := filepath.Join(t.TempDir(), "proj")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	w.set([]string{dir})
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.dirs) == 0 && len(w.wds) == 0
	})
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := w.set([]string{dir}); len(got) != 1 {
		t.Fatalf("recreated directory not watched again: added %v", got)
	}
	p := filepath.Join(dir, "b.jsonl")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	nextEvent(t, w, func(e watchEvent) bool { return e.path == p })
}

// A descriptor that fails ends the loop instead of spinning on it.
func TestInotifyLoopStopsOnDescriptorError(t *testing.T) {
	w := newTestWatcher(t)
	unix.Close(w.fd)
	select {
	case <-w.done:
	case <-time.After(3 * time.Second):
		t.Fatal("watcher loop still running on a closed descriptor")
	}
	w.fd = -1 // closed above
}
