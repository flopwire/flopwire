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
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/syncproto"
)

type crashBoundary struct {
	Header   syncproto.FlushHeader
	Response syncproto.FlushResponse
	Err      error
}

// crashProxy holds one real committed flush response before the agent can
// acknowledge it locally. It exists only in tests and streams request payloads.
type crashProxy struct {
	server  *httptest.Server
	path    string
	size    int64
	reached chan crashBoundary
	release chan struct{}
	once    sync.Once
	claimed bool
	mu      sync.Mutex
	cancel  context.CancelFunc
}

type crashHeaderKey struct{}
type crashRequest struct {
	header *syncproto.FlushHeader
	ctx    context.Context
	size   int64
}

func newCrashProxy(upstream, pin, path string, size int64) (*crashProxy, error) {
	target, err := url.Parse(upstream)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &crashProxy{path: path, size: size, reached: make(chan crashBoundary, 1), release: make(chan struct{}), cancel: cancel}
	hc := client.NewHTTPClient(pin)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = hc.Transport
	proxy.ErrorHandler = p.handleError
	wait := func(requestCtx context.Context) error {
		select {
		case <-p.release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-requestCtx.Done():
			return requestCtx.Err()
		case <-time.After(60 * time.Second):
			return fmt.Errorf("crash response gate timed out")
		}
	}
	proxy.ModifyResponse = func(res *http.Response) error {
		match, ok := res.Request.Context().Value(crashHeaderKey{}).(*crashRequest)
		if !ok {
			return nil
		}
		h := match.header
		p.mu.Lock()
		claim := !p.claimed
		if claim {
			p.claimed = true
		}
		p.mu.Unlock()
		if !claim {
			return wait(match.ctx)
		}
		event := crashBoundary{Header: *h}
		raw, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
		res.Body.Close()
		res.Body = io.NopCloser(bytes.NewReader(raw))
		if err != nil {
			event.Err = err
		} else if len(raw) > 1<<20 {
			event.Err = fmt.Errorf("flush response exceeds 1 MiB")
		} else if res.StatusCode != http.StatusOK {
			event.Err = fmt.Errorf("flush HTTP status %d", res.StatusCode)
		} else if err := json.Unmarshal(raw, &event.Response); err != nil {
			event.Err = err
		} else if r := event.Response; r.Version != syncproto.Version || (r.Status != syncproto.StatusOK && r.Status != syncproto.StatusPartial) || r.AckedEntries <= 0 || r.AckedOffset <= 0 || r.AckedOffset >= match.size || r.Generation != h.Generation {
			event.Err = fmt.Errorf("missed partial commit boundary: %+v", r)
		}
		p.reached <- event
		if event.Err != nil {
			return event.Err
		}
		return wait(match.ctx)
	}
	p.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == syncproto.PathFlush {
			var prefix bytes.Buffer
			h, _, err := syncproto.DecodeFlush(io.TeeReader(r.Body, &prefix))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			r.Body = &replayBody{Reader: io.MultiReader(bytes.NewReader(prefix.Bytes()), r.Body), original: r.Body}
			p.mu.Lock()
			path, size := p.path, p.size
			p.mu.Unlock()
			if path != "" && h.Source.Path == path {
				r = r.WithContext(context.WithValue(r.Context(), crashHeaderKey{}, &crashRequest{header: h, ctx: r.Context(), size: size}))
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	return p, nil
}

func (p *crashProxy) handleError(w http.ResponseWriter, r *http.Request, err error) {
	if match, ok := r.Context().Value(crashHeaderKey{}).(*crashRequest); ok {
		p.mu.Lock()
		claim := !p.claimed
		if claim {
			p.claimed = true
		}
		p.mu.Unlock()
		if claim {
			p.reached <- crashBoundary{Header: *match.header, Err: err}
		}
	}
	// Returning a synthetic 502 after downstream cancellation lets the
	// transport race that response against cancellation. Abort the response
	// instead; upstream and validation failures still produce a visible 502.
	if r.Context().Err() == context.Canceled && errors.Is(err, context.Canceled) {
		panic(http.ErrAbortHandler)
	}
	http.Error(w, "test proxy upstream failure", http.StatusBadGateway)
}

type replayBody struct {
	io.Reader
	original io.Closer
}

func (b *replayBody) Close() error { return b.original.Close() }
func (p *crashProxy) arm(path string, size int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.path, p.size = path, size
}
func (p *crashProxy) disarm() {
	p.mu.Lock()
	p.path = ""
	p.mu.Unlock()
	p.unblock()
}
func (p *crashProxy) unblock()            { p.once.Do(func() { close(p.release) }) }
func (p *crashProxy) close()              { p.unblock(); p.cancel(); p.server.Close() }
func (p *crashProxy) fingerprint() string { return client.Fingerprint(p.server.Certificate().Raw) }

// The agent is stopped while this helper preserves all credential fields.
func setDeviceEndpoint(path, server, pin string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	cfg["server"], _ = json.Marshal(server)
	cfg["tls_fingerprint"], _ = json.Marshal(pin)
	raw, err = json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0600)
}

// The same pinned endpoint forwards normally before arming, holds only the
// selected source at the crash boundary, and forwards again after disarming.
func TestCrashProxyStableEndpointGate(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, _, err := syncproto.DecodeFlush(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		json.NewEncoder(w).Encode(syncproto.FlushResponse{Version: syncproto.Version, Status: syncproto.StatusPartial, Generation: h.Generation, AckedEntries: 1, AckedOffset: 1})
	}))
	defer upstream.Close()
	gate, err := newCrashProxy(upstream.URL, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.close()
	endpoint, pin := gate.server.URL, gate.fingerprint()
	hc := client.NewHTTPClient(pin)
	call := func(path string) error {
		var frame bytes.Buffer
		header := syncproto.FlushHeader{Version: syncproto.Version, Source: syncproto.Source{Path: path, FileID: "test", Agent: "codex", StorageKind: "jsonl_append"}, CapturedAt: time.Now(), Chunker: syncproto.ChunkerParams{Algorithm: "fastcdc-v1.0.0", Min: 1, Avg: 2, Max: 4}}
		if err := syncproto.EncodeFlush(&frame, &syncproto.FlushRequest{Header: header, Payload: bytes.NewReader(nil)}); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+syncproto.PathFlush, &frame)
		if err != nil {
			return err
		}
		res, err := hc.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		io.Copy(io.Discard, res.Body)
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("flush status %d", res.StatusCode)
		}
		return nil
	}
	if err := call("/target"); err != nil {
		t.Fatal(err)
	}
	gate.arm("/target", 10)
	done := make(chan error, 1)
	go func() { done <- call("/target") }()
	select {
	case event := <-gate.reached:
		if event.Err != nil {
			t.Fatal(event.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("armed gate did not observe commit")
	}
	select {
	case err := <-done:
		t.Fatalf("response escaped armed gate: %v", err)
	default:
	}
	if err := call("/other"); err != nil {
		t.Fatal(err)
	}
	gate.disarm()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("disarm did not release response")
	}
	if err := call("/target"); err != nil {
		t.Fatal(err)
	}
	if gate.server.URL != endpoint || gate.fingerprint() != pin {
		t.Fatal("gate changed authority")
	}
}

