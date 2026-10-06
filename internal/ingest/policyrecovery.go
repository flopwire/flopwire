package ingest

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/jackc/pgx/v5"
)

// Resolve only provenance already stored on the credential's device. JSON
// arrays keep query count and bind count independent of the incoming batch.
const verifiedRecoveryCTE = `WITH refs AS (
 SELECT value ref FROM jsonb_array_elements($4::jsonb)
), explicit_matches AS (
 SELECT DISTINCT s.id FROM refs r JOIN sources s ON
 s.device_id=$1 AND s.agent=$2 AND s.storage_kind='cass_export'
 AND s.path=r.ref->'source'->>'path' AND s.file_id=r.ref->'source'->>'file_id'
 WHERE EXISTS(SELECT 1 FROM generations g WHERE g.source_id=s.id AND g.generation=(r.ref->'source'->>'generation')::bigint)
 AND EXISTS(SELECT 1 FROM conversations c WHERE c.source_id=s.id AND c.device_id=$1 AND c.agent=$2
 AND c.extra->>'recovered_history'='true' AND c.extra->>'cass_external_id'=$3
 AND c.extra->>'cass_source_path'=r.ref->>'original_path')
), missing AS (
 SELECT EXISTS(SELECT 1 FROM refs r WHERE NOT EXISTS(
 SELECT 1 FROM sources s WHERE s.device_id=$1 AND s.agent=$2 AND s.storage_kind='cass_export'
 AND s.path=r.ref->'source'->>'path' AND s.file_id=r.ref->'source'->>'file_id'
 AND EXISTS(SELECT 1 FROM generations g WHERE g.source_id=s.id AND g.generation=(r.ref->'source'->>'generation')::bigint)
 AND EXISTS(SELECT 1 FROM conversations c WHERE c.source_id=s.id AND c.device_id=$1 AND c.agent=$2
 AND c.extra->>'recovered_history'='true' AND c.extra->>'cass_external_id'=$3
 AND c.extra->>'cass_source_path'=r.ref->>'original_path'))) invalid
), aliases AS (
 SELECT id FROM explicit_matches
 UNION SELECT s.id FROM sources s JOIN conversations c ON c.source_id=s.id
 WHERE s.device_id=$1 AND c.device_id=$1 AND s.agent=$2 AND c.agent=$2 AND s.storage_kind='cass_export'
 AND c.extra->>'recovered_history'='true' AND c.extra->>'cass_external_id'=$3
 AND EXISTS(SELECT 1 FROM jsonb_array_elements($5::jsonb) p WHERE p->>'path'=c.extra->>'cass_source_path')
) `

func recoveryQueryArgs(device string, req *syncproto.PolicyPlacementsRequest) ([]any, error) {
	refs := req.RecoverySources
	if refs == nil {
		refs = []syncproto.PolicyRecoverySource{}
	}
	sources := req.Sources
	if sources == nil {
		sources = []syncproto.PolicySource{}
	}
	rawRefs, err := json.Marshal(refs)
	if err != nil {
		return nil, err
	}
	rawSources, err := json.Marshal(sources)
	if err != nil {
		return nil, err
	}
	return []any{device, req.Agent, req.SessionID, string(rawRefs), string(rawSources)}, nil
}

func recoveryProofMissing() error {
	return &Error{http.StatusConflict, "recovery_proof_missing", "recovered source generation and same-device native/path provenance must already be stored; uploads remain held"}
}

func verifiedRecoveryAliases(ctx context.Context, q policyQuerier, device string, req *syncproto.PolicyPlacementsRequest) ([]string, error) {
	args, err := recoveryQueryArgs(device, req)
	if err != nil {
		return nil, err
	}
	var invalid bool
	var ids []string
	err = q.QueryRow(ctx, verifiedRecoveryCTE+`SELECT invalid,ARRAY(SELECT id::text FROM aliases ORDER BY id LIMIT 1025) FROM missing`, args...).Scan(&invalid, &ids)
	if err != nil {
		return nil, err
	}
	if invalid {
		return nil, recoveryProofMissing()
	}
	if len(ids) > 1024 {
		return ids, &Error{http.StatusRequestEntityTooLarge, "policy_recovery_limit", "Cowork recovered copy set exceeds supported bound; uploads remain held"}
	}
	return ids, nil
}

