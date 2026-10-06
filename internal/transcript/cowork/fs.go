package cowork

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/flopwire/flopwire/internal/transcript/claude"
)

// FS confines native Claude parser side reads to evidence beneath a configured
// Cowork container. Root-level app metadata and audit streams are excluded even
// when a transcript references them as persisted tool output.
type FS struct{ Root string }

func (f FS) Open(path string) (claude.File, error) {
	if !f.evidencePath(path) || !SafeFile(f.Root, path) {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	file, err := OpenFile(f.Root, path)
	if err != nil {
		return nil, err
	}
	st, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return nativeFile{File: file, size: st.Size()}, nil
}

type nativeFile struct {
	*os.File
	size int64
}

func (f nativeFile) Size() int64 { return f.size }

func (f FS) Glob(pattern string) ([]string, error) {
	// Expand one path component at a time. Filter directories before the next
	// expansion so glob never enumerates through a symlink ancestor.
	if !f.evidencePath(pattern) {
		return nil, nil
	}
	if _, err := filepath.Match(pattern, ""); err != nil {
		return nil, err
	}
	if !safe(f.Root, f.Root, true) {
		return nil, nil
	}
	rel, _ := filepath.Rel(f.Root, pattern)
	parts := strings.Split(rel, string(filepath.Separator))
	parents := []string{f.Root}
	for i, part := range parts {
		var next []string
		for _, parent := range parents {
			matches, err := filepath.Glob(filepath.Join(parent, part))
			if err != nil {
				return nil, err
			}
			for _, p := range matches {
				if safe(f.Root, p, i < len(parts)-1) {
					next = append(next, p)
				}
			}
		}
		parents = next
	}
	return parents, nil
}

func (f FS) evidencePath(path string) bool {
	if f.Root == "" || !filepath.IsAbs(f.Root) || !filepath.IsAbs(path) {
		return false
	}
	rel, err := filepath.Rel(f.Root, path)
	if err != nil {
		return false
	}
	p := strings.Split(filepath.ToSlash(rel), "/")
	return len(p) >= 7 && p[0] != ".." && p[3] == ".claude" && p[4] == "projects"
}

var _ claude.FS = FS{}
