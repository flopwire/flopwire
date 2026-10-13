package retrieval

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/grep"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// BenchmarkGrepFirstPage compares the deployed wide candidate cursor with
// an ID-only cursor followed by ordered hydration. Each run is bounded,
// includes returning the first 256 complete rows, and uses synthetic text.
// Run explicitly with -run '^$' -bench BenchmarkGrepFirstPage -benchtime=3x.
func BenchmarkGrepFirstPage(b *testing.B) {
	s, _ := perfCorpus(b, 1200, 20)
	ctx := context.Background()
	_, err := s.Pool.Exec(ctx, `UPDATE messages m SET text=(SELECT string_agg(md5(m.id::text||g), '') FROM generate_series(1,96) g)||
	 CASE WHEN ordinal%5=0 THEN ' gh auth refresh read:project INSUFFICIENT_SCOPES example.test SSO SAML gh issue create issues/1234 Shared controls demo-mockup.workers.dev/authorize-flow project token' ELSE ' ordinary synthetic record' END,
	 text_len=3100`)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `VACUUM ANALYZE messages`); err != nil {
		b.Fatal(err)
	}
	for _, tc := range []struct{ name, pattern string }{
		{"finite_or", `read:project|gh auth refresh|INSUFFICIENT_SCOPES`},
		{"sso_overlap", `example\.test.*(saml|sso|okta|entra|google workspace|idp)|(saml|sso|okta|entra|idp).*example\.test`},
		{"issue_title_url", `gh issue create.*(1234|Shared controls)|issues/1234|Shared controls`},
		{"deployment_url", `demo-mockup[a-z0-9.-]*\.workers\.dev/[^ )"]*author[^ )"]*`},
		{"auth_overlap", `read:project|gh auth refresh|project.*token|token.*project`},
	} {
		b.Run(tc.name, func(b *testing.B) {
			plan, err := grep.Compile(format.GrepQuery{Pattern: tc.pattern, CaseSensitive: true})
			if err != nil {
				b.Fatal(err)
			}
			q := &query{}
			cond := literalRegexpCond(q, plan.Re)
			if cond == "" {
				cond = trigramCond(q, plan.Query)
			}
			q.where(cond)
			if err := hitFilters(q, format.Filters{}); err != nil {
				b.Fatal(err)
			}
			benchmarkGrepFirstPage(b, s, q)
		})
	}
}

// The broad fixture exceeds the candidate cap; no artificial body expansion
// is needed to measure the cost of sorting and hydrating 200,000 candidates.
func BenchmarkGrepFirstPageCap(b *testing.B) {
	s, _ := perfCorpus(b, 2200, 100)
	q := &query{}
	q.where("m.text ILIKE " + q.arg("%step%"))
	if err := hitFilters(q, format.Filters{}); err != nil {
		b.Fatal(err)
	}
	benchmarkGrepFirstPage(b, s, q)
}

func benchmarkGrepFirstPage(b *testing.B, s *Store, q *query) {
	ctx := context.Background()
	for _, mode := range []string{"wide", "ids"} {
		b.Run(mode, func(b *testing.B) {
			narrow := mode == "ids"
			sql := grepWideCandidates(q, false)
			if narrow {
				sql = grepCandidates(q, false)
			}
			var raw []byte
			planCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := s.Pool.QueryRow(planCtx, `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) `+sql, q.args...).Scan(&raw)
			cancel()
			if err != nil {
				b.Fatal(err)
			}
			var explain []map[string]any
			if err := json.Unmarshal(raw, &explain); err != nil {
				b.Fatal(err)
			}
			b.Logf("full candidate execution_ms=%.2f", explain[0]["Execution Time"])
			logGrepPlan(b, explain[0]["Plan"].(map[string]any))
			b.ResetTimer()
			var first, hydrate time.Duration
			for i := 0; i < b.N; i++ {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				opts := pgx.TxOptions{AccessMode: pgx.ReadOnly}
				if narrow {
					opts.IsoLevel = pgx.RepeatableRead
				}
				tx, err := s.Pool.BeginTx(ctx, opts)
				if err != nil {
					cancel()
					b.Fatal(err)
				}
				start := time.Now()
				cursor := "bench_candidates_" + mode
				_, err = tx.Exec(ctx, `DECLARE `+cursor+` NO SCROLL CURSOR FOR `+sql, q.args...)
				if err == nil {
					var rows pgx.Rows
					rows, err = tx.Query(ctx, `FETCH 256 FROM `+cursor)
					if err == nil && narrow {
						var ids []uuid.UUID
						ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
						first += time.Since(start)
						start = time.Now()
						if err == nil {
							rows, err = tx.Query(ctx, grepCandidateBodies, ids)
						}
					}
					if err == nil {
						n := 0
						for rows.Next() {
							var sha []byte
							var body string
							_, err = scanHit(rows, &sha, &body)
							if err != nil {
								break
							}
							n++
						}
						rows.Close()
						if err == nil {
							err = rows.Err()
						}
						if err == nil && n != 256 {
							err = fmt.Errorf("rows=%d", n)
						}
						if narrow {
							hydrate += time.Since(start)
						} else {
							first += time.Since(start)
						}
					}
				}
				_ = tx.Rollback(context.Background())
				cancel()
				if err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(first.Microseconds())/float64(b.N)/1000, "first-fetch-ms/op")
			b.ReportMetric(float64(hydrate.Microseconds())/float64(b.N)/1000, "hydrate-ms/op")
			b.ReportMetric(float64((first+hydrate).Microseconds())/float64(b.N)/1000, "complete-first-page-ms/op")
		})
	}
}

func logGrepPlan(b *testing.B, n map[string]any) {
	if n["Node Type"] == "Sort" || n["Node Type"] == "Bitmap Heap Scan" {
		stats := map[string]any{}
		for _, k := range []string{"Node Type", "Relation Name", "Plan Width", "Actual Rows", "Actual Loops", "Sort Method", "Sort Space Used", "Sort Space Type", "Rows Removed by Index Recheck", "Rows Removed by Filter", "Shared Hit Blocks", "Shared Read Blocks", "Temp Read Blocks", "Temp Written Blocks"} {
			if v, ok := n[k]; ok {
				stats[k] = v
			}
		}
		data, _ := json.Marshal(stats)
		b.Logf("plan node %s", data)
	}
	if children, ok := n["Plans"].([]any); ok {
		for _, child := range children {
			logGrepPlan(b, child.(map[string]any))
		}
	}
}

// Keep the deployed wide-row shape here as the measurement baseline.
func grepWideCandidates(q *query, oldest bool) string {
	order := " DESC"
	if oldest {
		order = ""
	}
	return `SELECT ` + hitCols + `,m.content_sha,m.text FROM ` + from + ` WHERE ` + q.sql() + ` ORDER BY m.ts` + order + ` NULLS LAST,m.id LIMIT 200000`
}
