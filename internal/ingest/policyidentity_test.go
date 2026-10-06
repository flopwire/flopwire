package ingest

import (
	"errors"
	"net/http"
	"testing"

	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func identityTransaction(e *env, fn func(pgx.Tx) error) error {
	return pgx.BeginTxFunc(e.ctx, e.pool, pgx.TxOptions{}, fn)
}
func identityWantConflict(t *testing.T, err error) {
	t.Helper()
	var conflict *Error
	if !errors.As(err, &conflict) || conflict.Status != http.StatusConflict {
		t.Fatalf("wanted identity conflict, got %v", err)
	}
}

func TestPolicySourceIdentityPendingAndImmutableCapture(t *testing.T) {
	e := newEnv(t)
	native, sibling := uuid.NewString(), uuid.NewString()
	policyStoredLedger(e, e.deviceID, native, true, "mapped", "allow")
	policyStoredLedger(e, e.deviceID, sibling, true, "mapped", "allow")
	e.exec(`INSERT INTO session_policy_links(device_id,agent,session_id,policy_session_id) VALUES($1,'claude',$2,$3),($1,'claude',$3,$2)`, e.deviceID, native, sibling)
	ref := syncproto.PolicySource{Path: "/protected/export.jsonl", FileID: "", Generation: 0}
	if err := identityTransaction(e, func(tx pgx.Tx) error {
		return bindPolicySourceOwner(e.ctx, tx, e.deviceID, "claude", native, ref, false)
	}); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT count(*) FROM source_policy_capture_identity WHERE device_id=$1`, e.deviceID) != 0 {
		t.Fatal("metadata fabricated captured identity")
	}
	src := syncproto.Source{Path: ref.Path, FileID: ref.FileID, Agent: "claude", SessionKey: native, StorageKind: "jsonl_append", Parser: "claude@4.1"}
	if err := identityTransaction(e, func(tx pgx.Tx) error { return bindPolicyCaptureIdentity(e.ctx, tx, e.deviceID, src) }); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*syncproto.Source){
		"same component sibling": func(s *syncproto.Source) { s.SessionKey = sibling },
		"agent":                  func(s *syncproto.Source) { s.Agent = "codex" },
		"storage":                func(s *syncproto.Source) { s.StorageKind = "cass_export" },
		"main becomes companion": func(s *syncproto.Source) { s.Parent = &syncproto.SourceRef{Path: "/parent", FileID: "f"} },
		"parser family":          func(s *syncproto.Source) { s.Parser = "codex@4.1" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := src
			mutate(&changed)
			identityWantConflict(t, checkPolicySourceIdentity(e.ctx, e.pool, e.deviceID, changed))
		})
	}
	upgraded := src
	upgraded.Parser = "claude@5.0"
	upgraded.Checkout = "/repo/new"
	upgraded.Remote = "github.com/owner/new"
	if err := identityTransaction(e, func(tx pgx.Tx) error { return bindPolicyCaptureIdentity(e.ctx, tx, e.deviceID, upgraded) }); err != nil {
		t.Fatalf("version/repo upgrade: %v", err)
	}
	ref.Generation = 1
	identityWantConflict(t, identityTransaction(e, func(tx pgx.Tx) error {
		return bindPolicySourceOwner(e.ctx, tx, e.deviceID, "claude", sibling, ref, false)
	}))
}

func TestPolicySourceIdentityCompanionParentImmutable(t *testing.T) {
	e := newEnv(t)
	native := uuid.NewString()
	parent1, parent2 := syncproto.SourceRef{Path: "/parent1", FileID: "one"}, syncproto.SourceRef{Path: "/parent2", FileID: "two"}
	for _, parent := range []syncproto.SourceRef{parent1, parent2} {
		ref := syncproto.PolicySource{Path: parent.Path, FileID: parent.FileID}
		if err := identityTransaction(e, func(tx pgx.Tx) error {
			return bindPolicySourceOwner(e.ctx, tx, e.deviceID, "claude", native, ref, false)
		}); err != nil {
			t.Fatal(err)
		}
	}
	ref := syncproto.PolicySource{Path: "/tool/result", FileID: "child"}
	if err := identityTransaction(e, func(tx pgx.Tx) error {
		return bindPolicySourceOwner(e.ctx, tx, e.deviceID, "claude", native, ref, false)
	}); err != nil {
		t.Fatal(err)
	}
	src := syncproto.Source{Path: ref.Path, FileID: ref.FileID, Agent: "claude", StorageKind: "jsonl_append", Parent: &parent1}
	if err := identityTransaction(e, func(tx pgx.Tx) error { return bindPolicyCaptureIdentity(e.ctx, tx, e.deviceID, src) }); err != nil {
		t.Fatal(err)
	}
	changed := src
	changed.Parent = &parent2
	identityWantConflict(t, checkPolicySourceIdentity(e.ctx, e.pool, e.deviceID, changed))
	changed = src
	changed.Parent = nil
	changed.SessionKey = native
	identityWantConflict(t, checkPolicySourceIdentity(e.ctx, e.pool, e.deviceID, changed))
}

func TestPolicySourceIdentityVerifiedRecoveryAliasFrozen(t *testing.T) {
	e := newEnv(t)
	native, alias := uuid.NewString(), "cass-recovery-physical"
	ref := syncproto.PolicySource{Path: "/recovered/export.jsonl", FileID: ""}
	e.exec(`INSERT INTO sources(id,device_id,agent,path,file_id,storage_kind,session_key,parser,first_seen_at) VALUES($1,$2,'claude',$3,'','cass_export',$4,'cass@1',now())`, uuid.NewString(), e.deviceID, ref.Path, alias)
	identityWantConflict(t, identityTransaction(e, func(tx pgx.Tx) error {
		return bindPolicySourceOwner(e.ctx, tx, e.deviceID, "claude", native, ref, false)
	}))
	if err := identityTransaction(e, func(tx pgx.Tx) error {
		return bindPolicySourceOwner(e.ctx, tx, e.deviceID, "claude", native, ref, true)
	}); err != nil {
		t.Fatal(err)
	}
	src := syncproto.Source{Path: ref.Path, FileID: "", Agent: "claude", StorageKind: "cass_export", SessionKey: alias, Parser: "cass@2"}
	if err := checkPolicySourceIdentity(e.ctx, e.pool, e.deviceID, src); err != nil {
		t.Fatal(err)
	}
	changed := src
	changed.SessionKey = native
	identityWantConflict(t, checkPolicySourceIdentity(e.ctx, e.pool, e.deviceID, changed))
	identityWantConflict(t, identityTransaction(e, func(tx pgx.Tx) error {
		return bindPolicySourceOwner(e.ctx, tx, e.deviceID, "claude", uuid.NewString(), ref, true)
	}))
}

func TestPolicySourceIdentityCanonicalNativeAndUnlistedCompanion(t *testing.T) {
	e := newEnv(t)
	native := uuid.NewString()
	parent := syncproto.SourceRef{Path: "/project/" + native + ".jsonl", FileID: "f"}
	if err := identityTransaction(e, func(tx pgx.Tx) error {
		return bindPolicySourceOwner(e.ctx, tx, e.deviceID, "claude", native, syncproto.PolicySource{Path: parent.Path, FileID: parent.FileID}, false)
	}); err != nil {
		t.Fatal(err)
	}
	src := syncproto.Source{Path: parent.Path, FileID: parent.FileID, Agent: "claude", StorageKind: "jsonl_append", Parser: "claude@4.1"}
	if err := identityTransaction(e, func(tx pgx.Tx) error { return bindPolicyCaptureIdentity(e.ctx, tx, e.deviceID, src) }); err != nil {
		t.Fatal(err)
	}
	src.SessionKey = native
	if err := checkPolicySourceIdentity(e.ctx, e.pool, e.deviceID, src); err != nil {
		t.Fatalf("empty to canonical session rejected: %v", err)
	}
	companion := syncproto.Source{Path: "/tool/result", FileID: "c", Agent: "claude", StorageKind: "jsonl_append", Parent: &parent}
	if err := checkPolicySourceIdentity(e.ctx, e.pool, e.deviceID, companion); err != nil {
		t.Fatal(err)
	}
	if err := identityTransaction(e, func(tx pgx.Tx) error { return bindPolicyCaptureIdentity(e.ctx, tx, e.deviceID, companion) }); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT count(*) FROM source_policy_identity WHERE device_id=$1 AND path=$2 AND owner_session_id=$3`, e.deviceID, companion.Path, native) != 1 {
		t.Fatal("unlisted companion owner not frozen")
	}
}

