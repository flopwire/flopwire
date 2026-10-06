package ingest

import (
	"fmt"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/google/uuid"
)

func policyStoredConversation(e *env, device, session, parent string) string {
	e.t.Helper()
	id := uuid.NewString()
	e.exec(`INSERT INTO conversations(id,agent,session_id,device_id,user_id,cwd,parent_conversation_id) VALUES($1,'claude',$2,$3,$4,'/sessions/vm/work',NULLIF($5,'')::uuid)`, id, session, device, e.userID, parent)
	return id
}

func policyStoredLedger(e *env, device, session string, known bool, scope, mode string) {
	e.t.Helper()
	e.exec(`INSERT INTO session_policy_placements(device_id,agent,session_id,placements,current_mapping_known,evidence_scope,client_mode,revision) VALUES($1,'claude',$2,'[{"cwd":"/Users/test/project"}]',$3,$4,$5,1)`, device, session, known, scope, mode)
}

// A user-wide admin hide may already span both devices when Cowork is first
// identified. Removing that admin rule must not expose the device's held copy.
func TestPolicyRestoreLegacyUserHidePreservesDeviceLedger(t *testing.T) {
	e := newEnv(t)
	other := uuid.NewString()
	e.exec(`INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$2,'other','darwin',now())`, other, e.userID)
	session := uuid.NewString()
	root := policyStoredConversation(e, other, session, "")
	held := policyStoredConversation(e, e.deviceID, session, "")
	policyStoredLedger(e, e.deviceID, session, false, "unmapped", "allow")
	at := time.Now().UTC().Add(-8 * 24 * time.Hour)
	e.exec(hideTreeSQL, root, at, "old-admin-rule", int64(1), e.userID)
	rows, err := e.queue.convRows(e.ctx, `c.id=$1`, root)
	if err != nil || len(rows) != 1 {
		t.Fatalf("load root: %v (%d rows)", err, len(rows))
	}
	if err := e.queue.restore(e.ctx, serverRules{}, rows[0], "rules_changed"); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM conversations WHERE id=$1 AND hidden_at IS NULL AND hidden_scope='user'`, root); n != 1 {
		t.Fatal("legacy root was not restored")
	}
	if n := e.count(`SELECT count(*) FROM conversations WHERE id=$1 AND hidden_at IS NOT NULL AND hidden_root=id AND hidden_scope='device'`, held); n != 1 {
		t.Fatal("Cowork hold was exposed or retained a dead root")
	}
	cutoff := time.Now()
	purged, _, err := e.queue.purgeHidden(e.ctx, serverRules{}, &cutoff, "", "", "", "hidden_expired")
	if err != nil || purged != 0 {
		t.Fatalf("unknown hold expiry: purged=%d err=%v", purged, err)
	}
	if e.count(`SELECT count(*) FROM conversations WHERE id=$1`, held) != 1 {
		t.Fatal("unknown history was deleted")
	}
}

// Nested descendants inherit all ancestor policy facts. A child's known
// mapping cannot erase an ancestor's historical unknown or known denial.
func TestPolicyHiddenNestedAncestryAndDeviceExtension(t *testing.T) {
	e := newEnv(t)
	session := uuid.NewString()
	root := policyStoredConversation(e, e.deviceID, session, "")
	childSession := uuid.NewString()
	child := policyStoredConversation(e, e.deviceID, childSession, root)
	grandSession := uuid.NewString()
	grand := policyStoredConversation(e, e.deviceID, grandSession, child)
	policyStoredLedger(e, e.deviceID, session, false, "unmapped", "allow")
	policyStoredLedger(e, e.deviceID, grandSession, true, "mapped", "allow")
	rows, err := e.queue.convRows(e.ctx, `c.id=$1`, grand)
	if err != nil || len(rows) != 1 {
		t.Fatalf("load descendant: %v (%d rows)", err, len(rows))
	}
	if d := (serverRules{}).decideConv(rows[0]); d.Mode != pathpolicy.Local {
		t.Fatalf("ancestor hold lost: %+v", d)
	}
	e.exec(`UPDATE session_policy_placements SET client_mode='deny' WHERE device_id=$1 AND session_id=$2`, e.deviceID, session)
	rows, err = e.queue.convRows(e.ctx, `c.id=$1`, grand)
	if err != nil {
		t.Fatal(err)
	}
	if d := (serverRules{}).decideConv(rows[0]); d.Mode != pathpolicy.Deny {
		t.Fatalf("known deny did not outrank unknown: %+v", d)
	}
	// A child may arrive before its relationship is linked by the sink.
	e.exec(`UPDATE conversations SET parent_conversation_id=NULL,parent_native_session_id=$2 WHERE id=$1`, grand, childSession)
	rows, err = e.queue.convRows(e.ctx, `c.id=$1`, grand)
	if err != nil {
		t.Fatal(err)
	}
	if d := (serverRules{}).decideConv(rows[0]); d.Mode != pathpolicy.Deny {
		t.Fatalf("unlinked nested ancestry lost deny: %+v", d)
	}
	roots, err := e.queue.convRows(e.ctx, `c.id=$1`, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.queue.hide(e.ctx, serverRules{}, roots[0], (serverRules{}).decideConv(roots[0])); err != nil {
		t.Fatal(err)
	}
	other := uuid.NewString()
	e.exec(`INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$2,'other','darwin',now())`, other, e.userID)
	copy := policyStoredConversation(e, other, session, "")
	late := policyStoredConversation(e, e.deviceID, uuid.NewString(), grand)
	if err := e.queue.extendHidden(e.ctx); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT count(*) FROM conversations WHERE id=$1 AND hidden_at IS NULL`, copy) != 1 {
		t.Fatal("device hold crossed to another device")
	}
	if e.count(`SELECT count(*) FROM conversations WHERE id=$1 AND hidden_at IS NOT NULL AND hidden_scope='device'`, late) != 1 {
		t.Fatal("late descendant escaped device hold")
	}
}

