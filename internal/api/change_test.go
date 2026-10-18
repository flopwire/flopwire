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

	// Expanded scope arrives without repo after device-side resolution. It must
	// retain authenticated owner/device restrictions and the exact scope values.
	owner, callerDevice := b.f.Owner, b.f.CallerDevice
	expanded := url.Values{"repo_root": {"/src/repo"}, "repo_checkout": {"/src/repo"}, "repo_remote": {"github.com/example/repo"}, "commit": {hash}}
	res = request(t, "GET", srv.URL+"/v1/change?"+expanded.Encode(), nil, bearer(device))
	if res.StatusCode != 200 {
		t.Fatalf("expanded status %d: %s", res.StatusCode, read(res))
	}
	res.Body.Close()
	if b.f.Repo != "" || len(b.f.RepoRoots) != 1 || b.f.RepoRoots[0] != "/src/repo" || len(b.f.RepoMains) != 1 || b.f.RepoMains[0] != "/src/repo" || len(b.f.RepoRemotes) != 1 || b.f.RepoRemotes[0] != "github.com/example/repo" || b.f.Owner != owner || b.f.CallerDevice != callerDevice {
		t.Fatalf("expanded scope/auth changed: %#v", b.f)
	}
	for _, suffix := range []string{"?repo=r&commit=abc", "?repo_root=relative&commit=" + hash, "?repo_remote=/invalid&commit=" + hash, "?commit=" + hash, "?repo=r&commit=" + hash + "&limit=101"} {
		res := request(t, "GET", srv.URL+"/v1/change"+suffix, nil, bearer(device))
		if res.StatusCode != 400 {
			t.Errorf("%s: %d", suffix, res.StatusCode)
		}
		res.Body.Close()
	}
}
