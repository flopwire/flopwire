package devicesync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/syncproto"
)

func policyRequest(mode, scope string) *syncproto.PolicyPlacementsRequest {
	return &syncproto.PolicyPlacementsRequest{Version: 1, Agent: "claude", SessionID: "synthetic-session", CurrentMappingKnown: true, EvidenceScope: scope, ClientMode: mode, Placements: []syncproto.PolicyPlacement{{CWD: "/synthetic/host"}}, Sources: []syncproto.PolicySource{{Path: "/does-not-exist/evidence.jsonl", FileID: "1:2", Generation: 3}}}
}

func TestPolicyClientMetadataWorksForEveryContentMode(t *testing.T) {
	for _, mode := range []string{syncproto.ClientModeAllow, syncproto.ClientModeLocal, syncproto.ClientModeDeny} {
		for _, scope := range []string{syncproto.EvidenceNone, syncproto.EvidenceMapped, syncproto.EvidenceUnmapped} {
			t.Run(mode+"/"+scope, func(t *testing.T) {
				calls := []string{}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls = append(calls, r.Method+" "+r.URL.Path)
					if r.Header.Get("Authorization") != "Bearer device-token" || r.Header.Get(syncproto.HeaderVersion) != "1" {
						t.Error("missing authenticated protocol headers")
					}
					switch r.URL.Path {
					case syncproto.PathCapabilities:
						if r.Method != http.MethodGet || r.ContentLength > 0 {
							t.Error("capabilities must be bodyless GET")
						}
						io.WriteString(w, `{"version":1,"policyplacements_version":1,"max_concurrent_flushes":1}`)
					case syncproto.PathPolicyPlacements:
						if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
							t.Error("placement request must be JSON POST")
						}
						raw, err := io.ReadAll(r.Body)
						if err != nil {
							t.Fatal(err)
						}
						var got syncproto.PolicyPlacementsRequest
						if err := json.Unmarshal(raw, &got); err != nil {
							t.Fatal(err)
						}
						if got.ClientMode != mode || got.EvidenceScope != scope || got.Sources[0].Path != "/does-not-exist/evidence.jsonl" {
							t.Errorf("metadata changed: %+v", got)
						}
						var fields map[string]json.RawMessage
						json.Unmarshal(raw, &fields)
						for _, key := range []string{"content", "payload", "messages", "chunks", "token", "device_id", "user_id"} {
							if _, ok := fields[key]; ok {
								t.Errorf("nonmetadata field %s sent", key)
							}
						}
						digest, err := syncproto.PolicyPlacementsDigest(&got)
						if err != nil {
							t.Fatal(err)
						}
						fmt.Fprintf(w, `{"version":1,"revision":7,"evidence_scope":%q,"allowed":false,"request_digest":%q}`, scope, digest)
					default:
						t.Errorf("unexpected request %s", r.URL.Path)
						w.WriteHeader(404)
					}
				}))
				defer server.Close()
				c := &PolicyClient{Server: server.URL + "/", Token: "device-token", HTTP: server.Client()}
				out, err := c.PolicyPlacements(context.Background(), policyRequest(mode, scope))
				if err != nil || out.Revision != 7 || out.Allowed {
					t.Fatalf("ack=%+v error=%v", out, err)
				}
				if strings.Join(calls, ",") != "GET /v1/sync/capabilities,POST /v1/sync/policyplacements" {
					t.Errorf("calls=%v", calls)
				}
			})
		}
	}
}

func TestPolicyClientCapabilitiesFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"old server", 404, `{"code":"not_found"}`},
		{"unavailable", 503, `{"code":"server_busy"}`},
		{"wrong sync version", 200, `{"version":2,"policyplacements_version":1,"max_concurrent_flushes":1}`},
		{"disabled policy version", 200, `{"version":1,"policyplacements_version":0,"max_concurrent_flushes":1}`},
		{"future policy version", 200, `{"version":1,"policyplacements_version":2,"max_concurrent_flushes":1}`},
		{"missing support", 200, `{"version":1,"max_concurrent_flushes":1}`},
		{"missing serial contract", 200, `{"version":1,"policyplacements_version":1}`},
		{"parallel contract", 200, `{"version":1,"policyplacements_version":1,"max_concurrent_flushes":2}`},
		{"malformed", 200, `{`},
		{"trailing JSON", 200, `{"version":1,"policyplacements_version":1,"max_concurrent_flushes":1}{}`},
		{"oversized", 200, strings.Repeat(" ", 64<<10) + `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts++
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			c := &PolicyClient{Server: server.URL, Token: "device-token", HTTP: server.Client()}
			if out, err := c.PolicyPlacements(context.Background(), policyRequest("local", "none")); err == nil || out != nil {
				t.Fatalf("unsupported capability accepted: %+v %v", out, err)
			}
			if posts != 0 {
				t.Errorf("%d metadata POSTs before compatible support", posts)
			}
		})
	}
}

func TestPolicyClientRequiresPinnedHTTPAndCredential(t *testing.T) {
	c := &PolicyClient{Server: "http://localhost", Token: "device-token"}
	if _, err := c.Capabilities(context.Background()); !errors.Is(err, syncproto.ErrNoHTTPClient) {
		t.Errorf("missing HTTP error=%v", err)
	}
	c.HTTP = &http.Client{}
	c.Token = ""
	if _, err := c.Capabilities(context.Background()); !errors.Is(err, ErrPolicyCredential) {
		t.Errorf("missing credential error=%v", err)
	}
	server := httptest.NewServer(http.NotFoundHandler())
	c.Server = server.URL
	c.Token = "device-token"
	c.HTTP = server.Client()
	server.Close()
	if out, err := c.Capabilities(context.Background()); err == nil || out != nil {
		t.Errorf("unreachable capability=%+v,%v", out, err)
	}
}

func TestPolicyClientRejectsInvalidRequestsAndAcknowledgements(t *testing.T) {
	for _, body := range []string{
		`{"version":0,"revision":1,"evidence_scope":"mapped","allowed":true}`,
		`{"version":1,"revision":0,"evidence_scope":"mapped","allowed":true}`,
		`{"version":1,"revision":1,"evidence_scope":"invalid","allowed":true}`,
		strings.Repeat(" ", syncproto.MaxPolicyPlacementsBytes) + `{}`,
	} {
		t.Run(fmt.Sprint(len(body), body[:min(len(body), 40)]), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == syncproto.PathCapabilities {
					io.WriteString(w, `{"version":1,"policyplacements_version":1,"max_concurrent_flushes":1}`)
				} else {
					writePolicyTestAck(w, r, body)
				}
			}))
			defer server.Close()
			c := &PolicyClient{Server: server.URL, Token: "device-token", HTTP: server.Client()}
			if out, err := c.PolicyPlacements(context.Background(), policyRequest("local", "mapped")); err == nil || out != nil {
				t.Fatalf("invalid ack accepted: %+v %v", out, err)
			}
		})
	}
	c := &PolicyClient{}
	for _, request := range []*syncproto.PolicyPlacementsRequest{nil, {}, policyRequest("invalid", "none"), policyRequest("deny", "invalid")} {
		if _, err := c.PolicyPlacements(context.Background(), request); err == nil {
			t.Error("invalid request accepted")
		}
	}
	request := policyRequest("deny", "mapped")
	request.Placements[0].CWD = strings.Repeat("x", syncproto.MaxPolicyPlacementsBytes)
	if _, err := c.PolicyPlacements(context.Background(), request); err == nil {
		t.Error("oversized request accepted")
	}
}

func TestPolicyClientRejectsInconsistentReadiness(t *testing.T) {
	for _, tc := range []struct {
		name, mode, scope, ackScope string
		known, allowed              bool
	}{
		{"unknown current", "allow", "mapped", "mapped", false, true},
		{"unmapped evidence", "allow", "unmapped", "unmapped", true, true},
		{"local content", "local", "mapped", "mapped", true, true},
		{"denied content", "deny", "mapped", "mapped", true, true},
		{"downgrade unmapped", "allow", "unmapped", "mapped", true, false},
		{"downgrade mapped", "allow", "mapped", "none", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == syncproto.PathCapabilities {
					io.WriteString(w, `{"version":1,"policyplacements_version":1,"max_concurrent_flushes":1}`)
				} else {
					writePolicyTestAck(w, r, fmt.Sprintf(`{"version":1,"revision":1,"evidence_scope":%q,"allowed":%v}`, tc.ackScope, tc.allowed))
				}
			}))
			defer server.Close()
			c := &PolicyClient{Server: server.URL, Token: "device-token", HTTP: server.Client()}
			request := policyRequest(tc.mode, tc.scope)
			request.CurrentMappingKnown = tc.known
			if out, err := c.PolicyPlacements(context.Background(), request); err == nil || out != nil {
				t.Fatalf("inconsistent readiness accepted: %+v,%v", out, err)
			}
		})
	}
}

func TestPolicyClientAcknowledgesMoreRestrictiveHistoricalEvidence(t *testing.T) {
	for _, scope := range []string{"none", "mapped"} {
		t.Run(scope, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == syncproto.PathCapabilities {
					io.WriteString(w, `{"version":1,"policyplacements_version":1,"max_concurrent_flushes":1}`)
				} else {
					writePolicyTestAck(w, r, `{"version":1,"revision":1,"evidence_scope":"unmapped","allowed":false}`)
				}
			}))
			defer server.Close()
			c := &PolicyClient{Server: server.URL, Token: "device-token", HTTP: server.Client()}
			out, err := c.PolicyPlacements(context.Background(), policyRequest("allow", scope))
			if err != nil || out.EvidenceScope != "unmapped" || out.Allowed {
				t.Fatalf("restrictive historical ack=%+v,%v", out, err)
			}
		})
	}
}

func TestPolicyClientAcceptsConsistentReadyAcknowledgement(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == syncproto.PathCapabilities {
			io.WriteString(w, `{"version":1,"policyplacements_version":1,"max_concurrent_flushes":1}`)
		} else {
			writePolicyTestAck(w, r, `{"version":1,"revision":9,"evidence_scope":"mapped","allowed":true}`)
		}
	}))
	defer server.Close()
	c := &PolicyClient{Server: server.URL, Token: "device-token", HTTP: server.Client()}
	out, err := c.PolicyPlacements(context.Background(), policyRequest("allow", "mapped"))
	if err != nil || !out.Allowed || out.Revision != 9 {
		t.Fatalf("consistent acknowledgement=%+v,%v", out, err)
	}
}

func writePolicyTestAck(w http.ResponseWriter, r *http.Request, body string) {
	var request syncproto.PolicyPlacementsRequest
	if json.NewDecoder(r.Body).Decode(&request) != nil {
		io.WriteString(w, body)
		return
	}
	if len(body) > syncproto.MaxPolicyPlacementsBytes {
		io.WriteString(w, body)
		return
	}
	var fields map[string]any
	if json.Unmarshal([]byte(body), &fields) != nil {
		io.WriteString(w, body)
		return
	}
	digest, err := syncproto.PolicyPlacementsDigest(&request)
	if err != nil {
		w.WriteHeader(500)
		return
	}
	fields["request_digest"] = digest
	json.NewEncoder(w).Encode(fields)
}

func TestPolicyClientRejectsMissingTamperedAndOtherBatchDigests(t *testing.T) {
	for _, kind := range []string{"missing", "tampered", "folder", "source generation", "mapping readiness", "parent identity"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == syncproto.PathCapabilities {
					io.WriteString(w, `{"version":1,"policyplacements_version":1,"max_concurrent_flushes":1}`)
					return
				}
				var request syncproto.PolicyPlacementsRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				digest := ""
				switch kind {
				case "tampered":
					digest = strings.Repeat("0", 64)
				case "folder":
					request.Placements[0].CWD = "/different/folder"
				case "source generation":
					request.Sources[0].Generation++
				case "mapping readiness":
					request.CurrentMappingKnown = false
				case "parent identity":
					request.ParentSessionID = "different-parent"
				}
				if kind != "missing" && kind != "tampered" {
					var err error
					digest, err = syncproto.PolicyPlacementsDigest(&request)
					if err != nil {
						t.Error(err)
						return
					}
				}
				fields := map[string]any{"version": 1, "revision": 1, "evidence_scope": "mapped", "allowed": true}
				if kind != "missing" {
					fields["request_digest"] = digest
				}
				json.NewEncoder(w).Encode(fields)
			}))
			defer server.Close()
			c := &PolicyClient{Server: server.URL, Token: "device-token", HTTP: server.Client()}
			request := policyRequest("allow", "mapped")
			request.ParentSessionID = "synthetic-parent"
			if out, err := c.PolicyPlacements(context.Background(), request); err == nil || out != nil {
				t.Fatalf("mismatched digest accepted: %+v,%v", out, err)
			}
		})
	}
}

func TestPolicyClientAcknowledgementUsesExactSubmittedSnapshot(t *testing.T) {
	request := policyRequest("allow", "mapped")
	request.ParentSessionID = "synthetic-parent"
	initialDigest, err := syncproto.PolicyPlacementsDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == syncproto.PathCapabilities {
			// The caller revises metadata after the transport took its snapshot.
			request.ParentSessionID = "new-parent"
			request.Placements[0].CWD = "/new/folder"
			request.Sources[0].Generation++
			io.WriteString(w, `{"version":1,"policyplacements_version":1,"max_concurrent_flushes":1}`)
			return
		}
		var got syncproto.PolicyPlacementsRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
			return
		}
		digest, err := syncproto.PolicyPlacementsDigest(&got)
		if err != nil {
			t.Error(err)
			return
		}
		if digest != initialDigest {
			t.Error("posted batch changed after capability roundtrip")
		}
		json.NewEncoder(w).Encode(syncproto.PolicyPlacementsResponse{Version: 1, Revision: 1, EvidenceScope: "mapped", Allowed: true, RequestDigest: digest})
	}))
	defer server.Close()
	c := &PolicyClient{Server: server.URL, Token: "device-token", HTTP: server.Client()}
	out, err := c.PolicyPlacements(context.Background(), request)
	if err != nil || out.RequestDigest != initialDigest {
		t.Fatalf("submitted snapshot acknowledgement=%+v,%v", out, err)
	}
}

func TestPolicyClientDeviceDirectoriesFreezeAndBindDigest(t *testing.T) {
	request := policyRequest("allow", "mapped")
	request.Device = &syncproto.DeviceDirs{Home: "/synthetic/home"}
	expected, err := syncproto.PolicyPlacementsDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == syncproto.PathCapabilities {
			request.Device.Home = "/different/home"
			io.WriteString(w, `{"version":1,"policyplacements_version":1,"max_concurrent_flushes":1}`)
			return
		}
		var got syncproto.PolicyPlacementsRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
			return
		}
		digest, err := syncproto.PolicyPlacementsDigest(&got)
		if err != nil {
			t.Error(err)
			return
		}
		if got.Device == nil || got.Device.Home != "/synthetic/home" || digest != expected {
			t.Error("device directories changed after authorization snapshot")
		}
		json.NewEncoder(w).Encode(syncproto.PolicyPlacementsResponse{Version: 1, Revision: 1, EvidenceScope: "mapped", Allowed: true, RequestDigest: digest})
	}))
	defer server.Close()
	c := &PolicyClient{Server: server.URL, Token: "device-token", HTTP: server.Client()}
	if out, err := c.PolicyPlacements(context.Background(), request); err != nil || out.RequestDigest != expected {
		t.Fatalf("frozen device ack=%+v,%v", out, err)
	}
}

func TestPolicyClientRejectsAcknowledgementForOtherDeviceHome(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == syncproto.PathCapabilities {
			io.WriteString(w, `{"version":1,"policyplacements_version":1,"max_concurrent_flushes":1}`)
			return
		}
		var got syncproto.PolicyPlacementsRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
			return
		}
		got.Device.Home = "/different/home"
		digest, err := syncproto.PolicyPlacementsDigest(&got)
		if err != nil {
			t.Error(err)
			return
		}
		json.NewEncoder(w).Encode(syncproto.PolicyPlacementsResponse{Version: 1, Revision: 1, EvidenceScope: "mapped", Allowed: true, RequestDigest: digest})
	}))
	defer server.Close()
	c := &PolicyClient{Server: server.URL, Token: "device-token", HTTP: server.Client()}
	request := policyRequest("allow", "mapped")
	request.Device = &syncproto.DeviceDirs{Home: "/synthetic/home"}
	if out, err := c.PolicyPlacements(context.Background(), request); err == nil || out != nil {
		t.Fatalf("wrong-home ack accepted: %+v,%v", out, err)
	}
}

func TestPolicyClientRecoverySourceSnapshotBindsExactRestriction(t *testing.T) {
	request := policyRequest("deny", "unmapped")
	request.RecoverySources = []syncproto.PolicyRecoverySource{{Source: syncproto.PolicySource{Path: "/synthetic/recovery/export.jsonl", FileID: "1:3", Generation: 0}, OriginalPath: "/synthetic/native/original.jsonl"}}
	expected, err := syncproto.PolicyPlacementsDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == syncproto.PathCapabilities {
			request.RecoverySources[0].Source.Generation++
			request.RecoverySources[0].OriginalPath = "/different/original.jsonl"
			io.WriteString(w, `{"version":1,"policyplacements_version":1,"max_concurrent_flushes":1}`)
			return
		}
		var got syncproto.PolicyPlacementsRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
			return
		}
		digest, err := syncproto.PolicyPlacementsDigest(&got)
		if err != nil {
			t.Error(err)
			return
		}
		if len(got.RecoverySources) != 1 || got.RecoverySources[0].Source.Generation != 0 || got.RecoverySources[0].OriginalPath != "/synthetic/native/original.jsonl" || digest != expected {
			t.Error("recovery restriction changed after transport snapshot")
		}
		json.NewEncoder(w).Encode(syncproto.PolicyPlacementsResponse{Version: 1, Revision: 1, EvidenceScope: "unmapped", Allowed: false, RequestDigest: digest})
	}))
	defer server.Close()
	c := &PolicyClient{Server: server.URL, Token: "device-token", HTTP: server.Client()}
	if out, err := c.PolicyPlacements(context.Background(), request); err != nil || out.RequestDigest != expected {
		t.Fatalf("frozen recovery restriction ack=%+v,%v", out, err)
	}
}

func TestPolicyClientRejectsAckForOtherRecoveryOriginalPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == syncproto.PathCapabilities {
			io.WriteString(w, `{"version":1,"policyplacements_version":1,"max_concurrent_flushes":1}`)
			return
		}
		var got syncproto.PolicyPlacementsRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
			return
		}
		got.RecoverySources[0].OriginalPath = "/different/original.jsonl"
		digest, err := syncproto.PolicyPlacementsDigest(&got)
		if err != nil {
			t.Error(err)
			return
		}
		json.NewEncoder(w).Encode(syncproto.PolicyPlacementsResponse{Version: 1, Revision: 1, EvidenceScope: "unmapped", Allowed: false, RequestDigest: digest})
	}))
	defer server.Close()
	c := &PolicyClient{Server: server.URL, Token: "device-token", HTTP: server.Client()}
	request := policyRequest("deny", "unmapped")
	request.RecoverySources = []syncproto.PolicyRecoverySource{{Source: syncproto.PolicySource{Path: "/synthetic/recovery/export.jsonl", FileID: "1:3", Generation: 0}, OriginalPath: "/synthetic/native/original.jsonl"}}
	if out, err := c.PolicyPlacements(context.Background(), request); err == nil || out != nil {
		t.Fatalf("different recovery original acknowledged: %+v,%v", out, err)
	}
}

func TestPolicyClientLimitHeldCompactsAndRevokesRecoveryBatches(t *testing.T) {
	for _, trigger := range []string{"bytes", "placements", "sources", "recovery", "server413", "server413 oversized body"} {
		t.Run(trigger, func(t *testing.T) {
			req := policyRequest("deny", "mapped")
			req.SessionID = "c6cb1b71-e23e-4482-9c48-65bfd186ac43"
			req.ParentSessionID = "e1318b2b-d299-4d0a-b4aa-90e7b2014824"
			req.Device = &syncproto.DeviceDirs{Home: "/synthetic/home", ClaudeProjects: "/synthetic/private"}
			recoveryCount := 2
			if trigger == "recovery" {
				recoveryCount = 513
			}
			for i := 0; i < recoveryCount; i++ {
				req.RecoverySources = append(req.RecoverySources, syncproto.PolicyRecoverySource{Source: syncproto.PolicySource{Path: fmt.Sprintf("/export/%d", i), Generation: int64(i)}, OriginalPath: "/old/native"})
			}
			switch trigger {
			case "bytes":
				req.Placements[0].CWD = strings.Repeat("x", syncproto.MaxPolicyPlacementsBytes)
			case "placements":
				req.Placements = make([]syncproto.PolicyPlacement, 257)
			case "sources":
				req.Sources = make([]syncproto.PolicySource, 257)
			case "server413":
				req.RecoverySources = req.RecoverySources[:2]
			}
			compactPosts, fullPosts, recovered, sourced, caps := 0, 0, 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == syncproto.PathCapabilities {
					caps++
					io.WriteString(w, `{"version":1,"policyplacements_version":1,"max_concurrent_flushes":1}`)
					return
				}
				raw, _ := io.ReadAll(r.Body)
				var got syncproto.PolicyPlacementsRequest
				if err := json.Unmarshal(raw, &got); err != nil {
					t.Error(err)
					return
				}
				if got.ScopeStatus == "" {
					fullPosts++
					w.WriteHeader(413)
					if trigger == "server413 oversized body" {
						io.WriteString(w, strings.Repeat("x", syncproto.MaxPolicyPlacementsBytes+1))
					}
					return
				}
				compactPosts++
				if len(raw) >= syncproto.MaxPolicyPlacementsBytes || len(got.RecoverySources) > 256 || len(got.Placements) != 0 || len(got.Sources) > 256 || got.CurrentMappingKnown || got.ClientMode != req.ClientMode || got.EvidenceScope != req.EvidenceScope || got.SessionID != req.SessionID || got.ParentSessionID != req.ParentSessionID || got.Device.Home != req.Device.Home || got.Device.ClaudeProjects != "" {
					t.Errorf("invalid compact restriction: %+v", got)
				}
				if compactPosts == 1 && (len(got.RecoverySources) != 0 || len(got.Sources) != 0) {
					t.Error("native revocation must precede recovery batches")
				}
				recovered += len(got.RecoverySources)
				sourced += len(got.Sources)
				digest, _ := syncproto.PolicyPlacementsDigest(&got)
				json.NewEncoder(w).Encode(syncproto.PolicyPlacementsResponse{Version: 1, Revision: int64(compactPosts), RequestDigest: digest, EvidenceScope: got.EvidenceScope})
			}))
			defer server.Close()
			c := &PolicyClient{Server: server.URL, Token: "device-token", HTTP: server.Client()}
			out, err := c.PolicyPlacements(context.Background(), req)
			var held *PolicyLimitHeldError
			if out != nil || !errors.Is(err, ErrPolicyLimitHeld) || !errors.As(err, &held) || !held.Reconciled || held.Cause != nil {
				t.Fatalf("out=%+v error=%v", out, err)
			}
			if recovered != len(req.RecoverySources) || sourced != len(req.Sources) || caps != compactPosts+fullPosts || compactPosts < 2 {
				t.Fatalf("caps=%d compact=%d full=%d recovered=%d", caps, compactPosts, fullPosts, recovered)
			}
			if strings.HasPrefix(trigger, "server413") != (fullPosts == 1) {
				t.Errorf("full posts=%d", fullPosts)
			}
		})
	}
}

func TestPolicyClientLimitHeldRejectsUnsafeCompactAcknowledgement(t *testing.T) {
	for _, kind := range []string{"digest", "revision", "allowed", "scope", "network", "capability", "batch failure"} {
		t.Run(kind, func(t *testing.T) {
			req := policyRequest("allow", "mapped")
			req.SessionID = "c6cb1b71-e23e-4482-9c48-65bfd186ac43"
			req.Device = &syncproto.DeviceDirs{Home: "/synthetic/home"}
			req.Placements = make([]syncproto.PolicyPlacement, 257)
			req.RecoverySources = []syncproto.PolicyRecoverySource{{OriginalPath: "/old/native", Source: syncproto.PolicySource{Path: "/export"}}}
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == syncproto.PathCapabilities {
					version := 1
					if kind == "capability" {
						version = 0
					}
					fmt.Fprintf(w, `{"version":1,"policyplacements_version":%d,"max_concurrent_flushes":1}`, version)
					return
				}
				posts++
				if kind == "network" || (kind == "batch failure" && posts == 2) {
					w.WriteHeader(503)
					return
				}
				var got syncproto.PolicyPlacementsRequest
				json.NewDecoder(r.Body).Decode(&got)
				digest, _ := syncproto.PolicyPlacementsDigest(&got)
				ack := syncproto.PolicyPlacementsResponse{Version: 1, Revision: 1, RequestDigest: digest, EvidenceScope: got.EvidenceScope}
				switch kind {
				case "digest":
					ack.RequestDigest = "wrong"
				case "revision":
					ack.Revision = 0
				case "allowed":
					ack.Allowed = true
				case "scope":
					ack.EvidenceScope = "none"
				}
				json.NewEncoder(w).Encode(ack)
			}))
			defer server.Close()
			c := &PolicyClient{Server: server.URL, Token: "device-token", HTTP: server.Client()}
			out, err := c.PolicyPlacements(context.Background(), req)
			var held *PolicyLimitHeldError
			if out != nil || !errors.As(err, &held) || held.Reconciled || held.Cause == nil {
				t.Fatalf("invalid revocation accepted: %+v %v", out, err)
			}
			if kind == "capability" && posts != 0 {
				t.Error("posted without supported capability")
			}
		})
	}
}

func TestPolicyClientLimitHeldCannotInventIdentityOrHome(t *testing.T) {
	for _, kind := range []string{"session", "parent", "agent", "home", "missing home", "oversized recovery"} {
		t.Run(kind, func(t *testing.T) {
			req := policyRequest("local", "none")
			req.SessionID = "c6cb1b71-e23e-4482-9c48-65bfd186ac43"
			req.Device = &syncproto.DeviceDirs{Home: "/synthetic/home"}
			req.Placements = make([]syncproto.PolicyPlacement, 257)
			switch kind {
			case "session":
				req.SessionID = "arbitrary"
			case "parent":
				req.ParentSessionID = "arbitrary"
			case "agent":
				req.Agent = "other"
			case "home":
				req.Device.Home = "relative"
			case "missing home":
				req.Device = nil
			case "oversized recovery":
				req.RecoverySources = []syncproto.PolicyRecoverySource{{OriginalPath: strings.Repeat("x", syncproto.MaxPolicyPlacementsBytes)}}
			}
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == syncproto.PathCapabilities {
					io.WriteString(w, `{"version":1,"policyplacements_version":1,"max_concurrent_flushes":1}`)
					return
				}
				posts++
				writePolicyTestAck(w, r, `{"version":1,"revision":1,"evidence_scope":"none","allowed":false}`)
			}))
			defer server.Close()
			c := &PolicyClient{Server: server.URL, Token: "device-token", HTTP: server.Client()}
			out, err := c.PolicyPlacements(context.Background(), req)
			var held *PolicyLimitHeldError
			if out != nil || !errors.As(err, &held) || held.Reconciled || held.Cause == nil {
				t.Fatalf("invalid compact accepted %+v %v", out, err)
			}
			if kind != "oversized recovery" && posts != 0 {
				t.Error("invented compact target posted")
			}
			if kind == "oversized recovery" && posts != 2 {
				t.Error("oversized recovery should preserve initial native revocation")
			}
		})
	}
}

func TestPolicyClientOrdinaryBytesStillOmitScopeStatus(t *testing.T) {
	req := policyRequest("local", "none")
	original, _ := json.Marshal(req)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == syncproto.PathCapabilities {
			io.WriteString(w, `{"version":1,"policyplacements_version":1,"max_concurrent_flushes":1}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if string(raw) != string(original) || strings.Contains(string(raw), "scope_status") {
			t.Error("ordinary request bytes changed")
		}
		digest, _ := syncproto.PolicyPlacementsDigest(req)
		json.NewEncoder(w).Encode(syncproto.PolicyPlacementsResponse{Version: 1, Revision: 1, EvidenceScope: "none", RequestDigest: digest})
	}))
	defer server.Close()
	c := &PolicyClient{Server: server.URL, Token: "device-token", HTTP: server.Client()}
	if _, err := c.PolicyPlacements(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyClientLimitHeldPreservesCurrentEvidenceScopeAndByteBounds(t *testing.T) {
	for _, scope := range []string{syncproto.EvidenceNone, syncproto.EvidenceMapped, syncproto.EvidenceUnmapped} {
		t.Run(scope, func(t *testing.T) {
			req := policyRequest("allow", scope)
			req.SessionID = "c6cb1b71-e23e-4482-9c48-65bfd186ac43"
			req.Device = &syncproto.DeviceDirs{Home: "/synthetic/home"}
			for i := 0; i < 250; i++ {
				req.RecoverySources = append(req.RecoverySources, syncproto.PolicyRecoverySource{OriginalPath: "/old/" + strings.Repeat("x", 8000), Source: syncproto.PolicySource{Path: fmt.Sprintf("/export/%d", i)}})
			}
			count, posts := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == syncproto.PathCapabilities {
					io.WriteString(w, `{"version":1,"policyplacements_version":1,"max_concurrent_flushes":1}`)
					return
				}
				posts++
				raw, _ := io.ReadAll(r.Body)
				var got syncproto.PolicyPlacementsRequest
				json.Unmarshal(raw, &got)
				if len(raw) >= syncproto.MaxPolicyPlacementsBytes || got.EvidenceScope != scope || got.ClientMode != "allow" || got.CurrentMappingKnown || got.ScopeStatus != syncproto.ScopeLimitHeld {
					t.Error("compact request changed history or exceeded byte bound")
				}
				count += len(got.RecoverySources)
				digest, _ := syncproto.PolicyPlacementsDigest(&got)
				json.NewEncoder(w).Encode(syncproto.PolicyPlacementsResponse{Version: 1, Revision: 1, RequestDigest: digest, EvidenceScope: got.EvidenceScope})
			}))
			defer server.Close()
			c := &PolicyClient{Server: server.URL, Token: "device-token", HTTP: server.Client()}
			_, err := c.PolicyPlacements(context.Background(), req)
			var held *PolicyLimitHeldError
			if !errors.As(err, &held) || !held.Reconciled || count != 250 || posts < 3 {
				t.Fatalf("error=%v count=%d posts=%d", err, count, posts)
			}
		})
	}
}

