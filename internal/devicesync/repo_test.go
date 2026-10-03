package devicesync

import (
	"context"
	"os"
	"testing"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

// #102: every flush carries the session's repository (main checkout and
// remote). A source whose bytes are all acknowledged when its
// repository becomes known or changes (the deleted-worktree recovery
// pass placed it) reports it once with a header-only flush; nothing is
// sent again while it stays the same.
func TestRepoReachesServer(t *testing.T) {
	e := newEnv(t, Config{}, 1<<30)
	sp := e.spec("r.jsonl", transcript.StorageJSONLAppend)
	if err := os.WriteFile(sp.Path, jsonlLines(1, 50, 200), 0o600); err != nil {
		t.Fatal(err)
	}
	sp.Checkout, sp.Remote = "/p/web", "github.com/acme/web"
	e.sync(sp)
	fid := fileIDOf(t, sp.Path)
	desc := func() syncproto.Source {
		d, _, ok := e.srv.Source(sp.Path, fid)
		if !ok {
			t.Fatal("no source on the server")
		}
		return d
	}
	if d := desc(); d.Checkout != "/p/web" || d.Remote != "github.com/acme/web" {
		t.Fatalf("first upload: %+v", d)
	}
	n := e.srv.FlushRequests
	e.sync(sp)
	if e.srv.FlushRequests != n {
		t.Fatalf("an unchanged source flushed %d times", e.srv.FlushRequests-n)
	}
	// Placed again: another checkout of the same remote.
	sp.Checkout = "/q/web-clone"
	e.sync(sp)
	if d := desc(); d.Checkout != "/q/web-clone" || e.srv.FlushRequests != n+1 {
		t.Fatalf("after the repository changed: %+v, %d flushes", d, e.srv.FlushRequests-n)
	}
	e.sync(sp)
	if e.srv.FlushRequests != n+1 {
		t.Fatalf("reported twice: %d flushes", e.srv.FlushRequests-n)
	}
	// The state survives a restart: the store, not memory, says it was sent.
	src, err := e.store.source(context.Background(), sp.Path, nil)
	if err != nil || src.RepoSent != sp.repoKey() || src.Spec.Checkout != "/q/web-clone" {
		t.Fatalf("stored: %+v %v", src, err)
	}
}
