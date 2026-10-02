package claude

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/flopwire/flopwire/internal/fsprobe"
)

// FS is how the parser reads the files beside a transcript: the
// agent-<id>.meta.json sidecar, tool-results/ outputs, and the parent
// transcript for the spawned-by fallback. Paths are the transcript's own
// namespace (the device path). A device reads its disk (the default); the
// server reads the same paths out of its chunk archive.
type FS interface {
	// Open opens a regular file. A missing file is an error wrapping
	// fs.ErrNotExist.
	Open(path string) (File, error)
	// Glob is filepath.Glob over the same namespace.
	Glob(pattern string) ([]string, error)
}

// File is an open file of an FS.
type File interface {
	io.ReaderAt
	io.Closer
	Size() int64
}

// OSFS is the local file system.
type OSFS struct{}

func (OSFS) Open(path string) (File, error) {
	f, err := fsprobe.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	return osFile{f, st.Size()}, nil
}

func (OSFS) Glob(pattern string) ([]string, error) { return filepath.Glob(pattern) }

type osFile struct {
	*os.File
	size int64
}

func (f osFile) Size() int64 { return f.size }

func (p *Parser) fs() FS {
	if p.FS != nil {
		return p.FS
	}
	return OSFS{}
}

// readFile reads at most limit bytes of path.
func readFile(fsys FS, path string, limit int64) ([]byte, error) {
	f, err := fsys.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	n := min(f.Size(), limit)
	b := make([]byte, n)
	if _, err := f.ReadAt(b, 0); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return b, nil
}

func fileExists(fsys FS, p string) bool {
	f, err := fsys.Open(p)
	if err != nil {
		return false
	}
	f.Close()
	return true
}
