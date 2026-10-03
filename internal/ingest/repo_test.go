package ingest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/transcript"
)

// #102: the server stores the repository the device placed each source's
// session in, from every flush; a header-only flush (the placement
// changed after every byte was acknowledged) updates it and leaves the
// bytes as they are.
func TestFlushRecordsRepository(t *testing.T) {
	e := newEnv(t)
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	data := []byte(`{"type":"session_meta","payload":{"id":"r1","cwd":"/p/web-wt"}}` + "\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	sp := devicesync.SourceSpec{Path: path, Agent: transcript.AgentCodex, StorageKind: transcript.StorageJSONLAppend,
		SessionKey: "r1", Parser: "codex@1", Checkout: "/p/web", Remote: "github.com/acme/web"}
	repoOf := func() (string, string) {
		t.Helper()
		var c, r *string
		if err := e.pool.QueryRow(e.ctx, `SELECT checkout,remote FROM sources WHERE device_id=$1 AND path=$2`, e.deviceID, path).Scan(&c, &r); err != nil {
			t.Fatal(err)
		}
		return deref(c), deref(r)
	}
	if err := sy.Sync(e.ctx, sp); err != nil {
		t.Fatal(err)
	}
	if c, r := repoOf(); c != "/p/web" || r != "github.com/acme/web" {
		t.Fatalf("first flush: %q %q", c, r)
	}
	gens := e.count(`SELECT count(*) FROM generations`)
	sp.Checkout, sp.Remote = "/q/web-clone", "github.com/acme/web"
	if err := sy.Sync(e.ctx, sp); err != nil {
		t.Fatal(err)
	}
	if c, r := repoOf(); c != "/q/web-clone" || r != "github.com/acme/web" {
		t.Fatalf("after the placement changed: %q %q", c, r)
	}
	if n := e.count(`SELECT count(*) FROM generations`); n != gens {
		t.Fatalf("a header-only flush made %d generations, had %d", n, gens)
	}
	// No repository known any more: the columns clear.
	sp.Checkout, sp.Remote = "", ""
	if err := sy.Sync(e.ctx, sp); err != nil {
		t.Fatal(err)
	}
	if c, r := repoOf(); c != "" || r != "" {
		t.Fatalf("cleared: %q %q", c, r)
	}
}
