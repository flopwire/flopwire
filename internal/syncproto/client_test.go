package syncproto

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
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
