package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/domain"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const maxPolicyPlacements = 256

func policyDeviceLockKey(device string) string { return "cowork-policy-device:" + device }

func validatePolicyDeviceDirs(d *syncproto.DeviceDirs) error {
	if d == nil {
		return nil
	}
	for _, v := range []string{d.Home, d.ClaudeProjects, d.CodexHome} {
		if len(v) > maxDirLen || strings.ContainsAny(v, "\x00\r\n\t") || v != "" && !absPath(v) {
			return badRequest("device directories must be bounded absolute host paths")
		}
	}
	if strings.HasPrefix(d.Home, "/sessions/") {
		return badRequest("device home must be a host path")
	}
	return nil
}

// Called under the exclusive device policy gate, before natural session locks.
// Home changes would reinterpret all existing ~ rules; a new identity or a
// separately controlled migration is required instead of silently doing so.
func recordPolicyDeviceDirs(ctx context.Context, tx pgx.Tx, device string, d *syncproto.DeviceDirs) error {
	if d == nil {
		return nil
	}
	var home string
	var ledger bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE(home,''),EXISTS(SELECT 1 FROM session_policy_placements WHERE device_id=$1) FROM devices WHERE id=$1`, device).Scan(&home, &ledger); err != nil {
		return err
	}
	if home != "" && home != d.Home {
		return &Error{http.StatusConflict, "device_home_conflict", "Cowork policy binds this device to its recorded home; enroll a new device identity or use a controlled home migration"}
	}
	// An omitted home never clears the initial identity during registration.
	if d.Home == "" && !ledger {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE devices SET home=NULLIF($2,''),claude_projects=NULLIF($3,''),codex_home=NULLIF($4,'') WHERE id=$1`, device, d.Home, d.ClaudeProjects, d.CodexHome)
	return err
}

var policyChildID = regexp.MustCompile(`^agent-[a-zA-Z0-9_-]{1,128}$`)

type policyPlacementState struct {
	SessionID           string
	Placements          []syncproto.PolicyPlacement
	CurrentMappingKnown bool
	EvidenceScope       string
	ScopeStatus         string
	ClientMode          string
	Revision            int64
}

// Metadata-only placeholders contribute actual restrictions, but their
// readiness belongs only to their own source and cannot taint first capture.
func policyForSession(p policyPlacementState, session string) policyPlacementState {
	if validPolicySession(session) && p.SessionID != "" && p.SessionID != session && p.EvidenceScope == syncproto.EvidenceNone {
		p.CurrentMappingKnown = true
	}
	return p
}

func validPolicySession(id string) bool {
	u, err := uuid.Parse(id)
	return (err == nil && u.String() == id) || policyChildID.MatchString(id)
}

func validatePolicyRequest(req *syncproto.PolicyPlacementsRequest) error {
	if err := validatePolicyDeviceDirs(req.Device); err != nil {
		return err
	}
	if req.Version != syncproto.Version || req.Agent != "claude" || !validPolicySession(req.SessionID) {
		return badRequest("unsupported version, agent or native session identity")
	}
	if req.ParentSessionID != "" && (!validPolicySession(req.ParentSessionID) || req.ParentSessionID == req.SessionID) {
		return badRequest("invalid parent native identity")
	}
	if req.EvidenceScope != "none" && req.EvidenceScope != "mapped" && req.EvidenceScope != "unmapped" {
		return badRequest("invalid evidence_scope")
	}
	if req.ClientMode != "allow" && req.ClientMode != "local" && req.ClientMode != "deny" {
		return badRequest("invalid client_mode")
	}
	if req.CurrentMappingKnown && len(req.Placements) == 0 {
		return badRequest("known mapping requires host placements")
	}
	if req.ScopeStatus != "" && req.ScopeStatus != "complete" && req.ScopeStatus != syncproto.ScopeLimitHeld {
		return badRequest("invalid scope_status")
	}
	for _, p := range req.Placements {
		if !absPath(p.CWD) || strings.HasPrefix(p.CWD, "/sessions/") {
			return badRequest("placement cwd must be a host absolute path")
		}
		for _, v := range []string{p.CWD, p.WorktreeRoot, p.MainRoot, p.Remote} {
			if len(v) > syncproto.MaxRepoField || strings.ContainsAny(v, "\x00\r\n\t") {
				return badRequest("invalid placement field")
			}
		}
		for _, v := range []string{p.WorktreeRoot, p.MainRoot} {
			if v != "" && !absPath(v) {
				return badRequest("placement roots must be absolute paths")
			}
		}
		if p.Remote != "" && normalizeRemote(p.Remote) != p.Remote {
			return badRequest("remote must be normalized")
		}
	}
	for _, src := range req.Sources {
		if !absPath(src.Path) || len(src.Path) > syncproto.MaxRepoField || len(src.FileID) > 512 || strings.ContainsAny(src.Path+src.FileID, "\x00\r\n") || src.Generation < 0 {
			return badRequest("invalid source reference")
		}
	}
	for _, ref := range req.RecoverySources {
		src := ref.Source
		if !absPath(src.Path) || len(src.Path) > syncproto.MaxRepoField || len(src.FileID) > 512 || src.Generation < 0 || !absPath(ref.OriginalPath) || len(ref.OriginalPath) > syncproto.MaxRepoField || strings.ContainsAny(src.Path+src.FileID+ref.OriginalPath, "\x00\r\n\t") {
			return badRequest("invalid recovered source reference")
		}
	}
	if len(req.Placements) > maxPolicyPlacements || len(req.Sources) > maxPolicyPlacements || len(req.RecoverySources) > maxPolicyPlacements {
		return &Error{http.StatusRequestEntityTooLarge, "policy_input_limit", "policy scope exceeds supported bound; committed copies will be held pending controlled scope remediation"}
	}
	return nil
}

