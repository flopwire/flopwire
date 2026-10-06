package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/cowork"
	"github.com/google/uuid"
)

type CoworkPolicyClient interface {
	PolicyPlacements(context.Context, *syncproto.PolicyPlacementsRequest) (*syncproto.PolicyPlacementsResponse, error)
}

type captureAuthorizer interface {
	SetAuthorize(func(context.Context, devicesync.SourceSpec) (*devicesync.CaptureAuthorization, error))
}

type captureEvidenceReader interface {
	CaptureEvidenceForOrigin(context.Context, transcript.Agent, string, string, ...string) (devicesync.CaptureEvidence, error)
}

var errCoworkHeld = errors.New("agent: Cowork policy scope is held")

// Sharing eligibility only schedules work. The authorization callback must
// independently obtain a durable server acknowledgement for each flush.
func (a *Agent) coworkSharingConfigured() bool {
	_, ok := a.cfg.Sync.(captureAuthorizer)
	_, evidence := a.cfg.Sync.(captureEvidenceReader)
	return ok && evidence && a.cfg.CoworkPolicy != nil
}

func (a *Agent) coworkMaySchedule(pv *policyView, t *target) bool {
	if !a.coworkSharingConfigured() {
		return false
	}
	keys, known := a.coworkCaptureScope(t)
	if !known || len(a.coworkUnregisteredPaths(t)) > 0 {
		return false
	}
	found := false
	for _, key := range keys {
		p, ok := a.storedPlace(key)
		if !ok || !localindex.IsCoworkPlacement(p.how) {
			continue
		}
		found = true
		if p.how == localindex.PlacedByCoworkUnknown || a.decide(pv.pol, p).Mode != pathpolicy.Allow {
			return false
		}
	}
	return found
}

func hostScopePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/" && path != "/sessions" && !strings.HasPrefix(path, "/sessions/") && !strings.ContainsAny(path, "\x00\t\n\r")
}

// policyRequest retains every observed host placement. VM locations in older
// native CLI placements cannot become Mac policy evidence.
func (a *Agent) coworkPolicyRequest(ctx context.Context, t *target) (*syncproto.PolicyPlacementsRequest, error) {
	keys, known := a.coworkCaptureScope(t)
	key := keys[0]
	p, ok := a.storedPlace(key)
	if !ok || !localindex.IsCoworkPlacement(p.how) {
		return nil, errCoworkHeld
	}
	r := &syncproto.PolicyPlacementsRequest{Version: 1, Agent: string(key.agent), SessionID: key.session, CurrentMappingKnown: known, ClientMode: syncproto.ClientModeAllow, EvidenceScope: syncproto.EvidenceNone,
		Device: &syncproto.DeviceDirs{Home: a.cfg.Home, ClaudeProjects: a.cfg.ClaudeProjects, CodexHome: a.cfg.CodexHome}}
	if len(keys) > 1 && keys[1].session != key.session {
		r.ParentSessionID = keys[1].session
	}
	placements := append([]pathpolicy.Placement{p.pl}, decodeOthers(p.others)...)
	historicalUnknown := p.how == localindex.PlacedByCoworkUnknown
	for _, parent := range keys[1:] {
		if pp, ok := a.storedPlace(parent); ok && localindex.IsCoworkPlacement(pp.how) {
			placements = append(placements, pp.pl)
			placements = append(placements, decodeOthers(pp.others)...)
			historicalUnknown = historicalUnknown || pp.how == localindex.PlacedByCoworkUnknown
		}
	}
	seen := map[syncproto.PolicyPlacement]bool{}
	for _, pl := range placements {
		if !hostScopePath(pl.Cwd) {
			continue
		}
		wire := syncproto.PolicyPlacement{CWD: pl.Cwd, WorktreeRoot: pl.Worktree, MainRoot: pl.Main, Remote: pl.Remote}
		if !seen[wire] {
			seen[wire] = true
			r.Placements = append(r.Placements, wire)
		}
	}
	sort.Slice(r.Placements, func(i, j int) bool {
		a, b := r.Placements[i], r.Placements[j]
		return a.CWD+"\x00"+a.WorktreeRoot+"\x00"+a.MainRoot+"\x00"+a.Remote < b.CWD+"\x00"+b.WorktreeRoot+"\x00"+b.MainRoot+"\x00"+b.Remote
	})
	if len(r.Placements) == 0 || len(a.coworkUnregisteredPaths(t)) > 0 {
		r.CurrentMappingKnown = false
	}
	if historicalUnknown {
		r.EvidenceScope = syncproto.EvidenceUnmapped
	} else {
		have, err := a.store.SessionHasEvidence(ctx, key.agent, key.session)
		if err != nil {
			return nil, err
		}
		if have {
			r.EvidenceScope = syncproto.EvidenceMapped
		}
	}
	// ClientMode carries actual configured restrictions. Readiness uncertainty
	// is separate, so a metadata-only child cannot invent a group-wide floor.
	pol := a.policy().pol
	floor := pathpolicy.Allow
	for _, pl := range placements {
		if !hostScopePath(pl.Cwd) {
			continue
		}
		subtree := pol.DecideSubtree(pl)
		if subtree.RepoScopeUnknown {
			r.CurrentMappingKnown = false
		}
		if subtree.Decision.Rule.Pattern != "" && subtree.Decision.Mode > floor {
			floor = subtree.Decision.Mode
		}
	}
	if historicalUnknown && pol.Unplaceable == pathpolicy.Deny {
		floor = pathpolicy.Deny
	}
	if floor == pathpolicy.Deny {
		r.ClientMode = syncproto.ClientModeDeny
	} else if floor == pathpolicy.Local {
		r.ClientMode = syncproto.ClientModeLocal
	}
	return r, nil
}

