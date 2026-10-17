package retrieval

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/grep"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type metadataStreamTrace struct {
	mu           sync.Mutex
	declarations []string
}

func (tr *metadataStreamTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(d.SQL, "DECLARE ") {
		tr.mu.Lock()
		tr.declarations = append(tr.declarations, d.SQL)
		tr.mu.Unlock()
	}
	return ctx
}
func (*metadataStreamTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestGrepMetadataStreamEligibility(t *testing.T) {
	for _, tt := range []struct {
		pattern string
		want    bool
	}{
		{"spotlight|mdutil|mds_stores", true}, {"(foo|bar)(baz|quux)", true},
		{"mdutil", false}, {"spotlight", false}, {"foo.*bar", false}, {"sssss|mdutil", false}, {"(?i:ask)|(?-i:SKY)", false},
	} {
		p, err := grep.Compile(format.GrepQuery{Pattern: tt.pattern})
		if err != nil {
			t.Fatal(err)
		}
		if got := grepMetadataStreamEligible(p.Re); got != tt.want {
			t.Errorf("%s: %v want %v", tt.pattern, got, tt.want)
		}
	}
}

// More than 20,000 eligible rows must not discard the late match or restart
// expensive global text rechecks before yielding the first metadata page.
func TestGrepMetadataStreamOverflowAndContinuation(t *testing.T) {
	s, _ := perfCorpus(t, 1000, 100)
	ctx := context.Background()
	for _, sql := range []string{
		`ALTER TABLE messages ALTER COLUMN text SET STATISTICS 1000`,
		`UPDATE messages SET kind=CASE WHEN ordinal<21 THEN 'assistant' ELSE 'tool_result' END,
   text=CASE WHEN ordinal=99 THEN 'spotlight '||id::text WHEN ordinal=20 AND conversation_id=md5('c1')::uuid THEN 'mdſ_ſtoreſ late match' ELSE 'ordinary synthetic text' END,
   ts=CASE WHEN ordinal=20 AND conversation_id=md5('c1')::uuid THEN '2026-10-01'::timestamptz ELSE ts END`,
		`VACUUM ANALYZE`,
	} {
		if _, err := s.Pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	tr := &metadataStreamTrace{}
	cfg := s.Pool.Config()
	cfg.ConnConfig.Tracer = tr
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	store := &Store{Pool: pool, GrepContinuations: NewGrepContinuations(pool)}
	defer store.GrepContinuations.Close()
	var want string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM messages WHERE ordinal=20 AND conversation_id=md5('c1')::uuid`).Scan(&want); err != nil {
		t.Fatal(err)
	}
	f := format.Filters{Owner: "test-owner", Agent: "claude", Kinds: []string{"assistant"}, Sort: format.SortOldest}
	q := format.GrepQuery{Pattern: "spotlight|mdutil|mds_stores", Limit: 20}
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	page, err := store.Grep(c, q, f)
	if err != nil || page.Truncated || !page.Exact || len(page.Hits) != 1 || page.Hits[0].MessageID != want {
		t.Fatalf("ordinary: %+v err=%v", page, err)
	}
	if len(tr.declarations) != 1 || strings.Contains(tr.declarations[0], "m.text") {
		t.Fatalf("overflow retained global text query: %v", tr.declarations)
	}
	q.Continue, q.Authority = true, "stream-test"
	var got []string
	for pages := 0; ; pages++ {
		if pages > 100 {
			t.Fatal("metadata continuation did not exhaust")
		}
		page := walkPage(t, store, q, f)
		if pages == 0 && (len(page.Hits) != 0 || page.Scan.Status != "more" || page.Scan.NextCursor == "") {
			t.Fatalf("empty first metadata page lost continuation: %+v", page)
		}
		for _, h := range page.Hits {
			got = append(got, h.MessageID)
		}
		if page.Scan.Status == "complete" {
			break
		}
		if page.Scan.Status != "more" {
			t.Fatalf("scan: %+v", page.Scan)
		}
		q.Cursor = page.Scan.NextCursor
	}
	if !slices.Equal(got, []string{want}) {
		t.Fatalf("late match lost or duplicated: %v", got)
	}
	last := tr.declarations[len(tr.declarations)-1]
	if strings.Contains(last, "m.text") || strings.Contains(last, " LIMIT ") {
		t.Fatalf("continuation must stream uncapped metadata: %s", last)
	}
	// A newly eligible giant nonmatch must not be hydrated into memory or
	// silently skipped: report incompleteness before claiming absence.
	if _, err := pool.Exec(ctx, `UPDATE messages SET text=repeat('x',$1) WHERE conversation_id=md5('c1')::uuid AND ordinal=0`, grepContinuationBody+1); err != nil {
		t.Fatal(err)
	}
	oversized, err := store.Grep(ctx, format.GrepQuery{Pattern: "spotlight|mdutil|mds_stores", Limit: 20}, f)
	if err != nil || !oversized.Truncated || oversized.Exact || len(oversized.Hits) != 0 || !strings.Contains(oversized.Reason, "body limit") {
		t.Fatalf("oversized metadata candidate: %+v err=%v", oversized, err)
	}
}
