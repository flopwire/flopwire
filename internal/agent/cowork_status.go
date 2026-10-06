package agent

import (
	"errors"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/cowork"
)

// CoworkPolicyAttempt describes the latest policy registration response. It is
// diagnostic history, never a cached authorization or proof that bytes shared.
type CoworkPolicyAttempt struct {
	At    time.Time `json:"at,omitempty"`
	State string    `json:"state"`
}

// CoworkStatus separates local coverage from scheduling eligibility. Every
// capture still needs a fresh durable server acknowledgement.
type CoworkStatus struct {
	Root                   string              `json:"root,omitempty"`
	State                  string              `json:"state"`
	Sessions               int                 `json:"sessions"`
	MetadataOnly           int                 `json:"metadata_only"`
	Excluded               int                 `json:"excluded_paths"`
	Unknown                int                 `json:"unknown_mapping"`
	RepositoryScopeUnknown int                 `json:"unknown_repository_scopes"`
	HistoricalUnknown      int                 `json:"historical_unknown_mapping"`
	MappingReasons         map[string]int      `json:"mapping_reasons,omitempty"`
	WatchCandidates        int                 `json:"watch_candidates"`
	Held                   int                 `json:"shared_held"`
	ScheduleEligible       int                 `json:"schedule_eligible"`
	HoldReasons            map[string]int      `json:"hold_reasons,omitempty"`
	SharedHold             string              `json:"shared_hold"`
	LastPolicyAttempt      CoworkPolicyAttempt `json:"last_policy_attempt"`
	Error                  string              `json:"error,omitempty"`
}

func (a *Agent) noteCoworkPolicyAttempt(state string) {
	a.mu.Lock()
	a.coworkPolicyAttempt = CoworkPolicyAttempt{At: a.now(), State: state}
	a.mu.Unlock()
}

func coworkPolicyAttemptState(req *syncproto.PolicyPlacementsRequest, ack *syncproto.PolicyPlacementsResponse, err error, failure string) string {
	if err != nil {
		switch {
		case errors.Is(err, devicesync.ErrPolicyLimitHeld):
			return "limit_held"
		case errors.Is(err, devicesync.ErrPolicyUnsupported):
			return "unsupported"
		case failure != "":
			return failure
		default:
			return "transport_unavailable"
		}
	}
	switch {
	case req.ScopeStatus == syncproto.ScopeLimitHeld:
		return "limit_held"
	case ack.EvidenceScope == syncproto.EvidenceUnmapped:
		return "history_unknown"
	case !req.CurrentMappingKnown:
		return "current_mapping_unknown"
	case req.ClientMode == syncproto.ClientModeDeny:
		return "policy_deny"
	case req.ClientMode == syncproto.ClientModeLocal:
		return "policy_local"
	case !ack.Allowed:
		return "server_restriction"
	default:
		return "acknowledged"
	}
}

func (a *Agent) coworkStatus() *CoworkStatus {
	a.captureScopeMu.RLock()
	defer a.captureScopeMu.RUnlock()
	a.mu.Lock()
	r := a.coworkResult
	st := &CoworkStatus{Root: a.cfg.CoworkRoot, State: "supported", Sessions: len(r.Sessions), MetadataOnly: r.MetadataOnly, Excluded: r.Excluded, Error: a.coworkError, WatchCandidates: len(r.WatchDirs), MappingReasons: map[string]int{}, HoldReasons: map[string]int{}, LastPolicyAttempt: a.coworkPolicyAttempt}
	keys := map[placeKey]*target{}
	for key, p := range a.places {
		if localindex.IsCoworkPlacement(p.how) {
			keys[key] = &target{kind: kindTranscript, src: transcript.Source{Agent: key.agent, SessionKey: key.session}}
			if p.how == localindex.PlacedByCoworkUnknown {
				st.HistoricalUnknown++
			}
		}
	}
	for key := range a.coworkPendingUnknown {
		if keys[key] == nil {
			keys[key] = &target{kind: kindTranscript, src: transcript.Source{Agent: key.agent, SessionKey: key.session}}
		}
	}
	for _, t := range a.targets {
		key, _ := t.placeKeyOf()
		if keys[key] != nil {
			keys[key] = &target{kind: t.kind, path: t.path, root: t.root, parent: t.parent, owner: t.owner, src: t.src}
		}
	}
	a.mu.Unlock()
	if st.LastPolicyAttempt.State == "" {
		st.LastPolicyAttempt.State = "not_attempted"
	}
	if a.coworkSharingConfigured() {
		st.SharedHold = "fresh server policy acknowledgement required for every capture"
	} else {
		st.SharedHold = "Cowork sharing is not configured"
	}
	for _, t := range keys {
		if a.coworkMaySchedule(a.policy(), t) {
			st.ScheduleEligible++
			continue
		}
		st.Held++
		st.HoldReasons[a.coworkStatusHoldReason(t)]++
	}
	switch {
	case a.cfg.CoworkRoot == "":
		st.State = "disabled"
	case st.Error != "" || r.Unavailable > 0:
		st.State = "unavailable"
	case r.Excluded > 0 && len(r.WatchDirs) == 0:
		st.State = "excluded"
	}
	seen := map[string]bool{}
	links := append([]cowork.Link(nil), r.IdentityLinks...)
	for _, entry := range r.Sessions {
		links = append(links, entry.Link)
	}
	for _, link := range links {
		key := link.MetadataPath + "\x00" + link.NativeSessionID
		if seen[key] {
			continue
		}
		seen[key] = true
		repoUnknown := false
		for _, path := range coworkHostPaths(link) {
			repoUnknown = repoUnknown || a.policy().pol.DecideSubtree(a.resolve(path, "")).RepoScopeUnknown
		}
		if repoUnknown {
			st.RepositoryScopeUnknown++
		}
		if !link.Mapping.Known() {
			st.Unknown++
			st.MappingReasons[link.Mapping.Reason()]++
		}
	}
	return st
}

// Each ineligible identity has one primary reason; reasons sum to shared_held.
func (a *Agent) coworkStatusHoldReason(t *target) string {
	keys, known := a.coworkCaptureScope(t)
	historical, repoUnknown, local := false, false, false
	a.mu.Lock()
	registrationError := a.coworkError != "" || a.coworkHistoryReadErr != nil
	for _, key := range keys {
		historical = historical || a.coworkPendingUnknown[key]
	}
	a.mu.Unlock()
	for _, key := range keys {
		p, ok := a.storedPlace(key)
		if !ok || !localindex.IsCoworkPlacement(p.how) {
			continue
		}
		historical = historical || p.how == localindex.PlacedByCoworkUnknown
		decision := a.decide(a.policy().pol, p)
		if decision.Mode == pathpolicy.Deny {
			return "policy_deny"
		}
		local = local || decision.Mode == pathpolicy.Local
		for _, pl := range append([]pathpolicy.Placement{p.pl}, decodeOthers(p.others)...) {
			repoUnknown = repoUnknown || a.policy().pol.DecideSubtree(pl).RepoScopeUnknown
		}
	}
	switch {
	case historical:
		return "historical_mapping_unknown"
	case repoUnknown:
		return "repository_scope_unknown"
	case registrationError || len(a.coworkUnregisteredPaths(t)) > 0:
		return "registration_error"
	case !known:
		return "current_mapping_unknown"
	case local:
		return "policy_local"
	case !a.coworkSharingConfigured():
		return "sharing_unconfigured"
	default:
		return "registration_error"
	}
}
