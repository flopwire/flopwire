package devicesync

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

type coordinatorTransport struct {
	syncproto.Transport
	before  func(context.Context, *syncproto.FlushRequest) error
	mu      sync.Mutex
	active  map[string]int
	maximum int
	overlap bool
}

func (tr *coordinatorTransport) Flush(ctx context.Context, r *syncproto.FlushRequest) (*syncproto.FlushResponse, error) {
	tr.mu.Lock()
	tr.active[r.Header.Source.Path]++
	tr.overlap = tr.overlap || tr.active[r.Header.Source.Path] > 1
	n := 0
	for _, v := range tr.active {
		n += v
	}
	tr.maximum = max(tr.maximum, n)
	tr.mu.Unlock()
	defer func() { tr.mu.Lock(); tr.active[r.Header.Source.Path]--; tr.mu.Unlock() }()
	if tr.before != nil {
		if err := tr.before(ctx, r); err != nil {
			return nil, err
		}
	}
	return tr.Transport.Flush(ctx, r)
}
func coordinatorAwait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("coordinator barrier timed out")
		var zero T
		return zero
	}
}
func coordinatorStart(t *testing.T, sc *Scheduler) (context.CancelFunc, chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- sc.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("coordinator did not drain")
		}
	})
	return cancel, done
}

