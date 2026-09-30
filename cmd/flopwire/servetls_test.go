package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/retrieval/format"
)

func TestPlainHTTPListenerOnlyOnLoopback(t *testing.T) {
	dir := t.TempDir()
	for _, addr := range []string{"127.0.0.1:8080", "[::1]:8080", "localhost:0"} {
		s, err := resolveServerTLS(&tlsFlags{mode: "off", dir: dir}, addr)
		if err != nil || s.config != nil {
			t.Fatalf("--tls=off on %s: %+v %v", addr, s, err)
		}
	}
	for _, addr := range []string{":8080", "0.0.0.0:8080", "[::]:8080", "100.82.11.20:8080", "flopwire.example:8080"} {
		if _, err := resolveServerTLS(&tlsFlags{mode: "off", dir: dir}, addr); err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Fatalf("--tls=off on %s: err=%v", addr, err)
		}
	}
	// proxy mode serves plain HTTP anywhere, and says so loudly.
	s, err := resolveServerTLS(&tlsFlags{mode: "proxy", dir: dir}, ":8080")
	if err != nil || s.config != nil {
		t.Fatalf("--tls=proxy: %+v %v", s, err)
	}
	var banner strings.Builder
	s.announce(&banner, ":8080")
	if !strings.Contains(banner.String(), "PLAIN HTTP") {
		t.Fatalf("proxy banner: %q", banner.String())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("plain modes wrote TLS material: %v", entries)
	}
}

func TestDefaultModeIsPersistedSelfSignedAndDomainMeansACME(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	a, err := resolveServerTLS(&tlsFlags{mode: "auto", dir: dir}, ":8080")
	if err != nil || a.mode != tlsSelfSigned || a.config == nil {
		t.Fatalf("default: %+v %v", a, err)
	}
	b, err := resolveServerTLS(&tlsFlags{mode: "", dir: dir}, ":8080")
	if err != nil || b.fingerprint != a.fingerprint {
		t.Fatalf("restart changed the certificate: %s -> %s (%v)", a.fingerprint, b.fingerprint, err)
	}
	if got := client.Fingerprint(a.config.Certificates[0].Certificate[0]); got != a.fingerprint {
		t.Fatalf("fingerprint %s is not the served certificate's %s", a.fingerprint, got)
	}
	if st, err := os.Stat(filepath.Join(dir, "key.pem")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode: %v %v", st, err)
	}
	out := captureStdout(t, func() error { return fingerprintCmd([]string{"--tls-dir", dir}) })
	if strings.TrimSpace(out) != a.fingerprint {
		t.Fatalf("flopwire fingerprint printed %q, want %s", out, a.fingerprint)
	}
	// Half an identity is an error, never a silent replacement.
	if err := os.Remove(filepath.Join(dir, "cert.pem")); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveServerTLS(&tlsFlags{dir: dir}, ":8080"); err == nil {
		t.Fatal("a key without a certificate was replaced")
	}
	c, err := resolveServerTLS(&tlsFlags{mode: "auto", dir: t.TempDir(), domains: "flopwire.example.com"}, ":443")
	if err != nil || c.mode != tlsACME || c.config == nil || c.config.GetCertificate == nil {
		t.Fatalf("--domain: %+v %v", c, err)
	}
	if _, err := resolveServerTLS(&tlsFlags{mode: "acme", dir: t.TempDir()}, ":443"); err == nil {
		t.Fatal("acme without a domain")
	}
	if _, err := resolveServerTLS(&tlsFlags{mode: "bogus", dir: t.TempDir()}, ":443"); err == nil {
		t.Fatal("unknown mode")
	}
}

// selfSignedServer serves h over TLS with a fresh flopwire self-signed
// certificate, as `flopwire serve` does by default.
func selfSignedServer(t *testing.T, h http.Handler) (*httptest.Server, string) {
	t.Helper()
	s, err := resolveServerTLS(&tlsFlags{mode: "self-signed", dir: t.TempDir()}, ":0")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = s.config
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, s.fingerprint
}

