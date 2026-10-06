package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/google/uuid"
)

func limitPlacements(n int, prefix string) []syncproto.PolicyPlacement {
	out := make([]syncproto.PolicyPlacement, n)
	for i := range out {
		out[i] = syncproto.PolicyPlacement{CWD: fmt.Sprintf("/Users/test/%s/%03d", prefix, i)}
	}
	return out
}

func limitSharedFixture(t *testing.T, n int) (*env, *syncproto.PolicyPlacementsRequest, string) {
	t.Helper()
	e := newEnv(t)
	req := policyRequest()
	req.SessionID = uuid.NewString()
	req.EvidenceScope = "none"
	req.Placements = limitPlacements(n, "allowed")
	if r := applyPolicy(t, e, req); !r.Allowed {
		t.Fatalf("initial registration: %+v", r)
	}
	sp := claudeAt(t, t.TempDir(), "-vm-work", req.SessionID, "/sessions/vm/work")
	sync1(t, e.syncer(devicesync.Config{SealAfter: -1}), sp)
	e.drain()
	req.EvidenceScope = "mapped"
	if r := applyPolicy(t, e, req); !r.Allowed || r.EvidenceScope != "mapped" {
		t.Fatalf("mapped captured fixture: %+v", r)
	}
	var source string
	if err := e.pool.QueryRow(e.ctx, `SELECT id::text FROM sources WHERE device_id=$1 AND path=$2`, e.deviceID, sp.Path).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT count(*) FROM conversations WHERE device_id=$1 AND session_id=$2 AND hidden_at IS NULL`, e.deviceID, req.SessionID) != 1 {
		t.Fatal("fixture lacks shared captured conversation")
	}
	return e, req, source
}

func limitWantStatus(t *testing.T, err error, status int) {
	t.Helper()
	var failure *Error
	if !errors.As(err, &failure) || failure.Status != status {
		t.Fatalf("wanted HTTP %d, got %v", status, err)
	}
}

func limitAssertHeld(t *testing.T, e *env, session string) {
	t.Helper()
	if e.count(`SELECT count(*) FROM session_policy_placements WHERE device_id=$1 AND session_id=$2 AND scope_status='limit-held' AND evidence_scope='mapped'`, e.deviceID, session) != 1 {
		t.Fatal("capacity failure did not durably preserve mapped scope and limit hold")
	}
	if e.count(`SELECT count(*) FROM conversations WHERE device_id=$1 AND session_id=$2 AND hidden_at IS NOT NULL AND hidden_scope='device'`, e.deviceID, session) != 1 {
		t.Fatal("capacity failure left captured shared copy visible")
	}
}

func TestPolicyLimitOverflowDurablyHoldsCapturedCopies(t *testing.T) {
	for _, kind := range []string{"union", "logical request"} {
		t.Run(kind, func(t *testing.T) {
			initial := 1
			if kind == "union" {
				initial = maxPolicyPlacements
			}
			e, req, source := limitSharedFixture(t, initial)
			child, grand, legacy := uuid.NewString(), uuid.NewString(), uuid.NewString()
			e.exec(`INSERT INTO conversations(id,agent,session_id,device_id,user_id,parent_native_session_id) VALUES($1,'claude','agent-limit-child',$2,$3,$4),($5,'claude','agent-limit-grandchild',$2,$3,'agent-limit-child')`, child, e.deviceID, e.userID, req.SessionID, grand)
			e.exec(`INSERT INTO conversations(id,agent,session_id,device_id,user_id,source_id) VALUES($1,'claude','legacy-source-only',$2,$3,$4)`, legacy, e.deviceID, e.userID, source)
			other, copy := uuid.NewString(), uuid.NewString()
			e.exec(`INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$2,'other','darwin',now())`, other, e.userID)
			e.exec(`INSERT INTO conversations(id,agent,session_id,device_id,user_id) VALUES($1,'claude',$2,$3,$4)`, copy, req.SessionID, other, e.userID)
			if kind == "union" {
				req.Placements = limitPlacements(1, "new")
			} else {
				req.Placements = limitPlacements(maxPolicyPlacements+1, "new")
			}
			response, err := (&Server{Pool: e.pool, Objects: e.objects}).PolicyPlacements(e.ctx, e.deviceID, req)
			limitWantStatus(t, err, http.StatusRequestEntityTooLarge)
			if response != nil {
				t.Fatalf("overflow returned a success acknowledgement: %+v", response)
			}
			limitAssertHeld(t, e, req.SessionID)
			for _, id := range []string{child, grand, legacy} {
				if e.count(`SELECT count(*) FROM conversations WHERE id=$1 AND hidden_at IS NOT NULL AND hidden_scope='device'`, id) != 1 {
					t.Fatalf("covered descendant/source-only copy escaped hold: %s", id)
				}
			}
			if e.count(`SELECT count(*) FROM conversations WHERE id=$1 AND hidden_at IS NULL`, copy) != 1 {
				t.Fatal("overflow hold crossed device boundary")
			}
		})
	}
}

func TestPolicyLimitCompactAcknowledgementAndStickyHold(t *testing.T) {
	e, req, _ := limitSharedFixture(t, 1)
	req.ScopeStatus = syncproto.ScopeLimitHeld
	response := applyPolicy(t, e, req)
	digest, err := syncproto.PolicyPlacementsDigest(req)
	if err != nil {
		t.Fatal(err)
	}
	if response.Allowed || response.RequestDigest != digest || response.EvidenceScope != "mapped" || response.Revision < 1 {
		t.Fatalf("compact hold acknowledgement: %+v", response)
	}
	limitAssertHeld(t, e, req.SessionID)
	req.ScopeStatus = "complete"
	response = applyPolicy(t, e, req)
	digest, err = syncproto.PolicyPlacementsDigest(req)
	if err != nil {
		t.Fatal(err)
	}
	if response.Allowed || response.RequestDigest != digest || response.EvidenceScope != "mapped" {
		t.Fatalf("complete request cleared limit hold: %+v", response)
	}
	limitAssertHeld(t, e, req.SessionID)
}

func TestPolicyLimitMalformedOverflowDoesNotMutateLedger(t *testing.T) {
	e, req, _ := limitSharedFixture(t, 1)
	var revision int64
	if err := e.pool.QueryRow(e.ctx, `SELECT revision FROM session_policy_placements WHERE device_id=$1 AND session_id=$2`, e.deviceID, req.SessionID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	req.Placements = limitPlacements(maxPolicyPlacements+1, "new")
	req.Placements[len(req.Placements)-1].CWD = "relative"
	_, err := (&Server{Pool: e.pool, Objects: e.objects}).PolicyPlacements(e.ctx, e.deviceID, req)
	limitWantStatus(t, err, http.StatusBadRequest)
	if e.count(`SELECT count(*) FROM session_policy_placements WHERE device_id=$1 AND session_id=$2 AND revision=$3 AND scope_status='complete' AND evidence_scope='mapped'`, e.deviceID, req.SessionID, revision) != 1 {
		t.Fatal("malformed oversized metadata changed ledger")
	}
	if e.count(`SELECT count(*) FROM conversations WHERE device_id=$1 AND session_id=$2 AND hidden_at IS NULL`, e.deviceID, req.SessionID) != 1 {
		t.Fatal("malformed oversized metadata changed visibility")
	}
}

func TestPolicyLimitDoesNotWeakenExistingDeny(t *testing.T) {
	e, req, _ := limitSharedFixture(t, 1)
	req.ClientMode = "deny"
	if r := applyPolicy(t, e, req); r.Allowed {
		t.Fatal("known Deny was allowed")
	}
	var oldRule string
	if err := e.pool.QueryRow(e.ctx, `SELECT hidden_rule FROM conversations WHERE device_id=$1 AND session_id=$2`, e.deviceID, req.SessionID).Scan(&oldRule); err != nil {
		t.Fatal(err)
	}
	req.Placements = limitPlacements(maxPolicyPlacements+1, "new")
	_, err := (&Server{Pool: e.pool, Objects: e.objects}).PolicyPlacements(e.ctx, e.deviceID, req)
	limitWantStatus(t, err, http.StatusRequestEntityTooLarge)
	limitAssertHeld(t, e, req.SessionID)
	rows, err := e.queue.convRows(e.ctx, `c.device_id=$1 AND c.session_id=$2`, e.deviceID, req.SessionID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("load Deny: %v", err)
	}
	if d := (serverRules{}).decideConv(rows[0]); d.Mode != pathpolicy.Deny {
		t.Fatalf("capacity hold weakened Deny: %+v", d)
	}
	if rows[0].rule != oldRule {
		t.Fatalf("capacity hold replaced restrictive hide reason: %q -> %q", oldRule, rows[0].rule)
	}
}

func TestPolicyLimitHiddenProjectionBoundsComponentAndRetainsCopies(t *testing.T) {
	e, req, _ := limitSharedFixture(t, 1)
	e.exec(`WITH added AS (
 INSERT INTO session_policy_placements(device_id,agent,session_id,placements,current_mapping_known,evidence_scope,client_mode)
 SELECT $1,'claude',gen_random_uuid()::text,'[{"cwd":"/Users/test/allowed/000"}]'::jsonb,true,'none','allow' FROM generate_series(1,1024)
 RETURNING session_id)
 INSERT INTO session_policy_links(device_id,agent,session_id,policy_session_id) SELECT $1,'claude',session_id,$2 FROM added`, e.deviceID, req.SessionID)
	// The compact floor must preserve a real stored client Deny without making
	// capacity alone permission to erase captured history at expiry.
	e.exec(`UPDATE session_policy_placements SET client_mode='deny' WHERE device_id=$1 AND session_id=(SELECT session_id FROM session_policy_placements WHERE device_id=$1 AND session_id<>$2 ORDER BY session_id DESC LIMIT 1)`, e.deviceID, req.SessionID)
	child, grand := uuid.NewString(), uuid.NewString()
	e.exec(`INSERT INTO conversations(id,agent,session_id,device_id,user_id,parent_native_session_id) VALUES($1,'claude','agent-bounded-child',$2,$3,$4),($5,'claude','agent-bounded-grandchild',$2,$3,'agent-bounded-child')`, child, e.deviceID, e.userID, req.SessionID, grand)
	rows, err := e.queue.convRows(e.ctx, `c.device_id=$1 AND c.session_id=$2`, e.deviceID, req.SessionID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("bounded projection: %v", err)
	}
	if len(rows[0].policies) > 1024 {
		t.Fatalf("unbounded state projection: %d", len(rows[0].policies))
	}
	found := false
	for _, state := range rows[0].policies {
		if state.ScopeStatus == syncproto.ScopeLimitHeld {
			found = true
			if state.EvidenceScope != "mapped" || !state.CurrentMappingKnown {
				t.Fatalf("capacity invented unknown metadata/history: %+v", state)
			}
		}
	}
	if !found {
		t.Fatal("component overflow lacks compact fail-closed hold")
	}
	if d := (serverRules{}).decideConv(rows[0]); d.Mode != pathpolicy.Deny {
		t.Fatalf("stored client floor lost: %+v", d)
	}
	if _, _, err := e.queue.applyRules(e.ctx, serverRules{}, ""); err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE conversations SET hidden_at=now()-interval '8 days' WHERE device_id=$1`, e.deviceID)
	cutoff := time.Now()
	purged, _, err := e.queue.purgeHidden(e.ctx, serverRules{}, &cutoff, "", "", "", "hidden_expired")
	if err != nil || purged != 0 {
		t.Fatalf("capacity-only floor purged: %d %v", purged, err)
	}
	if e.count(`SELECT count(*) FROM conversations WHERE device_id=$1 AND hidden_at IS NOT NULL`, e.deviceID) != 3 {
		t.Fatal("sweep failed to retain every old captured copy")
	}
}

