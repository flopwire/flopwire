package retrieval

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/retrieval/format"
)

func TestGrepContinuationSparseCost(t *testing.T) {
	s, counter := perfCorpus(t, 1, 4097)
	s.GrepContinuations = NewGrepContinuations(s.Pool)
	defer s.GrepContinuations.Close()
	ctx := context.Background()
	var owner, session string
	if err := s.Pool.QueryRow(ctx, `SELECT user_id::text,session_id FROM conversations LIMIT 1`).Scan(&owner, &session); err != nil {
		t.Fatal(err)
	}
	// A one-character pattern uses the scoped metadata cursor. Its single
	// match is deliberately after thousands of nonmatching candidates.
	if _, err := s.Pool.Exec(ctx, `UPDATE messages SET text=CASE WHEN ordinal=0 THEN 'z' ELSE 'ordinary' END`); err != nil {
		t.Fatal(err)
	}
	q := format.GrepQuery{Pattern: "z", Fixed: true, Continue: true, Authority: "test-device", Limit: 20}
	f := format.Filters{Owner: owner, Session: session}
	counter.Reset()
	start := time.Now()
	calls, checked, hits := 0, 0, 0
	for {
		page := walkPage(t, s, q, f)
		calls++
		checked += page.Scan.Checked
		hits += len(page.Hits)
		if page.Scan.Status == "complete" {
			break
		}
		if page.Scan.Status != "more" || page.Scan.NextCursor == "" || calls > 100 {
			t.Fatalf("scan stalled: %+v", page.Scan)
		}
		q.Cursor = page.Scan.NextCursor
	}
	t.Logf("calls=%d checked=%d hits=%d statements=%d wall=%s", calls, checked, hits, counter.Statements(), time.Since(start))
	if calls > 2 || counter.Statements() > 1000 {
		t.Fatalf("sparse scan regressed: calls=%d statements=%d", calls, counter.Statements())
	}
	if checked != 4097 || hits != 1 {
		t.Fatalf("lost candidates: checked=%d hits=%d", checked, hits)
	}
}

func TestGrepContinuationBatchByteBoundary(t *testing.T) {
	s, q, f := continuationCorpus(t, 5)
	q.Limit = 20
	// A sparse scoped scan isolates byte accounting from formatting and
	// reauthorizing multi-megabyte matching lines.
	q.Pattern = "z"
	if err := s.Pool.QueryRow(context.Background(), `SELECT session_id FROM conversations LIMIT 1`).Scan(&f.Session); err != nil {
		t.Fatal(err)
	}
	// Each candidate is exactly the existing body limit. Four fit in the
	// page byte budget; the fifth must survive for the next call.
	if _, err := s.Pool.Exec(context.Background(), `UPDATE messages SET text='step '||repeat('x',$1)`, grepContinuationBody-5); err != nil {
		t.Fatal(err)
	}
	// This test exercises byte limits, not wall time on race-instrumented
	// multi-megabyte payloads. Deadline behavior is tested separately.
	ctx, cancel := context.WithTimeout(context.Background(), MaxBudget)
	defer cancel()
	first, err := s.Grep(ctx, q, f)
	if err != nil {
		t.Fatal(err)
	}
	if first.Scan.Checked != 4 || len(first.Hits) != 0 || first.Scan.Status != "more" {
		t.Fatalf("first byte page: %+v", first.Scan)
	}
	q.Cursor = first.Scan.NextCursor
	second, err := s.Grep(ctx, q, f)
	if err != nil {
		t.Fatal(err)
	}
	if second.Scan.Checked != 1 || len(second.Hits) != 0 || second.Scan.Status != "complete" {
		t.Fatalf("second byte page: %+v", second.Scan)
	}
}

func TestGrepContinuationBatchDefersOversizedCandidate(t *testing.T) {
	s, q, f := continuationCorpus(t, 2)
	// Newest is valid and fills limit=1. The later oversized candidate must
	// not poison that page simply because batch preflight saw its size.
	if _, err := s.Pool.Exec(context.Background(), `UPDATE messages SET text='step '||repeat('x',$1) WHERE ordinal=0`, grepContinuationBody); err != nil {
		t.Fatal(err)
	}
	first := walkPage(t, s, q, f)
	if len(first.Hits) != 1 || first.Hits[0].Ordinal != 1 || first.Scan.Status != "more" {
		t.Fatalf("early valid page: %+v", first.Scan)
	}
	q.Cursor = first.Scan.NextCursor
	second := walkPage(t, s, q, f)
	if second.Scan.Status != "restart_required" || !strings.Contains(second.Reason, "body limit") {
		t.Fatalf("late oversized page: %+v", second)
	}
}

func TestGrepContinuationBatchHitBoundaries(t *testing.T) {
	for _, limit := range []int{1, 7, 20} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			s, q, f := continuationCorpus(t, 67)
			q.Limit = limit
			next := int64(66)
			for calls := 0; calls < 70; calls++ {
				page := walkPage(t, s, q, f)
				for _, h := range page.Hits {
					if h.Ordinal != next {
						t.Fatalf("candidate skipped or reordered: ordinal=%d want=%d", h.Ordinal, next)
					}
					next--
				}
				if page.Scan.Status == "complete" {
					if next != -1 {
						t.Fatalf("incomplete coverage: next=%d", next)
					}
					return
				}
				if page.Scan.Status != "more" {
					t.Fatalf("scan: %+v", page.Scan)
				}
				q.Cursor = page.Scan.NextCursor
			}
			t.Fatal("scan did not finish")
		})
	}
}

func TestGrepContinuationBatchDeadline(t *testing.T) {
	for _, stage := range []string{"SELECT octet_length(m.text)", "SELECT m.text,c.session_id,m.ordinal"} {
		t.Run(stage, func(t *testing.T) {
			original, q, f := continuationCorpus(t, 3)
			plan := &stallPlan{onWrite: []byte(stage)}
			s := (&findFixture{t: t, s: original}).stallStore(plan)
			s.GrepContinuations = NewGrepContinuations(s.Pool)
			defer s.GrepContinuations.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			page, err := s.Grep(ctx, q, f)
			plan.wasCut(t)
			if err != nil || page.Scan.Status != "restart_required" || len(page.Hits) != 0 {
				t.Fatalf("partial batch escaped: page=%+v err=%v", page, err)
			}
			for _, walk := range s.GrepContinuations.searches {
				if walk.tx != nil {
					t.Fatal("timed-out batch retained its snapshot")
				}
			}
		})
	}
}