// capturePolicyEvidence runs under the exclusive scope gate. Existing captured
// bytes without proof taint history before a server can acknowledge new scope.
func (a *Agent) capturePolicyEvidence(ctx context.Context, t *target) ([]syncproto.PolicySource, error) {
	reader, ok := a.cfg.Sync.(captureEvidenceReader)
	if !ok {
		return nil, nil
	}
	keys, _ := a.coworkCaptureScope(t)
	var refs []syncproto.PolicySource
	unknown := false
	for i, key := range keys {
		var paths []string
		a.mu.Lock()
		for _, candidate := range a.targets {
			ck, _ := candidate.placeKeyOf()
			if ck == key {
				paths = append(paths, candidate.path)
			}
		}
		a.mu.Unlock()
		evidence, err := reader.CaptureEvidenceForOrigin(ctx, key.agent, key.session, "cowork", paths...)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			refs = append(refs, evidence.Sources...)
			refs = append(refs, evidence.RestrictionSources...)
		}
		unknown = unknown || evidence.Unproven
	}
	if unknown {
		if err := a.markCoworkHistoryUnknown(ctx, keys); err != nil {
			return nil, err
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Path != refs[j].Path {
			return refs[i].Path < refs[j].Path
		}
		if refs[i].FileID != refs[j].FileID {
			return refs[i].FileID < refs[j].FileID
		}
		return refs[i].Generation < refs[j].Generation
	})
	return refs, nil
}

func (a *Agent) recoveryPolicyEvidence(ctx context.Context, t *target) ([]syncproto.PolicyRecoverySource, error) {
	keys, _ := a.coworkCaptureScope(t)
	var own []syncproto.PolicyRecoverySource
	historical := false
	for i, key := range keys {
		id, parseErr := uuid.Parse(key.session)
		if parseErr != nil || id == uuid.Nil || id.String() != key.session {
			continue
		}
		refs, err := a.store.CoworkRecoverySources(ctx, key.session)
		if err != nil {
			return nil, err
		}
		if a.cfg.RecoveredPolicySources != nil {
			receipts, err := a.cfg.RecoveredPolicySources(ctx, key.session)
			if err != nil {
				return nil, err
			}
			seen := make(map[syncproto.PolicyRecoverySource]bool, len(refs))
			for _, ref := range refs {
				seen[ref] = true
			}
			for _, ref := range receipts {
				if !seen[ref] {
					refs = append(refs, ref)
					seen[ref] = true
				}
			}
		}
		historical = historical || len(refs) > 0
		if i == 0 {
			own = refs
		}
	}
	if historical {
		if err := a.markCoworkHistoryUnknown(ctx, keys); err != nil {
			return nil, err
		}
	}
	return own, nil
}

