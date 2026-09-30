package client

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSaveAtomicallyPersistsPendingRotationWithPrivateMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	t.Setenv("FLOPWIRE_CONFIG", path)
	want := Config{Server: "https://flopwire.test", Token: "old-token", DeviceID: "device", PendingRotation: &PendingRotation{ID: "rotation", NewToken: "new-token", CommitToken: "commit-token", State: "prepared", ExpiresAt: time.Now().Add(time.Minute)}}
	if err := Save(want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.PendingRotation == nil || got.PendingRotation.NewToken != want.PendingRotation.NewToken || got.PendingRotation.State != "prepared" || !got.PendingRotation.ExpiresAt.Equal(want.PendingRotation.ExpiresAt) || got.Token != "old-token" {
		t.Fatalf("round trip=%#v", got)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".config-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files remain: %v", matches)
	}
}

func TestConfigLockSerializesConcurrentRotations(t *testing.T) {
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	var active, maxActive atomic.Int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := WithConfigLock(context.Background(), func() error {
				current := active.Add(1)
				for {
					old := maxActive.Load()
					if current <= old || maxActive.CompareAndSwap(old, current) {
						break
					}
				}
				time.Sleep(10 * time.Millisecond)
				active.Add(-1)
				return nil
			}); err != nil {
				t.Errorf("lock: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if maxActive.Load() != 1 {
		t.Fatalf("concurrent holders=%d", maxActive.Load())
	}
}

func TestSessionCredentialNeverPromotesEnrolledDeviceToken(t *testing.T) {
	for _, tc := range []struct {
		name    string
		config  Config
		want    string
		wantErr bool
	}{
		{"separate session", Config{Token: "device", DeviceID: "device-id", SessionToken: "session"}, "session", false},
		{"legacy login", Config{Token: "session"}, "session", false},
		{"legacy enrolled device", Config{Token: "device", DeviceID: "device-id"}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.config.SessionCredential()
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("credential=%q err=%v", got, err)
			}
		})
	}
}

func TestNormalizeServerRecognizesEquivalentURLs(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{" HTTPS://Flopwire.Example:443/ ", "https://flopwire.example"},
		{"HTTPS://FLOPWIRE.EXAMPLE:443/api/", "https://flopwire.example/api"},
		{"https://[2001:DB8::1]:443/", "https://[2001:db8::1]"},
		{"http://LOCALHOST:80/", "http://localhost"},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"http://[::1]:8080/", "http://[::1]:8080"},
	} {
		got, err := NormalizeServer(tc.in)
		if err != nil || got != tc.want {
			t.Fatalf("normalize %q=%q want=%q err=%v", tc.in, got, tc.want, err)
		}
	}
	for _, invalid := range []string{
		"flopwire.example", "ftp://flopwire.example", "http://flopwire.example", "http://100.82.11.20:8080", "http://[2001:db8::1]", "https://user:secret@flopwire.example", "https://flopwire.example?q=1",
		"https://flopwire.example/base%2Fadmin", "https://flopwire.example/base%5Cadmin", `https://flopwire.example/base\admin`,
		"https://flopwire.example/base/../admin", "https://flopwire.example/base/%2e%2e/admin", "https://flopwire.example/base//admin",
	} {
		if _, err := NormalizeServer(invalid); err == nil {
			t.Fatalf("accepted invalid server %q", invalid)
		}
	}
}

// FLOPWIRE_TOKEN with FLOPWIRE_SERVER is a whole config: no file is read or
// written, and it never stands in for a login session.
func TestLoadFromEnvironmentWithoutConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "config.json")
	t.Setenv("FLOPWIRE_CONFIG", path)
	t.Setenv(EnvToken, "minted-token")
	t.Setenv(EnvServer, "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "FLOPWIRE_SERVER") {
		t.Fatalf("token without server: %v", err)
	}
	t.Setenv(EnvServer, "https://flopwire.example:8443/")
	t.Setenv(EnvFingerprint, "sha256:"+strings.Repeat("ab", 32))
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.FromEnv || cfg.Token != "minted-token" || cfg.Server != "https://flopwire.example:8443" || cfg.TLSFingerprint == "" {
		t.Fatalf("env config=%+v", cfg)
	}
	if _, err := cfg.SessionCredential(); err == nil {
		t.Fatal("a minted token served as a login session")
	}
	if err := Save(cfg); err == nil {
		t.Fatal("an environment token was saved")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("config file touched: %v", err)
	}
	if _, err := LoadFile(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadFile read something: %v", err)
	}
}