func TestCoordinatorTwoWorkersOverlapAndParkNewerSamePath(t *testing.T) {
	e := newEnv(t, Config{UploadWorkers: 2, SealAfter: -1}, 1<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{Workers: 2})
	a, b, c := e.spec("a.jsonl", transcript.StorageJSONLAppend), e.spec("b.jsonl", transcript.StorageJSONLAppend), e.spec("c.jsonl", transcript.StorageJSONLAppend)
	initial := jsonlLines(901, 4, 100)
	extra := jsonlLines(902, 3, 100)
	for _, sp := range []SourceSpec{a, b, c} {
		appendFile(t, sp.Path, initial)
	}
	entered := make(chan string, 8)
	release := make(chan struct{})
	var once sync.Once
	tr := &coordinatorTransport{Transport: e.client, active: map[string]int{}}
	tr.before = func(ctx context.Context, r *syncproto.FlushRequest) error {
		entered <- r.Header.Source.Path
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	e.sy.tr = tr
	sc.due(a.Path, &job{spec: a, lane: changedLane})
	sc.due(b.Path, &job{spec: b, lane: changedLane})
	coordinatorStart(t, sc)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	first, second := coordinatorAwait(t, entered), coordinatorAwait(t, entered)
	if first == second {
		t.Fatal("same path admitted twice")
	}
	appendFile(t, a.Path, extra)
	sc.NotifyWithNotice(a, Notice{Kind: NoticeChanged, ActivityAt: time.Now()})
	sc.Flush(a)
	sc.due(c.Path, &job{spec: c, lane: changedLane})
	sc.mu.Lock()
	newer := sc.ready[a.Path]
	parked := newer != nil && newer.queued == nil && sc.active[a.Path] != nil
	sc.mu.Unlock()
	if !parked {
		t.Fatal("newer same-path capture was indexed while active")
	}
	select {
	case path := <-entered:
		t.Fatalf("third concurrent request: %s", path)
	case <-time.After(30 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	waitFor(t, "all three paths drained", func() bool { return sc.Progress().Queued == 0 })
	tr.mu.Lock()
	maximum, overlap := tr.maximum, tr.overlap
	tr.mu.Unlock()
	if maximum != 2 || overlap {
		t.Fatalf("maximum=%d samepath overlap=%v", maximum, overlap)
	}
	e.requireServerHas(a.Path, fileIDOf(t, a.Path), 0, append(append([]byte{}, initial...), extra...))
	e.requireServerHas(b.Path, fileIDOf(t, b.Path), 0, initial)
	e.requireServerHas(c.Path, fileIDOf(t, c.Path), 0, initial)
}

func TestCoordinatorCancellationCollectsBothAndKeepsNewerCapture(t *testing.T) {
	e := newEnv(t, Config{UploadWorkers: 2, SealAfter: -1}, 1<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{Workers: 2})
	a, b := e.spec("a.jsonl", transcript.StorageJSONLAppend), e.spec("b.jsonl", transcript.StorageJSONLAppend)
	for _, sp := range []SourceSpec{a, b} {
		appendFile(t, sp.Path, jsonlLines(903, 4, 100))
		sc.due(sp.Path, &job{spec: sp, lane: changedLane})
	}
	entered := make(chan string, 2)
	finished := make(chan string, 2)
	tr := &coordinatorTransport{Transport: e.client, active: map[string]int{}, before: func(ctx context.Context, r *syncproto.FlushRequest) error {
		entered <- r.Header.Source.Path
		<-ctx.Done()
		finished <- r.Header.Source.Path
		return ctx.Err()
	}}
	e.sy.tr = tr
	cancel, done := coordinatorStart(t, sc)
	coordinatorAwait(t, entered)
	coordinatorAwait(t, entered)
	newer := a
	newer.Checkout = "/synthetic/newer"
	sc.Notify(newer)
	sc.Flush(newer)
	cancel()
	coordinatorAwait(t, finished)
	coordinatorAwait(t, finished)
	// Preserve the result for cleanup's bounded join.
	result := coordinatorAwait(t, done)
	if !errors.Is(result, context.Canceled) {
		t.Fatalf("Run: %v", result)
	}
	done <- result // cleanup still joins the completed Run
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.running != 0 || len(sc.active) != 0 || sc.ready[a.Path] == nil || sc.ready[b.Path] == nil || sc.ready[a.Path].spec.Checkout != newer.Checkout || sc.ready[a.Path].action != captureSource {
		t.Fatal("cancellation lost admission or latest notification")
	}
}

type coordinatorPermanent struct{}

func (coordinatorPermanent) Error() string   { return "synthetic pin refusal" }
func (coordinatorPermanent) Permanent() bool { return true }

func TestCoordinatorStaleResultsCannotReplacePressure(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{BackoffMin: time.Hour, BackoffMax: time.Hour})
	a, b := &job{spec: e.spec("a.jsonl", transcript.StorageJSONLAppend)}, &job{spec: e.spec("b.jsonl", transcript.StorageJSONLAppend)}
	sc.active[a.spec.Path] = a
	sc.active[b.spec.Path] = b
	sc.running = 2
	busy := &syncproto.HTTPError{Status: 429, RetryAt: time.Now().Add(time.Hour), Body: syncproto.ErrorResponse{Code: "device_busy"}}
	sc.complete(turnResult{ctx: t.Context(), path: a.spec.Path, job: a, err: busy})
	epoch := sc.gateEpoch
	sc.complete(turnResult{ctx: t.Context(), path: b.spec.Path, job: b, err: &syncproto.HTTPError{Status: 503, Body: syncproto.ErrorResponse{Code: "server_busy"}}})
	if sc.gateEpoch != epoch || sc.halted != nil || !sc.pressure || sc.lastErr != busy {
		t.Fatal("stale transient error replaced newer pressure")
	}
}

func TestCoordinatorCapabilityFailureStaysSerialAndRetries(t *testing.T) {
	e := newEnv(t, Config{UploadWorkers: 2}, 1<<20)
	var calls atomic.Int32
	sc := NewScheduler(e.sy, SchedulerConfig{Workers: 2, NegotiatedWorkers: func(context.Context, int) (int, error) {
		if calls.Add(1) == 1 {
			return 0, fmt.Errorf("synthetic unavailable")
		}
		return 2, nil
	}})
	now := time.Now()
	sc.negotiate(t.Context(), now)
	if st := sc.Progress(); st.UploadWorkers != 1 || st.ConcurrencyError == "" || st.Stopped != "" {
		t.Fatalf("serial diagnostic: %+v", st)
	}
	sc.negotiate(t.Context(), now.Add(sc.cfg.RepinEvery))
	if st := sc.Progress(); st.UploadWorkers != 2 || st.ConcurrencyError != "" {
		t.Fatalf("recovered diagnostic: %+v", st)
	}
}

func TestCoordinatorPressureDrainsThenAdmitsOneProbe(t *testing.T) {
	e := newEnv(t, Config{UploadWorkers: 2, SealAfter: -1}, 1<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{Workers: 2, BackoffMin: time.Hour, BackoffMax: time.Hour})
	a, b, c := e.spec("a.jsonl", transcript.StorageJSONLAppend), e.spec("b.jsonl", transcript.StorageJSONLAppend), e.spec("c.jsonl", transcript.StorageJSONLAppend)
	for _, sp := range []SourceSpec{a, b, c} {
		appendFile(t, sp.Path, jsonlLines(904, 4, 100))
	}
	entered := make(chan string, 8)
	fail := make(chan struct{})
	finishOld := make(chan struct{})
	finishProbe := make(chan struct{})
	var phase atomic.Int32
	tr := &coordinatorTransport{Transport: e.client, active: map[string]int{}}
	tr.before = func(ctx context.Context, r *syncproto.FlushRequest) error {
		entered <- r.Header.Source.Path
		var gate <-chan struct{}
		if phase.Load() == 0 && r.Header.Source.Path == a.Path {
			gate = fail
		} else if phase.Load() == 0 {
			gate = finishOld
		} else {
			gate = finishProbe
		}
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
		if gate == fail {
			return &syncproto.HTTPError{Status: 429, Body: syncproto.ErrorResponse{Code: "device_busy"}}
		}
		return nil
	}
	e.sy.tr = tr
	sc.due(a.Path, &job{spec: a, lane: changedLane})
	sc.due(b.Path, &job{spec: b, lane: changedLane})
	coordinatorStart(t, sc)
	coordinatorAwait(t, entered)
	coordinatorAwait(t, entered)
	close(fail)
	waitFor(t, "pressure installed", func() bool { sc.mu.Lock(); defer sc.mu.Unlock(); return sc.pressure })
	sc.Flush(c)
	close(finishOld)
	waitFor(t, "old result collected", func() bool { sc.mu.Lock(); defer sc.mu.Unlock(); return sc.running == 0 })
	sc.mu.Lock()
	retained := sc.pressure && sc.lastErr != nil
	sc.retryAt = time.Now()
	sc.mu.Unlock()
	if !retained {
		t.Fatal("older success cleared pressure")
	}
	phase.Store(1)
	sc.Recheck()
	coordinatorAwait(t, entered)
	select {
	case path := <-entered:
		t.Fatalf("multiple probes admitted: %s", path)
	case <-time.After(30 * time.Millisecond):
	}
	close(finishProbe)
	waitFor(t, "probe recovered and drained", func() bool { return sc.Progress().Queued == 0 })
	sc.mu.Lock()
	pressure := sc.pressure
	sc.mu.Unlock()
	if pressure {
		t.Fatal("successful current probe did not reopen admission")
	}
}

func TestCoordinatorPermanentStopDrainsBeforeRepin(t *testing.T) {
	e := newEnv(t, Config{UploadWorkers: 2, SealAfter: -1}, 1<<20)
	a, b := e.spec("a.jsonl", transcript.StorageJSONLAppend), e.spec("b.jsonl", transcript.StorageJSONLAppend)
	entered := make(chan string, 8)
	fail := make(chan struct{})
	oldFinished := make(chan struct{})
	var recovered atomic.Bool
	var repins atomic.Int32
	sc := NewScheduler(e.sy, SchedulerConfig{Workers: 2, RepinEvery: time.Millisecond, Repin: func() bool {
		repins.Add(1)
		select {
		case <-oldFinished:
			recovered.Store(true)
			return true
		default:
			return false
		}
	}})
	tr := &coordinatorTransport{Transport: e.client, active: map[string]int{}, before: func(ctx context.Context, r *syncproto.FlushRequest) error {
		if recovered.Load() {
			return nil
		}
		entered <- r.Header.Source.Path
		if r.Header.Source.Path == a.Path {
			select {
			case <-fail:
				return coordinatorPermanent{}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		<-ctx.Done()
		close(oldFinished)
		return ctx.Err()
	}}
	e.sy.tr = tr
	for _, sp := range []SourceSpec{a, b} {
		appendFile(t, sp.Path, jsonlLines(905, 4, 100))
		sc.due(sp.Path, &job{spec: sp, lane: changedLane})
	}
	coordinatorStart(t, sc)
	coordinatorAwait(t, entered)
	coordinatorAwait(t, entered)
	if repins.Load() != 0 {
		t.Fatal("repin called before permanent failure")
	}
	close(fail)
	coordinatorAwait(t, oldFinished)
	waitFor(t, "drained repin resumes both", func() bool { return recovered.Load() && sc.Progress().Queued == 0 })
	if repins.Load() != 1 {
		t.Fatalf("repin calls=%d", repins.Load())
	}
}

func TestCoordinatorLegacyClampLastsUntilFreshNegotiation(t *testing.T) {
	e := newEnv(t, Config{UploadWorkers: 2}, 1<<20)
	var calls atomic.Int32
	var failed atomic.Bool
	sc := NewScheduler(e.sy, SchedulerConfig{Workers: 2, NegotiatedWorkers: func(_ context.Context, requested int) (int, error) {
		calls.Add(1)
		if requested != 2 {
			return 0, fmt.Errorf("requested %d instead of configured two", requested)
		}
		if failed.Load() {
			return 0, fmt.Errorf("synthetic capability failure")
		}
		return 2, nil
	}})
	now := time.Now()
	sc.negotiate(t.Context(), now)
	j := &job{spec: e.spec("legacy.jsonl", transcript.StorageJSONLAppend)}
	sc.mu.Lock()
	epoch := sc.gateEpoch
	sc.active[j.spec.Path] = j
	sc.running = 1
	sc.mu.Unlock()
	legacy := &syncproto.HTTPError{Status: 429, Body: syncproto.ErrorResponse{Code: "flush_in_progress"}}
	sc.complete(turnResult{ctx: t.Context(), path: j.spec.Path, job: j, epoch: epoch, err: legacy})
	if st := sc.Progress(); st.UploadWorkers != 1 {
		t.Fatalf("legacy response did not clamp: %+v", st)
	}
	sc.negotiate(t.Context(), now.Add(sc.cfg.RepinEvery/2))
	if calls.Load() != 1 || sc.Progress().UploadWorkers != 1 {
		t.Fatal("clamp reopened before periodic negotiation")
	}
	failed.Store(true)
	sc.negotiate(t.Context(), now.Add(sc.cfg.RepinEvery))
	if st := sc.Progress(); st.UploadWorkers != 1 || st.ConcurrencyError == "" {
		t.Fatalf("failed lookup weakened clamp: %+v", st)
	}
	failed.Store(false)
	lease, err := e.sy.acquireOperation(t.Context(), "synthetic-active-operation")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.release)
	negotiated := make(chan struct{}, 1)
	go func() { sc.negotiate(t.Context(), now.Add(2*sc.cfg.RepinEvery)); negotiated <- struct{}{} }()
	select {
	case <-negotiated:
		t.Fatal("capability epoch changed before operation drained")
	case <-time.After(30 * time.Millisecond):
	}
	if calls.Load() != 2 || sc.Progress().UploadWorkers != 1 {
		t.Fatal("active operation did not retain serial clamp")
	}
	lease.release()
	coordinatorAwait(t, negotiated)
	if st := sc.Progress(); st.UploadWorkers != 2 || st.ConcurrencyError != "" {
		t.Fatalf("fresh capability epoch did not reopen: %+v", st)
	}
}

func TestCoordinatorPermanentAfterPressureStillStops(t *testing.T) {
	e := newEnv(t, Config{UploadWorkers: 2}, 1<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{Workers: 2})
	a, b := &job{spec: e.spec("pressure.jsonl", transcript.StorageJSONLAppend)}, &job{spec: e.spec("refused.jsonl", transcript.StorageJSONLAppend)}
	sc.active[a.spec.Path] = a
	sc.active[b.spec.Path] = b
	sc.running = 2
	sc.complete(turnResult{ctx: t.Context(), path: a.spec.Path, job: a, err: &syncproto.HTTPError{Status: 429, Body: syncproto.ErrorResponse{Code: "device_busy"}}})
	if !sc.complete(turnResult{ctx: t.Context(), path: b.spec.Path, job: b, err: coordinatorPermanent{}}) || sc.halted == nil {
		t.Fatal("pressure epoch suppressed current permanent failure")
	}
	sc.clearPressureLocked(sc.gateEpoch)
	if sc.halted == nil {
		t.Fatal("success reopened permanent stop")
	}
}

func TestCoordinatorLegacyAfterPressureStillClampsSharedAdmission(t *testing.T) {
	e := newEnv(t, Config{UploadWorkers: 2}, 1<<20)
	sc := NewScheduler(e.sy, SchedulerConfig{Workers: 2})
	a, b := &job{spec: e.spec("pressure.jsonl", transcript.StorageJSONLAppend)}, &job{spec: e.spec("legacy.jsonl", transcript.StorageJSONLAppend)}
	sc.active[a.spec.Path] = a
	sc.active[b.spec.Path] = b
	sc.running = 2
	newer := &syncproto.HTTPError{Status: 503, RetryAt: time.Now().Add(time.Hour), Body: syncproto.ErrorResponse{Code: "server_busy"}}
	sc.complete(turnResult{ctx: t.Context(), path: a.spec.Path, job: a, err: newer})
	epoch := sc.gateEpoch
	sc.complete(turnResult{ctx: t.Context(), path: b.spec.Path, job: b, err: &syncproto.HTTPError{Status: 429, Body: syncproto.ErrorResponse{Code: "flush_in_progress"}}})
	if sc.workers != 1 || !sc.serialClamp || sc.gateEpoch != epoch || sc.lastErr != newer {
		t.Fatal("legacy refusal lost or replaced newer cooldown")
	}
	first, err := e.sy.acquireOperation(t.Context(), "synthetic-first")
	if err != nil {
		t.Fatal(err)
	}
	defer first.release()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	second, err := e.sy.acquireOperation(ctx, "synthetic-second")
	if second != nil {
		second.release()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("legacy shared limit admitted second: %v", err)
	}
}

func TestCoordinatorSerialFallbackIncludesPublicOperations(t *testing.T) {
	for _, lookupFails := range []bool{false, true} {
		t.Run(fmt.Sprint(lookupFails), func(t *testing.T) {
			e := newEnv(t, Config{UploadWorkers: 2, SealAfter: -1}, 1<<20)
			sc := NewScheduler(e.sy, SchedulerConfig{Workers: 2, NegotiatedWorkers: func(context.Context, int) (int, error) {
				if lookupFails {
					return 0, fmt.Errorf("synthetic unavailable")
				}
				return 1, nil
			}})
			sc.negotiate(t.Context(), time.Now())
			a, b := e.spec("public.jsonl", transcript.StorageJSONLAppend), e.spec("scheduler.jsonl", transcript.StorageJSONLAppend)
			data := jsonlLines(906, 4, 100)
			appendFile(t, a.Path, data)
			appendFile(t, b.Path, data)
			entered := make(chan string, 4)
			release := make(chan struct{})
			var once sync.Once
			tr := &coordinatorTransport{Transport: e.client, active: map[string]int{}, before: func(ctx context.Context, r *syncproto.FlushRequest) error {
				entered <- r.Header.Source.Path
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}}
			e.sy.tr = tr
			ctx, cancel := context.WithCancel(t.Context())
			publicDone := make(chan error, 1)
			go func() { publicDone <- e.sy.Sync(ctx, a) }()
			t.Cleanup(func() {
				cancel()
				once.Do(func() { close(release) })
				select {
				case <-publicDone:
				case <-time.After(5 * time.Second):
					t.Error("public operation did not join")
				}
			})
			if coordinatorAwait(t, entered) != a.Path {
				t.Fatal("public source did not enter first")
			}
			sc.due(b.Path, &job{spec: b})
			coordinatorStart(t, sc)
			select {
			case path := <-entered:
				t.Fatalf("serial fallback admitted concurrent path: %s", path)
			case <-time.After(30 * time.Millisecond):
			}
			once.Do(func() { close(release) })
			waitFor(t, "shared serial drain", func() bool { return sc.Progress().Queued == 0 })
			result := coordinatorAwait(t, publicDone)
			publicDone <- result
			if result != nil {
				t.Fatal(result)
			}
			tr.mu.Lock()
			maximum := tr.maximum
			tr.mu.Unlock()
			if maximum != 1 {
				t.Fatalf("serial fallback maximum=%d", maximum)
			}
			e.requireServerHas(a.Path, fileIDOf(t, a.Path), 0, data)
			e.requireServerHas(b.Path, fileIDOf(t, b.Path), 0, data)
		})
	}
}
