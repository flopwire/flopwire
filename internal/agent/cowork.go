package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/cowork"
)

// CoworkStatus distinguishes local coverage from shared readiness. The server
// currently cannot persist/reapply multi-folder host policy, so all identified
// Cowork evidence stays local, including overlapping CLI copies.
type CoworkStatus struct {
	Root                   string         `json:"root,omitempty"`
	State                  string         `json:"state"`
	Sessions               int            `json:"sessions"`
	MetadataOnly           int            `json:"metadata_only"`
	Excluded               int            `json:"excluded_paths"`
	Unknown                int            `json:"unknown_mapping"`
	RepositoryScopeUnknown int            `json:"unknown_repository_scopes"`
	HistoricalUnknown      int            `json:"historical_unknown_mapping"`
	MappingReasons         map[string]int `json:"mapping_reasons,omitempty"`
	WatchCandidates        int            `json:"watch_candidates"`
	Held                   int            `json:"shared_held"`
	SharedHold             string         `json:"shared_hold"`
	Error                  string         `json:"error,omitempty"`
}

// refreshCowork runs before files reach the worker pool. Host locations are
// stored monotonically in placements: revoking an app grant must not remove
// the rules protecting work already performed in that folder. The origin is
// durable, so pending uploads and CLI overlaps remain held after a restart or
// disappearance of app metadata.
func (a *Agent) refreshCowork(ctx context.Context) {
	a.coworkMu.Lock()
	defer a.coworkMu.Unlock()
	r, err := cowork.Discover(a.cfg.CoworkRoot)
	if err != nil {
		a.mu.Lock()
		a.coworkError = "Cowork container discovery failed"
		a.mu.Unlock()
		a.log.Warn("agent: Cowork discovery failed", "error", err)
		return
	}
	a.captureScopeMu.Lock()
	defer a.captureScopeMu.Unlock()
	changed := false
	registrationFailed := false
	scopes := append([]cowork.Link(nil), r.IdentityLinks...)
	for _, entry := range r.Sessions {
		s, link := entry.Session, entry.Link
		scopes = append(scopes, link)
		for _, sa := range s.Subagents {
			child := link
			child.NativeSessionID = "agent-" + sa.AgentID
			scopes = append(scopes, child)
		}
	}
	for _, link := range scopes {
		for _, session := range []string{link.NativeSessionID} {
			key := placeKey{transcript.AgentClaude, session}
			p, existed := a.storedPlace(key)
			old := p
			paths := coworkHostPaths(link)
			have := map[string]bool{p.pl.Cwd: true}
			for _, q := range decodeOthers(p.others) {
				have[q.Cwd] = true
			}
			var add []pathpolicy.Placement
			for _, path := range paths {
				if !have[path] {
					add = append(add, a.resolve(path, ""))
					have[path] = true
				}
			}
			if !existed && len(add) > 0 {
				p.pl = add[0]
				add = add[1:]
			}
			p.others = mergeOthers(p.others, encodeOthers(add))
			p.how = localindex.PlacedByCowork
			legacyEvidence := false
			if !localindex.IsCoworkPlacement(old.how) {
				have, err := a.store.SessionHasEvidence(ctx, transcript.AgentClaude, session)
				legacyEvidence = err != nil || have
			}
			if legacyEvidence || old.how == localindex.PlacedByCoworkUnknown {
				p.how = localindex.PlacedByCoworkUnknown
			}
			// These locations come from host metadata, not a deleted VM checkout.
			p.checked = time.Now().UnixMilli()
			if existed {
				p.checked = old.checked
				if p.checked == 0 {
					p.checked = time.Now().UnixMilli()
				}
			}
			if !existed || p != old {
				if err := a.saveCoworkPlace(ctx, key, p); err != nil {
					registrationFailed = true
					a.log.Warn("agent: Cowork policy registration failed", "error", err)
				} else {
					changed = true
				}
			}
		}
	}
	a.mu.Lock()
	a.coworkResult = r
	a.coworkError = ""
	if registrationFailed {
		a.coworkError = "Cowork policy registration failed; evidence held"
	}
	a.mu.Unlock()
	if changed || registrationFailed {
		a.polMu.Lock()
		a.enforce(ctx, a.policy().pol)
		a.polMu.Unlock()
	}
}

