package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func retrievalConfig(t *testing.T, server string) Config {
	t.Helper()
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv(EnvToken, "")
	c := Config{Server: server, Token: "old", DeviceID: "device"}
	if err := Save(c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRetrievalFollowsRotationAndLogin(t *testing.T) {
	var token atomic.Value
	token.Store("old")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+token.Load().(string) {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"code":"credential_revoked"}`)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	c := retrievalConfig(t, srv.URL)
	api := c.RetrievalAPI()
	for _, next := range []string{"old", "rotated", "login"} {
		c.Token = next
		if err := Save(c); err != nil {
			t.Fatal(err)
		}
		token.Store(next)
		before := calls.Load()
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				if err := api.JSON(t.Context(), "GET", "/v1/sessions", nil, nil); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		if calls.Load()-before != 8 {
			t.Fatal("fresh credential should avoid 401s")
		}
	}
	token.Store("revoked")
	before := calls.Load()
	err := api.JSON(t.Context(), "GET", "/v1/sessions", nil, nil)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != "credential_revoked" || !strings.Contains(err.Error(), "flopwire login") || calls.Load()-before != 1 {
		t.Fatalf("revoked credential should stop once with recovery: %v", err)
	}
}

func TestRetrievalWaitsForRotationCommitSave(t *testing.T) {
	refused := make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") == "Bearer old" {
			close(refused)
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	c := retrievalConfig(t, srv.URL)
	api := c.RetrievalAPI()
	locked := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- WithConfigLock(t.Context(), func() error {
			close(locked)
			select {
			case <-refused:
			case <-t.Context().Done():
				return t.Context().Err()
			}
			c.Token = "new"
			return Save(c)
		})
	}()
	<-locked
	if err := api.JSON(t.Context(), "GET", "/v1/sessions", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("requests=%d, want one retry", calls.Load())
	}
}

func TestRetrievalRetryIsBounded(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(401)
	}))
	defer srv.Close()
	c := retrievalConfig(t, srv.URL)
	api := c.RetrievalAPI()
	tr := api.Client.Transport.(*retrievalTransport)
	tr.lock = func(ctx context.Context, fn func() error) error {
		c.Token = "new"
		if err := Save(c); err != nil {
			return err
		}
		return fn()
	}
	if err := api.JSON(t.Context(), "GET", "/v1/sessions", nil, nil); err == nil || calls.Load() != 2 {
		t.Fatalf("repeated refusal must stop after retry: %v, calls=%d", err, calls.Load())
	}
}

func TestRetrievalDoesNotSwitchIdentityOrFallbackFromEnv(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer minted" {
			t.Error("environment credential replaced")
		}
		w.WriteHeader(401)
	}))
	defer srv.Close()
	c := retrievalConfig(t, srv.URL)
	api := c.RetrievalAPI()
	for _, field := range []string{"server", "device"} {
		other := c
		if field == "server" {
			other.Server = "https://elsewhere.example"
		} else {
			other.DeviceID = "other"
		}
		if err := Save(other); err != nil {
			t.Fatal(err)
		}
		if err := api.JSON(t.Context(), "GET", "/v1/sessions", nil, nil); err == nil || !strings.Contains(err.Error(), "restart") {
			t.Fatalf("identity change: %v", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("sent credentials after identity change")
	}
	c.Token, c.FromEnv = "minted", true
	err := c.RetrievalAPI().JSON(t.Context(), "GET", "/v1/sessions", nil, nil)
	if err == nil || calls.Load() != 1 {
		t.Fatalf("env refusal: %v, calls=%d", err, calls.Load())
	}
}

func TestRetrievalRejectsCrossServerRedirectAndWrites(t *testing.T) {
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, 302) }))
	defer srv.Close()
	api := retrievalConfig(t, srv.URL).RetrievalAPI()
	if err := api.JSON(t.Context(), "GET", "/v1/sessions", nil, nil); err == nil || leaked.Load() {
		t.Fatalf("redirect: %v, leaked=%v", err, leaked.Load())
	}
	if err := api.JSON(t.Context(), "POST", "/write", nil, nil); err == nil {
		t.Fatal("retrieval transport accepted a write")
	}
}

func TestRetrievalFollowsSavedPin(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) }))
	defer srv.Close()
	c := retrievalConfig(t, srv.URL)
	c.TLSFingerprint = Fingerprint(srv.Certificate().Raw)
	if err := Save(c); err != nil {
		t.Fatal(err)
	}
	api := c.RetrievalAPI()
	if err := api.JSON(t.Context(), "GET", "/v1/sessions", nil, nil); err != nil {
		t.Fatal(err)
	}
	c.TLSFingerprint = FingerprintPrefix + strings.Repeat("0", 64)
	if err := Save(c); err != nil {
		t.Fatal(err)
	}
	var pinErr *PinError
	if err := api.JSON(t.Context(), "GET", "/v1/sessions", nil, nil); !errors.As(err, &pinErr) {
		t.Fatalf("saved pin ignored: %v", err)
	}
	c.TLSFingerprint = Fingerprint(srv.Certificate().Raw)
	if err := Save(c); err != nil {
		t.Fatal(err)
	}
	if err := api.JSON(t.Context(), "GET", "/v1/sessions", nil, nil); err != nil {
		t.Fatal(err)
	}
}
