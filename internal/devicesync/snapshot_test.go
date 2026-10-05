package devicesync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

func TestSyncSnapshotPreservesPathAndBytesAcrossRetry(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := e.spec("original.jsonl", "cass_export")
	want := jsonlLines(13, 100, 400)
	if err := os.WriteFile(sp.Path, want, 0600); err != nil {
		t.Fatal(err)
	}
	id, err := transcript.StatIdentity(sp.Path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "checked.jsonl")
	if err := os.WriteFile(snapshot, want, 0400); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	e.srv.SetDown(true)
	if err := e.sy.SyncSnapshot(ctx, sp, snapshot, id); err == nil {
		t.Fatal("expected upload outage")
	}
	// Restart discards every held descriptor. The next unchanged capture
	// must install the staged descriptor before resuming pending bodies.
	e.sy.Close()
	sy, err := NewSyncer(e.sy.cfg, e.store, e.spool, e.client)
	if err != nil {
		t.Fatal(err)
	}
	defer sy.Close()
	if err := os.Remove(sp.Path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "unrelated.jsonl")
	if err := os.WriteFile(outside, []byte("unverified private replacement\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, sp.Path); err != nil {
		t.Fatal(err)
	}
	e.srv.SetDown(false)
	if err := sy.SyncSnapshot(ctx, sp, snapshot, id); err != nil {
		t.Fatal(err)
	}
	e.requireServerHas(sp.Path, id.ID.String(), 0, want)
	e.srv.Lock()
	before := e.srv.FlushRequests
	e.srv.Unlock()
	if err := sy.SyncSnapshot(ctx, sp, snapshot, id); err != nil {
		t.Fatal(err)
	}
	e.srv.Lock()
	after := e.srv.FlushRequests
	e.srv.Unlock()
	if after != before {
		t.Fatal("unchanged snapshot uploaded a new generation")
	}
}
