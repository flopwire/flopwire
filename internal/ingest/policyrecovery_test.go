package ingest

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestVerifiedRecoveryBatchAndOverflowBindings(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, pgtest.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	user, device, other, native := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	exec(`INSERT INTO users(id,email,name,role,identity_type,created_at) VALUES($1,$2,'test','member','human',now())`, user, user+"@example.test")
	for _, d := range []string{device, other} {
		exec(`INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$2,'test','darwin',now())`, d, user)
	}
	source := uuid.NewString()
	exec(`INSERT INTO sources(id,device_id,agent,path,file_id,storage_kind,parser,first_seen_at) VALUES($1,$2,'claude','/export','', 'cass_export','cass@1',now())`, source, device)
	exec(`INSERT INTO generations(source_id,generation,size,captured_at,complete) VALUES($1,0,10,now(),true),($1,1,10,now(),true)`, source)
	exec(`INSERT INTO conversations(id,source_id,device_id,user_id,agent,session_id,extra) VALUES($1,$2,$3,$4,'claude','cass-alias',jsonb_build_object('recovered_history',true,'cass_external_id',$5::text,'cass_source_path','/original'))`, uuid.NewString(), source, device, user, native)
	req := &syncproto.PolicyPlacementsRequest{Agent: "claude", SessionID: native, RecoverySources: []syncproto.PolicyRecoverySource{{Source: syncproto.PolicySource{Path: "/export", Generation: 0}, OriginalPath: "/original"}}}
	ids, err := verifiedRecoveryAliases(ctx, pool, device, req)
	if err != nil || len(ids) != 1 || ids[0] != source {
		t.Fatalf("verified empty export identity: %v %v", ids, err)
	}
	for _, tc := range []struct {
		name, dev  string
		generation int64
		path       string
	}{
		{"other device", other, 0, "/original"}, {"absent generation", device, 2, "/original"}, {"wrong original", device, 0, "/different"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := *req
			copy.RecoverySources = append([]syncproto.PolicyRecoverySource(nil), req.RecoverySources...)
			copy.RecoverySources[0].Source.Generation = tc.generation
			copy.RecoverySources[0].OriginalPath = tc.path
			_, err := verifiedRecoveryAliases(ctx, pool, tc.dev, &copy)
			var e *Error
			if !errors.As(err, &e) || e.Status != http.StatusConflict {
				t.Fatalf("wanted proof rejection: %v", err)
			}
		})
	}
	exec(`UPDATE sources SET storage_kind='jsonl_append' WHERE id=$1`, source)
	if _, err := verifiedRecoveryAliases(ctx, pool, device, req); err == nil {
		t.Fatal("ordinary source accepted")
	}
	exec(`UPDATE sources SET storage_kind='cass_export' WHERE id=$1`, source)
	ref := req.RecoverySources[0]
	req.RecoverySources = make([]syncproto.PolicyRecoverySource, 300)
	for i := range req.RecoverySources {
		req.RecoverySources[i] = ref
	}
	if ids, err := verifiedRecoveryAliases(ctx, pool, device, req); err != nil || len(ids) != 1 {
		t.Fatalf("bounded 300 reference batch: %v %v", ids, err)
	}
	req.RecoverySources[299].Source.Generation = 9
	if _, err := verifiedRecoveryAliases(ctx, pool, device, req); err == nil {
		t.Fatal("last proof in oversized batch ignored")
	}
	req.RecoverySources = nil
	req.Sources = []syncproto.PolicySource{{Path: "/original"}}
	// A large implicit alias set must be bound in full even when diagnostics cap.
	exec(`INSERT INTO sources(id,device_id,agent,path,file_id,storage_kind,parser,first_seen_at)
 SELECT md5('recovery-source-'||i)::uuid,$1,'claude','/exports/'||i,'','cass_export','cass@1',now() FROM generate_series(1,1025) i`, device)
	exec(`INSERT INTO generations(source_id,generation,size,captured_at,complete) SELECT id,0,10,now(),true FROM sources WHERE device_id=$1 AND path LIKE '/exports/%'`, device)
	exec(`INSERT INTO conversations(id,source_id,device_id,user_id,agent,session_id,extra)
 SELECT md5('recovery-conversation-'||s.path)::uuid,s.id,$1,$2,'claude','cass-'||s.path,jsonb_build_object('recovered_history',true,'cass_external_id',$3::text,'cass_source_path','/original') FROM sources s WHERE device_id=$1 AND path LIKE '/exports/%'`, device, user, native)
	ids, err = verifiedRecoveryAliases(ctx, pool, device, req)
	var limit *Error
	if len(ids) != 1025 || !errors.As(err, &limit) || limit.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("overflow: ids=%d err=%v", len(ids), err)
	}
	exec(`INSERT INTO session_policy_placements(device_id,agent,session_id,current_mapping_known,evidence_scope,client_mode) VALUES($1,'claude',$2,false,'unmapped','local')`, device, native)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err = bindVerifiedRecoverySources(ctx, tx, device, req); err != nil {
		t.Fatal(err)
	}
	var bindings, links int
	if err = tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM source_policy_placements WHERE device_id=$1),(SELECT count(*) FROM session_policy_links WHERE device_id=$1)`, device).Scan(&bindings, &links); err != nil {
		t.Fatal(err)
	}
	if bindings != 1027 || links != 1026 {
		t.Fatalf("full alias binding: generations=%d links=%d", bindings, links)
	}
	var owners, captures int
	if err = tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM source_policy_identity WHERE device_id=$1),(SELECT count(*) FROM source_policy_capture_identity WHERE device_id=$1)`, device).Scan(&owners, &captures); err != nil {
		t.Fatal(err)
	}
	if owners != 1026 || captures != 1026 {
		t.Fatalf("full immutable binding owners=%d captures=%d", owners, captures)
	}
	good := syncproto.Source{Path: "/export", FileID: "", Agent: "claude", StorageKind: "cass_export", Parser: "cass@2"}
	if err = checkPolicySourceIdentity(ctx, tx, device, good); err != nil {
		t.Fatalf("same capture parser version: %v", err)
	}
	for _, change := range []func(*syncproto.Source){
		func(s *syncproto.Source) { s.Agent = "codex" },
		func(s *syncproto.Source) { s.SessionKey = uuid.NewString() },
		func(s *syncproto.Source) { s.StorageKind = "jsonl_append" },
		func(s *syncproto.Source) { s.Parser = "claude@1" },
		func(s *syncproto.Source) { s.Parent = &syncproto.SourceRef{Path: "/different", FileID: "1:1"} },
	} {
		bad := good
		change(&bad)
		if err = checkPolicySourceIdentity(ctx, tx, device, bad); err == nil {
			t.Fatalf("capture rewrite accepted: %+v", bad)
		}
	}
	// Corrupt ownership outside the capped diagnostics set: the whole batch must
	// reject before writing any missing owner, capture, placement, or link.
	var lastPath string
	if err = tx.QueryRow(ctx, `SELECT path FROM sources WHERE device_id=$1 ORDER BY id OFFSET 1025 LIMIT 1`, device).Scan(&lastPath); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE source_policy_identity SET owner_session_id='different' WHERE device_id=$1 AND path=$2`, device, lastPath); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM source_policy_capture_identity WHERE device_id=$1 AND path='/export'`, device); err != nil {
		t.Fatal(err)
	}
	err = bindVerifiedRecoverySources(ctx, tx, device, req)
	var conflict *Error
	if !errors.As(err, &conflict) || conflict.Code != "policy_source_identity_conflict" {
		t.Fatalf("overflow identity conflict not rejected: %v", err)
	}
	var remaining int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM source_policy_capture_identity WHERE device_id=$1 AND path='/export'`, device).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatal("conflicting batch partially wrote capture")
	}

}
