package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPErrorDoesNotEchoResponseOrCredential(t *testing.T) {
	const secret = "plaintext-token-from-error"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, secret, http.StatusBadRequest) }))
	defer srv.Close()
	err := (HTTP{Server: srv.URL, Token: secret}).JSON(context.Background(), http.MethodPost, "/failure", map[string]string{"token": secret}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked secret: %v", err)
	}
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.StatusCode != 400 {
		t.Fatalf("error=%#v", err)
	}
}