func TestInviteCarriesPinThroughClaimEnrollAndServerQueries(t *testing.T) {
	srv, fp := selfSignedServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/admin/invites":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "one-time"})
		case "/v1/invites/claim":
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["code"] != "one-time" {
				http.Error(w, "bad code", 400)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "member-session", "user": map[string]any{"id": "u"}})
		case "/v1/devices":
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "member-device", "device": map[string]any{"id": "dev-1"}})
		case "/v1/search":
			_ = json.NewEncoder(w).Encode(map[string]any{"hits": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	// The admin pinned the server at bootstrap.
	adminCfg := filepath.Join(t.TempDir(), "admin.json")
	t.Setenv("FLOPWIRE_CONFIG", adminCfg)
	if err := client.Save(client.Config{Server: srv.URL, Token: "admin-session", SessionToken: "admin-session", TLSFingerprint: fp}); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() error { return invite(t.Context(), []string{"--email", "m@example.test"}) })
	var inv struct{ Code, Invite string }
	if err := json.Unmarshal([]byte(out), &inv); err != nil || inv.Invite == "" {
		t.Fatalf("invite output %q: %v", out, err)
	}
	parsed, err := client.ParseInvite(inv.Invite)
	if err != nil || parsed.Fingerprint != fp || parsed.Code != "one-time" || parsed.Server != srv.URL {
		t.Fatalf("invite %q parsed to %+v (%v), want pin %s", inv.Invite, parsed, err, fp)
	}

	// The member claims with the invite string alone and enrolls.
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "member.json"))
	p, err := claimInvite(inv.Invite, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() error { return claimAndSave(t.Context(), p.Server, p.Fingerprint, p.Code, "Member", "pw") })
	captureStdout(t, func() error { return enroll(t.Context(), []string{"--name", "laptop-b", "--platform", "test"}) })
	cfg, err := client.Load()
	if err != nil || cfg.TLSFingerprint != fp || cfg.DeviceID != "dev-1" {
		t.Fatalf("member config after claim and enroll: %+v %v", cfg, err)
	}
	// --server queries (and the raw fallback, which uses the same client)
	// verify the pin on every connection.
	c, err := serverClient()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Search(t.Context(), format.SearchQuery{Query: "x"}, format.Filters{}); err != nil {
		t.Fatalf("pinned --server search: %v", err)
	}
	cfg.TLSFingerprint = client.FingerprintPrefix + strings.Repeat("11", 32)
	if err := client.Save(cfg); err != nil {
		t.Fatal(err)
	}
	c, _ = serverClient()
	var pe *client.PinError
	if _, err := c.Search(t.Context(), format.SearchQuery{Query: "x"}, format.Filters{}); !errors.As(err, &pe) {
		t.Fatalf("--server search with a wrong pin: %v", err)
	}
}

func TestLoginRefusesMismatchedPinAndKeepsSavedPin(t *testing.T) {
	var logins atomic.Int32
	srv, fp := selfSignedServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logins.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "session", "user": map[string]any{}})
	}))
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	wrong := client.FingerprintPrefix + strings.Repeat("22", 32)
	var pe *client.PinError
	if err := loginAndSave(t.Context(), srv.URL, wrong, "a@example.test", "pw"); !errors.As(err, &pe) {
		t.Fatalf("login with a wrong pin: %v", err)
	}
	if err := loginAndSave(t.Context(), srv.URL, "", "a@example.test", "pw"); err == nil || !strings.Contains(err.Error(), "--fingerprint") {
		t.Fatalf("login to a self-signed server without a pin: %v", err)
	}
	if n := logins.Load(); n != 0 {
		t.Fatalf("password reached an unverified server %d times", n)
	}
	captureStdout(t, func() error { return loginAndSave(t.Context(), srv.URL, fp, "a@example.test", "pw") })
	// Logging in again without --fingerprint reuses the saved pin.
	captureStdout(t, func() error { return loginAndSave(t.Context(), srv.URL, "", "a@example.test", "pw") })
	if cfg, err := client.Load(); err != nil || cfg.TLSFingerprint != fp {
		t.Fatalf("config: %+v %v", cfg, err)
	}
	if n := logins.Load(); n != 2 {
		t.Fatalf("logins=%d", n)
	}
}

