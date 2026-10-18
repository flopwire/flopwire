package client

import (
	"context"
	"encoding/json"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGrepContinuationHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/grep" || r.URL.Query().Get("continue") != "true" || r.URL.Query().Get("cursor") != "opaque" || r.URL.Query().Get("pattern") != "needle" {
			t.Errorf("request: %s", r.URL)
		}
		_ = json.NewEncoder(w).Encode(format.Page{Scan: &format.GrepScan{Status: "more", NextCursor: "next", Checked: 12}})
	}))
	defer srv.Close()
	got, err := (HTTP{Server: srv.URL, Client: srv.Client()}).Grep(context.Background(), format.GrepQuery{Pattern: "needle", Cursor: "opaque"}, format.Filters{})
	if err != nil || got.Scan == nil || got.Scan.NextCursor != "next" || got.Scan.Checked != 12 {
		t.Fatalf("response: %+v %v", got, err)
	}
}
