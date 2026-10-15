package retrieval

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/grep"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestToolCallIndexPreparedPlans(t *testing.T) {
	s, _ := perfCorpus(t, 8, 1000)
	ctx := context.Background()
	if _, err := s.Pool.Exec(ctx, `UPDATE messages SET text=repeat(md5(id::text),12)||' cass unrelated search'; UPDATE messages SET text='cass search exactneedle' WHERE conversation_id=md5('c1')::uuid AND ordinal IN (1,2)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `VACUUM ANALYZE messages`); err != nil {
		t.Fatal(err)
	}
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	plan, _ := grep.Compile(format.GrepQuery{Pattern: "cass search"})
	for _, kinds := range [][]string{{"tool_call"}, {"tool_call", "assistant"}, {"assistant"}} {
		q := &query{}
		q.where(literalRegexpCond(q, plan.Re))
		if err := hitFilters(q, format.Filters{Kinds: kinds}); err != nil {
			t.Fatal(err)
		}
		sql := grepCandidates(q, false)
		if _, err := conn.Exec(ctx, `DEALLOCATE ALL`); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, `PREPARE tool_call_candidates AS `+sql); err != nil {
			t.Fatal(err)
		}
		var quoted []string
		for _, kind := range kinds {
			quoted = append(quoted, "'"+kind+"'")
		}
		execute := `EXECUTE tool_call_candidates('[Cc][Aa][Ssſ][Ssſ] [Ssſ][Ee][Aa][Rr][Cc][Hh]',ARRAY[` + strings.Join(quoted, ",") + `])`
		for _, mode := range []string{"auto", "force_generic_plan"} {
			if _, err := conn.Exec(ctx, `SET plan_cache_mode=`+mode); err != nil {
				t.Fatal(err)
			}
			var raw []byte
			if err := conn.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+execute).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var parsed any
			if err := json.Unmarshal(raw, &parsed); err != nil {
				t.Fatal(err)
			}
			uses := strings.Contains(string(raw), `messages_tool_call_trgm_idx`)
			want := len(kinds) == 1 && kinds[0] == "tool_call"
			if uses != want {
				t.Fatalf("kinds=%v mode=%s partial index=%v plan=%s", kinds, mode, uses, raw)
			}
			rows, err := conn.Query(ctx, execute)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					t.Fatal(err)
				}
				got = append(got, id)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			rows.Close()
			expected := len(kinds)
			if len(got) != expected {
				t.Fatalf("kinds=%v got=%v", kinds, got)
			}
		}
	}
}

func TestToolCallAdapterScopesAndViews(t *testing.T) {
	s, _ := perfCorpus(t, 3, len(literalEquivalenceTexts))
	ctx := context.Background()
	for i, body := range literalEquivalenceTexts {
		if _, err := s.Pool.Exec(ctx, `UPDATE messages SET text=$1,kind=CASE WHEN conversation_id=md5('c3')::uuid THEN 'assistant' ELSE 'tool_call' END WHERE ordinal=$2`, fmt.Sprintf("row%d %s", i, body), i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE messages SET superseded=true WHERE ordinal%7=0; UPDATE messages SET on_active_path=false WHERE ordinal%5=0; UPDATE messages SET ts=NULL WHERE ordinal%11=0; UPDATE conversations SET hidden_at=now() WHERE id=md5('c2')::uuid`); err != nil {
		t.Fatal(err)
	}
	var owner, session string
	if err := s.Pool.QueryRow(ctx, `SELECT md5('u1')::uuid::text,md5('s1')`).Scan(&owner, &session); err != nil {
		t.Fatal(err)
	}
	s.GrepContinuations = NewGrepContinuations(s.Pool)
	defer s.GrepContinuations.Close()
	type row struct {
		id, body, session, repo, kind string
		superseded, inactive, hidden  bool
		ts                            *time.Time
	}
	rows, err := s.Pool.Query(ctx, `SELECT m.id::text,m.text,c.session_id,c.repo_root,m.kind,m.superseded,m.on_active_path IS FALSE,c.hidden_at IS NOT NULL,m.ts FROM messages m JOIN conversations c ON c.id=m.conversation_id ORDER BY m.ts DESC NULLS LAST,m.id`)
	if err != nil {
		t.Fatal(err)
	}
	var corpus []row
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.id, &x.body, &x.session, &x.repo, &x.kind, &x.superseded, &x.inactive, &x.hidden, &x.ts); err != nil {
			t.Fatal(err)
		}
		corpus = append(corpus, x)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	filters := []format.Filters{
		{Kinds: []string{"tool_call"}, Until: time.Date(2026, 9, 1, 0, 1, 0, 20_000_000, time.UTC)},
		{Kinds: []string{"tool_call"}},
		{Kinds: []string{"tool_call"}, IncludeSuperseded: true, IncludeBranches: true},
		{Kinds: []string{"tool_call"}, Session: session, Self: true, Owner: owner},
		{Kinds: []string{"tool_call"}, Repo: "/src/repo1"},
		{Kinds: []string{"tool_call"}, ExcludeSession: session},
		{Kinds: []string{"tool_call", "assistant"}, IncludeSuperseded: true, IncludeBranches: true},
	}
	queries := []format.GrepQuery{{Pattern: "cass search"}, {Pattern: "ask"}, {Pattern: "(?i:ask)|(?-i:SKY)", CaseSensitive: true}, {Pattern: `foo\%\_\\bar\.baz`, CaseSensitive: true}, {Pattern: "missingneedle"}, {Pattern: "sssss"}}
	for fi, f := range filters {
		f.Owner = owner
		for _, gq := range queries {
			t.Run(fmt.Sprintf("filter%d/%s", fi, gq.Pattern), func(t *testing.T) {
				plan, err := grep.Compile(gq)
				if err != nil {
					t.Fatal(err)
				}
				var want []string
				for _, x := range corpus {
					if (!f.Until.IsZero() && (x.ts == nil || !x.ts.Before(f.Until))) || x.hidden || !slices.Contains(f.Kinds, x.kind) || (!f.IncludeSuperseded && x.superseded) || (!f.IncludeBranches && x.inactive) || (f.Session != "" && x.session != f.Session) || (f.ExcludeSession != "" && x.session == f.ExcludeSession) || (f.Repo != "" && x.repo != f.Repo) {
						continue
					}
					if plan.Re.MatchString(x.body) {
						want = append(want, x.id)
					}
				}
				for _, continuation := range []bool{false, true} {
					q := gq
					q.Limit = 500
					if continuation {
						q.Continue = true
						q.Authority = "test-kind-index"
						q.Limit = 3
					}
					var got []string
					for page := 0; ; page++ {
						if page > 100 {
							t.Fatal("walk did not complete")
						}
						p, err := s.Grep(ctx, q, f)
						if err != nil || p.Truncated {
							t.Fatalf("page=%+v err=%v", p, err)
						}
						for _, h := range p.Hits {
							got = append(got, h.MessageID)
						}
						if !continuation || p.Scan.Status == "complete" {
							break
						}
						if p.Scan.Status != "more" || p.Scan.NextCursor == "" {
							t.Fatalf("scan=%+v", p.Scan)
						}
						q.Cursor = p.Scan.NextCursor
					}
					if !slices.Equal(got, want) {
						t.Fatalf("continuation=%v got=%v want=%v", continuation, got, want)
					}
				}
			})
		}
	}
}

