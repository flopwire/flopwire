package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

type cancelingCoworkPolicy struct {
	*coworkPolicyRecorder
	cancel context.CancelFunc
}

func (c *cancelingCoworkPolicy) PolicyPlacements(ctx context.Context, req *syncproto.PolicyPlacementsRequest) (*syncproto.PolicyPlacementsResponse, error) {
	ack, err := c.coworkPolicyRecorder.PolicyPlacements(ctx, req)
	if c.cancel != nil {
		c.cancel()
		return nil, context.Canceled
	}
	return ack, err
}

func TestCoworkReconcileRotatesPastSlowFailedPrefix(t *testing.T) {
	f := newCoworkFixture(t)
	r := &cancelingCoworkPolicy{coworkPolicyRecorder: &coworkPolicyRecorder{}}
	f.cfg.CoworkPolicy = r
	f.restart()
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("11111111-2222-4333-8444-%012d", i)
		if err := f.a.saveCoworkPlace(ctx, placeKey{transcript.AgentClaude, id}, placed{how: localindex.PlacedByCowork}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 3; i++ {
		pass, cancel := context.WithCancel(ctx)
		r.cancel = cancel
		if err := f.a.reconcileCoworkPolicy(pass); err == nil {
			t.Fatal("expected interrupted metadata pass")
		}
		cancel()
		want := fmt.Sprintf("11111111-2222-4333-8444-%012d", i)
		if len(r.requests) != i || r.requests[i-1].SessionID != want {
			t.Fatalf("pass%d requests=%+v", i, r.requests)
		}
	}
	// Completing one loop wraps fairly without losing failed keys.
	r.cancel, r.requests = nil, nil
	if err := f.a.reconcileCoworkPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	if len(r.requests) != 3 || r.requests[0].SessionID != "11111111-2222-4333-8444-000000000001" {
		t.Fatalf("wrapped requests=%+v", r.requests)
	}
}

func TestCoworkInjectedClientCannotAllowLimitHeldScope(t *testing.T) {
	f := newCoworkFixture(t)
	f.a.cfg.CoworkPolicy = &coworkPolicyRecorder{alter: func(ack *syncproto.PolicyPlacementsResponse) { ack.Allowed = true }}
	req := &syncproto.PolicyPlacementsRequest{Version: 1, Agent: "claude", SessionID: coworkNativeID, CurrentMappingKnown: true, EvidenceScope: syncproto.EvidenceMapped, ClientMode: syncproto.ClientModeAllow, ScopeStatus: syncproto.ScopeLimitHeld}
	if _, err := f.a.acknowledgeCoworkPolicy(ctx, req); err == nil {
		t.Fatal("custom client allowed a durable limit hold")
	}
}

func TestCoworkServerHistoricalUnknownPersistsWithoutLocalEvidence(t *testing.T) {
	f := newCoworkFixture(t)
	key := placeKey{transcript.AgentClaude, coworkNativeID}
	if err := f.a.saveCoworkPlace(ctx, key, placed{how: localindex.PlacedByCowork}); err != nil {
		t.Fatal(err)
	}
	f.a.cfg.CoworkPolicy = &coworkPolicyRecorder{alter: func(ack *syncproto.PolicyPlacementsResponse) {
		ack.EvidenceScope = syncproto.EvidenceUnmapped
	}}
	req := &syncproto.PolicyPlacementsRequest{Version: 1, Agent: "claude", SessionID: coworkNativeID, CurrentMappingKnown: true, EvidenceScope: syncproto.EvidenceNone, ClientMode: syncproto.ClientModeAllow}
	if _, err := f.a.acknowledgeCoworkPolicy(ctx, req); err != nil {
		t.Fatal(err)
	}
	p, ok := f.a.storedPlace(key)
	if !ok || p.how != localindex.PlacedByCoworkUnknown {
		t.Fatalf("server history not retained: %+v", p)
	}
	f.restart()
	p, ok = f.a.storedPlace(key)
	if !ok || p.how != localindex.PlacedByCoworkUnknown {
		t.Fatalf("server history lost on restart: %+v", p)
	}
}

type coworkPolicyRecorder struct {
	requests []*syncproto.PolicyPlacementsRequest
	alter    func(*syncproto.PolicyPlacementsResponse)
	fail     bool
}

func (c *coworkPolicyRecorder) PolicyPlacements(_ context.Context, req *syncproto.PolicyPlacementsRequest) (*syncproto.PolicyPlacementsResponse, error) {
	c.requests = append(c.requests, req)
	if c.fail {
		return nil, errors.New("synthetic unreachable server")
	}
	digest, err := syncproto.PolicyPlacementsDigest(req)
	if err != nil {
		return nil, err
	}
	ack := &syncproto.PolicyPlacementsResponse{Version: 1, Revision: 1, RequestDigest: digest, EvidenceScope: req.EvidenceScope}
	if c.alter != nil {
		c.alter(ack)
	}
	return ack, nil
}

func TestCoworkReconcileMetadataWithoutContent(t *testing.T) {
	for _, mode := range []string{"allow", "local", "deny"} {
		t.Run(mode, func(t *testing.T) {
			f := newCoworkFixture(t)
			host := filepath.Join(f.home, "selected")
			if mode != "allow" {
				f.cfg.UserRuleList = []string{mode + " " + host}
			}
			recorder := &coworkPolicyRecorder{}
			f.cfg.CoworkPolicy = recorder
			f.restart()
			coworkMetadata(t, f, []string{host}, nil, nil)
			f.once()
			recorder.requests = nil
			if err := f.a.reconcileCoworkPolicy(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(recorder.requests) != 1 {
				t.Fatalf("metadata calls=%d", len(recorder.requests))
			}
			req := recorder.requests[0]
			if req.SessionID != coworkNativeID || !req.CurrentMappingKnown || req.ClientMode != mode || req.EvidenceScope != syncproto.EvidenceNone {
				t.Fatalf("request=%+v", req)
			}
			foundHost := false
			for _, pl := range req.Placements {
				foundHost = foundHost || pl.CWD == host
			}
			if !foundHost {
				t.Fatalf("placements=%+v", req.Placements)
			}
			if n := f.count(`SELECT count(*) FROM sources`); n != 0 {
				t.Fatalf("content sources=%d", n)
			}
			if len(req.Sources) != 0 {
				t.Fatalf("invented source references=%+v", req.Sources)
			}
		})
	}
}

func TestCoworkReconcileDisappearedHistoricalUnknown(t *testing.T) {
	f := newCoworkFixture(t)
	recorder := &coworkPolicyRecorder{}
	f.cfg.CoworkPolicy = recorder
	f.restart()
	key := placeKey{transcript.AgentClaude, coworkNativeID}
	if err := f.a.saveCoworkPlace(context.Background(), key, placed{how: localindex.PlacedByCoworkUnknown}); err != nil {
		t.Fatal(err)
	}
	// No discoverable app metadata, no target, and no body are required to
	// reconcile a retained restriction after restart.
	f.restart()
	if err := f.a.reconcileCoworkPolicy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(recorder.requests) != 1 {
		t.Fatalf("calls=%d", len(recorder.requests))
	}
	req := recorder.requests[0]
	if req.CurrentMappingKnown || req.EvidenceScope != syncproto.EvidenceUnmapped || req.ClientMode != syncproto.ClientModeAllow {
		t.Fatalf("request=%+v", req)
	}
}

func TestCoworkReconcileRejectsInvalidAcknowledgement(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(*syncproto.PolicyPlacementsResponse)
		fail  bool
	}{
		{name: "wrong batch", alter: func(a *syncproto.PolicyPlacementsResponse) { a.RequestDigest = "wrong" }},
		{name: "no durable revision", alter: func(a *syncproto.PolicyPlacementsResponse) { a.Revision = 0 }},
		{name: "weakened unknown", alter: func(a *syncproto.PolicyPlacementsResponse) { a.EvidenceScope = syncproto.EvidenceNone }},
		{name: "false sharing grant", alter: func(a *syncproto.PolicyPlacementsResponse) { a.Allowed = true }},
		{name: "offline", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCoworkFixture(t)
			recorder := &coworkPolicyRecorder{alter: tc.alter, fail: tc.fail}
			f.cfg.CoworkPolicy = recorder
			f.restart()
			if err := f.a.saveCoworkPlace(context.Background(), placeKey{transcript.AgentClaude, coworkNativeID}, placed{how: localindex.PlacedByCoworkUnknown}); err != nil {
				t.Fatal(err)
			}
			if err := f.a.reconcileCoworkPolicy(context.Background()); err == nil {
				t.Fatal("invalid acknowledgement accepted")
			}
			if p, ok := f.a.storedPlace(placeKey{transcript.AgentClaude, coworkNativeID}); !ok || p.how != localindex.PlacedByCoworkUnknown {
				t.Fatalf("historical scope changed: %+v", p)
			}
		})
	}
}