func TestHealthcheckProbesLoopbackTLS(t *testing.T) {
	srv, _ := selfSignedServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	if err := healthcheck(t.Context(), []string{"--url", srv.URL + "/readyz"}); err != nil {
		t.Fatalf("healthcheck over loopback TLS: %v", err)
	}
	if host, _, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://")); !client.IsLoopbackHost(host) {
		t.Fatalf("test server is not on loopback: %s", srv.URL)
	}
}

// In ACME mode the certificate is chosen by SNI, and a probe of
// https://127.0.0.1 sends none, so the healthcheck must name the domain.
func TestHealthcheckProbesACMEListenerWithDomainSNI(t *testing.T) {
	dir := t.TempDir()
	s, err := resolveServerTLS(&tlsFlags{mode: "auto", dir: dir, domains: "flopwire.example.test"}, ":8080")
	if err != nil || s.mode != tlsACME {
		t.Fatalf("acme: %+v %v", s, err)
	}
	// Seed the autocert cache so no ACME request is made.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"flopwire.example.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	cached := append(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	if err := os.WriteFile(filepath.Join(dir, "acme", "flopwire.example.test"), cached, 0o600); err != nil {
		t.Fatal(err)
	}
	// Serve s.config as flopwire serve does (httptest would add its own
	// certificate and hide a missing-SNI failure).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), TLSConfig: s.config}
	go hs.ServeTLS(ln, "", "")
	t.Cleanup(func() { hs.Close() })
	t.Setenv("FLOPWIRE_TLS", "auto")
	t.Setenv("FLOPWIRE_DOMAIN", "flopwire.example.test")
	if err := healthcheck(t.Context(), []string{"--url", "https://" + ln.Addr().String() + "/readyz"}); err != nil {
		t.Fatalf("healthcheck of an ACME listener over loopback: %v", err)
	}
}

// The agent fetches admin path rules through the pinned client: a
// self-signed server that matches the pin answers, and one that does not
// is refused.
// A pin saved for the same server while the agent runs (a re-pin) is used
// by the next fetch; a config for another server is not.
func TestAdminRulesPickUpRepin(t *testing.T) {
	srv, fp := selfSignedServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"path_rules": []string{"/secret/"}})
	}))
	stale := client.FingerprintPrefix + strings.Repeat("11", 32)
	start := client.Config{Server: srv.URL, Token: "dev", TLSFingerprint: stale}
	saved := start
	fetch := adminRulesFrom(start, func() (client.Config, error) { return saved, nil })
	if _, err := fetch(t.Context()); err == nil {
		t.Fatal("a stale pin was accepted")
	}
	saved.Server = "https://elsewhere.example"
	saved.TLSFingerprint = fp
	if _, err := fetch(t.Context()); err == nil {
		t.Fatal("a config saved for another server changed the pin")
	}
	saved.Server = srv.URL + "/" // the same server, spelled as login may save it
	got, err := fetch(t.Context())
	if err != nil || len(got.Rules) != 1 {
		t.Fatalf("fetch after the re-pin: %v %v", got, err)
	}
}

func TestAdminRulesUsePinnedClient(t *testing.T) {
	srv, fp := selfSignedServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"path_rules": []string{"/secret/"}})
	}))
	got, err := adminRules(client.Config{Server: srv.URL, Token: "dev", TLSFingerprint: fp})(t.Context())
	if err != nil || len(got.Rules) != 1 || got.Rules[0] != "/secret/" {
		t.Fatalf("pinned fetch: %v %v", got, err)
	}
	wrong := client.FingerprintPrefix + strings.Repeat("11", 32)
	if _, err := adminRules(client.Config{Server: srv.URL, Token: "dev", TLSFingerprint: wrong})(t.Context()); err == nil {
		t.Fatal("a pin mismatch was accepted")
	}
}
