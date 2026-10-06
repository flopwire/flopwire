package localindex

import (
	"encoding/json"
	"testing"
)

const recoveryNativeID = "11111111-2222-4333-8444-555555555555"

func recoveryExec(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if err := s.write(ctx, func(w *writeTx) error { _, err := w.exec(query, args...); return err }); err != nil {
		t.Fatal(err)
	}
}
func recoveryFixture(t *testing.T) *Store {
	t.Helper()
	s := openEvidenceTest(t)
	recoveryExec(t, s, `CREATE TABLE devsync_sources(id INTEGER PRIMARY KEY,path TEXT,spec TEXT,generation INTEGER)`)
	recoveryExec(t, s, `CREATE TABLE devsync_gens(source_id INTEGER,file_id TEXT,generation INTEGER,acked INTEGER,tail_acked INTEGER,tail_size INTEGER)`)
	recoveryExec(t, s, `INSERT INTO sources(id,device_id,agent,path,file_id,session_key,storage_kind,parser,first_seen_at) VALUES(1,'local-device','claude','/exports/recovery.jsonl','local-file',?,'cass_export','cass@1',0)`, recoveryNativeID)
	extra, _ := json.Marshal(map[string]any{"recovered_history": true, "cass_external_id": recoveryNativeID, "cass_source_path": "/gone/original.jsonl"})
	recoveryExec(t, s, `INSERT INTO conversations(id,device_id,agent,session_id,source_id,extra) VALUES(1,'local-device','claude',?,1,?)`, recoveryNativeID, string(extra))
	spec, _ := json.Marshal(map[string]any{"Path": "/exports/recovery.jsonl", "Agent": "claude", "StorageKind": "cass_export", "Parser": "cass@1", "SessionKey": recoveryNativeID, "Export": false})
	recoveryExec(t, s, `INSERT INTO devsync_sources VALUES(1,'/exports/recovery.jsonl',?,2)`, string(spec))
	recoveryExec(t, s, `INSERT INTO devsync_gens VALUES(1,'server-old-file',0,1,0,0),(1,'server-tail-file',1,0,1,10),(1,'unshared-new-file',2,0,1,0)`)
	return s
}
func TestCoworkRecoveryAcknowledgedHistoricalGenerations(t *testing.T) {
	s := recoveryFixture(t)
	refs, err := s.CoworkRecoverySources(ctx, recoveryNativeID)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("references=%+v", refs)
	}
	for i, ref := range refs {
		if ref.Source.Generation != int64(i) || ref.Source.Path != "/exports/recovery.jsonl" || ref.OriginalPath != "/gone/original.jsonl" {
			t.Fatalf("reference=%+v", ref)
		}
	}
	// Export mode differs across supported import APIs. Both retain CASS
	// provenance, rather than being mistaken for native folder evidence.
	recoveryExec(t, s, `UPDATE devsync_sources SET spec=json_set(spec,'$.Export',1)`)
	refs, err = s.CoworkRecoverySources(ctx, recoveryNativeID)
	if err != nil || len(refs) != 2 {
		t.Fatalf("export references=%+v,%v", refs, err)
	}
	// Generated exports use an empty file identity; preserve the exact server
	// identity rather than substituting a current physical inode.
	recoveryExec(t, s, `UPDATE devsync_gens SET file_id=''`)
	refs, err = s.CoworkRecoverySources(ctx, recoveryNativeID)
	if err != nil || len(refs) != 2 || refs[0].Source.FileID != "" || refs[1].Source.FileID != "" {
		t.Fatalf("generated export references=%+v,%v", refs, err)
	}
}
func TestCoworkRecoveryHistoricalMessageAssociation(t *testing.T) {
	s := recoveryFixture(t)
	recoveryExec(t, s, `UPDATE conversations SET source_id=NULL`)
	recoveryExec(t, s, `INSERT INTO messages(conversation_id,source_id,native_id,ordinal,kind,text_len,full_len,content_sha,source_generation,parser,text) VALUES(1,1,'historical',0,'user',1,1,zeroblob(32),0,'cass@1',zeroblob(0))`)
	refs, err := s.CoworkRecoverySources(ctx, recoveryNativeID)
	if err != nil || len(refs) != 2 {
		t.Fatalf("historical messages references=%+v,%v", refs, err)
	}
	recoveryExec(t, s, `DELETE FROM messages`)
	refs, err = s.CoworkRecoverySources(ctx, recoveryNativeID)
	if err != nil || len(refs) != 0 {
		t.Fatalf("source UUID alone inferred association: %+v,%v", refs, err)
	}
}
func TestCoworkRecoveryRejectsUnverifiedAssociation(t *testing.T) {
	for _, query := range []string{
		`UPDATE conversations SET device_id='other-device'`,
		`UPDATE sources SET device_id='other-device'`,
		`UPDATE conversations SET agent='codex'`,
		`UPDATE sources SET storage_kind='jsonl_append'`,
		`UPDATE sources SET parser='claude@1'`,
		`UPDATE conversations SET extra=json_set(extra,'$.cass_external_id','other')`,
		`UPDATE conversations SET extra=json_set(extra,'$.recovered_history',json('false'))`,
		`UPDATE devsync_sources SET spec=json_set(spec,'$.SessionKey','other')`,
		`UPDATE devsync_sources SET spec=json_remove(spec,'$.SessionKey')`,
		`UPDATE devsync_sources SET spec=json_set(spec,'$.StorageKind','jsonl_append')`,
		`UPDATE devsync_sources SET spec=json_set(spec,'$.Parser','cass@garbage')`,
		`UPDATE devsync_sources SET spec=json_set(spec,'$.Path','/other/export.jsonl')`,
		`UPDATE devsync_gens SET acked=0,tail_acked=0`,
	} {
		t.Run(query, func(t *testing.T) {
			s := recoveryFixture(t)
			recoveryExec(t, s, query)
			refs, err := s.CoworkRecoverySources(ctx, recoveryNativeID)
			if err != nil || len(refs) != 0 {
				t.Fatalf("unverified references=%+v,%v", refs, err)
			}
		})
	}
}
func TestCoworkRecoveryFailsClosedOnCorruption(t *testing.T) {
	for _, query := range []string{
		`UPDATE conversations SET extra='{'`,
		`UPDATE conversations SET extra=json_set(extra,'$.cass_source_path','relative')`,
		`UPDATE conversations SET extra=json_set(extra,'$.recovered_history','true')`,
		`UPDATE devsync_sources SET spec='{'`,
		`ALTER TABLE devsync_gens DROP COLUMN tail_size`,
		`DROP TABLE devsync_gens`,
	} {
		t.Run(query, func(t *testing.T) {
			s := recoveryFixture(t)
			recoveryExec(t, s, query)
			refs, err := s.CoworkRecoverySources(ctx, recoveryNativeID)
			if err == nil || len(refs) != 0 {
				t.Fatalf("corrupt references=%+v,%v", refs, err)
			}
		})
	}
}
func TestCoworkRecoveryOptionalSyncStore(t *testing.T) {
	s := openEvidenceTest(t)
	refs, err := s.CoworkRecoverySources(ctx, recoveryNativeID)
	if err != nil || len(refs) != 0 {
		t.Fatalf("optional store=%+v,%v", refs, err)
	}
	for _, id := range []string{"not-a-uuid", "00000000-0000-0000-0000-000000000000", "11111111-2222-4333-8444-55555555555A"} {
		if _, err = s.CoworkRecoverySources(ctx, id); err == nil {
			t.Fatalf("invalid UUID accepted: %s", id)
		}
	}
}
