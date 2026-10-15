package local

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/local/localtest"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/codex"
)

// Minimal synthetic records reproduce the ordering and shell command shape of
// a live Codex writer, without its harness context or unrelated messages.
func TestChangeCodexLiteralExecLiveShape(t *testing.T) {
	raw, err := os.ReadFile("testdata/codex-literal-exec-commits.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := localindex.Open(filepath.Join(t.TempDir(), "index.db"), localindex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	src := transcript.Source{Agent: transcript.AgentCodex, Path: path, Parser: codex.Name, StorageKind: transcript.StorageJSONLAppend}
	if err := localtest.IndexSource(ctx, store, &codex.Parser{}, src); err != nil {
		t.Fatal(err)
	}
	backend := &Backend{Store: store}
	for _, hash := range []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"} {
		page, err := backend.Change(ctx, format.ChangeQuery{Commits: []string{hash}}, format.Filters{Repo: "/tmp/synthetic-live-writer"})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Matches) != 1 {
			t.Fatalf("hash %s: %+v", hash, page)
		}
		match := page.Matches[0]
		if hash[0] == 'a' && match.ObservedHash != "aaaaaaa" {
			t.Fatalf("normal stdout observation missing: %+v", match)
		}
		if hash[0] == 'b' && match.ObservedHash != hash {
			t.Fatalf("quiet reveal missing: %+v", match)
		}
		if match.Operation != "commit" || !match.CommandRaw || !match.DirectoryKnown {
			t.Fatalf("evidence %+v", match)
		}
		if _, err := backend.RawAt(ctx, match.CommandAddress); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	backend.Remote = &fakeRemote{data: raw}
	page, err := backend.Change(ctx, format.ChangeQuery{Commits: []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}, format.Filters{Repo: "/tmp/synthetic-live-writer"})
	if err != nil || len(page.Matches) != 1 || page.Coverage.UnavailableSources != 1 {
		t.Fatalf("connected missing-source coverage: %+v %v", page, err)
	}
	if data, err := backend.RawAt(ctx, page.Matches[0].CommandAddress); !errors.Is(err, ErrNotLocal) || data != nil {
		t.Fatalf("connected missing-source evidence: %q %v", data, err)
	}
}
