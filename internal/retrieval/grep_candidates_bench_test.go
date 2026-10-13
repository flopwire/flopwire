package retrieval

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/retrieval/format"
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
	 CASE WHEN ordinal%5=0 THEN ' gh auth refresh' ELSE ' ordinary synthetic record' END,
	 text_len=3100; VACUUM ANALYZE messages`)
	if err != nil {
		b.Fatal(err)
	}
	q := &query{}
	q.where("m.text ILIKE " + q.arg("%gh auth refresh%"))
	if err := hitFilters(q, format.Filters{}); err != nil {
		b.Fatal(err)
	}
	for _, narrow := range []bool{false, true} {
		b.Run(fmt.Sprintf("ids=%v", narrow), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				tx, err := s.Pool.Begin(ctx)
				if err != nil {
					cancel()
					b.Fatal(err)
				}
				sql := grepCandidates(q, false)
				if narrow {
					sql = `SELECT m.id::text FROM ` + from + ` WHERE ` + q.sql() + ` ORDER BY m.ts DESC NULLS LAST,m.id LIMIT 200000`
				}
				_, err = tx.Exec(ctx, `DECLARE bench_candidates NO SCROLL CURSOR FOR `+sql, q.args...)
				if err == nil {
					var rows pgx.Rows
					rows, err = tx.Query(ctx, `FETCH 256 FROM bench_candidates`)
					if err == nil && narrow {
						var ids []string
						ids, err = pgx.CollectRows(rows, pgx.RowTo[string])
						if err == nil {
							rows, err = tx.Query(ctx, `SELECT `+hitCols+`,m.content_sha,m.text FROM unnest($1::uuid[]) WITH ORDINALITY page(id,n) JOIN `+from+` ON page.id=m.id ORDER BY page.n`, ids)
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
					}
				}
				_ = tx.Rollback(context.Background())
				cancel()
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
