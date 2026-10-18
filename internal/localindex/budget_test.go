package localindex

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

func TestScanCallerCancellationDuringVisit(t *testing.T) {
	for _, detail := range []Detail{DetailFull, DetailColumn} {
		t.Run(string(detail), func(t *testing.T) {
			s := openTest(t, detail)
			src := source(t, s, transcript.AgentClaude, "/synthetic/cancellation.jsonl")
			var messages []*transcript.Message
			for i := range 4 {
				messages = append(messages, msg("cancel", "row-"+itoa(int64(i)), int64(i+1), transcript.KindUser, "ocelot evidence"))
			}
			apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: messages})
			for _, indexed := range []bool{true, false} {
				t.Run(map[bool]string{true: "indexed", false: "unindexed"}[indexed], func(t *testing.T) {
					for _, keepGoing := range []bool{true, false} {
						t.Run(map[bool]string{true: "continue", false: "stop"}[keepGoing], func(t *testing.T) {
							parent, cancel := context.WithCancel(ctx)
							defer cancel()
							q := &TrigramQuery{Op: TrigramAll}
							if indexed {
								q = &TrigramQuery{Op: TrigramAnd, Trigrams: []string{"oce", "cel", "elo", "lot"}}
							}
							visited := 0
							partial, err := s.Scan(parent, q, ScanOptions{}, func(*Row) bool { visited++; cancel(); return keepGoing })
							if !errors.Is(err, context.Canceled) {
								t.Fatalf("error=%v, want caller cancellation; partial=%+v", err, partial)
							}
							if visited != 1 || partial.Checked != 1 {
								t.Fatalf("visited=%d partial=%+v", visited, partial)
							}
							if partial.Truncated || partial.Reason != "" {
								t.Fatalf("caller cancellation became a budget result: %+v", partial)
							}
						})
					}
				})
			}
		})
	}
}

func TestDeadlineSettlement(t *testing.T) {
	unrelated := errors.New("storage failed")
	for _, caller := range []string{"active", "canceled", "expired"} {
		t.Run(caller, func(t *testing.T) {
			parent := ctx
			var cancel context.CancelFunc
			switch caller {
			case "canceled":
				parent, cancel = context.WithCancel(ctx)
				cancel()
			case "expired":
				parent, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
			}
			for _, incoming := range []error{nil, unrelated} {
				d := newDeadline(parent, Budget{Timeout: time.Hour})
				defer d.cancel()
				partial := Partial{}
				err := d.settle(&partial, incoming)
				want := incoming
				if want == nil {
					want = parent.Err()
				}
				if !errors.Is(err, want) {
					t.Fatalf("incoming=%v: error=%v, want %v", incoming, err, want)
				}
				if partial.Truncated {
					t.Fatalf("caller state became budget timeout: %+v", partial)
				}
			}
		})
	}
	// The internal budget still yields a partial result, with either no lower
	// level error or an interrupt/deadline error returned by SQLite.
	for _, incoming := range []error{nil, context.DeadlineExceeded, context.Canceled, unrelated} {
		d := newDeadline(ctx, Budget{Timeout: time.Nanosecond})
		defer d.cancel()
		<-d.ctx.Done()
		partial := Partial{}
		err := d.settle(&partial, incoming)
		if incoming == unrelated {
			if err != unrelated || partial.Truncated {
				t.Fatalf("unrelated error changed: %v %+v", err, partial)
			}
		} else if err != nil || !partial.Truncated || partial.Reason != ReasonTimeout {
			t.Fatalf("budget error=%v partial=%+v", err, partial)
		}
	}
}