func TestCoworkOldWithholdNeverUsesLegacyRoute(t *testing.T) {
	f := newCoworkFixture(t)
	ctx := context.Background()
	for _, id := range []string{coworkNativeID, "ordinary"} {
		how := localindex.PlacedByNone
		if id == coworkNativeID {
			how = localindex.PlacedByCoworkUnknown
		}
		if err := f.a.saveCoworkPlace(ctx, placeKey{transcript.AgentClaude, id}, placed{how: how}); err != nil {
			t.Fatal(err)
		}
		if err := f.store.SetWithhold(ctx, transcript.AgentClaude, id, "deny", "synthetic"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	calls := []string{}
	f.cfg.Withhold = func(_ context.Context, w localindex.Withhold) error { calls = append(calls, w.SessionID); return nil }
	f.restart()
	// Ignore in-memory classification entirely: durable origin must guard the
	// old route even before startup has loaded placement state.
	f.a.mu.Lock()
	f.a.places = map[placeKey]placed{}
	f.a.mu.Unlock()
	f.a.sendWithholds(ctx)
	if len(calls) != 1 || calls[0] != "ordinary" {
		t.Fatalf("legacy calls=%v", calls)
	}
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	ws, err := f.store.Withholds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 || ws[0].SessionID != coworkNativeID {
		t.Fatalf("held legacy debt=%+v", ws)
	}
}
