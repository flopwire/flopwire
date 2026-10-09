package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/flopwire/flopwire/internal/localindex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/syncproto"
)

func awaitSyncTransport[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("sync transport barrier timed out")
		var zero T
		return zero
	}
}
func joinSyncTransport(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("sync transport did not join")
	}
}

func TestSyncWorkersRejectInvalidBeforeOpeningIndex(t *testing.T) {
	for _, n := range []string{"0", "3", "-1"} {
		_, err := runAgent(t.Context(), []string{"--sync-workers=" + n})
		if err == nil || !strings.Contains(err.Error(), "--sync-workers must be 1 or 2") {
			t.Fatalf("workers %s: %v", n, err)
		}
	}
}

func TestSyncConcurrencyStatusShowsSerialFallback(t *testing.T) {
	var out strings.Builder
	printAgentStatus(&out, agent.Response{Sync: &devicesync.Status{UploadWorkers: 1, ConcurrencyError: "synthetic lookup failed"}})
	for _, want := range []string{"upload workers: 1", "synthetic lookup failed", "using one worker; retrying"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("status lacks %q: %s", want, out.String())
		}
	}
}

// A config writer must not pass refresh between loading and installing its
// snapshot. Holding the transport mutex makes that interval deterministic.
func TestSyncRefreshHoldsConfigLockThroughInstall(t *testing.T) {
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	initial := client.Config{Server: "http://localhost:12345", DeviceID: "device", Token: "old"}
	saved := initial
	saved.Token = "new"
	loaded := make(chan struct{})
	tr := newSyncTransport(initial, func() (client.Config, error) { close(loaded); return saved, nil }, quiet)
	tr.mu.Lock()
	released := false
	defer func() {
		if !released {
			tr.mu.Unlock()
		}
	}()
	done := make(chan bool, 1)
	finished := make(chan struct{})
	t.Cleanup(func() { joinSyncTransport(t, finished) })
	go func() { defer close(finished); done <- tr.refresh(t.Context(), initial.Token) }()
	awaitSyncTransport(t, loaded)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	entered := false
	err := client.WithConfigLock(ctx, func() error { entered = true; return nil })
	tr.mu.Unlock()
	released = true
	if !awaitSyncTransport(t, done) {
		t.Fatal("refresh did not install")
	}
	if entered || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("config writer passed installation: entered=%v err=%v", entered, err)
	}
	if tr.current().Token != "new" {
		t.Fatal("new snapshot missing")
	}
}

func TestSyncCapabilityRejectsStaleReply(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable, http.StatusUnauthorized} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			releaseGate := func() { releaseOnce.Do(func() { close(release) }) }
			defer releaseGate()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer old" || r.URL.Query().Get("max_concurrent_flushes") != "2" {
					t.Error("wrong capability snapshot or query")
				}
				close(entered)
				<-release
				w.WriteHeader(status)
				if status == http.StatusOK {
					fmt.Fprintf(w, `{"version":%d,"max_concurrent_flushes":2}`, syncproto.Version)
				}
			}))
			defer func() { releaseGate(); srv.Close() }()
			initial := client.Config{Server: srv.URL, DeviceID: "device", Token: "old"}
			// Saved config remains old: the newer in-memory credential must survive.
			tr := newSyncTransport(initial, func() (client.Config, error) { return initial, nil }, quiet)
			result := make(chan error, 1)
			finished := make(chan struct{})
			t.Cleanup(func() { joinSyncTransport(t, finished) })
			go func() { defer close(finished); _, err := tr.uploadConcurrency(t.Context(), 2); result <- err }()
			awaitSyncTransport(t, entered)
			newer := initial
			newer.Token = "new"
			if !tr.install(newer) {
				t.Fatal("install failed")
			}
			releaseGate()
			err := awaitSyncTransport(t, result)
			var he *syncproto.HTTPError
			if !errors.As(err, &he) || he.Body.Code != "credential_refreshed" || syncproto.Permanent(err) {
				t.Fatalf("stale reply was not retryable: %v", err)
			}
			if tr.current().Token != "new" {
				t.Fatal("stale reply replaced current token")
			}
		})
	}
}

