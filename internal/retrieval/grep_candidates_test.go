package retrieval

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type candidateRow struct {
	Hit  format.Hit
	SHA  []byte
	Body string
}

func collectCandidateRows(t testing.TB, rows pgx.Rows) []candidateRow {
	t.Helper()
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidateRow, error) {
		var r candidateRow
		var err error
		r.Hit, err = scanHit(row, &r.SHA, &r.Body)
		return r, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestGrepCandidateRowsEquivalent(t *testing.T) {
	s, _ := perfCorpus(t, 20, 40)
	ctx := context.Background()
	for _, sql := range []string{
		`UPDATE messages SET ts=NULL WHERE ordinal%11=0`,
		`UPDATE messages SET ts='2026-09-01' WHERE ordinal%5=0`,
		`UPDATE messages SET superseded=true WHERE ordinal=2`,
		`UPDATE messages SET on_active_path=false WHERE ordinal=3`,
		`UPDATE conversations SET hidden_at=now() WHERE id=md5('c2')::uuid`,
	} {
		if _, err := s.Pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	for _, oldest := range []bool{false, true} {
		for i, f := range []format.Filters{
			{}, {Kinds: []string{"user", "assistant"}}, {Repo: "/src/repo1"},
			{Since: time.Date(2026, 9, 1, 0, 10, 0, 0, time.UTC)},
			{IncludeSuperseded: true, IncludeBranches: true},
		} {
			t.Run(fmt.Sprintf("oldest=%v/filter=%d", oldest, i), func(t *testing.T) {
				q := &query{}
				q.where("m.text ILIKE " + q.arg("%step%"))
				if err := hitFilters(q, f); err != nil {
					t.Fatal(err)
				}
				rows, err := s.Pool.Query(ctx, grepWideCandidates(q, oldest), q.args...)
				if err != nil {
					t.Fatal(err)
				}
				want := collectCandidateRows(t, rows)
				rows, err = s.Pool.Query(ctx, grepCandidates(q, oldest), q.args...)
				if err != nil {
					t.Fatal(err)
				}
				ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
				if err != nil {
					t.Fatal(err)
				}
				var got []candidateRow
				for start := 0; start < len(ids); start += 256 {
					rows, err = s.Pool.Query(ctx, grepCandidateBodies, ids[start:min(start+256, len(ids))])
					if err != nil {
						t.Fatal(err)
					}
					got = append(got, collectCandidateRows(t, rows)...)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("candidate order/body/metadata differ: got %d want %d", len(got), len(want))
				}
			})
		}
	}
}

// Inject a committed mutation at the actual Grep hydration statement. This
// fails if Grep itself drops its repeatable-read snapshot, even when helper
// transaction tests still use the correct isolation level.
type grepHydrationMutation struct {
	once   sync.Once
	change func()
}

func (m *grepHydrationMutation) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if data.SQL == grepCandidateBodies {
		m.once.Do(m.change)
	}
	return ctx
}
func (*grepHydrationMutation) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestGrepCandidateHydrationSnapshot(t *testing.T) {
	for _, change := range []string{"redact", "delete", "hide", "metadata"} {
		t.Run(change, func(t *testing.T) {
			f := newFindFixture(t)
			f.add("user", "needle first")
			f.add("assistant", "needle second")
			ctx := context.Background()
			query := format.GrepQuery{Pattern: "needle", Fixed: true}
			want, err := f.s.Grep(ctx, query, format.Filters{})
			if err != nil {
				t.Fatal(err)
			}
			target := want.Hits[0].MessageID
			mutation := map[string]string{
				"redact":   `UPDATE messages SET text='[redacted]' WHERE id=$1::uuid`,
				"delete":   `DELETE FROM messages WHERE id=$1::uuid`,
				"hide":     `UPDATE conversations SET hidden_at=now() WHERE id=(SELECT conversation_id FROM messages WHERE id=$1::uuid)`,
				"metadata": `UPDATE conversations SET title='new title' WHERE id=(SELECT conversation_id FROM messages WHERE id=$1::uuid)`,
			}[change]
			var mutationErr error
			changed := false
			cfg := f.s.Pool.Config()
			cfg.ConnConfig.Tracer = &grepHydrationMutation{change: func() {
				_, mutationErr = f.s.Pool.Exec(ctx, mutation, target)
				changed = true
			}}
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			actual := *f.s
			actual.Pool = pool
			got, err := actual.Grep(ctx, query, format.Filters{})
			if err != nil {
				t.Fatal(err)
			}
			if mutationErr != nil {
				t.Fatal(mutationErr)
			}
			if !changed {
				t.Fatal("actual Grep hydration injection did not run")
			}
			if !reflect.DeepEqual(got.Hits, want.Hits) {
				t.Fatal("actual Grep hydration escaped the candidate cursor snapshot")
			}
			next, err := f.s.Grep(ctx, query, format.Filters{})
			if err != nil {
				t.Fatal(err)
			}
			if change == "hide" {
				if len(next.Hits) != 0 {
					t.Fatal("next request showed hidden conversation")
				}
				return
			}
			for _, hit := range next.Hits {
				if hit.MessageID == target {
					if change != "metadata" {
						t.Fatalf("next request returned %s candidate", change)
					}
					if hit.Title != "new title" {
						t.Fatal("next request kept stale metadata")
					}
					return
				}
			}
			if change == "metadata" {
				t.Fatal("next request lost updated candidate")
			}
		})
	}
}

func TestGrepCandidateCapOrder(t *testing.T) {
	s, _ := perfCorpus(t, 2200, 100)
	ctx := context.Background()
	q := &query{}
	q.where("m.text ILIKE " + q.arg("%step%"))
	if err := hitFilters(q, format.Filters{}); err != nil {
		t.Fatal(err)
	}
	for _, oldest := range []bool{false, true} {
		rows, err := s.Pool.Query(ctx, grepCandidates(q, oldest), q.args...)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			t.Fatal(err)
		}
		order := "DESC"
		if oldest {
			order = ""
		}
		rows, err = s.Pool.Query(ctx, `SELECT id FROM messages ORDER BY ts `+order+` NULLS LAST,id LIMIT 200000`)
		if err != nil {
			t.Fatal(err)
		}
		want, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != maxCandidates || !reflect.DeepEqual(ids, want) {
			t.Fatalf("oldest=%v cap/order changed: %d rows", oldest, len(ids))
		}
		tx, err := s.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conversations, complete, err := grepConversations(ctx, tx, format.Filters{Agent: "claude"})
		if err != nil || !complete {
			tx.Rollback(ctx)
			t.Fatalf("conversations: %v", err)
		}
		sql, args, err := grepMetadataStreamQuery(format.Filters{}, conversations, oldest, maxCandidates)
		if err != nil {
			tx.Rollback(ctx)
			t.Fatal(err)
		}
		metadataIDs := filteredCandidateIDs(t, tx, sql, args...)
		tx.Rollback(ctx)
		if !reflect.DeepEqual(metadataIDs, want) {
			t.Fatalf("metadata cap/order changed oldest=%v count=%d", oldest, len(metadataIDs))
		}
	}

	// A large matchless metadata cohort still reaches the verification cap.
	// Global matches outside its kind filter keep the text plan broad.
	for _, sql := range []string{
		`ALTER TABLE messages ALTER COLUMN text SET STATISTICS 1000`,
		`UPDATE messages SET kind=CASE WHEN conversation_id=md5('c2200')::uuid THEN 'tool_result' ELSE 'assistant' END,
         text=CASE WHEN conversation_id=md5('c2200')::uuid THEN 'spotlight '||id::text ELSE 'ordinary small body' END`,
		`VACUUM ANALYZE`,
	} {
		if _, err := s.Pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	bounded, cancel := context.WithTimeout(ctx, MaxBudget)
	defer cancel()
	page, err := s.Grep(bounded, format.GrepQuery{Pattern: "spotlight|mdutil|mds_stores", Mode: format.ModeCount}, format.Filters{Agent: "claude", Kinds: []string{"assistant"}})
	if err != nil || !page.Truncated || page.Exact || page.Total != 0 || !strings.Contains(page.Reason, "verifying 200000 candidates") {
		t.Fatalf("metadata cap claimed absence or failed: %+v err=%v", page, err)
	}
}
