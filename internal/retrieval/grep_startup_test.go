package retrieval

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Each stall interrupts the named database operation before any candidate
// is checked. This catches misattribution of preselection to FETCH.
func TestGrepStartupTimeoutStages(t *testing.T) {
	for _, tc := range []struct {
		name, write, reason string
		filters             format.Filters
	}{
		{"transaction", "begin", "acquiring the read transaction", format.Filters{}},
		{"sessions", "SELECT c.id::text FROM", "resolving filtered sessions", format.Filters{Agent: "claude"}},
		{"planning", "EXPLAIN (FORMAT JSON)", "planning text candidates", format.Filters{Session: "sess-1", Kinds: []string{"user"}}},
		{"declare", "DECLARE grep_candidates", "declaring the candidate cursor", format.Filters{}},
		{"fetch", "FETCH 256 FROM grep_candidates", "fetching the first candidate page", format.Filters{}},
		{"hydrate", "WITH ORDINALITY", "loading candidate bodies and metadata", format.Filters{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFindFixture(t)
			f.add("user", "upload")
			plan := &stallPlan{onWrite: []byte(tc.write)}
			s := f.stallStore(plan)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			page, err := s.Grep(ctx, format.GrepQuery{Pattern: "upload", Fixed: tc.name != "planning"}, tc.filters)
			if err != nil {
				t.Fatal(err)
			}
			plan.wasCut(t)
			if !page.Truncated || page.Exact || len(page.Hits) != 0 || page.Total != 0 || !strings.Contains(page.Reason, tc.reason) || !strings.Contains(page.Reason, "checked 0 candidates") {
				t.Fatalf("wrong startup diagnostic: %+v", page)
			}
		})
	}
}

func TestGrepStartupPoolWait(t *testing.T) {
	f := newFindFixture(t)
	f.add("user", "upload")
	cfg := f.s.Pool.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	held, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	page, err := (&Store{Pool: pool}).Grep(ctx, format.GrepQuery{Pattern: "upload", Fixed: true}, format.Filters{})
	if err != nil {
		t.Fatal(err)
	}
	if !page.Truncated || page.Exact || page.Total != 0 || len(page.Hits) != 0 || !strings.Contains(page.Reason, "acquiring the read transaction") {
		t.Fatalf("pool wait: %+v", page)
	}
}

// At exactly the cap, every qualifying conversation (including undated
// sessions) must remain in the list. One more discards the entire list.
func TestGrepConversationPreselectionBoundary(t *testing.T) {
	s, _ := perfCorpus(t, grepConvCap+1, 1)
	ctx := context.Background()
	check := func(want []string) {
		t.Helper()
		tx, err := s.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		ids, ok, err := grepConversations(ctx, tx, format.Filters{Agent: "claude"})
		if err != nil {
			t.Fatal(err)
		}
		if want == nil {
			if ok || ids != nil {
				t.Fatalf("over cap: ids=%d ok=%v", len(ids), ok)
			}
			return
		}
		slices.Sort(ids)
		slices.Sort(want)
		if !ok || !slices.Equal(ids, want) {
			t.Fatalf("preselection: ids=%d want=%d ok=%v", len(ids), len(want), ok)
		}
	}
	check(nil)
	// Visibility is mirrored into conversation_activity by the store's
	// trigger. Hiding one session takes the visible set exactly to the cap.
	if _, err := s.Pool.Exec(ctx, `UPDATE conversations SET hidden_at=now() WHERE id=md5('c5001')::uuid`); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Pool.Query(ctx, `SELECT md5('c'||i)::uuid::text FROM generate_series(1,5000) i`)
	if err != nil {
		t.Fatal(err)
	}
	want, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	check(want)
	// The hidden session must not become a candidate in the narrowed scan.
	page, err := s.Grep(ctx, format.GrepQuery{Pattern: "step 0 of session 5001", Fixed: true}, format.Filters{Agent: "claude"})
	if err != nil || page.Truncated || page.Total != 0 || len(page.Hits) != 0 {
		t.Fatalf("hidden session: page=%+v err=%v", page, err)
	}
}
