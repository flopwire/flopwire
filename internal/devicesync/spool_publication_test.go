package devicesync

import (
	"bytes"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/syncproto"
)

func publicationSpool(t *testing.T, cap int64) *Spool {
	t.Helper()
	spool, err := OpenSpool(t.TempDir(), cap)
	if err != nil {
		t.Fatal(err)
	}
	return spool
}

func publicationResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("spool operation did not finish")
		return nil
	}
}

func publicationGate(t *testing.T, s *Spool, path string, data []byte) (<-chan error, func()) {
	t.Helper()
	paused := make(chan string, 1)
	proceed := make(chan struct{})
	done := make(chan error, 1)
	var once sync.Once
	release := func() { once.Do(func() { close(proceed) }) }
	t.Cleanup(release)
	go func() {
		done <- s.writeWithFileWriter(path, data, func(f *os.File, p []byte) (int, error) {
			n, err := f.Write(p[:len(p)/2])
			if err != nil {
				return n, err
			}
			paused <- f.Name()
			<-proceed
			m, err := f.Write(p[n:])
			return n + m, err
		})
	}()
	var tmp string
	select {
	case tmp = <-paused:
	case <-time.After(5 * time.Second):
		t.Fatal("publication barrier missed")
	}
	if !strings.HasSuffix(tmp, ".tmp") || tmp == path+".tmp" {
		t.Fatalf("temporary name not unique: %s", filepath.Base(tmp))
	}
	partial, err := os.ReadFile(tmp)
	if err != nil || !bytes.Equal(partial, data[:len(data)/2]) {
		t.Fatalf("actual partial write missing: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial file published early: %v", err)
	}
	if s.mu.TryLock() {
		s.mu.Unlock()
		t.Fatal("publication did not retain spool ownership through IO")
	}
	return done, release
}

func TestSpoolConcurrentDuplicatePublicationCountsOnce(t *testing.T) {
	data := []byte("literal shared chunk published once\n")
	h := syncproto.Sum(data)
	s := publicationSpool(t, int64(len(data)))
	first, release := publicationGate(t, s, s.chunkPath(h), data)
	const writers = 12
	started := make(chan struct{}, writers)
	done := make(chan error, writers)
	for range writers {
		go func() { started <- struct{}{}; done <- s.PutChunk(h, data) }()
	}
	for range writers {
		<-started
	}
	release()
	if err := publicationResult(t, first); err != nil {
		t.Fatal(err)
	}
	for range writers {
		if err := publicationResult(t, done); err != nil {
			t.Fatal(err)
		}
	}
	got, ok, err := s.Chunk(h)
	if err != nil || !ok || !bytes.Equal(got, data) {
		t.Fatalf("shared literal bytes changed: %v", err)
	}
	if s.Used() != int64(len(data)) {
		t.Fatalf("duplicate accounting=%d", s.Used())
	}
	files, err := os.ReadDir(filepath.Join(s.dir, "chunks"))
	if err != nil || len(files) != 1 || files[0].Name() != h.String() {
		t.Fatalf("publication files=%d err=%v", len(files), err)
	}
	s.DropChunk(h)
	s.DropChunk(h)
	if s.Used() != 0 {
		t.Fatalf("duplicate removal accounting=%d", s.Used())
	}
}

