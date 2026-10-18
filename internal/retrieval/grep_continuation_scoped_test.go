package retrieval

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type scopedContinuationTrace struct {
	mu           sync.Mutex
	declarations []string
}

func (tr *scopedContinuationTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "DECLARE resumable_grep ") {
		tr.mu.Lock()
		tr.declarations = append(tr.declarations, data.SQL)
		tr.mu.Unlock()
	}
	return ctx
}
func (*scopedContinuationTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestGrepContinuationScopedCandidates(t *testing.T) {
	s, _ := perfCorpus(t, 3, 12)
	ctx := context.Background()
	var owner, session string
	if err := s.Pool.QueryRow(ctx, `SELECT md5('u1')::uuid::text,md5('s1')`).Scan(&owner, &session); err != nil {
		t.Fatal(err)
	}
	tr := &scopedContinuationTrace{}
	cfg := s.Pool.Config()
	cfg.ConnConfig.Tracer = tr
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	scoped := &Store{Pool: pool, GrepContinuations: NewGrepContinuations(pool)}
	t.Cleanup(scoped.GrepContinuations.Close)
	for _, fixed := range []bool{true, false} {
		for _, sort := range []string{format.SortNewest, format.SortOldest} {
			f := format.Filters{Owner: owner, Session: session, Kinds: []string{"user", "assistant"}, Sort: sort}
			q := format.GrepQuery{Pattern: "step", Fixed: fixed, Limit: 100}
			ordinary, err := s.Grep(ctx, q, f)
			if err != nil || ordinary.Truncated || len(ordinary.Hits) == 0 {
				t.Fatalf("ordinary: %+v %v", ordinary, err)
			}
			var want, got []string
			for _, hit := range ordinary.Hits {
				want = append(want, hit.MessageID)
			}
			q.Continue, q.Authority, q.Limit = true, "test-device", 2
			for pages := 0; ; pages++ {
				if pages > 12 {
					t.Fatal("continuation did not finish")
				}
				page := walkPage(t, scoped, q, f)
				for _, hit := range page.Hits {
					got = append(got, hit.MessageID)
				}
				if page.Scan.Status == "complete" {
					break
				}
				if page.Scan.Status != "more" {
					t.Fatalf("scan: %+v", page.Scan)
				}
				q.Cursor = page.Scan.NextCursor
			}
			if !slices.Equal(got, want) {
				t.Fatalf("fixed=%v sort=%s: got %v want %v", fixed, sort, got, want)
			}
		}
	}
	// An empty scoped cohort must still declare an empty cursor and finish.
	page := walkPage(t, scoped, format.GrepQuery{Pattern: "step", Fixed: true, Continue: true, Authority: "test-device"}, format.Filters{Owner: owner, Agent: "missing-agent"})
	if page.Scan.Status != "complete" || page.Scan.Checked != 0 || len(page.Hits) != 0 {
		t.Fatalf("empty scope: %+v", page)
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(tr.declarations) != 5 {
		t.Fatalf("declarations=%d want 5", len(tr.declarations))
	}
	for _, sql := range tr.declarations {
		if !strings.Contains(sql, "m.conversation_id=ANY(") {
			t.Fatalf("scoped cursor lost indexed conversation narrowing: %s", sql)
		}
	}
}
