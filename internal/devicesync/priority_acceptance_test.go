package devicesync

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

// The priority promise concerns later batches and new bytes, not just the
// first hook-promoted request. Exercise the real capture/acknowledgement path
// while cold sources remain ready and a fresh notification replaces the
// running job without another hook.
func TestPriorityKeepsActiveAppendAheadOfColdBacklog(t *testing.T) {
	e := newEnv(t, Config{MaxRequestBytes: 16 << 10, SealAfter: -1}, 4<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{Append: Cadence{time.Nanosecond, time.Nanosecond}})
	active := e.spec("active.jsonl", transcript.StorageJSONLAppend)
	changed := e.spec("changed.jsonl", transcript.StorageJSONLAppend)
	activeData := jsonlLines(701, 500, 300)
	added := jsonlLines(702, 120, 300)
	changedData := jsonlLines(703, 500, 300)
	appendFile(t, active.Path, activeData)
	appendFile(t, changed.Path, changedData)

	const coldCount = 64
	cold := make([]SourceSpec, coldCount)
	coldBytes := make([][]byte, coldCount)
	for i := range coldCount {
		cold[i] = e.spec(fmt.Sprintf("history-%02d.jsonl", i), transcript.StorageJSONLAppend)
		coldBytes[i] = jsonlLines(uint64(800+i), 2, 100)
		appendFile(t, cold[i].Path, coldBytes[i])
		sc.NotifyWithNotice(cold[i], Notice{Kind: NoticeHistorical, ActivityAt: time.Unix(int64(i+1), 0)})
	}
	sc.NotifyWithNotice(changed, Notice{Kind: NoticeChanged, ActivityAt: time.Now()})
	waitFor(t, "cold sources and changed source ready", func() bool {
		sc.mu.Lock()
		defer sc.mu.Unlock()
		return len(sc.ready) == coldCount+1
	})
	sc.Flush(active)

	tr := &turnRecordingTransport{Transport: e.client}
	e.sy.tr = tr
	injected := false
	tr.before = func(_ context.Context, req *syncproto.FlushRequest) {
		if injected || req.Header.Source.Path != active.Path {
			return
		}
		injected = true
		appendFile(t, active.Path, added)
		sc.NotifyWithNotice(active, Notice{Kind: NoticeChanged, ActivityAt: time.Now()})
		waitFor(t, "append notification ready during hook turn", func() bool {
			sc.mu.Lock()
			defer sc.mu.Unlock()
			return sc.ready[active.Path] != nil
		})
	}
	sc.runOnce(t.Context())

	activeRequests, lastActive, firstHistory := 0, -1, -1
	for i, req := range tr.requests {
		switch req.path {
		case active.Path:
			activeRequests++
			lastActive = i
		case changed.Path:
		default:
			if firstHistory < 0 {
				firstHistory = i
			}
		}
	}
	if !injected || activeRequests < 6 {
		t.Fatalf("fixture did not exercise an append during multiple active turns: injected=%v requests=%d", injected, activeRequests)
	}
	if lastActive >= coldCount || firstHistory < 0 || firstHistory > 6 {
		t.Fatalf("active continuation or historical progress lost priority: active_last=%d history_first=%d requests=%d", lastActive, firstHistory, len(tr.requests))
	}
	t.Logf("active request count=%d; last active dispatch=%d; first historical dispatch=%d; total dispatches=%d", activeRequests, lastActive+1, firstHistory+1, len(tr.requests))
	if tr.maxActive != 1 || sc.Progress().Queued != 0 {
		t.Fatalf("serial queue did not drain: concurrency=%d status=%+v", tr.maxActive, sc.Progress())
	}
	e.requireServerHas(active.Path, fileIDOf(t, active.Path), 0, append(activeData, added...))
	e.requireServerHas(changed.Path, fileIDOf(t, changed.Path), 0, changedData)
	for i := range coldCount {
		e.requireServerHas(cold[i].Path, fileIDOf(t, cold[i].Path), 0, coldBytes[i])
	}
}
