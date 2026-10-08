package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/syncproto"
)

func admissionOwner(t *testing.T, limit int) *FlushAdmission {
	t.Helper()
	a, err := NewFlushAdmission(limit)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func admissionCount(a *FlushAdmission) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.active)
}

func TestFlushAdmissionLimitsAndDuplicateRelease(t *testing.T) {
	for _, invalid := range []int{-1, 0} {
		if _, err := NewFlushAdmission(invalid); err == nil {
			t.Fatal("invalid limit accepted")
		}
	}
	a := admissionOwner(t, 2)
	first, err := a.acquire("one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.acquire("two")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.acquire("one"); err == nil || err.Code != "flush_in_progress" || err.Status != 429 {
		t.Fatalf("device refusal: %v", err)
	}
	if _, err := a.acquire("three"); err == nil || err.Code != "server_busy" || err.Status != 503 {
		t.Fatalf("global refusal: %v", err)
	}
	first.release()
	next, err := a.acquire("one")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); first.release() }()
	}
	wg.Wait()
	if admissionCount(a) != 2 {
		t.Fatal("duplicate release removed successor")
	}
	if _, err := a.acquire("one"); err == nil {
		t.Fatal("successor no longer owns device")
	}
	next.release()
	second.release()
	if admissionCount(a) != 0 {
		t.Fatal("idle admission entries retained")
	}
}

func TestFlushAdmissionConcurrentCapacity(t *testing.T) {
	a := admissionOwner(t, 3)
	start := make(chan struct{})
	attempts := make(chan *flushTicket, 24)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(device string) {
			defer wg.Done()
			<-start
			ticket, _ := a.acquire(device)
			attempts <- ticket
		}(strings.Repeat("d", i+1))
	}
	close(start)
	wg.Wait()
	close(attempts)
	var admitted []*flushTicket
	for ticket := range attempts {
		if ticket != nil {
			admitted = append(admitted, ticket)
		}
	}
	if len(admitted) != 3 || admissionCount(a) != 3 {
		t.Fatalf("admitted %d, active %d", len(admitted), admissionCount(a))
	}
	for _, ticket := range admitted {
		ticket.release()
	}
	if admissionCount(a) != 0 {
		t.Fatal("entries retained after completion")
	}
}

type admissionBody struct {
	started chan struct{}
	finish  chan error
	once    sync.Once
	reads   atomic.Int64
}

func (b *admissionBody) Read([]byte) (int, error) {
	b.reads.Add(1)
	b.once.Do(func() { close(b.started) })
	return 0, <-b.finish
}
func (b *admissionBody) Close() error { return nil }

func admissionRequest(ctx context.Context, body io.Reader) *http.Request {
	r := httptest.NewRequest(http.MethodPost, syncproto.PathFlush, body).WithContext(ctx)
	r.Header.Set(syncproto.HeaderVersion, "1")
	r.Header.Set("Content-Type", syncproto.FlushContentType)
	return r
}

func waitAdmissionSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("admission barrier not reached")
	}
}

func requireAdmissionRefusal(t *testing.T, s *Server, device string, status int, code string) {
	t.Helper()
	body := &admissionBody{started: make(chan struct{}), finish: make(chan error, 1)}
	w := httptest.NewRecorder()
	s.ServeSync(w, admissionRequest(context.Background(), body), device)
	var response syncproto.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != status || response.Code != code || w.Header().Get("Retry-After") == "" || body.reads.Load() != 0 {
		t.Fatalf("refusal status=%d code=%s retry=%s reads=%d", w.Code, response.Code, w.Header().Get("Retry-After"), body.reads.Load())
	}
}

func TestServeSyncAdmissionBoundsBlockedHeaders(t *testing.T) {
	a := admissionOwner(t, 2)
	// Nil pools prove that admission refusal cannot acquire a database
	// connection. Two Server instances share the same process budget.
	one, two := &Server{Admission: a}, &Server{Admission: a}
	var bodies []*admissionBody
	var finished []chan struct{}
	for _, device := range []string{"one", "two"} {
		body := &admissionBody{started: make(chan struct{}), finish: make(chan error, 1)}
		done := make(chan struct{})
		bodies = append(bodies, body)
		finished = append(finished, done)
		go func() {
			defer close(done)
			one.ServeSync(httptest.NewRecorder(), admissionRequest(context.Background(), body), device)
		}()
		waitAdmissionSignal(t, body.started)
	}
	requireAdmissionRefusal(t, two, "one", 429, "flush_in_progress")
	requireAdmissionRefusal(t, two, "three", 503, "server_busy")
	for i, body := range bodies {
		body.finish <- io.EOF
		waitAdmissionSignal(t, finished[i])
	}
	if admissionCount(a) != 0 {
		t.Fatal("bad headers retained reservations")
	}
	// An accepted malformed header still returns its original bad-request
	// response, and immediately frees the reusable slot.
	w := httptest.NewRecorder()
	two.ServeSync(w, admissionRequest(context.Background(), strings.NewReader("bad")), "three")
	if w.Code != 400 || admissionCount(a) != 0 {
		t.Fatalf("bad-header release: status=%d active=%d", w.Code, admissionCount(a))
	}
}

