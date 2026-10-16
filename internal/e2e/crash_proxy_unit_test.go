package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/syncproto"
)

func smallCrashFrame(t *testing.T, path string) []byte {
	t.Helper()
	body, z := syncproto.EncodeBody([]byte("payload\n"))
	h := syncproto.FlushHeader{Version: syncproto.Version, Source: syncproto.Source{Path: path, FileID: "1:2", Agent: "codex", StorageKind: "jsonl_append"}, CapturedAt: time.Unix(1, 0).UTC(), Chunker: syncproto.ChunkerParams{Algorithm: "fastcdc-v1.0.0", Min: 1, Avg: 2, Max: 4}, Entries: []syncproto.Entry{{Ordinal: 0, Hash: body.Hash, Offset: 0, Size: 8}}, Bodies: []syncproto.Body{body}}
	var b bytes.Buffer
	if err := syncproto.EncodeFlush(&b, &syncproto.FlushRequest{Header: h, Payload: bytes.NewReader(z)}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestCrashProxyStreamsAndWithholds(t *testing.T) {
	raw := smallCrashFrame(t, "/target")
	good := syncproto.FlushResponse{Version: syncproto.Version, Status: syncproto.StatusOK, AckedEntries: 1, AckedOffset: 8}
	committed := make(chan struct{}, 1)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(got, raw) || r.ContentLength != int64(len(raw)) || r.Header.Get("Authorization") != "Bearer test" {
			t.Errorf("forwarded request changed: len=%d content-length=%d error=%v", len(got), r.ContentLength, err)
		}
		committed <- struct{}{}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(good)
	}))
	defer upstream.Close()
	p, err := newCrashProxy(upstream.URL, client.Fingerprint(upstream.Certificate().Raw), "/target", 100)
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	hc := client.NewHTTPClient(p.fingerprint())
	defer hc.CloseIdleConnections()
	req, _ := http.NewRequestWithContext(t.Context(), "POST", p.server.URL+syncproto.PathFlush, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer test")
	done := make(chan error, 1)
	go func() {
		res, err := hc.Do(req)
		if err == nil {
			defer res.Body.Close()
			var got syncproto.FlushResponse
			err = json.NewDecoder(res.Body).Decode(&got)
			if err == nil && (got.AckedOffset != good.AckedOffset || got.AckedEntries != good.AckedEntries) {
				err = io.ErrUnexpectedEOF
			}
		}
		done <- err
	}()
	select {
	case <-committed:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream not reached")
	}
	select {
	case event := <-p.reached:
		if event.Err != nil {
			t.Fatal(event.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("boundary not reached")
	}
	select {
	case err := <-done:
		t.Fatalf("response escaped gate: %v", err)
	default:
	}
	p.unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("release did not forward response")
	}
}

func TestCrashProxyRejectsMissedBoundary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"HTTP failure", 503, `{}`}, {"malformed", 200, `oops`}, {"no progress", 200, `{"version":1,"status":"ok"}`}, {"complete", 200, `{"version":1,"status":"ok","acked_entries":1,"acked_offset":100}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer upstream.Close()
			p, err := newCrashProxy(upstream.URL, client.Fingerprint(upstream.Certificate().Raw), "/target", 100)
			if err != nil {
				t.Fatal(err)
			}
			defer p.close()
			hc := client.NewHTTPClient(p.fingerprint())
			defer hc.CloseIdleConnections()
			res, err := hc.Post(p.server.URL+syncproto.PathFlush, syncproto.FlushContentType, bytes.NewReader(smallCrashFrame(t, "/target")))
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != 502 {
				t.Fatalf("status=%d", res.StatusCode)
			}
			select {
			case event := <-p.reached:
				if event.Err == nil {
					t.Fatal("missed boundary accepted")
				}
				if errors.Is(event.Err, context.Canceled) {
					t.Fatalf("boundary rejection reported cancellation: %v", event.Err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("missing boundary failure")
			}
		})
	}
}

func TestCrashProxyOtherSourceAndCancellation(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, `{"version":1,"status":"ok","acked_entries":1,"acked_offset":8}`)
	}))
	defer upstream.Close()
	p, err := newCrashProxy(upstream.URL, client.Fingerprint(upstream.Certificate().Raw), "/target", 100)
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	hc := client.NewHTTPClient(p.fingerprint())
	defer hc.CloseIdleConnections()
	res, err := hc.Post(p.server.URL+syncproto.PathFlush, syncproto.FlushContentType, bytes.NewReader(smallCrashFrame(t, "/other")))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("other source HTTP status=%d", res.StatusCode)
	}
	select {
	case <-p.reached:
		t.Fatal("other source claimed gate")
	default:
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", p.server.URL+syncproto.PathFlush, bytes.NewReader(smallCrashFrame(t, "/target")))
	type result struct {
		status int
		err    error
	}
	done := make(chan result, 1)
	go func() {
		res, err := hc.Do(req)
		got := result{err: err}
		if res != nil {
			got.status = res.StatusCode
			res.Body.Close()
		}
		done <- got
	}()
	select {
	case event := <-p.reached:
		if event.Err != nil {
			t.Fatalf("cannot test cancellation without a committed boundary: %v (response=%+v)", event.Err, event.Response)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("boundary not reached")
	}
	select {
	case got := <-done:
		t.Fatalf("response escaped gate before cancellation: HTTP status=%d error=%v", got.status, got.err)
	default:
	}
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("cancellation returned HTTP status=%d error=%v", got.status, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not unblock")
	}
	// Closing the server must also finish, even with its gate unreleased.
	closed := make(chan struct{})
	go func() { p.close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy close blocked")
	}
}

func TestCrashProxyPinsAndSingleGate(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, `{"version":1,"status":"ok","acked_entries":1,"acked_offset":8}`)
	}))
	defer upstream.Close()
	p, err := newCrashProxy(upstream.URL, client.Fingerprint(upstream.Certificate().Raw), "/target", 100)
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	wrong := client.NewHTTPClient("sha256:" + strings.Repeat("00", 32))
	defer wrong.CloseIdleConnections()
	if res, err := wrong.Get(p.server.URL); err == nil {
		res.Body.Close()
		t.Fatal("wrong proxy pin accepted")
	}
	hc := client.NewHTTPClient(p.fingerprint())
	defer hc.CloseIdleConnections()
	done := make(chan error, 2)
	raw := smallCrashFrame(t, "/target")
	send := func() {
		res, err := hc.Post(p.server.URL+syncproto.PathFlush, syncproto.FlushContentType, bytes.NewReader(raw))
		if res != nil {
			res.Body.Close()
		}
		done <- err
	}
	go send()
	select {
	case <-p.reached:
	case <-time.After(2 * time.Second):
		t.Fatal("boundary not reached")
	}
	go send()
	p.unblock()
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("request stuck")
		}
	}
	select {
	case <-p.reached:
		t.Fatal("gate claimed twice")
	default:
	}
	// With a wrong upstream pin the request fails before receiving a response.
	bad, err := newCrashProxy(upstream.URL, "sha256:"+strings.Repeat("00", 32), "/target", 100)
	if err != nil {
		t.Fatal(err)
	}
	defer bad.close()
	badClient := client.NewHTTPClient(bad.fingerprint())
	defer badClient.CloseIdleConnections()
	res, err := badClient.Post(bad.server.URL+syncproto.PathFlush, syncproto.FlushContentType, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 502 {
		t.Fatalf("wrong upstream pin status=%d", res.StatusCode)
	}
	select {
	case event := <-bad.reached:
		if event.Err == nil {
			t.Fatal("wrong upstream pin accepted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("missing upstream TLS failure")
	}
}

// Cancellation may race an available HTTP response in net/http. A canceled
// downstream request must not get a synthetic response from the test proxy.
func TestCrashProxyErrorHandlerCanceledDownstream(t *testing.T) {
	for _, tc := range []struct {
		name     string
		canceled bool
		err      error
		abort    bool
	}{
		{"downstream canceled", true, context.Canceled, true},
		{"wrapped downstream canceled", true, fmt.Errorf("gate: %w", context.Canceled), true},
		{"active downstream", false, context.Canceled, false},
		{"unrelated upstream error", true, errors.New("upstream unavailable"), false},
		{"validation error", false, errors.New("invalid committed boundary"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			req := httptest.NewRequest("POST", "https://proxy/flush", nil).WithContext(ctx)
			rr := httptest.NewRecorder()
			p := &crashProxy{}
			var caught any
			func() { defer func() { caught = recover() }(); p.handleError(rr, req, tc.err) }()
			if tc.abort {
				if caught != http.ErrAbortHandler {
					t.Fatalf("panic=%v want ErrAbortHandler; HTTP=%d body=%q", caught, rr.Code, rr.Body.String())
				}
				if rr.Body.Len() != 0 || len(rr.Header()) != 0 {
					t.Fatalf("canceled handler wrote response: headers=%v body=%q", rr.Header(), rr.Body.String())
				}
			} else if caught != nil || rr.Code != http.StatusBadGateway {
				t.Fatalf("upstream/validation error panic=%v HTTP=%d", caught, rr.Code)
			}
		})
	}
}