func TestPolicySourceIdentityUnresolvedParentPreserved(t *testing.T) {
	e := newEnv(t)
	native := uuid.NewString()
	parent := syncproto.SourceRef{Path: "/parent/" + native + ".jsonl", FileID: "parent"}
	if err := identityTransaction(e, func(tx pgx.Tx) error {
		return bindPolicySourceOwner(e.ctx, tx, e.deviceID, "claude", native, syncproto.PolicySource{Path: parent.Path, FileID: parent.FileID}, false)
	}); err != nil {
		t.Fatal(err)
	}
	ref := syncproto.PolicySource{Path: "/tool/unresolved", FileID: "child"}
	e.exec(`INSERT INTO sources(id,device_id,agent,path,file_id,storage_kind,parser,first_seen_at,parent_path,parent_file_id) VALUES($1,$2,'claude',$3,$4,'jsonl_append','',now(),$5,$6)`, uuid.NewString(), e.deviceID, ref.Path, ref.FileID, parent.Path, parent.FileID)
	if err := identityTransaction(e, func(tx pgx.Tx) error {
		return bindPolicySourceOwner(e.ctx, tx, e.deviceID, "claude", native, ref, false)
	}); err != nil {
		t.Fatal(err)
	}
	src := syncproto.Source{Path: ref.Path, FileID: ref.FileID, Agent: "claude", StorageKind: "jsonl_append", Parent: &parent}
	if err := checkPolicySourceIdentity(e.ctx, e.pool, e.deviceID, src); err != nil {
		t.Fatal(err)
	}
	src.Parent = nil
	src.SessionKey = native
	identityWantConflict(t, checkPolicySourceIdentity(e.ctx, e.pool, e.deviceID, src))
}

