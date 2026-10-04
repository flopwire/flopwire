package main

// Credential-transition tests ported from the CASS-era stack (#8: 62d5a64,
// 4bbf5fe, cb16d94); the collect and backup-audit cases went with CASS.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/domain"
	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestBootstrapDoesNotAcceptPasswordArguments(t *testing.T) {
	err := bootstrap(context.Background(), []string{"--name", "Admin", "--email", "admin@example.test", "--password", "secret-from-history"})
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("bootstrap accepted a password argument: %v", err)
	}
}

func TestLoginAndClaimDoNotAcceptPasswordArguments(t *testing.T) {
	for name, fn := range map[string]func(context.Context, []string) error{"login": login, "claim": claim} {
		t.Run(name, func(t *testing.T) {
			err := fn(context.Background(), []string{"--password", "secret-from-history"})
			if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
				t.Fatalf("accepted password argument: %v", err)
			}
		})
	}
}

func TestAdminCLIUsesSessionCredentialWithoutReplacingCollectorCredential(t *testing.T) {
	var authorizations []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/v1/admin/invites":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "invite-code"})
		case "/v1/devices":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "replacement-device", "device": map[string]any{"id": "replacement-id"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	if err := client.Save(client.Config{Server: srv.URL, Token: "collector-device", SessionToken: "admin-session", DeviceID: "collector-id"}); err != nil {
		t.Fatal(err)
	}
	if err := invite(t.Context(), []string{"--email", "member@example.test"}); err != nil {
		t.Fatal(err)
	}
	if err := enroll(t.Context(), []string{"--name", "replacement", "--platform", "test"}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(authorizations) != "[Bearer admin-session Bearer admin-session]" {
		t.Fatalf("authorizations=%v", authorizations)
	}
	cfg, err := client.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "replacement-device" || cfg.DeviceID != "replacement-id" || cfg.SessionToken != "admin-session" {
		t.Fatalf("config=%#v", cfg)
	}
	if err = saveAuth(t.Context(), srv.URL, "", map[string]any{"token": "refreshed-session", "user": map[string]any{"role": "admin"}}); err != nil {
		t.Fatal(err)
	}
	cfg, err = client.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "replacement-device" || cfg.DeviceID != "replacement-id" || cfg.SessionToken != "refreshed-session" {
		t.Fatalf("refreshed config=%#v", cfg)
	}
}

func TestLoginAndClaimWaitForPausedRotationAndPreserveItsResult(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth func(context.Context, string) error
	}{
		{"login", func(ctx context.Context, server string) error {
			return loginAndSave(ctx, server, "", "admin@example.test", "password")
		}},
		{"claim", func(ctx context.Context, server string) error {
			return claimAndSave(ctx, server, "", "invite", "Admin", "password")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prepareEntered := make(chan struct{})
			releasePrepare := make(chan struct{})
			authRequest := make(chan struct{}, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/rotation/prepare"):
					close(prepareEntered)
					<-releasePrepare
					_ = json.NewEncoder(w).Encode(map[string]any{"rotation_id": "rotation", "token": "rotated-device", "commit_token": "commit", "expires_at": time.Now().Add(time.Minute)})
				case strings.HasSuffix(r.URL.Path, "/rotation/commit"):
					_ = json.NewEncoder(w).Encode(map[string]any{"state": "committed"})
				case r.URL.Path == "/v1/login" || r.URL.Path == "/v1/invites/claim":
					authRequest <- struct{}{}
					_ = json.NewEncoder(w).Encode(map[string]any{"token": "refreshed-session", "user": map[string]any{"role": "admin"}})
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
			if err := client.Save(client.Config{Server: srv.URL, Token: "old-device", SessionToken: "old-session", DeviceID: "device"}); err != nil {
				t.Fatal(err)
			}
			rotationDone := make(chan error, 1)
			go func() { rotationDone <- rotateDevice(t.Context(), nil) }()
			<-prepareEntered
			blockedCtx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			err := tc.auth(blockedCtx, srv.URL)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("blocked authentication err=%v", err)
			}
			select {
			case <-authRequest:
				t.Fatal("authentication request escaped the config lock during rotation")
			default:
			}
			close(releasePrepare)
			if err := <-rotationDone; err != nil {
				t.Fatal(err)
			}
			if err := tc.auth(t.Context(), srv.URL); err != nil {
				t.Fatal(err)
			}
			cfg, err := client.Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Token != "rotated-device" || cfg.SessionToken != "refreshed-session" || cfg.DeviceID != "device" || cfg.PendingRotation != nil {
				t.Fatalf("config=%#v", cfg)
			}
		})
	}
}

func TestServerSwitchWaitsForRotationThenClearsServerBoundState(t *testing.T) {
	prepareEntered := make(chan struct{})
	releasePrepare := make(chan struct{})
	oldServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/rotation/prepare") {
			close(prepareEntered)
			<-releasePrepare
			_ = json.NewEncoder(w).Encode(map[string]any{"rotation_id": "rotation", "token": "rotated-old-device", "commit_token": "commit", "expires_at": time.Now().Add(time.Minute)})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "committed"})
	}))
	defer oldServer.Close()
	newLogin := make(chan struct{}, 1)
	newServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		newLogin <- struct{}{}
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "new-session", "user": map[string]any{"role": "admin"}})
	}))
	defer newServer.Close()
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	denylist := []string{"/private/", ".secrets"}
	if err := client.Save(client.Config{Server: oldServer.URL, Token: "old-device", SessionToken: "old-session", DeviceID: "old-device-id", Denylist: denylist}); err != nil {
		t.Fatal(err)
	}
	rotationDone := make(chan error, 1)
	go func() { rotationDone <- rotateDevice(t.Context(), nil) }()
	<-prepareEntered
	blockedCtx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	err := loginAndSave(blockedCtx, newServer.URL, "", "admin@example.test", "password")
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked server switch err=%v", err)
	}
	select {
	case <-newLogin:
		t.Fatal("server switch escaped the config lock during rotation")
	default:
	}
	close(releasePrepare)
	if err := <-rotationDone; err != nil {
		t.Fatal(err)
	}
	if err := loginAndSave(t.Context(), newServer.URL, "", "admin@example.test", "password"); err != nil {
		t.Fatal(err)
	}
	cfg, err := client.Load()
	if err != nil {
		t.Fatal(err)
	}
	wantServer, _ := client.NormalizeServer(newServer.URL)
	if cfg.Server != wantServer || cfg.Token != "new-session" || cfg.SessionToken != "new-session" || cfg.DeviceID != "" || cfg.PendingRotation != nil || fmt.Sprint(cfg.Denylist) != fmt.Sprint(denylist) {
		t.Fatalf("switched config=%#v", cfg)
	}
}

