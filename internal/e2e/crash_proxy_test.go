package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"sync"
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
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
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
		http.Error(w, "test proxy upstream failure", http.StatusBadGateway)
	}
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
		} else if r := event.Response; r.Version != syncproto.Version || (r.Status != syncproto.StatusOK && r.Status != syncproto.StatusPartial) || r.AckedEntries <= 0 || r.AckedOffset <= 0 || r.AckedOffset >= p.size || r.Generation != h.Generation {
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
			if h.Source.Path == p.path {
				r = r.WithContext(context.WithValue(r.Context(), crashHeaderKey{}, &crashRequest{header: h, ctx: r.Context()}))
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	return p, nil
}

type replayBody struct {
	io.Reader
	original io.Closer
}

func (b *replayBody) Close() error        { return b.original.Close() }
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