func (a *Agent) coworkSourceOpen(t *target) func(context.Context, devicesync.SourceSpec) (*os.File, error) {
	declared, physical := a.coworkSourceBoundary(t)
	return func(ctx context.Context, spec devicesync.SourceSpec) (*os.File, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rel, ok := under(declared, spec.Path)
		if !ok || declared == "" {
			return nil, fmt.Errorf("agent: evidence escaped its collection boundary")
		}
		return cowork.OpenFile(physical, filepath.Join(physical, rel))
	}
}

func (a *Agent) coworkSourceBoundary(t *target) (string, string) {
	root := a.nativeEvidenceRoot(t.path)
	if root == "" {
		// Identified CLI copies get the configured CLI boundary too. Resolving
		// a configured root alias does not authorize symlinks inside that root.
		root = a.cfg.ClaudeProjects
	}
	declared := root
	physical := root
	if a.nativeEvidenceRoot(t.path) == "" {
		physical = physicalPath(root)
	}
	return declared, physical
}

// markCoworkHistoryUnknown commits uncertainty before an old unqualified
// capture can be retried. A current complete mapping cannot qualify it.
func (a *Agent) markCoworkHistoryUnknown(ctx context.Context, keys []placeKey) error {
	err := a.markCoworkHistoricalUnknown(ctx, keys)
	// Durable facts remain authoritative even if a compatibility placement
	// write failed. A fact-write failure retains its scoped purge barrier.
	a.polMu.Lock()
	a.enforce(ctx, a.policy().pol)
	a.polMu.Unlock()
	return err
}

// mappingSignature checks the app's current grants, independently of the
// published in-memory snapshot. A changed grant invalidates an upload lease.
func (a *Agent) mappingSignature(t *target) (string, error) {
	keys, _ := a.coworkCaptureScope(t)
	r, err := cowork.Discover(a.cfg.CoworkRoot)
	if err != nil {
		return "", err
	}
	type grant struct {
		ID    string
		Known bool
		Paths []string
	}
	var grants []grant
	for _, l := range r.IdentityLinks {
		for _, k := range keys {
			if l.NativeSessionID == k.session {
				paths := coworkHostPaths(l)
				sort.Strings(paths)
				grants = append(grants, grant{l.NativeSessionID, l.Mapping.Known(), paths})
			}
		}
	}
	if len(grants) == 0 {
		return "", errCoworkHeld
	}
	sort.Slice(grants, func(i, j int) bool { return grants[i].ID < grants[j].ID })
	b, err := json.Marshal(grants)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(b)), nil
}

