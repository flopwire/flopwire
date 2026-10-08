//go:build darwin || linux

package devicesync

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

func TestPrepareLegacyTailsRejectsFIFOWithoutRead(t *testing.T) {
	f := newTailPreparationFixture(t)
	body := []byte("literal pending export\n")
	sid, hash := f.add(t, transcript.StorageSQLite, true, body, nil, false)
	name, _ := tailVersionName(sid, 0, hash)
	if err := syscall.Mkfifo(filepath.Join(f.spool.dir, "tails", name), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := PrepareLegacyTails(t.Context(), f.store.db, f.spool.dir)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO accepted as immutable tail")
		}
	case <-time.After(time.Second):
		t.Fatal("preparation blocked reading a nonregular candidate")
	}
}