// saveCoworkPlace publishes policy provenance only after its durable commit.
// Failed writes leave memory unchanged so every refresh retries registration.
func (a *Agent) saveCoworkPlace(ctx context.Context, key placeKey, p placed) error {
	a.placeWriteMu.Lock()
	defer a.placeWriteMu.Unlock()
	a.mu.Lock()
	old, ok := a.places[key]
	a.mu.Unlock()
	if ok {
		p.others = mergeOthers(old.others, p.others)
		p.cands = encodeCandidates(append(candidates(old.cands), candidates(p.cands)...))
		if old.pl != p.pl && old.pl.Cwd != "" {
			p.others = mergeOthers(p.others, encodeOthers([]pathpolicy.Placement{old.pl}))
		}
		if old.how == localindex.PlacedByCoworkUnknown {
			p.how = old.how
		}
	}
	if err := a.store.SavePlacement(ctx, localindex.Placement{Agent: key.agent, SessionID: key.session, Placement: p.pl, How: p.how, CheckedAt: p.checked, Candidates: p.cands, OtherCwds: p.others}); err != nil {
		return err
	}
	if err := a.store.Sync(ctx); err != nil {
		return err
	}
	a.mu.Lock()
	a.places[key] = p
	a.mu.Unlock()
	return nil
}

func (a *Agent) coworkDirectory(dir string) bool {
	if a.cfg.CoworkRoot == "" {
		return false
	}
	if _, ok := under(a.cfg.CoworkRoot, dir); ok {
		return true
	}
	// The configured container may not exist yet. Its existing ancestors are
	// watched so creating it can trigger a full pass before the next sweep.
	if _, ok := under(dir, a.cfg.CoworkRoot); ok {
		return true
	}
	return false
}

// coworkMode applies the stored union even if the app metadata disappeared.
// The durable origin restricts only identified native session identities.
func (a *Agent) coworkMode(pv *policyView, t *target) (pathpolicy.Decision, bool) {
	a.mu.Lock()
	key, _ := t.placeKeyOf()
	root := t.root
	a.mu.Unlock()
	p, ok := a.storedPlace(key)
	if (!ok || !localindex.IsCoworkPlacement(p.how)) && root != "" {
		p, ok = a.storedPlace(placeKey{transcript.AgentClaude, root})
	}
	if ok && localindex.IsCoworkPlacement(p.how) {
		d := a.decide(pv.pol, p)
		for _, path := range a.coworkUnregisteredPaths(t) {
			if path == "" {
				continue
			}
			next := pv.pol.DecideSubtree(a.resolve(path, "")).Decision
			if next.Mode > d.Mode {
				d = next
			}
		}
		if d.Mode < pathpolicy.Local {
			d.Mode = pathpolicy.Local
		}
		return d, true
	}
	if keys, known := a.coworkCaptureScope(t); known || a.coworkScopePresent(keys) {
		d := pathpolicy.Decision{Mode: pathpolicy.Local}
		for _, path := range a.coworkUnregisteredPaths(t) {
			next := pv.pol.DecideSubtree(a.resolve(path, "")).Decision
			if next.Mode > d.Mode {
				d = next
			}
		}
		return d, true
	}
	if a.cfg.CoworkRoot != "" {
		if _, ok := under(a.cfg.CoworkRoot, t.path); ok {
			return pathpolicy.Decision{Mode: pathpolicy.Local}, true
		}
	}
	return pathpolicy.Decision{}, false
}

func (a *Agent) coworkSafe(t *target) bool {
	if a.cfg.CoworkRoot == "" {
		return true
	}
	if _, ok := under(a.cfg.CoworkRoot, t.path); !ok {
		return true
	}
	return cowork.SafeFile(a.cfg.CoworkRoot, t.path)
}

