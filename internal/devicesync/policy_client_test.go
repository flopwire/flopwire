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