func TestSyncCapabilityCurrentRefusalStops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer srv.Close()
	cfg := client.Config{Server: srv.URL, DeviceID: "device", Token: "refused", FromEnv: true}
	tr := newSyncTransport(cfg, func() (client.Config, error) { return cfg, nil }, quiet)
	_, err := tr.uploadConcurrency(t.Context(), 2)
	if !syncproto.Permanent(err) {
		t.Fatalf("current refusal did not stop: %v", err)
	}
}

// Return a real pin error from the old immutable HTTP client after a new pin
// has been installed. Both upload entry points must retry instead of halting.
type concurrencyRoundTripper func(*http.Request) (*http.Response, error)

func (f concurrencyRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestSyncStalePinFailureDoesNotStopHasOrFlush(t *testing.T) {
	for _, method := range []string{"has", "flush"} {
		t.Run(method, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			releaseGate := func() { releaseOnce.Do(func() { close(release) }) }
			defer releaseGate()
			initial := client.Config{Server: "http://localhost:12345", DeviceID: "device", Token: "same-token"}
			tr := newSyncTransport(initial, func() (client.Config, error) { return initial, nil }, quiet)
			tr.cl.HTTP = &http.Client{Transport: concurrencyRoundTripper(func(r *http.Request) (*http.Response, error) {
				close(entered)
				<-release
				return nil, &client.PinError{Got: "synthetic-old-pin"}
			})}
			done := make(chan error, 1)
			finished := make(chan struct{})
			t.Cleanup(func() { joinSyncTransport(t, finished) })
			go func() {
				defer close(finished)
				var err error
				if method == "has" {
					_, err = tr.Has(t.Context(), nil)
				} else {
					_, err = tr.Flush(t.Context(), &syncproto.FlushRequest{})
				}
				done <- err
			}()
			awaitSyncTransport(t, entered)
			newer := initial
			newer.TLSFingerprint = client.FingerprintPrefix + strings.Repeat("22", 32)
			if !tr.install(newer) {
				t.Fatal("pin install failed")
			}
			releaseGate()
			err := awaitSyncTransport(t, done)
			var he *syncproto.HTTPError
			if !errors.As(err, &he) || he.Body.Code != "credential_refreshed" || syncproto.Permanent(err) {
				t.Fatalf("old pin error stopped %s: %v", method, err)
			}
			if tr.pin != newer.TLSFingerprint {
				t.Fatal("new pin lost")
			}
		})
	}
}

func TestSyncFlushRotationDoesNotReplayConsumedPayload(t *testing.T) {
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	initial := client.Config{Server: srv.URL, DeviceID: "device", Token: "old"}
	saved := initial
	saved.Token = "new"
	tr := newSyncTransport(initial, func() (client.Config, error) { return saved, nil }, quiet)
	payload := strings.NewReader("synthetic consumed payload")
	req := &syncproto.FlushRequest{Header: syncproto.FlushHeader{Tail: &syncproto.Tail{Size: int64(payload.Len())}}, Payload: payload}
	_, err := tr.Flush(t.Context(), req)
	var he *syncproto.HTTPError
	if !errors.As(err, &he) || he.Body.Code != "credential_refreshed" {
		t.Fatalf("rotation result: %v", err)
	}
	if calls.Load() != 1 || payload.Len() != 0 || tr.current().Token != "new" {
		t.Fatalf("consumed payload replayed or rotation lost: calls=%d remaining=%d", calls.Load(), payload.Len())
	}
}