// This seeds the historical raw-manifest boundary directly: the parent has
// only an empty generation, and the companion has stored bytes but no derived
// conversations/messages. Today's host folders cannot certify that capture.
func TestPolicyHistoricalRawOnlyCompanionRequiresCaptureProof(t *testing.T) {
	for _, floor := range []string{"upload", "exclude"} {
		for _, linked := range []bool{false, true} {
			name := floor + "/retained-parent"
			if linked {
				name = floor + "/linked-parent"
			}
			t.Run(name, func(t *testing.T) {
				e := newEnv(t)
				e.setRules(floor)
				req := policyRequest()
				rootID, childID := uuid.NewString(), uuid.NewString()
				root := syncproto.PolicySource{Path: "/legacy/" + req.SessionID + ".jsonl", FileID: "parent"}
				child := syncproto.PolicySource{Path: "/legacy/tool-results/arbitrary-output.txt", FileID: "child"}
				e.exec(`INSERT INTO sources(id,device_id,agent,path,file_id,session_key,storage_kind,parser,first_seen_at) VALUES($1,$2,'claude',$3,$4,$5,'jsonl_append','claude@1',now())`, rootID, e.deviceID, root.Path, root.FileID, req.SessionID)
				var parentID any
				if linked {
					parentID = rootID
				}
				e.exec(`INSERT INTO sources(id,device_id,agent,path,file_id,session_key,storage_kind,parser,first_seen_at,parent_source_id,parent_path,parent_file_id) VALUES($1,$2,'claude',$3,$4,NULL,'companion','',now(),$5,$6,$7)`, childID, e.deviceID, child.Path, child.FileID, parentID, root.Path, root.FileID)
				data := []byte("synthetic companion captured before host mapping proof\n")
				hash := syncproto.Sum(data)
				compressed := syncproto.Compress(nil, data)
				if err := e.objects.Put(e.ctx, ChunkKey(hash), compressed); err != nil {
					t.Fatal(err)
				}
				e.exec(`INSERT INTO generations(source_id,generation,size,captured_at,complete) VALUES($1,0,0,now(),true),($2,0,$3,now(),true)`, rootID, childID, len(data))
				e.exec(`INSERT INTO chunks(hash,size,stored_size,object_key) VALUES($1,$2,$3,$4)`, hash[:], len(data), len(compressed), ChunkKey(hash))
				e.exec(`INSERT INTO manifest_entries(source_id,generation,ordinal,chunk_hash,byte_offset) VALUES($1,0,0,$2,0)`, childID, hash[:])
				if e.count(`SELECT count(*) FROM messages`) != 0 || e.count(`SELECT count(*) FROM conversations`) != 0 {
					t.Fatal("raw companion fixture unexpectedly has derived evidence")
				}
				// Register only the root first to exercise recursive source ownership.
				// Then explicitly bind the companion; neither request can certify it.
				req.Sources = []syncproto.PolicySource{root}
				for _, includeCompanion := range []bool{false, true} {
					if includeCompanion {
						req.Sources = append(req.Sources, child)
					}
					ack := applyPolicy(t, e, req)
					if ack.Allowed || ack.EvidenceScope != syncproto.EvidenceUnmapped {
						t.Fatalf("current mapping certified old raw companion: %+v", ack)
					}
					states, err := loadSessionPolicies(e.ctx, e.pool, e.deviceID, "claude", req.SessionID, "", rootID)
					if err != nil {
						t.Fatal(err)
					}
					rules, err := loadRules(e.ctx, e.pool)
					if err != nil {
						t.Fatal(err)
					}
					want := pathpolicy.Local
					if floor == "exclude" {
						want = pathpolicy.Deny
					}
					if d := decideWithPolicies(rules, deviceDirs{}, pathpolicy.Decision{}, states); d.Mode != want {
						t.Fatalf("historical companion floor: got %+v, want %s", d, want)
					}
				}
				if e.count(`SELECT count(*) FROM session_policy_placements WHERE session_id=$1 AND evidence_scope='mapped'`, req.SessionID) != 0 {
					t.Fatal("historical bytes received mapped capture proof")
				}
			})
		}
	}
}
