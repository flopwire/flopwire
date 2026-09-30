package localindex

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

// A shard that lost committed work (a crash between the main commit and
// the shard's) catches up from fts_queue on the next Open.
func TestShardReplaysQueueOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "i.db")
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	src, err := s.EnsureSource(ctx, transcript.Source{Agent: "claude", Path: "/x.jsonl", StorageKind: transcript.StorageJSONLAppend, Parser: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyBatch(ctx, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{
		msg("s1", "n1", 0, transcript.KindUser, "the quick brown fox"),
		msg("s1", "n2", 100, transcript.KindAssistant, "jumps over the lazy dog"),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Roll a trigram part back to before that commit.
	db, err := sql.Open("sqlite", "file:"+path+"-tri0")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DELETE FROM fts_tri`, `UPDATE fts_meta SET value = 0 WHERE key = 'applied'`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	s, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	hits, err := s.Find(ctx, "lazy dog", FindOptions{})
	if err != nil || len(hits) != 1 {
		t.Fatalf("after replay: %d hits, %v", len(hits), err)
	}
	if got, _ := s.Search(ctx, "quick fox", SearchOptions{}); len(got) != 1 {
		t.Fatalf("token shard: %d hits", len(got))
	}
}

// A shard file left from a deleted index is rebuilt, not trusted.
func TestStaleShardIsRebuilt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "i.db")
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	src, _ := s.EnsureSource(ctx, transcript.Source{Agent: "claude", Path: "/x.jsonl", StorageKind: transcript.StorageJSONLAppend, Parser: "x"})
	for i, text := range []string{"alpha beta gamma", "delta epsilon"} {
		if _, err := s.ApplyBatch(ctx, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{msg("s1", string(rune('a'+i)), int64(i*100), transcript.KindUser, text)}}); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	s, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	src, _ = s.EnsureSource(ctx, transcript.Source{Agent: "claude", Path: "/x.jsonl", StorageKind: transcript.StorageJSONLAppend, Parser: "x"})
	if _, err := s.ApplyBatch(ctx, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{msg("s1", "z", 0, transcript.KindUser, "zeta eta theta")}}); err != nil {
		t.Fatal(err)
	}
	if hits, _ := s.Find(ctx, "zeta eta", FindOptions{}); len(hits) != 1 {
		t.Fatalf("new row: %d hits", len(hits))
	}
	if hits, _ := s.Find(ctx, "alpha beta", FindOptions{}); len(hits) != 0 {
		t.Fatalf("stale shard entry matched: %d hits", len(hits))
	}
}

func TestShardPageSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "i.db")
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var ps int
	var mode string
	if err := s.DB().QueryRow(`PRAGMA tri0.page_size`).Scan(&ps); err != nil || ps != shardPageSize {
		t.Fatalf("tri page size %d %v", ps, err)
	}
	if err := s.DB().QueryRow(`PRAGMA tri1.journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("tri journal mode %q %v", mode, err)
	}
}

// The trigram parts merge newest first, and no part sorts.
func TestTrigramPartsMergeInOrder(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "i.db"), Options{TriParts: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	src, _ := s.EnsureSource(ctx, transcript.Source{Agent: "claude", Path: "/x.jsonl", StorageKind: transcript.StorageJSONLAppend, Parser: "x"})
	var msgs []*transcript.Message
	for i := range 30 {
		msgs = append(msgs, msg("s1", "n"+strconv.Itoa(i), int64(i*100), transcript.KindUser, "needle number "+strconv.Itoa(i)))
	}
	if _, err := s.ApplyBatch(ctx, Batch{SourceID: src.ID, Generation: 1, Messages: msgs}); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Find(ctx, "needle", FindOptions{Limit: 100})
	if err != nil || len(hits) != 30 {
		t.Fatalf("%d hits, %v", len(hits), err)
	}
	for i := 1; i < len(hits); i++ {
		if hits[i].ID >= hits[i-1].ID {
			t.Fatalf("hits not newest first: %d after %d", hits[i].ID, hits[i-1].ID)
		}
	}
	for _, part := range s.tri {
		rows, err := s.DB().Query(`EXPLAIN QUERY PLAN SELECT t.rowid FROM `+part.schema+`.fts_tri t WHERE t.fts_tri MATCH ? ORDER BY t.rowid DESC`, s.substringExpr("needle"))
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, parent, unused int
			var detail string
			rows.Scan(&id, &parent, &unused, &detail)
			if strings.Contains(detail, "TEMP B-TREE") {
				t.Errorf("part %s sorts: %s", part.schema, detail)
			}
		}
		rows.Close()
	}
}
