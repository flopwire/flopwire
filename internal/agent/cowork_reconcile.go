package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

// coworkOrigin recognizes a durable policy scope independently of current
// discovery. CLI copies and a restart must not select user-scoped deletions.
func (a *Agent) coworkOrigin(keys ...placeKey) bool {
	for _, key := range keys {
		if p, ok := a.storedPlace(key); ok && localindex.IsCoworkPlacement(p.how) {
			return true
		}
	}
	return false
}

// acknowledgeCoworkPolicy validates the durable response even when a custom
// client implements Config.CoworkPolicy. Restrictions can be acknowledged
// without granting capture permission.
func (a *Agent) acknowledgeCoworkPolicy(ctx context.Context, req *syncproto.PolicyPlacementsRequest) (ack *syncproto.PolicyPlacementsResponse, err error) {
	failure := ""
	defer func() { a.noteCoworkPolicyAttempt(coworkPolicyAttemptState(req, ack, err, failure)) }()
	if a.cfg.CoworkPolicy == nil {
		failure = "unsupported"
		return nil, errCoworkHeld
	}
	digest, err := syncproto.PolicyPlacementsDigest(req)
	if err != nil {
		return nil, err
	}
	ack, err = a.cfg.CoworkPolicy.PolicyPlacements(ctx, req)
	if err != nil {
		return nil, err
	}
	failure = "invalid_ack"
	if ack == nil || ack.Version != 1 || ack.Revision <= 0 || ack.RequestDigest != digest {
		return nil, errors.New("agent: invalid durable Cowork policy acknowledgement")
	}
	switch ack.EvidenceScope {
	case syncproto.EvidenceNone, syncproto.EvidenceMapped, syncproto.EvidenceUnmapped:
	default:
		return nil, errors.New("agent: invalid Cowork evidence acknowledgement")
	}
	if req.EvidenceScope == syncproto.EvidenceUnmapped && ack.EvidenceScope != syncproto.EvidenceUnmapped || req.EvidenceScope == syncproto.EvidenceMapped && ack.EvidenceScope == syncproto.EvidenceNone {
		return nil, errors.New("agent: Cowork acknowledgement weakens historical scope")
	}
	if ack.Allowed && (!req.CurrentMappingKnown || req.ClientMode != syncproto.ClientModeAllow || ack.EvidenceScope == syncproto.EvidenceUnmapped || req.ScopeStatus == syncproto.ScopeLimitHeld) {
		return nil, errors.New("agent: inconsistent Cowork sharing acknowledgement")
	}
	// The server may retain older shared bytes that this local index no longer
	// knows about. Its durable historical fact must survive a client restart.
	failure = "history_unknown"
	if ack.EvidenceScope == syncproto.EvidenceUnmapped {
		keys := []placeKey{{transcript.AgentClaude, req.SessionID}}
		if req.ParentSessionID != "" && req.ParentSessionID != req.SessionID {
			keys = append(keys, placeKey{transcript.AgentClaude, req.ParentSessionID})
		}
		if err := a.markCoworkHistoryUnknown(ctx, keys); err != nil {
			return nil, err
		}
	}
	return ack, nil
}

// reconcileCoworkPolicy sends metadata for every retained Cowork placement,
// including denied, metadata-only, and disappeared sessions. It never opens
// content or enables sharing. Call outside the capture scope gate after local
// placement/policy refresh; each batch holds that gate through acknowledgement.
func (a *Agent) reconcileCoworkPolicy(ctx context.Context) error {
	if a.cfg.CoworkPolicy == nil {
		return nil
	}
	a.mu.Lock()
	keys := make([]placeKey, 0)
	for key, p := range a.places {
		if localindex.IsCoworkPlacement(p.how) {
			keys = append(keys, key)
		}
	}
	cursor := a.coworkReconcileCursor
	a.mu.Unlock()
	orderKey := func(key placeKey) string { return string(key.agent) + ":" + key.session }
	sort.Slice(keys, func(i, j int) bool {
		return orderKey(keys[i]) < orderKey(keys[j])
	})
	start := sort.Search(len(keys), func(i int) bool { return orderKey(keys[i]) > orderKey(cursor) })
	keys = append(append([]placeKey(nil), keys[start:]...), keys[:start]...)
	var failures []error
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		// Advance before a slow or failing RPC. The next bounded pass starts
		// after this key rather than indefinitely repeating the same prefix.
		a.mu.Lock()
		a.coworkReconcileCursor = key
		a.mu.Unlock()
		err := a.reconcileCoworkKey(ctx, key)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s:%s: %w", key.agent, key.session, err))
		}
	}
	return errors.Join(failures...)
}

func (a *Agent) reconcileCoworkBounded(ctx context.Context) {
	if a.cfg.CoworkPolicy == nil {
		return
	}
	// Local preparation can wait behind the index writer. Only the network
	// request below has a 10-second budget; preparation must not consume it.
	if err := a.reconcileCoworkPolicy(ctx); err != nil && ctx.Err() == nil {
		a.log.Info("agent: Cowork sharing held; policy metadata not acknowledged", "err", err)
	}
}

func (a *Agent) reconcileCoworkKey(ctx context.Context, key placeKey) error {
	a.captureScopeMu.Lock()
	defer a.captureScopeMu.Unlock()
	// The gate is not context-aware. Never dispatch canceled work after a wait.
	if err := ctx.Err(); err != nil {
		return err
	}
	if !a.coworkOrigin(key) {
		return nil
	}
	// A retained session can have no live target after denial or deletion.
	t := &target{kind: kindTranscript, src: transcript.Source{Agent: key.agent, SessionKey: key.session}}
	a.mu.Lock()
	for _, existing := range a.targets {
		existingKey, _ := existing.placeKeyOf()
		if existingKey == key {
			t = existing
			break
		}
	}
	a.mu.Unlock()
	refs, err := a.capturePolicyEvidence(ctx, t)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	recovery, err := a.recoveryPolicyEvidence(ctx, t)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	req, err := a.coworkPolicyRequest(ctx, t)
	if err != nil {
		return err
	}
	req.Sources = refs
	req.RecoverySources = recovery
	// The serialized index writer may finish a read after caller cancellation.
	if err := ctx.Err(); err != nil {
		return err
	}
	rpcCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = a.acknowledgeCoworkPolicy(rpcCtx, req)
	if err != nil {
		return err
	}
	// The device-scoped ledger now owns this restriction. Do not leave an
	// obsolete user-scoped deletion queued after successful reconciliation.
	return a.store.SetWithhold(ctx, key.agent, key.session, "", "")
}
