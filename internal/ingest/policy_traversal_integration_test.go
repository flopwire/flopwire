package ingest

import (
	"context"
	_ "embed"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Keep the previous traversal as an independent result-set oracle. In
// particular, path edges remain valid even with a different nonnull parent ID.
//
//go:embed testdata/policy_historical_unmapped_baseline.sql
var previousPolicyHistoricalSQL string

func previousPolicyOwnedSQL() string {
	return strings.Split(previousPolicyHistoricalSQL, "\n SELECT EXISTS")[0]
}

func policyOwnedSQL() string {
	return strings.Split(policyHistoricalUnmappedSQL, "\n SELECT EXISTS")[0]
}

func policyOwnedIDs(t *testing.T, e *env, query string, args ...any) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(e.ctx, 20*time.Second)
	defer cancel()
	rows, err := e.pool.Query(ctx, query+` SELECT id::text FROM owned ORDER BY id`, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestPolicyOwnedTraversalPreservesEdgesAndCycles(t *testing.T) {
	e := newEnv(t)
	other := uuid.NewString()
	e.exec(`INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$2,'other','darwin',now())`, other, e.userID)
	root, direct, exact, wildcard, mixed, overlap, grandchild := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	insert := func(id, device, agent, path, file string, parent any, parentPath any, parentFile any, session string) {
		e.exec(`INSERT INTO sources(id,device_id,agent,path,file_id,session_key,storage_kind,parser,first_seen_at,parent_source_id,parent_path,parent_file_id) VALUES($1,$2,$3,$4,$5,$6,'jsonl_append','claude@1',now(),$7,$8,$9)`, id, device, agent, path, file, session, parent, parentPath, parentFile)
	}
	insert(root, e.deviceID, "claude", "/root", "root-file", nil, nil, nil, "target")
	insert(direct, e.deviceID, "claude", "/direct", "d", root, nil, nil, "")
	insert(exact, e.deviceID, "claude", "/exact", "e", nil, "/root", "root-file", "")
	insert(wildcard, e.deviceID, "claude", "/wildcard", "w", nil, "/root", nil, "")
	unrelated := uuid.NewString()
	insert(unrelated, e.deviceID, "claude", "/other", "o", nil, nil, nil, "")
	insert(mixed, e.deviceID, "claude", "/mixed", "m", unrelated, "/root", "root-file", "")
	insert(overlap, e.deviceID, "claude", "/overlap", "o", root, "/root", "root-file", "")
	insert(grandchild, e.deviceID, "claude", "/grandchild", "g", nil, "/mixed", nil, "")
	// Both an ID cycle and a path cycle must terminate with one row per ID.
	e.exec(`UPDATE sources SET parent_source_id=$2,parent_path='/grandchild',parent_file_id=NULL WHERE id=$1`, root, direct)
	insert(uuid.NewString(), e.deviceID, "claude", "/wrong-file", "x", nil, "/root", "wrong", "")
	insert(uuid.NewString(), other, "claude", "/other-device", "x", root, "/root", nil, "")
	insert(uuid.NewString(), e.deviceID, "codex", "/other-agent", "x", root, "/root", nil, "")
	// Exercise every seed predicate separately, including a ref with empty file ID.
	alias, ref, suffix, conversation := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	insert(alias, e.deviceID, "claude", "/alias", "a", nil, nil, nil, "")
	insert(ref, e.deviceID, "claude", "/ref", "", nil, nil, nil, "")
	insert(suffix, e.deviceID, "claude", "/copies/target.jsonl", "s", nil, nil, nil, "")
	insert(conversation, e.deviceID, "claude", "/conversation", "c", nil, nil, nil, "")
	conv := policyStoredConversation(e, e.deviceID, "target", "")
	e.exec(`UPDATE conversations SET source_id=$2 WHERE id=$1`, conv, conversation)
	args := []any{e.deviceID, "claude", "target", []string{alias}, `[{"path":"/ref","file_id":""}]`}
	want := []string{root, direct, exact, wildcard, mixed, overlap, grandchild, alias, ref, suffix, conversation}
	sort.Strings(want)
	for _, query := range []string{previousPolicyOwnedSQL(), policyOwnedSQL()} {
		if got := policyOwnedIDs(t, e, query, args...); !reflect.DeepEqual(got, want) {
			t.Fatalf("owned IDs: got %v want %v", got, want)
		}
	}
}

func TestPolicyTraversalPreservesPerGenerationHistoryProof(t *testing.T) {
	e := newEnv(t)
	req := policyRequest()
	root, child := uuid.NewString(), uuid.NewString()
	e.exec(`INSERT INTO sources(id,device_id,agent,path,file_id,session_key,storage_kind,parser,first_seen_at) VALUES($1,$2,'claude','/root','r',$3,'jsonl_append','claude@1',now())`, root, e.deviceID, req.SessionID)
	e.exec(`INSERT INTO sources(id,device_id,agent,path,file_id,session_key,storage_kind,parser,first_seen_at,parent_path) VALUES($1,$2,'claude','/child','c','different','jsonl_append','claude@1',now(),'/root')`, child, e.deviceID)
	e.exec(`INSERT INTO generations(source_id,generation,size,captured_at,complete) VALUES($1,1,1,now(),true),($1,2,1,now(),true)`, child)
	e.exec(`INSERT INTO session_policy_placements(device_id,agent,session_id,placements,current_mapping_known,evidence_scope,client_mode) VALUES($1,'claude',$2,'[]',true,'mapped','allow')`, e.deviceID, req.SessionID)
	e.exec(`INSERT INTO source_policy_placements(device_id,agent,session_id,path,file_id,generation) VALUES($1,'claude',$2,'/child','c',1)`, e.deviceID, req.SessionID)
	// Messages also carry history proof when a generation has no captured bytes.
	conv := policyStoredConversation(e, e.deviceID, req.SessionID, "")
	e.exec(`UPDATE generations SET size=0 WHERE source_id=$1 AND generation=2`, child)
	e.exec(`INSERT INTO messages(id,conversation_id,source_id,ordinal,kind,text,text_len,content_sha,source_generation,parser) VALUES($1,$2,$3,1,'text','synthetic',9,decode(repeat('00',32),'hex'),2,'claude@1')`, uuid.NewString(), conv, child)
	for _, want := range []bool{true, false} {
		for _, query := range []string{previousPolicyHistoricalSQL, policyHistoricalUnmappedSQL} {
			var got bool
			if err := e.pool.QueryRow(e.ctx, query, e.deviceID, "claude", req.SessionID, []string{}, `[]`).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("unmapped=%v want %v", got, want)
			}
		}
		e.exec(`INSERT INTO source_policy_placements(device_id,agent,session_id,path,file_id,generation) VALUES($1,'claude',$2,'/child','c',2) ON CONFLICT DO NOTHING`, e.deviceID, req.SessionID)
	}
	// A message proof cannot hide an unproven nonempty generation either.
	e.exec(`INSERT INTO generations(source_id,generation,size,captured_at,complete) VALUES($1,3,1,now(),true)`, child)
	for _, query := range []string{previousPolicyHistoricalSQL, policyHistoricalUnmappedSQL} {
		var got bool
		if err := e.pool.QueryRow(e.ctx, query, e.deviceID, "claude", req.SessionID, []string{}, `[]`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if !got {
			t.Fatal("unproven nonempty generation was overlooked")
		}
	}

}

func TestPolicyOwnedTraversalScale(t *testing.T) {
	if os.Getenv("FLOPWIRE_POLICY_PERF") != "1" {
		t.Skip("set FLOPWIRE_POLICY_PERF=1 for 40k-source plan comparison")
	}
	e := newEnv(t)
	e.exec(`INSERT INTO devices(id,user_id,name,platform,created_at) SELECT md5('policy-scale-device-'||n)::uuid,$1,'synthetic','darwin',now() FROM generate_series(1,9) n`, e.userID)
	e.exec(`INSERT INTO sources(id,device_id,agent,path,file_id,session_key,storage_kind,parser,first_seen_at,parent_path,parent_file_id)
 SELECT md5('policy-scale-'||n)::uuid,CASE WHEN n<=4000 THEN $1::uuid ELSE md5('policy-scale-device-'||((n-1)/4000))::uuid END,CASE WHEN n%10=0 THEN 'codex' ELSE 'claude' END,
 '/scale/'||n,'f'||n,CASE WHEN n=1 THEN 'target' ELSE 'unrelated-'||n END,'jsonl_append','claude@1',now(),
 CASE WHEN n BETWEEN 2 AND 8 THEN '/scale/1' ELSE '/scale/'||(10000+n%1000) END,NULL
 FROM generate_series(1,40000) n`, e.deviceID)
	e.exec(`ANALYZE sources`)
	for _, session := range []string{"target", "absent"} {
		t.Run(session, func(t *testing.T) {
			args := []any{e.deviceID, "claude", session, []string{}, `[]`}
			baseline := policyOwnedIDs(t, e, previousPolicyOwnedSQL(), args...)
			if got := policyOwnedIDs(t, e, policyOwnedSQL(), args...); !reflect.DeepEqual(got, baseline) {
				t.Fatal("scale ownership differs")
			}
			for _, tc := range []struct{ name, query string }{{"baseline", previousPolicyHistoricalSQL}, {"anchored", policyHistoricalUnmappedSQL}} {
				ctx, cancel := context.WithTimeout(e.ctx, 20*time.Second)
				rows, err := e.pool.Query(ctx, `EXPLAIN (ANALYZE,BUFFERS,FORMAT TEXT) `+tc.query, args...)
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				var plan strings.Builder
				for rows.Next() {
					var line string
					if err := rows.Scan(&line); err != nil {
						t.Fatal(err)
					}
					plan.WriteString(line + "\n")
				}
				err = rows.Err()
				rows.Close()
				cancel()
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("%s plan:\n%s", tc.name, plan.String())
			}
		})
	}
}
