package retrieval

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
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

type filteredGrepTrace struct {
	mu                 sync.Mutex
	metadata, filtered bool
}

func (tr *filteredGrepTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if strings.Contains(data.SQL, "LIMIT 20001") {
		tr.metadata = true
	}
	if strings.Contains(data.SQL, "DECLARE grep_candidates") && strings.Contains(data.SQL, "JOIN LATERAL") {
		tr.filtered = true
	}
	return ctx
}
func (*filteredGrepTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestGrepFilteredRarePredicatesKeepGIN(t *testing.T) {
	s, _ := perfCorpus(t, 20, 20)
	ctx := context.Background()
	if _, err := s.Pool.Exec(ctx, `UPDATE messages SET text='zebracorn rarealpha' WHERE conversation_id=md5('c1')::uuid AND ordinal=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `ANALYZE messages`); err != nil {
		t.Fatal(err)
	}
	for _, pattern := range []string{"zebracorn", "rarealpha|rarebeta"} {
		tr := &filteredGrepTrace{}
		cfg := s.Pool.Config()
		cfg.ConnConfig.Tracer = tr
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		store := &Store{Pool: pool}
		f := format.Filters{Agent: "claude", Kinds: []string{"assistant"}, Since: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Sort: format.SortOldest}
		page, err := store.Grep(ctx, format.GrepQuery{Pattern: pattern}, f)
		pool.Close()
		if err != nil || page.Truncated || page.Total != 1 {
			t.Fatalf("rare old match: %+v %v", page, err)
		}
		if tr.metadata || tr.filtered {
			t.Fatal("rare predicate used metadata probes")
		}
	}
}

func TestGrepMetadataPlanGate(t *testing.T) {
	text := grepCandidatePlan{Node: "Bitmap Index Scan", Index: "messages_text_trgm_idx", Rows: 704}
	heap := grepCandidatePlan{Node: "Bitmap Heap Scan", Relation: "messages", Filter: "metadata predicate", Rows: 1, Plans: []grepCandidatePlan{text}}
	for _, tc := range []struct {
		name string
		plan grepCandidatePlan
		want bool
	}{
		{"global GIN postfilter", heap, true},
		{"tool-call GIN postfilter", grepCandidatePlan{Node: "Bitmap Heap Scan", Relation: "messages", Filter: "metadata", Plans: []grepCandidatePlan{{Node: "Bitmap Index Scan", Index: "messages_tool_call_trgm_idx", Rows: 704}}}, true},
		{"OR full and tool-call GIN", grepCandidatePlan{Node: "Bitmap Heap Scan", Relation: "messages", Filter: "metadata", Plans: []grepCandidatePlan{{Node: "BitmapOr", Rows: 704, Plans: []grepCandidatePlan{text, {Node: "Bitmap Index Scan", Index: "messages_tool_call_trgm_idx", Rows: 704}}}}}, true},
		{"rare tool-call GIN", grepCandidatePlan{Node: "Bitmap Heap Scan", Relation: "messages", Filter: "metadata", Plans: []grepCandidatePlan{{Node: "Bitmap Index Scan", Index: "messages_tool_call_trgm_idx", Rows: 64}}}, false},
		{"unknown index", grepCandidatePlan{Node: "Bitmap Heap Scan", Relation: "messages", Filter: "metadata", Plans: []grepCandidatePlan{{Node: "Bitmap Index Scan", Index: "unknown_trgm_idx", Rows: 704}}}, false},
		{"OR global GIN", grepCandidatePlan{Node: "Bitmap Heap Scan", Relation: "messages", Filter: "metadata", Plans: []grepCandidatePlan{{Node: "BitmapOr", Rows: 704, Plans: []grepCandidatePlan{text, text}}}}, true},
		{"OR estimate overrides children", grepCandidatePlan{Node: "Bitmap Heap Scan", Relation: "messages", Filter: "metadata", Plans: []grepCandidatePlan{{Node: "BitmapOr", Rows: 64, Plans: []grepCandidatePlan{text, text}}}}, false},
		{"rare text", grepCandidatePlan{Node: "Bitmap Heap Scan", Relation: "messages", Filter: "metadata", Plans: []grepCandidatePlan{{Node: "Bitmap Index Scan", Index: "messages_text_trgm_idx", Rows: 64}}}, false},
		{"metadata BitmapAnd", grepCandidatePlan{Node: "Bitmap Heap Scan", Relation: "messages", Filter: "metadata", Plans: []grepCandidatePlan{{Node: "BitmapAnd", Rows: 704, Plans: []grepCandidatePlan{text, {Node: "Bitmap Index Scan", Index: "messages_default_filter_idx", Rows: 12}}}}}, false},
		{"metadata index scan", grepCandidatePlan{Node: "Index Scan", Relation: "messages", Index: "messages_default_filter_idx", Rows: 704}, false},
		{"metadata sequential scan", grepCandidatePlan{Node: "Seq Scan", Relation: "messages", Filter: "metadata", Rows: 704}, false},
		{"no postfilter", grepCandidatePlan{Node: "Bitmap Heap Scan", Relation: "messages", Plans: []grepCandidatePlan{text}}, false},
		{"unknown bitmap", grepCandidatePlan{Node: "Bitmap Heap Scan", Relation: "messages", Filter: "metadata", Plans: []grepCandidatePlan{{Node: "future bitmap", Rows: 704}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := grepNeedsMetadataProbes(grepCandidatePlan{Node: "Limit", Plans: []grepCandidatePlan{tc.plan}}); got != tc.want {
				t.Fatalf("gate=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestGrepFilteredAdaptivePathUsesPrimaryKeyProbes(t *testing.T) {
	s, _ := perfCorpus(t, 1000, 100)
	ctx := context.Background()
	for _, sql := range []string{
		// Sample the full synthetic text distribution so gate activation does
		// not depend on which rare marker rows ANALYZE happened to sample.
		`ALTER TABLE messages ALTER COLUMN text SET STATISTICS 1000`,
		`UPDATE messages SET kind=CASE WHEN ordinal=0 THEN 'assistant' ELSE 'tool_result' END`,
		`UPDATE messages SET text='literalmarker '||id::text WHERE ordinal=1 OR (ordinal=0 AND conversation_id IN (SELECT md5('c'||i)::uuid FROM generate_series(1,10) i))`,
		`VACUUM ANALYZE`,
	} {
		if _, err := s.Pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	tr := &filteredGrepTrace{}
	cfg := s.Pool.Config()
	cfg.ConnConfig.Tracer = tr
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	store := &Store{Pool: pool}
	page, err := store.Grep(ctx, format.GrepQuery{Pattern: "literalmarker", Limit: 500}, format.Filters{Agent: "claude", Kinds: []string{"assistant"}})
	if err != nil || page.Truncated || !page.Exact || page.Total != 10 {
		t.Fatalf("adaptive result: %+v %v", page, err)
	}
	if !tr.metadata || !tr.filtered {
		t.Fatalf("production-shaped query did not use adaptive path: metadata=%v filtered=%v", tr.metadata, tr.filtered)
	}
	tr.metadata, tr.filtered = false, false
	page, err = store.Grep(ctx, format.GrepQuery{Pattern: "literalmarker", Limit: 500}, format.Filters{Agent: "claude", Kinds: []string{"missing"}})
	if err != nil || page.Truncated || !page.Exact || page.Total != 0 || !tr.metadata || tr.filtered {
		t.Fatalf("empty eligible adaptive cohort: %+v %v metadata=%v filtered=%v", page, err, tr.metadata, tr.filtered)
	}
	// At 20,000 metadata rows the entire set is probed. One extra switches
	// back to the original GIN cursor, preserving every text-qualified hit.
	if _, err := s.Pool.Exec(ctx, `UPDATE messages SET kind=CASE WHEN ordinal<20 THEN 'assistant' ELSE 'tool_result' END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `ANALYZE messages`); err != nil {
		t.Fatal(err)
	}
	for _, overflow := range []bool{false, true} {
		if overflow {
			if _, err := s.Pool.Exec(ctx, `UPDATE messages SET kind='assistant' WHERE conversation_id=md5('c1')::uuid AND ordinal=20`); err != nil {
				t.Fatal(err)
			}
		}
		tr.metadata, tr.filtered = false, false
		page, err = store.Grep(ctx, format.GrepQuery{Pattern: "literalmarker", Mode: format.ModeCount, Limit: 500}, format.Filters{Agent: "claude", Kinds: []string{"assistant"}})
		if err != nil || page.Truncated || !page.Exact || page.Total != 1010 || !tr.metadata || tr.filtered == overflow {
			t.Fatalf("adaptive boundary overflow=%v: total=%d err=%v metadata=%v filtered=%v", overflow, page.Total, err, tr.metadata, tr.filtered)
		}
	}
	// Timeout attribution must include the actual metadata-selection stage.
	stall := &stallPlan{onWrite: []byte("LIMIT 20001")}
	stalled := (&findFixture{t: t, s: s}).stallStore(stall)
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	page, err = stalled.Grep(timeoutCtx, format.GrepQuery{Pattern: "literalmarker"}, format.Filters{Agent: "claude", Kinds: []string{"assistant"}})
	stall.wasCut(t)
	if err != nil || !page.Truncated || !strings.Contains(page.Reason, "resolving filtered messages") || !strings.Contains(page.Reason, "checked 0 candidates") {
		t.Fatalf("metadata timeout stage: %+v %v", page, err)
	}
}

func TestGrepPlanningTimeoutFallsBackInSameSnapshot(t *testing.T) {
	f := newFindFixture(t)
	f.add("assistant", "needle original")
	ctx := context.Background()
	// IMMUTABLE makes this constant expression execute during planning. The
	// fixture creates a deterministic planning stall without planner settings.
	if _, err := f.s.Pool.Exec(ctx, `CREATE FUNCTION grep_slow_plan() RETURNS boolean LANGUAGE plpgsql IMMUTABLE AS $$ BEGIN PERFORM pg_sleep(0.2); RETURN true; END $$`); err != nil {
		t.Fatal(err)
	}
	tx, err := f.s.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM messages`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	q := &query{}
	q.where("m.text ILIKE " + q.arg("%needle%"))
	q.where("grep_slow_plan()")
	if err := hitFilters(q, format.Filters{}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	use, err := grepPlanNeedsMetadata(ctx, tx, q, false, time.Now().Add(time.Second))
	if err != nil || use {
		t.Fatalf("planning timeout fallback: use=%v err=%v", use, err)
	}
	if time.Since(start) > 250*time.Millisecond {
		t.Fatal("planning subbudget was not bounded")
	}
	if _, err := f.s.Pool.Exec(ctx, `UPDATE messages SET text='redacted after snapshot'`); err != nil {
		t.Fatal(err)
	}
	ids := filteredCandidateIDs(t, tx, grepCandidates(q, false), q.args...)
	if len(ids) != 1 {
		t.Fatal("planning fallback lost original snapshot")
	}
	// The original query includes another 200ms plan; success also proves the
	// short planning statement_timeout was restored to the remaining budget.
}

func TestGrepPlanningDoesNotMaskErrorsOrCallerDeadline(t *testing.T) {
	f := newFindFixture(t)
	f.add("assistant", "needle original")
	ctx := context.Background()
	if _, err := f.s.Pool.Exec(ctx, `CREATE FUNCTION grep_slow_plan() RETURNS boolean LANGUAGE plpgsql IMMUTABLE AS $$ BEGIN PERFORM pg_sleep(0.2); RETURN true; END $$`); err != nil {
		t.Fatal(err)
	}
	for _, callerDeadline := range []bool{false, true} {
		tx, err := f.s.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
		if err != nil {
			t.Fatal(err)
		}
		q := &query{}
		q.where("m.text ILIKE " + q.arg("%needle%"))
		queryCtx := ctx
		cancel := func() {}
		if callerDeadline {
			q.where("grep_slow_plan()")
			queryCtx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
		} else {
			q.where("grep_nonexistent_function()")
		}
		use, err := grepPlanNeedsMetadata(queryCtx, tx, q, false, time.Now().Add(time.Second))
		cancel()
		tx.Rollback(ctx)
		if err == nil || use {
			t.Fatalf("masked error: use=%v err=%v", use, err)
		}
		if callerDeadline {
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("caller deadline changed: %v", err)
			}
		} else {
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "42883" {
				t.Fatalf("planning error changed: %v", err)
			}
		}
	}
}
