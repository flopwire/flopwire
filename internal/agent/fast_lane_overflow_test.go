package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/perfguard"
	"github.com/flopwire/flopwire/internal/transcript"
)

// Three hot transcripts exceed a two-file tick budget by exactly one.
// An append to the oldest cached identity must eventually be observed
// without requiring a filesystem event or the full discovery sweep.
func TestFastLaneOverflowEventuallyChecksOlderTranscript(t *testing.T) {
	f := newFixture(t, "-")
	f.once()
	p := f.path(alphaRel)
	oldest := f.a.targets[p]
	if oldest == nil {
		t.Fatal("fixture transcript was not discovered")
	}
	targets := map[string]*target{p: oldest}
	for path, candidate := range f.a.targets {
		if path != p && candidate.kind == kindTranscript && len(targets) < 3 {
			targets[path] = candidate
		}
	}
	if len(targets) != 3 {
		t.Fatal("need three fixture transcripts")
	}
	f.a.targets = targets
	f.a.cfg.MaxHot = 2
	oldest.seen.CTime = time.Now().Add(-time.Minute).UnixNano()
	appendFile(t, p, claudeUser("c1000000-0000-4000-8000-000000000399", "overflow fairness witness"))

	runCtx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	f.a.startWorkers(runCtx, &workers)
	defer func() { cancel(); f.a.kick(); workers.Wait() }()
	for range 3 {
		f.a.fastLane(runCtx)
		f.a.WaitIdle()
	}
	if err := f.store.Sync(runCtx); err != nil {
		t.Fatal(err)
	}
	if len(f.find("overflow fairness witness", false)) != 1 {
		t.Fatal("older hot transcript starved beyond a complete polling cycle")
	}
}

func TestFastLaneOverflowKeepsStatBudget(t *testing.T) {
	f := newFixture(t, "-")
	f.a.cfg.MaxHot = 2
	f.a.targets = map[string]*target{}
	for i := range 3 {
		p := filepath.Join(f.home, fmt.Sprintf("missing-%d.jsonl", i))
		f.a.targets[p] = &target{path: p, kind: kindTranscript,
			seen: transcript.Identity{CTime: time.Now().UnixNano()}}
	}
	counter := perfguard.CountFS(t, f.home)
	for range 3 {
		cost := counter.Measure(func() { f.a.fastLane(ctx) })
		if cost.Stats != 2 {
			t.Fatalf("fast-lane tick made %d stats, want budget 2", cost.Stats)
		}
	}
}
