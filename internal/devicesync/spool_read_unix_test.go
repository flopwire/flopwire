//go:build darwin || linux

package devicesync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/syncproto"
)

// A FIFO makes real os.ReadFile wait for EOF. It is an IO barrier only,
// not a spool format: normal published chunks remain regular files.
func TestSpoolReadOwnsFileUntilActualReadCompletes(t *testing.T) {
	s := publicationSpool(t, 1024)
	data := []byte("literal bytes read before deletion\n")
	h := syncproto.Sum(data)
	other := []byte("literal accounted regular chunk\n")
	j := syncproto.Sum(other)
	if err := syscall.Mkfifo(s.chunkPath(h), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.PutChunk(j, other); err != nil {
		t.Fatal(err)
	}
	type result struct {
		body    []byte
		present bool
		err     error
	}
	readDone := make(chan result, 1)
	readExited := make(chan struct{})
	go func() {
		defer close(readExited)
		b, ok, err := s.Chunk(h)
		readDone <- result{b, ok, err}
	}()
	// O_WRONLY returns only once the reader actually opens the FIFO. Keep
	// that writer open so os.ReadFile cannot finish, even after data arrives.
	opened := make(chan *os.File, 1)
	openError := make(chan error, 1)
	writerDone := make(chan struct{})
	abort := make(chan struct{})
	var writer *os.File
	var drops sync.WaitGroup
	t.Cleanup(func() {
		if err := abortSpoolFIFO(s.chunkPath(h), abort, writer, readExited, writerDone, opened, &drops, nil); err != nil {
			t.Error(err)
		}
	})
	go func() {
		defer close(writerDone)
		f, err := os.OpenFile(s.chunkPath(h), os.O_WRONLY, 0)
		if err != nil {
			openError <- err
			return
		}
		select {
		case <-abort:
			_ = f.Close()
		default:
			opened <- f
		}
	}()
	select {
	case writer = <-opened:
	case err := <-openError:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("actual ReadFile FIFO barrier not reached")
	}
	defer writer.Close()
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if s.mu.TryLock() {
		s.mu.Unlock()
		t.Fatal("actual read did not hold spool mutex")
	}
	started := make(chan struct{}, 2)
	dropDone := make(chan struct{}, 2)
	for _, hash := range []syncproto.Hash{h, j} {
		drops.Add(1)
		go func() { defer drops.Done(); started <- struct{}{}; s.DropChunk(hash); dropDone <- struct{}{} }()
	}
	for range 2 {
		<-started
	}
	select {
	case <-dropDone:
		t.Fatal("remove completed while ReadFile was still live")
	default:
	}
	if _, err := os.Stat(s.chunkPath(h)); err != nil {
		t.Fatalf("live read file removed: %v", err)
	}
	if _, err := os.Stat(s.chunkPath(j)); err != nil {
		t.Fatalf("accounted file removed through active IO owner: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-readDone:
		if got.err != nil || !got.present || !bytes.Equal(got.body, data) {
			t.Fatalf("literal read changed: %v", got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read failed to finish after EOF")
	}
	finished := make(chan struct{})
	go func() { drops.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("removals retained after read completed")
	}
	if s.Used() != 0 {
		t.Fatalf("read/remove accounting=%d", s.Used())
	}
	if _, ok, err := s.Chunk(j); err != nil || ok {
		t.Fatalf("regular chunk removal failed: %v", err)
	}
}

func TestSpoolFailedTemporaryUnlinkRemainsAccountedUntilSweep(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires non-root Unix directory permission enforcement")
	}
	data := []byte("literal requested publication bytes\n")
	partial := data[:7]
	s := publicationSpool(t, int64(len(data)))
	parent := filepath.Dir(s.chunkPath(syncproto.Sum(data)))
	t.Cleanup(func() { _ = os.Chmod(parent, 0700) })
	wantErr := errors.New("injected failure after partial file write")
	var tmp string
	err := s.writeWithFileWriter(s.chunkPath(syncproto.Sum(data)), data, func(f *os.File, _ []byte) (int, error) {
		tmp = f.Name()
		n, err := f.Write(partial)
		if err != nil {
			return n, err
		}
		if err := os.Chmod(parent, 0555); err != nil {
			return n, err
		}
		return n, wantErr
	})
	if !errors.Is(err, wantErr) || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("expected write and cleanup errors, got %v", err)
	}
	if s.Used() != int64(len(partial)) || s.Blocked() {
		t.Fatalf("retained bytes=%d blocked=%v", s.Used(), s.Blocked())
	}
	if got, err := os.ReadFile(tmp); err != nil || !bytes.Equal(got, partial) {
		t.Fatalf("actual retained partial bytes changed: %q %v", got, err)
	}
	if !s.mu.TryLock() {
		t.Fatal("failed cleanup retained spool owner")
	}
	s.mu.Unlock()
	if err := s.PutChunk(syncproto.Sum(data), data); !errors.Is(err, ErrSpoolFull) {
		t.Fatalf("orphan capacity was ignored: %v", err)
	}
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSpool(s.dir, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Used() != int64(len(partial)) {
		t.Fatalf("restart lost retained-byte accounting: %d", reopened.Used())
	}
	if err := reopened.sweep(context.Background(), func(context.Context, []syncproto.Hash) (map[syncproto.Hash]bool, error) { return nil, nil }, func(int64, int64) (syncproto.Tail, bool, error) { return syncproto.Tail{}, false, nil }); err != nil {
		t.Fatal(err)
	}
	if reopened.Used() != 0 {
		t.Fatalf("startup sweep did not reclaim orphan: %d", reopened.Used())
	}
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan survived sweep: %v", err)
	}
	if err := reopened.PutChunk(syncproto.Sum(data), data); err != nil {
		t.Fatalf("reclaimed capacity unusable: %v", err)
	}
}

// This test-only cleanup joins actual FIFO IO, including readers scheduled
// after the first rescue has already released the writer.
func abortSpoolFIFO(path string, abort chan struct{}, writer *os.File, readExited, writerDone <-chan struct{}, opened <-chan *os.File, drops *sync.WaitGroup, afterFirstRescueClose func()) error {
	close(abort)
	if writer != nil {
		_ = writer.Close()
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	readerFinished, writerFinished := false, false
	for !readerFinished || !writerFinished {
		select {
		case <-readExited:
			readerFinished = true
		default:
		}
		select {
		case <-writerDone:
			writerFinished = true
		default:
		}
		select {
		case f := <-opened:
			_ = f.Close()
		default:
		}
		if readerFinished && writerFinished {
			break
		}
		rescue, _ := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
		select {
		case <-time.After(10 * time.Millisecond):
		case <-deadline.C:
			if rescue != nil {
				_ = rescue.Close()
			}
			return fmt.Errorf("FIFO reader or writer did not exit during cleanup")
		}
		if rescue != nil {
			_ = rescue.Close()
		}
		if afterFirstRescueClose != nil {
			afterFirstRescueClose()
			afterFirstRescueClose = nil
		}
		// Leave a no-writer interval for a blocked ReadFile to observe EOF.
		// A reader still waiting in open gets a peer on the next iteration.
		select {
		case <-time.After(10 * time.Millisecond):
		case <-deadline.C:
			return fmt.Errorf("FIFO reader or writer did not exit during cleanup")
		}
	}
	removed := make(chan struct{})
	go func() { drops.Wait(); close(removed) }()
	select {
	case <-removed:
		return nil
	case <-deadline.C:
		return fmt.Errorf("FIFO removals did not exit during cleanup")
	}
}

func TestSpoolFIFOAbortJoinsReaderStartingAfterFirstRescue(t *testing.T) {
	s := publicationSpool(t, 1024)
	h := syncproto.Sum([]byte("delayed FIFO reader cleanup fixture"))
	path := s.chunkPath(h)
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	abort := make(chan struct{})
	opened := make(chan *os.File, 1)
	writerDone := make(chan struct{})
	writerError := make(chan error, 1)
	go func() {
		defer close(writerDone)
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			writerError <- err
			return
		}
		select {
		case <-abort:
			_ = f.Close()
		default:
			opened <- f
		}
	}()
	startReader := make(chan struct{})
	var startOnce sync.Once
	start := func() { startOnce.Do(func() { close(startReader) }) }
	t.Cleanup(start)
	readExited := make(chan struct{})
	readResult := make(chan error, 1)
	go func() {
		defer close(readExited)
		<-startReader
		b, ok, err := s.Chunk(h)
		if err == nil && (!ok || len(b) != 0) {
			err = fmt.Errorf("rescue changed empty FIFO read: present=%v bytes=%d", ok, len(b))
		}
		readResult <- err
	}()
	firstRescueClosed := make(chan struct{})
	var drops sync.WaitGroup
	cleanupDone := make(chan error, 1)
	cleanupExited := make(chan struct{})
	go func() {
		defer close(cleanupExited)
		cleanupDone <- abortSpoolFIFO(path, abort, nil, readExited, writerDone, opened, &drops, func() { close(firstRescueClosed) })
	}()
	t.Cleanup(func() {
		start()
		select {
		case <-cleanupExited:
		case <-time.After(6 * time.Second):
			t.Error("abort regression cleanup did not exit")
		}
	})
	// Channel barriers force the missed-barrier order: the first rescue
	// releases the writer and closes while the actual reader cannot start.
	select {
	case <-firstRescueClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("first rescue close missed")
	}
	select {
	case <-writerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("rescue did not release writer")
	}
	select {
	case err := <-writerError:
		t.Fatal(err)
	default:
	}
	select {
	case <-readExited:
		t.Fatal("reader ran before release")
	default:
	}
	start()
	if err := publicationResult(t, cleanupDone); err != nil {
		t.Fatal(err)
	}
	select {
	case <-readExited:
	default:
		t.Fatal("cleanup did not join reader")
	}
	if err := publicationResult(t, readResult); err != nil {
		t.Fatal(err)
	}
}
