package ingest

// Hiding stored sessions a rule change covers (D18). When the admin path
// rules change, the parse queue's sweep re-checks every stored
// conversation (EnforceRules):
//
//   - one the new rules cover is hidden at once, with its subagents and
//     the same session on the user's other devices (the set a deletion
//     takes): retrieval leaves hidden conversations out, and the hide
//     records when, under which rules version and rule, and by whom;
//   - one hidden that the new rules no longer cover (its rule removed, or
//     edited so it no longer matches) is restored;
//   - a hidden session is purged through the deletion machinery when an
//     administrator confirms (PurgeHidden) or once it has been hidden for
//     HiddenPurgeAfter; a purge re-checks the current rules first and
//     restores instead when nothing covers the session any more.
//
// Every hide, restore and purge is audited. New uploads a rule covers are
// still refused at parse time and never stored (rules.go).

import (
	"context"
	"errors"
	"time"

	"github.com/flopwire/flopwire/internal/domain"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// HiddenPurgeAfter is how long a session a rule change hid stays
// restorable before the sweep purges it.
const HiddenPurgeAfter = 7 * 24 * time.Hour

// Enforcement counts what one EnforceRules pass did.
type Enforcement struct {
	Hidden, Restored, Purged int
}

// convRow is a stored conversation with what the rules decide on.
type convRow struct {
	id, agent, session, cwd, remote, path, source, user, device string
	dev                                                         deviceDirs
	hiddenAt                                                    *time.Time
	root, rule, by                                              string
	others                                                      []string // other_cwds
	scope                                                       string
	policies                                                    []policyPlacementState
	hasPolicy                                                   bool
}

// isRoot reports whether the conversation is hidden as the one its rule
// matched (its tree names it as hidden_root).
func (c convRow) isRoot() bool { return c.hiddenAt != nil && c.root == c.id }

const convRowsSQL = `SELECT c.id::text,c.agent,COALESCE(c.cwd,''),COALESCE(c.extra->'git'->>'repository_url',''),
		COALESCE(s.path,''),COALESCE(c.source_id::text,''),c.user_id::text,c.device_id::text,COALESCE(d.home,''),COALESCE(d.claude_projects,''),
		c.hidden_at,COALESCE(c.hidden_root::text,''),COALESCE(c.hidden_rule,''),COALESCE(c.hidden_by::text,''),c.other_cwds,c.session_id,c.hidden_scope,COALESCE(p.states,'[]'::jsonb)
	FROM conversations c JOIN devices d ON d.id=c.device_id LEFT JOIN sources s ON s.id=c.source_id
	LEFT JOIN LATERAL (WITH RECURSIVE ancestors AS (
		SELECT c.id,c.session_id,c.parent_native_session_id,c.parent_conversation_id
		UNION SELECT parent.id,parent.session_id,parent.parent_native_session_id,parent.parent_conversation_id
		FROM conversations parent JOIN ancestors child ON parent.id=child.parent_conversation_id
		 OR (child.parent_conversation_id IS NULL AND parent.session_id=child.parent_native_session_id)
		WHERE parent.device_id=c.device_id AND parent.user_id=c.user_id AND parent.agent=c.agent),
		source_ancestors AS (
		SELECT s.id,s.path,s.file_id,s.session_key,s.parent_source_id FROM sources s WHERE s.id=c.source_id AND s.device_id=c.device_id
		UNION SELECT parent.id,parent.path,parent.file_id,parent.session_key,parent.parent_source_id
		FROM sources parent JOIN source_ancestors child ON parent.id=child.parent_source_id WHERE parent.device_id=c.device_id),
		keys AS (SELECT session_id FROM ancestors UNION SELECT parent_native_session_id FROM ancestors
		UNION SELECT session_key FROM source_ancestors UNION SELECT binding.session_id
		FROM source_policy_placements binding JOIN source_ancestors source ON binding.path=source.path AND binding.file_id=source.file_id
		WHERE binding.device_id=c.device_id AND binding.agent=c.agent),
		policy_keys AS (SELECT session_id FROM keys UNION SELECT CASE WHEN link.session_id=k.session_id THEN link.policy_session_id ELSE link.session_id END FROM session_policy_links link JOIN policy_keys k ON link.session_id=k.session_id OR link.policy_session_id=k.session_id WHERE link.device_id=c.device_id AND link.agent=c.agent)
		, policy_stats AS (
		 SELECT count(*) AS state_count,COALESCE(sum(octet_length(p.placements::text)),0) AS placement_bytes,
		 CASE WHEN bool_or(p.evidence_scope='unmapped') THEN 'unmapped' WHEN bool_or(p.evidence_scope='mapped') THEN 'mapped' ELSE 'none' END AS evidence_scope,
		 CASE WHEN bool_or(p.client_mode='deny') THEN 'deny' WHEN bool_or(p.client_mode='local') THEN 'local' ELSE 'allow' END AS client_mode
		 FROM session_policy_placements p WHERE p.device_id=c.device_id AND p.agent=c.agent AND p.session_id IN(SELECT session_id FROM policy_keys)),
		 candidates AS (
		 SELECT p.* FROM session_policy_placements p WHERE p.device_id=c.device_id AND p.agent=c.agent AND p.session_id IN(SELECT session_id FROM policy_keys)
		 ORDER BY (p.session_id=c.session_id) DESC,p.session_id COLLATE "C" LIMIT 1025),
		 budgeted AS (
		 SELECT p.*,row_number() OVER w AS position,sum(octet_length(p.placements::text)) OVER w AS placement_bytes
		 FROM candidates p WINDOW w AS (ORDER BY (p.session_id=c.session_id) DESC,p.session_id COLLATE "C" ROWS UNBOUNDED PRECEDING))
		 SELECT COALESCE((SELECT jsonb_agg(jsonb_build_object(
		 'SessionID',p.session_id,'Placements',p.placements,'CurrentMappingKnown',p.current_mapping_known,
		 'EvidenceScope',p.evidence_scope,'ScopeStatus',p.scope_status,'ClientMode',p.client_mode) ORDER BY p.position)
		 FROM budgeted p WHERE p.position<=CASE WHEN stats.state_count>1024 OR stats.placement_bytes>4194304 THEN 1023 ELSE 1024 END
		 AND p.placement_bytes<=4194304),'[]'::jsonb)
		 || CASE WHEN stats.state_count>1024 OR stats.placement_bytes>4194304 THEN jsonb_build_array(jsonb_build_object(
		 'SessionID',c.session_id,'Placements','[]'::jsonb,'CurrentMappingKnown',true,
		 'EvidenceScope',stats.evidence_scope,'ScopeStatus','limit-held','ClientMode',stats.client_mode)) ELSE '[]'::jsonb END AS states
		 FROM policy_stats stats) p ON true`

func (q *Queue) convRows(ctx context.Context, where string, args ...any) ([]convRow, error) {
	rows, err := q.Pool.Query(ctx, convRowsSQL+` WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanConvRow)
}

func scanConvRow(row pgx.CollectableRow) (convRow, error) {
	var c convRow
	err := row.Scan(&c.id, &c.agent, &c.cwd, &c.remote, &c.path, &c.source, &c.user, &c.device, &c.dev.home, &c.dev.claudeProjects,
		&c.hiddenAt, &c.root, &c.rule, &c.by, &c.others, &c.session, &c.scope, &c.policies)
	for i := range c.policies {
		c.policies[i] = policyForSession(c.policies[i], c.session)
	}
	c.hasPolicy = len(c.policies) > 0
	return c, err
}

func (r serverRules) decideConv(c convRow) pathpolicy.Decision {
	return decideWithPolicies(r, c.dev, r.decideAll(c.dev, c.agent, c.path, c.cwd, c.others, c.remote), c.policies)
}

// Capacity projection preserves visibility restrictions without authorizing
// irreversible deletion from a compact client floor alone. Native/admin path
// proof retained inside the budget can still authorize the existing purge.
func (r serverRules) capacityPurgeHeld(c convRow) bool {
	limited := false
	for _, state := range c.policies {
		if state.ScopeStatus == "limit-held" {
			limited = true
			break
		}
	}
	if !limited {
		return false
	}
	native := r.decideAll(c.dev, c.agent, c.path, c.cwd, c.others, c.remote)
	if native.Mode != pathpolicy.Allow && !isPolicyHold(native) && native.Admin && !native.Unplaceable {
		return false
	}
	for _, state := range c.policies {
		state.ClientMode = "allow"
		state.CurrentMappingKnown = true
		state.ScopeStatus = "complete"
		d := r.decidePolicy(c.dev, state)
		if d.Mode != pathpolicy.Allow && !isPolicyHold(d) && d.Admin {
			return false
		}
	}
	return true
}

// EnforceRules applies changed admin path rules to the stored
// conversations (collection_policy.rules_version passed
// rules_swept_version): it hides what they now cover and restores what
// they no longer cover. On every call it also extends each hide to
// conversations that arrived under it since (a subagent uploaded later),
// and purges sessions hidden for HiddenPurgeAfter. The parse queue runs
// it from its sweep: a rule change on every sweep, the rest at most every
// hiddenPass.
func (q *Queue) EnforceRules(ctx context.Context) (Enforcement, error) {
	return q.enforceRules(ctx, true)
}

// hiddenPass is how often the sweep extends hides and purges expired ones.
const hiddenPass = time.Minute

func (q *Queue) enforceRules(ctx context.Context, hiddenToo bool) (Enforcement, error) {
	var e Enforcement
	r, err := loadRules(ctx, q.Pool)
	if err != nil {
		return e, err
	}
	if r.version > r.swept {
		if e.Hidden, e.Restored, err = q.applyRules(ctx, r, ""); err != nil {
			return e, err
		}
		if _, err = q.Pool.Exec(ctx, `UPDATE collection_policy SET rules_swept_version=GREATEST(rules_swept_version,$1) WHERE singleton`, r.version); err != nil {
			return e, err
		}
	}
	if !hiddenToo {
		return e, nil
	}
	if err := q.extendHidden(ctx); err != nil {
		return e, err
	}
	cutoff := time.Now().Add(-HiddenPurgeAfter)
	var restored int
	e.Purged, restored, err = q.purgeHidden(ctx, r, &cutoff, "", "", "", "hidden_expired")
	e.Restored += restored
	return e, err
}

// applyRules hides the conversations the rules cover and restores hidden
// ones they no longer cover; sourceID limits it to one source's.
func (q *Queue) applyRules(ctx context.Context, r serverRules, sourceID string) (hidden, restored int, err error) {
	list, err := q.convRows(ctx, `($1='' OR c.source_id=$1::uuid) ORDER BY c.depth,c.id`, sourceID)
	if err != nil {
		return 0, 0, err
	}
	for _, c := range list {
		d := r.decideConv(c)
		switch {
		case c.hiddenAt == nil && d.Mode != pathpolicy.Allow:
			n, err := q.hide(ctx, r, c, d)
			if err != nil {
				return hidden, restored, err
			}
			if n > 0 {
				hidden++
			}
		case c.isRoot() && d.Mode == pathpolicy.Allow:
			if err := q.restore(ctx, r, c, "rules_changed"); err != nil {
				return hidden, restored, err
			}
			restored++
		case c.isRoot() && ruleName(d) != c.rule:
			// Still covered, now by another rule: the window runs on.
			if _, err := q.Pool.Exec(ctx, lockHideRoot+`UPDATE conversations SET hidden_rule=$2,hidden_rules_version=$3 WHERE id IN (SELECT id FROM l)`,
				c.id, ruleName(d), r.version); err != nil {
				return hidden, restored, err
			}
		}
	}
	return hidden, restored, nil
}

// hideTreeSQL hides a conversation, every conversation of the same user
// and session (the user's other devices), and every subagent conversation
// below them, linked or still waiting for its parent's id: the set a
// deletion of it takes. Conversations already hidden keep their hide.
//
// The tree spans sessions, so CTE l locks it first in the order of
// store.LockConversationsSQL: the UPDATE alone would lock it in plan order
// and could deadlock with a deletion, recount or checkpoint of it.
const hideTreeSQL = `WITH RECURSIVE t AS (
		SELECT id,user_id,agent,session_id FROM conversations WHERE id=$1
		UNION
		SELECT c.id,c.user_id,c.agent,c.session_id FROM conversations c JOIN t
			ON (c.user_id=t.user_id AND c.agent=t.agent AND c.session_id=t.session_id) OR c.parent_conversation_id=t.id
			OR (c.parent_conversation_id IS NULL AND c.user_id=t.user_id AND c.agent=t.agent AND c.parent_native_session_id=t.session_id)),
	l AS MATERIALIZED (SELECT id FROM conversations WHERE id IN (SELECT id FROM t) AND hidden_at IS NULL ORDER BY session_id COLLATE "C",id FOR UPDATE)
	UPDATE conversations SET hidden_at=$2,hidden_rule=$3,hidden_rules_version=$4,hidden_root=$1,hidden_by=NULLIF($5,'')::uuid,hidden_scope='user'
	WHERE id IN (SELECT id FROM l)`

// Ledger placement restrictions belong to the credential's device. Keep every
// recursive edge on that device, including unlinked children.
const hideDeviceTreeSQL = `WITH RECURSIVE t AS (
	SELECT id,user_id,device_id,agent,session_id FROM conversations WHERE id=$1
	UNION
	SELECT c.id,c.user_id,c.device_id,c.agent,c.session_id FROM conversations c JOIN t
	ON c.device_id=t.device_id AND c.user_id=t.user_id AND (
		(c.agent=t.agent AND c.session_id=t.session_id) OR c.parent_conversation_id=t.id
		OR (c.parent_conversation_id IS NULL AND c.agent=t.agent AND c.parent_native_session_id=t.session_id))),
	l AS MATERIALIZED (SELECT id FROM conversations WHERE id IN (SELECT id FROM t) AND hidden_at IS NULL ORDER BY session_id COLLATE "C",id FOR UPDATE)
	UPDATE conversations SET hidden_at=$2,hidden_rule=$3,hidden_rules_version=$4,hidden_root=$1,hidden_by=NULLIF($5,'')::uuid,hidden_scope='device'
	WHERE id IN (SELECT id FROM l)`

// lockHideRoot (CTE l) locks the conversations hidden under root $1 in
// the order of store.LockConversationsSQL, for the UPDATE that follows.
const lockHideRoot = `WITH l AS MATERIALIZED (SELECT id FROM conversations WHERE hidden_root=$1 AND hidden_at IS NOT NULL
	ORDER BY session_id COLLATE "C",id FOR UPDATE) `

// hide hides c's tree under decision d, audited, and returns how many
// conversations it hid.
func (q *Queue) hide(ctx context.Context, r serverRules, c convRow, d pathpolicy.Decision) (int64, error) {
	var n int64
	at := time.Now().UTC().Truncate(time.Microsecond)
	rule := ruleName(d)
	err := pgx.BeginTxFunc(ctx, q.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		tree, who := hideTreeSQL, r.updatedBy
		if c.hasPolicy {
			tree = hideDeviceTreeSQL
			if who == "" {
				who = c.user
			}
		}
		tag, err := tx.Exec(ctx, tree, c.id, at, rule, r.version, who)
		if err != nil {
			return err
		}
		if n = tag.RowsAffected(); n == 0 {
			return nil // hidden meanwhile with a parent's tree
		}
		return store.InsertAudit(ctx, tx, domain.AuditEvent{ID: uuid.NewString(), ActorID: who,
			Action: "conversation.hidden", TargetType: "conversation", TargetID: c.id,
			Metadata: map[string]any{"rule": rule, "mode": d.Mode.String(), "unplaceable": d.Unplaceable, "rules_version": r.version,
				"user_id": c.user, "device_id": c.device, "source_id": c.source, "path": c.path, "conversations": n,
				"purge_after": at.Add(HiddenPurgeAfter)},
			CreatedAt: at})
	})
	if err == nil && n > 0 {
		q.Log.Info("ingest: path rules now cover a stored conversation; hidden", "conversation", c.id, "rule", rule, "conversations", n)
	}
	return n, err
}

// restore unhides the tree c's hide covers, audited.
func (q *Queue) restore(ctx context.Context, r serverRules, c convRow, reason string) error {
	who := r.updatedBy
	if who == "" && c.hasPolicy {
		who = c.user
	}
	return pgx.BeginTxFunc(ctx, q.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		// Lock the original hide before checking its members. A removed
		// user-wide admin rule cannot expose another device's ledger hold.
		if _, err := tx.Exec(ctx, lockHideRoot+`SELECT id FROM l`, c.id); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, convRowsSQL+` WHERE c.hidden_root=$1 AND c.hidden_at IS NOT NULL ORDER BY c.depth,c.id`, c.id)
		if err != nil {
			return err
		}
		members, err := pgx.CollectRows(rows, scanConvRow)
		if err != nil {
			return err
		}
		var restoreIDs []string
		for _, member := range members {
			decision := r.decideConv(member)
			if decision.Mode == pathpolicy.Allow {
				restoreIDs = append(restoreIDs, member.id)
				continue
			}
			// Preserve the original hiding time. Give every retained member
			// a live root, so extension and expiry remain enforceable even
			// after the previous root is restored.
			scope := member.scope
			if member.hasPolicy {
				scope = "device"
			}
			if _, err := tx.Exec(ctx, `UPDATE conversations SET hidden_root=id,hidden_rule=$2,hidden_rules_version=$3,hidden_scope=$4,
				hidden_by=COALESCE(hidden_by,NULLIF($5,'')::uuid) WHERE id=$1`, member.id, ruleName(decision), r.version, scope, member.user); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(ctx, `UPDATE conversations SET hidden_at=NULL,hidden_rule=NULL,hidden_rules_version=NULL,hidden_root=NULL,hidden_by=NULL,hidden_scope='user'
			WHERE id=ANY($1::uuid[])`, restoreIDs)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return store.InsertAudit(ctx, tx, domain.AuditEvent{ID: uuid.NewString(), ActorID: who,
			Action: "conversation.restored", TargetType: "conversation", TargetID: c.id,
			Metadata: map[string]any{"reason": reason, "rule": c.rule, "hidden_at": c.hiddenAt, "rules_version": r.version,
				"user_id": c.user, "device_id": c.device, "source_id": c.source, "conversations": tag.RowsAffected()},
			CreatedAt: time.Now().UTC()})
	})
}

// extendHidden hides, under each hide, the conversations that joined its
// tree since (a subagent or another device's copy uploaded later).
func (q *Queue) extendHidden(ctx context.Context) error {
	rows, err := q.Pool.Query(ctx, `SELECT id::text,hidden_at,hidden_rule,hidden_rules_version,COALESCE(hidden_by::text,''),hidden_scope
		FROM conversations WHERE hidden_at IS NOT NULL AND hidden_root=id`)
	if err != nil {
		return err
	}
	type root struct {
		id, rule, by, scope string
		at                  time.Time
		version             int64
	}
	roots, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (root, error) {
		var r root
		return r, row.Scan(&r.id, &r.at, &r.rule, &r.version, &r.by, &r.scope)
	})
	if err != nil {
		return err
	}
	for _, r := range roots {
		tree := hideTreeSQL
		if r.scope == "device" {
			tree = hideDeviceTreeSQL
		}
		if _, err := q.Pool.Exec(ctx, tree, r.id, r.at, r.rule, r.version, r.by); err != nil {
			return err
		}
	}
	return nil
}

// purgeHidden purges hidden sessions through the deletion machinery:
// those hidden before cutoff (nil: any), under rule ("": any). Each is
// re-checked against the current rules first and restored instead when
// they no longer cover it. actor requests the deletions (an administrator
// confirming); "" means the administrator whose rule change hid it. It
// returns how many it purged and restored.
func (q *Queue) purgeHidden(ctx context.Context, r serverRules, cutoff *time.Time, rule, actor, deviceID, reason string) (purged, restored int, err error) {
	list, err := q.convRows(ctx, `c.hidden_at IS NOT NULL AND c.hidden_root=c.id AND ($1::timestamptz IS NULL OR c.hidden_at<$1)
		AND ($2='' OR c.hidden_rule=$2) ORDER BY c.hidden_at,c.id`, cutoff, rule)
	if err != nil {
		return 0, 0, err
	}
	for _, c := range list {
		p, restoredOne, err := q.purgeHiddenOne(ctx, r, c, cutoff, rule, actor, deviceID, reason)
		purged += p
		restored += restoredOne
		if err != nil {
			return purged, restored, err
		}
	}
	return purged, restored, nil
}

// purgeHiddenOne pins policy state through the final deletion. Metadata updates
// take the exclusive form of these device locks before natural session locks.
func (q *Queue) purgeHiddenOne(ctx context.Context, r serverRules, c convRow, cutoff *time.Time, rule, actor, deviceID, reason string) (purged, restored int, outErr error) {
	conn, err := q.Pool.Acquire(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer conn.Release()
	if err := store.PinBackend(ctx, conn); err != nil {
		return 0, 0, err
	}
	var locks []store.AdvisoryLock
	defer func() { outErr = errors.Join(outErr, store.ReleaseAdvisoryLocks(conn, q.Pool, locks...)) }()
	if c.agent == "claude" {
		rows, err := q.Pool.Query(ctx, `SELECT id::text FROM devices WHERE user_id=$1 ORDER BY id::text COLLATE "C"`, c.user)
		if err != nil {
			return 0, 0, err
		}
		devices, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return 0, 0, err
		}
		for _, device := range devices {
			key := policyDeviceLockKey(device)
			if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock_shared(hashtextextended($1,0))`, key); err != nil {
				return 0, 0, err
			}
			locks = append(locks, store.AdvisoryLock{Key: key, Shared: true})
		}
	}
	// Keep the singleton's current rule version stable through deletion.
	// UpdatePolicyWithAudit cannot acknowledge removal while this row is held.
	guard, err := conn.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := guard.Rollback(cleanup); !errors.Is(err, pgx.ErrTxClosed) {
			outErr = errors.Join(outErr, err)
		}
	}()
	var singleton bool
	if err := guard.QueryRow(ctx, `SELECT singleton FROM collection_policy WHERE singleton FOR SHARE`).Scan(&singleton); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, err
	}
	// The initial sweep snapshot only nominates a candidate. Recheck after the
	// policy gate, including expiry/confirmation filters and current admin rules.
	fresh, err := q.convRows(ctx, `c.id=$1 AND c.hidden_at IS NOT NULL AND c.hidden_root=c.id
		AND ($2::timestamptz IS NULL OR c.hidden_at<$2) AND ($3='' OR c.hidden_rule=$3)`, c.id, cutoff, rule)
	if err != nil {
		return 0, 0, err
	}
	if len(fresh) == 0 {
		return 0, 0, nil
	}
	c = fresh[0]
	r, err = loadRules(ctx, q.Pool)
	if err != nil {
		return 0, 0, err
	}
	deviceOnly := c.scope == "device" || c.hasPolicy
	if c.agent == "claude" && !deviceOnly {
		members, err := q.convRows(ctx, `c.hidden_root=$1 AND c.hidden_at IS NOT NULL`, c.id)
		if err != nil {
			return 0, 0, err
		}
		for _, member := range members {
			if member.hasPolicy {
				deviceOnly = true
				break
			}
		}
	}
	st := store.NewPostgres(q.Pool, nil, "")

	d := r.decideConv(c)
	if d.Mode == pathpolicy.Allow {
		if err := q.restore(ctx, r, c, "no_longer_covered"); err != nil {
			return 0, 0, err
		}
		return 0, 1, nil
	}
	// Missing mapping proof is a sharing hold, never expiry deletion.
	if c.hasPolicy && (isPolicyHold(r.decideConv(c)) || r.capacityPurgeHeld(c)) {
		return 0, 0, nil
	}
	who := actor
	if who == "" {
		who = c.by
	}
	if who == "" {
		who = r.updatedBy
	}
	if who == "" && c.hasPolicy {
		who = c.user
	}
	if who == "" {
		return 0, 0, errors.New("ingest: hidden conversation with no administrator to purge it as")
	}
	var job domain.DeletionJob
	if deviceOnly {
		// Keep surviving device copies rooted after the original root goes.
		if _, err := q.Pool.Exec(ctx, lockHideRoot+`UPDATE conversations SET hidden_root=id,hidden_scope='device'
			WHERE id IN(SELECT id FROM l) AND device_id<>$2::uuid`, c.id, c.device); err != nil {
			return 0, 0, err
		}
		job, _, err = st.WithholdDeviceSession(ctx, c.user, c.device, c.agent, c.session)
	} else {
		job, err = st.RequestConversationDeletion(ctx, c.id, who, deviceID, false)
	}
	if errors.Is(err, store.ErrNotFound) {
		return 0, 0, nil // went with a parent purged before it
	}
	if err != nil {
		return 0, 0, err
	}
	name := ruleName(d)
	err = pgx.BeginTxFunc(ctx, q.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if c.source != "" {
			if _, err := tx.Exec(ctx, `UPDATE sources SET refused_rule=COALESCE(refused_rule,$2) WHERE id=$1 AND tombstoned_at IS NOT NULL`, c.source, name); err != nil {
				return err
			}
		}
		return store.InsertAudit(ctx, tx, domain.AuditEvent{ID: uuid.NewString(), ActorID: who, DeviceID: deviceID,
			Action: "conversation.purged", TargetType: "conversation", TargetID: c.id,
			Metadata: map[string]any{"reason": reason, "rule": name, "hidden_rule": c.rule, "hidden_at": c.hiddenAt, "rules_version": r.version,
				"user_id": c.user, "device_id": c.device, "source_id": c.source, "path": c.path, "job_id": job.ID},
			CreatedAt: time.Now().UTC()})
	})
	if err != nil {
		return 1, 0, err
	}
	q.Log.Info("ingest: hidden conversation purged", "conversation", c.id, "reason", reason, "rule", name, "job", job.ID)
	return 1, 0, nil
}

