package devicesync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (e *env) flushes() int {
	e.srv.Lock()
	defer e.srv.Unlock()
	return e.srv.FlushRequests
}

// Debounce coalesces a burst of appends into few flushes; max wait bounds
// the delay during a continuous burst.
func TestSchedulerDebounce(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{Append: Cadence{Debounce: 60 * time.Millisecond, MaxWait: 250 * time.Millisecond}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sc.Run(ctx)
	sp := e.spec("live.jsonl", transcript.StorageJSONLAppend)
	data := jsonlLines(20, 100, 100)
	start := time.Now()
	firstFlush := time.Duration(0)
	for i := range 50 { // 50 lines over ~500ms
		appendFile(t, sp.Path, data[i*len(data)/100:(i+1)*len(data)/100])
		sc.Notify(sp)
		time.Sleep(10 * time.Millisecond)
		if firstFlush == 0 && e.flushes() > 0 {
			firstFlush = time.Since(start)
		}
	}
	waitFor(t, "idle", func() bool { return sc.Status().Queued == 0 && e.flushes() > 0 })
	n := e.flushes()
	if n < 2 || n > 5 {
		t.Fatalf("want 2..5 flushes for a 500ms burst with 250ms max wait, got %d", n)
	}
	if firstFlush == 0 || firstFlush > 400*time.Millisecond {
		t.Fatalf("first flush after %v; max wait is 250ms", firstFlush)
	}
	// Hook flush: immediate, no debounce.
	appendFile(t, sp.Path, data[len(data)/2:])
	sc.Flush(sp)
	waitFor(t, "hook flush", func() bool { return e.flushes() == n+1 })
	e.requireServerHas(sp.Path, fileIDOf(t, sp.Path), 0, data[:len(data)/2+len(data)-len(data)/2])
}

// Server down is normal: the scheduler backs off, reports it, never blocks
// Notify, and drains the queue (including sources captured before a
// restart) when the server returns.
func TestSchedulerBackoffAndRecovery(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A capture from a previous run that never reached the server.
	old := e.spec("old.jsonl", transcript.StorageJSONLAppend)
	oldData := jsonlLines(21, 100, 100)
	appendFile(t, old.Path, oldData)
	e.srv.SetDown(true)
	if err := e.sy.Sync(ctx, old); err == nil {
		t.Fatal("want error while down")
	}

	sc := NewScheduler(e.sy, SchedulerConfig{Append: Cadence{10 * time.Millisecond, 50 * time.Millisecond},
		BackoffMin: 20 * time.Millisecond, BackoffMax: 80 * time.Millisecond})
	go sc.Run(ctx)
	sp := e.spec("new.jsonl", transcript.StorageJSONLAppend)
	data := jsonlLines(22, 100, 100)
	appendFile(t, sp.Path, data)
	t0 := time.Now()
	sc.Notify(sp)
	if time.Since(t0) > 10*time.Millisecond {
		t.Fatal("Notify blocked")
	}
	waitFor(t, "server down reported", func() bool { return sc.Status().ServerDown })
	e.srv.Lock()
	before := e.srv.Requests
	e.srv.Unlock()
	time.Sleep(300 * time.Millisecond)
	e.srv.Lock()
	retries := e.srv.Requests - before
	e.srv.Unlock()
	if retries < 2 || retries > 20 {
		t.Fatalf("want a few backed-off retries in 300ms, got %d", retries)
	}
	e.srv.SetDown(false)
	waitFor(t, "drained", func() bool {
		st := sc.Status()
		return !st.ServerDown && st.Queued == 0
	})
	e.requireServerHas(old.Path, fileIDOf(t, old.Path), 0, oldData)
	e.requireServerHas(sp.Path, fileIDOf(t, sp.Path), 0, data)
}

// NotifyExportFunc builds the export when the flush runs, not at notify
// time, and the flush survives a server outage.
func TestSchedulerExportFunc(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{Document: Cadence{10 * time.Millisecond, 50 * time.Millisecond},
		BackoffMin: 20 * time.Millisecond, BackoffMax: 40 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sp := SourceSpec{Path: e.dir + "/db#s1", Agent: transcript.AgentDevin, StorageKind: transcript.StorageSQLite, SessionKey: "s1", Parser: "devin-export@2"}
	data := jsonlLines(23, 50, 100)
	var calls atomic.Int32
	fn := func(context.Context, []byte) (Export, error) { calls.Add(1); return Export{Data: data}, nil }
	e.srv.SetDown(true)
	for range 5 {
		sc.NotifyExportFunc(sp, fn)
	}
	go sc.Run(ctx)
	waitFor(t, "server down reported", func() bool { return sc.Status().ServerDown })
	if n := calls.Load(); n < 1 {
		t.Fatalf("export built %d times before the first flush", n)
	}
	e.srv.SetDown(false)
	waitFor(t, "drained", func() bool { st := sc.Status(); return !st.ServerDown && st.Queued == 0 })
	e.requireServerHas(sp.Path, "", 0, data)
}

// A hook flush jumps a backlog: after an outage (or on a first sync) the
// live session goes first. Found by the two-device e2e with a real-corpus
// sample: a live append waited 10s behind the first sync.
func TestSchedulerFlushJumpsBacklog(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{Append: Cadence{time.Millisecond, time.Millisecond}, BackoffMin: time.Hour, BackoffMax: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.srv.SetDown(true)
	var backlog []SourceSpec
	for i := range 5 {
		sp := e.spec(fmt.Sprintf("old%d.jsonl", i), transcript.StorageJSONLAppend)
		appendFile(t, sp.Path, jsonlLines(uint64(30+i), 10, 100))
		backlog = append(backlog, sp)
		sc.Notify(sp)
	}
	go sc.Run(ctx)
	waitFor(t, "backlog queued and failing", func() bool {
		sc.mu.Lock()
		defer sc.mu.Unlock()
		return len(sc.order) == len(backlog) && sc.down
	})
	live := e.spec("live.jsonl", transcript.StorageJSONLAppend)
	appendFile(t, live.Path, jsonlLines(40, 10, 100))
	sc.Flush(live)
	sc.mu.Lock()
	head := sc.order[0]
	sc.mu.Unlock()
	if head != live.Path {
		t.Fatalf("queue head is %s, want the flushed source", head)
	}
}

// S3/D15: a source failing on its own (here an export whose function
// errors) backs off alone; other sources keep uploading, and the failing
// one shows in Status.
func TestSchedulerFailingSourceDelaysOnlyItself(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sc := NewScheduler(e.sy, SchedulerConfig{Append: Cadence{5 * time.Millisecond, 10 * time.Millisecond},
		Document: Cadence{5 * time.Millisecond, 10 * time.Millisecond}, BackoffMin: 20 * time.Millisecond, BackoffMax: 80 * time.Millisecond})
	go sc.Run(ctx)
	bad := SourceSpec{Path: "devin:x#bad", Agent: transcript.AgentDevin, StorageKind: transcript.StorageSQLite, Parser: "devin@1"}
	sc.NotifyExportFunc(bad, func(context.Context, []byte) (Export, error) { return Export{}, errors.New("devin schema changed") })
	waitFor(t, "bad source failing", func() bool { return len(sc.Status().Failing) == 1 })
	for i := range 3 {
		sp := e.spec(fmt.Sprintf("good%d.jsonl", i), transcript.StorageJSONLAppend)
		appendFile(t, sp.Path, jsonlLines(uint64(50+i), 50, 100))
		sc.Notify(sp)
		waitFor(t, "good source synced", func() bool {
			_, _, ok := e.srv.Source(sp.Path, fileIDOf(t, sp.Path))
			return ok
		})
	}
	st := sc.Status()
	if st.ServerDown || len(st.Failing) != 1 || st.Failing[0].Path != bad.Path || !strings.Contains(st.Failing[0].Error, "devin schema changed") {
		t.Fatalf("status %+v", st)
	}
	if st.Failing[0].Attempts < 2 {
		t.Fatalf("bad source retried %d times", st.Failing[0].Attempts)
	}
}

// D15: a hook flush resets the server backoff, so the live session goes
// up as soon as the server is back instead of after the backoff.
func TestSchedulerHookFlushResetsBackoff(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sc := NewScheduler(e.sy, SchedulerConfig{Append: Cadence{time.Millisecond, time.Millisecond}, BackoffMin: time.Hour, BackoffMax: time.Hour})
	go sc.Run(ctx)
	sp := e.spec("live.jsonl", transcript.StorageJSONLAppend)
	appendFile(t, sp.Path, jsonlLines(60, 20, 100))
	e.srv.SetDown(true)
	sc.Notify(sp)
	waitFor(t, "server down reported", func() bool { return sc.Status().ServerDown })
	e.srv.SetDown(false)
	sc.Flush(sp)
	waitFor(t, "flushed after the hook", func() bool { st := sc.Status(); return !st.ServerDown && st.Queued == 0 })
	e.requireServerHas(sp.Path, fileIDOf(t, sp.Path), 0, jsonlLines(60, 20, 100))
}

// D15: backoff is capped at 30s by default.
func TestSchedulerBackoffCap(t *testing.T) {
	var c SchedulerConfig
	c.defaults()
	if c.BackoffMax != 30*time.Second {
		t.Fatalf("BackoffMax %v", c.BackoffMax)
	}
}

// D21: a Devin export that stops changing has its provisional tail sealed
// after SealAfter, the same as a quiet file, without another notification.
func TestSchedulerSealsQuietExport(t *testing.T) {
	e := newEnv(t, Config{SealAfter: 300 * time.Millisecond}, 4<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{Document: Cadence{Debounce: 10 * time.Millisecond, MaxWait: 50 * time.Millisecond}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sc.Run(ctx)
	sp := SourceSpec{Path: "devin:sessions.db#quiet", Agent: transcript.AgentDevin, StorageKind: transcript.StorageSQLite, Parser: "devin-export@2"}
	rows := jsonlLines(31, 300, 150)
	var exports atomic.Int32
	sc.NotifyExportFunc(sp, func(context.Context, []byte) (Export, error) { exports.Add(1); return Export{Data: rows}, nil })
	waitFor(t, "first flush", func() bool { _, tail := e.srv.Manifest(sp.Path, "", 0); return tail != nil })
	waitFor(t, "sealed tail", func() bool {
		entries, tail := e.srv.Manifest(sp.Path, "", 0)
		return tail == nil && len(entries) > 0 && entries[len(entries)-1].End() == int64(len(rows))
	})
	e.requireServerHas(sp.Path, "", 0, rows)
	// Sealed: no further re-exports.
	n := exports.Load()
	time.Sleep(time.Second + 700*time.Millisecond)
	if got := exports.Load(); got != n {
		t.Fatalf("export re-run %d times after sealing", got-n)
	}
}

// A filter (the agent's path rules) drops a queued source without
// uploading it; others still go.
func TestSchedulerFilterDropsRejectedSources(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{Append: Cadence{Debounce: 10 * time.Millisecond, MaxWait: 50 * time.Millisecond}})
	denied, allowed := e.spec("denied.jsonl", transcript.StorageJSONLAppend), e.spec("allowed.jsonl", transcript.StorageJSONLAppend)
	sc.SetFilter(func(sp SourceSpec) bool { return sp.Path != denied.Path })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sc.Run(ctx)
	appendFile(t, denied.Path, jsonlLines(41, 20, 100))
	appendFile(t, allowed.Path, jsonlLines(42, 20, 100))
	sc.Notify(denied)
	sc.Notify(allowed)
	waitFor(t, "allowed upload", func() bool { _, tail := e.srv.Manifest(allowed.Path, fileIDOf(t, allowed.Path), 0); return tail != nil })
	waitFor(t, "idle", func() bool { return sc.Status().Queued == 0 })
	if entries, tail := e.srv.Manifest(denied.Path, fileIDOf(t, denied.Path), 0); len(entries) > 0 || tail != nil {
		t.Fatal("a rejected source was uploaded")
	}
}

// The bound limits what a flush captures, and a negative bound drops the
// source as the filter does.
func TestSchedulerBound(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{Append: Cadence{Debounce: 10 * time.Millisecond, MaxWait: 50 * time.Millisecond}})
	held, part := e.spec("held.jsonl", transcript.StorageJSONLAppend), e.spec("part.jsonl", transcript.StorageJSONLAppend)
	data := jsonlLines(43, 20, 100)
	cut := int64(bytes.IndexByte(data, '\n') + 1)
	sc.SetBound(func(sp SourceSpec) (int64, bool) {
		if sp.Path == held.Path {
			return -1, true
		}
		return cut, true
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sc.Run(ctx)
	appendFile(t, held.Path, data)
	appendFile(t, part.Path, data)
	sc.Notify(held)
	sc.Notify(part)
	waitFor(t, "bounded upload", func() bool { _, tail := e.srv.Manifest(part.Path, fileIDOf(t, part.Path), 0); return tail != nil })
	waitFor(t, "idle", func() bool { return sc.Status().Queued == 0 })
	e.requireServerHas(part.Path, fileIDOf(t, part.Path), 0, data[:cut])
	if entries, tail := e.srv.Manifest(held.Path, fileIDOf(t, held.Path), 0); len(entries) > 0 || tail != nil {
		t.Fatal("a source with a negative bound was uploaded")
	}
}

// A debounce timer that fires while the scheduler lock is held, after its
// job has left waiting (a hook Flush made it due and the worker took it),
// must not queue that job again: a later Notify would then rewrite the job
// the worker is flushing (a data race seen under go test -race).
func TestSchedulerLateTimerDoesNotRequeueTakenJob(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{Append: Cadence{time.Millisecond, time.Millisecond}})
	sp := e.spec("live.jsonl", transcript.StorageJSONLAppend)
	sc.Notify(sp)
	sc.mu.Lock()
	j := sc.waiting[sp.Path]
	time.Sleep(50 * time.Millisecond) // the timer fires and blocks on the lock
	// Flush made it due and the worker took it: gone from waiting and ready.
	delete(sc.waiting, sp.Path)
	sc.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.ready[sp.Path] == j {
		t.Fatal("a late debounce timer queued a job the worker already took")
	}
}

// A file whose tail was sealed and that has not changed since needs no
// further seal check: the scheduler must not re-arm its seal timer after
// the sealing sync (else every synced file re-syncs each SealAfter
// forever).
func TestSchedulerStopsSealTimerOnceSealed(t *testing.T) {
	e := newEnv(t, Config{SealAfter: 300 * time.Millisecond}, 4<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{Append: Cadence{Debounce: 10 * time.Millisecond, MaxWait: 50 * time.Millisecond}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sc.Run(ctx)
	sp := e.spec("quiet.jsonl", transcript.StorageJSONLAppend)
	data := jsonlLines(32, 300, 150)
	appendFile(t, sp.Path, data)
	sc.Notify(sp)
	id := fileIDOf(t, sp.Path)
	waitFor(t, "first flush", func() bool { _, tail := e.srv.Manifest(sp.Path, id, 0); return tail != nil })
	waitFor(t, "sealed tail", func() bool {
		entries, tail := e.srv.Manifest(sp.Path, id, 0)
		return tail == nil && len(entries) > 0 && entries[len(entries)-1].End() == int64(len(data))
	})
	waitFor(t, "idle", func() bool { return sc.Status().Queued == 0 })
	sc.mu.Lock()
	armed := len(sc.seal)
	sc.mu.Unlock()
	if armed != 0 {
		t.Fatalf("%d seal timers still armed for a sealed, unchanged source", armed)
	}
	// A later append arms it again and gets sealed in turn.
	more := jsonlLines(33, 50, 150)
	appendFile(t, sp.Path, more)
	sc.Notify(sp)
	all := int64(len(data) + len(more))
	waitFor(t, "re-sealed tail", func() bool {
		entries, tail := e.srv.Manifest(sp.Path, id, 0)
		return tail == nil && len(entries) > 0 && entries[len(entries)-1].End() == all
	})
}

// The scheduler drops a source's seal timer when provisional says it has
// no tail. A store read that fails must not say so: the tail stays
// unsealed with nothing left to re-check it until the file changes.
func TestProvisionalUnknownKeepsSealCheck(t *testing.T) {
	e := newEnv(t, Config{SealAfter: time.Hour}, 4<<20)
	sp := e.spec("open.jsonl", transcript.StorageJSONLAppend)
	appendFile(t, sp.Path, jsonlLines(34, 20, 100))
	e.sync(sp)
	if !e.sy.provisional(context.Background(), sp.Path) {
		t.Fatal("want a provisional tail while active")
	}
	if e.sy.provisional(context.Background(), e.path("never-synced.jsonl")) {
		t.Fatal("a source never synced has no tail")
	}
	e.store.Close()
	if !e.sy.provisional(context.Background(), sp.Path) {
		t.Fatal("a failed store read reported no tail; the seal timer would be dropped")
	}
}

func TestSchedulerStopsSealTimerWithUnfinishedRecord(t *testing.T) {
	e := newEnv(t, Config{SealAfter: 300 * time.Millisecond}, 4<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{Append: Cadence{Debounce: 10 * time.Millisecond, MaxWait: 50 * time.Millisecond}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); sc.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	sp := e.spec("unfinished.jsonl", transcript.StorageJSONLAppend)
	complete := jsonlLines(37, 8, 150)
	partial := []byte(`{"text":"unfinished`)
	appendFile(t, sp.Path, append(append([]byte{}, complete...), partial...))
	sc.Notify(sp)
	id := fileIDOf(t, sp.Path)
	waitFor(t, "complete prefix flushed", func() bool { _, tail := e.srv.Manifest(sp.Path, id, 0); return tail != nil })
	waitFor(t, "complete prefix sealed", func() bool {
		entries, tail := e.srv.Manifest(sp.Path, id, 0)
		return tail == nil && len(entries) > 0 && entries[len(entries)-1].End() == int64(len(complete))
	})
	waitFor(t, "idle", func() bool { return sc.Status().Queued == 0 })
	sc.mu.Lock()
	armed := len(sc.seal)
	sc.mu.Unlock()
	if armed != 0 {
		t.Fatalf("unfinished record kept %d seal timers armed", armed)
	}
	e.requireServerHas(sp.Path, id, 0, complete)
	appendFile(t, sp.Path, []byte(` record"}`+"\n"))
	sc.Notify(sp)
	all := append(append(append([]byte{}, complete...), partial...), []byte(` record"}`+"\n")...)
	waitFor(t, "completed record captured", func() bool { got, err := e.srv.Reconstruct(sp.Path, id, 0); return err == nil && bytes.Equal(got, all) })
}