// mergePolicy retains every captured-history restriction. Current readiness is
// reversible; unknown captured scope is not. A grant removed from metadata
// cannot remove its paths from the union protecting older generations.
func mergePolicy(old policyPlacementState, req *syncproto.PolicyPlacementsRequest, legacy bool) (policyPlacementState, error) {
	p := old
	if p.ScopeStatus == "" {
		p.ScopeStatus = "complete"
	}
	if req.ScopeStatus == syncproto.ScopeLimitHeld {
		p.ScopeStatus = syncproto.ScopeLimitHeld
	}
	p.Placements = append([]syncproto.PolicyPlacement{}, old.Placements...)
	for _, v := range req.Placements {
		if p.ScopeStatus != syncproto.ScopeLimitHeld && !slices.Contains(p.Placements, v) {
			p.Placements = append(p.Placements, v)
		}
	}
	if len(p.Placements) > maxPolicyPlacements {
		return p, &Error{http.StatusRequestEntityTooLarge, "policy_scope_limit", "historical policy scope exceeds supported bound; uploads remain held"}
	}
	p.CurrentMappingKnown = req.CurrentMappingKnown
	p.ClientMode = req.ClientMode
	if p.ScopeStatus == syncproto.ScopeLimitHeld {
		oldMode, _ := pathpolicy.ParseMode(old.ClientMode)
		newMode, _ := pathpolicy.ParseMode(p.ClientMode)
		if oldMode > newMode {
			p.ClientMode = old.ClientMode
		}
	}
	if p.EvidenceScope == "unmapped" || req.EvidenceScope == "unmapped" || legacy {
		p.EvidenceScope = "unmapped"
	} else if p.EvidenceScope == "mapped" || req.EvidenceScope == "mapped" {
		p.EvidenceScope = "mapped"
	} else {
		p.EvidenceScope = "none"
	}
	p.Revision++
	return p, nil
}

func (r serverRules) decidePolicy(dev deviceDirs, p policyPlacementState) pathpolicy.Decision {
	d := pathpolicy.Decision{}
	pol := r.policy(dev.home)
	if dev.home == "" {
		pol.Admin = slices.DeleteFunc(pol.Admin, func(rule pathpolicy.Rule) bool {
			return rule.Pattern == "~" || strings.HasPrefix(rule.Pattern, "~/")
		})
	}
	for _, v := range p.Placements {
		sub := pol.DecideSubtree(pathpolicy.Placement{Cwd: v.CWD, Worktree: v.WorktreeRoot, Main: v.MainRoot, Remote: v.Remote})
		next := sub.Decision
		if sub.RepoScopeUnknown && next.Mode == pathpolicy.Local && next.Rule.Pattern == "" {
			next.Rule = pathpolicy.Rule{Mode: pathpolicy.Local, Pattern: "cowork-repository-scope-unknown"}
		}
		d = stricterPolicyDecision(d, next)
	}
	if dev.home == "" {
		for _, rule := range r.admin {
			if rule.Mode > pathpolicy.Allow && (rule.Pattern == "~" || strings.HasPrefix(rule.Pattern, "~/")) {
				d = stricterPolicyDecision(d, pathpolicy.Decision{Mode: pathpolicy.Local, Rule: pathpolicy.Rule{Mode: pathpolicy.Local, Pattern: "cowork-home-unknown"}})
			}
		}
	}
	if p.EvidenceScope == syncproto.EvidenceUnmapped && r.unplaceable == pathpolicy.Deny && d.Mode < pathpolicy.Deny {
		d = pol.Decide(pathpolicy.Placement{})
	}
	mode, _ := pathpolicy.ParseMode(p.ClientMode)
	if mode > d.Mode || mode == d.Mode && isPolicyHold(d) {
		d = pathpolicy.Decision{Mode: mode, Rule: pathpolicy.Rule{Mode: mode, Pattern: "cowork-client-policy"}}
	}
	if d.Mode < pathpolicy.Local && p.ScopeStatus == "limit-held" {
		d = pathpolicy.Decision{Mode: pathpolicy.Local, Rule: pathpolicy.Rule{Mode: pathpolicy.Local, Pattern: "cowork-policy-limit"}}
	}
	if d.Mode < pathpolicy.Local && (!p.CurrentMappingKnown || p.EvidenceScope == "unmapped") {
		reason := "cowork-mapping-pending"
		if p.EvidenceScope == "unmapped" {
			reason = "cowork-historical-unmapped"
		}
		d = pathpolicy.Decision{Mode: pathpolicy.Local, Rule: pathpolicy.Rule{Mode: pathpolicy.Local, Pattern: reason}}
	}
	return d
}

