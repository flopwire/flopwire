package api_test

// Ported from the CASS-era hardening stack (#6, #8); the upload, search, and
// staging cases went with the segment model.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/api"
	"github.com/flopwire/flopwire/internal/domain"
	"github.com/flopwire/flopwire/internal/store"
)

func TestConcurrentInviteClaimIsAtomic(t *testing.T) {
	storeCases(t, func(t *testing.T, s store.Store) {
		srv := newServer(t, s, api.Config{AuthRate: api.Rate{Burst: 100, Refill: time.Second}})
		adminToken := seedAdmin(t, s)
		invite := postJSON(t, srv.URL+"/v1/admin/invites", map[string]string{"email": "member@example.test", "role": "member"}, bearer(adminToken))
		body := []byte(`{"code":"` + invite["code"].(string) + `","name":"Member","password":"another correct password"}`)
		start := make(chan struct{})
		statuses := make(chan int, 4)
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				res := request(t, http.MethodPost, srv.URL+"/v1/invites/claim", body, map[string]string{"Content-Type": "application/json"})
				statuses <- res.StatusCode
				res.Body.Close()
			}()
		}
		close(start)
		wg.Wait()
		close(statuses)
		created := 0
		for status := range statuses {
			if status == 201 {
				created++
			} else if status != 409 && status != 400 {
				t.Fatalf("unexpected claim status %d", status)
			}
		}
		users, err := s.ListUsers(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if created != 1 || len(users) != 2 {
			t.Fatalf("created=%d users=%d, want one claim and admin plus one member", created, len(users))
		}
	})
}

func TestDeviceRotationPrepareSaveCommitSemantics(t *testing.T) {
	storeCases(t, func(t *testing.T, s store.Store) {
		srv := newServer(t, s, api.Config{})
		adminToken := seedAdmin(t, s)
		device := postJSON(t, srv.URL+"/v1/devices", map[string]string{"name": "admin-mac", "platform": "darwin-arm64"}, bearer(adminToken))
		oldToken := device["token"].(string)
		deviceID := device["device"].(map[string]any)["id"].(string)
		other := postJSON(t, srv.URL+"/v1/devices", map[string]string{"name": "other", "platform": "linux"}, bearer(adminToken))
		cross := request(t, http.MethodPost, srv.URL+"/v1/devices/"+deviceID+"/rotation/prepare", []byte(`{}`), merge(bearer(other["token"].(string)), map[string]string{"Content-Type": "application/json"}))
		if cross.StatusCode != 403 {
			t.Fatalf("cross-device prepare status=%d", cross.StatusCode)
		}
		cross.Body.Close()
		// A second prepare replaces the first; the first commit token dies.
		first := postJSON(t, srv.URL+"/v1/devices/"+deviceID+"/rotation/prepare", map[string]string{}, bearer(oldToken))
		prepared := postJSON(t, srv.URL+"/v1/devices/"+deviceID+"/rotation/prepare", map[string]string{}, bearer(oldToken))
		stale := request(t, http.MethodPost, srv.URL+"/v1/device-rotations/"+first["rotation_id"].(string)+"/commit", []byte(`{"commit_token":"`+first["commit_token"].(string)+`"}`), map[string]string{"Content-Type": "application/json"})
		if stale.StatusCode != 400 || !strings.Contains(read(stale), "rotation_invalid_or_expired") {
			t.Fatalf("stale commit status=%d", stale.StatusCode)
		}
		newToken := prepared["token"].(string)
		before := request(t, http.MethodGet, srv.URL+"/v1/policy", nil, bearer(newToken))
		if before.StatusCode != 401 {
			t.Fatalf("prepared token status=%d, want inactive", before.StatusCode)
		}
		before.Body.Close()
		commitBody := []byte(`{"commit_token":"` + prepared["commit_token"].(string) + `"}`)
		for i := 0; i < 2; i++ {
			res := request(t, http.MethodPost, srv.URL+"/v1/device-rotations/"+prepared["rotation_id"].(string)+"/commit", commitBody, map[string]string{"Content-Type": "application/json"})
			if res.StatusCode != 200 {
				t.Fatalf("commit %d status=%d body=%s", i, res.StatusCode, read(res))
			}
			res.Body.Close()
		}
		old := request(t, http.MethodGet, srv.URL+"/v1/policy", nil, bearer(oldToken))
		if old.StatusCode != 401 {
			t.Fatalf("old token status=%d", old.StatusCode)
		}
		old.Body.Close()
		current := request(t, http.MethodGet, srv.URL+"/v1/policy", nil, bearer(newToken))
		if current.StatusCode != 200 {
			t.Fatalf("new token status=%d body=%s", current.StatusCode, read(current))
		}
		current.Body.Close()
		seen := auditActions(t, s)
		if seen["device.rotation.prepare"] != 2 || seen["device.rotation.commit"] != 1 || seen["device.rotation.commit.failed"] != 1 {
			t.Fatalf("rotation audits: %v", seen)
		}
	})
}

