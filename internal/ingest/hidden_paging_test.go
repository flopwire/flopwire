package ingest

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/google/uuid"
)

func hiddenPagingFixture(t *testing.T) (*env, string, string) {
	t.Helper()
	e := newEnv(t)
	session := uuid.NewString()
	root := policyStoredConversation(e, e.deviceID, session, "")
	placements := make([]syncproto.PolicyPlacement, 200)
	for i := range placements {
		placements[i] = syncproto.PolicyPlacement{CWD: fmt.Sprintf("/Users/test/%s/%d", strings.Repeat("x", 4000), i)}
	}
	raw, err := json.Marshal(placements)
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`INSERT INTO session_policy_placements(device_id,agent,session_id,placements,current_mapping_known,evidence_scope,client_mode) VALUES($1,'claude',$2,$3,true,'mapped','allow')`, e.deviceID, session, raw)
	e.exec(`INSERT INTO conversations(id,agent,session_id,device_id,user_id,parent_native_session_id,cwd,depth)
 SELECT gen_random_uuid(),'claude','agent-page-'||i,$1,$2,$3,'/sessions/vm/work',1 FROM generate_series(1,33) i`, e.deviceID, e.userID, session)
	return e, session, root
}

// A page's byte limit can stop before its row limit. Resume after the last
// returned key, not the extra scanned key, and close statements before writes.
func TestHiddenPolicyPagesBoundBytesWithoutSkippingScannedRow(t *testing.T) {
	e, _, _ := hiddenPagingFixture(t)
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(e.ctx)
	var last hiddenCursor
	seen := map[string]bool{}
	pages := 0
	byteLimited := false
	for {
		page, done, err := hiddenPolicyPage(e.ctx, tx, `c.device_id=$1 AND ($2='' OR (c.depth,c.id)>($3::int,NULLIF($2,'')::uuid)) ORDER BY c.depth,c.id`, e.deviceID, last.id, last.depth)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		pages++
		if len(page) > hiddenPageSize {
			t.Fatalf("row page exceeded bound: %d", len(page))
		}
		bytes := 0
		for _, c := range page {
			if seen[c.id] {
				t.Fatalf("conversation repeated: %s", c.id)
			}
			seen[c.id] = true
			raw, err := json.Marshal(c.policies)
			if err != nil {
				t.Fatal(err)
			}
			bytes += len(raw)
			last = hiddenCursor{id: c.id, depth: c.depth, at: c.hiddenAt}
		}
		if len(page) > 1 && bytes > hiddenPagePolicyBytes {
			t.Fatalf("page retained too much policy data: %d", bytes)
		}
		if !done && len(page) < hiddenPageSize {
			byteLimited = true
		}
		// This fails with a busy connection if page rows remain open.
		if _, err := tx.Exec(e.ctx, `UPDATE conversations SET title=COALESCE(title,'') WHERE id=$1`, last.id); err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
	}
	if len(seen) != 34 || pages < 2 || !byteLimited {
		t.Fatalf("paging coverage: seen=%d pages=%d byteLimited=%v", len(seen), pages, byteLimited)
	}
}

