package retrieval_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/local"
	"github.com/flopwire/flopwire/internal/retrieval/local/localtest"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
)

// A local generation and an archive generation are independent snapshots,
// even when path, inode and record offsets agree. Exercise real device sync,
// PostgreSQL archive metadata and S3 reads with a deliberately lagging index.
func TestLocalRawRefusesRewrittenArchive(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	s.ingestFixtures()
	idx, err := localindex.Open(filepath.Join(t.TempDir(), "index.db"), localindex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	if err := localtest.IndexHome(ctx, idx, s.home); err != nil {
		t.Fatal(err)
	}
	backend := &local.Backend{Store: idx, Remote: s.client}
	page, err := backend.Search(ctx, format.SearchQuery{Query: "exponential backoff"}, format.Filters{})
	if err != nil || len(page.Hits) == 0 {
		t.Fatalf("local search: %+v %v", page, err)
	}
	hit := page.Hits[0]
	original, err := backend.RawAt(ctx, hit.Address)
	if err != nil {
		t.Fatal(err)
	}
	path := hit.Provenance.Path
	before, err := transcript.StatIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	source := file0(t, path)
	rewritten := bytes.ReplaceAll(source, []byte("backoff"), []byte("rewrite"))
	if bytes.Equal(source, rewritten) {
		t.Fatal("fixture lacks rewrite target")
	}
	if err := os.WriteFile(path, rewritten, 0600); err != nil {
		t.Fatal(err)
	}
	after, err := transcript.StatIdentity(path)
	if err != nil || before.ID != after.ID || before.Size != after.Size {
		t.Fatalf("not same-inode, same-size rewrite: %+v %+v %v", before, after, err)
	}
	if data, err := backend.RawAt(ctx, hit.Address); !errors.Is(err, local.ErrNotLocal) || data != nil {
		t.Fatalf("rewritten local source: %q %v", data, err)
	}
	if err := s.sy.Sync(ctx, devicesync.SourceSpec{Path: path, Agent: transcript.AgentClaude, StorageKind: transcript.StorageJSONLAppend, Parser: claude.ParserName}); err != nil {
		t.Fatal(err)
	}
	if err := s.queue.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	var generations int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM generations g JOIN sources s ON s.id=g.source_id WHERE s.path=$1 AND s.file_id=$2`, path, before.ID.String()).Scan(&generations); err != nil {
		t.Fatal(err)
	}
	if generations != 2 {
		t.Fatalf("archive generations=%d, want 2", generations)
	}
	// Explicit archive reads still address either snapshot correctly.
	off, n := *hit.Provenance.ByteOffset, hit.Provenance.ByteLen
	old, err := s.client.RawByPath(ctx, path, before.ID.String(), 0, off, n)
	if err != nil || !bytes.Equal(old, original) {
		t.Fatalf("original archived record: %q %v", old, err)
	}
	latest, err := s.client.RawByPath(ctx, path, before.ID.String(), -1, off, n)
	if err != nil || bytes.Equal(latest, original) {
		t.Fatalf("rewritten archive did not differ: %q %v", latest, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if data, err := backend.RawAt(ctx, hit.Address); !errors.Is(err, local.ErrNotLocal) || data != nil {
		t.Fatalf("missing local source substituted archive: %q %v", data, err)
	}
}
