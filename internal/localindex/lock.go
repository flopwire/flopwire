package localindex

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// One writer per index (decision D12). A writing Open takes an exclusive
// flock on <index>.lock and holds it until Close; the file records the
// holder's pid. Two writers on one index lose each other's FTS entries:
// each shard keeps one applied sequence, and a writer that commits later
// but applies first moves it past the other's work.

// LockedError is returned by a writing Open when another process holds
// the index lock.
type LockedError struct {
	Path string
	PID  int // 0 when the holder's pid is unknown
}

func (e *LockedError) Error() string {
	if e.PID > 0 {
		return fmt.Sprintf("localindex: %s is locked by pid %d", e.Path, e.PID)
	}
	return fmt.Sprintf("localindex: %s is locked by another process", e.Path)
}

// LockPath is the lock file of the index at path.
func LockPath(path string) string { return path + ".lock" }

// AcquireMaintenanceLock owns an existing index without opening or migrating
// it. Offline maintenance must hold the returned file through all state and
// spool operations. A running collector refuses this lock immediately.
func AcquireMaintenanceLock(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("localindex: maintenance requires an existing regular index")
	}
	return acquireLock(path, nil)
}

// errWouldBlock is what tryLock returns when the lock is held elsewhere.
var errWouldBlock = errors.New("lock held")

// acquireLock takes the index lock, adopting held when it is an already
// locked file (a re-executed agent keeps its lock across exec).
func acquireLock(path string, held *os.File) (*os.File, error) {
	lp := LockPath(path)
	f := held
	if f == nil {
		var err error
		if f, err = os.OpenFile(lp, os.O_RDWR|os.O_CREATE, 0o600); err != nil {
			return nil, err
		}
	}
	if err := tryLock(f); err != nil {
		f.Close()
		if errors.Is(err, errWouldBlock) {
			return nil, &LockedError{Path: path, PID: readLockPID(lp)}
		}
		return nil, fmt.Errorf("localindex: lock %s: %w", lp, err)
	}
	if err := f.Truncate(0); err == nil {
		f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return f, nil
}

func readLockPID(lp string) int {
	b, err := os.ReadFile(lp)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

// LockFile hands the held index lock to the caller, which keeps it past
// Close: an agent that re-executes itself passes it to the new process
// (Options.LockFile), so no other writer can take the index in between.
// Nil for a read-only store.
func (s *Store) LockFile() *os.File {
	s.keepLock = true
	return s.lock
}
