package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/flopwire/flopwire/internal/api"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/store"
)

type matchingRetrieval struct {
	api.Retrieval
	seen []string
}

func (b *matchingRetrieval) Search(_ context.Context, q format.SearchQuery, _ format.Filters) (*format.Page, error) {
	b.seen = append(b.seen, q.Matching)
	return &format.Page{Hits: []format.Hit{}}, nil
}
func TestSearchMatchingParser(t *testing.T) {
	s := store.NewMemory()
	b := &matchingRetrieval{}
	srv := newServer(t, s, api.Config{Retrieval: b})
	admin := seedAdmin(t, s)
	device := postJSON(t, srv.URL+"/v1/devices", map[string]string{"name": "mac", "platform": "darwin"}, bearer(admin))["token"].(string)
	for _, mode := range []string{"", "auto", "all", "any", "invalid"} {
		res := request(t, "GET", srv.URL+"/v1/search?q=autoreview&matching="+mode, nil, bearer(device))
		status := res.StatusCode
		res.Body.Close()
		want := http.StatusOK
		if mode == "invalid" {
			want = http.StatusBadRequest
		}
		if status != want {
			t.Fatalf("%s: status %d", mode, status)
		}
	}
	if len(b.seen) != 4 || b.seen[0] != "" || b.seen[1] != "auto" || b.seen[2] != "all" || b.seen[3] != "any" {
		t.Fatalf("backend modes: %v", b.seen)
	}
}