func TestEquivalentServerRefreshPreservesDeviceAndPendingRotation(t *testing.T) {
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	pending := &client.PendingRotation{ID: "pending", NewToken: "new-device", CommitToken: "commit", State: "prepared", ExpiresAt: time.Now().Add(time.Minute)}
	if err := client.Save(client.Config{Server: "HTTPS://FLOPWIRE.EXAMPLE:443/", Token: "device", SessionToken: "old-session", DeviceID: "device-id", Denylist: []string{"private"}, PendingRotation: pending}); err != nil {
		t.Fatal(err)
	}
	if err := saveAuth(t.Context(), "https://flopwire.example", "", map[string]any{"token": "new-session", "user": map[string]any{"role": "admin"}}); err != nil {
		t.Fatal(err)
	}
	cfg, err := client.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server != "https://flopwire.example" || cfg.Token != "device" || cfg.SessionToken != "new-session" || cfg.DeviceID != "device-id" || cfg.PendingRotation == nil || cfg.PendingRotation.ID != "pending" || fmt.Sprint(cfg.Denylist) != "[private]" {
		t.Fatalf("equivalent refresh config=%#v", cfg)
	}
}

func TestAmbiguousLegacyServerPathForcesCapabilityClearingSwitch(t *testing.T) {
	for _, legacyServer := range []string{
		"https://flopwire.example/base%2Fadmin",
		"https://flopwire.example/base%5Cadmin",
		`https://flopwire.example/base\admin`,
		"https://flopwire.example/base/../admin",
		"https://flopwire.example/base/%2e%2e/admin",
	} {
		t.Run(legacyServer, func(t *testing.T) {
			t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
			pending := &client.PendingRotation{ID: "old-pending", NewToken: "old-new-device", CommitToken: "old-commit", State: "prepared", ExpiresAt: time.Now().Add(time.Minute)}
			if err := client.Save(client.Config{Server: legacyServer, Token: "old-device", SessionToken: "old-session", DeviceID: "old-device-id", Denylist: []string{"/private/"}, PendingRotation: pending}); err != nil {
				t.Fatal(err)
			}
			if err := saveAuth(t.Context(), "https://flopwire.example/base/admin", "", map[string]any{"token": "new-session", "user": map[string]any{"role": "admin"}}); err != nil {
				t.Fatal(err)
			}
			cfg, err := client.Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Server != "https://flopwire.example/base/admin" || cfg.Token != "new-session" || cfg.SessionToken != "new-session" || cfg.DeviceID != "" || cfg.PendingRotation != nil || fmt.Sprint(cfg.Denylist) != "[/private/]" {
				t.Fatalf("legacy=%q switched config=%#v", legacyServer, cfg)
			}
		})
	}
}

