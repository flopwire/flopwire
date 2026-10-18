package devicesync

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

func TestScheduleHintsSurviveSpecRewritesAndRestartWithoutLane(t *testing.T) {
	e := newEnv(t, Config{MaxRequestBytes: 16 << 10, SealAfter: -1}, 1<<20)
	sp := e.spec("pending.jsonl", transcript.StorageJSONLAppend)
	appendFile(t, sp.Path, jsonlLines(200, 500, 100))
	activity, waiting := time.Unix(1234, 0), time.Unix(1000, 0)
	h := scheduleHints{ActivityAt: activity, WaitingSince: waiting}
	if err := e.store.admitScheduleHints(t.Context(), sp.Path, h); err != nil {
		t.Fatal(err)
	}
	outcome, err := e.sy.syncTurn(t.Context(), sp, nil, -1, nil, captureSource)
	if err != nil || outcome != uploadPending {
		t.Fatalf("outcome=%v err=%v", outcome, err)
	}
	src, err := e.store.source(t.Context(), sp.Path, &sp)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.repoSent(t.Context(), src); err != nil {
		t.Fatal(err)
	}
	// Serving a pending turn resets age; admission cannot invent fresh activity.
	serviced := time.Unix(1500, 0)
	if err := e.store.serviceScheduleHints(t.Context(), sp.Path, scheduleHints{ActivityAt: activity, WaitingSince: serviced}); err != nil {
		t.Fatal(err)
	}
	restartTurnEnv(t, e)
	pending, err := e.store.pendingSources(t.Context())
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	if !pending[0].Hints.ActivityAt.Equal(activity) || !pending[0].Hints.WaitingSince.Equal(serviced) {
		t.Fatalf("hints=%+v", pending[0].Hints)
	}
	sc := NewScheduler(e.sy, SchedulerConfig{})
	// Restoration has facts, not interactive/changed membership.
	j := &job{spec: pending[0].Spec, hints: pending[0].Hints}
	sc.due(sp.Path, j)
	if j.lane != historicalLane {
		t.Fatal("restart persisted a priority class")
	}
}

func TestScheduleHintsLegacyPendingUsesCaptureAgeNotActivity(t *testing.T) {
	e := newEnv(t, Config{MaxRequestBytes: 16 << 10, SealAfter: -1}, 1<<20)
	sp := e.spec("legacy.jsonl", transcript.StorageJSONLAppend)
	appendFile(t, sp.Path, jsonlLines(201, 500, 100))
	outcome, err := e.sy.syncTurn(t.Context(), sp, nil, -1, nil, captureSource)
	if err != nil || outcome != uploadPending {
		t.Fatalf("outcome=%v err=%v", outcome, err)
	}
	pending, err := e.store.pendingSources(t.Context())
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	var captured int64
	if err := e.store.db.QueryRow(`SELECT min(captured_at) FROM devsync_gens`).Scan(&captured); err != nil {
		t.Fatal(err)
	}
	if !pending[0].Hints.ActivityAt.IsZero() || pending[0].Hints.WaitingSince.UnixNano() != captured {
		t.Fatalf("legacy hints=%+v captured=%d", pending[0].Hints, captured)
	}
}

func TestTurnQueueIndexesRemainBoundedAcrossCoalescingAndRetries(t *testing.T) {
	q := newTurnQueue()
	now := time.Unix(100, 0)
	const n = 1000
	jobs := make([]*job, n)
	for i := range jobs {
		jobs[i] = &job{hints: scheduleHints{ActivityAt: now, WaitingSince: now}}
		q.insert(fmt.Sprint(i), jobs[i], now, now)
	}
	for round := range 20 {
		for i, j := range jobs {
			j.hints.ActivityAt = now.Add(time.Duration(round) * time.Second)
			q.insert(fmt.Sprint(i), j, now.Add(time.Hour), now)
			q.insert(fmt.Sprint(i), j, now, now)
		}
	}
	if len(q.recent.items) != n || len(q.oldest.items) != n || len(q.delayed.items) != 0 {
		t.Fatalf("index lengths recent=%d oldest=%d delayed=%d", len(q.recent.items), len(q.oldest.items), len(q.delayed.items))
	}
	for range n {
		e, _ := q.take(now)
		if e == nil {
			t.Fatal("missing indexed job")
		}
	}
	if len(q.recent.items) != 0 || len(q.oldest.items) != 0 {
		t.Fatal("terminal indexes retained entries")
	}
}

