package ingest

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/api"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

// throttle sends request bodies at rate bytes per second.
type throttle struct{ rate int }

func (t throttle) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		r = r.Clone(r.Context())
		r.Body = &slowBody{rc: r.Body, rate: t.rate}
	}
	return http.DefaultTransport.RoundTrip(r)
}

type slowBody struct {
	rc   io.ReadCloser
	rate int
}

func (b *slowBody) Read(p []byte) (int, error) {
	p = p[:min(len(p), b.rate/50)]
	n, err := b.rc.Read(p)
	time.Sleep(time.Duration(n) * time.Second / time.Duration(b.rate))
	return n, err
}

func (b *slowBody) Close() error { return b.rc.Close() }

// S9: a device on a link slower than a request's size over the server's
// fixed ReadTimeout (a 4MB request on a link below ~1.1Mbit/s) but above
// the client's MinRate must still get its flush in: the server extends a
// sync request's read (and write) deadline by its declared length at
// MinRate. Scaled down: a 500ms ReadTimeout and WriteTimeout, a 400KB
// request at 256KB/s (about 1.6s).
func TestSlowFlushGetsBodyScaledDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("slow link")
	}
	e := newEnv(t)
	defer func(b time.Duration) { syncReadBase = b }(syncReadBase)
	syncReadBase = 500 * time.Millisecond
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := &Server{Pool: e.pool, Objects: e.objects, Log: log, Queue: e.queue}
	hs := httptest.NewUnstartedServer(api.New(e.store, api.Config{Logger: log, Sync: srv}).Handler(nil))
	hs.Config.ReadTimeout, hs.Config.WriteTimeout = syncReadBase, syncReadBase
	hs.Start()
	defer hs.Close()
	e.client = &syncproto.Client{Server: hs.URL, Token: e.token, HTTP: &http.Client{Transport: throttle{rate: 256 << 10}}}
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	p := filepath.Join(t.TempDir(), "big.jsonl")
	// Bodies travel compressed: base64 noise keeps the request near 400KB.
	var data []byte
	for i := 0; len(data) < 400<<10; i++ {
		data = append(data, `{"type":"x","payload":"`+noise(i, 96)+`"}`+"\n"...)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	sync1(t, sy, devicesync.SourceSpec{Path: p, Agent: transcript.AgentCodex, StorageKind: transcript.StorageJSONLAppend, Parser: "codex@1"})
	if d := time.Since(start); d < time.Second {
		t.Fatalf("the upload took %v; the throttle did not apply", d)
	}
}

// startSyncServer serves the real api and ingest handler with the given
// server Read/WriteTimeout.
func startSyncServer(t *testing.T, e *env, timeout time.Duration) *httptest.Server {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := &Server{Pool: e.pool, Objects: e.objects, Log: log, Queue: e.queue}
	hs := httptest.NewUnstartedServer(api.New(e.store, api.Config{Logger: log, Sync: srv}).Handler(nil))
	hs.Config.ReadTimeout, hs.Config.WriteTimeout = timeout, timeout
	hs.Start()
	t.Cleanup(hs.Close)
	return hs
}

// stalledFlush sends a flush's headers declaring length bytes of body,
// and no body.
func stalledFlush(t *testing.T, hs *httptest.Server, token string, length int64) net.Conn {
	c, err := net.Dial("tcp", hs.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\n%s: %d\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n",
		syncproto.PathFlush, token, syncproto.HeaderVersion, syncproto.Version, syncproto.FlushContentType, length)
	return c
}

// A declared body length past what a device really sends in one request
// buys no more read time: an 80MB declaration gets the deadline of the
// 5MB cap, not 21 minutes. Scaled: 1MB/s, so the cap is 5s and the
// uncapped deadline 80s.
func TestFlushDeadlineCappedAtRequestSize(t *testing.T) {
	e := newEnv(t)
	defer func(b time.Duration, r int64) { syncReadBase, syncMinRate = b, r }(syncReadBase, syncMinRate)
	syncReadBase, syncMinRate = 200*time.Millisecond, 1<<20
	hs := startSyncServer(t, e, syncReadBase)
	c := stalledFlush(t, hs, e.token, MaxFlushBytes)
	start := time.Now()
	_ = c.SetReadDeadline(start.Add(20 * time.Second))
	res, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err == nil {
		res.Body.Close()
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("the server still held the request after %v", time.Since(start))
	}
	if d := time.Since(start); d < 4*time.Second || d > 10*time.Second {
		t.Fatalf("request ended after %v, want about 5s", d)
	}
}

// One flush per device at a time: a second concurrent flush is refused
// with 429 and Retry-After, and a flush after the first ends goes in.
func TestSecondConcurrentFlushOfDeviceRefused(t *testing.T) {
	e := newEnv(t)
	hs := startSyncServer(t, e, 30*time.Second)
	first := stalledFlush(t, hs, e.token, 1<<20)
	e.client = &syncproto.Client{Server: hs.URL, Token: e.token, HTTP: hs.Client()}
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	p := filepath.Join(t.TempDir(), "a.jsonl")
	if err := os.WriteFile(p, []byte(`{"type":"x"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := devicesync.SourceSpec{Path: p, Agent: transcript.AgentCodex, StorageKind: transcript.StorageJSONLAppend, Parser: "codex@1"}
	// The stalled flush reached the handler once a raw flush is refused.
	var res *http.Response
	deadline := time.Now().Add(5 * time.Second)
	for {
		req, _ := http.NewRequest(http.MethodPost, hs.URL+syncproto.PathFlush, strings.NewReader("x"))
		req.Header.Set("Authorization", "Bearer "+e.token)
		req.Header.Set(syncproto.HeaderVersion, strconv.Itoa(syncproto.Version))
		req.Header.Set("Content-Type", syncproto.FlushContentType)
		var err error
		if res, err = hs.Client().Do(req); err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode == http.StatusTooManyRequests || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "" {
		t.Fatalf("second flush: %d, Retry-After %q", res.StatusCode, res.Header.Get("Retry-After"))
	}
	if err := sy.Sync(e.ctx, spec); !syncproto.Retryable(err) {
		t.Fatalf("a refused flush must be retryable: %v", err)
	}
	first.Close()
	for err := sy.Sync(e.ctx, spec); err != nil; err = sy.Sync(e.ctx, spec) {
		if time.Now().After(deadline.Add(5 * time.Second)) {
			t.Fatalf("flush after the first ended: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
