package workmetrics

import (
	"sync"
	"testing"
)

func BenchmarkOperationBeginEnd(b *testing.B) {
	var op Operation
	b.ReportAllocs()
	for b.Loop() {
		done := op.Begin()
		done.End()
	}
}

func TestOperationConcurrentSnapshots(t *testing.T) {
	var op Operation
	done := op.Begin()
	if s := op.Snapshot("fixed"); s.Calls != 1 || s.Completed != 0 || s.InFlight != 1 {
		t.Fatalf("running: %+v", s)
	}
	done.End()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				finish := op.Begin()
				_ = op.Snapshot("fixed")
				finish.End()
			}
		})
	}
	wg.Wait()
	s := op.Snapshot("fixed")
	if s.Calls != 801 || s.Completed != 801 || s.InFlight != 0 || s.TotalWallNS < s.MaxWallNS || s.MaxWallNS == 0 {
		t.Fatalf("finished: %+v", s)
	}
	if next := op.Snapshot("fixed"); next != s {
		t.Fatalf("snapshot changed counters: %+v -> %+v", s, next)
	}
}
