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

// weightedAdmit completes debounce deterministically, without inventing a hook.
func weightedAdmit(t *testing.T, sc *Scheduler, sp SourceSpec, notice Notice) {
	t.Helper()
	sc.NotifyWithNotice(sp, notice)
	sc.mu.Lock()
	j := sc.waiting[sp.Path]
	if j != nil && j.timer != nil {
		j.timer.Stop()
	}
	sc.mu.Unlock()
	if j != nil {
		sc.debounced(sp.Path, j)
	}
}

// Stop before dispatch after n accepted manifests, so each completed turn has
// updated its scheduling age before the next selection is inspected.
func weightedRunManifests(t *testing.T, sc *Scheduler, tr *turnRecordingTransport, n int) {
	t.Helper()
	target := len(tr.requests) + n
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sc.SetFilter(func(SourceSpec) bool {
		if len(tr.requests) >= target {
			cancel()
		}
		return true
	})
	sc.runOnce(ctx)
	sc.SetFilter(nil)
	if len(tr.requests) != target {
		t.Fatalf("manifest count=%d want %d", len(tr.requests), target)
	}
}

func weightedLargeSource(t *testing.T, e *env, name string, seed uint64) SourceSpec {
	t.Helper()
	sp := e.spec(name, transcript.StorageJSONLAppend)
	appendFile(t, sp.Path, jsonlLines(seed, 1200, 500))
	return sp
}

func TestWeightedSchedulerGivesAllClassesFourTwoOneTurns(t *testing.T) {
	e := newEnv(t, Config{MaxRequestBytes: 16 << 10, SealAfter: -1}, 1<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{Append: Cadence{time.Hour, time.Hour}})
	tr := &turnRecordingTransport{Transport: e.client}
	e.sy.tr = tr
	historical := weightedLargeSource(t, e, "historical.jsonl", 140)
	changed := weightedLargeSource(t, e, "changed.jsonl", 141)
	interactive := weightedLargeSource(t, e, "interactive.jsonl", 142)
	weightedAdmit(t, sc, historical, Notice{Kind: NoticeHistorical, ActivityAt: time.Unix(100, 0)})
	weightedAdmit(t, sc, changed, Notice{Kind: NoticeChanged, ActivityAt: time.Unix(200, 0)})
	sc.Flush(interactive)
	weightedRunManifests(t, sc, tr, 14)
	cycle := []string{interactive.Path, interactive.Path, interactive.Path, interactive.Path, changed.Path, changed.Path, historical.Path}
	for i, req := range tr.requests {
		if req.path != cycle[i%len(cycle)] {
			t.Fatalf("weighted turn %d path=%s want=%s trace=%+v", i+1, req.path, cycle[i%len(cycle)], tr.requests)
		}
	}
	if tr.maxActive != 1 {
		t.Fatalf("weighted turns overlapped manifests: %d", tr.maxActive)
	}
}

func TestWeightedSchedulerLendsEmptySlotsToHighestEligibleClass(t *testing.T) {
	for _, withChanged := range []bool{false, true} {
		t.Run(fmt.Sprint("changed=", withChanged), func(t *testing.T) {
			e := newEnv(t, Config{MaxRequestBytes: 16 << 10, SealAfter: -1}, 1<<20)
			sc := NewScheduler(e.sy, SchedulerConfig{Append: Cadence{time.Hour, time.Hour}})
			tr := &turnRecordingTransport{Transport: e.client}
			e.sy.tr = tr
			historical := weightedLargeSource(t, e, "history.jsonl", 143)
			changed := weightedLargeSource(t, e, "changes.jsonl", 144)
			weightedAdmit(t, sc, historical, Notice{ActivityAt: time.Unix(100, 0)})
			if withChanged {
				weightedAdmit(t, sc, changed, Notice{Kind: NoticeChanged, ActivityAt: time.Unix(200, 0)})
			}
			weightedRunManifests(t, sc, tr, 7)
			for i, req := range tr.requests {
				want := historical.Path
				if withChanged && i < 6 {
					want = changed.Path
				}
				if req.path != want {
					t.Fatalf("borrowed turn %d path=%s want=%s trace=%+v", i+1, req.path, want, tr.requests)
				}
			}
		})
	}
}

func TestWeightedHistoricalAgeRotatesOldestLargeSources(t *testing.T) {
	e := newEnv(t, Config{MaxRequestBytes: 16 << 10, SealAfter: -1}, 2<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{Append: Cadence{time.Hour, time.Hour}})
	tr := &turnRecordingTransport{Transport: e.client}
	e.sy.tr = tr
	recent := weightedLargeSource(t, e, "recent-large.jsonl", 145)
	oldest := weightedLargeSource(t, e, "older-waiter.jsonl", 146)
	newerWaiter := weightedLargeSource(t, e, "newer-waiter.jsonl", 147)
	weightedAdmit(t, sc, oldest, Notice{ActivityAt: time.Unix(100, 0)})
	weightedAdmit(t, sc, recent, Notice{ActivityAt: time.Unix(300, 0)})
	tr.before = func(context.Context, *syncproto.FlushRequest) {
		if len(tr.requests) == 1 {
			weightedAdmit(t, sc, newerWaiter, Notice{ActivityAt: time.Unix(200, 0)})
		}
	}
	weightedRunManifests(t, sc, tr, 16)
	for i, req := range tr.requests {
		want := recent.Path
		if i == 7 {
			want = oldest.Path
		}
		if i == 15 {
			want = newerWaiter.Path
		}
		if req.path != want {
			t.Fatalf("historical choice %d path=%s want=%s trace=%+v", i+1, req.path, want, tr.requests)
		}
	}
}

