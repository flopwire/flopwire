package api_test

import (
	"context"
	"github.com/flopwire/flopwire/internal/api"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/store"
	"net/url"
	"strings"
	"testing"
)

type changeRetrieval struct {
	api.Retrieval
	q      format.ChangeQuery
	f      format.Filters
	budget bool
}

func (b *changeRetrieval) Change(ctx context.Context, q format.ChangeQuery, f format.Filters) (*format.Changes, error) {
	b.q, b.f = q, f
	_, b.budget = ctx.Deadline()
	return &format.Changes{Kind: "changes", Matches: []format.ChangeMatch{}}, nil
}
func TestChangeRoute(t *testing.T) {
	s := store.NewMemory()
	b := &changeRetrieval{}
	srv := newServer(t, s, api.Config{Retrieval: b})
	admin := seedAdmin(t, s)
	device := postJSON(t, srv.URL+"/v1/devices", map[string]string{"name": "mac", "platform": "darwin"}, bearer(admin))["token"].(string)
	hash := strings.Repeat("a", 40)
	res := request(t, "GET", srv.URL+"/v1/change?"+url.Values{"repo": {"example/repo"}, "commit": {hash}}.Encode(), nil, bearer(device))
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status %d: %s", res.StatusCode, read(res))
	}
	if b.q.Limit != 20 || b.f.Owner == "" || b.f.CallerDevice == "" || !b.budget {
		t.Fatalf("query %#v filters %#v budget %v", b.q, b.f, b.budget)
	}
	for _, suffix := range []string{"?repo=r&commit=abc", "?commit=" + hash, "?repo=r&commit=" + hash + "&limit=101"} {
		res := request(t, "GET", srv.URL+"/v1/change"+suffix, nil, bearer(device))
		if res.StatusCode != 400 {
			t.Errorf("%s: %d", suffix, res.StatusCode)
		}
		res.Body.Close()
	}
}