// Restoration re-roots protected members in the same transaction while
// clearing other members across every page. Its audit counts all pages.
func TestHiddenPagedRestoreAndPurgeCoverAllMembers(t *testing.T) {
	e, session, root := hiddenPagingFixture(t)
	e.exec(`INSERT INTO session_policy_placements(device_id,agent,session_id,placements,current_mapping_known,evidence_scope,client_mode)
 SELECT $1,'claude','agent-page-'||i,'[{"cwd":"/Users/test/local"}]'::jsonb,true,'mapped','local' FROM generate_series(1,17) i`, e.deviceID)
	rules, invalid := pathpolicy.ParseRules([]string{"deny /sessions"})
	if invalid != nil {
		t.Fatal(invalid)
	}
	old := serverRules{admin: rules, updatedBy: e.userID, version: 1}
	if _, _, err := e.queue.applyRules(e.ctx, old, ""); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT count(*) FROM conversations WHERE device_id=$1 AND hidden_at IS NOT NULL`, e.deviceID) != 34 {
		t.Fatal("paged apply missed conversations")
	}
	if _, _, err := e.queue.applyRules(e.ctx, serverRules{updatedBy: e.userID}, ""); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT count(*) FROM conversations WHERE device_id=$1 AND hidden_at IS NULL`, e.deviceID) != 17 {
		t.Fatal("paged restore skipped allowed members")
	}
	if e.count(`SELECT count(*) FROM conversations WHERE device_id=$1 AND hidden_at IS NOT NULL AND hidden_root=id AND hidden_scope='device'`, e.deviceID) != 17 {
		t.Fatal("paged restore exposed or orphaned protected members")
	}
	var audited int
	if err := e.pool.QueryRow(e.ctx, `SELECT (metadata->>'conversations')::int FROM audit_events WHERE action='conversation.restored' AND target_id=$1 ORDER BY created_at DESC LIMIT 1`, root).Scan(&audited); err != nil {
		t.Fatal(err)
	}
	if audited != 17 {
		t.Fatalf("restore audit excludes later pages: %d", audited)
	}
	e.exec(`UPDATE conversations SET hidden_at=now()-interval '8 days' WHERE device_id=$1 AND hidden_at IS NOT NULL`, e.deviceID)
	cutoff := time.Now()
	purged, _, err := e.queue.purgeHidden(e.ctx, serverRules{}, &cutoff, "", "", "", "hidden_expired")
	if err != nil || purged != 17 {
		t.Fatalf("paged purge: purged=%d err=%v", purged, err)
	}
	if e.count(`SELECT count(*) FROM conversations WHERE device_id=$1`, e.deviceID) != 17 {
		t.Fatal("paged deletion starved or affected allowed copies")
	}
	if e.count(`SELECT count(*) FROM conversations WHERE device_id=$1 AND session_id=$2 AND hidden_at IS NULL`, e.deviceID, session) != 1 {
		t.Fatal("root was not retained visible")
	}
}

// Keep the ordinary small-source sweep at one full policy statement.
func TestHiddenSmallPolicyPageIsComplete(t *testing.T) {
	e := newEnv(t)
	id := policyStoredConversation(e, e.deviceID, uuid.NewString(), "")
	page, done, err := hiddenPolicyPage(e.ctx, e.pool, `c.id=$1 ORDER BY c.depth,c.id`, id)
	if err != nil || len(page) != 1 || !done {
		t.Fatalf("small page: rows=%d done=%v err=%v", len(page), done, err)
	}
}

func TestHiddenPagedLegacyMemberCheckFindsLaterDevicePolicy(t *testing.T) {
	e := newEnv(t)
	e.setRules("upload", "deny /sessions")
	session := uuid.NewString()
	root := policyStoredConversation(e, e.deviceID, session, "")
	e.exec(`INSERT INTO conversations(id,agent,session_id,device_id,user_id,parent_native_session_id,cwd,depth)
 SELECT gen_random_uuid(),'claude','agent-legacy-page-'||i,$1,$2,$3,'/sessions/vm/work',1 FROM generate_series(1,33) i`, e.deviceID, e.userID, session)
	other, held := uuid.NewString(), uuid.NewString()
	e.exec(`INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$2,'other','darwin',now())`, other, e.userID)
	e.exec(`INSERT INTO conversations(id,agent,session_id,device_id,user_id,parent_native_session_id,cwd,depth) VALUES($1,'claude','agent-held-last',$2,$3,$4,'/outside',2)`, held, other, e.userID, session)
	policyStoredLedger(e, other, "agent-held-last", false, "none", "allow")
	e.exec(hideTreeSQL, root, time.Now().Add(-8*24*time.Hour), "/sessions", int64(1), e.userID)
	cutoff := time.Now()
	purged, _, err := e.queue.purgeHidden(e.ctx, serverRules{}, &cutoff, "", "", "", "hidden_expired")
	if err != nil || purged != 1 {
		t.Fatalf("legacy purge: purged=%d err=%v", purged, err)
	}
	if e.count(`SELECT count(*) FROM conversations WHERE device_id=$1`, e.deviceID) != 0 {
		t.Fatal("legacy device tree not purged")
	}
	if e.count(`SELECT count(*) FROM conversations WHERE id=$1 AND hidden_at IS NOT NULL AND hidden_root=id AND hidden_scope='device'`, held) != 1 {
		t.Fatal("policy on a later member page was deleted or orphaned")
	}
}
