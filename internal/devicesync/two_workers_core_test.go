package devicesync

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

func coreResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("core operation did not finish")
		return nil
	}
}

// The gate is inside the actual HTTP handler. Each Client.Flush still owns its
// pipe encoder and payload while blocked; cleanup opens the gate before joins.
func coreHTTPGate(t *testing.T, e *env, endpoint string) (<-chan struct{}, func(), *operationCalls) {
	t.Helper()
	entered, release := make(chan struct{}, 8), make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == endpoint {
			entered <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		e.srv.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	e.client.Server, e.client.HTTP = server.URL, server.Client()
	calls := newOperationCalls(t, open)
	return entered, open, calls
}

func TestTwoWorkerCoreActualHTTPOverlapAndPublicPathOwnership(t *testing.T) {
	e := newEnv(t, Config{UploadWorkers: 2, SealAfter: -1}, 1<<20)
	a, b := e.spec("overlap-a.jsonl", transcript.StorageJSONLAppend), e.spec("overlap-b.jsonl", transcript.StorageJSONLAppend)
	bodyA, bodyB := []byte("{\"record\":\"independent A\"}\n"), []byte("{\"record\":\"independent B\"}\n")
	appendFile(t, a.Path, bodyA)
	appendFile(t, b.Path, bodyB)
	idA, idB := fileIDOf(t, a.Path), fileIDOf(t, b.Path)
	entered, open, calls := coreHTTPGate(t, e, syncproto.PathFlush)
	ra := calls.start(t.Context(), func(ctx context.Context) error { return e.sy.Sync(ctx, a) })
	descriptorTestSignal(t, entered)
	rb := calls.start(t.Context(), func(ctx context.Context) error { return e.sy.Sync(ctx, b) })
	descriptorTestSignal(t, entered)
	if !e.sy.mu.TryLock() {
		t.Fatal("transport still owns global state mutex")
	}
	e.sy.mu.Unlock()
	var captures int
	if err := e.store.db.QueryRow(`SELECT COUNT(*) FROM devsync_gens WHERE size>0`).Scan(&captures); err != nil || captures != 2 {
		t.Fatalf("both durable captures missing: %d %v", captures, err)
	}
	e.sy.operations.mu.Lock()
	la, lb := e.sy.operations.active[a.Path], e.sy.operations.active[b.Path]
	distinct := la != nil && lb != nil && la.scratch != lb.scratch
	e.sy.operations.mu.Unlock()
	if !distinct {
		t.Fatal("overlapped paths shared workspace")
	}
	ctx, cancel := context.WithCancel(t.Context())
	sameStarted := make(chan struct{})
	same := calls.start(ctx, func(ctx context.Context) error { close(sameStarted); return e.sy.Resume(ctx, a) })
	<-sameStarted
	cancel()
	if err := coreResult(t, same); !errors.Is(err, context.Canceled) {
		t.Fatalf("same-path waiter=%v", err)
	}
	select {
	case <-entered:
		t.Fatal("same-path public call entered HTTP")
	default:
	}
	open()
	if err := coreResult(t, ra); err != nil {
		t.Fatal(err)
	}
	if err := coreResult(t, rb); err != nil {
		t.Fatal(err)
	}
	e.requireServerHas(a.Path, idA, 0, bodyA)
	e.requireServerHas(b.Path, idB, 0, bodyB)
}

func TestTwoWorkerCoreHasOverlapSharedSpoolAndExactBytes(t *testing.T) {
	e := newEnv(t, Config{UploadWorkers: 2, HasThreshold: 1, SealAfter: -1}, 2<<20)
	a, b := e.spec("shared-a.json", transcript.StorageJSONDoc), e.spec("shared-b.json", transcript.StorageJSONDoc)
	body := jsonlLines(441, 50, 700)
	appendFile(t, a.Path, body)
	appendFile(t, b.Path, body)
	idA, idB := fileIDOf(t, a.Path), fileIDOf(t, b.Path)
	entered, open, calls := coreHTTPGate(t, e, syncproto.PathHas)
	ra := calls.start(t.Context(), func(ctx context.Context) error { return e.sy.Sync(ctx, a) })
	descriptorTestSignal(t, entered)
	rb := calls.start(t.Context(), func(ctx context.Context) error { return e.sy.Sync(ctx, b) })
	descriptorTestSignal(t, entered)
	if !e.sy.mu.TryLock() {
		t.Fatal("Has owns global mutex")
	}
	e.sy.mu.Unlock()
	var shared int
	if err := e.store.db.QueryRow(`SELECT COUNT(*) FROM (SELECT hash FROM devsync_manifest GROUP BY hash HAVING COUNT(DISTINCT source_id)=2)`).Scan(&shared); err != nil || shared == 0 {
		t.Fatalf("no shared durable manifests: %d %v", shared, err)
	}
	open()
	if err := coreResult(t, ra); err != nil {
		t.Fatal(err)
	}
	if err := coreResult(t, rb); err != nil {
		t.Fatal(err)
	}
	e.requireServerHas(a.Path, idA, 0, body)
	e.requireServerHas(b.Path, idB, 0, body)
	if pending, err := e.store.pendingSources(t.Context()); err != nil || len(pending) != 0 {
		t.Fatalf("pending=%d %v", len(pending), err)
	}
}

func TestTwoWorkerCoreDefaultSerialLockAndCanceledWaiter(t *testing.T) {
	e := newEnv(t, Config{SealAfter: -1}, 1<<20)
	if e.sy.cfg.UploadWorkers != 1 || e.sy.operations.slots[0] != &e.sy.serialScratch {
		t.Fatal("default workspace changed")
	}
	a, b := e.spec("serial-a.jsonl", transcript.StorageJSONLAppend), e.spec("serial-b.jsonl", transcript.StorageJSONLAppend)
	body := []byte("{\"record\":\"serial exact bytes\"}\n")
	appendFile(t, a.Path, body)
	appendFile(t, b.Path, body)
	entered, open, calls := coreHTTPGate(t, e, syncproto.PathFlush)
	ra := calls.start(t.Context(), func(ctx context.Context) error { return e.sy.Sync(ctx, a) })
	descriptorTestSignal(t, entered)
	if e.sy.mu.TryLock() {
		e.sy.mu.Unlock()
		t.Fatal("default transport released global mutex")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rb := calls.start(ctx, func(ctx context.Context) error { return e.sy.Sync(ctx, b) })
	if err := coreResult(t, rb); !errors.Is(err, context.Canceled) {
		t.Fatalf("serial waiter=%v", err)
	}
	select {
	case <-entered:
		t.Fatal("serial second request entered")
	default:
	}
	open()
	if err := coreResult(t, ra); err != nil {
		t.Fatal(err)
	}
	e.requireServerHas(a.Path, fileIDOf(t, a.Path), 0, body)
}

func TestTwoWorkerCoreAdmissionCloseJoinsAndReleasesExactSlots(t *testing.T) {
	e := newEnv(t, Config{UploadWorkers: 2}, 1<<20)
	a, err := e.sy.acquireOperation(t.Context(), "A")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.release)
	b, err := e.sy.acquireOperation(t.Context(), "B")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.release)
	if a.scratch == b.scratch || a.scratch != &e.sy.serialScratch {
		t.Fatal("fixed slot ownership incorrect")
	}
	a.scratch.deferred = part{z: []byte("A-owned cached repair")}
	b.scratch.deferred = part{z: []byte("B-owned cached repair")}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := e.sy.acquireOperation(ctx, "C"); !errors.Is(err, context.Canceled) {
		t.Fatalf("third admission=%v", err)
	}
	if !bytes.Equal(a.scratch.deferred.z, []byte("A-owned cached repair")) || !bytes.Equal(b.scratch.deferred.z, []byte("B-owned cached repair")) {
		t.Fatal("workspace repair bytes mixed")
	}
	// Retain a separate concrete file to prove descriptor closure follows real
	// admission release, rather than cancellation or marking admission closed.
	retained := descriptorTestFile(t, e.path("held-close"), []byte("retained until admitted operations finish"))
	if !e.sy.descriptors.retain(99, retained) {
		t.Fatal("retention failed")
	}
	e.sy.operations.mu.Lock()
	changed := e.sy.operations.changed
	e.sy.operations.mu.Unlock()
	done := make(chan struct{})
	go func() { e.sy.Close(); close(done) }()
	descriptorTestSignal(t, changed)
	if _, err := e.sy.acquireOperation(t.Context(), "C"); !errors.Is(err, errOperationsClosed) {
		t.Fatalf("closed admission=%v", err)
	}
	if _, err := retained.Stat(); err != nil {
		t.Fatal("Close released descriptor before operation joins", err)
	}
	a.release()
	a.release()
	select {
	case <-done:
		t.Fatal("Close ignored second active operation")
	default:
	}
	b.release()
	descriptorTestSignal(t, done)
	descriptorTestClosed(t, retained)
	e.sy.operations.mu.Lock()
	count := len(e.sy.operations.active)
	e.sy.operations.mu.Unlock()
	if count != 0 {
		t.Fatal("idle operation entries leaked")
	}
}