func TestPolicyClientExplicitLimitHoldNeverAcceptsAllowedAck(t *testing.T) {
	req := policyRequest("allow", "mapped")
	req.ScopeStatus = syncproto.ScopeLimitHeld
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == syncproto.PathCapabilities {
			io.WriteString(w, `{"version":1,"policyplacements_version":1,"max_concurrent_flushes":1}`)
			return
		}
		writePolicyTestAck(w, r, `{"version":1,"revision":1,"evidence_scope":"mapped","allowed":true}`)
	}))
	defer server.Close()
	c := &PolicyClient{Server: server.URL, Token: "device-token", HTTP: server.Client()}
	if out, err := c.PolicyPlacements(context.Background(), req); out != nil || err == nil {
		t.Fatalf("allowed explicit hold accepted: %+v %v", out, err)
	}
	req.ScopeStatus = "unknown"
	if out, err := c.PolicyPlacements(context.Background(), req); out != nil || err == nil {
		t.Fatalf("unknown status accepted: %+v %v", out, err)
	}
}

func TestPolicyClientLimitHeldPreservesPreviouslyAcknowledgedHistory(t *testing.T) {
	req := policyRequest("local", "none")
	req.SessionID = "c6cb1b71-e23e-4482-9c48-65bfd186ac43"
	req.Device = &syncproto.DeviceDirs{Home: "/synthetic/home"}
	req.Placements = make([]syncproto.PolicyPlacement, 257)
	req.RecoverySources = []syncproto.PolicyRecoverySource{{OriginalPath: "/old/native", Source: syncproto.PolicySource{Path: "/export"}}}
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == syncproto.PathCapabilities {
			io.WriteString(w, `{"version":1,"policyplacements_version":1,"max_concurrent_flushes":1}`)
			return
		}
		posts++
		scope := "unmapped"
		if posts > 1 {
			scope = "none"
		}
		writePolicyTestAck(w, r, fmt.Sprintf(`{"version":1,"revision":1,"evidence_scope":%q,"allowed":false}`, scope))
	}))
	defer server.Close()
	c := &PolicyClient{Server: server.URL, Token: "device-token", HTTP: server.Client()}
	_, err := c.PolicyPlacements(context.Background(), req)
	var held *PolicyLimitHeldError
	if !errors.As(err, &held) || held.Reconciled || held.Cause == nil || posts != 2 {
		t.Fatalf("history weakened: %v posts=%d", err, posts)
	}
}

