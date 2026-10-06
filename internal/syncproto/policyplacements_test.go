package syncproto

import "testing"

func TestPolicyPlacementsDigestBindsExactBatch(t *testing.T) {
	req := &PolicyPlacementsRequest{Version: 1, Agent: "claude", SessionID: "session", CurrentMappingKnown: true, EvidenceScope: EvidenceMapped, ClientMode: ClientModeAllow, Placements: []PolicyPlacement{{CWD: "/root"}}, Sources: []PolicySource{{Path: "/transcript", FileID: "1:1"}}}
	original, err := PolicyPlacementsDigest(req)
	if err != nil {
		t.Fatal(err)
	}
	same, err := PolicyPlacementsDigest(req)
	if err != nil || same != original || len(original) != 64 {
		t.Fatalf("unstable digest: %q %q %v", original, same, err)
	}
	mutations := []func(*PolicyPlacementsRequest){
		func(r *PolicyPlacementsRequest) {
			r.Placements = append(r.Placements, PolicyPlacement{CWD: "/private"})
		},
		func(r *PolicyPlacementsRequest) { r.CurrentMappingKnown = false },
		func(r *PolicyPlacementsRequest) { r.EvidenceScope = EvidenceUnmapped },
		func(r *PolicyPlacementsRequest) { r.ClientMode = ClientModeLocal },
		func(r *PolicyPlacementsRequest) { r.ParentSessionID = "parent" },
		func(r *PolicyPlacementsRequest) {
			r.Sources = append(r.Sources, PolicySource{Path: "/other", Generation: 1})
		},
	}
	for _, mutate := range mutations {
		changed := *req
		mutate(&changed)
		digest, err := PolicyPlacementsDigest(&changed)
		if err != nil || digest == original {
			t.Fatalf("changed batch reused ack digest: %+v %v", changed, err)
		}
	}
}