func isPolicyHold(d pathpolicy.Decision) bool {
	return d.Mode == pathpolicy.Local && (d.Rule.Pattern == "cowork-mapping-pending" || d.Rule.Pattern == "cowork-historical-unmapped" || d.Rule.Pattern == "cowork-repository-scope-unknown" || d.Rule.Pattern == "cowork-home-unknown" || d.Rule.Pattern == "cowork-policy-limit")
}

func stricterPolicyDecision(a, b pathpolicy.Decision) pathpolicy.Decision {
	if b.Mode > a.Mode || b.Mode == a.Mode && (isPolicyHold(a) && !isPolicyHold(b) || a.Unplaceable && !b.Unplaceable || b.Admin && !a.Admin) {
		return b
	}
	return a
}

// Verified host locations replace the native VM's missing-placement floor.
// Actual rules matched by native fields remain part of the strictest decision.
func decideWithPolicies(r serverRules, dev deviceDirs, native pathpolicy.Decision, policies []policyPlacementState) pathpolicy.Decision {
	d := native
	unmapped := slices.ContainsFunc(policies, func(p policyPlacementState) bool { return p.EvidenceScope == syncproto.EvidenceUnmapped })
	for _, p := range policies {
		if d.Unplaceable && p.CurrentMappingKnown && len(p.Placements) > 0 && (d.Mode != pathpolicy.Deny || !unmapped) {
			d = pathpolicy.Decision{}
		}
	}
	for _, p := range policies {
		d = stricterPolicyDecision(d, r.decidePolicy(dev, p))
	}
	return d
}

func policyHoldOnly(r serverRules, dev deviceDirs, p policyPlacementState) bool {
	return isPolicyHold(r.decidePolicy(dev, p))
}

type policyQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// loadSessionPolicies includes the native parent and every policy binding of
// the source/its companion ancestors. Device identity is always credential-bound.
func loadSessionPolicies(ctx context.Context, q policyQuerier, device, agent, session, parent, sourceID string) ([]policyPlacementState, error) {
	rows, err := q.Query(ctx, `WITH RECURSIVE ancestry AS (
 SELECT id,path,file_id,session_key,parent_source_id FROM sources WHERE id=NULLIF($5,'')::uuid AND device_id=$1
 UNION SELECT s.id,s.path,s.file_id,s.session_key,s.parent_source_id FROM sources s JOIN ancestry a ON s.id=a.parent_source_id WHERE s.device_id=$1),
 conversation_ancestors AS (
 SELECT id,session_id,parent_native_session_id,parent_conversation_id FROM conversations WHERE device_id=$1 AND agent=$2 AND (session_id=$3 OR session_id=NULLIF($4,''))
 UNION SELECT c.id,c.session_id,c.parent_native_session_id,c.parent_conversation_id FROM conversations c JOIN conversation_ancestors a ON c.id=a.parent_conversation_id OR c.session_id=a.parent_native_session_id WHERE c.device_id=$1 AND c.agent=$2),
 keys AS (SELECT $3::text session_id UNION SELECT NULLIF($4,'') UNION SELECT session_id FROM conversation_ancestors UNION SELECT parent_native_session_id FROM conversation_ancestors UNION SELECT session_key FROM ancestry
 UNION SELECT b.session_id FROM source_policy_placements b JOIN ancestry a ON b.path=a.path AND b.file_id=a.file_id WHERE b.device_id=$1 AND b.agent=$2),
 policy_keys AS(SELECT session_id FROM keys UNION SELECT CASE WHEN link.session_id=k.session_id THEN link.policy_session_id ELSE link.session_id END FROM session_policy_links link JOIN policy_keys k ON link.session_id=k.session_id OR link.policy_session_id=k.session_id WHERE link.device_id=$1 AND link.agent=$2)
 SELECT p.session_id,p.placements,p.current_mapping_known,p.evidence_scope,p.scope_status,p.client_mode,p.revision FROM session_policy_placements p
 WHERE p.device_id=$1 AND p.agent=$2 AND p.session_id IN (SELECT session_id FROM policy_keys) LIMIT 1025`, device, agent, session, parent, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []policyPlacementState
	var scopeBytes int
	for rows.Next() {
		var p policyPlacementState
		var raw []byte
		if err := rows.Scan(&p.SessionID, &raw, &p.CurrentMappingKnown, &p.EvidenceScope, &p.ScopeStatus, &p.ClientMode, &p.Revision); err != nil {
			return nil, err
		}
		scopeBytes += len(raw)
		if scopeBytes > 4<<20 {
			return nil, &Error{http.StatusRequestEntityTooLarge, "policy_component_limit", "Cowork component exceeds supported memory bound; uploads remain held"}
		}
		if err := json.Unmarshal(raw, &p.Placements); err != nil {
			return nil, err
		}
		out = append(out, policyForSession(p, session))
		if len(out) > 1024 {
			return nil, &Error{http.StatusRequestEntityTooLarge, "policy_component_limit", "Cowork policy component exceeds supported bound; uploads remain held"}
		}
	}
	return out, rows.Err()
}

// PolicyPlacements accepts metadata even when content is forbidden. The ack
// follows the durable union and reconciliation of this device's stored copies.
func (s *Server) PolicyPlacements(ctx context.Context, device string, req *syncproto.PolicyPlacementsRequest) (*syncproto.PolicyPlacementsResponse, error) {
	var withheld *Error
	if err := validatePolicyRequest(req); err != nil {
		if !errors.As(err, &withheld) || withheld.Code != "policy_input_limit" {
			return nil, err
		}
	}
	var out syncproto.PolicyPlacementsResponse
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var user, home string
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, policyDeviceLockKey(device)); err != nil {
			return err
		}
		if err := recordPolicyDeviceDirs(ctx, tx, device, req.Device); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT user_id::text,COALESCE(home,'') FROM devices WHERE id=$1`, device).Scan(&user, &home); err != nil {
			return err
		}
		var old policyPlacementState
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT placements,current_mapping_known,evidence_scope,scope_status,client_mode,revision FROM session_policy_placements WHERE device_id=$1 AND agent=$2 AND session_id=$3 FOR UPDATE`, device, req.Agent, req.SessionID).Scan(&raw, &old.CurrentMappingKnown, &old.EvidenceScope, &old.ScopeStatus, &old.ClientMode, &old.Revision)
		fresh := errors.Is(err, pgx.ErrNoRows)
		if err != nil && !fresh {
			return err
		}
		if !fresh {
			if err := json.Unmarshal(raw, &old.Placements); err != nil {
				return err
			}
		}
		aliases, err := verifiedRecoveryAliases(ctx, tx, device, req)
		if err != nil {
			var limit *Error
			if !errors.As(err, &limit) || limit.Code != "policy_recovery_limit" {
				return err
			}
			withheld = limit
		}
		var legacy bool
		// Proof is per source generation, not inferred from another mapped
		// copy or from today's grants. Late recovery of old bytes can taint an
		// already mapped native session.
		proofRefs, err := json.Marshal(req.Sources)
		if err != nil {
			return err
		}
		if req.Sources == nil {
			proofRefs = []byte("[]")
		}
		if err := tx.QueryRow(ctx, `WITH RECURSIVE owned AS (
 SELECT s.id FROM sources s WHERE s.device_id=$1 AND s.agent=$2 AND (
 s.session_key=$3 OR right(s.path,length($3)+6)=$3||'.jsonl' OR s.id=ANY($4::uuid[])
 OR EXISTS(SELECT 1 FROM conversations c WHERE c.source_id=s.id AND c.device_id=$1 AND c.agent=$2 AND c.session_id=$3)
 OR EXISTS(SELECT 1 FROM jsonb_array_elements($5::jsonb) ref WHERE s.path=ref->>'path' AND s.file_id=ref->>'file_id'))
 UNION SELECT child.id FROM sources child JOIN owned parent_id ON true JOIN sources parent ON parent.id=parent_id.id
 WHERE child.device_id=$1 AND child.agent=$2 AND (child.parent_source_id=parent.id OR (child.parent_path=parent.path AND (child.parent_file_id IS NULL OR child.parent_file_id=parent.file_id))))
 SELECT EXISTS(SELECT 1 FROM sources s WHERE s.id IN(SELECT id FROM owned) AND (EXISTS(SELECT 1 FROM generations g WHERE g.source_id=s.id AND g.size>0 AND NOT EXISTS(SELECT 1 FROM source_policy_placements b JOIN session_policy_placements p ON p.device_id=b.device_id AND p.agent=b.agent AND p.session_id=b.session_id WHERE b.device_id=s.device_id AND b.path=s.path AND b.file_id=s.file_id AND b.generation=g.generation AND p.evidence_scope='mapped'))
		 OR EXISTS(SELECT 1 FROM messages m WHERE m.source_id=s.id AND NOT EXISTS(SELECT 1 FROM source_policy_placements b JOIN session_policy_placements p ON p.device_id=b.device_id AND p.agent=b.agent AND p.session_id=b.session_id WHERE b.device_id=s.device_id AND b.path=s.path AND b.file_id=s.file_id AND b.generation=m.source_generation AND p.evidence_scope='mapped'))))`, device, req.Agent, req.SessionID, aliases, proofRefs).Scan(&legacy); err != nil {
			return err
		}

		p, err := mergePolicy(old, req, legacy || len(req.RecoverySources) > 0)
		if err != nil {
			var limit *Error
			if !errors.As(err, &limit) || limit.Code != "policy_scope_limit" {
				return err
			}
			withheld = limit
			bounded := *req
			bounded.Placements = nil
			p, err = mergePolicy(old, &bounded, legacy || len(req.RecoverySources) > 0)
			if err != nil {
				return err
			}
		}
		if withheld != nil {
			p.ScopeStatus = syncproto.ScopeLimitHeld
		}
		raw, err = json.Marshal(p.Placements)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO session_policy_placements(device_id,agent,session_id,placements,current_mapping_known,evidence_scope,scope_status,client_mode,revision)
   VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(device_id,agent,session_id) DO UPDATE SET placements=excluded.placements,current_mapping_known=excluded.current_mapping_known,evidence_scope=excluded.evidence_scope,scope_status=excluded.scope_status,client_mode=excluded.client_mode,revision=excluded.revision,updated_at=now()`, device, req.Agent, req.SessionID, raw, p.CurrentMappingKnown, p.EvidenceScope, p.ScopeStatus, p.ClientMode, p.Revision); err != nil {
			return err
		}
		if req.ParentSessionID != "" {
			if _, err := tx.Exec(ctx, `INSERT INTO session_policy_placements(device_id,agent,session_id,placements,current_mapping_known,evidence_scope,client_mode) VALUES($1,$2,$3,'[]',false,'none','allow') ON CONFLICT DO NOTHING`, device, req.Agent, req.ParentSessionID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO session_policy_links(device_id,agent,session_id,policy_session_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, device, req.Agent, req.SessionID, req.ParentSessionID); err != nil {
				return err
			}
		}
		if err := bindVerifiedRecoverySources(ctx, tx, device, req); err != nil {
			return err
		}

		if withheld == nil {
			for _, ref := range req.Sources {
				if err := bindPolicySourceOwner(ctx, tx, device, req.Agent, req.SessionID, ref, false); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `INSERT INTO source_policy_placements(device_id,agent,session_id,path,file_id,generation) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, device, req.Agent, req.SessionID, ref.Path, ref.FileID, ref.Generation); err != nil {
					return err
				}
			}
		}
		// Keep the rule version stable through row waits and restoration. An
		// admin update cannot acknowledge a new Deny before this ack commits.
		if _, err := tx.Exec(ctx, `SELECT singleton FROM collection_policy WHERE singleton FOR SHARE`); err != nil {
			return err
		}
		rules, err := loadRules(ctx, tx)
		if err != nil {
			return err
		}
		if withheld != nil || p.ScopeStatus == syncproto.ScopeLimitHeld {
			// Capacity loses enumeration, never a verified incoming restriction.
			// Retain the actual Deny as a restriction-only floor; it provides no
			// independent permission to purge when its path proof was omitted.
			incoming := policyPlacementState{Placements: req.Placements, CurrentMappingKnown: true, EvidenceScope: syncproto.EvidenceNone, ClientMode: "allow"}
			if rules.decidePolicy(deviceDirs{home: home}, incoming).Mode == pathpolicy.Deny && p.ClientMode != "deny" {
				p.ClientMode = "deny"
				if _, err := tx.Exec(ctx, `UPDATE session_policy_placements SET client_mode='deny' WHERE device_id=$1 AND agent=$2 AND session_id=$3`, device, req.Agent, req.SessionID); err != nil {
					return err
				}
			}
			if err := holdSessionPolicyLimit(ctx, tx, rules, user, device, req); err != nil {
				return err
			}
			if withheld != nil {
				return nil
			}
			scope, err := policyLimitEvidence(ctx, tx, device, req.Agent, req.SessionID)
			if err != nil {
				return err
			}
			digest, err := syncproto.PolicyPlacementsDigest(req)
			if err != nil {
				return err
			}
			out = syncproto.PolicyPlacementsResponse{Version: syncproto.Version, Revision: p.Revision, EvidenceScope: scope, Allowed: false, RequestDigest: digest}
			return nil
		} else if err := reconcileSessionPolicy(ctx, tx, rules, user, device, req.Agent, req.SessionID); err != nil {
			var limit *Error
			if !errors.As(err, &limit) || !policyLimitError(limit) {
				return err
			}
			withheld = limit
			return holdSessionPolicyLimit(ctx, tx, rules, user, device, req)
		}
		d := rules.decidePolicy(deviceDirs{home: home}, p)
		states, err := loadSessionPolicies(ctx, tx, device, req.Agent, req.SessionID, req.ParentSessionID, "")
		if err != nil {
			var limit *Error
			if !errors.As(err, &limit) || !policyLimitError(limit) {
				return err
			}
			withheld = limit
			return holdSessionPolicyLimit(ctx, tx, rules, user, device, req)
		}
		for _, state := range states {
			if state.EvidenceScope == "unmapped" {
				p.EvidenceScope = "unmapped"
			}
			d = stricterPolicyDecision(d, rules.decidePolicy(deviceDirs{home: home}, state))
		}
		digest, err := syncproto.PolicyPlacementsDigest(req)
		if err != nil {
			return err
		}
		out = syncproto.PolicyPlacementsResponse{Version: syncproto.Version, Revision: p.Revision, EvidenceScope: p.EvidenceScope, Allowed: d.Mode == pathpolicy.Allow, RequestDigest: digest}
		return store.InsertAudit(ctx, tx, domain.AuditEvent{ID: uuid.NewString(), ActorID: user, DeviceID: device, Action: "session.policy_placements", TargetType: "session", TargetID: req.SessionID, Metadata: map[string]any{"agent": req.Agent, "revision": p.Revision, "evidence_scope": p.EvidenceScope, "placements": len(p.Placements), "allowed": out.Allowed}, CreatedAt: time.Now().UTC()})
	})
	if err != nil {
		return nil, err
	}
	if withheld != nil {
		return nil, withheld
	}
	return &out, nil
}

