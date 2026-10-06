package ingest

import (
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/syncproto"
	"testing"
)

func policyRequest() *syncproto.PolicyPlacementsRequest {
	return &syncproto.PolicyPlacementsRequest{Version: syncproto.Version, Agent: "claude", SessionID: "0b7e2c1a-0000-4000-8000-00000000f001", CurrentMappingKnown: true, EvidenceScope: "mapped", ClientMode: "allow", Placements: []syncproto.PolicyPlacement{{CWD: "/Users/test/work"}}}
}

func TestPolicyHistoryBoundary(t *testing.T) {
	req := policyRequest()
	req.CurrentMappingKnown = false
	req.Placements = nil
	req.EvidenceScope = "none"
	p, err := mergePolicy(policyPlacementState{}, req, false)
	if err != nil {
		t.Fatal(err)
	}
	if p.EvidenceScope != "none" {
		t.Fatalf("metadata-only unknown tainted history: %+v", p)
	}
	req = policyRequest()
	p, err = mergePolicy(p, req, false)
	if err != nil {
		t.Fatal(err)
	}
	if p.EvidenceScope != "mapped" || !p.CurrentMappingKnown {
		t.Fatalf("first known capture held: %+v", p)
	}
	req.CurrentMappingKnown = false
	req.EvidenceScope = "mapped"
	p, err = mergePolicy(p, req, false)
	if err != nil {
		t.Fatal(err)
	}
	if p.EvidenceScope != "mapped" {
		t.Fatalf("readiness failure tainted unchanged history: %+v", p)
	}
	req.EvidenceScope = "unmapped"
	p, err = mergePolicy(p, req, false)
	if err != nil {
		t.Fatal(err)
	}
	req.CurrentMappingKnown = true
	req.EvidenceScope = "mapped"
	req.Placements = []syncproto.PolicyPlacement{{CWD: "/Users/test/other"}}
	p, err = mergePolicy(p, req, false)
	if err != nil {
		t.Fatal(err)
	}
	if p.EvidenceScope != "unmapped" || len(p.Placements) != 2 {
		t.Fatalf("historical restriction erased: %+v", p)
	}
	legacy, err := mergePolicy(policyPlacementState{}, policyRequest(), true)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.EvidenceScope != "unmapped" {
		t.Fatalf("current metadata certified preexisting content: %+v", legacy)
	}
}

func TestPolicyUnknownCannotOverrideKnownDeny(t *testing.T) {
	rules, _ := pathpolicy.ParseRules([]string{"deny ~/private"})
	r := serverRules{admin: rules}
	dev := deviceDirs{home: "/Users/test"}
	p := policyPlacementState{Placements: []syncproto.PolicyPlacement{{CWD: "/Users/test/private/project"}}, EvidenceScope: "unmapped", ClientMode: "allow"}
	if d := r.decidePolicy(dev, p); d.Mode != pathpolicy.Deny {
		t.Fatalf("known deny lost to unknown: %+v", d)
	}
	if policyHoldOnly(r, dev, p) {
		t.Fatal("known deny classified as mapping hold")
	}
	p.Placements = []syncproto.PolicyPlacement{{CWD: "/Users/test/open"}}
	if d := r.decidePolicy(dev, p); d.Mode != pathpolicy.Local {
		t.Fatalf("unknown scope permitted sharing: %+v", d)
	}
	if !policyHoldOnly(r, dev, p) {
		t.Fatal("unmapped scope classified as purge restriction")
	}
}

func TestPolicyRequestBounds(t *testing.T) {
	for _, change := range []func(*syncproto.PolicyPlacementsRequest){
		func(r *syncproto.PolicyPlacementsRequest) { r.Agent = "codex" },
		func(r *syncproto.PolicyPlacementsRequest) { r.SessionID = "../other" },
		func(r *syncproto.PolicyPlacementsRequest) { r.Placements[0].CWD = "/sessions/vm/project" },
		func(r *syncproto.PolicyPlacementsRequest) { r.Placements[0].CWD = "relative" },
		func(r *syncproto.PolicyPlacementsRequest) { r.Placements = nil },
		func(r *syncproto.PolicyPlacementsRequest) {
			r.Sources = []syncproto.PolicySource{{Path: "/native", Generation: -1}}
		},
	} {
		r := policyRequest()
		change(r)
		if err := validatePolicyRequest(r); err == nil {
			t.Fatalf("invalid policy accepted: %+v", r)
		}
	}
	if err := validatePolicyRequest(policyRequest()); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyVerifiedHostOverridesOnlyNativeMissingPlacementFloor(t *testing.T) {
	p := policyPlacementState{Placements: []syncproto.PolicyPlacement{{CWD: "/Users/test/work"}}, CurrentMappingKnown: true, EvidenceScope: "mapped", ClientMode: "allow"}
	for _, floor := range []pathpolicy.Mode{pathpolicy.Local, pathpolicy.Deny} {
		r := serverRules{unplaceable: floor}
		dev := deviceDirs{}
		native := r.decideAll(dev, "claude", "/nonstandard/session.jsonl", "", nil, "")
		if !native.Unplaceable {
			t.Fatal("fixture is unexpectedly placed")
		}
		if d := decideWithPolicies(r, dev, native, []policyPlacementState{p}); d.Mode != pathpolicy.Allow {
			t.Fatalf("host placement retained native floor %s: %+v", floor, d)
		}
	}
	rules, _ := pathpolicy.ParseRules([]string{"deny /sessions/private"})
	r := serverRules{admin: rules}
	native := r.decideAll(deviceDirs{}, "claude", "/native", "/sessions/private", nil, "")
	if d := decideWithPolicies(r, deviceDirs{}, native, []policyPlacementState{p}); d.Mode != pathpolicy.Deny {
		t.Fatalf("actual native deny erased: %+v", d)
	}
}

func TestPolicyMissingHomeHoldsWithoutErasingKnownDeny(t *testing.T) {
	rules, _ := pathpolicy.ParseRules([]string{"deny ~/private", "deny /absolute/private"})
	r := serverRules{admin: rules}
	p := policyPlacementState{CurrentMappingKnown: true, EvidenceScope: "mapped", ClientMode: "allow", Placements: []syncproto.PolicyPlacement{{CWD: "/somewhere"}}}
	d := r.decidePolicy(deviceDirs{}, p)
	if !isPolicyHold(d) || d.Rule.Pattern != "cowork-home-unknown" {
		t.Fatalf("missing home allowed: %+v", d)
	}
	p.Placements = append(p.Placements, syncproto.PolicyPlacement{CWD: "/absolute/private"})
	if d = r.decidePolicy(deviceDirs{}, p); d.Mode != pathpolicy.Deny {
		t.Fatalf("known deny weakened: %+v", d)
	}
	for _, home := range []string{"relative", "/sessions/vm/home", "/Users/bad\npath"} {
		req := policyRequest()
		req.Device = &syncproto.DeviceDirs{Home: home}
		if validatePolicyRequest(req) == nil {
			t.Fatalf("invalid home accepted: %q", home)
		}
	}
}
