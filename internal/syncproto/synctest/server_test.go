package synctest

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/flopwire/flopwire/internal/syncproto"
)

func setup(t *testing.T) (*Server, *syncproto.Client) {
	s := New("tok")
	h := httptest.NewServer(s)
	t.Cleanup(h.Close)
	return s, &syncproto.Client{Server: h.URL, Token: "tok", HTTP: h.Client()}
}

func TestConformance(t *testing.T) {
	Conformance(t, func(t *testing.T) Target {
		s, c := setup(t)
		return Target{Client: c, Reconstruct: s.Reconstruct}
	})
}

func TestDownIsRetryable(t *testing.T) {
	s, c := setup(t)
	s.SetDown(true)
	_, err := c.Has(context.Background(), nil)
	if !syncproto.Retryable(err) {
		t.Fatalf("503 should be retryable: %v", err)
	}
	c.Server = "http://127.0.0.1:1"
	if _, err = c.Has(context.Background(), nil); !syncproto.Retryable(err) {
		t.Fatalf("connection refused should be retryable: %v", err)
	}
}
