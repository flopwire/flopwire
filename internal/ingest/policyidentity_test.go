package ingest

import (
	"errors"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/transcript"
	"net/http"
	"os"
	"path/filepath"
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

func TestPolicyIdentifiedCoworkCannotChangeAgentToBypassGate(t *testing.T) {
	e := newEnv(t)
	for _, agent := range []string{"claude", "codex"} {
		src := syncproto.Source{Agent: agent, Path: "/Users/test/Claude/local-agent-mode-sessions/session/transcript.jsonl", FileID: "1:1", StorageKind: "jsonl_append", SessionKey: uuid.NewString()}
		_, err := checkFlushPolicy(e.ctx, e.pool, e.deviceID, src)
		var held *Error
		if !errors.As(err, &held) || held.Code != "policy_placements_required" {
			t.Fatalf("identified Cowork agent=%s bypassed gate: %v", agent, err)
		}
	}
}

func TestPolicyProtectedCompanionFirstMainCapture(t *testing.T) {
	e := newEnv(t)
	req := policyRequest()
	req.EvidenceScope = "none"
	main := claudeAt(t, t.TempDir(), "-vm-work", req.SessionID, "/sessions/vm/work")
	applyPolicy(t, e, req)
	artifact := filepath.Join(filepath.Dir(main.Path), "tool-result.txt")
	if err := os.WriteFile(artifact, []byte("synthetic companion output"), 0600); err != nil {
		t.Fatal(err)
	}
	companion := devicesync.SourceSpec{Path: artifact, Agent: transcript.AgentClaude, StorageKind: transcript.StorageCompanion, SessionKey: req.SessionID, Parser: "claude@1", Parent: main.Path}
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	req.EvidenceScope = "mapped"
	req.Sources = []syncproto.PolicySource{{Path: artifact, FileID: fileIDOf(t, artifact)}}
	applyPolicy(t, e, req)
	sync1(t, sy, companion)
	if e.count(`SELECT count(*) FROM sources WHERE device_id=$1 AND path=$2`, e.deviceID, main.Path) != 0 {
		t.Fatal("companion invented a captured parent source")
	}
	req.Sources = []syncproto.PolicySource{{Path: artifact, FileID: fileIDOf(t, artifact)}, {Path: main.Path, FileID: fileIDOf(t, main.Path)}}
	if r := applyPolicy(t, e, req); !r.Allowed || r.EvidenceScope != "mapped" {
		t.Fatalf("known companion capture was held: %+v", r)
	}
	if e.count(`SELECT count(*) FROM source_policy_capture_identity WHERE device_id=$1 AND path=$2`, e.deviceID, main.Path) != 0 {
		t.Fatal("parent metadata invented capture attributes")
	}
	sync1(t, sy, main)
	e.drain()
	if e.count(`SELECT count(*) FROM sources s JOIN generations g ON g.source_id=s.id WHERE s.device_id=$1 AND s.path=$2 AND s.storage_kind='jsonl_append' AND g.size>0`, e.deviceID, main.Path) != 1 {
		t.Fatal("first main capture failed")
	}
	if e.count(`SELECT count(*) FROM conversations WHERE device_id=$1 AND session_id=$2 AND hidden_at IS NULL`, e.deviceID, req.SessionID) != 1 {
		t.Fatal("known main capture is not shared")
	}
	sibling := policyRequest()
	sibling.SessionID = uuid.NewString()
	sibling.ParentSessionID = req.SessionID
	sibling.Sources = req.Sources
	_, err := (&Server{Pool: e.pool, Objects: e.objects}).PolicyPlacements(e.ctx, e.deviceID, sibling)
	identityWantConflict(t, err)
}

func TestPolicyCapturedOrForeignAgentSourceIdentity(t *testing.T) {
	for _, kind := range []string{"captured generation", "foreign agent", "partial descriptor"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t)
			native := uuid.NewString()
			ref := syncproto.PolicySource{Path: "/placeholder/" + native + ".jsonl", FileID: "parent"}
			id := uuid.NewString()
			agent, parser, storage := "claude", "", "jsonl_append"
			if kind == "foreign agent" {
				agent = "codex"
			}
			if kind == "partial descriptor" {
				parser = "claude@1"
				storage = "companion"
			}
			e.exec(`INSERT INTO sources(id,device_id,agent,path,file_id,storage_kind,parser,first_seen_at)VALUES($1,$2,$3,$4,$5,$6,$7,now())`, id, e.deviceID, agent, ref.Path, ref.FileID, storage, parser)
			if kind == "captured generation" {
				e.exec(`INSERT INTO generations(source_id,generation,size,captured_at,complete)VALUES($1,0,0,now(),true)`, id)
			}
			err := identityTransaction(e, func(tx pgx.Tx) error {
				return bindPolicySourceOwner(e.ctx, tx, e.deviceID, "claude", native, ref, false)
			})
			if kind == "captured generation" {
				if err != nil {
					t.Fatal(err)
				}
				if e.count(`SELECT count(*) FROM source_policy_capture_identity WHERE device_id=$1 AND path=$2 AND storage_kind='jsonl_append'`, e.deviceID, ref.Path) != 1 {
					t.Fatal("zero-byte capture was not frozen")
				}
				identityWantConflict(t, identityTransaction(e, func(tx pgx.Tx) error {
					return bindPolicySourceOwner(e.ctx, tx, e.deviceID, "claude", uuid.NewString(), ref, false)
				}))
			} else {
				identityWantConflict(t, err)
				if e.count(`SELECT count(*) FROM source_policy_identity WHERE device_id=$1 AND path=$2`, e.deviceID, ref.Path) != 0 {
					t.Fatal("conflicting source acquired owner")
				}
			}
		})
	}
}

