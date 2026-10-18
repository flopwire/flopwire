package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/syncproto"
)

func TestSyncTransportKeepsInitialDeviceAcrossCredentialInstallPaths(t *testing.T) {
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	initial := client.Config{Server: "http://localhost:12345", DeviceID: "device-a", Token: "token-a"}
	for _, action := range []string{"refresh", "reload", "repin", "install"} {
		t.Run(action, func(t *testing.T) {
			saved := initial
			tr := newSyncTransport(initial, func() (client.Config, error) { return saved, nil }, quiet)
			saved.DeviceID, saved.Token = "device-b", "token-b"
			saved.TLSFingerprint = client.FingerprintPrefix + "changed"
			switch action {
			case "refresh":
				if tr.refresh(t.Context(), initial.Token) {
					t.Fatal("refreshed another device")
				}
			case "reload":
				tr.reload(t.Context())
			case "repin":
				if tr.repin() {
					t.Fatal("re-pinned another device")
				}
			case "install":
				if tr.install(saved) {
					t.Fatal("installed another device")
				}
			}
			got := tr.current()
			if got.Token != initial.Token || tr.pin != initial.TLSFingerprint {
				t.Fatal("initial device credential or pin changed")
			}
			saved.DeviceID = initial.DeviceID
			saved.TLSFingerprint = ""
			if !tr.refresh(t.Context(), initial.Token) || tr.current().Token != saved.Token {
				t.Fatal("same-device rotation was blocked")
			}
			saved.TLSFingerprint = client.FingerprintPrefix + "repinned"
			if !tr.repin() || tr.pin != saved.TLSFingerprint {
				t.Fatal("same-device repin was blocked")
			}
		})
	}
}

func TestSyncTransportRetryDoesNotCrossDeviceIdentity(t *testing.T) {
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	for _, changedDevice := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-device", true: "different-device"}[changedDevice], func(t *testing.T) {
			var mu sync.Mutex
			var saved client.Config
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				calls++
				if calls == 1 {
					saved.Token = "rotated-token"
					if changedDevice {
						saved.DeviceID = "device-b"
					}
					w.WriteHeader(http.StatusUnauthorized)
					_ = json.NewEncoder(w).Encode(syncproto.ErrorResponse{Code: "credential_invalid"})
					return
				}
				if r.Header.Get("Authorization") != "Bearer rotated-token" {
					t.Error("retry used wrong credential")
				}
				_ = json.NewEncoder(w).Encode(syncproto.HasResponse{})
			}))
			defer srv.Close()
			saved = client.Config{Server: srv.URL, DeviceID: "device-a", Token: "initial-token"}
			tr := newSyncTransport(saved, func() (client.Config, error) { mu.Lock(); defer mu.Unlock(); return saved, nil }, quiet)
			_, err := tr.Has(t.Context(), nil)
			mu.Lock()
			defer mu.Unlock()
			if changedDevice {
				if err == nil || calls != 1 || tr.current().Token != "initial-token" {
					t.Fatalf("cross-device retry: calls=%d err=%v", calls, err)
				}
			} else if err != nil || calls != 2 {
				t.Fatalf("same-device retry: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestDeviceBoundConfigFreezesEnvironmentAndRejectsDeviceChange(t *testing.T) {
	initial := client.Config{Server: "http://localhost:12345", DeviceID: "device-a", Token: "initial-token"}
	saved := initial
	load := deviceBoundConfig(initial, func() (client.Config, error) { return saved, nil })
	saved.DeviceID = "device-b"
	if _, err := load(); !errors.Is(err, errDeviceChanged) {
		t.Fatalf("changed device: %v", err)
	}
	initial.FromEnv = true
	envLoad := deviceBoundConfig(initial, func() (client.Config, error) { t.Fatal("environment token read saved config"); return saved, nil })
	if got, err := envLoad(); err != nil || got.Token != initial.Token {
		t.Fatalf("environment config: %+v %v", got, err)
	}
	tr := newSyncTransport(initial, envLoad, quiet)
	if tr.repin() || tr.install(saved) {
		t.Fatal("environment credential adopted saved credential")
	}
}
