package ingest

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/syncproto"
)

// These are process-local admission tests, not multi-process source locking
// or qualification for increasing the HTTP per-device upload limit.
func pathAdmissionCount(a *FlushAdmission) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.paths)
}

func pathRequireRefusal(t *testing.T, err error) {
	t.Helper()
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Status != http.StatusTooManyRequests || refusal.Code != "source_busy" {
		t.Fatalf("want source_busy/429, got %v", err)
	}
}

func TestPathAdmissionExactKeysAndStaleRelease(t *testing.T) {
	a := admissionOwner(t, 1)
	for _, key := range [][2]string{{"", "/a"}, {"device", ""}} {
		if ticket, err := a.acquirePath(key[0], key[1]); err == nil || err.Status != 400 || ticket != nil || pathAdmissionCount(a) != 0 {
			t.Fatalf("empty identity allocated: %v %v", ticket, err)
		}
	}
	first, err := a.acquirePath("one", "/a/../b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.acquirePath("one", "/a/../b"); err == nil {
		t.Fatal("duplicate raw key accepted")
	} else {
		pathRequireRefusal(t, err)
	}
	otherDevice, err := a.acquirePath("two", "/a/../b")
	if err != nil {
		t.Fatal(err)
	}
	cleanPath, err := a.acquirePath("one", "/b")
	if err != nil {
		t.Fatal("raw path was normalized", err)
	}
	first.release()
	next, err := a.acquirePath("one", "/a/../b")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); first.release() }()
	}
	wg.Wait()
	if pathAdmissionCount(a) != 3 {
		t.Fatal("stale release removed successor")
	}
	if _, err := a.acquirePath("one", "/a/../b"); err == nil {
		t.Fatal("successor lost ownership")
	} else {
		pathRequireRefusal(t, err)
	}
	next.release()
	otherDevice.release()
	cleanPath.release()
	if pathAdmissionCount(a) != 0 {
		t.Fatal("idle path identities retained")
	}
}

// Every asynchronously started handler has a buffered result and a cleanup
// owner. Cleanup opens the barrier, cancels outstanding IO and joins handlers
// before later fixture cleanups can close PostgreSQL or MinIO resources.
type pathHandlers struct {
	wg      sync.WaitGroup
	cancels []context.CancelFunc
}

func newPathHandlers(t *testing.T, openBarrier func()) *pathHandlers {
	t.Helper()
	handlers := new(pathHandlers)
	t.Cleanup(func() {
		openBarrier()
		for _, cancel := range handlers.cancels {
			cancel()
		}
		handlers.wg.Wait()
	})
	return handlers
}
func (h *pathHandlers) start(ctx context.Context, fn func(context.Context) error) <-chan error {
	ctx, cancel := context.WithCancel(ctx)
	h.cancels = append(h.cancels, cancel)
	result := make(chan error, 1)
	h.wg.Add(1)
	go func() { defer h.wg.Done(); defer cancel(); result <- fn(ctx) }()
	return result
}

type pathQueueBarrier struct {
	entered  chan struct{}
	finish   chan struct{}
	calls    atomic.Int64
	handlers *pathHandlers
}

func (q *pathQueueBarrier) Notify(string) {}
func (q *pathQueueBarrier) Overloaded() *Error {
	q.calls.Add(1)
	q.entered <- struct{}{}
	<-q.finish
	return &Error{http.StatusServiceUnavailable, "parse_backlog", "synthetic barrier"}
}
func pathQueue(t *testing.T) *pathQueueBarrier {
	t.Helper()
	q := &pathQueueBarrier{entered: make(chan struct{}, 4), finish: make(chan struct{})}
	q.handlers = newPathHandlers(t, func() { close(q.finish) })
	return q
}
func pathAwait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("path admission barrier not reached")
	}
}
func pathResult(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("flush did not return")
		return nil
	}
}

func pathHeader(path string) syncproto.FlushHeader {
	return syncproto.FlushHeader{Version: syncproto.Version, Source: syncproto.Source{Path: path, FileID: "1:1", Agent: "codex", StorageKind: "jsonl_append"}}
}

