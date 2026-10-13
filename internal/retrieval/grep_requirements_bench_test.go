package retrieval

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/grep"
)

// Explicit opt-in paired benchmark. Both variants use the same synthetic
// 24,000-row, 3KB-body fixture, full-row cursor and 256-row first page.
// Run -run '^$' -bench BenchmarkGrepRequiredSpans -benchtime=3x.
func BenchmarkGrepRequiredSpans(b *testing.B) {
	s, _ := perfCorpus(b, 1200, 20)
	ctx := context.Background()
	_, err := s.Pool.Exec(ctx, `UPDATE messages m SET text=(SELECT string_agg(md5(m.id::text||g),'') FROM generate_series(1,96)g)||
	CASE WHEN ordinal%5=0 THEN ' gh auth refresh read:project INSUFFICIENT_SCOPES example.test SSO SAML gh issue create issues/1234 Shared controls demo-mockup.workers.dev/authorize-flow project token' ELSE ' ordinary synthetic record' END,text_len=3300`)
	if err != nil {
		b.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, `VACUUM ANALYZE messages`); err != nil {
		b.Fatal(err)
	}
	patterns := []string{`read:project|gh auth refresh|INSUFFICIENT_SCOPES`}
	patterns = append(patterns, requirementPatterns[len(requirementPatterns)-4:]...)
	for i, pattern := range patterns {
		for _, spans := range []bool{false, true} {
			b.Run(fmt.Sprintf("shape%d/spans=%v", i, spans), func(b *testing.B) {
				plan, err := grep.Compile(format.GrepQuery{Pattern: pattern, CaseSensitive: i == 0 || i == 2})
				if err != nil {
					b.Fatal(err)
				}
				q := &query{}
				cond := literalRegexpCond(q, plan.Re)
				if cond == "" && spans {
					cond = requiredRegexpCond(q, plan.Re)
				}
				if cond == "" {
					cond = trigramCond(q, plan.Query)
				}
				q.where(cond)
				if err := hitFilters(q, format.Filters{}); err != nil {
					b.Fatal(err)
				}
				b.Logf("candidate predicate parameters=%d", len(q.args))
				// Keep row materialization identical to the deployed baseline;
				// this benchmark isolates predicates from page hydration changes.
				sql := `SELECT ` + hitCols + `,m.content_sha,m.text FROM ` + from + ` WHERE ` + q.sql() + ` ORDER BY m.ts DESC NULLS LAST,m.id LIMIT ` + strconv.Itoa(maxCandidates)
				b.ResetTimer()
				var total time.Duration
				for j := 0; j < b.N; j++ {
					ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
					tx, err := s.Pool.Begin(ctx)
					if err != nil {
						cancel()
						b.Fatal(err)
					}
					cursor := fmt.Sprintf("requirements_%v", spans)
					start := time.Now()
					_, err = tx.Exec(ctx, `DECLARE `+cursor+` NO SCROLL CURSOR FOR `+sql, q.args...)
					if err == nil {
						rows, e := tx.Query(ctx, `FETCH 256 FROM `+cursor)
						err = e
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
								err = fmt.Errorf("firstpage rows=%d", n)
							}
						}
					}
					total += time.Since(start)
					_ = tx.Rollback(context.Background())
					cancel()
					if err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(total.Microseconds())/float64(b.N)/1000, "complete-first-page-ms/op")
			})
		}
	}
}
