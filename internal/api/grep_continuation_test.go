package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/api"
	"github.com/flopwire/flopwire/internal/auth"
	"github.com/flopwire/flopwire/internal/domain"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/store"
)

type continuationRequest struct {
	query   format.GrepQuery
	filters format.Filters
}

type continuationRetrieval struct {
	api.Retrieval
	seen  chan continuationRequest
	after func(context.Context) error
}

func (b *continuationRetrieval) Grep(ctx context.Context, q format.GrepQuery, f format.Filters) (*format.Page, error) {
	b.seen <- continuationRequest{q, f}
	if b.after != nil {
		if err := b.after(ctx); err != nil {
			return nil, err
		}
	}
	return &format.Page{Hits: []format.Hit{}, Scan: &format.GrepScan{Status: "complete"}}, nil
}

func TestGrepContinuationRevokedDuringRetrieval(t *testing.T) {
	s := store.NewMemory()
	b := &continuationRetrieval{seen: make(chan continuationRequest, 1)}
	srv := newServer(t, s, api.Config{Retrieval: b})
	admin := seedAdmin(t, s)
	m := newMember(t, srv.URL, admin, "revoked-search@example.test")
	authorizer, err := s.CredentialByTokenHash(context.Background(), auth.HashToken(admin))
	if err != nil {
		t.Fatal(err)
	}
	b.after = func(ctx context.Context) error {
		return s.RevokeDeviceWithAudit(ctx, authorizer.ID, m.deviceID, time.Now(), domain.AuditEvent{Action: "device.revoke"})
	}
	res := request(t, "GET", srv.URL+"/v1/grep?pattern=cobalt&continue=true", nil, bearer(m.device))
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revocation during retrieval: status %d: %s", res.StatusCode, read(res))
	}
}

func TestGrepContinuationAuthenticatedAuthority(t *testing.T) {
	s := store.NewMemory()
	b := &continuationRetrieval{seen: make(chan continuationRequest, 4)}
	srv := newServer(t, s, api.Config{Retrieval: b})
	admin := seedAdmin(t, s)
	m := newMember(t, srv.URL, admin, "continuation@example.test")
	url := srv.URL + "/v1/grep?pattern=cobalt&continue=true&cursor=opaque&authority=spoof&owner=spoof"
	check := func(token string) continuationRequest {
		t.Helper()
		res := request(t, "GET", url, nil, bearer(token))
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status %d: %s", res.StatusCode, read(res))
		}
		return <-b.seen
	}
	first := check(m.device)
	if first.query.Authority != "device:"+m.deviceID || first.filters.Owner != m.userID || first.filters.CallerDevice != m.deviceID || !first.query.Continue || first.query.Cursor != "opaque" {
		t.Fatalf("untrusted continuation identity: %+v", first)
	}
	rotated, _ := rotate(t, srv.URL, m.deviceID, m.device)
	if got := check(rotated); got.query.Authority != first.query.Authority {
		t.Fatal("device rotation changed continuation authority")
	}
	res := request(t, "GET", url, nil, bearer(m.device))
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked credential status %d", res.StatusCode)
	}
	if len(b.seen) != 0 {
		t.Fatal("revoked credential reached continuation backend")
	}
	if got := check(m.session); got.query.Authority == first.query.Authority {
		t.Fatal("login session reused device continuation authority")
	}
}
