package retrieval

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/grep"
)

func TestFoldedLiteralPostgresAdapter(t *testing.T) {
	s, _ := perfCorpus(t, 1, len(literalEquivalenceTexts))
	ctx := context.Background()
	for i, body := range literalEquivalenceTexts {
		if _, err := s.Pool.Exec(ctx, `UPDATE messages SET text=$1,kind='tool_call' WHERE ordinal=$2`, fmt.Sprintf("row%d %s", i, body), i); err != nil {
			t.Fatal(err)
		}
	}
	s.GrepContinuations = NewGrepContinuations(s.Pool)
	defer s.GrepContinuations.Close()
	var owner string
	if err := s.Pool.QueryRow(ctx, `SELECT user_id::text FROM conversations LIMIT 1`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	f := format.Filters{Owner: owner, Kinds: []string{"tool_call"}, Until: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}
	for _, gq := range append(literalEquivalenceQueries, format.GrepQuery{Pattern: "missingneedle"}, format.GrepQuery{Pattern: "ssssss"}) {
		t.Run(fmt.Sprintf("%s/%v", gq.Pattern, gq.CaseSensitive), func(t *testing.T) {
			plan, err := grep.Compile(gq)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := s.Pool.Query(ctx, `SELECT id::text,text FROM messages ORDER BY ts DESC NULLS LAST,id`)
			if err != nil {
				t.Fatal(err)
			}
			var want []string
			for rows.Next() {
				var id, body string
				if err := rows.Scan(&id, &body); err != nil {
					t.Fatal(err)
				}
				if plan.Re.MatchString(body) {
					want = append(want, id)
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			rows.Close()
			for _, continuation := range []bool{false, true} {
				q := gq
				q.Limit = 500
				if continuation {
					q.Continue = true
					q.Authority = "test-fold"
					q.Limit = 3
				}
				var got []string
				for pages := 0; ; pages++ {
					if pages > 100 {
						t.Fatal("continuation failed to finish")
					}
					p, err := s.Grep(ctx, q, f)
					if err != nil {
						t.Fatal(err)
					}
					if p.Truncated {
						t.Fatalf("incomplete page: %+v", p)
					}
					for _, h := range p.Hits {
						got = append(got, h.MessageID)
					}
					if !continuation || p.Scan.Status == "complete" {
						break
					}
					if p.Scan.Status != "more" || p.Scan.NextCursor == "" {
						t.Fatalf("unexpected scan: %+v", p.Scan)
					}
					q.Cursor = p.Scan.NextCursor
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("continuation=%v got=%v want=%v", continuation, got, want)
				}
			}
		})
	}
}

func TestFoldedLiteralEligibility(t *testing.T) {
	for _, pattern := range []string{"cass search", "(ask)", "sssss", "ask|sky", "spotlight|mdutil|mds_stores", "1f877ad98|repo/additive-migrations-2049", "(foo|bar)baz"} {
		p, _ := grep.Compile(format.GrepQuery{Pattern: pattern})
		q := &query{}
		if got := literalRegexpCond(q, p.Re); !strings.HasPrefix(got, "m.text ~ ") {
			t.Fatalf("%s: %s", pattern, got)
		}
	}
	for _, gq := range []format.GrepQuery{{Pattern: "ordinary"}, {Pattern: "ask", CaseSensitive: true}, {Pattern: "ask|sky", CaseSensitive: true}, {Pattern: "(?i:ask)|(?-i:SKY)", CaseSensitive: true}, {Pattern: "(?i:ask)(?-i:SKY)", CaseSensitive: true}, {Pattern: "ssssss"}} {
		p, _ := grep.Compile(gq)
		q := &query{}
		if got := literalRegexpCond(q, p.Re); strings.HasPrefix(got, "m.text ~ ") {
			t.Fatalf("unexpected specialization %+v: %s", gq, got)
		}
	}
}