// An error can leave the transport before its scheduler result is published.
// Recover even when the other worker already installed the saved replacement.
func TestSyncReturnedPermanentRecoversAlreadyInstalledCredentials(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(dir, "config.json"))
	store, err := localindex.Open(filepath.Join(dir, "index.db"), localindex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer new" {
			t.Error("recovered request used old token")
		}
		if r.URL.Path == syncproto.PathCapabilities {
			fmt.Fprintf(w, `{"version":%d,"max_concurrent_flushes":2}`, syncproto.Version)
		} else {
			fmt.Fprint(w, `{"missing":[]}`)
		}
	}))
	defer srv.Close()
	initial := client.Config{Server: srv.URL, DeviceID: "device", Token: "old"}
	saved := initial
	sched, tr, start, stop, err := startSyncFrom(t.Context(), store, dir, 1<<20, 2, syncproto.DeviceDirs{}, quiet, initial, func() (client.Config, error) { return saved, nil })
	if err != nil {
		t.Fatal(err)
	}
	tr.cl.HTTP = &http.Client{Transport: concurrencyRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == syncproto.PathCapabilities {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"version":%d,"max_concurrent_flushes":2}`, syncproto.Version)))}, nil
		}
		return nil, &client.PinError{Got: "old-pin"}
	})}
	returned, release := make(chan struct{}), make(chan struct{})
	recovered := make(chan string, 1)
	var releaseOnce sync.Once
	releaseGate := func() { releaseOnce.Do(func() { close(release) }) }
	var calls atomic.Int32
	sched.SetAuthorize(func(ctx context.Context, sp devicesync.SourceSpec) (*devicesync.CaptureAuthorization, error) {
		if calls.Add(1) == 1 {
			_, requestErr := tr.Has(ctx, nil)
			if !syncproto.Permanent(requestErr) {
				t.Error("fixture did not return a current permanent error")
			}
			return &devicesync.CaptureAuthorization{OnError: func(context.Context, error) { close(returned); <-release }}, requestErr
		}
		_, requestErr := tr.Has(ctx, nil)
		if requestErr != nil {
			return nil, requestErr
		}
		select {
		case recovered <- tr.current().Token:
		default:
		}
		return nil, errors.New("synthetic stop after verifying recovered credential")
	})
	path := filepath.Join(dir, "source.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	start()
	defer func() { releaseGate(); stop() }()
	sched.Flush(devicesync.SourceSpec{Path: path})
	awaitSyncTransport(t, returned)
	// The transport has already returned its error; only scheduler publication
	// waits. Install the same-device replacement under the normal config lock.
	if err := client.WithConfigLock(t.Context(), func() error {
		saved.Token = "new"
		saved.TLSFingerprint = client.FingerprintPrefix + strings.Repeat("22", 32)
		if !tr.install(saved) {
			return errors.New("replacement install failed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	releaseGate()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for sched.Progress().Stopped == "" {
		select {
		case <-deadline.C:
			t.Fatal("returned permanent error never reached coordinator")
		case <-tick.C:
		}
	}
	sched.Recheck()
	if token := awaitSyncTransport(t, recovered); token != "new" {
		t.Fatalf("recovery token %q", token)
	}
	if sched.Progress().Stopped != "" {
		t.Fatal("already installed credentials remained halted")
	}
	if tr.repin() {
		t.Fatal("same failure was recovered twice")
	}
}

func TestSyncRepinDoesNotResumeUnchangedPermanentFailure(t *testing.T) {
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	cfg := client.Config{Server: "http://localhost:12345", DeviceID: "device", Token: "same"}
	tr := newSyncTransport(cfg, func() (client.Config, error) { return cfg, nil }, quiet)
	tr.cl.HTTP = &http.Client{Transport: concurrencyRoundTripper(func(r *http.Request) (*http.Response, error) { return nil, &client.PinError{Got: "current-pin"} })}
	_, err := tr.Has(t.Context(), nil)
	if !syncproto.Permanent(err) {
		t.Fatalf("fixture error: %v", err)
	}
	if tr.repin() {
		t.Fatal("unchanged failed credentials resumed")
	}
}
