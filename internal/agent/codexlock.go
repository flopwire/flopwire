package agent

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/flopwire/flopwire/internal/fsprobe"
)

// Codex writer lock states (codexWriter).
const (
	codexUnknown  = iota // could not tell this time
	codexHeld            // a running Codex holds the thread's writer lock
	codexReleased        // the lock file is there and nobody holds it
)

// codexWriter reports whether a Codex process holds the writer lock
// thread-writer-locks/<thread>.lock. Codex (codex-rs/rollout writer_lock.rs)
// holds an exclusive flock on that file while a process writes the
// thread, and the kernel releases it when the process exits, also on
// SIGKILL; the file itself is left behind by an exit that skips cleanup
// (codex exec), so its existence says nothing.
//
// Every Codex writer takes the directory's .coordination.lock before it
// tries the thread lock, and so does its cleanup. The probe does the
// same: it takes the coordination lock without waiting (busy: unknown
// this time), then tries a shared lock on the thread file without waiting
// (refused: held), and lets both go at once. A Codex that starts meanwhile
// waits on the coordination lock for those microseconds; it is never
// refused its thread. Without a coordination file (a Codex from before
// it) the state is unknown. Nothing is written.
func codexWriter(lock string) int {
	coord, err := fsprobe.Open(filepath.Join(filepath.Dir(lock), ".coordination.lock"))
	if err != nil {
		return codexUnknown
	}
	defer coord.Close()
	if unix.Flock(int(coord.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		return codexUnknown
	}
	defer unix.Flock(int(coord.Fd()), unix.LOCK_UN)
	f, err := fsprobe.Open(lock)
	if errors.Is(err, os.ErrNotExist) {
		return codexReleased // removed by its writer's cleanup meanwhile
	}
	if err != nil {
		return codexUnknown
	}
	defer f.Close()
	switch err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB); {
	case errors.Is(err, unix.EWOULDBLOCK):
		return codexHeld
	case err != nil:
		return codexUnknown
	}
	unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return codexReleased
}