func BenchmarkTurnQueueHistoricalDispatch(b *testing.B) {
	for _, n := range []int{22000, 50000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			q := newTurnQueue()
			now := time.Unix(100, 0)
			for i := range n {
				j := &job{hints: scheduleHints{ActivityAt: now.Add(time.Duration(i)), WaitingSince: now}}
				q.insert(fmt.Sprint(i), j, now, now)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				e, _ := q.take(now)
				if e == nil {
					b.Fatal("empty")
				}
				e.job.hints.WaitingSince = now
				q.insert(e.path, e.job, now, now)
			}
		})
	}
}

// Canceled metadata writes cannot silently claim durable scheduling facts.
func TestScheduleHintsCanceledWriteReportsFailure(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := e.store.admitScheduleHints(ctx, "cancel", scheduleHints{WaitingSince: time.Now()}); err == nil {
		t.Fatal("canceled admission succeeded")
	}
}

func TestPendingRestorationPreservesNewerNoticeAndExporter(t *testing.T) {
	e := newEnv(t, Config{MaxRequestBytes: 16 << 10, SealAfter: -1}, 1<<20)
	sp := e.spec("restored.jsonl", transcript.StorageJSONLAppend)
	appendFile(t, sp.Path, jsonlLines(202, 500, 100))
	outcome, err := e.sy.syncTurn(t.Context(), sp, nil, -1, nil, captureSource)
	if err != nil || outcome != uploadPending {
		t.Fatalf("outcome=%v err=%v", outcome, err)
	}
	old := scheduleHints{ActivityAt: time.Unix(50, 0), WaitingSince: time.Unix(40, 0)}
	if err := e.store.admitScheduleHints(t.Context(), sp.Path, old); err != nil {
		t.Fatal(err)
	}
	for _, ready := range []bool{false, true} {
		t.Run(fmt.Sprint(ready), func(t *testing.T) {
			sc := NewScheduler(e.sy, SchedulerConfig{Append: Cadence{time.Hour, time.Hour}})
			latest := sp
			latest.Parser = "claude@fresh"
			called := false
			sc.NotifyExportFuncWithNotice(latest, func(context.Context, []byte) (Export, error) { called = true; return Export{}, nil }, Notice{Kind: NoticeChanged, ActivityAt: time.Unix(100, 0)})
			sc.mu.Lock()
			j := sc.waiting[sp.Path]
			j.timer.Stop()
			if ready {
				sc.dueLocked(sp.Path, j)
			}
			sc.mu.Unlock()
			ctx, cancel := context.WithCancel(t.Context())
			sc.SetFilter(func(SourceSpec) bool { cancel(); return true })
			if !ready { // Restore merges into the waiting notification without making it due.
				sc.halted = errors.New("synthetic pin gate")
				sc.cfg.Repin = func() bool { cancel(); return false }
			}
			_ = sc.Run(ctx)
			sc.mu.Lock()
			surviving := sc.ready[sp.Path]
			if surviving == nil {
				surviving = sc.waiting[sp.Path]
			}
			sc.mu.Unlock()
			if surviving != j || surviving.spec.Parser != latest.Parser || surviving.exportFn == nil || surviving.action != captureSource || surviving.lane != changedLane || called {
				t.Fatalf("new notification replaced: %+v called=%v", surviving, called)
			}
			if !surviving.hints.ActivityAt.Equal(time.Unix(100, 0)) || !surviving.hints.WaitingSince.Equal(old.WaitingSince) {
				t.Fatalf("hints=%+v", surviving.hints)
			}
		})
	}
}

