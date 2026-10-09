package devicesync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

// These tests preserve one serial Syncer. They do not qualify parallel
// workers, concurrent scratch borrowing, or caller-owned callback mutation.
type operationTransport struct {
	syncproto.Transport
	flush func(context.Context, *syncproto.FlushRequest) (*syncproto.FlushResponse, error)
}

func (tr operationTransport) Flush(ctx context.Context, r *syncproto.FlushRequest) (*syncproto.FlushResponse, error) {
	return tr.flush(ctx, r)
}

type operationCalls struct {
	wg      sync.WaitGroup
	cancels []context.CancelFunc
}

func newOperationCalls(t *testing.T, openBarrier func()) *operationCalls {
	t.Helper()
	calls := new(operationCalls)
	t.Cleanup(func() {
		openBarrier()
		for _, cancel := range calls.cancels {
			cancel()
		}
		calls.wg.Wait()
	})
	return calls
}
func (c *operationCalls) start(ctx context.Context, fn func(context.Context) error) <-chan error {
	ctx, cancel := context.WithCancel(ctx)
	c.cancels = append(c.cancels, cancel)
	result := make(chan error, 1)
	c.wg.Add(1)
	go func() { defer c.wg.Done(); defer cancel(); result <- fn(ctx) }()
	return result
}
func operationSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("operation barrier not reached")
	}
}
func operationResult(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("operation did not finish")
		return nil
	}
}

