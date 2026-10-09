package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/syncproto"
)

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
	go func() { done <- tr.refresh(t.Context(), initial.Token) }()
	<-loaded
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	entered := false
	err := client.WithConfigLock(ctx, func() error { entered = true; return nil })
	tr.mu.Unlock()
	released = true
	if !<-done {
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
			defer srv.Close()
			initial := client.Config{Server: srv.URL, DeviceID: "device", Token: "old"}
			// Saved config remains old: the newer in-memory credential must survive.
			tr := newSyncTransport(initial, func() (client.Config, error) { return initial, nil }, quiet)
			result := make(chan error, 1)
			go func() { _, err := tr.uploadConcurrency(t.Context(), 2); result <- err }()
			<-entered
			newer := initial
			newer.Token = "new"
			if !tr.install(newer) {
				t.Fatal("install failed")
			}
			close(release)
			err := <-result
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
			initial := client.Config{Server: "http://localhost:12345", DeviceID: "device", Token: "same-token"}
			tr := newSyncTransport(initial, func() (client.Config, error) { return initial, nil }, quiet)
			tr.cl.HTTP = &http.Client{Transport: concurrencyRoundTripper(func(r *http.Request) (*http.Response, error) {
				close(entered)
				<-release
				return nil, &client.PinError{Got: "synthetic-old-pin"}
			})}
			done := make(chan error, 1)
			go func() {
				var err error
				if method == "has" {
					_, err = tr.Has(t.Context(), nil)
				} else {
					_, err = tr.Flush(t.Context(), &syncproto.FlushRequest{})
				}
				done <- err
			}()
			<-entered
			newer := initial
			newer.TLSFingerprint = client.FingerprintPrefix + strings.Repeat("22", 32)
			if !tr.install(newer) {
				t.Fatal("pin install failed")
			}
			close(release)
			err := <-done
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