func TestFailedServiceHintsKeepWaitingAgeAfterAcceptedUpload(t *testing.T) {
	e := newEnv(t, Config{MaxRequestBytes: 16 << 10, SealAfter: -1}, 1<<20)
	sp := e.spec("hint-failure.jsonl", transcript.StorageJSONLAppend)
	appendFile(t, sp.Path, jsonlLines(203, 500, 100))
	old := time.Unix(100, 0)
	sc := NewScheduler(e.sy, SchedulerConfig{})
	j := &job{spec: sp, hints: scheduleHints{WaitingSince: old}}
	if err := e.store.admitScheduleHints(t.Context(), sp.Path, j.hints); err != nil {
		t.Fatal(err)
	}
	// Admission merges the unchanged waiting age; only successful-service write fails.
	if _, err := e.store.db.Exec(`CREATE TRIGGER reject_service BEFORE UPDATE ON devsync_schedule_hints WHEN NEW.waiting_since>OLD.waiting_since BEGIN SELECT RAISE(ABORT,'synthetic service failure'); END`); err != nil {
		t.Fatal(err)
	}
	outcome, err := sc.executeTurn(t.Context(), j, -1, false, nil)
	if err == nil || outcome != uploadPending || e.flushes() != 1 {
		t.Fatalf("outcome=%v err=%v flushes=%d", outcome, err, e.flushes())
	}
	if !j.hints.WaitingSince.Equal(old) {
		t.Fatalf("failed service reset age=%v", j.hints.WaitingSince)
	}
	h, err := e.store.loadScheduleHints(t.Context(), sp.Path)
	if err != nil || !h.WaitingSince.Equal(old) {
		t.Fatalf("durable=%+v err=%v", h, err)
	}
	if _, err := e.store.db.Exec(`DROP TRIGGER reject_service`); err != nil {
		t.Fatal(err)
	}
	j.action = resumeUpload
	if _, err := sc.executeTurn(t.Context(), j, -1, false, nil); err != nil {
		t.Fatal(err)
	}
	if !j.hints.WaitingSince.After(old) {
		t.Fatal("successful retry did not reset service age")
	}
}

type cancelAcceptedTurn struct {
	syncproto.Transport
	cancel context.CancelFunc
}

func (tr cancelAcceptedTurn) Flush(ctx context.Context, req *syncproto.FlushRequest) (*syncproto.FlushResponse, error) {
	response, err := tr.Transport.Flush(ctx, req)
	if err == nil {
		tr.cancel()
	}
	return response, err
}
func TestCanceledAcceptedTurnKeepsWaitingAgeAndOldestOrder(t *testing.T) {
	e := newEnv(t, Config{MaxRequestBytes: 16 << 10, SealAfter: -1}, 1<<20)
	sp := e.spec("cancel-hints.jsonl", transcript.StorageJSONLAppend)
	appendFile(t, sp.Path, jsonlLines(204, 500, 100))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e.sy.tr = cancelAcceptedTurn{Transport: e.client, cancel: cancel}
	sc := NewScheduler(e.sy, SchedulerConfig{})
	old := time.Unix(100, 0)
	j := &job{spec: sp, hints: scheduleHints{WaitingSince: old}}
	_, err := sc.executeTurn(ctx, j, -1, false, nil)
	if err == nil || e.flushes() != 1 {
		t.Fatalf("canceled accepted request err=%v flushes=%d", err, e.flushes())
	}
	if !j.hints.WaitingSince.Equal(old) {
		t.Fatalf("canceled age=%v", j.hints.WaitingSince)
	}
	newer := &job{spec: SourceSpec{Path: "newer"}, hints: scheduleHints{WaitingSince: old.Add(time.Second)}}
	sc.due(sp.Path, j)
	sc.due(newer.spec.Path, newer)
	sc.queue.historicalTurns = 7
	if entry, _ := sc.next(time.Now()); entry == nil || entry.job != j {
		t.Fatal("canceled oldest lineage displaced")
	}
}

// This comparison isolates eligibility selection: the former slice loop had
// to inspect every delayed path before the sole eligible path. The indexed
// queue promotes only expired deadlines and directly selects a lane entry.
func BenchmarkTurnQueueDelayedBacklog(b *testing.B) {
	for _, n := range []int{22000, 50000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			now := time.Unix(100, 0)
			b.Run("slice-scan", func(b *testing.B) {
				deadlines := make([]time.Time, n+1)
				for i := range n {
					deadlines[i] = now.Add(time.Hour)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					selected := -1
					for i, due := range deadlines {
						if !now.Before(due) {
							selected = i
							break
						}
					}
					if selected != n {
						b.Fatal("wrong selection")
					}
				}
			})
			b.Run("indexed", func(b *testing.B) {
				q := newTurnQueue()
				for i := range n {
					j := &job{}
					q.insert(fmt.Sprint(i), j, now.Add(time.Hour), now)
				}
				j := &job{}
				q.insert("eligible", j, now, now)
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					e, _ := q.take(now)
					if e == nil || e.path != "eligible" {
						b.Fatal("wrong selection")
					}
					q.insert(e.path, j, now, now)
				}
			})
		})
	}
}