// A payload probe starts after the exactly sized frame header. DecodeFlush
// performs no speculative body read, so refusals can prove zero payload IO.
type pathPayloadProbe struct {
	reader io.Reader
	reads  atomic.Int64
}

func (p *pathPayloadProbe) Read(b []byte) (int, error) { p.reads.Add(1); return p.reader.Read(b) }
func pathWire(t *testing.T, h syncproto.FlushHeader, data []byte) (io.Reader, *pathPayloadProbe) {
	t.Helper()
	if data != nil {
		hash := syncproto.Sum(data)
		h.Entries = []syncproto.Entry{{Ordinal: 0, Hash: hash, Offset: 0, Size: int64(len(data))}}
		h.Bodies = bodyOf(data)
	}
	var b bytes.Buffer
	if err := syncproto.EncodeFlush(&b, &syncproto.FlushRequest{Header: h, Payload: zpayload(data)}); err != nil {
		t.Fatal(err)
	}
	raw := b.Bytes()
	end := 8 + int(binary.BigEndian.Uint32(raw[4:8]))
	probe := &pathPayloadProbe{reader: bytes.NewReader(raw[end:])}
	return io.MultiReader(bytes.NewReader(raw[:end]), probe), probe
}
func pathDecoded(t *testing.T, h syncproto.FlushHeader, data []byte) (*syncproto.FlushHeader, *syncproto.PayloadReader, *pathPayloadProbe) {
	t.Helper()
	wire, probe := pathWire(t, h, data)
	got, pr, err := syncproto.DecodeFlush(wire)
	if err != nil {
		t.Fatal(err)
	}
	if probe.reads.Load() != 0 {
		t.Fatal("header decoding read payload")
	}
	return got, pr, probe
}

func TestPathAdmissionActualFlushIgnoresChangedSourceIdentity(t *testing.T) {
	a := admissionOwner(t, 2)
	q := pathQueue(t)
	s, other := &Server{Admission: a, Queue: q}, &Server{Admission: a, Queue: q}
	h := pathHeader("/same.jsonl")
	done := q.handlers.start(context.Background(), func(ctx context.Context) error {
		_, err := s.Flush(ctx, "device", &h, nil)
		return err
	})
	pathAwait(t, q.entered)
	variants := []syncproto.FlushHeader{h, h, h, h, h}
	variants[0].Source.FileID = "2:2"
	variants[1].Generation = 8
	variants[2].Source.Agent = "claude"
	variants[3].Source.StorageKind = "json_doc"
	variants[4].Source.SessionKey = "other-session"
	for _, v := range variants {
		header, pr, probe := pathDecoded(t, v, []byte("synthetic refused payload\n"))
		conflict := q.handlers.start(context.Background(), func(ctx context.Context) error {
			_, err := other.Flush(ctx, "device", header, pr)
			return err
		})
		pathRequireRefusal(t, pathResult(t, conflict))
		if probe.reads.Load() != 0 || q.calls.Load() != 1 {
			t.Fatal("refusal reached payload or queue")
		}
	}
	// Nil pools in both servers make pre-database refusal mandatory.
	q.finish <- struct{}{}
	if err := pathResult(t, done); err == nil {
		t.Fatal("queue rejection missing")
	}
	if pathAdmissionCount(a) != 0 {
		t.Fatal("queue rejection retained path")
	}
}

func TestPathAdmissionDistinctDirectFlushesOverlap(t *testing.T) {
	for _, other := range []struct{ device, path string }{{"one", "/other"}, {"two", "/same"}} {
		t.Run(other.device+other.path, func(t *testing.T) {
			a := admissionOwner(t, 1)
			q := pathQueue(t)
			s := &Server{Admission: a, Queue: q}
			var results []<-chan error
			for _, key := range []struct{ device, path string }{{"one", "/same"}, other} {
				results = append(results, q.handlers.start(context.Background(), func(ctx context.Context) error {
					h := pathHeader(key.path)
					_, err := s.Flush(ctx, key.device, &h, nil)
					return err
				}))
			}
			pathAwait(t, q.entered)
			pathAwait(t, q.entered)
			if pathAdmissionCount(a) != 2 {
				t.Fatal("distinct direct flushes did not overlap")
			}
			q.finish <- struct{}{}
			q.finish <- struct{}{}
			for _, result := range results {
				if err := pathResult(t, result); err == nil {
					t.Fatal("queue rejection missing")
				}
			}
			if pathAdmissionCount(a) != 0 {
				t.Fatal("completed paths retained")
			}
		})
	}
}

