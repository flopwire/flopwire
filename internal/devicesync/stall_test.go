package devicesync

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

func snapshotStalls(owner *stallOwner) map[stallKey]int {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	out := make(map[stallKey]int, len(owner.counts))
	for key, count := range owner.counts {
		out[key] = count
	}
	return out
}

func TestStallOwnerConcurrentRecordsAndRetry(t *testing.T) {
	var owner stallOwner
	key := stallKey{12, 3}
	start := make(chan struct{})
	ready := make(chan struct{}, 2)
	counts := make(chan int, 2)
	done := make(chan struct{}, 2)
	var startOnce sync.Once
	release := func() { startOnce.Do(func() { close(start) }) }
	t.Cleanup(func() {
		release()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		for joined := 0; joined < 2; joined++ {
			select {
			case <-done:
			case <-timer.C:
				t.Errorf("stall record cleanup joined %d of 2 goroutines before deadline", joined)
				return
			}
		}
	})
	for range 2 {
		go func() {
			defer func() { done <- struct{}{} }()
			ready <- struct{}{}
			<-start
			counts <- owner.record(key)
		}()
	}
	for range 2 {
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ready:
			timer.Stop()
		case <-timer.C:
			t.Fatal("stall record goroutine did not reach start barrier")
		}
	}
	release()
	got := make([]int, 0, 2)
	for range 2 {
		timer := time.NewTimer(5 * time.Second)
		select {
		case count := <-counts:
			timer.Stop()
			got = append(got, count)
		case <-timer.C:
			t.Fatal("stall record did not complete before deadline")
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("concurrent cumulative counts: %v", got)
	}
	if count := owner.record(key); count != 3 {
		t.Fatalf("third stall=%d", count)
	}
	if counts := snapshotStalls(&owner); len(counts) != 0 {
		t.Fatalf("third stall left a poisoned bucket: %v", counts)
	}
	if count := owner.record(key); count != 1 {
		t.Fatalf("retry count=%d", count)
	}
}

func TestStallOwnerExactSourceAndGenerationCleanup(t *testing.T) {
	var owner stallOwner
	old := stallKey{12, 3}
	next := stallKey{12, 4}
	other := stallKey{13, 3}
	owner.record(old)
	owner.record(old)
	owner.record(next)
	owner.record(other)
	owner.forget(old)
	counts := snapshotStalls(&owner)
	if len(counts) != 2 || counts[next] != 1 || counts[other] != 1 {
		t.Fatalf("old-generation cleanup changed independent keys: %v", counts)
	}
	owner.forget(old) // absent cleanup is harmless
	if count := owner.record(next); count != 2 {
		t.Fatalf("replacement generation lost its count: %d", count)
	}
}

func TestPublicFullDrainIgnoresSharedStallsAndCleansCompletion(t *testing.T) {
	e := newEnv(t, Config{SealAfter: -1}, 1<<20)
	sp := e.spec("public-local-counter.jsonl", transcript.StorageJSONLAppend)
	data := jsonlLines(193, 20, 100)
	appendFile(t, sp.Path, data)
	e.srv.SetDown(true)
	if err := e.sy.Sync(t.Context(), sp); err == nil {
		t.Fatal("expected capture to remain pending during outage")
	}
	src, err := e.store.source(t.Context(), sp.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	key := stallKey{src.ID, src.Gen}
	e.sy.stalls.record(key)
	e.sy.stalls.record(key)
	e.srv.SetDown(false)
	tr := &stalledTurnTransport{Transport: e.client, progressOn: 2}
	tr.notify = func() {
		if counts := snapshotStalls(&e.sy.stalls); counts[key] != 2 {
			t.Errorf("full-drain response changed shared scheduler count: %v", counts)
		}
	}
	e.sy.tr = tr
	if err := e.sy.Sync(t.Context(), sp); err != nil {
		t.Fatalf("public full drain consumed prior scheduler stalls: %v", err)
	}
	if tr.calls != 2 {
		t.Fatalf("full-drain response count=%d, want one stall then completion", tr.calls)
	}
	if counts := snapshotStalls(&e.sy.stalls); len(counts) != 0 {
		t.Fatalf("completion retained shared counter: %v", counts)
	}
	e.requireServerHas(sp.Path, fileIDOf(t, sp.Path), src.Gen, data)
}
