//go:build darwin || linux

package cowork

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/flopwire/flopwire/internal/fsprobe"
	"golang.org/x/sys/unix"
)

// OpenFile opens a regular file through directory descriptors. Every component
// at or below the configured root rejects symlinks at open time. An opened
// descriptor stays attached to its original file if a path is later replaced.
// OS directory aliases above the configured root remain trusted.
func OpenFile(root, path string) (*os.File, error) {
	fail := func(err error) (*os.File, error) { return nil, &fs.PathError{Op: "open", Path: path, Err: err} }
	if root == "" || !filepath.IsAbs(root) || !filepath.IsAbs(path) {
		return fail(fs.ErrNotExist)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fail(fs.ErrNotExist)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	fsprobe.Note(fsprobe.OpOpen, path)
	dirFD, err := unix.Open(filepath.Clean(root), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = unix.Close(dirFD) }()
	for _, part := range parts[:len(parts)-1] {
		next, err := unix.Openat(dirFD, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return fail(err)
		}
		_ = unix.Close(dirFD)
		dirFD = next
	}
	// Nonblocking prevents a substituted FIFO from hanging before fstat rejects
	// it. The flag has no effect on ordinary transcript files.
	fd, err := unix.Openat(dirFD, parts[len(parts)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOCTTY, 0)
	if err != nil {
		return fail(err)
	}
	f := os.NewFile(uintptr(fd), path)
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fail(err)
	}
	if !st.Mode().IsRegular() {
		_ = f.Close()
		return fail(fs.ErrNotExist)
	}
	return f, nil
}