// Reconcile under the exclusive device gate, which excludes same-device sinks
// and manifest commits before they acquire any natural or row lock. User-wide
// natural keys would unnecessarily conflict with another device's parent tree.
// Each row becomes
// its own device-scoped hide root so restoring readiness cannot restore a
// child whose own historical scope or rules still forbid sharing.
func reconcileSessionPolicy(ctx context.Context, tx pgx.Tx, r serverRules, user, device, agent, session string) error {
	rows, err := tx.Query(ctx, `WITH RECURSIVE affected AS (
 SELECT $3::text session_id UNION SELECT CASE WHEN link.session_id=p.session_id THEN link.policy_session_id ELSE link.session_id END FROM session_policy_links link JOIN affected p ON link.policy_session_id=p.session_id OR link.session_id=p.session_id WHERE link.device_id=$1 AND link.agent=$2),
 tree AS (
 SELECT id,session_id,parent_native_session_id FROM conversations WHERE device_id=$1 AND agent=$2 AND (session_id IN(SELECT session_id FROM affected) OR source_id IN (SELECT s.id FROM sources s JOIN source_policy_placements b ON s.device_id=b.device_id AND s.path=b.path AND s.file_id=b.file_id WHERE b.device_id=$1 AND b.agent=$2 AND b.session_id IN(SELECT session_id FROM affected)))
 UNION SELECT c.id,c.session_id,c.parent_native_session_id FROM conversations c JOIN tree t ON c.parent_conversation_id=t.id OR c.parent_native_session_id=t.session_id WHERE c.device_id=$1 AND c.agent=$2)
 SELECT c.id::text,c.session_id,COALESCE(c.parent_native_session_id,''),COALESCE(c.cwd,''),c.other_cwds,COALESCE(c.extra->'git'->>'repository_url',''),COALESCE(c.source_id::text,''),COALESCE(s.path,''),COALESCE(d.home,''),COALESCE(d.claude_projects,'') FROM tree t JOIN conversations c ON c.id=t.id JOIN devices d ON d.id=c.device_id LEFT JOIN sources s ON s.id=c.source_id ORDER BY c.depth,c.session_id LIMIT 4097`, device, agent, session)
	if err != nil {
		return err
	}
	type conv struct {
		id, session, parent, cwd, remote, source, path string
		others                                         []string
		dev                                            deviceDirs
	}
	var list []conv
	for rows.Next() {
		var c conv
		if err := rows.Scan(&c.id, &c.session, &c.parent, &c.cwd, &c.others, &c.remote, &c.source, &c.path, &c.dev.home, &c.dev.claudeProjects); err != nil {
			rows.Close()
			return err
		}
		list = append(list, c)
		if len(list) > 4096 {
			rows.Close()
			return &Error{http.StatusRequestEntityTooLarge, "policy_reconciliation_limit", "Cowork policy reconciliation exceeds supported bound; uploads remain held"}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, c := range list {
		policies, err := loadSessionPolicies(ctx, tx, device, agent, c.session, c.parent, c.source)
		if err != nil {
			return err
		}
		d := decideWithPolicies(r, c.dev, r.decideAll(c.dev, agent, c.path, c.cwd, c.others, c.remote), policies)
		if d.Mode != pathpolicy.Allow {
			if _, err := tx.Exec(ctx, `UPDATE conversations SET hidden_at=COALESCE(hidden_at,now()),hidden_rule=CASE WHEN hidden_at IS NULL THEN $2 ELSE hidden_rule END,hidden_rules_version=CASE WHEN hidden_at IS NULL THEN $3 ELSE hidden_rules_version END,hidden_root=CASE WHEN hidden_at IS NULL THEN id ELSE hidden_root END,hidden_by=CASE WHEN hidden_at IS NULL THEN $4::uuid ELSE hidden_by END,hidden_scope=CASE WHEN hidden_at IS NULL THEN 'device' ELSE hidden_scope END WHERE id=$1 AND device_id=$5`, c.id, ruleName(d), r.version, user, device); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(ctx, `UPDATE conversations SET hidden_at=NULL,hidden_rule=NULL,hidden_rules_version=NULL,hidden_root=NULL,hidden_by=NULL,hidden_scope='user' WHERE id=$1 AND device_id=$2 AND hidden_scope='device' AND hidden_root=id`, c.id, device); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkFlushPolicy runs before receiving content and again in the manifest
// transaction under the shared device gate. Registration takes the exclusive
// gate, so an acknowledged restriction cannot race the committed manifest.
func checkFlushPolicy(ctx context.Context, q policyQuerier, device string, src syncproto.Source) ([]string, error) {
	// Upgrade fixtures seed historical archives through the current uploader
	// before migration 014. Such schemas cannot contain Cowork policy records.
	var supported bool
	if err := q.QueryRow(ctx, `SELECT to_regclass('session_policy_placements') IS NOT NULL`).Scan(&supported); err != nil {
		return nil, err
	}
	if !supported {
		if strings.Contains(strings.ReplaceAll(src.Path, `\`, "/"), "/local-agent-mode-sessions/") {
			return nil, &Error{http.StatusConflict, "policy_placements_required", "Cowork host policy is unavailable on this schema; uploads remain held"}
		}
		return nil, nil
	}
	if err := checkPolicySourceIdentity(ctx, q, device, src); err != nil {
		return nil, err
	}
	if src.Agent != "claude" {
		return nil, nil
	}
	native, err := policySourceSession(ctx, q, device, src)
	if err != nil {
		return nil, err
	}
	parentPath, parentID := "", ""
	if src.Parent != nil {
		parentPath, parentID = src.Parent.Path, src.Parent.FileID
	}
	rows, err := q.Query(ctx, `WITH RECURSIVE ancestry AS (
 SELECT id,path,file_id,session_key,parent_source_id FROM sources WHERE device_id=$1 AND ((path=$3 AND file_id=$4) OR (path=$5 AND ($6='' OR file_id=$6)))
 UNION SELECT s.id,s.path,s.file_id,s.session_key,s.parent_source_id FROM sources s JOIN ancestry a ON s.id=a.parent_source_id WHERE s.device_id=$1),
 keys AS(SELECT $7::text session_id UNION SELECT session_key FROM ancestry
 UNION SELECT session_id FROM source_policy_placements WHERE device_id=$1 AND agent=$2 AND ((path=$3 AND file_id=$4) OR (path=$5 AND ($6='' OR file_id=$6)))),
 policy_keys AS(SELECT session_id FROM keys UNION SELECT CASE WHEN link.session_id=k.session_id THEN link.policy_session_id ELSE link.session_id END FROM session_policy_links link JOIN policy_keys k ON link.session_id=k.session_id OR link.policy_session_id=k.session_id WHERE link.device_id=$1 AND link.agent=$2)
 SELECT p.session_id FROM session_policy_placements p WHERE p.device_id=$1 AND p.agent=$2 AND p.session_id IN(SELECT session_id FROM policy_keys) ORDER BY p.session_id COLLATE "C"`, device, src.Agent, src.Path, src.FileID, parentPath, parentID, native)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// Identified container evidence must never fall back to native VM cwd rules.
	if strings.Contains(strings.ReplaceAll(src.Path, `\`, "/"), "/local-agent-mode-sessions/") && len(ids) == 0 {
		return nil, &Error{http.StatusConflict, "policy_placements_required", "Cowork host policy must be acknowledged before content"}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	rules, err := loadRules(ctx, q)
	if err != nil {
		return nil, err
	}
	var home string
	if err := q.QueryRow(ctx, `SELECT COALESCE(home,'') FROM devices WHERE id=$1`, device).Scan(&home); err != nil {
		return nil, err
	}
	for _, id := range ids {
		states, err := loadSessionPolicies(ctx, q, device, src.Agent, id, "", "")
		if err != nil {
			return nil, err
		}
		for _, p := range states {
			if d := rules.decidePolicy(deviceDirs{home: home}, policyForSession(p, native)); d.Mode != pathpolicy.Allow {
				return nil, &Error{http.StatusConflict, "policy_placements_held", "Cowork content is held by host policy or unresolved historical scope"}
			}
		}
	}
	return ids, nil
}

// A companion's owning identity is its actual parent, never an arbitrary
// member of the connected policy component.
func policySourceSession(ctx context.Context, q policyQuerier, device string, src syncproto.Source) (string, error) {
	if src.SessionKey != "" {
		return src.SessionKey, nil
	}
	path := src.Path
	if src.StorageKind == "companion" && src.Parent != nil {
		var session string
		err := q.QueryRow(ctx, `SELECT COALESCE(session_key,'') FROM sources WHERE device_id=$1 AND path=$2 AND ($3='' OR file_id=$3) ORDER BY first_seen_at DESC LIMIT 1`, device, src.Parent.Path, src.Parent.FileID).Scan(&session)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
		if session != "" {
			return session, nil
		}
		path = src.Parent.Path
	}
	if strings.HasSuffix(path, ".jsonl") {
		parts := strings.Split(strings.ReplaceAll(path, `\`, "/"), "/")
		session := strings.TrimSuffix(parts[len(parts)-1], ".jsonl")
		if validPolicySession(session) {
			return session, nil
		}
	}
	return "", nil
}

// checkSessionPolicy is called after the sink holds the session's natural-key
// lock, so an acknowledged restriction cannot be bypassed by a stale parse.
func (s *sink) checkSessionPolicy(tx pgx.Tx, session, parent string) error {
	if s.src.agent != "claude" || !s.devicePolicy {
		return nil
	}
	policies, err := loadSessionPolicies(s.ctx, tx, s.src.deviceID, s.src.agent, session, parent, s.src.id)
	if err != nil {
		return err
	}
	if len(policies) == 0 {
		return nil
	}
	rules, err := loadRules(s.ctx, tx)
	if err != nil {
		return err
	}
	if s.gate == nil {
		var dev deviceDirs
		var path string
		var hadStored bool
		if err := tx.QueryRow(s.ctx, `SELECT COALESCE(d.home,''),COALESCE(d.claude_projects,''),COALESCE(src.path,''),EXISTS(SELECT 1 FROM conversations c WHERE c.source_id=src.id) FROM devices d LEFT JOIN sources src ON src.id=NULLIF($2,'')::uuid WHERE d.id=$1`, s.src.deviceID, s.src.id).Scan(&dev.home, &dev.claudeProjects, &path, &hadStored); err != nil {
			return err
		}
		s.gate = newGate(rules, s.src, path, dev, s.pool, hadStored)
	}
	d := pathpolicy.Decision{}
	holdOnly := true
	for _, p := range policies {
		next := rules.decidePolicy(s.gate.dev, p)
		d = stricterPolicyDecision(d, next)
		if next.Mode != pathpolicy.Allow && !policyHoldOnly(rules, s.gate.dev, p) {
			holdOnly = false
		}
	}
	if d.Mode == pathpolicy.Allow {
		return nil
	}
	var stored bool
	if err := tx.QueryRow(s.ctx, `SELECT EXISTS(SELECT 1 FROM conversations WHERE device_id=$1 AND agent=$2 AND session_id=$3)`, s.src.deviceID, s.src.agent, session).Scan(&stored); err != nil {
		return err
	}
	if !holdOnly && !stored && !s.gate.hadStored {
		return &refusal{d: d, session: session}
	}
	if s.gate.rules.updatedBy == "" {
		s.gate.rules.updatedBy = s.src.userID
	}
	s.gate.hide[session] = d
	return nil
}
