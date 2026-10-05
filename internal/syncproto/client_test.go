package syncproto

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A Client without HTTP fails instead of falling back to an unpinned
// default client, and sends nothing.
func TestClientRequiresHTTP(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer srv.Close()
	c := &Client{Server: srv.URL, Token: "tok"}
	if _, err := c.Has(context.Background(), []Hash{{}}); !errors.Is(err, ErrNoHTTPClient) {
		t.Fatalf("Has: %v", err)
	}
	if _, err := c.Flush(context.Background(), &FlushRequest{Header: FlushHeader{Version: Version}}); !errors.Is(err, ErrNoHTTPClient) {
		t.Fatalf("Flush: %v", err)
	}
	if hit || Retryable(ErrNoHTTPClient) {
		t.Fatalf("request sent %v, retryable %v", hit, Retryable(ErrNoHTTPClient))
	}
}

func TestClientPreservesRetryAfter(t *testing.T) {
	for _, status := range []int{429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(status)
				w.Write([]byte(`{"code":"flush_in_progress"}`))
			}))
			defer srv.Close()
			c := &Client{Server: srv.URL, HTTP: srv.Client()}
			before := time.Now()
			_, err := c.Has(context.Background(), nil)
			var he *HTTPError
			if !errors.As(err, &he) || he.RetryAt.Before(before.Add(time.Minute)) || !Busy(err) || !Retryable(err) {
				t.Fatalf("retry metadata lost: %v", err)
			}
		})
	}
}

func TestRetryAfterFormats(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	for _, value := range []string{"60", now.Add(time.Minute).Format(http.TimeFormat)} {
		if got := retryAfter(value, now); !got.Equal(now.Add(time.Minute)) {
			t.Fatalf("%q: %v", value, got)
		}
	}
	for _, value := range []string{"", "nonsense", "-1", "18446744073709551615", now.Add(-time.Minute).Format(http.TimeFormat)} {
		if got := retryAfter(value, now); !got.IsZero() {
			t.Fatalf("%q: %v", value, got)
		}
	}
}

func TestBusyClassificationWithoutRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		busy   bool
	}{
		{429, "flush_in_progress", true}, {503, "flush_in_progress", true},
		{503, "parse_backlog", true}, {503, "server_busy", true},
		{503, "object_store_unavailable", false}, {503, "unavailable", false},
		{400, "server_busy", false},
	} {
		if got := Busy(&HTTPError{Status: tc.status, Body: ErrorResponse{Code: tc.code}}); got != tc.busy {
			t.Errorf("%d %s: busy=%v, want %v", tc.status, tc.code, got, tc.busy)
		}
	}
}