// The sweep candidate may predate an acknowledged mapping update. Expiry must
// re-evaluate after the policy gate rather than tombstoning that old decision.
func TestPolicyPurgeRechecksAfterDeviceGate(t *testing.T) {
	e := newEnv(t)
	session := uuid.NewString()
	id := policyStoredConversation(e, e.deviceID, session, "")
	policyStoredLedger(e, e.deviceID, session, true, "mapped", "local")
	e.exec(hideDeviceTreeSQL, id, time.Now().UTC().Add(-8*24*time.Hour), "cowork-client-policy", int64(1), e.userID)
	rows, err := e.queue.convRows(e.ctx, `c.id=$1`, id)
	if err != nil || len(rows) != 1 {
		t.Fatalf("snapshot: %v", err)
	}
	stale := rows[0]
	gate, err := e.pool.Acquire(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Release()
	key := policyDeviceLockKey(e.deviceID)
	if _, err := gate.Exec(e.ctx, `SELECT pg_advisory_lock(hashtextextended($1,0))`, key); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = gate.Exec(e.ctx, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, key)
		}
	}()
	type result struct {
		p, r int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		p, r, err := e.queue.purgeHiddenOne(e.ctx, serverRules{}, stale, nil, "", "", "", "hidden_expired")
		done <- result{p, r, err}
	}()
	// This is the metadata writer's exclusive gate; acknowledge its new unknown
	// state before releasing the pending purge's shared gate.
	e.exec(`UPDATE session_policy_placements SET client_mode='allow',current_mapping_known=false,evidence_scope='unmapped',revision=revision+1 WHERE device_id=$1 AND session_id=$2`, e.deviceID, session)
	if _, err := gate.Exec(e.ctx, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, key); err != nil {
		t.Fatal(err)
	}
	locked = false
	select {
	case got := <-done:
		if got.err != nil || got.p != 0 || got.r != 0 {
			t.Fatalf("stale expiry decision: %+v", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("purge did not release its policy gate")
	}
	if e.count(`SELECT count(*) FROM conversations WHERE id=$1 AND hidden_at IS NOT NULL`, id) != 1 {
		t.Fatal("new unknown hold was destroyed")
	}
}

// A rule removal cannot be acknowledged between the fresh admin decision and
// expiry deletion. Hold deletion's natural lock to observe that ordering.
func TestPolicyPurgeSerializesAdminRuleRemoval(t *testing.T) {
	e := newEnv(t)
	e.setRules("upload", "deny /sessions")
	session := uuid.NewString()
	id := policyStoredConversation(e, e.deviceID, session, "")
	e.exec(hideTreeSQL, id, time.Now().UTC().Add(-8*24*time.Hour), "/sessions", int64(1), e.userID)
	rows, err := e.queue.convRows(e.ctx, `c.id=$1`, id)
	if err != nil || len(rows) != 1 {
		t.Fatalf("snapshot: %v", err)
	}
	gate, err := e.pool.Acquire(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Release()
	key := store.ConversationLockKey(e.userID, "claude", session)
	if _, err := gate.Exec(e.ctx, `SELECT pg_advisory_lock(hashtextextended($1,0))`, key); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = gate.Exec(e.ctx, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, key)
		}
	}()
	done := make(chan error, 1)
	go func() {
		p, _, err := e.queue.purgeHiddenOne(e.ctx, serverRules{}, rows[0], nil, "", "", "", "hidden_expired")
		if err == nil && p != 1 {
			err = fmt.Errorf("purged %d conversations", p)
		}
		done <- err
	}()
	waitFor := func(query string, args ...any) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			var yes bool
			if err := gate.QueryRow(e.ctx, query, args...).Scan(&yes); err != nil {
				t.Fatal(err)
			}
			if yes {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("expected lock ordering was not observed")
	}
	waitFor(`SELECT EXISTS(SELECT 1 FROM pg_locks WHERE relation='collection_policy'::regclass AND mode='RowShareLock' AND granted)`)
	updater, err := e.pool.Acquire(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	if err := updater.QueryRow(e.ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		updater.Release()
		t.Fatal(err)
	}
	updated := make(chan error, 1)
	go func() {
		defer updater.Release()
		_, err := updater.Exec(e.ctx, `UPDATE collection_policy SET path_rules='[]'::jsonb,rules_version=rules_version+1 WHERE singleton`)
		updated <- err
	}()
	waitFor(`SELECT cardinality(pg_blocking_pids($1))>0`, pid)
	if _, err := gate.Exec(e.ctx, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, key); err != nil {
		t.Fatal(err)
	}
	locked = false
	for _, ch := range []<-chan error{done, updated} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("policy serialization did not finish")
		}
	}
}