func TestOperationSerialCallsKeepDistinctAuthorization(t *testing.T) {
	e := newEnv(t, Config{SealAfter: -1}, 1<<20)
	aSpec := authorizedSpec(e, "operation-a.jsonl", transcript.StorageJSONLAppend)
	bSpec := authorizedSpec(e, "operation-b.jsonl", transcript.StorageJSONLAppend)
	aBody := []byte("{\"record\":\"authorized A only\"}\n")
	bBody := []byte("{\"record\":\"authorized B only\"}\n")
	appendFile(t, aSpec.Path, aBody)
	appendFile(t, bSpec.Path, bBody)
	a := fileAuthorization(t, aSpec, int64(len(aBody)))
	b := fileAuthorization(t, bSpec, int64(len(bBody)))
	b.Proof.PolicyRequestDigest = strings.Repeat("b", 64)
	var aChecks, bChecks, bOpens, releases atomic.Int64
	a.Check = func(ctx context.Context) error { aChecks.Add(1); return ctx.Err() }
	a.Release = func() { releases.Add(1) }
	b.Check = func(ctx context.Context) error { bChecks.Add(1); return ctx.Err() }
	b.Release = func() { releases.Add(1) }
	openB := b.Open
	b.Open = func(ctx context.Context, sp SourceSpec) (*os.File, error) {
		bOpens.Add(1)
		if sp.Path != bSpec.Path {
			return nil, errors.New("B opened another operation's source")
		}
		return openB(ctx, sp)
	}
	entered, finish := make(chan struct{}), make(chan struct{})
	var once sync.Once
	calls := newOperationCalls(t, func() { once.Do(func() { close(finish) }) })
	e.sy.tr = operationTransport{Transport: e.client, flush: func(ctx context.Context, r *syncproto.FlushRequest) (*syncproto.FlushResponse, error) {
		if r.Header.Source.Path == aSpec.Path {
			close(entered)
			select {
			case <-finish:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return e.client.Flush(ctx, r)
	}}
	aDone := calls.start(t.Context(), func(ctx context.Context) error { return e.sy.SyncAuthorized(ctx, aSpec, a) })
	operationSignal(t, entered)
	beforeA := aChecks.Load()
	attempting := make(chan struct{})
	bDone := calls.start(t.Context(), func(ctx context.Context) error { close(attempting); return e.sy.SyncAuthorized(ctx, bSpec, b) })
	operationSignal(t, attempting)
	// A's actual materialized request owns the global lock. B cannot enter
	// capture or invoke its policy/file callbacks before this owner releases.
	if e.sy.mu.TryLock() {
		e.sy.mu.Unlock()
		t.Fatal("blocked A transport lost serial owner")
	}
	if bChecks.Load() != 0 || bOpens.Load() != 0 {
		t.Fatal("B entered policy/file work while A owned the Syncer")
	}
	once.Do(func() { close(finish) })
	if err := operationResult(t, aDone); err != nil {
		t.Fatal(err)
	}
	if err := operationResult(t, bDone); err != nil {
		t.Fatal(err)
	}
	if aChecks.Load() != beforeA || bChecks.Load() == 0 || bOpens.Load() == 0 {
		t.Fatalf("lease callbacks crossed operations: A=%d/%d B=%d/%d", beforeA, aChecks.Load(), bChecks.Load(), bOpens.Load())
	}
	if releases.Load() != 0 {
		t.Fatal("public authorized calls released caller-owned leases")
	}
	e.requireServerHas(aSpec.Path, fileID(a.Proof.Identity), 0, aBody)
	e.requireServerHas(bSpec.Path, fileID(b.Proof.Identity), 0, bBody)
	for _, want := range []struct {
		path, digest string
		size         int64
	}{
		{aSpec.Path, strings.Repeat("a", 64), int64(len(aBody))},
		{bSpec.Path, strings.Repeat("b", 64), int64(len(bBody))},
	} {
		var digest string
		var bound int64
		if err := e.store.db.QueryRow(`SELECT json_extract(g.capture_proof,'$.PolicyRequestDigest'),json_extract(g.capture_proof,'$.Offset') FROM devsync_gens g JOIN devsync_sources s ON s.id=g.source_id WHERE s.path=? AND g.generation=0`, want.path).Scan(&digest, &bound); err != nil {
			t.Fatal(err)
		}
		if digest != want.digest || bound != want.size {
			t.Fatalf("operation proof crossed source: digest=%q bound=%d", digest, bound)
		}
	}
}

func TestOperationScratchReusesMatchingFrameAndDiscardsDifferentHash(t *testing.T) {
	for _, matching := range []bool{true, false} {
		t.Run(fmt.Sprintf("matching=%v", matching), func(t *testing.T) {
			cfg := Config{Chunk: ChunkParams{Min: 64, Avg: 128, Max: 256}, MaxRequestBytes: 256, HasThreshold: 1 << 30, SealAfter: -1}
			e := newEnv(t, cfg, 1<<20)
			originalBuffer := &e.sy.serialScratch.buf[0]
			aSpec := authorizedSpec(e, "scratch-a.jsonl", transcript.StorageJSONLAppend)
			aBody := jsonlLines(201, 15, 800)
			appendFile(t, aSpec.Path, aBody)
			a := fileAuthorization(t, aSpec, int64(len(aBody)))
			outcome, err := e.sy.syncTurn(t.Context(), aSpec, nil, -1, a, captureSource)
			if err != nil || outcome != uploadPending {
				t.Fatalf("first bounded capture outcome=%v err=%v", outcome, err)
			}
			deferred := e.sy.serialScratch.deferred
			if len(deferred.z) == 0 {
				t.Fatal("fixture did not force compressed overflow")
			}
			if &e.sy.serialScratch.buf[0] != originalBuffer {
				t.Fatal("capture replaced scan scratch backing")
			}
			targetSpec, targetBody := aSpec, aBody
			action := resumeUpload
			if !matching {
				targetSpec = authorizedSpec(e, "scratch-b.jsonl", transcript.StorageJSONLAppend)
				targetBody = jsonlLines(202, 15, 800)
				appendFile(t, targetSpec.Path, targetBody)
				action = captureSource
			}
			fresh := fileAuthorization(t, targetSpec, int64(len(targetBody)))
			fresh.Proof.PolicyRequestDigest = strings.Repeat("c", 64)
			observed := false
			e.sy.tr = operationTransport{Transport: e.client, flush: func(ctx context.Context, r *syncproto.FlushRequest) (*syncproto.FlushResponse, error) {
				p, ok := r.Payload.(*payload)
				if !ok || len(p.parts) == 0 {
					return nil, errors.New("fixture missing compressed body")
				}
				first := p.parts[0]
				sameHash := first.e.Hash == deferred.e.Hash
				sameFrame := &first.z[0] == &deferred.z[0]
				if sameHash != matching || sameFrame != matching {
					return nil, fmt.Errorf("deferred reuse hash=%v backing=%v matching=%v", sameHash, sameFrame, matching)
				}
				if &e.sy.serialScratch.buf[0] != originalBuffer {
					return nil, errors.New("new operation allocated another scan buffer")
				}
				observed = true
				return e.client.Flush(ctx, r)
			}}
			if _, err := e.sy.syncTurn(t.Context(), targetSpec, nil, -1, fresh, action); err != nil {
				t.Fatal(err)
			}
			if !observed {
				t.Fatal("next operation did not hand over compressed payload")
			}
			e.sy.tr = e.client
			if err := e.sy.ResumeAuthorized(t.Context(), aSpec, fileAuthorization(t, aSpec, int64(len(aBody)))); err != nil {
				t.Fatal(err)
			}
			e.requireServerHas(aSpec.Path, fileID(a.Proof.Identity), 0, aBody)
			if !matching {
				if err := e.sy.ResumeAuthorized(t.Context(), targetSpec, fresh); err != nil {
					t.Fatal(err)
				}
				e.requireServerHas(targetSpec.Path, fileID(fresh.Proof.Identity), 0, targetBody)
			}
			if &e.sy.serialScratch.buf[0] != originalBuffer {
				t.Fatal("draining operations replaced scan backing")
			}
		})
	}
}

func TestOperationSchedulerCancellationAfterMaterializationReleasesLease(t *testing.T) {
	e := newEnv(t, Config{SealAfter: -1, HasThreshold: 1 << 30}, 1<<20)
	sp := authorizedSpec(e, "cancel-operation.jsonl", transcript.StorageJSONLAppend)
	body := []byte("{\"record\":\"cancel after raw materialization\"}\n")
	appendFile(t, sp.Path, body)
	a := fileAuthorization(t, sp, int64(len(body)))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered, finish := make(chan struct{}), make(chan struct{})
	var once sync.Once
	calls := newOperationCalls(t, func() { once.Do(func() { close(finish) }) })
	var payloadFD *os.File
	opens, checks := 0, 0
	baseOpen := a.Open
	a.Open = func(ctx context.Context, s SourceSpec) (*os.File, error) {
		f, err := baseOpen(ctx, s)
		opens++
		if opens == 2 && err == nil {
			payloadFD = f
		}
		return f, err
	}
	a.Check = func(ctx context.Context) error {
		checks++
		if payloadFD != nil {
			close(entered)
			<-finish
			return ctx.Err()
		}
		return ctx.Err()
	}
	var events []string
	a.OnError = func(_ context.Context, err error) {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("lease error=%v", err)
		}
		if !e.sy.mu.TryLock() {
			t.Error("OnError called under Syncer owner")
		} else {
			e.sy.mu.Unlock()
		}
		events = append(events, "error")
	}
	a.Release = func() { events = append(events, "release") }
	sc := NewScheduler(e.sy, SchedulerConfig{})
	done := calls.start(ctx, func(ctx context.Context) error {
		_, err := sc.syncJobTurn(ctx, &job{spec: sp}, -1, false, func(context.Context, SourceSpec) (*CaptureAuthorization, error) { return a, nil })
		return err
	})
	operationSignal(t, entered)
	if _, err := payloadFD.Stat(); err != nil {
		t.Fatal("materialized descriptor already closed", err)
	}
	cancel()
	var acked int64
	var tailAcked bool
	if err := e.store.db.QueryRow(`SELECT acked,tail_acked FROM devsync_gens`).Scan(&acked, &tailAcked); err != nil {
		t.Fatal(err)
	}
	if acked != 0 || tailAcked || e.flushes() != 0 {
		t.Fatal("cancellation barrier advanced ACK or transport")
	}
	once.Do(func() { close(finish) })
	if err := operationResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel result=%v", err)
	}
	if _, err := payloadFD.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("payload descriptor retained: %v", err)
	}
	if strings.Join(events, ",") != "error,release" {
		t.Fatalf("lease callbacks=%v", events)
	}
	oldChecks := checks
	fresh := fileAuthorization(t, sp, int64(len(body)))
	fresh.Proof.PolicyRequestDigest = strings.Repeat("d", 64)
	released := 0
	fresh.Release = func() { released++ }
	outcome, err := sc.syncJobTurn(t.Context(), &job{spec: sp, action: resumeUpload}, -1, false, func(context.Context, SourceSpec) (*CaptureAuthorization, error) { return fresh, nil })
	if err != nil || outcome != syncDone || released != 1 || checks != oldChecks {
		t.Fatalf("fresh lease retry outcome=%v err=%v release=%d oldchecks=%d/%d", outcome, err, released, oldChecks, checks)
	}
	e.requireServerHas(sp.Path, fileID(fresh.Proof.Identity), 0, body)
}

