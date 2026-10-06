package ingest

import (
	"context"
	"encoding/json"
	"time"

	"github.com/flopwire/flopwire/internal/domain"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func policyLimitError(e *Error) bool {
	switch e.Code {
	case "policy_input_limit", "policy_scope_limit", "policy_component_limit", "policy_reconciliation_limit", "policy_recovery_limit":
		return true
	}
	return false
}

// SQL streams the complete verified component, without materializing all its
// conversations or placement arrays in the application on a capacity error.
const policyLimitComponent = `WITH RECURSIVE component AS (
 SELECT $3::text session_id UNION
 SELECT CASE WHEN link.session_id=k.session_id THEN link.policy_session_id ELSE link.session_id END
 FROM session_policy_links link JOIN component k ON link.session_id=k.session_id OR link.policy_session_id=k.session_id
 WHERE link.device_id=$1 AND link.agent=$2)`

func holdSessionPolicyLimit(ctx context.Context, tx pgx.Tx, r serverRules, user, device string, req *syncproto.PolicyPlacementsRequest) error {
	if _, err := tx.Exec(ctx, `UPDATE session_policy_placements SET scope_status='limit-held',revision=revision+CASE WHEN scope_status='limit-held' THEN 0 ELSE 1 END,updated_at=now() WHERE device_id=$1 AND agent=$2 AND session_id=$3`, device, req.Agent, req.SessionID); err != nil {
		return err
	}
	refs := req.Sources
	if refs == nil {
		refs = []syncproto.PolicySource{}
	}
	rawRefs, err := json.Marshal(refs)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, policyLimitComponent+`, covered_sources AS (
 SELECT s.id FROM sources s WHERE s.device_id=$1 AND s.agent=$2 AND (
 s.session_key IN(SELECT session_id FROM component)
 OR EXISTS(SELECT 1 FROM jsonb_array_elements($6::jsonb) ref WHERE s.path=ref->>'path' AND s.file_id=ref->>'file_id')
 OR EXISTS(SELECT 1 FROM component k WHERE right(s.path,length(k.session_id)+6)=k.session_id||'.jsonl')
 OR EXISTS(SELECT 1 FROM source_policy_identity own WHERE own.device_id=$1 AND own.path=s.path AND own.file_id=s.file_id AND own.owner_agent=$2 AND own.owner_session_id IN(SELECT session_id FROM component))
 OR EXISTS(SELECT 1 FROM source_policy_placements b WHERE b.device_id=$1 AND b.agent=$2 AND b.path=s.path AND b.file_id=s.file_id AND b.session_id IN(SELECT session_id FROM component)))),
 tree AS (
 SELECT id,session_id FROM conversations WHERE device_id=$1 AND agent=$2 AND (session_id IN(SELECT session_id FROM component) OR parent_native_session_id IN(SELECT session_id FROM component) OR source_id IN(SELECT id FROM covered_sources))
 UNION SELECT c.id,c.session_id FROM conversations c JOIN tree t ON c.parent_conversation_id=t.id OR c.parent_native_session_id=t.session_id WHERE c.device_id=$1 AND c.agent=$2),
 locked AS MATERIALIZED(SELECT c.id FROM conversations c JOIN tree t ON t.id=c.id WHERE c.device_id=$1 ORDER BY c.session_id COLLATE "C",c.id FOR UPDATE OF c)
 UPDATE conversations c SET hidden_at=COALESCE(hidden_at,now()),
 hidden_rule=CASE WHEN hidden_at IS NULL THEN 'cowork-policy-limit' ELSE hidden_rule END,
 hidden_rules_version=CASE WHEN hidden_at IS NULL THEN $4 ELSE hidden_rules_version END,
 hidden_root=CASE WHEN hidden_at IS NULL THEN c.id ELSE hidden_root END,
 hidden_by=CASE WHEN hidden_at IS NULL THEN $5::uuid ELSE hidden_by END,
 hidden_scope=CASE WHEN hidden_at IS NULL THEN 'device' ELSE hidden_scope END
 FROM locked l WHERE c.id=l.id AND c.device_id=$1`, device, req.Agent, req.SessionID, r.version, user, rawRefs)
	if err != nil {
		return err
	}
	return store.InsertAudit(ctx, tx, domain.AuditEvent{ID: uuid.NewString(), ActorID: user, DeviceID: device, Action: "session.policy_limit_held", TargetType: "session", TargetID: req.SessionID, Metadata: map[string]any{"agent": req.Agent, "conversations": tag.RowsAffected(), "scope_status": syncproto.ScopeLimitHeld, "remediation": "controlled scope review required"}, CreatedAt: time.Now().UTC()})
}

func policyLimitEvidence(ctx context.Context, q policyQuerier, device, agent, session string) (string, error) {
	var scope string
	err := q.QueryRow(ctx, policyLimitComponent+` SELECT CASE WHEN bool_or(evidence_scope='unmapped') THEN 'unmapped' WHEN bool_or(evidence_scope='mapped') THEN 'mapped' ELSE 'none' END FROM session_policy_placements WHERE device_id=$1 AND agent=$2 AND session_id IN(SELECT session_id FROM component)`, device, agent, session).Scan(&scope)
	return scope, err
}