func TestSetDeviceEndpointPreservesCredentialAndPinsDestination(t *testing.T) {
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("restored")) }))
	defer destination.Close()
	pin := client.Fingerprint(destination.Certificate().Raw)
	path := filepath.Join(t.TempDir(), "restored-client.json")
	original := `{"server":"https://old-proxy.test","tls_fingerprint":"sha256:` + strings.Repeat("0", 64) + `","token":"retained-device-token","device_id":"retained-device","pending_rotation":{"id":"retained-rotation"}}`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if err := setDeviceEndpoint(path, destination.URL, pin); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"server": destination.URL, "tls_fingerprint": pin, "token": "retained-device-token", "device_id": "retained-device"} {
		var got string
		if err := json.Unmarshal(cfg[key], &got); err != nil || got != want {
			t.Fatalf("%s=%q want%q (%v)", key, got, want, err)
		}
	}
	var pending map[string]string
	if err := json.Unmarshal(cfg["pending_rotation"], &pending); err != nil || pending["id"] != "retained-rotation" {
		t.Fatal("endpoint selection changed pending rotation")
	}
	var selected client.Config
	if err := json.Unmarshal(raw, &selected); err != nil {
		t.Fatal(err)
	}
	res, err := client.NewHTTPClient(selected.TLSFingerprint).Get(selected.Server)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("pinned destination status=%d", res.StatusCode)
	}
}
