//go:build darwin || linux

package devicesync

import (
	"os"
	"syscall"
	"testing"

	"github.com/flopwire/flopwire/internal/syncproto"
)

func TestSpoolReferencesFIFORejectedBeforeOpen(t *testing.T) {
	f := newTailPreparationFixture(t)
	body := []byte("literal charged bytes before FIFO substitution\n")
	hash := syncproto.Sum(body)
	if err := f.spool.withReferences(f.store, func(scope *spoolReferenceScope) error { return scope.putChunk(hash, body) }); err != nil {
		t.Fatal(err)
	}
	path := f.spool.chunkPath(hash)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.spool.withReferences(f.store, func(scope *spoolReferenceScope) error {
		return scope.releaseChunks(t.Context(), []syncproto.Hash{hash})
	}); err == nil {
		t.Fatal("FIFO authorized cleanup")
	}
	if err := f.spool.withReferences(f.store, func(scope *spoolReferenceScope) error { return scope.putChunk(hash, body) }); err == nil {
		t.Fatal("FIFO authorized publication")
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 || f.spool.Used() != int64(len(body)) {
		t.Fatalf("FIFO or charge changed: %v", err)
	}
}
