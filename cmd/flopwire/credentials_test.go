package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/api"
	"github.com/flopwire/flopwire/internal/auth"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/domain"
	"github.com/flopwire/flopwire/internal/ingest"
	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/flopwire/flopwire/internal/retrieval"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const adminPassword = "correct horse battery staple"

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// seedAdmin bootstraps the first administrator in s and returns a login
// session token.
func seedAdmin(t *testing.T, s store.Store) (userID, session string) {
	t.Helper()
	now := time.Now().UTC()
	hash, err := auth.HashPassword(adminPassword)
	if err != nil {
		t.Fatal(err)
	}
	plain, tokenHash, _ := auth.NewToken()
	u := domain.User{ID: uuid.NewString(), Email: "admin@example.test", Name: "Admin", Role: domain.RoleAdmin, IdentityType: domain.IdentityHuman, PasswordHash: hash, CreatedAt: now}
	c := domain.Credential{ID: uuid.NewString(), UserID: u.ID, Kind: domain.CredentialSession, TokenHash: tokenHash, ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now, Active: true}
	if err := s.BootstrapIdentity(context.Background(), u, c, domain.AuditEvent{ID: uuid.NewString(), ActorID: u.ID, Action: "bootstrap", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	return u.ID, plain
}

func apiCall(t *testing.T, server, token, method, path string, in any) map[string]any {
	t.Helper()
	var out map[string]any
	if err := (client.HTTP{Server: server, Token: token}).JSON(context.Background(), method, path, in, &out); err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return out
}

// The agent rotates its device credential daily. When a thief holding a
// copy of the token rotates first, the agent's next rotation is refused:
// it records that a re-login is required, sync stops, and `flopwire agent
// status` says so. `flopwire login` renews the same device and locks the
// thief out.
func TestAgentRotationStolenTokenRequiresRelogin(t *testing.T) {
	s := store.NewMemory()
	srv := httptest.NewServer(api.New(s, api.Config{Logger: quiet}).Handler(nil))
	defer srv.Close()
	_, admin := seedAdmin(t, s)
	invite := apiCall(t, srv.URL, admin, "POST", "/v1/admin/invites", map[string]string{"email": "gary@example.test"})
	claimed := apiCall(t, srv.URL, "", "POST", "/v1/invites/claim", map[string]string{"code": invite["code"].(string), "name": "Gary", "password": adminPassword})
	session := claimed["token"].(string)
	enrolled := apiCall(t, srv.URL, session, "POST", "/v1/devices", map[string]string{"name": "mac", "platform": "darwin"})
	deviceID := enrolled["device"].(map[string]any)["id"].(string)
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv(client.EnvToken, "")
	if err := client.Save(client.Config{Server: srv.URL, Token: enrolled["token"].(string), SessionToken: session, DeviceID: deviceID, RotatedAt: time.Now().Add(-25 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Happy path: due, rotated, not due again.
	if !rotateIfDue(ctx, nil, time.Now, quiet) {
		t.Fatal("a day-old credential was not rotated")
	}
	cfg, _ := client.LoadFile()
	if cfg.Token == enrolled["token"] || time.Since(cfg.RotatedAt) > time.Minute || time.Until(cfg.CredentialExpiresAt) < 89*24*time.Hour {
		t.Fatalf("after rotation: %+v", cfg)
	}
	if rotateIfDue(ctx, nil, time.Now, quiet) {
		t.Fatal("rotated twice in a day")
	}
	var b strings.Builder
	credentialStatus(&b, agent.Credential{Source: credDevice}, cfg, time.Now())
	if !strings.HasPrefix(b.String(), "credential: device login; ok") {
		t.Fatalf("status %q", b.String())
	}

	// The thief rotates the copied token first.
	prepared := apiCall(t, srv.URL, cfg.Token, "POST", "/v1/devices/"+deviceID+"/rotation/prepare", map[string]any{})
	apiCall(t, srv.URL, "", "POST", "/v1/device-rotations/"+prepared["rotation_id"].(string)+"/commit", map[string]string{"commit_token": prepared["commit_token"].(string)})
	thief := prepared["token"].(string)
	cfg.RotatedAt = time.Now().Add(-25 * time.Hour)
	if err := client.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if rotateIfDue(ctx, nil, time.Now, quiet) {
		t.Fatal("rotation succeeded after the thief rotated")
	}
	cfg, _ = client.LoadFile()
	if cfg.ReloginRequired != "credential_rotated" {
		t.Fatalf("relogin=%q", cfg.ReloginRequired)
	}
	b.Reset()
	credentialStatus(&b, agent.Credential{Source: credDevice}, cfg, time.Now())
	if !strings.Contains(b.String(), "re-login required (credential_rotated)") {
		t.Fatalf("status %q", b.String())
	}
	// Sync with the refused token stops (a permanent error) and says why.
	tr := newSyncTransport(cfg, client.Load, quiet)
	_, err := tr.Has(ctx, nil)
	if !syncproto.Permanent(err) || !strings.Contains(err.Error(), reloginPrefix) {
		t.Fatalf("sync with a refused credential: %v", err)
	}
	b.Reset()
	printAgentStatus(&b, agent.Response{Sync: &devicesync.Status{Stopped: err.Error()}})
	if !strings.Contains(b.String(), "sync: stopped: re-login required") {
		t.Fatalf("agent status %q", b.String())
	}

	// The owner logs in: same device, fresh credential, thief locked out.
	if err := loginAndSave(ctx, srv.URL, "", "gary@example.test", adminPassword); err != nil {
		t.Fatal(err)
	}
	cfg, _ = client.LoadFile()
	if cfg.ReloginRequired != "" || cfg.DeviceID != deviceID || cfg.Token == thief {
		t.Fatalf("after login: %+v", cfg)
	}
	if !tr.repin() {
		t.Fatal("sync did not pick up the renewed credential")
	}
	var he *syncproto.HTTPError
	if _, err := tr.Has(ctx, nil); !errors.As(err, &he) || he.Status != 501 {
		t.Fatalf("sync after login: %v", err) // no sync backend in this server: 501 means authenticated
	}
	var apiErr *client.APIError
	if err := (client.HTTP{Server: srv.URL, Token: thief}).JSON(ctx, "GET", "/v1/policy", nil, &map[string]any{}); !errors.As(err, &apiErr) || apiErr.StatusCode != 401 {
		t.Fatalf("thief after login: %v", err)
	}
}

// A sandbox with only FLOPWIRE_TOKEN and FLOPWIRE_SERVER (no config file)
// uploads with `flopwire agent run --once`; the uploads belong to the
// minting user under an ephemeral device with the token's label. A read
// token reads through the CLI's server client; an upload token cannot.
func TestEnvTokenAgentUploadWithoutConfigFile(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, pgtest.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	mc, bucket := pgtest.NewBucket(t)
	objects := ingest.MinIO{Client: mc, Bucket: bucket}
	pg := store.NewPostgres(pool, mc, bucket)
	queue := &ingest.Queue{Pool: pool, Objects: objects, Log: quiet}
	srv := httptest.NewServer(api.New(pg, api.Config{Logger: quiet, Sync: &ingest.Server{Pool: pool, Objects: objects, Log: quiet, Queue: queue},
		Parse: queue, Retrieval: &retrieval.Store{Pool: pool, Objects: objects}}).Handler(nil))
	defer srv.Close()
	adminID, admin := seedAdmin(t, pg)
	upload := apiCall(t, srv.URL, admin, "POST", "/v1/tokens", map[string]any{"ttl_seconds": 7200, "scopes": []string{"upload"}, "label": "ci-sandbox"})["token"].(string)
	read := apiCall(t, srv.URL, admin, "POST", "/v1/tokens", map[string]any{"scopes": []string{"read"}, "label": "ci-reader"})["token"].(string)

	home := filepath.Join(t.TempDir(), "home")
	if err := os.CopyFS(home, os.DirFS("../../testdata/oracle/home")); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "flopwire", "config.json")
	t.Setenv("FLOPWIRE_CONFIG", configPath)
	t.Setenv("FLOPWIRE_INDEX", filepath.Join(t.TempDir(), "index.db"))
	t.Setenv(client.EnvServer, srv.URL)
	t.Setenv(client.EnvToken, upload)
	sock, err := os.MkdirTemp("", "tm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sock)
	if err := run(ctx, []string{"agent", "run", "--once", "--desktop-code-root", "-", "--cowork-root", "-", "--claude-projects", filepath.Join(home, ".claude", "projects"), "--codex-home", filepath.Join(home, ".codex"),
		"--devin-db", "-", "--opencode-db", "-", "--mem-limit", "0", "--gc-percent", "100", "--socket", filepath.Join(sock, "a.sock"), "--sync-timeout", "2m"}); err != nil {
		t.Fatal(err)
	}
	if err := queue.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a config file was written: %v", err)
	}
	var conversations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversations c JOIN devices d ON d.id=c.device_id
		WHERE c.user_id=$1 AND d.kind='ephemeral' AND d.label='ci-sandbox'`, adminID).Scan(&conversations); err != nil {
		t.Fatal(err)
	}
	if conversations == 0 {
		t.Fatal("the sandbox's sessions were not uploaded under its ephemeral device")
	}

	// The CLI's server client authenticates from the environment too.
	t.Setenv(client.EnvToken, read)
	c, err := serverClient()
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := c.Sessions(ctx, "", "", format.Filters{})
	if err != nil || len(sessions.Sessions) == 0 {
		t.Fatalf("read token sessions: %v %+v", err, sessions)
	}
	t.Setenv(client.EnvToken, upload)
	c, _ = serverClient()
	var apiErr *client.APIError
	if _, err := c.Sessions(ctx, "", "", format.Filters{}); !errors.As(err, &apiErr) || apiErr.StatusCode != 403 {
		t.Fatalf("upload token read: %v", err)
	}
	// Minting from a minted token is refused before any request.
	if err := tokenCmd(ctx, []string{"mint", "--label", "x"}); err == nil || !strings.Contains(err.Error(), "cannot mint") {
		t.Fatalf("mint from FLOPWIRE_TOKEN: %v", err)
	}
}

func TestCredentialStatusWarnsBeforeDeadlines(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		cfg  client.Config
		want string
	}{
		{client.Config{DeviceID: "d", CredentialExpiresAt: now.Add(6 * 24 * time.Hour), IdleExpiresAt: now.Add(29 * 24 * time.Hour)}, "interactive re-login due by"},
		{client.Config{DeviceID: "d", CredentialExpiresAt: now.Add(60 * 24 * time.Hour), IdleExpiresAt: now.Add(5 * 24 * time.Hour)}, "idle limit reached"},
		{client.Config{DeviceID: "d", CredentialExpiresAt: now.Add(60 * 24 * time.Hour), IdleExpiresAt: now.Add(29 * 24 * time.Hour)}, "credential: device login; ok"},
		{client.Config{DeviceID: "d", ReloginRequired: "reauth_required"}, "re-login required (reauth_required)"},
		{client.Config{FromEnv: true}, "credential: FLOPWIRE_TOKEN (a minted token"},
	} {
		var b strings.Builder
		src := agent.Credential{Source: credDevice}
		if c.cfg.FromEnv {
			src.Source = credEnv
		}
		credentialStatus(&b, src, c.cfg, now)
		if !strings.Contains(b.String(), c.want) {
			t.Errorf("%+v: %q lacks %q", c.cfg, b.String(), c.want)
		}
	}
}

func TestWriteDevices(t *testing.T) {
	now := time.Now()
	var b strings.Builder
	writeDevices(&b, []domain.Device{
		{ID: "d1", UserEmail: "a@x.test", Name: "mac", Kind: domain.DeviceEnrolled, CreatedAt: now, LastSeen: now, LastIP: "10.0.0.2", ExpiresAt: now.Add(time.Hour), Scopes: []string{"upload", "read"}},
		{ID: "d2", UserEmail: "a@x.test", Label: "ci-17", Kind: domain.DeviceEphemeral, CreatedAt: now, ExpiresAt: now.Add(-time.Minute), Scopes: []string{"upload"}},
	}, now)
	out := b.String()
	for _, want := range []string{"LAST IP", "10.0.0.2", "ci-17", "ephemeral", "expired", "upload,read"} {
		if !strings.Contains(out, want) {
			t.Errorf("device list %q lacks %q", out, want)
		}
	}
}
