package devicesync

import (
	"context"
	"errors"
	"fmt"
	"io"
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
		fi, err := d.Info()
		if err != nil {
			return err
		}
		s.used += fi.Size()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s, nil
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

// Reserve checks that n more bytes fit at this moment, without writing or
// holding a reservation. Another writer may consume capacity before a later
// PutChunk/PutTail, which checks again. It sets Blocked when bytes do not fit.
func (s *Spool) Reserve(n int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkSpaceLocked(n)
}

func (s *Spool) checkSpaceLocked(n int64) error {
	if n < 0 || s.used > s.cap || n > s.cap-s.used {
		s.blocked = true
		return fmt.Errorf("%w: %d + %d bytes over cap %d", ErrSpoolFull, s.used, n, s.cap)
	}
	return nil
}

func (s *Spool) write(path string, data []byte) error {
	return s.writeWithFileWriter(path, data, (*os.File).Write)
}

func (s *Spool) writeWithFileWriter(path string, data []byte, write func(*os.File, []byte) (int, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fi, err := os.Stat(path); err == nil {
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("devicesync: spool destination is not a regular file: %s", path)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := s.checkSpaceLocked(int64(len(data))); err != nil {
		return err
	}
	retained, err := publishSpoolFile(path, data, write)
	if err != nil {
		// Failed cleanup can leave a partial temporary file. Charge it while
		// still owning the spool, so later writers cannot ignore those bytes.
		s.used += retained
		return err
	}
	s.used += int64(len(data))
	s.blocked = false
	return nil
}

// Publication has no mutable fault hooks. The write operation is passed
// explicitly so tests can prove partial-write/IO failure cleanup with real files.
func publishSpoolFile(path string, data []byte, write func(*os.File, []byte) (int, error)) (retained int64, err error) {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+"-*.tmp")
	if err != nil {
		return 0, err
	}
	tmp := f.Name()
	defer func() {
		_ = f.Close()
		if err != nil {
			if cleanupErr := removeSpoolTemp(tmp); cleanupErr != nil {
				fi, statErr := os.Stat(tmp)
				switch {
				case statErr == nil:
					retained = fi.Size()
				case errors.Is(statErr, os.ErrNotExist):
					retained = 0
				default:
					retained = int64(len(data))
				}
				err = errors.Join(err, cleanupErr, statErr)
			}
		}
	}()
	var n int
	n, err = write(f, data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	return 0, err
}

func removeSpoolTemp(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *Spool) read(path string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return b, err == nil, err
}

func (s *Spool) remove(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	if os.Remove(path) == nil {
		s.used -= fi.Size()
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

// sweep runs with no capture or upload in progress. Tail decisions use exact
// committed hashes; chunk reference/delete coordination remains serial.
func (s *Spool) sweep(ctx context.Context, keepChunk func(syncproto.Hash) (bool, error), required requiredTailFunc) error {
	for _, d := range []string{"chunks"} {
		ents, err := os.ReadDir(filepath.Join(s.dir, d))
		if err != nil {
			return err
		}
		for _, de := range ents {
			if err := ctx.Err(); err != nil {
				return err
			}
			path := filepath.Join(s.dir, d, de.Name())
			if de.IsDir() {
				continue
			}
			if strings.HasSuffix(de.Name(), ".tmp") {
				s.remove(path)
				continue
			}
			var h syncproto.Hash
			ok := h.UnmarshalText([]byte(de.Name())) == nil
			if ok {
				if ok, err = keepChunk(h); err != nil {
					return err
				}
			}
			if !ok {
				s.remove(path)
			}
		}
	}
	return s.sweepTails(ctx, required)
}
