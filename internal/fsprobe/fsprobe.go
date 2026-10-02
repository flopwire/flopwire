// Package fsprobe is the file system seam of the device agent's change
// detection: the agent and the transcript discovery code stat, list and
// open files through it, so a test can count those calls per operation
// (perfguard.CountFS) and prove that a sweep over unchanged files stats and
// lists in proportion to the files and opens none. With no hook installed
// a call costs one atomic load on top of the os call.
package fsprobe

import (
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
)

// Op is a kind of file system call.
type Op uint8

const (
	OpStat Op = iota // stat or lstat of a path (DirEntry.Info included)
	OpList           // a directory listing
	OpOpen           // a file opened or read
)

func (o Op) String() string {
	switch o {
	case OpStat:
		return "stat"
	case OpList:
		return "list"
	case OpOpen:
		return "open"
	}
	return "op?"
}

var hook atomic.Pointer[func(Op, string)]

// SetHook installs f to see every call (nil removes it). Tests only.
func SetHook(f func(Op, string)) {
	if f == nil {
		hook.Store(nil)
		return
	}
	hook.Store(&f)
}

// Note reports a call made outside the wrappers below.
func Note(op Op, path string) {
	if h := hook.Load(); h != nil {
		(*h)(op, path)
	}
}

func Stat(path string) (fs.FileInfo, error) {
	Note(OpStat, path)
	return os.Stat(path)
}

func Lstat(path string) (fs.FileInfo, error) {
	Note(OpStat, path)
	return os.Lstat(path)
}

func ReadDir(path string) ([]os.DirEntry, error) {
	Note(OpList, path)
	return os.ReadDir(path)
}

func Open(path string) (*os.File, error) {
	Note(OpOpen, path)
	return os.Open(path)
}

func ReadFile(path string) ([]byte, error) {
	Note(OpOpen, path)
	return os.ReadFile(path)
}

// EvalSymlinks is filepath.EvalSymlinks, noted as one stat (it lstats each
// element of the path).
func EvalSymlinks(path string) (string, error) {
	Note(OpStat, path)
	return filepath.EvalSymlinks(path)
}

// Glob is filepath.Glob, noted as one listing of the pattern's directory.
func Glob(pattern string) ([]string, error) {
	Note(OpList, filepath.Dir(pattern))
	return filepath.Glob(pattern)
}

// WalkDir is filepath.WalkDir, noting the stat of root and the listing of
// every directory the walk reads (each one fn does not skip).
func WalkDir(root string, fn fs.WalkDirFunc) error {
	Note(OpStat, root)
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		r := fn(path, d, err)
		if r == nil && err == nil && d != nil && d.IsDir() {
			Note(OpList, path)
		}
		return r
	})
}