func TestPathAdmissionCancellationWaitsForActualFlushReturn(t *testing.T) {
	a := admissionOwner(t, 1)
	q := pathQueue(t)
	s := &Server{Admission: a, Queue: q}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := pathHeader("/cancel")
	done := q.handlers.start(ctx, func(ctx context.Context) error {
		_, err := s.Flush(ctx, "one", &h, nil)
		return err
	})
	pathAwait(t, q.entered)
	cancel()
	if _, err := a.acquirePath("one", h.Source.Path); err == nil {
		t.Fatal("cancellation released running flush")
	} else {
		pathRequireRefusal(t, err)
	}
	if pathAdmissionCount(a) != 1 {
		t.Fatal("canceled callback lost ownership")
	}
	q.finish <- struct{}{}
	if err := pathResult(t, done); err == nil {
		t.Fatal("queue rejection missing")
	}
	next, err := a.acquirePath("one", h.Source.Path)
	if err != nil {
		t.Fatal(err)
	}
	next.release()
	if pathAdmissionCount(a) != 0 {
		t.Fatal("completed canceled flush retained path")
	}
}

func TestPathAdmissionHTTPRefusalAndOuterDeviceLimit(t *testing.T) {
	a := admissionOwner(t, 2)
	s := &Server{Admission: a}
	ticket, err := a.acquirePath("one", "/busy")
	if err != nil {
		t.Fatal(err)
	}
	defer ticket.release()
	h := pathHeader("/busy")
	h.Source.FileID = "changed"
	wire, probe := pathWire(t, h, []byte("synthetic HTTP refused payload\n"))
	w := httptest.NewRecorder()
	s.ServeSync(w, admissionRequest(context.Background(), wire), "one")
	var result syncproto.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != 429 || result.Code != "source_busy" || w.Header().Get("Retry-After") != "5" || probe.reads.Load() != 0 || admissionCount(a) != 0 {
		t.Fatalf("HTTP refusal status=%d code=%s retry=%q payloadreads=%d outer=%d", w.Code, result.Code, w.Header().Get("Retry-After"), probe.reads.Load(), admissionCount(a))
	}
	// The existing outer device slot rejects a different path before even
	// decoding its header. Path admission does not increase that limit.
	outer, err := a.acquire("one")
	if err != nil {
		t.Fatal(err)
	}
	requireAdmissionRefusal(t, s, "one", 429, "flush_in_progress")
	outer.release()
	ticket.release()
	if admissionCount(a) != 0 || pathAdmissionCount(a) != 0 {
		t.Fatal("HTTP ownership leaked")
	}
}

type pathNotifyBarrier struct {
	entered chan struct{}
	finish  chan struct{}
}

func (*pathNotifyBarrier) Overloaded() *Error { return nil }
func (q *pathNotifyBarrier) Notify(string)    { q.entered <- struct{}{}; <-q.finish }

