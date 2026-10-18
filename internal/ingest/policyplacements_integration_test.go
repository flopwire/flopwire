package ingest

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/google/uuid"
)

func applyPolicy(t *testing.T, e *env, req *syncproto.PolicyPlacementsRequest) *syncproto.PolicyPlacementsResponse {
	t.Helper()
	r, err := (&Server{Pool: e.pool, Objects: e.objects}).PolicyPlacements(e.ctx, e.deviceID, req)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPolicyMetadataOnlyReadinessAndKnownCapture(t *testing.T) {
	e := newEnv(t)
	req := policyRequest()
	req.CurrentMappingKnown = false
	req.EvidenceScope = "none"
	req.Placements = nil
	if r := applyPolicy(t, e, req); r.Allowed || r.EvidenceScope != "none" {
		t.Fatalf("metadata readiness: %+v", r)
	}
	req = policyRequest()
	req.EvidenceScope = "none"
	if r := applyPolicy(t, e, req); !r.Allowed || r.EvidenceScope != "none" {
		t.Fatalf("first known scope: %+v", r)
	}
	sp := claudeAt(t, t.TempDir(), "-vm-work", req.SessionID, "/sessions/vm/work")
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	sync1(t, sy, sp)
	e.drain()
	if e.count(`SELECT count(*) FROM session_policy_placements WHERE session_id=$1 AND evidence_scope='mapped'`, req.SessionID) != 1 {
		t.Fatal("committed known bytes lack mapping proof")
	}
	if e.count(`SELECT count(*) FROM conversations WHERE session_id=$1 AND hidden_at IS NULL`, req.SessionID) != 1 {
		t.Fatal("known content hidden")
	}
	req.CurrentMappingKnown = false
	req.EvidenceScope = "mapped"
	if r := applyPolicy(t, e, req); r.Allowed || r.EvidenceScope != "mapped" {
		t.Fatalf("temporary unreadiness: %+v", r)
	}
	if e.count(`SELECT count(*) FROM conversations WHERE session_id=$1 AND hidden_at IS NOT NULL AND hidden_scope='device'`, req.SessionID) != 1 {
		t.Fatal("existing copy remains visible")
	}
	req.CurrentMappingKnown = true
	if r := applyPolicy(t, e, req); !r.Allowed {
		t.Fatalf("temporary readiness did not recover: %+v", r)
	}
	if e.count(`SELECT count(*) FROM conversations WHERE session_id=$1 AND hidden_at IS NULL`, req.SessionID) != 1 {
		t.Fatal("mapped copy not restored")
	}
	e.setRules("", "deny /Users/test/work/private")
	e.enforce()
	if e.count(`SELECT count(*) FROM conversations WHERE session_id=$1 AND hidden_at IS NOT NULL AND hidden_scope='device'`, req.SessionID) != 1 {
		t.Fatal("descendant admin rule not reapplied")
	}
}

func TestPolicyExistingCopiesRequireHistoryProofAndStayDeviceScoped(t *testing.T) {
	e := newEnv(t)
	req := policyRequest()
	sp := claudeAt(t, t.TempDir(), "-vm-work", req.SessionID, "/sessions/vm/work")
	sync1(t, e.syncer(devicesync.Config{SealAfter: -1}), sp)
	e.drain()
	other := uuid.NewString()
	e.exec(`INSERT INTO devices(id,user_id,name,platform,created_at)VALUES($1,$2,'other','darwin',now())`, other, e.userID)
	copy := policyStoredConversation(e, other, req.SessionID, "")
	if r := applyPolicy(t, e, req); r.Allowed || r.EvidenceScope != "unmapped" {
		t.Fatalf("current metadata certified old bytes: %+v", r)
	}
	if e.count(`SELECT count(*) FROM conversations WHERE device_id=$1 AND session_id=$2 AND hidden_at IS NOT NULL`, e.deviceID, req.SessionID) != 1 {
		t.Fatal("old copy not reconciled")
	}
	if e.count(`SELECT count(*) FROM conversations WHERE id=$1 AND hidden_at IS NULL`, copy) != 1 {
		t.Fatal("same native UUID on another device was restricted")
	}
	req.Placements = []syncproto.PolicyPlacement{{CWD: "/Users/test/new"}}
	if r := applyPolicy(t, e, req); r.Allowed || r.EvidenceScope != "unmapped" {
		t.Fatalf("later grant erased history: %+v", r)
	}
	if e.count(`SELECT jsonb_array_length(placements) FROM session_policy_placements WHERE session_id=$1`, req.SessionID) != 2 {
		t.Fatal("historical folder union shrank")
	}
}

func TestPolicyRecoveredAliasRequiresStoredNativeAndPathProof(t *testing.T) {
	e := newEnv(t)
	req := policyRequest()
	nativePath := "/Users/test/Library/Application Support/Claude/local-agent-mode-sessions/a/w/s/.claude/projects/vm/" + req.SessionID + ".jsonl"
	alias := uuid.NewString()
	sourceID := uuid.NewString()
	archivePath := "/recovery/archive.jsonl"
	e.exec(`INSERT INTO sources(id,device_id,agent,path,file_id,session_key,storage_kind,parser,first_seen_at) VALUES($1,$2,'claude',$3,'','cass-alias','cass_export','cass-export@1',now())`, sourceID, e.deviceID, archivePath)
	e.exec(`INSERT INTO generations(source_id,generation,size,captured_at,complete) VALUES($1,0,1,now(),true)`, sourceID)
	conv := policyStoredConversation(e, e.deviceID, "cass-alias", "")
	e.exec(`UPDATE conversations SET source_id=$2,extra=jsonb_build_object('recovered_history',true,'cass_external_id',$3::text,'cass_source_path',$4::text) WHERE id=$1`, conv, sourceID, req.SessionID, nativePath)
	unrelated := policyStoredConversation(e, e.deviceID, alias, "")
	req.Sources = []syncproto.PolicySource{{Path: nativePath, FileID: "1:1", Generation: 0}}
	if r := applyPolicy(t, e, req); r.Allowed || r.EvidenceScope != "unmapped" {
		t.Fatalf("recovery evidence not held: %+v", r)
	}
	if e.count(`SELECT count(*) FROM conversations WHERE id=$1 AND hidden_at IS NOT NULL AND hidden_scope='device'`, conv) != 1 {
		t.Fatal("proven recovered alias remains visible")
	}
	if e.count(`SELECT count(*) FROM conversations WHERE id=$1 AND hidden_at IS NULL`, unrelated) != 1 {
		t.Fatal("unrelated recovery alias affected")
	}
}

// Stop a flush after its initial policy check and object write, then acknowledge
// a restriction. Its manifest commit must reread policy before becoming durable.
type policyPutBarrier struct {
	Objects
	entered, release chan struct{}
}

func (b *policyPutBarrier) Put(ctx context.Context, key string, data []byte) error {
	close(b.entered)
	select {
	case <-b.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return b.Objects.Put(ctx, key, data)
}

func TestPolicyRestrictionDuringFlushPreventsManifestCommit(t *testing.T) {
	e := newEnv(t)
	req := policyRequest()
	req.EvidenceScope = "none"
	applyPolicy(t, e, req)
	data := []byte("synthetic bytes awaiting manifest commit\n")
	hash := syncproto.Sum(data)
	wire := &syncproto.FlushRequest{Header: syncproto.FlushHeader{Version: 1, CapturedAt: time.Now(), Source: syncproto.Source{Path: "/synthetic/" + req.SessionID + ".jsonl", FileID: "1:1", SessionKey: req.SessionID, Agent: "claude", StorageKind: "jsonl_append", Parser: "claude@1"}, Entries: []syncproto.Entry{{Hash: hash, Size: int64(len(data))}}, Bodies: bodyOf(data)}, Payload: zpayload(data)}
	var frame bytes.Buffer
	if err := syncproto.EncodeFlush(&frame, wire); err != nil {
		t.Fatal(err)
	}
	hdr, payload, err := syncproto.DecodeFlush(&frame)
	if err != nil {
		t.Fatal(err)
	}
	b := &policyPutBarrier{Objects: e.objects, entered: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
	defer cancel()
	server := &Server{Pool: e.pool, Objects: b}
	done := make(chan error, 1)
	go func() { _, err := server.Flush(ctx, e.deviceID, hdr, payload); done <- err }()
	select {
	case <-b.entered:
	case <-ctx.Done():
		t.Fatal("flush never reached object barrier")
	}
	req.CurrentMappingKnown = false
	applyPolicy(t, e, req)
	close(b.release)
	select {
	case err := <-done:
		var policyErr *Error
		if !errors.As(err, &policyErr) || policyErr.Code != "policy_placements_held" {
			t.Fatalf("flush result %v", err)
		}
	case <-ctx.Done():
		t.Fatal("flush did not finish")
	}
	if e.count(`SELECT count(*) FROM generations`) > 0 {
		t.Fatal("stale flush committed raw evidence after restriction acknowledgement")
	}
}

func TestPolicyStaleParseRechecksAfterSessionLock(t *testing.T) {
	e := newEnv(t)
	req := policyRequest()
	req.EvidenceScope = "none"
	applyPolicy(t, e, req)
	srcID := uuid.NewString()
	e.exec(`INSERT INTO sources(id,device_id,agent,path,file_id,session_key,storage_kind,parser,first_seen_at) VALUES($1,$2,'claude','/synthetic/native.jsonl','1:1',$3,'jsonl_append','claude@1',now())`, srcID, e.deviceID, req.SessionID)
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(e.ctx)
	if _, err := tx.Exec(e.ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, store.ConversationLockKey(e.userID, "claude", req.SessionID)); err != nil {
		t.Fatal(err)
	}
	s := newSink(e.ctx, e.pool, source{id: srcID, deviceID: e.deviceID, userID: e.userID, agent: "claude"})
	s.convs[req.SessionID] = &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: req.SessionID, Cwd: "/sessions/vm/work"}
	done := make(chan error, 1)
	go func() { done <- s.flush() }()
	waitForLockWait(t, e, "stale policy parse")
	if _, err := tx.Exec(e.ctx, `UPDATE session_policy_placements SET current_mapping_known=false WHERE device_id=$1 AND session_id=$2`, e.deviceID, req.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("parse did not finish")
	}
	if e.count(`SELECT count(*) FROM conversations WHERE session_id=$1 AND hidden_at IS NOT NULL AND hidden_scope='device'`, req.SessionID) != 1 {
		t.Fatal("parse exposed rows after waiting on a restriction")
	}
}

func TestPolicyKnownChildAckIncludesParentHistory(t *testing.T) {
	e := newEnv(t)
	parent := policyRequest()
	parent.CurrentMappingKnown = false
	parent.EvidenceScope = "unmapped"
	applyPolicy(t, e, parent)
	child := policyRequest()
	child.SessionID = uuid.NewString()
	child.ParentSessionID = parent.SessionID
	if ack := applyPolicy(t, e, child); ack.Allowed || ack.EvidenceScope != "unmapped" {
		t.Fatalf("known child erased controlling history: %+v", ack)
	}
}

func TestPolicyRecoveryAliasSurvivesCurrentSourceReplacement(t *testing.T) {
	e := newEnv(t)
	req := policyRequest()
	nativePath := "/synthetic/" + req.SessionID + ".jsonl"
	oldSource := uuid.NewString()
	e.exec(`INSERT INTO sources(id,device_id,agent,path,file_id,session_key,storage_kind,parser,first_seen_at) VALUES($1,$2,'claude','/recovery/old.jsonl','','cass-alias','cass_export','cass-export@1',now())`, oldSource, e.deviceID)
	e.exec(`INSERT INTO generations(source_id,generation,size,captured_at,complete) VALUES($1,0,1,now(),true)`, oldSource)
	conv := policyStoredConversation(e, e.deviceID, "cass-alias", "")
	e.exec(`UPDATE conversations SET source_id=$2,extra=jsonb_build_object('recovered_history',true,'cass_external_id',$3::text,'cass_source_path',$4::text) WHERE id=$1`, conv, oldSource, req.SessionID, nativePath)
	req.Sources = []syncproto.PolicySource{{Path: nativePath, FileID: "1:1"}}
	if ack := applyPolicy(t, e, req); ack.Allowed {
		t.Fatal("old alias was not held")
	}
	newSource := uuid.NewString()
	e.exec(`INSERT INTO sources(id,device_id,agent,path,file_id,session_key,storage_kind,parser,first_seen_at) VALUES($1,$2,'claude','/recovery/new.jsonl','2:2','cass-alias','jsonl_append','claude@1',now())`, newSource, e.deviceID)
	e.exec(`UPDATE conversations SET source_id=$2,extra='{}' WHERE id=$1`, conv, newSource)
	rows, err := e.queue.convRows(e.ctx, `c.id=$1`, conv)
	if err != nil || len(rows) != 1 {
		t.Fatalf("load replacement: %v", err)
	}
	if d := (serverRules{}).decideConv(rows[0]); d.Mode != pathpolicy.Local || !isPolicyHold(d) {
		t.Fatalf("replacement escaped durable alias policy: %+v", d)
	}
}

// A new grandchild need not exist in registration's tree snapshot. The device
// policy gate keeps its first write after the ancestor restriction commits.
func TestPolicyNewGrandchildWaitsForAncestorRegistration(t *testing.T) {
	e := newEnv(t)
	req := policyRequest()
	req.EvidenceScope = "none"
	applyPolicy(t, e, req)
	root := policyStoredConversation(e, e.deviceID, req.SessionID, "")
	childID := uuid.NewString()
	policyStoredConversation(e, e.deviceID, childID, root)
	grandID := uuid.NewString()
	srcID := uuid.NewString()
	e.exec(`INSERT INTO sources(id,device_id,agent,path,file_id,session_key,storage_kind,parser,first_seen_at) VALUES($1,$2,'claude','/synthetic/grand.jsonl','1:1',$3,'jsonl_append','claude@1',now())`, srcID, e.deviceID, grandID)
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(e.ctx)
	if _, err := tx.Exec(e.ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, policyDeviceLockKey(e.deviceID)); err != nil {
		t.Fatal(err)
	}
	s := newSink(e.ctx, e.pool, source{id: srcID, deviceID: e.deviceID, userID: e.userID, agent: "claude"})
	s.convs[grandID] = &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: grandID, ParentSessionID: childID, Cwd: "/sessions/vm/work"}
	done := make(chan error, 1)
	go func() { done <- s.flush() }()
	waitForLockWait(t, e, "new grandchild policy gate")
	if _, err := tx.Exec(e.ctx, `UPDATE session_policy_placements SET current_mapping_known=false,evidence_scope='mapped' WHERE device_id=$1 AND session_id=$2`, e.deviceID, req.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := reconcileSessionPolicy(e.ctx, tx, serverRules{}, e.userID, e.deviceID, "claude", req.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("grandchild did not finish")
	}
	if e.count(`SELECT count(*) FROM conversations WHERE session_id=$1 AND hidden_at IS NOT NULL AND hidden_scope='device'`, grandID) != 1 {
		t.Fatal("new grandchild appeared after ancestor restriction ack")
	}
}

func TestPolicyRegistrationReconcilesTransitiveMetadataParents(t *testing.T) {
	e := newEnv(t)
	root := policyRequest()
	root.EvidenceScope = "none"
	applyPolicy(t, e, root)
	child := policyRequest()
	child.SessionID = uuid.NewString()
	child.ParentSessionID = root.SessionID
	child.EvidenceScope = "none"
	applyPolicy(t, e, child)
	grand := policyRequest()
	grand.SessionID = uuid.NewString()
	grand.ParentSessionID = child.SessionID
	grand.EvidenceScope = "none"
	applyPolicy(t, e, grand)
	conv := policyStoredConversation(e, e.deviceID, grand.SessionID, "")
	root.EvidenceScope = "mapped" // Restrict captured parent history, not an empty readiness placeholder.
	root.CurrentMappingKnown = false
	if ack := applyPolicy(t, e, root); ack.Allowed {
		t.Fatal("unknown root was allowed")
	}
	if e.count(`SELECT count(*) FROM conversations WHERE id=$1 AND hidden_at IS NOT NULL AND hidden_scope='device'`, conv) != 1 {
		t.Fatal("metadata-only parent chain escaped acknowledged root restriction")
	}
}

func TestPolicyEndpointKnownHostPlacementWithoutNativeCwd(t *testing.T) {
	for _, floor := range []string{"local", "exclude"} {
		t.Run(floor, func(t *testing.T) {
			e := newEnv(t)
			e.setRules(floor)
			req := policyRequest()
			req.EvidenceScope = "none"
			applyPolicy(t, e, req)
			conv := policyStoredConversation(e, e.deviceID, req.SessionID, "")
			e.exec(`UPDATE conversations SET cwd=NULL WHERE id=$1`, conv)
			if ack := applyPolicy(t, e, req); !ack.Allowed {
				t.Fatalf("known host scope rejected: %+v", ack)
			}
			if e.count(`SELECT count(*) FROM conversations WHERE id=$1 AND hidden_at IS NULL`, conv) != 1 {
				t.Fatal("metadata reconciliation retained missing-native-cwd floor despite verified host scope")
			}
			e.enforce()
			if e.count(`SELECT count(*) FROM conversations WHERE id=$1 AND hidden_at IS NULL`, conv) != 1 {
				t.Fatal("admin sweep disagreed with verified host scope")
			}
		})
	}
}

func TestPolicyOppositeDeviceTreesDoNotInvertNativeLockOrder(t *testing.T) {
	e := newEnv(t)
	other := uuid.NewString()
	e.exec(`INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$2,'other','darwin',now())`, other, e.userID)
	r, c := uuid.NewString(), uuid.NewString()
	ownRoot := policyStoredConversation(e, e.deviceID, r, "")
	policyStoredConversation(e, e.deviceID, c, ownRoot)
	otherRoot := policyStoredConversation(e, other, c, "")
	policyStoredConversation(e, other, r, otherRoot)
	server := &Server{Pool: e.pool, Objects: e.objects}
	for range 5 {
		ready := make(chan struct{})
		done := make(chan error, 2)
		for _, device := range []string{e.deviceID, other} {
			go func(device string) {
				req := policyRequest()
				req.EvidenceScope = "none"
				req.ClientMode = "local"
				req.SessionID = r
				if device == other {
					req.SessionID = c
				}
				<-ready
				ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second)
				defer cancel()
				_, err := server.PolicyPlacements(ctx, device, req)
				done <- err
			}(device)
		}
		close(ready)
		for range 2 {
			if err := <-done; err != nil {
				t.Fatalf("device-native collision caused metadata deadlock/retry: %v", err)
			}
		}
	}
}

func TestPolicyMetadataReportsHomeBeforeFolderEvaluation(t *testing.T) {
	e := newEnv(t)
	e.exec(`UPDATE devices SET home=NULL WHERE id=$1`, e.deviceID)
	e.setRules("", "deny ~/Code/private")
	req := policyRequest()
	req.EvidenceScope = "none"
	req.Placements = []syncproto.PolicyPlacement{{CWD: "/Users/test/Code"}}
	if ack := applyPolicy(t, e, req); ack.Allowed {
		t.Fatal("unknown home bypassed restrictive tilde rule")
	}
	conv := policyStoredConversation(e, e.deviceID, req.SessionID, "")
	req.Device = &syncproto.DeviceDirs{Home: "/Users/test"}
	if ack := applyPolicy(t, e, req); ack.Allowed {
		t.Fatal("reported home failed to enforce selected descendant deny")
	}
	if e.count(`SELECT count(*) FROM conversations WHERE id=$1 AND hidden_at IS NOT NULL`, conv) != 1 {
		t.Fatal("metadata-only deny failed to hide existing copy")
	}
	if e.count(`SELECT count(*) FROM devices WHERE id=$1 AND home='/Users/test'`, e.deviceID) != 1 {
		t.Fatal("home not durably registered")
	}
	for _, home := range []string{"", "/Users/other"} {
		req.Device.Home = home
		_, err := (&Server{Pool: e.pool}).PolicyPlacements(e.ctx, e.deviceID, req)
		var refused *Error
		if !errors.As(err, &refused) || refused.Code != "device_home_conflict" {
			t.Fatalf("changed home accepted: %q %v", home, err)
		}
		conn, err := e.pool.Acquire(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = recordDeviceDirs(e.ctx, conn, e.deviceID, req.Device)
		conn.Release()
		if !errors.As(err, &refused) || refused.Code != "device_home_conflict" {
			t.Fatalf("flush changed home: %q %v", home, err)
		}
	}
}

func TestPolicyMetadataDoesNotWaitOnOtherDeviceNativeLocks(t *testing.T) {
	e := newEnv(t)
	req := policyRequest()
	req.EvidenceScope = "none"
	policyStoredConversation(e, e.deviceID, req.SessionID, "")
	other := uuid.NewString()
	e.exec(`INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$2,'other','darwin',now())`, other, e.userID)
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(e.ctx)
	if _, err = tx.Exec(e.ctx, `SELECT pg_advisory_xact_lock_shared(hashtextextended($1,0))`, policyDeviceLockKey(other)); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(e.ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, store.ConversationLockKey(e.userID, "claude", req.SessionID)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(e.ctx, 2*time.Second)
	defer cancel()
	if _, err = (&Server{Pool: e.pool}).PolicyPlacements(ctx, e.deviceID, req); err != nil {
		t.Fatalf("metadata waited for unrelated device's sink natural lock: %v", err)
	}
}

func TestPolicyMetadataPinsAdminRulesThroughRestoreWait(t *testing.T) {
	e := newEnv(t)
	req := policyRequest()
	req.EvidenceScope = "none"
	applyPolicy(t, e, req)
	conv := policyStoredConversation(e, e.deviceID, req.SessionID, "")
	req.CurrentMappingKnown = false
	applyPolicy(t, e, req)
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(e.ctx)
	if _, err = tx.Exec(e.ctx, `SELECT id FROM conversations WHERE id=$1 FOR UPDATE`, conv); err != nil {
		t.Fatal(err)
	}
	req.CurrentMappingKnown = true
	ackDone := make(chan error, 1)
	go func() { _, err := (&Server{Pool: e.pool}).PolicyPlacements(e.ctx, e.deviceID, req); ackDone <- err }()
	waitForLockWait(t, e, "metadata restore row")
	ruleDone := make(chan error, 1)
	go func() {
		_, err := e.pool.Exec(e.ctx, `UPDATE collection_policy SET path_rules='["deny /Users/test/work"]',rules_version=rules_version+1 WHERE singleton`)
		ruleDone <- err
	}()
	select {
	case err := <-ruleDone:
		t.Fatalf("admin rule changed through metadata restore guard: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ackDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("metadata did not finish")
	}
	select {
	case err := <-ruleDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("admin rule update did not finish")
	}
	e.enforce()
	if e.count(`SELECT count(*) FROM conversations WHERE id=$1 AND hidden_at IS NOT NULL`, conv) != 1 {
		t.Fatal("completed admin deny sweep left metadata-restored row visible")
	}
}

func TestPolicyHistoricalUnknownExcludeReconcilesAndPurgesKnownCurrentScope(t *testing.T) {
	e := newEnv(t)
	req := policyRequest()
	req.EvidenceScope = "none"
	applyPolicy(t, e, req)
	conv := policyStoredConversation(e, e.deviceID, req.SessionID, "")
	req.EvidenceScope = "unmapped"
	e.setRules("exclude")
	if ack := applyPolicy(t, e, req); ack.Allowed || ack.EvidenceScope != "unmapped" {
		t.Fatalf("historical exclusion ack: %+v", ack)
	}
	if e.count(`SELECT count(*) FROM conversations WHERE id=$1 AND hidden_at IS NOT NULL`, conv) != 1 {
		t.Fatal("explicit exclusion did not hide stored history")
	}
	e.exec(`UPDATE conversations SET hidden_at=now()-interval '8 days' WHERE id=$1`, conv)
	e.enforce()
	if e.count(`SELECT count(*) FROM conversations WHERE id=$1`, conv) != 0 {
		t.Fatal("explicit exclusion was treated as nonpurgeable unknown hold")
	}
}

func TestPolicyRecoveredSourceReferenceHandlesMissingNativeTranscript(t *testing.T) {
	e := newEnv(t)
	req := policyRequest()
	original := "/gone/cowork/" + req.SessionID + ".jsonl"
	archive := "/recovery/missing-native.jsonl"
	src := uuid.NewString()
	e.exec(`INSERT INTO sources(id,device_id,agent,path,file_id,session_key,storage_kind,parser,first_seen_at) VALUES($1,$2,'claude',$3,'cass-file','cass-only','cass_export','cass-export@1',now())`, src, e.deviceID, archive)
	e.exec(`INSERT INTO generations(source_id,generation,size,captured_at,complete) VALUES($1,0,1,now(),true)`, src)
	conv := policyStoredConversation(e, e.deviceID, "cass-only", "")
	e.exec(`UPDATE conversations SET source_id=$2,extra=jsonb_build_object('recovered_history',true,'cass_external_id',$3::text,'cass_source_path',$4::text) WHERE id=$1`, conv, src, req.SessionID, original)
	req.RecoverySources = []syncproto.PolicyRecoverySource{{Source: syncproto.PolicySource{Path: archive, FileID: "cass-file", Generation: 0}, OriginalPath: original}}
	if ack := applyPolicy(t, e, req); ack.Allowed || ack.EvidenceScope != "unmapped" {
		t.Fatalf("recovered proof permittedsharing: %+v", ack)
	}
	if e.count(`SELECT count(*) FROM conversations WHERE id=$1 AND hidden_at IS NOT NULL`, conv) != 1 {
		t.Fatal("missing-native recovered copy remained visible")
	}
	if e.count(`SELECT count(*) FROM source_policy_placements WHERE device_id=$1 AND session_id=$2 AND path=$3`, e.deviceID, req.SessionID, archive) != 1 {
		t.Fatal("recovery restriction not durably bound")
	}
	for _, change := range []func(*syncproto.PolicyRecoverySource){
		func(r *syncproto.PolicyRecoverySource) { r.Source.Generation = 1 },
		func(r *syncproto.PolicyRecoverySource) { r.Source.FileID = "unrelated-file" },
		func(r *syncproto.PolicyRecoverySource) { r.OriginalPath = "/different/native.jsonl" },
	} {
		bad := *req
		bad.RecoverySources = append([]syncproto.PolicyRecoverySource(nil), req.RecoverySources...)
		change(&bad.RecoverySources[0])
		_, err := (&Server{Pool: e.pool}).PolicyPlacements(e.ctx, e.deviceID, &bad)
		var refused *Error
		if !errors.As(err, &refused) || refused.Code != "recovery_proof_missing" {
			t.Fatalf("unproven recovery reference accepted: %v", err)
		}
	}
	other := uuid.NewString()
	e.exec(`INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$2,'other','darwin',now())`, other, e.userID)
	if _, err := (&Server{Pool: e.pool}).PolicyPlacements(e.ctx, other, req); err == nil {
		t.Fatal("another device reused recovery reference")
	}
}

func TestPolicyCapturedChildRestrictionsProtectWholeVerifiedSession(t *testing.T) {
	for _, floor := range []string{"", "exclude"} {
		t.Run(floor, func(t *testing.T) {
			e := newEnv(t)
			e.setRules(floor)
			root := policyRequest()
			root.EvidenceScope = "none"
			applyPolicy(t, e, root)
			conv := policyStoredConversation(e, e.deviceID, root.SessionID, "")
			child := policyRequest()
			child.SessionID = uuid.NewString()
			child.ParentSessionID = root.SessionID
			child.EvidenceScope = "none"
			child.CurrentMappingKnown = false
			child.Placements = nil
			applyPolicy(t, e, child)
			if ack := applyPolicy(t, e, root); !ack.Allowed {
				t.Fatalf("metadata-only child tainted complete first capture: %+v", ack)
			}
			child.EvidenceScope = "unmapped"
			applyPolicy(t, e, child)
			if ack := applyPolicy(t, e, root); ack.Allowed || ack.EvidenceScope != "unmapped" {
				t.Fatalf("root escaped captured child scope: %+v", ack)
			}
			if e.count(`SELECT count(*) FROM conversations WHERE id=$1 AND hidden_at IS NOT NULL`, conv) != 1 {
				t.Fatal("child history failed to reconcile root")
			}
			rootPath := "/synthetic/" + root.SessionID + ".jsonl"
			_, err := checkFlushPolicy(e.ctx, e.pool, e.deviceID, syncproto.Source{Agent: "claude", Path: rootPath, FileID: "root-file", SessionKey: root.SessionID})
			var refused *Error
			if !errors.As(err, &refused) || refused.Code != "policy_placements_held" {
				t.Fatalf("root flush escaped component: %v", err)
			}
			rows, err := e.queue.convRows(e.ctx, `c.id=$1`, conv)
			if err != nil || len(rows) != 1 {
				t.Fatal(err)
			}
			d := mustRules(t, e).decideConv(rows[0])
			if floor == "exclude" {
				if d.Mode != pathpolicy.Deny || !d.Unplaceable {
					t.Fatalf("group exclusion weakened: %+v", d)
				}
			} else if !isPolicyHold(d) {
				t.Fatalf("group unknown not retained: %+v", d)
			}
		})
	}
}

func TestPolicyCompanionUnknownParentReadinessCannotUpload(t *testing.T) {
	e := newEnv(t)
	req := policyRequest()
	req.EvidenceScope = "none"
	req.CurrentMappingKnown = false
	req.Placements = nil
	applyPolicy(t, e, req)
	parentPath := "/synthetic/" + req.SessionID + ".jsonl"
	src := syncproto.Source{Agent: "claude", StorageKind: "companion", Path: "/synthetic/output.txt", FileID: "output-file", Parent: &syncproto.SourceRef{Path: parentPath, FileID: "parent-file"}}
	_, err := checkFlushPolicy(e.ctx, e.pool, e.deviceID, src)
	var refused *Error
	if !errors.As(err, &refused) || refused.Code != "policy_placements_held" {
		t.Fatalf("unknown parent allowed companion bytes: %v", err)
	}
	if e.count(`SELECT count(*) FROM session_policy_placements WHERE session_id=$1 AND evidence_scope='none'`, req.SessionID) != 1 {
		t.Fatal("held companion marked history captured")
	}
}

func TestPolicyParentCaptureDoesNotMarkMetadataChildrenCaptured(t *testing.T) {
	e := newEnv(t)
	root := policyRequest()
	root.EvidenceScope = "none"
	applyPolicy(t, e, root)
	children := make([]*syncproto.PolicyPlacementsRequest, 0, 2)
	for _, known := range []bool{true, false} {
		child := policyRequest()
		child.SessionID = uuid.NewString()
		child.ParentSessionID = root.SessionID
		child.EvidenceScope = "none"
		child.CurrentMappingKnown = known
		if !known {
			child.Placements = nil
		}
		applyPolicy(t, e, child)
		children = append(children, child)
	}
	sp := claudeAt(t, t.TempDir(), "-vm-work", root.SessionID, "/sessions/vm/work")
	sync1(t, e.syncer(devicesync.Config{SealAfter: -1}), sp)
	e.drain()
	for _, child := range children {
		if e.count(`SELECT count(*) FROM session_policy_placements WHERE session_id=$1 AND evidence_scope='none'`, child.SessionID) != 1 {
			t.Fatal("parent upload marked child as captured")
		}
		child.CurrentMappingKnown = false
		child.Placements = nil
		applyPolicy(t, e, child)
	}
	root.EvidenceScope = "mapped"
	if ack := applyPolicy(t, e, root); !ack.Allowed {
		t.Fatalf("uncaptured child readiness held parent history: %+v", ack)
	}
}
