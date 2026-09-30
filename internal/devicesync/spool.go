package devicesync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/flopwire/flopwire/internal/syncproto"
)

// ErrSpoolFull means a spool write would pass the cap. The spool never
// evicts: the caller stops advancing (rewritten documents, exports) or
// records the bytes as lost (salvage), and Status reports it.
var ErrSpoolFull = errors.New("devicesync: spool full")

// Spool holds unacknowledged bytes the source may destroy (spec §6.5):
// chunks and tails of rewritten documents and SQLite exports, and chunks of
// an append-only file salvaged when a rewrite or deletion was about to
// remove them. Append-only files are otherwise never copied: their bytes
// are re-read from the source. Chunks are stored by hash, tails by source
// and generation. Entries are dropped once acknowledged.
type Spool struct {
	dir string
	cap int64

	mu      sync.Mutex
	used    int64
	blocked bool
}

// OpenSpool opens (creating) a spool directory with a size cap in bytes.
func OpenSpool(dir string, capBytes int64) (*Spool, error) {
	s := &Spool{dir: dir, cap: capBytes}
	for _, d := range []string{"chunks", "tails"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return nil, err
		}
	}
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if fi, err := d.Info(); err == nil {
			s.used += fi.Size()
		}
		return nil
	})
	return s, err
}

func (s *Spool) chunkPath(h syncproto.Hash) string {
	return filepath.Join(s.dir, "chunks", h.String())
}

func (s *Spool) tailPath(sid, gen int64) string {
	return filepath.Join(s.dir, "tails", fmt.Sprintf("%d-%d", sid, gen))
}

// Used returns the spooled bytes; Blocked reports whether the last write
// hit the cap.
func (s *Spool) Used() int64   { s.mu.Lock(); defer s.mu.Unlock(); return s.used }
func (s *Spool) Blocked() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.blocked }

// Reserve checks that n more bytes fit, without writing. It sets Blocked
// when they do not.
func (s *Spool) Reserve(n int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.used+n > s.cap {
		s.blocked = true
		return fmt.Errorf("%w: %d + %d bytes over cap %d", ErrSpoolFull, s.used, n, s.cap)
	}
	return nil
}

func (s *Spool) write(path string, data []byte) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := s.Reserve(int64(len(data))); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	s.mu.Lock()
	s.used += int64(len(data))
	s.blocked = false
	s.mu.Unlock()
	return nil
}

func (s *Spool) read(path string) ([]byte, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return b, err == nil, err
}

func (s *Spool) remove(path string) {
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	if os.Remove(path) == nil {
		s.mu.Lock()
		s.used -= fi.Size()
		s.mu.Unlock()
	}
}

func (s *Spool) PutChunk(h syncproto.Hash, data []byte) error { return s.write(s.chunkPath(h), data) }
func (s *Spool) Chunk(h syncproto.Hash) ([]byte, bool, error) { return s.read(s.chunkPath(h)) }
func (s *Spool) DropChunk(h syncproto.Hash)                   { s.remove(s.chunkPath(h)) }

func (s *Spool) PutTail(sid, gen int64, data []byte) error {
	// A tail is replaced on each capture; drop the old one first so the
	// cap counts only the live version.
	s.remove(s.tailPath(sid, gen))
	return s.write(s.tailPath(sid, gen), data)
}
func (s *Spool) Tail(sid, gen int64) ([]byte, bool, error) { return s.read(s.tailPath(sid, gen)) }
func (s *Spool) DropTail(sid, gen int64)                   { s.remove(s.tailPath(sid, gen)) }

// sweep deletes partial writes (*.tmp) and every chunk or tail that keep
// rejects: keep gets a chunk's hash, or nil and a tail's source and
// generation. Run it before any capture, so nothing is mid-write.
func (s *Spool) sweep(keep func(h *syncproto.Hash, sid, gen int64) (bool, error)) error {
	for _, d := range []string{"chunks", "tails"} {
		ents, err := os.ReadDir(filepath.Join(s.dir, d))
		if err != nil {
			return err
		}
		for _, de := range ents {
			path := filepath.Join(s.dir, d, de.Name())
			if de.IsDir() {
				continue
			}
			var (
				h        *syncproto.Hash
				sid, gen int64
				ok       bool
			)
			if strings.HasSuffix(de.Name(), ".tmp") {
				s.remove(path)
				continue
			}
			if d == "chunks" {
				var hh syncproto.Hash
				if hh.UnmarshalText([]byte(de.Name())) == nil {
					h, ok = &hh, true
				}
			} else if _, err := fmt.Sscanf(de.Name(), "%d-%d", &sid, &gen); err == nil {
				ok = true
			}
			if ok {
				if ok, err = keep(h, sid, gen); err != nil {
					return err
				}
			}
			if !ok {
				s.remove(path)
			}
		}
	}
	return nil
}