func TestWeightedRunningHookSurvivesLatestChangedNotification(t *testing.T) {
	e := newEnv(t, Config{MaxRequestBytes: 16 << 10, SealAfter: -1}, 1<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{Append: Cadence{time.Hour, time.Hour}})
	tr := &turnRecordingTransport{Transport: e.client}
	e.sy.tr = tr
	hook := weightedLargeSource(t, e, "hook.jsonl", 148)
	changed := weightedLargeSource(t, e, "changed-other.jsonl", 149)
	history := weightedLargeSource(t, e, "history-other.jsonl", 150)
	weightedAdmit(t, sc, changed, Notice{Kind: NoticeChanged, ActivityAt: time.Unix(200, 0)})
	weightedAdmit(t, sc, history, Notice{ActivityAt: time.Unix(100, 0)})
	sc.Flush(hook)
	tr.before = func(context.Context, *syncproto.FlushRequest) {
		if len(tr.requests) != 1 {
			return
		}
		appendFile(t, hook.Path, jsonlLines(151, 3, 100))
		weightedAdmit(t, sc, hook, Notice{Kind: NoticeChanged, ActivityAt: time.Unix(400, 0)})
		sc.mu.Lock()
		defer sc.mu.Unlock()
		if sc.ready[hook.Path] == nil || sc.ready[hook.Path].action != captureSource {
			t.Fatal("running hook lost latest capture notification")
		}
	}
	weightedRunManifests(t, sc, tr, 7)
	want := []string{hook.Path, hook.Path, hook.Path, hook.Path, changed.Path, changed.Path, history.Path}
	for i, req := range tr.requests {
		if req.path != want[i] {
			t.Fatalf("running hook downgraded at turn %d: trace=%+v", i+1, tr.requests)
		}
	}
}

func TestWeightedHistoricalNotificationPreservesActivityAndWaitingAge(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{Append: Cadence{time.Hour, time.Hour}})
	sp := e.spec("metadata-only-history.jsonl", transcript.StorageJSONLAppend)
	activity, waiting := time.Unix(100, 0), time.Unix(50, 0)
	sc.due(sp.Path, &job{spec: sp, lane: historicalLane, hints: scheduleHints{ActivityAt: activity, WaitingSince: waiting}, action: resumeUpload})
	sc.NotifyWithNotice(sp, Notice{Kind: NoticeHistorical})
	sc.mu.Lock()
	j := sc.ready[sp.Path]
	if j.lane != historicalLane || !j.hints.ActivityAt.Equal(activity) || !j.hints.WaitingSince.Equal(waiting) || j.action != captureSource {
		sc.mu.Unlock()
		t.Fatalf("historical notification invented new change or reset age: %+v", j)
	}
	sc.mu.Unlock()
	sc.NotifyWithNotice(sp, Notice{Kind: NoticeHistorical, ActivityAt: time.Unix(90, 0)})
	sc.mu.Lock()
	defer sc.mu.Unlock()
	j = sc.ready[sp.Path]
	if j.lane != historicalLane || !j.hints.ActivityAt.Equal(activity) || !j.hints.WaitingSince.Equal(waiting) {
		t.Fatalf("older historical metadata replaced stable hints: %+v", j)
	}
}

func TestWeightedFailuresAndCancellationRetainWaitingAge(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint("cancelled=", cancelled), func(t *testing.T) {
			e := newEnv(t, Config{}, 1<<20)
			sc := NewScheduler(e.sy, SchedulerConfig{BackoffMin: time.Hour, BackoffMax: time.Hour})
			sp := SourceSpec{Path: "devin:weighted.db#failure", Agent: transcript.AgentDevin, StorageKind: transcript.StorageSQLite, Parser: "devin-export@2", Export: true}
			age := time.Unix(50, 0)
			sc.due(sp.Path, &job{spec: sp, lane: changedLane, hints: scheduleHints{ActivityAt: time.Unix(100, 0), WaitingSince: age}, exportFn: func(context.Context, []byte) (Export, error) {
				return Export{}, errors.New("synthetic exporter failed")
			}})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if cancelled {
				sc.SetFilter(func(SourceSpec) bool { cancel(); return true })
			}
			sc.runOnce(ctx)
			sc.mu.Lock()
			defer sc.mu.Unlock()
			j := sc.ready[sp.Path]
			if j == nil || j.lane != changedLane || !j.hints.WaitingSince.Equal(age) {
				t.Fatalf("failure/cancellation reset waiting age: %+v", j)
			}
			if !cancelled && (sc.failing[sp.Path] == nil || sc.failing[sp.Path].attempts != 1) {
				t.Fatal("source failure was not delayed independently")
			}
		})
	}
}