func (a *Agent) coworkStatus() *CoworkStatus {
	a.mu.Lock()
	r := a.coworkResult
	st := &CoworkStatus{Root: a.cfg.CoworkRoot, State: "supported", Sessions: len(r.Sessions), MetadataOnly: r.MetadataOnly, Excluded: r.Excluded, Error: a.coworkError, SharedHold: "server host-folder policy support pending", WatchCandidates: len(r.WatchDirs), MappingReasons: map[string]int{}}
	for _, p := range a.places {
		if localindex.IsCoworkPlacement(p.how) {
			st.Held++
			if p.how == localindex.PlacedByCoworkUnknown {
				st.HistoricalUnknown++
			}
		}
	}
	a.mu.Unlock()
	switch {
	case a.cfg.CoworkRoot == "":
		st.State = "disabled"
	case st.Error != "":
		st.State = "unavailable"
	case r.Unavailable > 0:
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

// coworkCaptureScope separates current readiness from historical uncertainty.
// Metadata-only unknown sessions do not taint future first controlled content.
func (a *Agent) coworkCaptureScope(t *target) ([]placeKey, bool) {
	a.mu.Lock()
	key, _ := t.placeKeyOf()
	root := t.root
	r := a.coworkResult
	if t.kind == kindCompanion {
		if parent := a.targets[t.parent]; parent != nil {
			root = parent.root
		}
	}
	a.mu.Unlock()
	keys := []placeKey{key}
	if root != "" {
		keys = append(keys, placeKey{transcript.AgentClaude, root})
	}
	found, known := false, true
	for _, link := range r.IdentityLinks {
		if link.NativeSessionID == key.session {
			found = true
			known = known && link.Mapping.Known()
		}
	}
	for _, entry := range r.Sessions {
		s := entry.Session
		if s.SessionID == key.session {
			found = true
			known = known && entry.Link.Mapping.Known()
		}
		for _, child := range s.Subagents {
			if "agent-"+child.AgentID == key.session {
				if root == "" {
					keys = append(keys, placeKey{transcript.AgentClaude, s.SessionID})
				}
				found = true
				known = known && entry.Link.Mapping.Known()
			}
		}
	}
	return keys, found && known
}

func (a *Agent) coworkScopePresent(keys []placeKey) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, key := range keys {
		for _, link := range a.coworkResult.IdentityLinks {
			if link.NativeSessionID == key.session {
				return true
			}
		}
		for _, entry := range a.coworkResult.Sessions {
			if entry.Session.SessionID == key.session {
				return true
			}
			for _, child := range entry.Session.Subagents {
				if "agent-"+child.AgentID == key.session {
					return true
				}
			}
		}
	}
	return false
}

// coworkHostPaths preserves logical grants and verified physical aliases,
// including missing descendants whose existing ancestor resolves physically.
func coworkHostPaths(link cowork.Link) []string {
	paths := link.Mapping.HostPaths()
	seen := map[string]bool{}
	for _, path := range paths {
		seen[path] = true
	}
	for _, path := range append([]string(nil), paths...) {
		physical := physicalPath(path)
		if physical != "" && !seen[physical] {
			paths = append(paths, physical)
			seen[physical] = true
		}
	}
	return paths
}

// coworkUnregisteredPaths detects incomplete durable scope registration,
// including failures extending an already registered session's folder union.
func (a *Agent) coworkUnregisteredPaths(t *target) []string {
	keys, _ := a.coworkCaptureScope(t)
	a.mu.Lock()
	defer a.mu.Unlock()
	var missing []string
	for _, key := range keys {
		p, ok := a.places[key]
		have := map[string]bool{p.pl.Cwd: true}
		for _, other := range decodeOthers(p.others) {
			have[other.Cwd] = true
		}
		check := func(link cowork.Link) {
			if !ok || !localindex.IsCoworkPlacement(p.how) {
				missing = append(missing, "")
			}
			for _, path := range coworkHostPaths(link) {
				if !have[path] {
					missing = append(missing, path)
				}
			}
		}
		for _, link := range a.coworkResult.IdentityLinks {
			if link.NativeSessionID == key.session {
				check(link)
			}
		}
		for _, entry := range a.coworkResult.Sessions {
			if entry.Session.SessionID == key.session {
				check(entry.Link)
			}
			for _, child := range entry.Session.Subagents {
				if "agent-"+child.AgentID == key.session {
					check(entry.Link)
				}
			}
		}
	}
	return missing
}

// taintCoworkEvidence commits uncertainty before unknown bytes are indexed.
// Once evidence was accepted without a complete mapping, a later metadata
// snapshot cannot establish the folder scope of that earlier work.
func (a *Agent) taintCoworkEvidence(ctx context.Context, t *target) error {
	keys, known := a.coworkCaptureScope(t)
	if len(a.coworkUnregisteredPaths(t)) > 0 {
		return fmt.Errorf("Cowork policy scope is not durably registered")
	}
	if a.coworkScopePresent(keys) {
		for _, key := range keys {
			p, ok := a.storedPlace(key)
			if !ok || !localindex.IsCoworkPlacement(p.how) {
				return fmt.Errorf("Cowork policy provenance is not durably registered")
			}
		}
	}
	if known {
		return nil
	}
	a.placeWriteMu.Lock()
	defer a.placeWriteMu.Unlock()
	updates := map[placeKey]placed{}
	for _, key := range keys {
		a.mu.Lock()
		p, ok := a.places[key]
		a.mu.Unlock()
		if !ok || !localindex.IsCoworkPlacement(p.how) || p.how == localindex.PlacedByCoworkUnknown {
			continue
		}
		p.how = localindex.PlacedByCoworkUnknown
		if err := a.store.SavePlacement(ctx, localindex.Placement{Agent: key.agent, SessionID: key.session, Placement: p.pl, How: p.how, CheckedAt: p.checked, Candidates: p.cands, OtherCwds: p.others}); err != nil {
			return err
		}
		updates[key] = p
	}
	if len(updates) == 0 {
		return nil
	}
	if err := a.store.Sync(ctx); err != nil {
		return err
	}
	a.mu.Lock()
	for key, p := range updates {
		a.places[key] = p
	}
	a.mu.Unlock()
	return nil
}
