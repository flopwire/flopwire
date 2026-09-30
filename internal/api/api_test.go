package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/api"
	"github.com/flopwire/flopwire/internal/auth"
	"github.com/flopwire/flopwire/internal/domain"
	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// storeCases runs a test against the memory store and, when
// FLOPWIRE_TEST_DATABASE_URL is set, against a freshly migrated Postgres.
func storeCases(t *testing.T, run func(t *testing.T, s store.Store)) {
	t.Run("memory", func(t *testing.T) { run(t, store.NewMemory()) })
	t.Run("postgres", func(t *testing.T) { run(t, postgresStore(t)) })
}

func postgresStore(t *testing.T) *store.Postgres {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), pgtest.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = store.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return store.NewPostgres(pool, nil, "")
}

func newServer(t *testing.T, s store.Store, cfg api.Config) *httptest.Server {
	t.Helper()
	reg := prometheus.NewRegistry()
	cfg.Registry = reg
	srv := httptest.NewServer(api.New(s, cfg).Handler(reg))
	t.Cleanup(srv.Close)
	return srv
}

// seedAdmin creates the first administrator the way `flopwire bootstrap` does
// and returns a login-session token.
func seedAdmin(t *testing.T, s store.Store) string {
	t.Helper()
	now := time.Now().UTC()
	passwordHash, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	plain, tokenHash, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	u := domain.User{ID: uuid.NewString(), Email: "admin@example.test", Name: "Admin", Role: domain.RoleAdmin, IdentityType: domain.IdentityHuman, PasswordHash: passwordHash, CreatedAt: now}
	c := domain.Credential{ID: uuid.NewString(), UserID: u.ID, Kind: domain.CredentialSession, TokenHash: tokenHash, ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now, Active: true}
	if err = s.BootstrapIdentity(context.Background(), u, c, domain.AuditEvent{ID: uuid.NewString(), ActorID: u.ID, Action: "bootstrap", TargetType: "user", TargetID: u.ID, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	return plain
}

func auditActions(t *testing.T, s store.Store) map[string]int {
	t.Helper()
	events, err := s.ListAudit(context.Background(), 500)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, e := range events {
		out[e.Action]++
	}
	return out
}

func TestIdentityFlowAndRemovedEndpoints(t *testing.T) {
	storeCases(t, func(t *testing.T, s store.Store) {
		srv := newServer(t, s, api.Config{})
		adminToken := seedAdmin(t, s)
		if err := s.BootstrapIdentity(context.Background(), domain.User{ID: uuid.NewString(), Email: "second@example.test", Name: "Second", Role: domain.RoleAdmin, IdentityType: domain.IdentityHuman, CreatedAt: time.Now()}, domain.Credential{ID: uuid.NewString(), Kind: domain.CredentialSession, TokenHash: "x", CreatedAt: time.Now(), Active: true}, domain.AuditEvent{ID: uuid.NewString(), Action: "bootstrap", CreatedAt: time.Now()}); err != store.ErrConflict {
			t.Fatalf("second bootstrap err=%v", err)
		}
		network := request(t, "POST", srv.URL+"/v1/bootstrap", []byte(`{}`), map[string]string{"Content-Type": "application/json"})
		if network.StatusCode != 404 {
			t.Fatalf("network bootstrap status=%d", network.StatusCode)
		}
		network.Body.Close()
		invite := postJSON(t, srv.URL+"/v1/admin/invites", map[string]string{"email": "gary@example.test", "role": "member"}, bearer(adminToken))
		code := invite["code"].(string)
		member := postJSON(t, srv.URL+"/v1/invites/claim", map[string]string{"code": code, "name": "Gary", "password": "a different correct password"}, nil)
		memberToken := member["token"].(string)
		reclaim := request(t, "POST", srv.URL+"/v1/invites/claim", []byte(`{"code":"`+code+`","name":"Eve","password":"a different correct password"}`), map[string]string{"Content-Type": "application/json"})
		if reclaim.StatusCode != 400 {
			t.Fatalf("reclaim status=%d", reclaim.StatusCode)
		}
		reclaim.Body.Close()
		login := postJSON(t, srv.URL+"/v1/login", map[string]string{"email": "GARY@example.test", "password": "a different correct password"}, nil)
		if login["user"].(map[string]any)["name"] != "Gary" {
			t.Fatalf("login user=%#v", login["user"])
		}
		device := postJSON(t, srv.URL+"/v1/devices", map[string]string{"name": "gary-mac", "platform": "darwin-arm64"}, bearer(memberToken))
		deviceToken := device["token"].(string)
		deviceID := device["device"].(map[string]any)["id"].(string)
		for _, path := range []string{"/v1/search?q=income", "/v1/raw?source_id=x"} {
			res := request(t, "GET", srv.URL+path, nil, bearer(deviceToken))
			if res.StatusCode != 501 {
				t.Fatalf("%s status=%d body=%s", path, res.StatusCode, read(res))
			}
			res.Body.Close()
		}
		memberAdmin := request(t, "GET", srv.URL+"/v1/admin/users", nil, bearer(memberToken))
		if memberAdmin.StatusCode != 403 {
			t.Fatalf("member admin status=%d", memberAdmin.StatusCode)
		}
		memberAdmin.Body.Close()
		revoke := request(t, "POST", srv.URL+"/v1/admin/devices/"+deviceID+"/revoke", nil, bearer(adminToken))
		if revoke.StatusCode != 200 {
			t.Fatalf("revoke status=%d body=%s", revoke.StatusCode, read(revoke))
		}
		revoke.Body.Close()
		revoked := request(t, "GET", srv.URL+"/v1/policy", nil, bearer(deviceToken))
		if revoked.StatusCode != 401 {
			t.Fatalf("revoked device status=%d", revoked.StatusCode)
		}
		revoked.Body.Close()
		seen := auditActions(t, s)
		for _, want := range []string{"bootstrap", "invite.create", "invite.claim", "invite.claim.failed", "login", "device.enroll", "device.revoke", "authorization.failed", "auth.failed"} {
			if seen[want] == 0 {
				t.Errorf("audit missing %s: %v", want, seen)
			}
		}
	})
}

func TestAdminPolicyAndStatus(t *testing.T) {
	storeCases(t, func(t *testing.T, s store.Store) {
		srv := newServer(t, s, api.Config{})
		adminToken := seedAdmin(t, s)
		policyResponse := request(t, "PUT", srv.URL+"/v1/admin/policy", []byte(`{"max_storage_bytes":1000,"max_user_bytes":8,"path_rules":["/Personal/","/personal","\\personal\\","local repo:git@github.com:Acme/Web.git"],"unplaceable":"Exclude"}`), merge(bearer(adminToken), map[string]string{"Content-Type": "application/json"}))
		if policyResponse.StatusCode != 200 {
			t.Fatalf("policy status=%d body=%s", policyResponse.StatusCode, read(policyResponse))
		}
		policyResponse.Body.Close()
		statusResponse := request(t, "GET", srv.URL+"/v1/admin/status", nil, bearer(adminToken))
		if statusResponse.StatusCode != 200 {
			t.Fatalf("admin status=%d body=%s", statusResponse.StatusCode, read(statusResponse))
		}
		var status map[string]any
		decodeResponse(t, statusResponse, &status)
		storage := status["storage"].(map[string]any)
		if storage["quota_bytes"] != float64(1000) || storage["used_bytes"] != float64(0) {
			t.Fatalf("wrong status storage: %#v", storage)
		}
		if status["audit"].(map[string]any)["event_count"].(float64) < 2 {
			t.Fatalf("audit count: %#v", status["audit"])
		}
		device := postJSON(t, srv.URL+"/v1/devices", map[string]string{"name": "admin-mac", "platform": "darwin-arm64"}, bearer(adminToken))
		policy := request(t, "GET", srv.URL+"/v1/policy", nil, bearer(device["token"].(string)))
		var got map[string]any
		decodeResponse(t, policy, &got)
		rules := got["path_rules"].([]any)
		if len(rules) != 3 || rules[0] != "/personal/" || rules[1] != "/personal" || rules[2] != "local:repo:github.com/acme/web" {
			t.Fatalf("path rules not normalized: %#v", rules)
		}
		if got["unplaceable"] != "exclude" {
			t.Fatalf("unplaceable: %#v", got["unplaceable"])
		}
		bad := request(t, "PUT", srv.URL+"/v1/admin/policy", []byte(`{"unplaceable":"deny"}`), merge(bearer(adminToken), map[string]string{"Content-Type": "application/json"}))
		if bad.StatusCode != 400 {
			t.Fatalf("an unknown unplaceable value was accepted: %d", bad.StatusCode)
		}
		bad.Body.Close()
	})
}

func TestAdminDeviceCredentialCannotUseAdministrativeRoutes(t *testing.T) {
	storeCases(t, func(t *testing.T, s store.Store) {
		srv := newServer(t, s, api.Config{})
		adminSession := seedAdmin(t, s)
		device := postJSON(t, srv.URL+"/v1/devices", map[string]string{"name": "admin-collector", "platform": "linux-amd64"}, bearer(adminSession))
		deviceToken := device["token"].(string)
		requests := []struct{ method, path, body string }{
			{"POST", "/v1/admin/invites", `{"email":"forbidden@example.test","role":"member"}`},
			{"PUT", "/v1/admin/policy", `{"max_storage_bytes":1,"max_user_bytes":1}`},
			{"GET", "/v1/admin/audit", ""},
		}
		for _, tc := range requests {
			res := request(t, tc.method, srv.URL+tc.path, []byte(tc.body), merge(bearer(deviceToken), map[string]string{"Content-Type": "application/json"}))
			if res.StatusCode != http.StatusForbidden {
				t.Fatalf("%s %s status=%d body=%s", tc.method, tc.path, res.StatusCode, read(res))
			}
			res.Body.Close()
		}
		enroll := request(t, "POST", srv.URL+"/v1/devices", []byte(`{"name":"x","platform":"y"}`), merge(bearer(deviceToken), map[string]string{"Content-Type": "application/json"}))
		if enroll.StatusCode != http.StatusForbidden {
			t.Fatalf("device-credential enrollment status=%d", enroll.StatusCode)
		}
		enroll.Body.Close()
		allowed := request(t, "POST", srv.URL+"/v1/admin/invites", []byte(`{"email":"allowed@example.test","role":"member"}`), merge(bearer(adminSession), map[string]string{"Content-Type": "application/json"}))
		if allowed.StatusCode != http.StatusCreated {
			t.Fatalf("session invite status=%d body=%s", allowed.StatusCode, read(allowed))
		}
		allowed.Body.Close()
		events, err := s.ListAudit(context.Background(), 100)
		if err != nil {
			t.Fatal(err)
		}
		denials := 0
		for _, event := range events {
			if event.Action == "authorization.failed" && event.Metadata["reason"] == "admin_session_required" {
				denials++
			}
		}
		if denials != len(requests) {
			t.Fatalf("admin device denials=%d want=%d", denials, len(requests))
		}
	})
}

func TestServiceAccountIsUploadOnlyAndRevocable(t *testing.T) {
	storeCases(t, func(t *testing.T, s store.Store) {
		srv := newServer(t, s, api.Config{})
		adminToken := seedAdmin(t, s)
		service := postJSON(t, srv.URL+"/v1/admin/service-accounts", map[string]string{"name": "CI collector"}, bearer(adminToken))
		serviceToken := service["token"].(string)
		deviceID := service["device"].(map[string]any)["id"].(string)
		for _, path := range []string{"/v1/search?q=anything", "/v1/raw?source_id=x"} {
			res := request(t, "GET", srv.URL+path, nil, bearer(serviceToken))
			if res.StatusCode != 403 {
				t.Fatalf("service %s status=%d", path, res.StatusCode)
			}
			res.Body.Close()
		}
		revoke := request(t, "POST", srv.URL+"/v1/admin/devices/"+deviceID+"/revoke", nil, bearer(adminToken))
		if revoke.StatusCode != 200 {
			t.Fatalf("revoke status=%d body=%s", revoke.StatusCode, read(revoke))
		}
		revoke.Body.Close()
		after := request(t, "GET", srv.URL+"/v1/policy", nil, bearer(serviceToken))
		if after.StatusCode != 401 {
			t.Fatalf("revoked service status=%d body=%s", after.StatusCode, read(after))
		}
		after.Body.Close()
	})
}

func postJSON(t *testing.T, url string, body any, headers map[string]string) map[string]any {
	t.Helper()
	b, _ := json.Marshal(body)
	res := request(t, "POST", url, b, merge(headers, map[string]string{"Content-Type": "application/json"}))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		t.Fatalf("POST %s status=%d body=%s", url, res.StatusCode, read(res))
	}
	var out map[string]any
	decodeResponse(t, res, &out)
	return out
}
func request(t *testing.T, method, url string, body []byte, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}
func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}
func merge(ms ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}
func decodeResponse(t *testing.T, res *http.Response, out any) {
	t.Helper()
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
}
func read(res *http.Response) string {
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return string(b)
}

