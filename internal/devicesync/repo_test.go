package devicesync

import (
	"context"
	"os"
	"path/filepath"
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

func TestAntigravityLineageBackfillsAfterServerUpgrade(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	const child = "d18f778d-65eb-4daf-a402-244b505fae9a"
	const parent = "ec102b6f-5bea-419a-9dc4-555ef893421b"
	sp := e.spec(filepath.Join(child, ".system_generated", "logs", "transcript.jsonl"), transcript.StorageJSONLAppend)
	sp.Agent, sp.SessionKey, sp.Parser = transcript.AgentAntigravity, child, "antigravity@1"
	if err := os.MkdirAll(filepath.Dir(sp.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sp.Path, jsonlLines(1, 50, 200), 0o600); err != nil {
		t.Fatal(err)
	}
	e.sync(sp)
	n := e.srv.FlushRequests
	sp.AntigravityLineage = &syncproto.AntigravityLineage{ParentSessionID: parent, Depth: 1}

	// The old server accepts the source without acknowledging the new field.
	e.sync(sp)
	if e.srv.FlushRequests != n+1 {
		t.Fatalf("old server received %d lineage updates, want one", e.srv.FlushRequests-n)
	}
	src, err := e.store.source(t.Context(), sp.Path, nil)
	if err != nil || src.LineageAckedAt != 0 || !src.metadataPending() {
		t.Fatalf("old server advanced lineage state: %+v, err = %v", src, err)
	}

	observed := src.LineageObservedAt
	if observed <= 0 {
		t.Fatalf("initial lineage has no observation timestamp: %+v", src)
	}
	// After upgrade, the same unchanged transcript needs just one header.
	e.sy.tr = operationTransport{Transport: e.client, flush: func(ctx context.Context, req *syncproto.FlushRequest) (*syncproto.FlushResponse, error) {
		if len(req.Header.Entries) != 0 || req.Header.Tail != nil || req.Header.Source.AntigravityLineageObservedAt <= 0 || !sameLineage(req.Header.Source.AntigravityLineage, sp.AntigravityLineage) {
			t.Fatalf("expected a lineage-only flush, got %+v", req.Header)
		}
		resp, err := e.client.Flush(ctx, req)
		if err == nil {
			resp.AntigravityLineage = req.Header.Source.AntigravityLineage
		}
		return resp, err
	}}
	e.sync(sp)
	e.sync(sp)
	if e.srv.FlushRequests != n+2 {
		t.Fatalf("upgraded server received %d lineage updates, want one", e.srv.FlushRequests-n-1)
	}
	src, err = e.store.source(t.Context(), sp.Path, nil)
	if err != nil || src.LineageAckedAt != observed || src.LineageObservedAt != observed || src.metadataPending() {
		t.Fatalf("upgrade didn't persist acknowledgment: %+v, err = %v", src, err)
	}

	sp.AntigravityLineage = &syncproto.AntigravityLineage{}
	e.sync(sp)
	src, err = e.store.source(t.Context(), sp.Path, nil)
	if err != nil || src.LineageObservedAt <= observed || src.LineageAckedAt != src.LineageObservedAt || src.metadataPending() {
		t.Fatalf("explicit root didn't advance the observation: %+v, err = %v", src, err)
	}
}
