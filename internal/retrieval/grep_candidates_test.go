package retrieval

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

func TestGrepCandidateHydrationSnapshot(t *testing.T) {
	for _, change := range []string{"redact", "delete", "hide", "metadata"} {
		t.Run(change, func(t *testing.T) {
			f := newFindFixture(t)
			f.add("user", "needle first")
			f.add("assistant", "needle second")
			ctx := context.Background()
			q := &query{}
			q.where("m.text ILIKE " + q.arg("%needle%"))
			if err := hitFilters(q, format.Filters{}); err != nil {
				t.Fatal(err)
			}
			var target string
			err := f.s.readTransaction(ctx, time.Second*5, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
				rows, err := tx.Query(ctx, grepWideCandidates(q, false), q.args...)
				if err != nil {
					return err
				}
				want := collectCandidateRows(t, rows)
				target = want[0].Hit.MessageID
				if _, err = tx.Exec(ctx, `DECLARE snapshot_candidates CURSOR FOR `+grepCandidates(q, false), q.args...); err != nil {
					return err
				}
				rows, err = tx.Query(ctx, `FETCH 256 FROM snapshot_candidates`)
				if err != nil {
					return err
				}
				ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
				if err != nil {
					return err
				}
				mutation := map[string]string{
					"redact":   `UPDATE messages SET text='[redacted]' WHERE id=$1::uuid`,
					"delete":   `DELETE FROM messages WHERE id=$1::uuid`,
					"hide":     `UPDATE conversations SET hidden_at=now() WHERE id=(SELECT conversation_id FROM messages WHERE id=$1::uuid)`,
					"metadata": `UPDATE conversations SET title='new title' WHERE id=(SELECT conversation_id FROM messages WHERE id=$1::uuid)`,
				}[change]
				if _, err = f.s.Pool.Exec(ctx, mutation, target); err != nil {
					return err
				}
				rows, err = tx.Query(ctx, grepCandidateBodies, ids)
				if err != nil {
					return err
				}
				got := collectCandidateRows(t, rows)
				if !reflect.DeepEqual(got, want) {
					t.Fatal("hydration escaped the candidate cursor snapshot")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			page, err := f.s.Grep(ctx, format.GrepQuery{Pattern: "needle", Fixed: true}, format.Filters{})
			if err != nil {
				t.Fatal(err)
			}
			if change == "hide" {
				if len(page.Hits) != 0 {
					t.Fatal("next request showed hidden conversation")
				}
				return
			}
			for _, hit := range page.Hits {
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
	}
}
