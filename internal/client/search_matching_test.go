package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/flopwire/flopwire/internal/retrieval/format"
)

func TestHTTPSearchMatching(t *testing.T) {
	for _, mode := range []string{"", "auto", "all", "any"} {
		t.Run(mode, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				v := r.URL.Query()
				if r.URL.Path != "/v1/search" || v.Get("matching") != mode || v.Get("q") != `can't café "quoted phrase"` || v.Get("offset") != "7" || v.Get("kind") != "assistant" {
					t.Errorf("request: %s", r.URL)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"hits":[]}`))
			}))
			defer srv.Close()
			_, err := (HTTP{Server: srv.URL}).Search(context.Background(), format.SearchQuery{Query: `can't café "quoted phrase"`, Matching: mode, Offset: 7}, format.Filters{Kinds: []string{"assistant"}})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