func TestToolCallScopedMetadataPath(t *testing.T) {
	s, _ := perfCorpus(t, 200, 100)
	ctx := context.Background()
	for _, sql := range []string{
		`ALTER TABLE messages ALTER COLUMN text SET STATISTICS 1000`,
		`UPDATE messages SET kind='tool_call',ts=CASE WHEN ordinal=0 THEN '2020-01-01'::timestamptz ELSE '2026-10-01'::timestamptz END`,
		`UPDATE messages SET text='needlealpha '||id::text WHERE ordinal IN (1,2) OR (ordinal=0 AND conversation_id IN(SELECT md5('c'||i)::uuid FROM generate_series(1,10)i))`,
		// Exercise the new index classifier rather than allowing the full index
		// to hide its omission. The remaining normal planner can still choose
		// a sequential scan or a conversation index if those cost less.
		`DROP INDEX messages_text_trgm_idx`,
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
	adapter := &Store{Pool: pool}
	p, err := adapter.Grep(ctx, format.GrepQuery{Pattern: "needlealpha", Limit: 500}, format.Filters{Agent: "claude", Kinds: []string{"tool_call"}, Until: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil || p.Truncated || !p.Exact || p.Total != 10 {
		t.Fatalf("page=%+v err=%v", p, err)
	}
	if !tr.metadata || !tr.filtered {
		t.Fatalf("partial GIN bypassed scoped metadata selection: metadata=%v filtered=%v", tr.metadata, tr.filtered)
	}
	rows, err := pool.Query(ctx, `SELECT md5('m'||i||'/0')::uuid::text FROM generate_series(1,10)i ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var want, got []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		want = append(want, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for _, h := range p.Hits {
		got = append(got, h.MessageID)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
}