func TestPolicyProtectedCompanionMissingParentProof(t *testing.T) {
	for _, kind := range []string{"valid", "wrong path", "wrong session", "wrong agent", "empty parent file", "different known parent file", "foreign parent descriptor", "partial parent descriptor"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t)
			owner := uuid.NewString()
			p := policySourceIdentity{Agent: "claude", Owner: owner}
			src := syncproto.Source{Agent: "claude", Path: "/companion/result.txt", FileID: "companion", SessionKey: owner, StorageKind: "companion", Parser: "claude@1", Parent: &syncproto.SourceRef{Path: "/native/" + owner + ".jsonl", FileID: "parent"}}
			switch kind {
			case "wrong path":
				src.Parent.Path = "/native/" + uuid.NewString() + ".jsonl"
			case "wrong session":
				src.SessionKey = uuid.NewString()
			case "wrong agent":
				src.Agent = "codex"
			case "empty parent file":
				src.Parent.FileID = ""
			case "different known parent file", "foreign parent descriptor", "partial parent descriptor":
				agent, file, parser, storage := "claude", "parent", "", "jsonl_append"
				if kind == "different known parent file" {
					file = "different"
				}
				if kind == "foreign parent descriptor" {
					agent = "codex"
				}
				if kind == "partial parent descriptor" {
					parser = "claude@1"
					storage = "companion"
				}
				e.exec(`INSERT INTO sources(id,device_id,agent,path,file_id,storage_kind,parser,first_seen_at) VALUES($1,$2,$3,$4,$5,$6,$7,now())`, uuid.NewString(), e.deviceID, agent, src.Parent.Path, file, storage, parser)
			}
			err := identityTransaction(e, func(tx pgx.Tx) error {
				if _, err := tx.Exec(e.ctx, `INSERT INTO source_policy_identity(device_id,path,file_id,owner_agent,owner_session_id) VALUES($1,$2,$3,$4,$5)`, e.deviceID, src.Path, src.FileID, p.Agent, p.Owner); err != nil {
					return err
				}
				return bindPolicyCaptureIdentity(e.ctx, tx, e.deviceID, src)
			})
			if kind == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				if e.count(`SELECT count(*) FROM source_policy_identity WHERE device_id=$1 AND path=$2 AND owner_session_id=$3`, e.deviceID, src.Parent.Path, owner) != 1 {
					t.Fatal("missing immutable parent owner")
				}
				if e.count(`SELECT count(*) FROM source_policy_capture_identity WHERE device_id=$1 AND path=$2`, e.deviceID, src.Parent.Path) != 0 {
					t.Fatal("missing parent invented capture attributes")
				}
			} else {
				identityWantConflict(t, err)
			}
		})
	}
}