func TestPolicyClientLimitHeldChildAndSourceRestrictions(t *testing.T) {
	for _, parent := range []string{"c6cb1b71-e23e-4482-9c48-65bfd186ac43", "agent-parent_2", ""} {
		t.Run("parent="+parent, func(t *testing.T) {
			req := policyRequest("deny", "mapped")
			req.SessionID, req.ParentSessionID = "agent-child_1", parent
			req.Device = &syncproto.DeviceDirs{Home: "/synthetic/home"}
			req.Placements = make([]syncproto.PolicyPlacement, 257)
			req.Sources = nil
			for i := 0; i < 513; i++ {
				req.Sources = append(req.Sources, syncproto.PolicySource{Path: fmt.Sprintf("/native/legacy-%d.jsonl", i), FileID: fmt.Sprint(i), Generation: int64(i)})
			}
			for i := 0; i < 300; i++ {
				req.RecoverySources = append(req.RecoverySources, syncproto.PolicyRecoverySource{Source: syncproto.PolicySource{Path: fmt.Sprintf("/export/%d", i)}, OriginalPath: "/old/native"})
			}
			sources, recoveries, posts := 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == syncproto.PathCapabilities {
					io.WriteString(w, `{"version":1,"policyplacements_version":1,"max_concurrent_flushes":1}`)
					return
				}
				raw, _ := io.ReadAll(r.Body)
				var got syncproto.PolicyPlacementsRequest
				json.Unmarshal(raw, &got)
				posts++
				if got.SessionID != req.SessionID || got.ParentSessionID != parent || got.ScopeStatus != syncproto.ScopeLimitHeld || got.CurrentMappingKnown || got.ClientMode != "deny" || len(got.Sources) > 256 || len(got.RecoverySources) > 256 || len(raw) >= syncproto.MaxPolicyPlacementsBytes {
					t.Errorf("invalid child/source restriction: %+v", got)
				}
				if posts == 1 && (len(got.Sources) > 0 || len(got.RecoverySources) > 0) {
					t.Error("native hold must be first")
				}
				for _, src := range got.Sources {
					if src != req.Sources[sources] {
						t.Error("actual source identity changed")
					}
					sources++
				}
				for _, src := range got.RecoverySources {
					if src != req.RecoverySources[recoveries] {
						t.Error("recovery identity changed")
					}
					recoveries++
				}
				digest, _ := syncproto.PolicyPlacementsDigest(&got)
				json.NewEncoder(w).Encode(syncproto.PolicyPlacementsResponse{Version: 1, Revision: int64(posts), EvidenceScope: got.EvidenceScope, RequestDigest: digest})
			}))
			defer server.Close()
			c := &PolicyClient{Server: server.URL, Token: "device-token", HTTP: server.Client()}
			out, err := c.PolicyPlacements(context.Background(), req)
			var held *PolicyLimitHeldError
			if out != nil || !errors.As(err, &held) || !held.Reconciled || sources != 513 || recoveries != 300 {
				t.Fatalf("out=%+v err=%v sources=%d recovered=%d", out, err, sources, recoveries)
			}
		})
	}
}

