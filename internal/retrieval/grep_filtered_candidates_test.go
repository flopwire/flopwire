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

func filteredCandidateIDs(t testing.TB, tx pgx.Tx, sql string, args ...any) []uuid.UUID {
	t.Helper()
	rows, err := tx.Query(context.Background(), sql, args...)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestGrepFilteredCandidateEquivalence(t *testing.T) {
	s, _ := perfCorpus(t, 12, 40)
	ctx := context.Background()
	for _, sql := range []string{
		`UPDATE messages SET ts=NULL WHERE ordinal%11=0`,
		`UPDATE messages SET ts='2026-09-01' WHERE ordinal%5=0`,
		`UPDATE messages SET superseded=true WHERE ordinal=2`,
		`UPDATE messages SET on_active_path=false WHERE ordinal=3`,
		`UPDATE messages SET kind='injected' WHERE ordinal=4`,
		`UPDATE messages SET tool_name='shell' WHERE ordinal=6`,
		`UPDATE conversations SET hidden_at=now() WHERE id=md5('c2')::uuid`,
		`UPDATE conversations SET parent_native_session_id=md5('s1'),depth=1 WHERE id=md5('c3')::uuid`,
	} {
		if _, err := s.Pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	var self, owner string
	if err := s.Pool.QueryRow(ctx, `SELECT md5('s1'),md5('u1')::uuid::text`).Scan(&self, &owner); err != nil {
		t.Fatal(err)
	}
	for _, oldest := range []bool{false, true} {
		for n, f := range []format.Filters{
			{}, {Kinds: []string{"assistant"}}, {ExcludeKinds: []string{"tool_result"}},
			{Kinds: []string{"assistant"}, Since: time.Date(2026, 9, 1, 0, 4, 0, 0, time.UTC)},
			{Until: time.Date(2026, 9, 1, 0, 4, 0, 0, time.UTC)},
			{Tools: []string{"SHELL"}}, {IncludeSuperseded: true, IncludeBranches: true},
			{ExcludeSession: self}, {ExcludeConversation: sessionAt(t, s.Pool, 4)},
			{Session: self, Self: true, Owner: owner}, {Session: self, Self: true, Owner: uuid.NewString()},
			{ExcludeSubagents: true}, {Kinds: []string{"injected"}},
		} {
			t.Run(fmt.Sprintf("oldest=%v/filter=%d", oldest, n), func(t *testing.T) {
				f.Agent = "claude"
				tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(ctx)
				conversations, ok, err := grepConversations(ctx, tx, f)
				if err != nil || !ok {
					t.Fatalf("conversations complete=%v err=%v", ok, err)
				}
				q := &query{}
				q.where("m.text ILIKE " + q.arg("%step%"))
				if err := hitFilters(q, f); err != nil {
					t.Fatal(err)
				}
				q.where("m.conversation_id=ANY(" + q.arg(conversations) + "::uuid[])")
				want := filteredCandidateIDs(t, tx, grepCandidates(q, oldest), q.args...)
				ids, complete, err := grepMetadataCandidates(ctx, tx, f, conversations)
				if err != nil || !complete {
					t.Fatalf("metadata complete=%v err=%v", complete, err)
				}
				sql := grepFilteredCandidates(q, ids, oldest)
				got := filteredCandidateIDs(t, tx, sql, q.args...)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("ordered IDs differ got=%d want=%d", len(got), len(want))
				}
				tx.Rollback(ctx)
				if oldest {
					f.Sort = format.SortOldest
				} else {
					f.Sort = format.SortNewest
				}
				page, err := s.Grep(ctx, format.GrepQuery{Pattern: "step", Fixed: true, Limit: 500}, f)
				if err != nil || page.Truncated || !page.Exact || page.Total != len(want) {
					t.Fatalf("filtered grep: %+v %v expected=%d", page, err, len(want))
				}
				for i, h := range page.Hits {
					if h.MessageID != want[i].String() {
						t.Fatal("page order differs")
					}
				}
			})
		}
	}
}

func TestGrepFilteredMetadataBoundaryAndFallback(t *testing.T) {
	s, _ := perfCorpus(t, 1, grepMetadataCap+1)
	ctx := context.Background()
	if _, err := s.Pool.Exec(ctx, `UPDATE messages SET kind='assistant',text=CASE WHEN ordinal=$1 THEN 'unique filtered needle' ELSE 'ordinary synthetic content' END`, grepMetadataCap); err != nil {
		t.Fatal(err)
	}
	f := format.Filters{Agent: "claude", Kinds: []string{"assistant"}}
	check := func(wantComplete bool, wantCount int) {
		t.Helper()
		tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		convs, ok, err := grepConversations(ctx, tx, f)
		if err != nil || !ok {
			t.Fatal(err)
		}
		ids, complete, err := grepMetadataCandidates(ctx, tx, f, convs)
		if err != nil || complete != wantComplete || len(ids) != wantCount {
			t.Fatalf("complete=%v ids=%d err=%v", complete, len(ids), err)
		}
	}
	check(false, 0)
	page, err := s.Grep(ctx, format.GrepQuery{Pattern: "unique filtered needle", Fixed: true}, f)
	if err != nil || page.Truncated || page.Total != 1 {
		t.Fatalf("overflow lost late text match: %+v %v", page, err)
	}
	if _, err := s.Pool.Exec(ctx, `DELETE FROM messages WHERE ordinal=0`); err != nil {
		t.Fatal(err)
	}
	check(true, grepMetadataCap)
	page, err = s.Grep(ctx, format.GrepQuery{Pattern: "unique filtered needle", Fixed: true}, f)
	if err != nil || page.Truncated || page.Total != 1 {
		t.Fatalf("exact cap lost text match: %+v %v", page, err)
	}
	f.Kinds = []string{"missing"}
	check(true, 0)
	page, err = s.Grep(ctx, format.GrepQuery{Pattern: "unique filtered needle", Fixed: true}, f)
	if err != nil || page.Truncated || !page.Exact || page.Total != 0 {
		t.Fatalf("empty metadata cohort: %+v %v", page, err)
	}
}
