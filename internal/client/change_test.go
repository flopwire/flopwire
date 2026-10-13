package client

import (
	"fmt"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChangeTransport(t *testing.T) {
	hash := strings.Repeat("a", 40)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query()
		if r.Method != "GET" || r.URL.Path != "/v1/change" || v.Get("repo") != "example/repo" || v.Get("commit") != hash || v.Get("cursor") != "opaque" || v.Get("limit") != "20" {
			t.Errorf("request %s", r.URL)
		}
		fmt.Fprint(w, `{"kind":"changes","matches":[],"has_more":false,"coverage":{"visible_index":true}}`)
	}))
	defer srv.Close()
	c := retrievalConfig(t, srv.URL)
	page, err := c.RetrievalAPI().Change(t.Context(), format.ChangeQuery{Commits: []string{hash}, Limit: 20, Cursor: "opaque"}, format.Filters{Repo: "example/repo"})
	if err != nil || page.Kind != "changes" || !page.Coverage.VisibleIndex {
		t.Fatalf("page %#v error %v", page, err)
	}
}