func TestPolicyLimitHiddenProjectionBoundsPlacementBytes(t *testing.T) {
	e, req, _ := limitSharedFixture(t, 1)
	placements := make([]syncproto.PolicyPlacement, 256)
	for i := range placements {
		placements[i] = syncproto.PolicyPlacement{CWD: fmt.Sprintf("/Users/test/%s/%d", strings.Repeat("x", 4000), i)}
	}
	raw, err := json.Marshal(placements)
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`WITH added AS (
 INSERT INTO session_policy_placements(device_id,agent,session_id,placements,current_mapping_known,evidence_scope,client_mode)
 SELECT $1,'claude',gen_random_uuid()::text,$3::jsonb,true,'none','allow' FROM generate_series(1,5)
 RETURNING session_id)
 INSERT INTO session_policy_links(device_id,agent,session_id,policy_session_id) SELECT $1,'claude',session_id,$2 FROM added`, e.deviceID, req.SessionID, raw)
	rows, err := e.queue.convRows(e.ctx, `c.device_id=$1 AND c.session_id=$2`, e.deviceID, req.SessionID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("byte projection: %v", err)
	}
	total := 0
	held := false
	for _, state := range rows[0].policies {
		data, err := json.Marshal(state.Placements)
		if err != nil {
			t.Fatal(err)
		}
		total += len(data)
		held = held || state.ScopeStatus == syncproto.ScopeLimitHeld
	}
	if total > 4*1024*1024 {
		t.Fatalf("placement budget exceeded: %d", total)
	}
	if !held || (serverRules{}).decideConv(rows[0]).Mode == pathpolicy.Allow {
		t.Fatal("placement byte overflow allowed sharing")
	}
}