// A privileged write re-checks the authorizer's credential inside its
// transaction, so a session revoked after authentication cannot finish.
func TestRevokedAuthorizerCannotCompletePrivilegedWrite(t *testing.T) {
	storeCases(t, func(t *testing.T, s store.Store) {
		now := time.Now().UTC()
		admin := domain.User{ID: "00000000-0000-4000-8000-000000000001", Email: "a@example.test", Name: "A", Role: domain.RoleAdmin, IdentityType: domain.IdentityHuman, CreatedAt: now}
		session := domain.Credential{ID: "00000000-0000-4000-8000-000000000002", UserID: admin.ID, Kind: domain.CredentialSession, TokenHash: "session", ExpiresAt: now.Add(time.Hour), CreatedAt: now, Active: true}
		if err := s.BootstrapIdentity(context.Background(), admin, session, domain.AuditEvent{ID: "00000000-0000-4000-8000-000000000003", Action: "bootstrap", CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		expired := domain.Policy{MaxStorageBytes: 1, UpdatedAt: now}
		ev := domain.AuditEvent{ID: "00000000-0000-4000-8000-000000000004", Action: "policy.update", CreatedAt: now}
		if err := s.UpdatePolicyWithAudit(context.Background(), "00000000-0000-4000-8000-00000000dead", expired, ev); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("unknown authorizer err=%v", err)
		}
		if err := s.UpdatePolicyWithAudit(context.Background(), session.ID, expired, ev); err != nil {
			t.Fatalf("active authorizer err=%v", err)
		}
	})
}

type actionFailStore struct {
	store.Store
	action string // empty fails every append
	calls  atomic.Int32
}

func (s *actionFailStore) AppendAudit(ctx context.Context, event domain.AuditEvent) error {
	if s.action == "" || event.Action == s.action {
		s.calls.Add(1)
		return errors.New("injected audit failure: plaintext-token-from-audit-error")
	}
	return s.Store.AppendAudit(ctx, event)
}

func TestSensitiveReadsFailClosedWhenAuditUnavailable(t *testing.T) {
	memory := store.NewMemory()
	adminToken := seedAdmin(t, memory)
	var logs bytes.Buffer
	srv := newServer(t, &actionFailStore{Store: memory}, api.Config{Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	for _, path := range []string{"/v1/admin/users", "/v1/admin/audit", "/v1/admin/status", "/v1/policy"} {
		res := request(t, http.MethodGet, srv.URL+path, nil, bearer(adminToken))
		if res.StatusCode != 503 {
			t.Fatalf("%s status=%d, want fail-closed 503", path, res.StatusCode)
		}
		res.Body.Close()
	}
	if strings.Contains(logs.String(), "plaintext-token-from-audit-error") {
		t.Fatalf("audit error text leaked into logs: %s", logs.String())
	}
}

func TestRateLimitRejectionFailsClosedWhenAuditUnavailable(t *testing.T) {
	memory := store.NewMemory()
	wrapped := &actionFailStore{Store: memory, action: "rate_limit.reject"}
	srv := newServer(t, wrapped, api.Config{AuthRate: api.Rate{Burst: 1, Refill: time.Hour}})
	body := []byte(`{"email":"nobody@example.test","password":"wrong password"}`)
	first := request(t, http.MethodPost, srv.URL+"/v1/login", body, map[string]string{"Content-Type": "application/json"})
	if first.StatusCode != 401 {
		t.Fatalf("first status=%d", first.StatusCode)
	}
	first.Body.Close()
	second := request(t, http.MethodPost, srv.URL+"/v1/login", body, map[string]string{"Content-Type": "application/json"})
	defer second.Body.Close()
	if second.StatusCode != 503 || wrapped.calls.Load() != 1 {
		t.Fatalf("rate-limit audit failure status=%d calls=%d", second.StatusCode, wrapped.calls.Load())
	}
}

func TestForwardedClientIPRequiresTrustedProxy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trusted []string
	}{{"untrusted", nil}, {"trusted", []string{"127.0.0.0/8"}}} {
		t.Run(tc.name, func(t *testing.T) {
			memory := store.NewMemory()
			srv := newServer(t, memory, api.Config{TrustedProxyCIDRs: tc.trusted, AuthRate: api.Rate{Burst: 1, Refill: time.Hour}})
			body := []byte(`{"email":"nobody@example.test","password":"wrong password"}`)
			for i, ip := range []string{"198.51.100.1", "198.51.100.2"} {
				res := request(t, http.MethodPost, srv.URL+"/v1/login", body, map[string]string{"Content-Type": "application/json", "X-Forwarded-For": ip})
				want := 401
				if i == 1 {
					want = 429 // same account either way; the identity dimension trips
				}
				if res.StatusCode != want {
					t.Fatalf("request %d status=%d want=%d", i, res.StatusCode, want)
				}
				res.Body.Close()
			}
			events, _ := memory.ListAudit(context.Background(), 10)
			ip := ""
			for _, e := range events {
				if e.Action == "auth.failed" {
					ip, _ = e.Metadata["client_ip"].(string)
				}
			}
			if tc.trusted == nil && ip != "127.0.0.1" || tc.trusted != nil && ip != "198.51.100.1" {
				t.Fatalf("audited client_ip=%q", ip)
			}
		})
	}
}

func TestIndependentRateLimitDimensions(t *testing.T) {
	srv := newServer(t, store.NewMemory(), api.Config{TrustedProxyCIDRs: []string{"127.0.0.0/8"}, AuthRate: api.Rate{Burst: 1, Refill: time.Hour, MaxKeys: 4096}})
	login := func(ip, email string) int {
		body := []byte(`{"email":"` + email + `","password":"wrong password"}`)
		res := request(t, http.MethodPost, srv.URL+"/v1/login", body, map[string]string{"Content-Type": "application/json", "X-Forwarded-For": ip})
		defer res.Body.Close()
		return res.StatusCode
	}
	if got := login("198.51.100.1", "a@example.test"); got != 401 {
		t.Fatalf("initial=%d", got)
	}
	if got := login("198.51.100.2", "a@example.test"); got != 429 {
		t.Fatalf("rotated IP bypassed account limit: %d", got)
	}
	if got := login("198.51.100.1", "b@example.test"); got != 429 {
		t.Fatalf("rotated account bypassed IP limit: %d", got)
	}
}

func TestSecurityHeadersAndSingleJSONValue(t *testing.T) {
	srv := newServer(t, store.NewMemory(), api.Config{})
	res := request(t, http.MethodPost, srv.URL+"/v1/login", []byte(`{"email":"a","password":"b"} {"x":1}`), map[string]string{"Content-Type": "application/json"})
	defer res.Body.Close()
	if res.StatusCode != 400 {
		t.Fatalf("trailing JSON status=%d", res.StatusCode)
	}
	for header, want := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer"} {
		if got := res.Header.Get(header); got != want {
			t.Errorf("%s=%q want %q", header, got, want)
		}
	}
}
