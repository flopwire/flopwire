package retrieval

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/retrieval/format"
)

// BenchmarkFiniteLiteralAlternativesAdapter measures production first pages on
// synthetic scattered-trigram bodies. Seven days admits 30,000 metadata rows;
// two days admits 10,000. Setup and continuation cleanup are outside the timer.
// Run explicitly with -run '^$' -bench BenchmarkFiniteLiteralAlternativesAdapter
// -benchtime=3x. This adversarial corpus is not a capacity qualification.
func BenchmarkFiniteLiteralAlternativesAdapter(b *testing.B) {
	s, _ := perfCorpus(b, 30, 1000)
	ctx := context.Background()
	for _, sql := range []string{
		`UPDATE messages m SET text=(SELECT string_agg(md5(m.id::text||j),' ') FROM generate_series(1,96) j)
 ||' spo pot otl tli lig igh ght mdu dut uti til mds sto tor ore res '
 ||CASE WHEN ordinal%100=0 THEN ' spotlight ' WHEN ordinal%100=1 THEN ' mdutil ' WHEN ordinal%100=2 THEN ' mdſ_ſtoreſ ' ELSE '' END,
 ts=CASE WHEN conversation_id IN (SELECT md5('c'||i)::uuid FROM generate_series(1,20) i) THEN '2026-10-12'::timestamptz ELSE '2026-10-16'::timestamptz END+ordinal*interval '1 millisecond'`,
		`UPDATE conversation_activity a SET last_activity_at=x.newest FROM (SELECT conversation_id,max(ts) newest FROM messages GROUP BY 1) x WHERE x.conversation_id=a.conversation_id`,
		`VACUUM ANALYZE`,
	} {
		if _, err := s.Pool.Exec(ctx, sql); err != nil {
			b.Fatal(err)
		}
	}
	var owner string
	if err := s.Pool.QueryRow(ctx, `SELECT user_id::text FROM conversations LIMIT 1`).Scan(&owner); err != nil {
		b.Fatal(err)
	}
	for _, pattern := range []string{`spotlight|mdutil|mds_stores`, `mdutil`, `missingneedle`, `spotlight|absentneedle`} {
		for _, days := range []int{7, 2} {
			for _, continuation := range []bool{false, true} {
				b.Run(fmt.Sprintf("%s/%dd/continue=%v", pattern, days, continuation), func(b *testing.B) {
					q := format.GrepQuery{Pattern: pattern, Limit: 35, MaxPerSession: 7}
					if continuation {
						q.Continue, q.Authority, q.Limit, q.MaxPerSession = true, "benchmark-finite-or", 20, 0
					}
					f := format.Filters{Owner: owner, Since: time.Date(2026, 10, 17-days, 0, 0, 0, 0, time.UTC)}
					var signature string
					b.ResetTimer()
					for n := 0; n < b.N; n++ {
						b.StopTimer()
						if continuation {
							s.GrepContinuations = NewGrepContinuations(s.Pool)
						}
						b.StartTimer()
						p, err := s.Grep(ctx, q, f)
						b.StopTimer()
						if continuation {
							s.GrepContinuations.Close()
						}
						if err != nil {
							b.Fatal(err)
						}
						if p.Truncated || (p.Scan != nil && p.Scan.Status != "more" && p.Scan.Status != "complete") {
							b.Fatalf("incomplete first page: %s %+v", p.Reason, p.Scan)
						}
						var ids strings.Builder
						for _, h := range p.Hits {
							ids.WriteString(h.MessageID)
							ids.WriteByte('\n')
						}
						sig := fmt.Sprintf("%x", sha256.Sum256([]byte(ids.String())))
						if n > 0 && sig != signature {
							b.Fatal("unstable first-page IDs")
						}
						signature = sig
						b.StartTimer()
					}
					b.StopTimer()
					b.Logf("first-page IDs sha256=%s", signature)
				})
			}
		}
	}
}
