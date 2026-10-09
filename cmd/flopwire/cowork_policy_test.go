package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/recoveryreceipt"
	"github.com/flopwire/flopwire/internal/syncproto"
)

func TestCoworkPolicyFollowsSameServerCredentialAndPin(t *testing.T) {
	var expectedToken string
	var posts int
	var calls atomic.Int64
	capability := devicesync.SupportedPolicyPlacementsVersion
	srv, fp := selfSignedServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+expectedToken {
			http.Error(w, "credential refused", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case syncproto.PathCapabilities:
			_ = json.NewEncoder(w).Encode(syncproto.CapabilitiesResponse{Version: syncproto.Version, PolicyPlacementsVersion: capability, MaxConcurrentFlushes: 1})
		case syncproto.PathPolicyPlacements:
			posts++
			var req syncproto.PolicyPlacementsRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			digest, err := syncproto.PolicyPlacementsDigest(&req)
			if err != nil {
				t.Error(err)
			}
			_ = json.NewEncoder(w).Encode(syncproto.PolicyPlacementsResponse{Version: devicesync.SupportedPolicyPlacementsVersion, Revision: 1, EvidenceScope: req.EvidenceScope, RequestDigest: digest})
		default:
			http.NotFound(w, r)
		}
	}))
	start := client.Config{Server: srv.URL, DeviceID: "device-a", Token: "old-token", TLSFingerprint: client.FingerprintPrefix + strings.Repeat("11", 32)}
	saved := start
	var loadErr error
	policy := coworkPolicyClient(start, func() (client.Config, error) { return saved, loadErr })
	req := &syncproto.PolicyPlacementsRequest{Version: devicesync.SupportedPolicyPlacementsVersion, Agent: "claude", SessionID: "synthetic-session", EvidenceScope: syncproto.EvidenceNone, ClientMode: syncproto.ClientModeLocal}
	expectedToken = start.Token
	if _, err := policy.PolicyPlacements(t.Context(), req); err == nil {
		t.Fatal("accepted a stale TLS pin")
	}
	saved.Server = "https://another.example"
	saved.Token, saved.TLSFingerprint = "new-token", fp
	if _, err := policy.PolicyPlacements(t.Context(), req); err == nil {
		t.Fatal("followed another server's pin")
	}
	saved.Server = srv.URL + "/"
	expectedToken = saved.Token
	if _, err := policy.PolicyPlacements(t.Context(), req); err != nil {
		t.Fatalf("same-server credential and pin refresh: %v", err)
	}
	saved.Token = "rotated-token"
	expectedToken = saved.Token
	if _, err := policy.PolicyPlacements(t.Context(), req); err != nil {
		t.Fatalf("credential rotation with unchanged pin: %v", err)
	}
	// A new call must inspect capabilities again, even after an acknowledgement.
	capability = 0
	if _, err := policy.PolicyPlacements(t.Context(), req); !errors.Is(err, devicesync.ErrPolicyUnsupported) {
		t.Fatalf("disabled capability: %v", err)
	}
	if posts != 2 {
		t.Fatalf("disabled capability sent metadata: %d posts", posts)
	}
	capability = devicesync.SupportedPolicyPlacementsVersion
	valid := saved
	validPolicy := coworkPolicyClient(valid, func() (client.Config, error) { return saved, loadErr })
	beforeRequests := calls.Load()
	saved.DeviceID, saved.Token = "device-b", "device-b-token"
	if _, err := policy.PolicyPlacements(t.Context(), req); !errors.Is(err, errDeviceChanged) {
		t.Fatalf("changed-device policy registration: %v", err)
	}
	if calls.Load() != beforeRequests {
		t.Fatal("changed-device policy registration sent HTTP")
	}
	saved.Server = "https://another.example"
	if _, err := policy.PolicyPlacements(t.Context(), req); !errors.Is(err, errDeviceChanged) || calls.Load() != beforeRequests {
		t.Fatalf("changed server and device sent policy HTTP: %v", err)
	}
	// A valid initial client cannot bypass an unreadable current config.
	saved = valid
	loadErr = errors.New("config unavailable")
	if _, err := validPolicy.PolicyPlacements(t.Context(), req); !errors.Is(err, loadErr) || calls.Load() != beforeRequests {
		t.Fatalf("unreadable config sent policy HTTP: %v", err)
	}
}

func TestSyncStartupUsesPolicyCredentialSnapshot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(dir, "config.json"))
	store, err := localindex.Open(filepath.Join(dir, "index.db"), localindex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	initial := client.Config{Server: "http://localhost:12345", DeviceID: "device-a", Token: "token-a"}
	saved := initial
	saved.DeviceID, saved.Token = "device-b", "token-b"
	reads := 0
	load := deviceBoundConfig(initial, func() (client.Config, error) { reads++; return saved, nil })
	_, tr, start, stop, err := startSyncFrom(t.Context(), store, dir, 1<<20, 1, syncproto.DeviceDirs{}, quiet, initial, load)
	if err != nil {
		t.Fatal(err)
	}
	start()
	defer stop()
	if reads != 0 || tr.current().Token != initial.Token {
		t.Fatal("sync construction replaced the policy credential snapshot")
	}
	if tr.refresh(t.Context(), initial.Token) || tr.current().Token != initial.Token {
		t.Fatal("sync adopted the device selected after policy construction")
	}
}

func TestRecoveredPolicySourcesRemainBoundAndFailClosedOnCorruption(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(dir, "config.json"))
	initial := client.Config{Server: "https://example.test", DeviceID: "device-a", Token: "token-a"}
	saved := initial
	var loadErr error
	read := recoveredPolicySources(initial, func() (client.Config, error) { return saved, loadErr })
	const nativeUUID = "11111111-2222-4333-8444-555555555555"
	if refs, err := read(t.Context(), nativeUUID); err != nil || len(refs) != 0 {
		t.Fatalf("fresh native session without recovery receipts: %+v %v", refs, err)
	}
	binding := recoveryreceipt.Binding{Server: initial.Server, DeviceID: initial.DeviceID}
	r := recoveryreceipt.Receipt{NativeSessionID: nativeUUID, OriginalPath: "/old/native.jsonl", Source: syncproto.PolicySource{Path: "/exports/recovery.jsonl", FileID: "fixture", Generation: 4}}
	if err := recoveryreceipt.Write(dir, binding, r); err != nil {
		t.Fatal(err)
	}
	saved.Token = "rotated-token"
	refs, err := read(t.Context(), nativeUUID)
	if err != nil || len(refs) != 1 || refs[0].Source != r.Source || refs[0].OriginalPath != r.OriginalPath {
		t.Fatalf("same-device recovered source: %+v %v", refs, err)
	}
	saved.DeviceID = "device-b"
	if _, err := read(t.Context(), nativeUUID); !errors.Is(err, errDeviceChanged) {
		t.Fatalf("changed-device receipt read: %v", err)
	}
	saved.DeviceID = initial.DeviceID
	loadErr = errors.New("config unavailable")
	if _, err := read(t.Context(), nativeUUID); !errors.Is(err, loadErr) {
		t.Fatalf("receipt loader error: %v", err)
	}
	loadErr = nil
	files, err := filepath.Glob(filepath.Join(dir, "recovery-policy-receipts", "*", nativeUUID, "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("synthetic receipt fixture: %v %v", files, err)
	}
	if err := os.WriteFile(files[0], []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := read(t.Context(), nativeUUID); err == nil || errors.Is(err, recoveryreceipt.ErrAbsent) {
		t.Fatalf("corrupt requested receipt became absence: %v", err)
	}
}
