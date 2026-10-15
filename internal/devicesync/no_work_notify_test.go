package devicesync

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/fsprobe"
	"github.com/flopwire/flopwire/internal/transcript"
)

// A notification arriving during the no-work proof must survive completion of
// that turn. The second stat represents a write before the next queued turn.
func TestSchedulerNoWorkPreservesConcurrentNotification(t *testing.T) {
	now := time.Now().Add(time.Hour)
	e := newEnv(t, Config{Now: func() time.Time { return now }}, 1<<20)
	sp := e.spec("notify.jsonl", transcript.StorageJSONLAppend)
	data := jsonlLines(50, 3, 100)
	more := jsonlLines(51, 3, 100)
	appendFile(t, sp.Path, data)
	e.sync(sp)
	if !e.sy.noWork(t.Context(), sp) {
		t.Fatal("fixture not settled")
	}
	sc := NewScheduler(e.sy, SchedulerConfig{})
	var stats, auths atomic.Int32
	sc.SetAuthorize(func(context.Context, SourceSpec) (*CaptureAuthorization, error) { auths.Add(1); return nil, nil })
	fsprobe.SetHook(func(op fsprobe.Op, path string) {
		if op != fsprobe.OpStat || path != sp.Path {
			return
		}
		switch stats.Add(1) {
		case 1:
			sc.Flush(sp)
		case 2:
			appendFile(t, sp.Path, more)
		}
	})
	defer fsprobe.SetHook(nil)
	before := e.flushes()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	sc.Flush(sp)
	go func() { defer close(done); sc.Run(ctx) }()
	waitFor(t, "concurrent notification uploaded", func() bool { return sc.Status().Queued == 0 && e.flushes() > before })
	if stats.Load() < 2 || auths.Load() == 0 {
		t.Fatalf("stats=%d authorizations=%d", stats.Load(), auths.Load())
	}
	e.requireServerHas(sp.Path, fileIDOf(t, sp.Path), 0, append(data, more...))
}