// Bind the complete verified SQL set, including overflow aliases, without
// materializing it in the client. Caller holds the device policy lock and has
// created the native session ledger. No partial proof batch can add links.
func bindVerifiedRecoverySources(ctx context.Context, tx pgx.Tx, device string, req *syncproto.PolicyPlacementsRequest) error {
	args, err := recoveryQueryArgs(device, req)
	if err != nil {
		return err
	}
	var invalid, conflict bool
	err = tx.QueryRow(ctx, verifiedRecoveryCTE+`, captures AS (
 SELECT s.device_id,s.path,s.file_id,COALESCE(s.session_key,'') physical_session,
 COALESCE(s.parent_path,parent.path,'') parent_path,COALESCE(s.parent_file_id,parent.file_id,'') parent_file_id,
 split_part(COALESCE(s.parser,''),'@',1) parser_family,
 CASE WHEN COALESCE(s.parent_path,parent.path,'')<>'' THEN 'companion' ELSE s.storage_kind END storage_kind,
 CASE WHEN COALESCE(s.parent_path,parent.path,'')<>'' THEN $3 ELSE COALESCE(s.session_key,'') END capture_session,
 COALESCE(parent_policy.owner_agent,physical_parent.agent,'') parent_agent,
 COALESCE(parent_policy.owner_session_id,physical_parent.session_key,'') parent_owner
 FROM aliases a JOIN sources s ON s.id=a.id
 LEFT JOIN sources parent ON parent.device_id=s.device_id AND parent.id=s.parent_source_id
 LEFT JOIN sources physical_parent ON physical_parent.device_id=s.device_id
 AND physical_parent.path=COALESCE(s.parent_path,parent.path,'') AND physical_parent.file_id=COALESCE(s.parent_file_id,parent.file_id,'')
 LEFT JOIN source_policy_identity parent_policy ON parent_policy.device_id=s.device_id
 AND parent_policy.path=COALESCE(s.parent_path,parent.path,'') AND parent_policy.file_id=COALESCE(s.parent_file_id,parent.file_id,'')
 ), conflicts AS (
 SELECT EXISTS(SELECT 1 FROM captures c
 LEFT JOIN source_policy_identity p USING(device_id,path,file_id)
 LEFT JOIN source_policy_capture_identity old USING(device_id,path,file_id)
 WHERE (p.device_id IS NOT NULL AND (p.owner_agent<>$2 OR p.owner_session_id<>$3))
 OR (c.parent_path<>'' AND (c.parent_agent<>$2 OR c.parent_owner<>$3 OR (c.physical_session<>'' AND c.physical_session<>$3)))
 OR (old.device_id IS NOT NULL AND (old.capture_session_key<>c.capture_session OR old.storage_kind<>c.storage_kind
 OR old.parent_path<>c.parent_path OR old.parent_file_id<>c.parent_file_id
 OR (old.parser_family<>'' AND old.parser_family<>c.parser_family)))) conflict
 ), owners AS (
 INSERT INTO source_policy_identity(device_id,path,file_id,owner_agent,owner_session_id)
 SELECT device_id,path,file_id,$2,$3 FROM captures WHERE NOT (SELECT invalid FROM missing) AND NOT (SELECT conflict FROM conflicts)
 ON CONFLICT DO NOTHING RETURNING 1
 ), physical AS (
 INSERT INTO source_policy_capture_identity(device_id,path,file_id,capture_session_key,storage_kind,parent_path,parent_file_id,parser_family)
 SELECT device_id,path,file_id,capture_session,storage_kind,parent_path,parent_file_id,parser_family FROM captures
 WHERE NOT (SELECT invalid FROM missing) AND NOT (SELECT conflict FROM conflicts) AND (SELECT count(*) FROM owners)>=0
 ON CONFLICT(device_id,path,file_id) DO UPDATE SET parser_family=CASE WHEN source_policy_capture_identity.parser_family='' THEN EXCLUDED.parser_family ELSE source_policy_capture_identity.parser_family END
 RETURNING 1
 ), bindings AS (
 INSERT INTO source_policy_placements(device_id,agent,session_id,path,file_id,generation)
 SELECT s.device_id,s.agent,$3,s.path,s.file_id,g.generation FROM aliases a
 JOIN sources s ON s.id=a.id JOIN generations g ON g.source_id=s.id
 WHERE NOT (SELECT invalid FROM missing) AND NOT (SELECT conflict FROM conflicts) ON CONFLICT DO NOTHING RETURNING 1
 ), links AS (
 INSERT INTO session_policy_links(device_id,agent,session_id,policy_session_id)
 SELECT DISTINCT c.device_id,c.agent,c.session_id,$3 FROM aliases a JOIN conversations c ON c.source_id=a.id
 WHERE c.device_id=$1 AND c.agent=$2 AND NOT (SELECT invalid FROM missing) AND NOT (SELECT conflict FROM conflicts)
 ON CONFLICT DO NOTHING RETURNING 1
 ) SELECT invalid,conflict FROM missing CROSS JOIN conflicts`, args...).Scan(&invalid, &conflict)
	if err != nil {
		return err
	}
	if invalid {
		return recoveryProofMissing()
	}
	if conflict {
		return policyIdentityConflict()
	}
	return nil
}
