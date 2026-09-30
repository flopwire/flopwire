package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/api"
	"github.com/flopwire/flopwire/internal/auth"
	"github.com/flopwire/flopwire/internal/domain"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/google/uuid"
)

// fakeClock is the API's clock in credential tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Now().UTC().Truncate(time.Second)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

// member is an invited member with a login session and an enrolled device.
type member struct {
	userID, session, device, deviceID string
}

func newMember(t *testing.T, url, adminToken, email string) member {
	t.Helper()
	invite := postJSON(t, url+"/v1/admin/invites", map[string]string{"email": email, "role": "member"}, bearer(adminToken))
	claimed := postJSON(t, url+"/v1/invites/claim", map[string]string{"code": invite["code"].(string), "name": "M", "password": "a member's correct password"}, nil)
	m := member{userID: claimed["user"].(map[string]any)["id"].(string), session: claimed["token"].(string)}
	enrolled := postJSON(t, url+"/v1/devices", map[string]string{"name": "laptop", "platform": "darwin-arm64"}, bearer(m.session))
	m.device, m.deviceID = enrolled["token"].(string), enrolled["device"].(map[string]any)["id"].(string)
	return m
}

// call sends a request and returns its status and problem code.
func call(t *testing.T, method, url, token string, body []byte) (int, string) {
	t.Helper()
	headers := bearer(token)
	if body != nil {
		headers = merge(headers, map[string]string{"Content-Type": "application/json"})
	}
	res := request(t, method, url, body, headers)
	defer res.Body.Close()
	var problem struct {
		Code string `json:"code"`
	}
	if res.StatusCode >= 300 {
		decodeResponse(t, res, &problem)
	}
	return res.StatusCode, problem.Code
}

// policyOK is an authenticated request that needs no scope.
func policyOK(t *testing.T, url, token string) (int, string) {
	return call(t, "GET", url+"/v1/policy", token, nil)
}

func rotate(t *testing.T, url, deviceID, token string) (newToken string, prepared map[string]any) {
	t.Helper()
	prepared = postJSON(t, url+"/v1/devices/"+deviceID+"/rotation/prepare", map[string]any{}, bearer(token))
	postJSON(t, url+"/v1/device-rotations/"+prepared["rotation_id"].(string)+"/commit", map[string]string{"commit_token": prepared["commit_token"].(string)}, nil)
	return prepared["token"].(string), prepared
}

func mint(t *testing.T, url, token string, body map[string]any) (int, map[string]any) {
	t.Helper()
	res := request(t, "POST", url+"/v1/tokens", mustJSON(t, body), merge(bearer(token), map[string]string{"Content-Type": "application/json"}))
	var out map[string]any
	decodeResponse(t, res, &out)
	return res.StatusCode, out
}