func TestPolicyClientLimitHeldStrictChildIdentity(t *testing.T) {
	for _, id := range []string{"agent-", "agent-child/x", "agent-child.id", "agent-" + strings.Repeat("x", 129), "C6CB1B71-E23E-4482-9C48-65BFD186AC43"} {
		if _, err := canonicalPolicyID(id); err == nil {
			t.Errorf("malformed identity accepted %q", id)
		}
	}
	req := policyRequest("local", "none")
	req.SessionID = "agent-child"
	req.ParentSessionID = "agent-child"
	req.Device = &syncproto.DeviceDirs{Home: "/synthetic/home"}
	req.Placements = make([]syncproto.PolicyPlacement, 257)
	if _, err := (&PolicyClient{}).PolicyPlacements(context.Background(), req); err == nil || !strings.Contains(err.Error(), "parent must differ") {
		t.Fatalf("self-parent accepted: %v", err)
	}
}

func TestPolicyClientLimitHeldSourceByteBoundAndAckFailure(t *testing.T) {
	for _, failSourceAck := range []bool{false, true} {
		t.Run(fmt.Sprint(failSourceAck), func(t *testing.T) {
			req := policyRequest("local", "mapped")
			req.SessionID = "agent-child"
			req.ParentSessionID = "agent-parent"
			req.Device = &syncproto.DeviceDirs{Home: "/synthetic/home"}
			req.Sources = nil
			for i := 0; i < 250; i++ {
				req.Sources = append(req.Sources, syncproto.PolicySource{Path: fmt.Sprintf("/native/%d/", i) + strings.Repeat("x", 8000), FileID: fmt.Sprint(i), Generation: int64(i)})
			}
			sources, posts := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == syncproto.PathCapabilities {
					io.WriteString(w, `{"version":1,"policyplacements_version":1,"max_concurrent_flushes":1}`)
					return
				}
				raw, _ := io.ReadAll(r.Body)
				var got syncproto.PolicyPlacementsRequest
				json.Unmarshal(raw, &got)
				posts++
				if len(raw) >= syncproto.MaxPolicyPlacementsBytes || len(got.Sources) > 256 {
					t.Error("source batch exceeded bounds")
				}
				sources += len(got.Sources)
				digest, _ := syncproto.PolicyPlacementsDigest(&got)
				if failSourceAck && posts == 2 {
					digest = "wrong"
				}
				json.NewEncoder(w).Encode(syncproto.PolicyPlacementsResponse{Version: 1, Revision: int64(posts), EvidenceScope: got.EvidenceScope, RequestDigest: digest})
			}))
			defer server.Close()
			c := &PolicyClient{Server: server.URL, Token: "device-token", HTTP: server.Client()}
			out, err := c.PolicyPlacements(context.Background(), req)
			var held *PolicyLimitHeldError
			if out != nil || !errors.As(err, &held) || held.Reconciled == failSourceAck {
				t.Fatalf("out=%+v err=%v", out, err)
			}
			if !failSourceAck && (sources != 250 || posts < 3) {
				t.Errorf("lost sources=%d posts=%d", sources, posts)
			}
			if failSourceAck && posts != 2 {
				t.Error("continued after unverified source acknowledgement")
			}
		})
	}
}
