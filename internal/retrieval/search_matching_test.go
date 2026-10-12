package retrieval

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/local"
	"github.com/flopwire/flopwire/internal/transcript"
)

// Requires FLOPWIRE_TEST_DATABASE_URL; uses existing PostgreSQL and SQLite
// lexical indexes against the identical synthetic corpus, without Docker.
func TestSearchMatchingLocalServerParity(t *testing.T) {
	f := newFindFixture(t)
	ctx := context.Background()
	idx, err := localindex.Open(filepath.Join(t.TempDir(), "index.db"), localindex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	src, err := idx.EnsureSource(ctx, transcript.Source{Agent: transcript.AgentClaude, Path: "/synthetic/matching.jsonl", FileID: transcript.FileID{Dev: 1, Ino: 1}, StorageKind: transcript.StorageJSONLAppend, Parser: "claude@1"})
	if err != nil {
		t.Fatal(err)
	}
	batch := localindex.Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "sess-1", Cwd: "/src/flopwire"}}}
	for i, text := range []string{"autoreview review only café can't", "UX review automatic edits", "UX review off always", "UX review only"} {
		kind := transcript.KindAssistant
		if i == 3 {
			kind = transcript.KindToolResult
		}
		f.add(kind.String(), text)
		batch.Messages = append(batch.Messages, &transcript.Message{SessionID: "sess-1", NativeID: fmt.Sprint(i), Ordinal: int64(i), Kind: kind, Text: text, FullLen: len(text), ContentSHA: sha256.Sum256([]byte(text)), Parser: "claude@1"})
	}
	if _, err = idx.ApplyBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	be := &local.Backend{Store: idx}
	query := "autoreview automatic edits off always review only"
	for _, tc := range []struct {
		query, mode string
		offset      int
		filter      format.Filters
		want        int
	}{
		{query, "auto", 0, format.Filters{}, 1},
		{query, "auto", 100, format.Filters{}, 0},
		{query, "all", 0, format.Filters{}, 0},
		{query, "any", 0, format.Filters{}, 4},
		{`autoreview missing "review only"`, "auto", 0, format.Filters{}, 1},
		{`autoreview missing "UX review"`, "auto", 0, format.Filters{}, 0},
		{`autoreview "review only"`, "any", 0, format.Filters{}, 1},
		{"autoreview autoreview, vanished?", "auto", 0, format.Filters{}, 1},
		{"café can't vanished", "auto", 0, format.Filters{}, 1},
		{"neverindexed vanished", "auto", 0, format.Filters{}, 0},
		{"how did we", "auto", 0, format.Filters{}, 0},
		{query, "auto", 0, format.Filters{Kinds: []string{"assistant"}}, 1},
		{query, "auto", 0, format.Filters{Repo: "/src/flopwire"}, 1},
		{query, "auto", 0, format.Filters{Session: "sess-1"}, 1},
	} {
		t.Run(fmt.Sprintf("%s/%s/%d/%+v", tc.query, tc.mode, tc.offset, tc.filter), func(t *testing.T) {
			q := format.SearchQuery{Query: tc.query, Matching: tc.mode, Offset: tc.offset, Limit: 20}
			sp, err := f.s.Search(ctx, q, tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			lp, err := be.Search(ctx, q, tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			texts := func(p *format.Page) []string {
				var out []string
				for _, h := range p.Hits {
					out = append(out, h.Snippet)
				}
				slices.Sort(out)
				return out
			}
			if len(sp.Hits) != tc.want || len(lp.Hits) != tc.want || !slices.Equal(texts(sp), texts(lp)) || !slices.Equal(sp.Notes, lp.Notes) {
				t.Fatalf("server %+v; local %+v", sp, lp)
			}
		})
	}
	for _, query := range []string{"autoreview OR automatic", "review -UX"} {
		p, err := f.s.Search(ctx, format.SearchQuery{Query: query}, format.Filters{})
		if err != nil || len(p.Hits) == 0 || len(p.Notes) != 0 {
			t.Fatalf("operator %s: %+v %v", query, p, err)
		}
		if strings.Contains(query, "-UX") && len(p.Hits) != 1 {
			t.Fatalf("negative semantics lost: %+v", p)
		}
	}
}