// authorizeCapture holds the outer scope gate until the scheduler finishes all
// capture and network handovers. No target, placement, or database lock spans a
// server call. Eligibility alone never authorizes evidence.
func (a *Agent) authorizeCapture(ctx context.Context, spec devicesync.SourceSpec) (*devicesync.CaptureAuthorization, error) {
	a.refreshCowork(ctx)
	a.refreshPolicy(ctx, false)
	a.captureScopeMu.Lock()
	release := a.captureScopeMu.Unlock
	if !a.allowUpload(spec) {
		release()
		return nil, errCoworkHeld
	}
	a.mu.Lock()
	t := a.targets[spec.Path]
	a.mu.Unlock()
	if t == nil {
		nt := &target{path: spec.Path, kind: kindTranscript, src: transcript.Source{Agent: spec.Agent, SessionKey: spec.SessionKey}}
		if spec.Parent != "" {
			nt.kind, nt.owner, nt.parent = kindCompanion, spec.SessionKey, spec.Parent
		}
		keys, _ := a.coworkCaptureScope(nt)
		protected := a.coworkOrigin(keys...) || a.nativeEvidenceRoot(spec.Path) != ""
		release()
		if protected {
			return nil, errCoworkHeld
		}
		return nil, nil
	}
	keys, _ := a.coworkCaptureScope(t)
	if !a.coworkOrigin(keys...) && !a.coworkScopePresent(keys) {
		if a.desktopCodeScoped(spec.Path) {
			return a.desktopCodeAuthorizationLocked(ctx, spec, t, release)
		}
		release()
		return nil, nil
	}
	if spec.Export || !a.coworkSharingConfigured() {
		release()
		return nil, errCoworkHeld
	}
	t.mu.Lock()
	a.mu.Lock()
	identity, offset := t.seen, t.scanned
	want := t.spec()
	indexed := t.kind != kindTranscript || t.parser != nil && transcript.ReparseKey(t.indexedWith) == indexingVersion(t.parser)
	if t.kind == kindCompanion {
		offset = identity.Size
	}
	a.mu.Unlock()
	t.mu.Unlock()
	if !indexed || spec.Agent != want.Agent || spec.SessionKey != want.SessionKey || spec.StorageKind != want.StorageKind || spec.Parent != want.Parent || spec.Parser != want.Parser || identity.ID == (transcript.FileID{}) || identity.CTime == 0 || offset <= 0 || offset > identity.Size {
		release()
		return nil, errCoworkHeld
	}
	refs, err := a.capturePolicyEvidence(ctx, t)
	if err != nil {
		release()
		return nil, err
	}
	recovery, err := a.recoveryPolicyEvidence(ctx, t)
	if err != nil {
		release()
		return nil, err
	}
	r, err := a.coworkPolicyRequest(ctx, t)
	if err != nil {
		release()
		return nil, err
	}
	r.Sources = refs
	r.RecoverySources = recovery
	signature, err := a.mappingSignature(t)
	if err != nil {
		release()
		return nil, err
	}
	policyKey, userStamp, adminStamp := a.policy().key, stampOf(a.cfg.UserRules), stampOf(a.cfg.AdminRulesCache)
	digest, err := syncproto.PolicyPlacementsDigest(r)
	if err != nil {
		release()
		return nil, err
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	ack, err := a.acknowledgeCoworkPolicy(rctx, r)
	cancel()
	if err != nil {
		release()
		return nil, err
	}
	if ack == nil || ack.Version != 1 || ack.Revision <= 0 || ack.RequestDigest != digest || !ack.Allowed || ack.EvidenceScope == syncproto.EvidenceUnmapped || !r.CurrentMappingKnown || r.ClientMode != syncproto.ClientModeAllow {
		release()
		return nil, errCoworkHeld
	}
	_, boundary := a.coworkSourceBoundary(t)
	auth := &devicesync.CaptureAuthorization{Origin: "cowork", Root: boundary, Proof: devicesync.CaptureProof{PolicyRequestDigest: digest, Identity: identity, Offset: offset}, Open: a.coworkSourceOpen(t), Release: release}
	if t.kind == kindCompanion {
		auth.Proof.ContentSHA, err = a.store.CompanionDigest(ctx, t.path)
		if err != nil || len(auth.Proof.ContentSHA) != sha256.Size {
			release()
			if err != nil {
				return nil, err
			}
			return nil, errCoworkHeld
		}
	}
	auth.Check = func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if a.policy().key != policyKey || stampOf(a.cfg.UserRules) != userStamp || stampOf(a.cfg.AdminRulesCache) != adminStamp {
			return errCoworkHeld
		}
		a.mu.Lock()
		stillIndexed := t.seen == identity && (t.kind != kindTranscript || t.scanned >= offset && t.parser != nil && transcript.ReparseKey(t.indexedWith) == indexingVersion(t.parser))
		a.mu.Unlock()
		if !stillIndexed {
			return errCoworkHeld
		}
		now, err := a.mappingSignature(t)
		if err != nil {
			return err
		}
		if now != signature {
			return errCoworkHeld
		}
		return nil
	}
	auth.OnError = func(ctx context.Context, err error) {
		var unproven *devicesync.UnprovenCaptureError
		if errors.As(err, &unproven) && unproven.Captured {
			if e := a.markCoworkHistoryUnknown(ctx, keys); e != nil {
				a.log.Error("agent: retaining unknown Cowork capture scope", "err", e)
			}
		}
	}
	if err := auth.Check(ctx); err != nil {
		release()
		return nil, err
	}
	return auth, nil
}
