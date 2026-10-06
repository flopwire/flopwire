package syncproto

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestPolicyDigestWithoutDeviceRetainsOriginalWireEncoding(t *testing.T) {
	req := &PolicyPlacementsRequest{Version: 1, Agent: "claude", SessionID: "session", CurrentMappingKnown: true, EvidenceScope: EvidenceMapped, ClientMode: ClientModeAllow, Placements: []PolicyPlacement{{CWD: "/root"}}}
	old := []byte(`{"version":1,"agent":"claude","session_id":"session","current_mapping_known":true,"evidence_scope":"mapped","placements":[{"cwd":"/root","worktree_root":"","main_root":"","remote":""}],"client_mode":"allow"}`)
	sum := sha256.Sum256(old)
	digest, err := PolicyPlacementsDigest(req)
	if err != nil || digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("optional device changed an existing request digest: %s %v", digest, err)
	}
}

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
			r.RecoverySources = []PolicyRecoverySource{{Source: PolicySource{Path: "/recovery/export.jsonl", FileID: "1:2", Generation: 3}, OriginalPath: "/gone/native.jsonl"}}
		},
		func(r *PolicyPlacementsRequest) { r.Device = &DeviceDirs{Home: "/Users/test"} },
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

func TestPolicyRecoveryDigestBindsEveryProofField(t *testing.T) {
	req := &PolicyPlacementsRequest{Version: 1, Agent: "claude", SessionID: "session", RecoverySources: []PolicyRecoverySource{{Source: PolicySource{Path: "/recovery/export.jsonl", FileID: "1:2", Generation: 3}, OriginalPath: "/gone/native.jsonl"}}}
	original, err := PolicyPlacementsDigest(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*PolicyRecoverySource){
		func(r *PolicyRecoverySource) { r.Source.Path = "/other/export.jsonl" },
		func(r *PolicyRecoverySource) { r.Source.FileID = "1:3" },
		func(r *PolicyRecoverySource) { r.Source.Generation = 4 },
		func(r *PolicyRecoverySource) { r.OriginalPath = "/other/native.jsonl" },
	} {
		changed := *req
		changed.RecoverySources = append([]PolicyRecoverySource(nil), req.RecoverySources...)
		change(&changed.RecoverySources[0])
		digest, err := PolicyPlacementsDigest(&changed)
		if err != nil || digest == original {
			t.Fatalf("recovery provenance reused acknowledgement: %v", err)
		}
	}
}