func TestAmbiguousNewServerPathFailsBeforeConfigMutation(t *testing.T) {
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	want := client.Config{Server: "https://flopwire.example/base/admin", Token: "device", SessionToken: "session", DeviceID: "device-id", Denylist: []string{"/private/"}}
	if err := client.Save(want); err != nil {
		t.Fatal(err)
	}
	err := saveAuth(t.Context(), "https://flopwire.example/base%2Fadmin", "", map[string]any{"token": "new-session", "user": map[string]any{"role": "admin"}})
	if err == nil || !strings.Contains(err.Error(), "server base path") {
		t.Fatalf("ambiguous save err=%v", err)
	}
	got, loadErr := client.Load()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if got.Server != want.Server || got.Token != want.Token || got.SessionToken != want.SessionToken || got.DeviceID != want.DeviceID || fmt.Sprint(got.Denylist) != fmt.Sprint(want.Denylist) {
		t.Fatalf("config mutated=%#v", got)
	}
}

func TestCredentialSaveDoesNotOverwriteUnreadableExistingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("FLOPWIRE_CONFIG", path)
	corrupt := []byte(`{"denylist":["/private/"],`)
	if err := os.WriteFile(path, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	err := saveAuth(t.Context(), "https://flopwire.example", "", map[string]any{"token": "new-session", "user": map[string]any{"role": "admin"}})
	if err == nil || !strings.Contains(err.Error(), "load existing config") {
		t.Fatalf("save err=%v", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, corrupt) {
		t.Fatalf("corrupt config was overwritten: %q", got)
	}
}

func TestRotateDeviceRecoversExpiredAndInvalidPendingState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pending *client.PendingRotation
		invalid bool
	}{{"expired", &client.PendingRotation{ID: "expired", NewToken: "discard", CommitToken: "discard", State: "prepared", ExpiresAt: time.Unix(900, 0)}, false}, {"invalid", &client.PendingRotation{ID: "invalid", NewToken: "discard", CommitToken: "discard", State: "prepared", ExpiresAt: time.Unix(2000, 0)}, true}} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Unix(1000, 0)
			prepareCalls, invalidCalls, commitCalls := 0, 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/rotation/prepare"):
					prepareCalls++
					_ = json.NewEncoder(w).Encode(map[string]any{"rotation_id": "new-rotation", "token": "new-token", "commit_token": "new-commit", "expires_at": now.Add(time.Minute)})
				case strings.Contains(r.URL.Path, "/invalid/commit"):
					invalidCalls++
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(400)
					_ = json.NewEncoder(w).Encode(map[string]any{"code": "rotation_invalid_or_expired", "detail": "plaintext-old-commit-secret"})
				case strings.Contains(r.URL.Path, "/expired/commit"):
					invalidCalls++
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(400)
					_ = json.NewEncoder(w).Encode(map[string]any{"code": "rotation_invalid_or_expired"})
				case strings.Contains(r.URL.Path, "/new-rotation/commit"):
					commitCalls++
					_ = json.NewEncoder(w).Encode(map[string]any{"state": "committed"})
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			path := filepath.Join(t.TempDir(), "config.json")
			t.Setenv("FLOPWIRE_CONFIG", path)
			if err := client.Save(client.Config{Server: srv.URL, Token: "old-token", DeviceID: "device", PendingRotation: tc.pending}); err != nil {
				t.Fatal(err)
			}
			if err := rotateDeviceLocked(context.Background(), func() time.Time { return now }); err != nil {
				t.Fatal(err)
			}
			got, err := client.Load()
			if err != nil {
				t.Fatal(err)
			}
			if got.Token != "new-token" || got.PendingRotation != nil {
				t.Fatalf("config=%#v", got)
			}
			if prepareCalls != 1 || commitCalls != 1 {
				t.Fatalf("prepare=%d commit=%d", prepareCalls, commitCalls)
			}
			if invalidCalls != 1 {
				t.Fatalf("invalid commits=%d", invalidCalls)
			}
		})
	}
}