func auditMeta(t *testing.T, s store.Store, action string) []domain.AuditEvent {
	t.Helper()
	events, err := s.ListAudit(context.Background(), 500)
	if err != nil {
		t.Fatal(err)
	}
	var out []domain.AuditEvent
	for _, e := range events {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// Rotation replaces the device credential and keeps its 90-day deadline;
// a thief holding a copy who rotates first leaves the real device with a
// refused credential (credential_rotated), and the owner's next login
// takes the device back and revokes the thief's token.
func TestDeviceRotationAndStolenTokenRace(t *testing.T) {
	storeCases(t, func(t *testing.T, s store.Store) {
		srv := newServer(t, s, api.Config{})
		m := newMember(t, srv.URL, seedAdmin(t, s), "gary@example.test")

		// Happy path.
		rotated, prepared := rotate(t, srv.URL, m.deviceID, m.device)
		if code, reason := policyOK(t, srv.URL, m.device); code != 401 || reason != "credential_rotated" {
			t.Fatalf("old token after rotation: %d %q", code, reason)
		}
		if code, _ := policyOK(t, srv.URL, rotated); code != 200 {
			t.Fatalf("rotated token: %d", code)
		}
		credExpiry, _ := time.Parse(time.RFC3339Nano, prepared["credential_expires_at"].(string))
		if credExpiry.Sub(time.Now()) < 89*24*time.Hour || credExpiry.Sub(time.Now()) > 90*24*time.Hour {
			t.Fatalf("credential_expires_at %v is not the 90-day deadline", credExpiry)
		}

		// The race: a thief with a copy of the current token rotates first.
		stolen := rotated
		thief, _ := rotate(t, srv.URL, m.deviceID, stolen)
		status, reason := call(t, "POST", srv.URL+"/v1/devices/"+m.deviceID+"/rotation/prepare", rotated, []byte(`{}`))
		if status != 401 || reason != "credential_rotated" {
			t.Fatalf("real device rotation after the thief: %d %q", status, reason)
		}
		if code, _ := policyOK(t, srv.URL, thief); code != 200 {
			t.Fatalf("thief token before re-login: %d", code)
		}
		// The owner logs in again and re-authenticates the same device.
		login := postJSON(t, srv.URL+"/v1/login", map[string]string{"email": "gary@example.test", "password": "a member's correct password"}, nil)
		reauth := postJSON(t, srv.URL+"/v1/devices/"+m.deviceID+"/reauth", map[string]any{}, bearer(login["token"].(string)))
		if reauth["device_id"] != m.deviceID {
			t.Fatalf("reauth=%v", reauth)
		}
		if code, reason := policyOK(t, srv.URL, thief); code != 401 || reason != "credential_revoked" {
			t.Fatalf("thief token after re-login: %d %q", code, reason)
		}
		if code, _ := policyOK(t, srv.URL, reauth["token"].(string)); code != 200 {
			t.Fatalf("re-authenticated token: %d", code)
		}
		// A device credential cannot re-authenticate a device.
		if code, _ := call(t, "POST", srv.URL+"/v1/devices/"+m.deviceID+"/reauth", reauth["token"].(string), []byte(`{}`)); code != 403 {
			t.Fatalf("reauth with a device credential: %d", code)
		}
		if len(auditMeta(t, s, "device.reauth")) != 1 || len(auditMeta(t, s, "device.rotation.commit")) != 2 {
			t.Fatalf("audit: %v", auditActions(t, s))
		}
	})
}

// A device credential lives 90 days from the interactive login that issued
// it, however often it rotates, and 30 days without being seen. The sweep
// then ends it with a credential.expire event; a login restores the device.
func TestDeviceCredential90DayAnd30DayIdleExpiry(t *testing.T) {
	storeCases(t, func(t *testing.T, s store.Store) {
		clock := newFakeClock()
		srv := newServer(t, s, api.Config{Now: clock.Now})
		admin := seedAdmin(t, s)
		active := newMember(t, srv.URL, admin, "active@example.test")
		idle := newMember(t, srv.URL, admin, "idle@example.test")

		// 29 days on, both work; only the active device keeps being seen.
		clock.Advance(29 * 24 * time.Hour)
		for _, tok := range []string{active.device, idle.device} {
			if code, _ := policyOK(t, srv.URL, tok); code != 200 {
				t.Fatalf("day 29: %d", code)
			}
		}
		token := active.device
		for day := 29; day < 89; day += 20 {
			clock.Advance(20 * 24 * time.Hour)
			if day+20 < 89 {
				token, _ = rotate(t, srv.URL, active.deviceID, token)
			}
			if code, _ := policyOK(t, srv.URL, token); code != 200 {
				t.Fatalf("active device on day %d: %d", day+20, code)
			}
		}
		// Day 89: idle has not been seen since day 29.
		if code, reason := policyOK(t, srv.URL, idle.device); code != 401 || reason != "reauth_required" {
			t.Fatalf("idle device: %d %q", code, reason)
		}
		if sweep, err := s.SweepCredentials(context.Background(), clock.Now()); err != nil || sweep != (domain.CredentialSweep{Idle: 1}) {
			t.Fatalf("day 89 sweep=%+v %v", sweep, err)
		}
		// Day 90: the active device reaches its interactive deadline,
		// rotations notwithstanding.
		clock.Advance(24 * time.Hour)
		if code, reason := policyOK(t, srv.URL, token); code != 401 || reason != "reauth_required" {
			t.Fatalf("active device on day 90: %d %q", code, reason)
		}
		if sweep, err := s.SweepCredentials(context.Background(), clock.Now()); err != nil || sweep != (domain.CredentialSweep{Expired: 1}) {
			t.Fatalf("day 90 sweep=%+v %v", sweep, err)
		}
		reasons := map[string]bool{}
		for _, e := range auditMeta(t, s, "credential.expire") {
			reasons[e.Metadata["reason"].(string)] = true
		}
		if !reasons[domain.RevokeExpired] || !reasons[domain.RevokeIdle] {
			t.Fatalf("credential.expire reasons=%v", reasons)
		}
		// After the sweep the refusal says why, too.
		if code, reason := policyOK(t, srv.URL, idle.device); code != 401 || reason != "reauth_required" {
			t.Fatalf("swept idle device: %d %q", code, reason)
		}
		// A login re-authenticates the idle device: same device, new deadline.
		login := postJSON(t, srv.URL+"/v1/login", map[string]string{"email": "idle@example.test", "password": "a member's correct password"}, nil)
		reauth := postJSON(t, srv.URL+"/v1/devices/"+idle.deviceID+"/reauth", map[string]any{}, bearer(login["token"].(string)))
		if code, _ := policyOK(t, srv.URL, reauth["token"].(string)); code != 200 {
			t.Fatalf("re-authenticated idle device: %d", code)
		}
	})
}

// A minted token grants only its scopes: upload-only cannot read,
// read-only cannot upload, and neither mints, rotates, deletes or
// administers. Its first use registers an ephemeral device of the minting
// user, carrying the label. A service identity mints upload tokens only.
func TestMintedTokenScopes(t *testing.T) {
	storeCases(t, func(t *testing.T, s store.Store) {
		srv := newServer(t, s, api.Config{})
		admin := seedAdmin(t, s)
		m := newMember(t, srv.URL, admin, "gary@example.test")

		status, up := mint(t, srv.URL, m.device, map[string]any{"ttl_seconds": 7200, "scopes": []string{"upload"}, "label": "ci-build-17"})
		if status != 201 {
			t.Fatalf("mint upload: %d %v", status, up)
		}
		status, rd := mint(t, srv.URL, m.device, map[string]any{"ttl_seconds": 3600, "scopes": []string{"read"}, "label": "sandbox-read"})
		if status != 201 {
			t.Fatalf("mint read: %d %v", status, rd)
		}
		upload, read := up["token"].(string), rd["token"].(string)
		// No retrieval or sync backend: an allowed request reaches the
		// handler (501); a refused one stops at the scope check (403).
		for _, c := range []struct {
			token, method, path string
			want                int
		}{
			{upload, "GET", "/v1/search?q=x", 403},
			{upload, "GET", "/v1/raw?source_id=x", 403},
			{upload, "POST", "/v1/sync/has", 501},
			{read, "GET", "/v1/search?q=x", 501},
			{read, "GET", "/v1/read?address=x", 501},
			{read, "POST", "/v1/sync/has", 403},
			{read, "DELETE", "/v1/conversations/00000000-0000-0000-0000-000000000000", 403},
			{read, "POST", "/v1/redactions", 403},
			{read, "POST", "/v1/conversations/withhold", 403},
			{upload, "POST", "/v1/tokens", 403},
			{read, "GET", "/v1/admin/devices", 403},
		} {
			var body []byte
			if c.method == "POST" {
				body = []byte(`{"scopes":["upload"],"label":"x"}`)
				if c.path == "/v1/sync/has" {
					body = []byte(`{}`)
				}
			}
			if code, _ := call(t, c.method, srv.URL+c.path, c.token, body); code != c.want {
				t.Errorf("%s %s: %d, want %d", c.method, c.path, code, c.want)
			}
		}
		devices := listDevices(t, srv.URL, admin)
		var eph []domain.Device
		for _, d := range devices {
			if d.Kind == domain.DeviceEphemeral {
				eph = append(eph, d)
			}
		}
		if len(eph) != 2 {
			t.Fatalf("ephemeral devices=%+v", devices)
		}
		for _, d := range eph {
			if d.UserID != m.userID || d.UserEmail != "gary@example.test" || (d.Label != "ci-build-17" && d.Label != "sandbox-read") || d.ExpiresAt.IsZero() || d.LastSeen.IsZero() {
				t.Errorf("ephemeral device %+v", d)
			}
			if d.Label == "ci-build-17" && (len(d.Scopes) != 1 || d.Scopes[0] != "upload") {
				t.Errorf("upload token scopes %v", d.Scopes)
			}
		}
		// Rotation is for enrolled devices only.
		if code, _ := call(t, "POST", srv.URL+"/v1/devices/"+eph[0].ID+"/rotation/prepare", upload, []byte(`{}`)); code != 403 {
			t.Fatalf("minted token rotation: %d", code)
		}
		// A service identity is upload-only, and so is what it mints.
		svc := postJSON(t, srv.URL+"/v1/admin/service-accounts", map[string]string{"name": "ci"}, bearer(admin))
		if status, _ := mint(t, srv.URL, svc["token"].(string), map[string]any{"scopes": []string{"upload", "read"}, "label": "svc"}); status != 403 {
			t.Fatalf("service mint read: %d", status)
		}
		status, svcTok := mint(t, srv.URL, svc["token"].(string), map[string]any{"scopes": []string{"upload"}, "label": "svc"})
		if status != 201 {
			t.Fatalf("service mint upload: %d", status)
		}
		if code, _ := call(t, "POST", srv.URL+"/v1/sync/has", svcTok["token"].(string), []byte(`{}`)); code != 501 {
			t.Fatalf("service token upload: %d", code)
		}
		for _, d := range listDevices(t, srv.URL, admin) {
			if d.Label == "svc" && d.UserID != svc["user"].(map[string]any)["id"] {
				t.Fatalf("service token device belongs to %s", d.UserID)
			}
		}
		// Bad requests.
		for _, body := range []map[string]any{
			{"scopes": []string{"admin"}, "label": "x"},
			{"scopes": []string{}, "label": "x"},
			{"scopes": []string{"read"}},
			{"scopes": []string{"read"}, "label": "x", "ttl_seconds": -1},
		} {
			if status, _ := mint(t, srv.URL, m.device, body); status != 400 {
				t.Errorf("mint %v: %d", body, status)
			}
		}
		mints := auditMeta(t, s, "token.mint")
		if len(mints) != 3 || len(auditMeta(t, s, "device.register")) != 3 {
			t.Fatalf("audit: %v", auditActions(t, s))
		}
	})
}

// A minted token's TTL defaults to an hour, is capped at 24 hours unless
// the policy says otherwise, and never outlives the minting credential.
func TestMintTTLCap(t *testing.T) {
	storeCases(t, func(t *testing.T, s store.Store) {
		clock := newFakeClock()
		srv := newServer(t, s, api.Config{Now: clock.Now})
		admin := seedAdmin(t, s)
		m := newMember(t, srv.URL, admin, "gary@example.test")
		expiry := func(out map[string]any) time.Duration {
			at, err := time.Parse(time.RFC3339Nano, out["expires_at"].(string))
			if err != nil {
				t.Fatal(err)
			}
			return at.Sub(clock.Now())
		}
		if status, out := mint(t, srv.URL, m.device, map[string]any{"scopes": []string{"upload"}, "label": "x"}); status != 201 || expiry(out) != time.Hour {
			t.Fatalf("default ttl: %d %v", status, out)
		}
		if status, out := mint(t, srv.URL, m.device, map[string]any{"scopes": []string{"upload"}, "label": "x", "ttl_seconds": 25 * 3600}); status != 400 || out["code"] != "ttl_exceeds_max" {
			t.Fatalf("25h: %d %v", status, out)
		}
		if status, _ := mint(t, srv.URL, m.device, map[string]any{"scopes": []string{"upload"}, "label": "x", "ttl_seconds": 24 * 3600}); status != 201 {
			t.Fatalf("24h: %d", status)
		}
		// The admin lowers the cap to 2h; a policy update that omits it
		// keeps it.
		res := request(t, "PUT", srv.URL+"/v1/admin/policy", []byte(`{"max_token_ttl_seconds":7200}`), merge(bearer(admin), map[string]string{"Content-Type": "application/json"}))
		if res.StatusCode != 200 {
			t.Fatalf("policy: %d %s", res.StatusCode, read(res))
		}
		res.Body.Close()
		res = request(t, "PUT", srv.URL+"/v1/admin/policy", []byte(`{"max_storage_bytes":0}`), merge(bearer(admin), map[string]string{"Content-Type": "application/json"}))
		res.Body.Close()
		if status, _ := mint(t, srv.URL, m.device, map[string]any{"scopes": []string{"upload"}, "label": "x", "ttl_seconds": 3 * 3600}); status != 400 {
			t.Fatalf("3h under a 2h cap: %d", status)
		}
		if status, _ := mint(t, srv.URL, m.device, map[string]any{"scopes": []string{"upload"}, "label": "x", "ttl_seconds": 2 * 3600}); status != 201 {
			t.Fatalf("2h under a 2h cap: %d", status)
		}
		// A login session (24h) mints no further than its own expiry.
		clock.Advance(23 * time.Hour)
		if status, out := mint(t, srv.URL, m.session, map[string]any{"scopes": []string{"read"}, "label": "x", "ttl_seconds": 2 * 3600}); status != 201 || expiry(out) > time.Hour {
			t.Fatalf("session-minted ttl: %d %v", status, out)
		}
	})
}

// An expired ephemeral device leaves the device list a grace period after
// its token expired; its row and audit events stay.
func TestSweepExpiredEphemeralDevices(t *testing.T) {
	storeCases(t, func(t *testing.T, s store.Store) {
		clock := newFakeClock()
		srv := newServer(t, s, api.Config{Now: clock.Now})
		admin := seedAdmin(t, s)
		m := newMember(t, srv.URL, admin, "gary@example.test")
		_, out := mint(t, srv.URL, m.device, map[string]any{"scopes": []string{"upload"}, "label": "sandbox-1", "ttl_seconds": 3600})
		token := out["token"].(string)
		if code, _ := policyOK(t, srv.URL, token); code != 200 {
			t.Fatalf("first use: %d", code)
		}
		ephemeralID := ""
		for _, d := range listDevices(t, srv.URL, admin) {
			if d.Label == "sandbox-1" {
				ephemeralID = d.ID
			}
		}
		if ephemeralID == "" {
			t.Fatal("no ephemeral device registered")
		}
		clock.Advance(time.Hour)
		if code, reason := policyOK(t, srv.URL, token); code != 401 || reason != "credential_expired" {
			t.Fatalf("expired token: %d %q", code, reason)
		}
		if sweep, err := s.SweepCredentials(context.Background(), clock.Now()); err != nil || sweep.Expired != 1 || sweep.Swept != 0 {
			t.Fatalf("sweep at expiry: %+v %v", sweep, err)
		}
		clock.Advance(domain.EphemeralSweepGrace)
		if sweep, err := s.SweepCredentials(context.Background(), clock.Now()); err != nil || sweep.Swept != 1 {
			t.Fatalf("sweep after grace: %+v %v", sweep, err)
		}
		for _, d := range listDevices(t, srv.URL, seedSession(t, s, clock.Now())) {
			if d.ID == ephemeralID {
				t.Fatal("swept device is still listed")
			}
		}
		if d, err := s.DeviceByID(context.Background(), ephemeralID); err != nil || d.SweptAt.IsZero() {
			t.Fatalf("swept device row: %+v %v", d, err)
		}
		for _, action := range []string{"token.mint", "device.register", "credential.expire", "device.sweep"} {
			if len(auditMeta(t, s, action)) == 0 {
				t.Errorf("audit lacks %s", action)
			}
		}
		// The enrolled device is not an ephemeral one: never swept.
		if d, _ := s.DeviceByID(context.Background(), m.deviceID); !d.SweptAt.IsZero() {
			t.Fatal("enrolled device swept")
		}
	})
}

// Revoking a user kills every credential it holds or minted and every
// device it owns; revoking a device kills the tokens minted from it but
// not those its owner minted elsewhere.
func TestRevokePrincipalCascade(t *testing.T) {
	storeCases(t, func(t *testing.T, s store.Store) {
		srv := newServer(t, s, api.Config{})
		admin := seedAdmin(t, s)
		m := newMember(t, srv.URL, admin, "gary@example.test")
		other := newMember(t, srv.URL, admin, "other@example.test")
		_, fromDevice := mint(t, srv.URL, m.device, map[string]any{"scopes": []string{"upload"}, "label": "from-device"})
		_, fromSession := mint(t, srv.URL, m.session, map[string]any{"scopes": []string{"read"}, "label": "from-session"})
		for _, tok := range []string{fromDevice["token"].(string), fromSession["token"].(string)} {
			if code, _ := policyOK(t, srv.URL, tok); code != 200 {
				t.Fatalf("minted token: %d", code)
			}
		}
		// revoke-device: the device and what it minted.
		if code, _ := call(t, "POST", srv.URL+"/v1/admin/devices/"+m.deviceID+"/revoke", admin, nil); code != 200 {
			t.Fatalf("revoke device: %d", code)
		}
		if code, _ := policyOK(t, srv.URL, fromDevice["token"].(string)); code != 401 {
			t.Fatalf("token minted from a revoked device: %d", code)
		}
		if code, _ := policyOK(t, srv.URL, fromSession["token"].(string)); code != 200 {
			t.Fatalf("session-minted token after device revoke: %d", code)
		}
		// revoke-user: everything else.
		res := request(t, "POST", srv.URL+"/v1/admin/users/"+m.userID+"/revoke", nil, bearer(admin))
		var out map[string]any
		decodeResponse(t, res, &out)
		if res.StatusCode != 200 || out["revoked_credentials"].(float64) < 2 {
			t.Fatalf("revoke user: %d %v", res.StatusCode, out)
		}
		for _, tok := range []string{m.session, fromSession["token"].(string)} {
			if code, reason := policyOK(t, srv.URL, tok); code != 401 || reason != "credential_revoked" {
				t.Fatalf("after revoke-user: %d %q", code, reason)
			}
		}
		for _, d := range listDevices(t, srv.URL, admin) {
			if d.UserID == m.userID && d.RevokedAt.IsZero() {
				t.Fatalf("device %s of the revoked user is live", d.ID)
			}
		}
		if code, _ := policyOK(t, srv.URL, other.device); code != 200 {
			t.Fatalf("another user's device: %d", code)
		}
		if code, _ := call(t, "POST", srv.URL+"/v1/admin/users/00000000-0000-0000-0000-000000000000/revoke", admin, nil); code != 404 {
			t.Fatalf("unknown user: %d", code)
		}
		// The member can log in again and enroll a new device.
		login := postJSON(t, srv.URL+"/v1/login", map[string]string{"email": "gary@example.test", "password": "a member's correct password"}, nil)
		if code, reason := call(t, "POST", srv.URL+"/v1/devices/"+m.deviceID+"/reauth", login["token"].(string), []byte(`{}`)); code != 409 || reason != "device_revoked" {
			t.Fatalf("reauth of a revoked device: %d %q", code, reason)
		}
		ev := auditMeta(t, s, "principal.revoke")
		if len(ev) != 1 || ev[0].TargetID != m.userID || len(auditMeta(t, s, "device.revoke")) != 1 {
			t.Fatalf("audit: %v", auditActions(t, s))
		}
		if _, ok := ev[0].Metadata["credentials"]; !ok {
			t.Fatalf("principal.revoke metadata=%v", ev[0].Metadata)
		}
	})
}

func listDevices(t *testing.T, url, adminToken string) []domain.Device {
	t.Helper()
	res := request(t, "GET", url+"/v1/admin/devices", nil, bearer(adminToken))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("list devices: %d %s", res.StatusCode, read(res))
	}
	var out struct{ Devices []domain.Device }
	decodeResponse(t, res, &out)
	return out.Devices
}

// seedSession is a login session of the first admin, issued at now.
func seedSession(t *testing.T, s store.Store, now time.Time) string {
	t.Helper()
	u, err := s.UserByEmail(context.Background(), "admin@example.test")
	if err != nil {
		t.Fatal(err)
	}
	plain, hash, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	c := domain.Credential{ID: uuid.NewString(), UserID: u.ID, Kind: domain.CredentialSession, TokenHash: hash, ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now, Active: true}
	if err := s.CreateCredentialWithAudit(context.Background(), c, domain.AuditEvent{ID: uuid.NewString(), ActorID: u.ID, Action: "login", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	return plain
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A thief holding a copy of the device token mints a sandbox token from
// it. The owner's re-login takes the device back; the thief's minted token
// and its ephemeral device must die with the stolen credential.
func TestReauthRevokesTokensMintedFromTheDevice(t *testing.T) {
	storeCases(t, func(t *testing.T, s store.Store) {
		srv := newServer(t, s, api.Config{})
		m := newMember(t, srv.URL, seedAdmin(t, s), "gary@example.test")
		status, out := mint(t, srv.URL, m.device, map[string]any{"scopes": []string{"read"}, "label": "thief", "ttl_seconds": 24 * 3600})
		if status != 201 {
			t.Fatalf("mint: %d %v", status, out)
		}
		minted := out["token"].(string)
		if code, _ := call(t, "GET", srv.URL+"/v1/policy", minted, nil); code != 200 {
			t.Fatalf("minted token before re-login: %d", code)
		}
		login := postJSON(t, srv.URL+"/v1/login", map[string]string{"email": "gary@example.test", "password": "a member's correct password"}, nil)
		postJSON(t, srv.URL+"/v1/devices/"+m.deviceID+"/reauth", map[string]any{}, bearer(login["token"].(string)))
		if code, reason := policyOK(t, srv.URL, minted); code != 401 || reason != "credential_revoked" {
			t.Fatalf("token minted from the stolen credential after re-login: %d %q", code, reason)
		}
	})
}

// The minted-token TTL cap must fail closed: a cap too large for a
// time.Duration overflowed to a negative limit and a minted token with no
// expiry at all.
func TestMintTTLCapRejectsOverflow(t *testing.T) {
	storeCases(t, func(t *testing.T, s store.Store) {
		srv := newServer(t, s, api.Config{})
		admin := seedAdmin(t, s)
		res := request(t, "PUT", srv.URL+"/v1/admin/policy", []byte(`{"max_token_ttl_seconds":9223372037}`), merge(bearer(admin), map[string]string{"Content-Type": "application/json"}))
		res.Body.Close()
		if res.StatusCode != 400 {
			t.Fatalf("policy with an overflowing cap: %d", res.StatusCode)
		}
		res = request(t, "PUT", srv.URL+"/v1/admin/policy", []byte(`{"max_token_ttl_seconds":7776001}`), merge(bearer(admin), map[string]string{"Content-Type": "application/json"}))
		res.Body.Close()
		if res.StatusCode != 400 {
			t.Fatalf("policy with a cap past 90 days: %d", res.StatusCode)
		}
	})
}
