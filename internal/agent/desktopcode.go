package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/flopwire/flopwire/internal/fsprobe"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/cowork"
	"github.com/flopwire/flopwire/internal/transcript/desktopcode"
	"github.com/google/uuid"
)

type DesktopCodeStatus struct {
	Root            string                `json:"root,omitempty"`
	State           string                `json:"state"`
	Sessions        int                   `json:"scoped_native_sessions"`
	NormalLinks     int                   `json:"normal_root_links"`
	NormalRootState desktopcode.RootState `json:"normal_root_state,omitempty"`
	MetadataOnly    int                   `json:"missing_linked_transcripts"`
	OutOfScope      int                   `json:"outside_local_code_scope"`
	InvalidMetadata int                   `json:"invalid_metadata"`
	Excluded        int                   `json:"excluded_paths"`
	CoworkAliases   int                   `json:"cowork_origin_aliases"`
	SharedHold      string                `json:"scoped_shared_hold,omitempty"`
	Error           string                `json:"error,omitempty"`
}

// Called under the shared desktop registration/capture writer lease. Both
// metadata snapshots precede normal CLI target capture, including Cowork copies
// whose original app container disappeared.
func (a *Agent) refreshDesktopCodeLocked(ctx context.Context) {
	r, err := desktopcode.Discover(a.cfg.DesktopCodeRoot, a.cfg.ClaudeProjects)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.desktopCodeError = "Local Code container discovery failed"
		return
	}
	a.desktopCodeResult, a.desktopCodeError = r, ""
}

// Identity-only imports join the same durable family classifier as genuine
// Cowork metadata. Current genuine mapping wins; Code cwd supplies no proof.
func (a *Agent) desktopCodeCoworkScopes(r cowork.Result) ([]cowork.Link, map[string][]string) {
	a.mu.Lock()
	aliases := a.desktopCodeResult.CoworkOriginAliases
	a.mu.Unlock()
	var links []cowork.Link
	roots := map[string][]string{}
	for _, alias := range aliases {
		projects := filepath.Join(filepath.Dir(alias.MetadataPath), alias.SessionID, ".claude", "projects")
		for _, id := range alias.CLISessionIDs {
			link := cowork.Link{NativeSessionID: id, CLISessionID: id, SessionID: alias.SessionID, MetadataPath: alias.MetadataPath, ProjectsRoot: projects}
			for _, actual := range r.IdentityLinks {
				if actual.NativeSessionID == id {
					link = actual
					break
				}
			}
			for _, actual := range r.Sessions {
				if actual.Session.SessionID == id {
					link = actual.Link
					break
				}
			}
			links = append(links, link)
			roots[id] = append(roots[id], projects)
		}
	}
	return links, roots
}

func (a *Agent) desktopCodeCoworkFamilies(groups map[string]map[string]bool) {
	a.mu.Lock()
	aliases := a.desktopCodeResult.CoworkOriginAliases
	a.mu.Unlock()
	for _, alias := range aliases {
		for _, root := range alias.CLISessionIDs {
			if groups[root] == nil {
				groups[root] = map[string]bool{}
			}
			for _, id := range alias.CLISessionIDs {
				groups[root][id] = true
			}
		}
	}
}

func (a *Agent) desktopCodeCoworkAlias(keys []placeKey) bool {
	a.mu.Lock()
	aliases := a.desktopCodeResult.CoworkOriginAliases
	a.mu.Unlock()
	for _, key := range keys {
		if key.agent != transcript.AgentClaude {
			continue
		}
		for _, alias := range aliases {
			for _, id := range alias.CLISessionIDs {
				if id == key.session {
					return true
				}
			}
		}
	}
	return false
}

func (a *Agent) desktopCodeScoped(path string) bool {
	if a.cfg.DesktopCodeRoot == "" {
		return false
	}
	_, ok := under(a.cfg.DesktopCodeRoot, path)
	return ok
}
func (a *Agent) desktopCodeDirectory(dir string) bool {
	if a.cfg.DesktopCodeRoot == "" {
		return false
	}
	if _, ok := under(a.cfg.DesktopCodeRoot, dir); ok {
		return true
	}
	_, ok := under(dir, a.cfg.DesktopCodeRoot)
	return ok
}
func (a *Agent) nativeEvidenceRoot(path string) string {
	for _, root := range []string{a.cfg.CoworkRoot, a.cfg.DesktopCodeRoot} {
		if root != "" {
			if _, ok := under(root, path); ok {
				return root
			}
		}
	}
	return ""
}
func (a *Agent) openNativeEvidence(path string) (*os.File, error) {
	if root := a.nativeEvidenceRoot(path); root != "" {
		return cowork.OpenFile(root, path)
	}
	return fsprobe.Open(path)
}

// Infer only the native Claude parent-directory layout inside configured roots.
// Callers still require a verified metadata identity before granting provenance.
func (a *Agent) nativeClaudeParent(path string) string {
	for _, root := range []string{a.cfg.ClaudeProjects, a.cfg.CoworkRoot, a.cfg.DesktopCodeRoot} {
		if root == "" {
			continue
		}
		rel, ok := under(root, path)
		if !ok {
			continue
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		parentIndex := 1
		if root != a.cfg.ClaudeProjects {
			if len(parts) < 8 || parts[3] != ".claude" || parts[4] != "projects" {
				continue
			}
			parentIndex = 6
		}
		if len(parts) <= parentIndex+1 {
			continue
		}
		id := parts[parentIndex]
		u, e := uuid.Parse(id)
		if e == nil && u != uuid.Nil && u.String() == id {
			return id
		}
	}
	return ""
}
func (a *Agent) desktopCodeStatus() *DesktopCodeStatus {
	a.mu.Lock()
	r := a.desktopCodeResult
	err := a.desktopCodeError
	a.mu.Unlock()
	st := &DesktopCodeStatus{Root: a.cfg.DesktopCodeRoot, State: "supported", Sessions: len(r.Sessions), NormalRootState: r.NormalRootState, MetadataOnly: r.MetadataOnly, OutOfScope: r.OutOfScope, InvalidMetadata: r.UnreadableMetadata, Excluded: r.Excluded, CoworkAliases: len(r.CoworkOriginAliases), Error: err}
	for _, link := range r.IdentityLinks {
		st.NormalLinks += link.NormalTranscripts
	}
	switch {
	case a.cfg.DesktopCodeRoot == "":
		st.State = "disabled"
	case err != "" || r.Unavailable > 0:
		st.State = "unavailable"
	case r.Excluded > 0 && len(r.WatchDirs) == 0:
		st.State = "excluded"
	}
	if st.Sessions > 0 && !a.desktopCodeSharingConfigured() {
		st.SharedHold = "contained sync authorization support pending"
	}
	return st
}