func TestOperationResumeUsesPersistedBoundAndRequiresCompanionDigest(t *testing.T) {
	t.Run("stored bound", func(t *testing.T) {
		e := newEnv(t, Config{SealAfter: -1}, 1<<20)
		sp := authorizedSpec(e, "bound-operation.jsonl", transcript.StorageJSONLAppend)
		body := []byte("{\"record\":\"persisted bound remains authoritative\"}\n")
		appendFile(t, sp.Path, body)
		original := fileAuthorization(t, sp, int64(len(body)))
		e.sy.tr = operationTransport{Transport: e.client, flush: func(context.Context, *syncproto.FlushRequest) (*syncproto.FlushResponse, error) {
			return nil, errors.New("synthetic outage")
		}}
		if err := e.sy.SyncAuthorized(t.Context(), sp, original); err == nil {
			t.Fatal("outage missing")
		}
		fresh := fileAuthorization(t, sp, 0)
		fresh.Proof.PolicyRequestDigest = strings.Repeat("e", 64)
		e.sy.tr = e.client
		if err := e.sy.ResumeAuthorized(t.Context(), sp, fresh); err != nil {
			t.Fatal(err)
		}
		e.requireServerHas(sp.Path, fileID(original.Proof.Identity), 0, body)
		var bound, size int64
		if err := e.store.db.QueryRow(`SELECT json_extract(capture_proof,'$.Offset'),size FROM devsync_gens`).Scan(&bound, &size); err != nil {
			t.Fatal(err)
		}
		if bound != int64(len(body)) || size != int64(len(body)) {
			t.Fatalf("stored proof clipped to current bound=%d size=%d", bound, size)
		}
	})
	t.Run("missing stored companion digest", func(t *testing.T) {
		e := newEnv(t, Config{SealAfter: -1}, 1<<20)
		sp := authorizedSpec(e, "digest-operation.txt", transcript.StorageCompanion)
		body := []byte("independent synthetic companion bytes")
		appendFile(t, sp.Path, body)
		a := fileAuthorization(t, sp, int64(len(body)))
		sum := sha256.Sum256(body)
		a.Proof.ContentSHA = sum[:]
		e.sy.tr = operationTransport{Transport: e.client, flush: func(context.Context, *syncproto.FlushRequest) (*syncproto.FlushResponse, error) {
			return nil, errors.New("synthetic outage")
		}}
		if err := e.sy.SyncAuthorized(t.Context(), sp, a); err == nil {
			t.Fatal("outage missing")
		}
		var proof string
		if err := e.store.db.QueryRow(`SELECT capture_proof FROM devsync_gens`).Scan(&proof); err != nil {
			t.Fatal(err)
		}
		if _, err := e.store.db.Exec(`UPDATE devsync_gens SET capture_proof=json_remove(capture_proof,'$.ContentSHA')`); err != nil {
			t.Fatal(err)
		}
		e.sy.tr = e.client
		if err := e.sy.ResumeAuthorized(t.Context(), sp, a); !errors.Is(err, ErrUnprovenCapture) {
			t.Fatalf("missing historical digest accepted: %v", err)
		}
		if e.flushes() != 0 {
			t.Fatal("invalid historical proof reached network")
		}
		if _, err := e.store.db.Exec(`UPDATE devsync_gens SET capture_proof=?`, proof); err != nil {
			t.Fatal(err)
		}
		if err := e.sy.ResumeAuthorized(t.Context(), sp, a); err != nil {
			t.Fatal(err)
		}
		e.requireServerHas(sp.Path, fileID(a.Proof.Identity), 0, body)
		if !bytes.Equal(a.Proof.ContentSHA, sum[:]) {
			t.Fatal("caller digest mutated")
		}
	})
}