func TestWeightedDelayedQueueExcludesRetryBacklogUntilDeadline(t *testing.T) {
	q := newTurnQueue()
	now := time.Unix(1000, 0)
	deadline := now.Add(time.Hour)
	for i := range 1000 {
		path := fmt.Sprintf("delayed-%04d", i)
		q.insert(path, &job{lane: interactiveLane, hints: scheduleHints{WaitingSince: now}}, deadline, now)
	}
	live := &job{lane: historicalLane, hints: scheduleHints{ActivityAt: now, WaitingSince: now}}
	q.insert("live", live, time.Time{}, now)
	for range 10 {
		entry, wait := q.peek(now)
		if entry == nil || entry.path != "live" || wait != 0 {
			t.Fatalf("delayed retries obstruct live source: %+v,%v", entry, wait)
		}
	}
	if q.slot != 0 || q.historicalTurns != 0 || len(q.delayed.items) != 1000 || q.interactive.Len() != 0 {
		t.Fatal("peek consumed selection or admitted delayed backlog")
	}
	entry, _ := q.take(now)
	if entry == nil || entry.path != "live" {
		t.Fatal("eligible source not dispatched")
	}
	entry, wait := q.peek(now)
	if entry != nil || wait != time.Hour || len(q.delayed.items) != 1000 {
		t.Fatalf("retry deadline ignored: entry=%+v wait=%v", entry, wait)
	}
	entry, wait = q.take(deadline)
	if entry == nil || entry.path != "delayed-0000" || wait != 0 || len(q.delayed.items) != 0 {
		t.Fatalf("due retry backlog failed admission: entry=%+v wait=%v", entry, wait)
	}
}

func TestWeightedHistoricalActivityTiesUseStablePath(t *testing.T) {
	q := newTurnQueue()
	now := time.Unix(1000, 0)
	for _, path := range []string{"z-history", "a-history", "m-history"} {
		q.insert(path, &job{lane: historicalLane, hints: scheduleHints{ActivityAt: now, WaitingSince: now}}, time.Time{}, now)
	}
	for _, want := range []string{"a-history", "m-history", "z-history"} {
		entry, _ := q.take(now)
		if entry == nil || entry.path != want {
			t.Fatalf("historical tie selected %+v want %s", entry, want)
		}
	}
}

func TestWeightedContinuousNotifyAndFlushKeepEligibleFIFOPosition(t *testing.T) {
	for _, hooks := range []bool{false, true} {
		t.Run(fmt.Sprint("hooks=", hooks), func(t *testing.T) {
			e := newEnv(t, Config{MaxRequestBytes: 16 << 10, SealAfter: -1}, 1<<20)
			sc := NewScheduler(e.sy, SchedulerConfig{Append: Cadence{time.Hour, time.Hour}})
			tr := &turnRecordingTransport{Transport: e.client}
			e.sy.tr = tr
			first := weightedLargeSource(t, e, "first-in-lane.jsonl", 152)
			second := weightedLargeSource(t, e, "second-in-lane.jsonl", 153)
			if hooks {
				sc.Flush(first)
				sc.Flush(second)
			} else {
				weightedAdmit(t, sc, first, Notice{Kind: NoticeChanged, ActivityAt: time.Unix(100, 0)})
				weightedAdmit(t, sc, second, Notice{Kind: NoticeChanged, ActivityAt: time.Unix(200, 0)})
			}
			sc.mu.Lock()
			originalAge := sc.ready[first.Path].hints.WaitingSince
			sc.mu.Unlock()
			for i := range 20 {
				for _, sp := range []SourceSpec{first, second} {
					if hooks {
						sc.Flush(sp)
					} else {
						sc.NotifyWithNotice(sp, Notice{Kind: NoticeChanged, ActivityAt: time.Unix(int64(300+i), 0)})
					}
					sc.mu.Lock()
					entry, wait := sc.next(time.Now())
					age := sc.ready[first.Path].hints.WaitingSince
					queued := len(sc.ready)
					sc.mu.Unlock()
					if entry == nil || entry.path != first.Path || wait != 0 || queued != 2 || !age.Equal(originalAge) {
						t.Fatalf("continuous update moved eligible FIFO position or reset age: entry=%+v wait=%v queued=%d age=%v original=%v", entry, wait, queued, age, originalAge)
					}
				}
			}
			weightedRunManifests(t, sc, tr, 2)
			if tr.requests[0].path != first.Path || tr.requests[1].path != second.Path {
				t.Fatalf("continuously refreshed lane violated FIFO turns: %+v", tr.requests)
			}
		})
	}
}
