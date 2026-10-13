package retrieval

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/grep"
	"github.com/jackc/pgx/v5"
)

// Large tool bodies carry the trigrams of the literal alternatives, but
// not the complete phrases. They must be TOAST-rechecked by the global GIN
// path even though the kind filter excludes them. Assistant bodies are small.
// This benchmark is explicit; it does not add its large fixture to unit tests.
func BenchmarkGrepFilteredCandidates(b *testing.B) {
	s, _ := perfCorpus(b, 400, 10)
	ctx := context.Background()
	if _, err := s.Pool.Exec(ctx, `UPDATE messages m SET kind=CASE WHEN ordinal%5=0 THEN 'assistant' ELSE 'tool_result' END,
 text=CASE WHEN ordinal%5=0 THEN CASE WHEN conversation_id=md5('c1')::uuid THEN 'zebracorn read:project gh auth refresh' ELSE 'ordinary assistant record' END
 ELSE (SELECT string_agg(md5(m.id::text||g),'') FROM generate_series(1,1024) g)||' read:unrelated project gh auth unrelated refresh INSUFFICIENT_unrelated SCOPES' END`); err != nil {
		b.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE conversation_activity a SET last_activity_at=x.newest FROM (SELECT conversation_id,max(ts) newest FROM messages GROUP BY 1) x WHERE x.conversation_id=a.conversation_id`); err != nil {
		b.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `VACUUM ANALYZE`); err != nil {
		b.Fatal(err)
	}
	f := format.Filters{Agent: "claude", Kinds: []string{"assistant"}, Since: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	for _, pattern := range []string{`read:project|gh auth refresh|INSUFFICIENT_SCOPES`, `zebracorn`} {
		plan, err := grep.Compile(format.GrepQuery{Pattern: pattern})
		if err != nil {
			b.Fatal(err)
		}
		q := &query{}
		q.where(literalRegexpCond(q, plan.Re))
		if err := hitFilters(q, f); err != nil {
			b.Fatal(err)
		}
		tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
		if err != nil {
			b.Fatal(err)
		}
		convs, complete, err := grepConversations(ctx, tx, f)
		if err != nil || !complete {
			b.Fatalf("conversations %v %v", complete, err)
		}
		q.where("m.conversation_id=ANY(" + q.arg(convs) + "::uuid[])")
		ids, complete, err := grepMetadataCandidates(ctx, tx, f, convs)
		if err != nil || !complete {
			b.Fatalf("metadata %v %v", complete, err)
		}
		tx.Rollback(ctx)
		original := grepCandidates(q, false)
		filtered := grepFilteredCandidates(q, ids, false)
		// Original SQL predates the extra eligible-ID argument.
		originalArgs := q.args[:len(q.args)-1]
		for _, mode := range []string{"gin", "metadata"} {
			b.Run(pattern+"/"+mode, func(b *testing.B) {
				sql, args := original, originalArgs
				if mode == "metadata" {
					sql, args = filtered, q.args
				}
				var raw []byte
				if err := s.Pool.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) `+sql, args...).Scan(&raw); err != nil {
					b.Fatal(err)
				}
				var explain []map[string]any
				if err := json.Unmarshal(raw, &explain); err != nil {
					b.Fatal(err)
				}
				b.Logf("candidate execution_ms=%.3f", explain[0]["Execution Time"])
				b.ResetTimer()
				for range b.N {
					tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
					if err != nil {
						b.Fatal(err)
					}
					start := time.Now()
					if mode == "metadata" {
						if _, complete, err := grepMetadataCandidates(ctx, tx, f, convs); err != nil || !complete {
							b.Fatalf("metadata %v %v", complete, err)
						}
					}
					rows, err := tx.Query(ctx, sql, args...)
					if err != nil {
						b.Fatal(err)
					}
					for rows.Next() {
						var id string
						if err := rows.Scan(&id); err != nil {
							b.Fatal(err)
						}
					}
					rows.Close()
					if err := rows.Err(); err != nil {
						b.Fatal(err)
					}
					b.ReportMetric(float64(time.Since(start).Microseconds())/1000, "candidate-ms")
					tx.Rollback(ctx)
				}
			})
		}
	}
}