// PurgeHidden purges now, as the administrator actor (on deviceID, which
// may be ""), every hidden session (under rule, when not ""), each
// re-checked against the current rules. It returns how many it purged
// and how many it restored instead.
func (q *Queue) PurgeHidden(ctx context.Context, actor, deviceID, rule string) (purged, restored int, err error) {
	if actor == "" {
		return 0, 0, errors.New("ingest: purging hidden sessions needs an administrator")
	}
	r, err := loadRules(ctx, q.Pool)
	if err != nil {
		return 0, 0, err
	}
	return q.purgeHidden(ctx, r, nil, rule, actor, deviceID, "admin_confirmed")
}

// Hidden summarizes the hidden sessions per user and per rule.
func (q *Queue) Hidden(ctx context.Context) (domain.HiddenSummary, error) {
	out := domain.HiddenSummary{ByUser: []domain.HiddenCount{}, ByRule: []domain.HiddenCount{}, PurgeAfter: HiddenPurgeAfter.String()}
	const roots = `FROM conversations c WHERE c.hidden_at IS NOT NULL AND c.hidden_root=c.id`
	if err := q.Pool.QueryRow(ctx, `SELECT count(*),min(c.hidden_at) `+roots).Scan(&out.Total, &out.OldestHiddenAt); err != nil {
		return out, err
	}
	rows, err := q.Pool.Query(ctx, `SELECT c.user_id::text,u.email,count(*) FROM conversations c JOIN users u ON u.id=c.user_id
		WHERE c.hidden_at IS NOT NULL AND c.hidden_root=c.id GROUP BY 1,2 ORDER BY 3 DESC,2`)
	if err != nil {
		return out, err
	}
	if out.ByUser, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.HiddenCount, error) {
		var h domain.HiddenCount
		return h, row.Scan(&h.UserID, &h.Email, &h.Sessions)
	}); err != nil {
		return out, err
	}
	rows, err = q.Pool.Query(ctx, `SELECT c.hidden_rule,count(*) `+roots+` GROUP BY 1 ORDER BY 2 DESC,1`)
	if err != nil {
		return out, err
	}
	out.ByRule, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.HiddenCount, error) {
		var h domain.HiddenCount
		return h, row.Scan(&h.Rule, &h.Sessions)
	})
	return out, err
}