func TestRotateDeviceKeepsPendingStateOnGenericProxyNotFound(t *testing.T) {
	now := time.Now()
	prepareCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/rotation/prepare") {
			prepareCalls++
		}
		http.Error(w, "proxy route missing", http.StatusNotFound)
	}))
	defer srv.Close()
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	pending := &client.PendingRotation{ID: "pending", NewToken: "new", CommitToken: "commit", State: "prepared", ExpiresAt: now.Add(time.Minute)}
	if err := client.Save(client.Config{Server: srv.URL, Token: "old", DeviceID: "device", PendingRotation: pending}); err != nil {
		t.Fatal(err)
	}
	if err := rotateDeviceLocked(context.Background(), time.Now); err == nil {
		t.Fatal("expected proxy error")
	}
	got, err := client.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.PendingRotation == nil || got.PendingRotation.ID != pending.ID || prepareCalls != 0 {
		t.Fatalf("pending=%#v prepare_calls=%d", got.PendingRotation, prepareCalls)
	}
}

func TestRotateFailureDoesNotEchoCapabilityOrResponseBody(t *testing.T) {
	const secret = "plaintext-rotation-capability"
	now := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, secret, 500) }))
	defer srv.Close()
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	if err := client.Save(client.Config{Server: srv.URL, Token: "old", DeviceID: "device", PendingRotation: &client.PendingRotation{ID: "pending", NewToken: "new", CommitToken: secret, State: "prepared", ExpiresAt: now.Add(time.Minute)}}); err != nil {
		t.Fatal(err)
	}
	err := rotateDeviceLocked(context.Background(), time.Now)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("rotation error leaked secret: %v", err)
	}
}

func TestExpiredPendingRotationFinalizesAfterLostCommitResponse(t *testing.T) {
	now := time.Unix(1000, 0)
	prepareCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/expired/commit") {
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "committed"})
			return
		}
		prepareCalls++
		http.NotFound(w, r)
	}))
	defer srv.Close()
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	if err := client.Save(client.Config{Server: srv.URL, Token: "revoked-old", DeviceID: "device", PendingRotation: &client.PendingRotation{ID: "expired", NewToken: "already-active-new", CommitToken: "commit", State: "prepared", ExpiresAt: now.Add(-time.Minute)}}); err != nil {
		t.Fatal(err)
	}
	if err := rotateDeviceLocked(context.Background(), func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	got, err := client.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "already-active-new" || got.PendingRotation != nil || prepareCalls != 0 {
		t.Fatalf("config=%#v prepare_calls=%d", got, prepareCalls)
	}
}

func TestRawJSONErrorDoesNotEchoPasswordOrResponseBody(t *testing.T) {
	const secret = "plaintext-password-in-error"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, secret, 400) }))
	defer srv.Close()
	err := rawJSON(context.Background(), client.DefaultHTTPClient(), srv.URL, "", "POST", "/login", map[string]string{"password": secret}, nil, &map[string]any{})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("HTTP error leaked secret: %v", err)
	}
}