func TestConversationDeletionRoutes(t *testing.T) {
	memory := store.NewMemory()
	memorySrv := newServer(t, memory, api.Config{})
	memToken := seedAdmin(t, memory)
	res := request(t, "DELETE", memorySrv.URL+"/v1/admin/conversations/"+uuid.NewString(), nil, bearer(memToken))
	if res.StatusCode != 501 {
		t.Fatalf("memory delete status=%d", res.StatusCode)
	}
	res.Body.Close()

	url := pgtest.NewDatabase(t)
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = store.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	s := store.NewPostgres(pool, nil, "")
	srv := newServer(t, s, api.Config{})
	adminToken := seedAdmin(t, s)
	device := postJSON(t, srv.URL+"/v1/devices", map[string]string{"name": "mac", "platform": "darwin"}, bearer(adminToken))
	deviceID := device["device"].(map[string]any)["id"].(string)
	conv := uuid.NewString()
	if _, err = pool.Exec(context.Background(), `INSERT INTO conversations(id,agent,session_id,device_id,user_id) SELECT $1,'claude','s1',$2,user_id FROM devices WHERE id=$2`, conv, deviceID); err != nil {
		t.Fatal(err)
	}
	memberDelete := request(t, "DELETE", srv.URL+"/v1/admin/conversations/"+conv, nil, bearer(device["token"].(string)))
	if memberDelete.StatusCode != 403 {
		t.Fatalf("device-credential delete status=%d", memberDelete.StatusCode)
	}
	memberDelete.Body.Close()
	del := request(t, "DELETE", srv.URL+"/v1/admin/conversations/"+conv, nil, bearer(adminToken))
	if del.StatusCode != 202 {
		t.Fatalf("delete status=%d body=%s", del.StatusCode, read(del))
	}
	location := del.Header.Get("Location")
	var out struct {
		Deletion domain.DeletionJob `json:"deletion"`
	}
	decodeResponse(t, del, &out)
	if location != "/v1/admin/deletions/"+out.Deletion.ID || out.Deletion.State != domain.DeletionQueued {
		t.Fatalf("location=%q job=%+v", location, out.Deletion)
	}
	missing := request(t, "DELETE", srv.URL+"/v1/admin/conversations/"+uuid.NewString(), nil, bearer(adminToken))
	if missing.StatusCode != 404 {
		t.Fatalf("missing status=%d", missing.StatusCode)
	}
	missing.Body.Close()
	get := request(t, "GET", srv.URL+location, nil, bearer(adminToken))
	if get.StatusCode != 200 {
		t.Fatalf("get status=%d", get.StatusCode)
	}
	get.Body.Close()
	retry := request(t, "POST", srv.URL+location+"/retry", nil, bearer(adminToken))
	if retry.StatusCode != 409 {
		t.Fatalf("retry of queued job status=%d", retry.StatusCode)
	}
	retry.Body.Close()

	// D9: a member deletes their own conversations through the member
	// route, not anyone else's, and reads back only their own jobs.
	member, memberConv, memberDevice := uuid.NewString(), uuid.NewString(), uuid.NewString()
	memberToken, memberHash, _ := auth.NewToken()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id,email,name,role,identity_type,created_at) VALUES($1,'m@example.test','M','member','human',now())`, []any{member}},
		{`INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$2,'m-mac','darwin',now())`, []any{memberDevice, member}},
		{`INSERT INTO credentials(id,user_id,device_id,kind,token_hash,created_at) VALUES($1,$2,$3,'device',$4,now())`, []any{uuid.NewString(), member, memberDevice, memberHash}},
		{`INSERT INTO conversations(id,agent,session_id,device_id,user_id) VALUES($1,'claude','m1',$2,$3)`, []any{memberConv, memberDevice, member}},
	} {
		if _, err = pool.Exec(context.Background(), q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	other := uuid.NewString()
	if _, err = pool.Exec(context.Background(), `INSERT INTO conversations(id,agent,session_id,device_id,user_id) SELECT $1,'claude','s2',$2,user_id FROM devices WHERE id=$2`, other, deviceID); err != nil {
		t.Fatal(err)
	}
	if res := request(t, "DELETE", srv.URL+"/v1/conversations/"+other, nil, bearer(memberToken)); res.StatusCode != 403 {
		t.Fatalf("member deleting another user's conversation: %d", res.StatusCode)
	} else {
		res.Body.Close()
	}
	own := request(t, "DELETE", srv.URL+"/v1/conversations/"+memberConv, nil, bearer(memberToken))
	if own.StatusCode != 202 || !strings.HasPrefix(own.Header.Get("Location"), "/v1/deletions/") {
		t.Fatalf("member deleting own conversation: %d %s", own.StatusCode, read(own))
	}
	own.Body.Close()
	if res := request(t, "GET", srv.URL+own.Header.Get("Location"), nil, bearer(memberToken)); res.StatusCode != 200 {
		t.Fatalf("member reading own job: %d", res.StatusCode)
	} else {
		res.Body.Close()
	}
	if res := request(t, "GET", srv.URL+"/v1/deletions/"+out.Deletion.ID, nil, bearer(memberToken)); res.StatusCode != 404 {
		t.Fatalf("member reading another's job: %d", res.StatusCode)
	} else {
		res.Body.Close()
	}

	ready := request(t, "GET", srv.URL+"/readyz", nil, nil)
	if ready.StatusCode != 503 {
		t.Fatalf("readyz without object store status=%d", ready.StatusCode)
	}
	ready.Body.Close()
	live := request(t, "GET", srv.URL+"/livez", nil, nil)
	if live.StatusCode != 200 {
		t.Fatalf("livez status=%d", live.StatusCode)
	}
	live.Body.Close()
}
