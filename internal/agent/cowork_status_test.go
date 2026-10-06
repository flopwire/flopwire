package agent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/syncproto"
)

type statusPolicyClient struct {
	err   error
	alter func(*syncproto.PolicyPlacementsResponse)
}

func (c statusPolicyClient) PolicyPlacements(_ context.Context, r *syncproto.PolicyPlacementsRequest) (*syncproto.PolicyPlacementsResponse, error) {
	if c.err != nil {
		return nil, c.err
	}
	digest, err := syncproto.PolicyPlacementsDigest(r)
	if err != nil {
		return nil, err
	}
	ack := &syncproto.PolicyPlacementsResponse{Version: 1, Revision: 1, RequestDigest: digest, EvidenceScope: r.EvidenceScope, Allowed: true}
	if c.alter != nil {
		c.alter(ack)
	}
	return ack, nil
}

func TestCoworkStatusDistinguishesSchedulingFromFreshCaptureAuthorization(t *testing.T) {
	f, r, p, path := newCoworkLeaseFixture(t)
	st := f.a.coworkStatus()
	if st.ScheduleEligible != 1 || st.Held != 0 || st.SharedHold != "fresh server policy acknowledgement required for every capture" {
		t.Fatalf("known mapping status: %+v", st)
	}
	if st.LastPolicyAttempt.State != "acknowledged" {
		t.Fatalf("latest registration status: %+v", st.LastPolicyAttempt)
	}
	sp, _ := r.spec(path)
	// An old acknowledged registration remains diagnostic only. The next capture
	// must contact the server again and hold when that attempt fails.
	before := len(p.requests)
	p.fail = true
	if auth, err := r.authorize(ctx, sp); err == nil || auth != nil {
		t.Fatal("diagnostic ACK authorized later capture")
	}
	if len(p.requests) <= before {
		t.Fatal("capture skipped fresh policy attempt")
	}
	st = f.a.coworkStatus()
	if st.LastPolicyAttempt.State != "transport_unavailable" || st.ScheduleEligible != 1 || st.Held != 0 {
		t.Fatalf("transport diagnostic conflated scheduling and capture: %+v", st)
	}
}

func TestCoworkStatusPrimaryHoldReasons(t *testing.T) {
	for _, tc := range []struct {
		name, rule, reason string
		unknown            bool
	}{
		{name: "not configured", reason: "sharing_unconfigured"},
		{name: "local policy", rule: "local /host/public/private", reason: "policy_local"},
		{name: "deny policy", rule: "deny /host/public/private", reason: "policy_deny"},
		{name: "nested repositories", rule: "deny repo:example.com/private/*", reason: "repository_scope_unknown"},
		{name: "unknown current mapping", reason: "current_mapping_unknown", unknown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCoworkFixture(t)
			if tc.rule != "" {
				rules := filepath.Join(f.home, "rules")
				writeRules(t, rules, tc.rule)
				f.cfg.UserRules = rules
				f.restart()
			}
			var approved []string
			if tc.unknown {
				approved = []string{"/sessions/unresolved"}
			}
			coworkMetadata(t, f, []string{"/host/public"}, approved, nil)
			f.a.refreshCowork(ctx) // metadata alone must not invent historical captures
			st := f.a.coworkStatus()
			if st.Held != 1 || st.ScheduleEligible != 0 || st.HoldReasons[tc.reason] != 1 || len(st.HoldReasons) != 1 || st.HistoricalUnknown != 0 {
				t.Fatalf("hold classification: %+v", st)
			}
			if st.LastPolicyAttempt.State != "not_attempted" {
				t.Fatal("configuration invented a policy attempt")
			}
		})
	}
}

func TestCoworkLatestPolicyAttemptSanitizedStates(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		err         error
		alter       func(*syncproto.PolicyPlacementsResponse)
		adjust      func(*syncproto.PolicyPlacementsRequest)
	}{
		{name: "unsupported", state: "unsupported", err: devicesync.ErrPolicyUnsupported},
		{name: "transport limit", state: "limit_held", err: &devicesync.PolicyLimitHeldError{}},
		{name: "transport", state: "transport_unavailable", err: errors.New("private /secret/path transcript body")},
		{name: "invalid", state: "invalid_ack", alter: func(a *syncproto.PolicyPlacementsResponse) { a.RequestDigest = "invalid" }},
		{name: "server restriction", state: "server_restriction", alter: func(a *syncproto.PolicyPlacementsResponse) { a.Allowed = false }},
		{name: "historical", state: "history_unknown", alter: func(a *syncproto.PolicyPlacementsResponse) {
			a.Allowed = false
			a.EvidenceScope = syncproto.EvidenceUnmapped
		}},
		{name: "mapping", state: "current_mapping_unknown", adjust: func(r *syncproto.PolicyPlacementsRequest) { r.CurrentMappingKnown = false }, alter: func(a *syncproto.PolicyPlacementsResponse) { a.Allowed = false }},
		{name: "local", state: "policy_local", adjust: func(r *syncproto.PolicyPlacementsRequest) { r.ClientMode = syncproto.ClientModeLocal }, alter: func(a *syncproto.PolicyPlacementsResponse) { a.Allowed = false }},
		{name: "deny", state: "policy_deny", adjust: func(r *syncproto.PolicyPlacementsRequest) { r.ClientMode = syncproto.ClientModeDeny }, alter: func(a *syncproto.PolicyPlacementsResponse) { a.Allowed = false }},
		{name: "request limit", state: "limit_held", adjust: func(r *syncproto.PolicyPlacementsRequest) { r.ScopeStatus = syncproto.ScopeLimitHeld }, alter: func(a *syncproto.PolicyPlacementsResponse) { a.Allowed = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCoworkFixture(t)
			f.a.cfg.CoworkPolicy = statusPolicyClient{err: tc.err, alter: tc.alter}
			req := &syncproto.PolicyPlacementsRequest{Version: 1, Agent: "claude", SessionID: coworkNativeID, CurrentMappingKnown: true, ClientMode: syncproto.ClientModeAllow, EvidenceScope: syncproto.EvidenceNone}
			if tc.adjust != nil {
				tc.adjust(req)
			}
			f.a.captureScopeMu.Lock()
			_, _ = f.a.acknowledgeCoworkPolicy(ctx, req)
			f.a.captureScopeMu.Unlock()
			st := f.a.coworkStatus()
			if st.LastPolicyAttempt.State != tc.state || st.LastPolicyAttempt.At.IsZero() {
				t.Fatalf("attempt state: %+v", st.LastPolicyAttempt)
			}
			raw, err := json.Marshal(st)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "/secret/path") || strings.Contains(string(raw), "transcript body") {
				t.Fatal("diagnostic leaked arbitrary error content")
			}
		})
	}
}
