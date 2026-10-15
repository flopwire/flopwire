package retrieval

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"testing"
	"time"
)

// BenchmarkFoldedLiteralAdapter uses production migrations, filters, candidate
// selection, hydration and Go verification. Run with -benchtime=3x; setup is
// excluded. Bodies are synthetic and deliberately contain scattered trigrams.
func BenchmarkFoldedLiteralAdapter(b *testing.B) {
	s, _ := perfCorpus(b, 30, 1000)
	ctx := context.Background()
	_, err := s.Pool.Exec(ctx, `UPDATE messages SET kind='tool_call',text=
 (SELECT string_agg(md5(messages.id::text||j),' ') FROM generate_series(1,24) j)
 ||' cass unrelated search marker '
 ||CASE WHEN ordinal%100=0 THEN ' cass search ancientneedle ' ELSE '' END
 ||CASE WHEN ordinal%500=0 THEN repeat(' cass unrelated search marker ',4000) ELSE '' END`)
	if err != nil {
		b.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, `VACUUM ANALYZE messages`); err != nil {
		b.Fatal(err)
	}
	s.GrepContinuations = NewGrepContinuations(s.Pool)
	defer s.GrepContinuations.Close()
	var owner string
	if err := s.Pool.QueryRow(ctx, `SELECT user_id::text FROM conversations LIMIT 1`).Scan(&owner); err != nil {
		b.Fatal(err)
	}
	for _, pattern := range []string{"cass search", "search", "missingneedle", "ancientneedle", "sssss", "(?i:ask)(?-i:SKY)"} {
		for _, continued := range []bool{false, true} {
			b.Run(fmt.Sprintf("%s/continue=%v", pattern, continued), func(b *testing.B) {
				q := format.GrepQuery{Pattern: pattern, Limit: 20, Continue: continued, Authority: "benchmark"}
				f := format.Filters{Owner: owner, Kinds: []string{"tool_call"}, Until: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}
				var signature string
				b.ResetTimer()
				for n := 0; n < b.N; n++ {
					p, err := s.Grep(ctx, q, f)
					if err != nil {
						b.Fatal(err)
					}
					if p.Truncated {
						b.Fatal(p.Reason)
					}
					ids := ""
					for _, h := range p.Hits {
						ids += h.MessageID + "\n"
					}
					sig := fmt.Sprintf("%x", sha256.Sum256([]byte(ids)))
					if n > 0 && sig != signature {
						b.Fatal("unstable IDs")
					}
					signature = sig
				}
				b.StopTimer()
				b.Logf("first-page IDs sha256=%s", signature)
			})
		}
	}
}