func TestConcurrentRotateCommandsLeaveUsableFinalCredential(t *testing.T) {
	var mu sync.Mutex
	current := "token-0"
	issued := map[string]string{}
	next := 0
	var active, maxActive atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nowActive := active.Add(1)
		defer active.Add(-1)
		for {
			old := maxActive.Load()
			if nowActive <= old || maxActive.CompareAndSwap(old, nowActive) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/rotation/prepare") {
			if r.Header.Get("Authorization") != "Bearer "+current {
				http.Error(w, "unauthorized", 401)
				return
			}
			next++
			rotation := fmt.Sprintf("rotation-%d", next)
			token := fmt.Sprintf("token-%d", next)
			issued[rotation] = token
			_ = json.NewEncoder(w).Encode(map[string]any{"rotation_id": rotation, "token": token, "commit_token": "commit-" + rotation, "expires_at": time.Now().Add(time.Minute)})
			return
		}
		for rotation, token := range issued {
			if strings.Contains(r.URL.Path, "/"+rotation+"/commit") {
				current = token
				_ = json.NewEncoder(w).Encode(map[string]any{"state": "committed"})
				return
			}
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	if err := client.Save(client.Config{Server: srv.URL, Token: current, DeviceID: "device"}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() { <-start; results <- rotateDevice(context.Background(), nil) }()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	got, err := client.Load()
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	final := current
	mu.Unlock()
	if got.Token != final || got.PendingRotation != nil || next != 2 {
		t.Fatalf("config=%#v server_token=%s rotations=%d", got, final, next)
	}
	if maxActive.Load() != 1 {
		t.Fatalf("concurrent rotation requests=%d", maxActive.Load())
	}
}

func TestReadHiddenPasswordFromNonTerminalStdin(t *testing.T) {
	saved := stdinLines
	t.Cleanup(func() { stdinLines = saved })
	stdinLines = bufio.NewReader(strings.NewReader("correct horse battery staple\ncorrect horse battery staple\n"))
	got, err := readHiddenPassword("Password: ", true)
	if err != nil || got != "correct horse battery staple" {
		t.Fatalf("password=%q err=%v", got, err)
	}
	stdinLines = bufio.NewReader(strings.NewReader("one\ntwo\n"))
	if _, err = readHiddenPassword("Password: ", true); err == nil {
		t.Fatal("mismatched confirmation accepted")
	}
}

func TestBootstrapIdentityMigratesAndCreatesExactlyOneAdmin(t *testing.T) {
	url := pgtest.NewDatabase(t)
	out, err := bootstrapIdentity(t.Context(), url, "Admin", "Admin@Example.test", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if out["token"] == "" || out["user"].(domain.User).Email != "admin@example.test" {
		t.Fatalf("bootstrap output=%#v", out)
	}
	if _, err = bootstrapIdentity(t.Context(), url, "Other", "other@example.test", "correct horse battery staple"); err == nil || !strings.Contains(err.Error(), "already has an administrator") {
		t.Fatalf("second bootstrap err=%v", err)
	}
	pool, err := pgxpool.New(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var admins, failures int
	if err = pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM users WHERE role='admin'), (SELECT count(*) FROM audit_events WHERE action='bootstrap.failed')`).Scan(&admins, &failures); err != nil || admins != 1 || failures != 1 {
		t.Fatalf("admins=%d failures=%d err=%v", admins, failures, err)
	}
}

// FLOPWIRE_BUS_RETENTION takes a duration or days; a value that does not
// parse, or is under an hour, stops the server rather than falling back.
func TestBusRetention(t *testing.T) {
	for v, want := range map[string]time.Duration{"": 7 * 24 * time.Hour, "7d": 7 * 24 * time.Hour, " 30d ": 30 * 24 * time.Hour, "168h": 168 * time.Hour, "1h": time.Hour} {
		if got, err := busRetention(v); err != nil || got != want {
			t.Errorf("busRetention(%q) = %v, %v; want %v", v, got, err, want)
		}
	}
	for _, v := range []string{"7", "seven days", "0d", "-1h", "30m", "1.5d"} {
		if got, err := busRetention(v); err == nil {
			t.Errorf("busRetention(%q) = %v, want an error", v, got)
		}
	}
}