func TestTwoWorkerCoreIdleCallbackWaitsForRealRelease(t *testing.T) {
	e := newEnv(t, Config{UploadWorkers: 2}, 1<<20)
	lease, err := e.sy.acquireOperation(t.Context(), "A")
	if err != nil {
		t.Fatal(err)
	}
	var release sync.Once
	t.Cleanup(func() { release.Do(lease.release) })
	called := make(chan struct{})
	result := make(chan error, 1)
	e.sy.operations.mu.Lock()
	changed := e.sy.operations.changed
	e.sy.operations.mu.Unlock()
	go func() { result <- e.sy.withIdleOperations(t.Context(), func() error { close(called); return nil }) }()
	descriptorTestSignal(t, changed)
	e.sy.operations.mu.Lock()
	paused := e.sy.operations.idle
	e.sy.operations.mu.Unlock()
	if !paused {
		t.Fatal("idle callback did not pause admission")
	}

	select {
	case <-called:
		t.Fatal("callback ran before real release")
	default:
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := e.sy.acquireOperation(ctx, "B"); !errors.Is(err, context.Canceled) {
		t.Fatal("idle waiter not cancellable", err)
	}
	release.Do(lease.release)
	if err := coreResult(t, result); err != nil {
		t.Fatal(err)
	}
	descriptorTestSignal(t, called)
	next, err := e.sy.acquireOperation(t.Context(), "B")
	if err != nil {
		t.Fatal("idle callback did not reopen admission", err)
	}
	next.release()
}

// Only transport callbacks panic here; the production handoff must restore the
// caller's mutex even on panic so the enclosing deferred unlock is valid.
type corePanicTransport struct{ syncproto.Transport }

func (corePanicTransport) Has(context.Context, []syncproto.Hash) ([]syncproto.Hash, error) {
	panic("synthetic transport panic")
}
func (corePanicTransport) Flush(context.Context, *syncproto.FlushRequest) (*syncproto.FlushResponse, error) {
	panic("synthetic transport panic")
}
func TestTwoWorkerCoreTransportPanicRestoresCallerMutex(t *testing.T) {
	for _, flush := range []bool{false, true} {
		t.Run(map[bool]string{false: "Has", true: "Flush"}[flush], func(t *testing.T) {
			e := newEnv(t, Config{UploadWorkers: 2}, 1<<20)
			e.sy.tr = corePanicTransport{}
			lease, err := e.sy.acquireOperation(t.Context(), "panic")
			if err != nil {
				t.Fatal(err)
			}
			defer lease.release()
			op := e.sy.operationWithScratch(nil, lease.scratch)
			func() {
				e.sy.mu.Lock()
				defer func() {
					v := recover()
					if v != "synthetic transport panic" {
						t.Errorf("unexpected panic: %v", v)
					}
					if e.sy.mu.TryLock() {
						e.sy.mu.Unlock()
						t.Error("transport did not restore mutex")
					}
					e.sy.mu.Unlock()
				}()
				if flush {
					_, _ = op.transportFlush(t.Context(), &syncproto.FlushRequest{})
				} else {
					_, _ = op.transportHas(t.Context(), nil)
				}
			}()
		})
	}
}

// Done observes the actual cancellable wait (or a regressed SQL/HTTP call).
// Unlike a sleep, this lets the test inspect exact lease identity after the
// contender has entered production code and before canceling it.
type coreWaitContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *coreWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestTwoWorkerCoreSamePathExcludedWithFreeSecondSlot(t *testing.T) {
	e := newEnv(t, Config{UploadWorkers: 2, SealAfter: -1}, 1<<20)
	a, b := e.spec("path-owner-a.jsonl", transcript.StorageJSONLAppend), e.spec("path-owner-b.jsonl", transcript.StorageJSONLAppend)
	oldBody, newBody, bodyB := []byte("{\"record\":\"old inode A\"}\n"), []byte("{\"record\":\"replacement inode A\"}\n"), []byte("{\"record\":\"distinct B\"}\n")
	appendFile(t, a.Path, oldBody)
	appendFile(t, b.Path, bodyB)
	oldID, bID := fileIDOf(t, a.Path), fileIDOf(t, b.Path)
	entered, open, calls := coreHTTPGate(t, e, syncproto.PathFlush)
	ra := calls.start(t.Context(), func(ctx context.Context) error { return e.sy.Sync(ctx, a) })
	descriptorTestSignal(t, entered)
	e.sy.operations.mu.Lock()
	initial := e.sy.operations.active[a.Path]
	occupied := len(e.sy.operations.active)
	e.sy.operations.mu.Unlock()
	if initial == nil || occupied != 1 {
		t.Fatal("fixture must leave second slot free")
	}
	replacement := e.path("path-owner-replacement")
	if err := os.WriteFile(replacement, newBody, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, a.Path); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	observed := &coreWaitContext{Context: ctx, entered: make(chan struct{})}
	same := make(chan error, 1)
	calls.cancels = append(calls.cancels, cancel)
	calls.wg.Add(1)
	go func() { defer calls.wg.Done(); same <- e.sy.Sync(observed, a) }()
	descriptorTestSignal(t, observed.entered)
	e.sy.operations.mu.Lock()
	unchanged := e.sy.operations.active[a.Path] == initial && len(e.sy.operations.active) == 1
	e.sy.operations.mu.Unlock()
	if !unchanged {
		t.Fatal("same path obtained free slot or replaced original lease")
	}
	var generation, size int64
	if err := e.store.db.QueryRow(`SELECT s.generation,g.size FROM devsync_sources s JOIN devsync_gens g ON s.id=g.source_id AND s.generation=g.generation WHERE s.path=?`, a.Path).Scan(&generation, &size); err != nil || generation != 0 || size != int64(len(oldBody)) {
		t.Fatalf("same-path replacement captured early: %d/%d %v", generation, size, err)
	}
	// The blocked path must not reserve or obstruct the otherwise free slot.
	rb := calls.start(t.Context(), func(ctx context.Context) error { return e.sy.Sync(ctx, b) })
	descriptorTestSignal(t, entered)
	e.sy.operations.mu.Lock()
	distinct := e.sy.operations.active[b.Path] != nil && e.sy.operations.active[a.Path] == initial
	e.sy.operations.mu.Unlock()
	if !distinct {
		t.Fatal("same-path waiter blocked distinct-path admission")
	}
	cancel()
	if err := coreResult(t, same); !errors.Is(err, context.Canceled) {
		t.Fatalf("same-path cancellation=%v", err)
	}
	open()
	if err := coreResult(t, ra); err != nil {
		t.Fatal(err)
	}
	if err := coreResult(t, rb); err != nil {
		t.Fatal(err)
	}
	e.requireServerHas(a.Path, oldID, 0, oldBody)
	e.requireServerHas(b.Path, bID, 0, bodyB)
	if err := e.sy.Sync(t.Context(), a); err != nil {
		t.Fatal("replacement failed after previous operation released", err)
	}
	src, err := e.store.source(t.Context(), a.Path, nil)
	if err != nil || src.Gen != 1 {
		t.Fatalf("replacement generation=%v %v", src, err)
	}
	e.requireServerHas(a.Path, fileIDOf(t, a.Path), 1, newBody)
}

func TestTwoWorkerCoreLoweredLimitBlocksFreeSlotUntilDrain(t *testing.T) {
	e := newEnv(t, Config{UploadWorkers: 2}, 1<<20)
	a, err := e.sy.acquireOperation(t.Context(), "A")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.release)
	b, err := e.sy.acquireOperation(t.Context(), "B")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.release)
	if err := e.sy.setOperationLimit(1); err != nil {
		t.Fatal(err)
	}
	a.release() // Slot zero is physically free, but B consumes the new limit.
	ctx, cancel := context.WithCancel(t.Context())
	observed := &coreWaitContext{Context: ctx, entered: make(chan struct{})}
	done := make(chan error, 1)
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	wg.Add(1)
	go func() {
		defer wg.Done()
		lease, err := e.sy.acquireOperation(observed, "C")
		if lease != nil {
			lease.release()
		}
		done <- err
	}()
	select {
	case <-observed.entered:
	case err := <-done:
		t.Fatalf("lowered limit admitted free slot: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("limit waiter did not enter")
	}
	e.sy.operations.mu.Lock()
	held := len(e.sy.operations.active) == 1 && e.sy.operations.active["B"] == b && e.sy.operations.limit == 1
	e.sy.operations.mu.Unlock()
	if !held {
		t.Fatal("limit downgrade changed actual ownership")
	}
	cancel()
	if err := coreResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("downgraded waiter=%v", err)
	}
	b.release()
	c, err := e.sy.acquireOperation(t.Context(), "C")
	if err != nil {
		t.Fatal("serial admission failed after drain", err)
	}
	c.release()
	if err := e.sy.setOperationLimit(2); err == nil {
		t.Fatal("undrained callback-free increase accepted")
	}
	if err := e.sy.withIdleOperations(t.Context(), func() error { return e.sy.setOperationLimit(2) }); err != nil {
		t.Fatal(err)
	}
	c, err = e.sy.acquireOperation(t.Context(), "C")
	if err != nil {
		t.Fatal(err)
	}
	defer c.release()
	d, err := e.sy.acquireOperation(t.Context(), "D")
	if err != nil {
		t.Fatal(err)
	}
	defer d.release()
	if c.scratch == d.scratch {
		t.Fatal("raised drained limit did not restore separate workspaces")
	}
}