func TestSpoolConcurrentDisjointPublicationRespectsCap(t *testing.T) {
	hBytes, jBytes := []byte("literal H publication\n"), []byte("literal J publication\n")
	h, j := syncproto.Sum(hBytes), syncproto.Sum(jBytes)
	s := publicationSpool(t, int64(len(hBytes)))
	first, release := publicationGate(t, s, s.chunkPath(h), hBytes)
	started := make(chan struct{})
	second := make(chan error, 1)
	go func() { close(started); second <- s.PutChunk(j, jBytes) }()
	<-started
	release()
	if err := publicationResult(t, first); err != nil {
		t.Fatal(err)
	}
	if err := publicationResult(t, second); !errors.Is(err, ErrSpoolFull) {
		t.Fatalf("second disjoint write=%v", err)
	}
	if s.Used() != int64(len(hBytes)) || !s.Blocked() {
		t.Fatalf("cap/accounting used=%d blocked=%v", s.Used(), s.Blocked())
	}
	if _, ok, err := s.Chunk(j); err != nil || ok {
		t.Fatalf("over-cap chunk published: %v", err)
	}
	if err := s.Reserve(math.MaxInt64); !errors.Is(err, ErrSpoolFull) {
		t.Fatalf("overflowing check=%v", err)
	}
	s.DropChunk(h)
	// Reserve is a check, not a held claim. Two checks both succeed while
	// no file has consumed capacity; PutChunk still checks atomically.
	if err := s.Reserve(int64(len(jBytes))); err != nil {
		t.Fatal(err)
	}
	if err := s.Reserve(int64(len(jBytes))); err != nil {
		t.Fatal(err)
	}
	if err := s.PutChunk(j, jBytes); err != nil {
		t.Fatal(err)
	}
}

func TestSpoolPublicationFailureCleansTemporaryAndReleasesOwner(t *testing.T) {
	for _, kind := range []string{"write", "short write", "sync", "rename"} {
		t.Run(kind, func(t *testing.T) {
			data := []byte("literal failure-retry chunk\n")
			h := syncproto.Sum(data)
			s := publicationSpool(t, int64(len(data)))
			failure := errors.New("synthetic write failure")
			var temporary string
			err := s.writeWithFileWriter(s.chunkPath(h), data, func(f *os.File, p []byte) (int, error) {
				temporary = f.Name()
				switch kind {
				case "write":
					n, err := f.Write(p[:3])
					if err != nil {
						return n, err
					}
					return n, failure
				case "short write":
					return f.Write(p[:3])
				case "sync":
					n, err := f.Write(p)
					if err != nil {
						return n, err
					}
					return n, f.Close()
				case "rename":
					n, err := f.Write(p)
					if err != nil {
						return n, err
					}
					return n, os.Mkdir(s.chunkPath(h), 0700)
				}
				panic("unknown failure case")
			})
			if err == nil {
				t.Fatal("failed publication accepted")
			}
			if kind == "write" && !errors.Is(err, failure) {
				t.Fatalf("write error lost: %v", err)
			}
			if kind == "short write" && !errors.Is(err, io.ErrShortWrite) {
				t.Fatalf("short write error lost: %v", err)
			}
			if !s.mu.TryLock() {
				t.Fatal("failure retained spool mutex")
			}
			s.mu.Unlock()
			if s.Used() != 0 {
				t.Fatalf("failed publication charged %d bytes", s.Used())
			}
			if _, statErr := os.Stat(temporary); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("failed temporary remains: %v", statErr)
			}
			if kind == "rename" {
				if err := os.Remove(s.chunkPath(h)); err != nil {
					t.Fatal(err)
				}
			}
			if _, statErr := os.Stat(s.chunkPath(h)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("failed final file exists: %v", statErr)
			}
			if err := s.PutChunk(h, data); err != nil {
				t.Fatalf("capacity unusable after failure: %v", err)
			}
			got, ok, err := s.Chunk(h)
			if err != nil || !ok || !bytes.Equal(got, data) || s.Used() != int64(len(data)) {
				t.Fatalf("retry failed: present=%v err=%v", ok, err)
			}
		})
	}
}

func TestSpoolRejectsNonRegularPublicationDestination(t *testing.T) {
	s := publicationSpool(t, 100)
	h := syncproto.Sum([]byte("literal"))
	if err := os.Mkdir(s.chunkPath(h), 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.PutChunk(h, []byte("literal")); err == nil {
		t.Fatal("directory treated as published bytes")
	}
	if s.Used() != 0 {
		t.Fatal("directory charged as publication")
	}
}