func TestPathAdmissionDurableFlushRetainsOwnershipThroughNotify(t *testing.T) {
	e := newEnv(t)
	a := admissionOwner(t, 2)
	q := &pathNotifyBarrier{entered: make(chan struct{}, 4), finish: make(chan struct{})}
	handlers := newPathHandlers(t, func() { close(q.finish) })
	s := &Server{Admission: a, Pool: e.pool, Objects: e.objects, Log: e.queue.Log, Queue: q}
	body := []byte("{\"record\":\"durable before notification\"}\n")
	header, pr, _ := pathDecoded(t, pathHeader("/durable-notify.jsonl"), body)
	var firstReply *syncproto.FlushResponse
	done := handlers.start(e.ctx, func(ctx context.Context) error {
		var err error
		firstReply, err = s.Flush(ctx, e.deviceID, header, pr)
		return err
	})
	pathAwait(t, q.entered)
	if e.pool.Stat().AcquiredConns() != 1 || pathAdmissionCount(a) != 1 {
		t.Fatalf("notify lost resources: conns=%d paths=%d", e.pool.Stat().AcquiredConns(), pathAdmissionCount(a))
	}
	// Reconstruct uses a separate connection and reads the committed manifest
	// and MinIO object while the original handler is still in Notify.
	got, err := e.reconstruct(header.Source.Path, "1:1", 0)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("committed bytes %q: %v", got, err)
	}
	if e.pool.Stat().AcquiredConns() != 1 {
		t.Fatal("reconstruction leaked a connection")
	}
	changed := *header
	changed.Source.FileID = "2:2"
	changed.Generation = 1
	next, reader, probe := pathDecoded(t, changed, []byte("refused changed identity\n"))
	conflict := handlers.start(e.ctx, func(ctx context.Context) error {
		_, err := s.Flush(ctx, e.deviceID, next, reader)
		return err
	})
	pathRequireRefusal(t, pathResult(t, conflict))
	if probe.reads.Load() != 0 || e.pool.Stat().AcquiredConns() != 1 {
		t.Fatal("busy path reached payload or acquired another connection")
	}
	q.finish <- struct{}{}
	if err := pathResult(t, done); err != nil || firstReply == nil || firstReply.Status != syncproto.StatusOK || firstReply.AckedEntries != 1 {
		t.Fatalf("flush result %+v %v", firstReply, err)
	}
	if e.pool.Stat().AcquiredConns() != 0 || pathAdmissionCount(a) != 0 {
		t.Fatal("completed flush leaked resources")
	}
	// A completed request can retry the same generation using the actual
	// request path; remove the blocking queue to leave no synthetic waiter.
	s.Queue = nil
	header, pr, _ = pathDecoded(t, pathHeader(header.Source.Path), body)
	if reply, err := s.Flush(e.ctx, e.deviceID, header, pr); err != nil || reply.Status != syncproto.StatusOK || reply.AckedEntries != 1 {
		t.Fatalf("same-path retry %+v %v", reply, err)
	}
	if e.pool.Stat().AcquiredConns() != 0 || pathAdmissionCount(a) != 0 {
		t.Fatal("retry leaked resources")
	}
}

type pathInterruptedReader struct{}

func (pathInterruptedReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestPathAdmissionPayloadFailureAllowsActualRetry(t *testing.T) {
	e := newEnv(t)
	a := admissionOwner(t, 1)
	s := &Server{Admission: a, Pool: e.pool, Objects: e.objects, Log: e.queue.Log}
	body := []byte("{\"record\":\"retry after interrupted payload\"}\n")
	header, pr, probe := pathDecoded(t, pathHeader("/interrupted-path.jsonl"), body)
	probe.reader = pathInterruptedReader{}
	if _, err := s.Flush(e.ctx, e.deviceID, header, pr); err == nil {
		t.Fatal("interrupted payload accepted")
	}
	if probe.reads.Load() == 0 {
		t.Fatal("payload failure not exercised")
	}
	if e.pool.Stat().AcquiredConns() != 0 || pathAdmissionCount(a) != 0 {
		t.Fatal("payload failure leaked ownership or connection")
	}
	header, pr, _ = pathDecoded(t, pathHeader(header.Source.Path), body)
	if reply, err := s.Flush(e.ctx, e.deviceID, header, pr); err != nil || reply.Status != syncproto.StatusOK || reply.AckedEntries != 1 {
		t.Fatalf("retry %+v %v", reply, err)
	}
	got, err := e.reconstruct(header.Source.Path, "1:1", 0)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("retried bytes %q: %v", got, err)
	}
	if e.pool.Stat().AcquiredConns() != 0 || pathAdmissionCount(a) != 0 {
		t.Fatal("retry leaked resources")
	}
}