func TestServeSyncAdmissionCancellationWaitsForActualReadEnd(t *testing.T) {
	for _, cause := range []error{context.Canceled, io.ErrUnexpectedEOF} {
		t.Run(cause.Error(), func(t *testing.T) {
			s := new(Server) // Lazy fallback stays bounded at one.
			body := &admissionBody{started: make(chan struct{}), finish: make(chan error, 1)}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() { defer close(done); s.ServeSync(httptest.NewRecorder(), admissionRequest(ctx, body), "one") }()
			waitAdmissionSignal(t, body.started)
			cancel()
			// Cancellation has fired but the body read still runs. Admission
			// must retain the slot until that read actually returns.
			requireAdmissionRefusal(t, s, "two", 503, "server_busy")
			body.finish <- cause
			waitAdmissionSignal(t, done)
			if admissionCount(s.flushAdmission()) != 0 {
				t.Fatal("read failure retained slot")
			}
			w := httptest.NewRecorder()
			s.ServeSync(w, admissionRequest(context.Background(), strings.NewReader("bad")), "two")
			if w.Code != 400 {
				t.Fatalf("slot not reusable: %d", w.Code)
			}
		})
	}
}

type admissionOverloadedQueue struct{}

func (admissionOverloadedQueue) Notify(string) {}
func (admissionOverloadedQueue) Overloaded() *Error {
	return &Error{http.StatusServiceUnavailable, "parse_backlog", "synthetic parse backlog"}
}

func TestServeSyncAdmissionReleasesAfterFlushRejection(t *testing.T) {
	for _, kind := range []string{"unknown storage", "parse backlog"} {
		t.Run(kind, func(t *testing.T) {
			a := admissionOwner(t, 1)
			s := &Server{Admission: a}
			header := syncproto.FlushHeader{Version: syncproto.Version, Source: syncproto.Source{Path: "/synthetic.jsonl", Agent: "codex", StorageKind: "jsonl_append"}}
			want := http.StatusServiceUnavailable
			if kind == "unknown storage" {
				header.Source.StorageKind = "unknown"
				want = http.StatusBadRequest
			} else {
				s.Queue = admissionOverloadedQueue{}
			}
			var body bytes.Buffer
			if err := syncproto.EncodeFlush(&body, &syncproto.FlushRequest{Header: header, Payload: strings.NewReader("")}); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			s.ServeSync(w, admissionRequest(context.Background(), &body), "one")
			if w.Code != want || admissionCount(a) != 0 {
				t.Fatalf("flush rejection status=%d active=%d", w.Code, admissionCount(a))
			}
			ticket, err := a.acquire("one")
			if err != nil {
				t.Fatal(err)
			}
			ticket.release()
		})
	}
}

func TestServeSyncAdmissionActualDisconnectReleasesSlot(t *testing.T) {
	a := admissionOwner(t, 1)
	s := &Server{Admission: a}
	readStarted := make(chan struct{})
	done := make(chan struct{})
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("stalled") == "1" {
			r.Body = &admissionReadObserver{ReadCloser: r.Body, started: readStarted}
			defer close(done)
		}
		s.ServeSync(w, r, "authenticated-device")
	}))
	defer h.Close()
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	req, err := http.NewRequest(http.MethodPost, h.URL+syncproto.PathFlush+"?stalled=1", reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(syncproto.HeaderVersion, "1")
	req.Header.Set("Content-Type", syncproto.FlushContentType)
	clientDone := make(chan error, 1)
	go func() {
		response, err := h.Client().Do(req)
		if response != nil {
			response.Body.Close()
		}
		clientDone <- err
	}()
	// Send an incomplete prefix so the transport flushes its headers and
	// the server blocks in the real request-body read, not in client setup.
	if _, err := writer.Write([]byte{'F'}); err != nil {
		t.Fatal(err)
	}
	waitAdmissionSignal(t, readStarted)
	requireAdmissionRefusal(t, s, "authenticated-device", 429, "flush_in_progress")
	writer.CloseWithError(errors.New("synthetic client disconnect"))
	waitAdmissionSignal(t, done)
	select {
	case <-clientDone:
	case <-time.After(5 * time.Second):
		t.Fatal("client request did not finish")
	}
	if admissionCount(a) != 0 {
		t.Fatal("disconnect retained reservation")
	}
}

type admissionReadObserver struct {
	io.ReadCloser
	started chan struct{}
	once    sync.Once
}

func (b *admissionReadObserver) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	return b.ReadCloser.Read(p)
}