func TestPolicyLimitPurgeRequiresRetainedAdminProof(t *testing.T) {
	c := convRow{agent: "claude", cwd: "/sessions/vm/work", dev: deviceDirs{home: "/Users/test"}, hasPolicy: true, policies: []policyPlacementState{{CurrentMappingKnown: true, EvidenceScope: "mapped", ScopeStatus: syncproto.ScopeLimitHeld, ClientMode: "deny"}}}
	if !(serverRules{}).capacityPurgeHeld(c) {
		t.Fatal("compact client floor authorized history deletion")
	}
	rules, err := pathpolicy.ParseRules([]string{"deny /Users/test/private"})
	if err != nil {
		t.Fatal(err)
	}
	c.policies = append(c.policies, policyPlacementState{CurrentMappingKnown: true, EvidenceScope: "mapped", ClientMode: "allow", Placements: []syncproto.PolicyPlacement{{CWD: "/Users/test/private/project"}}})
	if (serverRules{admin: rules}).capacityPurgeHeld(c) {
		t.Fatal("retained actual admin path proof lost purge eligibility")
	}
	c.policies = c.policies[:1]
	c.policies[0].EvidenceScope = "unmapped"
	if (serverRules{unplaceable: pathpolicy.Deny}).capacityPurgeHeld(c) {
		t.Fatal("actual historical unknown lost configured exclude precedence")
	}
}
