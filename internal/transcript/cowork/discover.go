// Package cowork locates native Claude transcripts in Claude Desktop's
// local-agent-mode-sessions container. App metadata is policy evidence only;
// it is never returned as a transcript or companion.
package cowork

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/google/uuid"
)

const maxMetadataBytes = 8 << 20

// DefaultRoot returns the verified macOS app container. Other platforms have
// no verified default; callers may configure an explicit container.
func DefaultRoot(home string) string {
	if runtime.GOOS != "darwin" || home == "" {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", "Claude", "local-agent-mode-sessions")
}

// Mapping is an immutable snapshot of host-folder policy evidence. Unknown
// mappings still expose verified host paths so deny rules can be applied.
type Mapping struct {
	hostPaths []string
	reason    string
}

func (m Mapping) HostPaths() []string { return append([]string(nil), m.hostPaths...) }
func (m Mapping) Known() bool         { return m.reason == "" && len(m.hostPaths) > 0 }
func (m Mapping) Reason() string      { return m.reason }

// Link associates a native session (including all its subagents/companions)
// with its app metadata. Native session identity and file paths are preserved.
type Link struct {
	NativeSessionID string
	ProjectDir      string
	SessionID       string
	CLISessionID    string
	MetadataPath    string
	ProjectsRoot    string
	Mapping         Mapping
}

// DiscoveredSession binds native evidence to its policy/linkage snapshot.
type DiscoveredSession struct {
	Session *claude.Session
	Link    Link
}

type Result struct {
	Sessions      []DiscoveredSession
	IdentityLinks []Link   // verified metadata identities, even without native files
	WatchDirs     []string // existing ancestors, including metadata-only sessions
	MetadataOnly  int
	Unavailable   int
	Excluded      int // rejected symlink/non-regular paths
}

type metadata struct {
	SessionID         string
	CLISessionID      string
	Selected          []string
	Approved          []string
	DeleteMounts      []string
	Malformed         bool
	identityMalformed bool
}

// Decode policy fields independently. A malformed mapping must hold sharing,
// while a valid host path in another field must remain available to deny rules.
// RawMessage is used only for these five fields, never private prompt/title data.
func (m *metadata) UnmarshalJSON(b []byte) error {
	var raw struct {
		SessionID    json.RawMessage `json:"sessionId"`
		CLISessionID json.RawMessage `json:"cliSessionId"`
		Selected     json.RawMessage `json:"userSelectedFolders"`
		Approved     json.RawMessage `json:"userApprovedFileAccessPaths"`
		DeleteMounts json.RawMessage `json:"fileDeleteApprovedMounts"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	for _, field := range []struct {
		raw json.RawMessage
		dst *string
	}{{raw.SessionID, &m.SessionID}, {raw.CLISessionID, &m.CLISessionID}} {
		if len(field.raw) > 0 && (string(field.raw) == "null" || json.Unmarshal(field.raw, field.dst) != nil) {
			m.Malformed = true
			m.identityMalformed = true
		}
	}
	for _, field := range []struct {
		raw json.RawMessage
		dst *[]string
	}{{raw.Selected, &m.Selected}, {raw.Approved, &m.Approved}, {raw.DeleteMounts, &m.DeleteMounts}} {
		if len(field.raw) == 0 {
			continue
		}
		var parts []json.RawMessage
		if string(field.raw) == "null" || json.Unmarshal(field.raw, &parts) != nil {
			m.Malformed = true
			continue
		}
		for _, part := range parts {
			var s string
			if string(part) == "null" || json.Unmarshal(part, &s) != nil {
				m.Malformed = true
				continue
			}
			*field.dst = append(*field.dst, s)
		}
	}
	return nil
}

// Discover enumerates only account/workspace/session directories. Missing
// containers are normal. Symlinks below the declared root, and a symlink root,
// are rejected; OS aliases above the configured root are trusted.
func Discover(root string) (Result, error) {
	var out Result
	if root == "" {
		return out, nil
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return out, err
	}
	watch := map[string]bool{}
	if !safe(root, root, true) {
		if _, err := os.Lstat(root); os.IsNotExist(err) {
			out.Unavailable++
			// Watch the nearest existing ancestor so a container appearing after
			// startup can trigger rediscovery. Sweeps remain the fallback.
			for p := filepath.Dir(root); ; p = filepath.Dir(p) {
				if st, err := os.Lstat(p); err == nil && st.IsDir() {
					watch[p] = true
					break
				}
				if filepath.Dir(p) == p {
					break
				}
			}
		} else {
			out.Excluded++
		}
		out.WatchDirs = sortedKeys(watch)
		return out, nil
	}
	watch[root] = true
	accounts, err := os.ReadDir(root)
	if err != nil {
		return out, err
	}
	for _, account := range accounts {
		accountPath := filepath.Join(root, account.Name())
		if !account.IsDir() {
			if account.Type()&os.ModeSymlink != 0 {
				out.Excluded++
			}
			continue
		}
		if !safe(root, accountPath, true) {
			out.Excluded++
			continue
		}
		watch[accountPath] = true
		workspaces, err := os.ReadDir(accountPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return out, err
		}
		for _, workspace := range workspaces {
			workspacePath := filepath.Join(accountPath, workspace.Name())
			if !workspace.IsDir() {
				if workspace.Type()&os.ModeSymlink != 0 {
					out.Excluded++
				}
				continue
			}
			if !safe(root, workspacePath, true) {
				out.Excluded++
				continue
			}
			watch[workspacePath] = true
			entries, err := os.ReadDir(workspacePath)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return out, err
			}
			ids := map[string]bool{}
			for _, entry := range entries {
				if entry.Type()&os.ModeSymlink != 0 {
					out.Excluded++
					continue
				}
				if entry.IsDir() {
					ids[entry.Name()] = true
				} else if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".json") {
					ids[strings.TrimSuffix(entry.Name(), ".json")] = true
				}
			}
			for _, id := range sortedKeys(ids) {
				if id == "" {
					continue
				}
				metaPath := filepath.Join(workspacePath, id+".json")
				md, mapping := readMetadata(root, metaPath, id)
				projectsRoot := filepath.Join(workspacePath, id, ".claude", "projects")
				if !md.identityMalformed && md.SessionID == id && canonicalCLIIdentity(md.CLISessionID) {
					out.IdentityLinks = append(out.IdentityLinks, Link{NativeSessionID: md.CLISessionID, SessionID: id, CLISessionID: md.CLISessionID, MetadataPath: metaPath, ProjectsRoot: projectsRoot, Mapping: mapping})
				}
				complete := true
				for _, p := range []string{filepath.Join(workspacePath, id), filepath.Join(workspacePath, id, ".claude"), projectsRoot} {
					if !safe(root, p, true) {
						complete = false
						if _, e := os.Lstat(p); e == nil {
							out.Excluded++
						}
						break
					}
					watch[p] = true
				}
				if !complete {
					out.MetadataOnly++
					continue
				}
				sessions, err := claude.Discover(projectsRoot)
				if err != nil {
					if os.IsNotExist(err) {
						continue
					}
					return out, err
				}
				before := len(out.Sessions)
				for _, s := range sessions {
					if !safe(root, s.ProjectDir, true) {
						out.Excluded++
						continue
					}
					watch[s.ProjectDir] = true
					filterSession(root, s, &out)
					if s.Transcript == "" && len(s.Subagents) == 0 && len(s.Companions) == 0 {
						continue
					}
					m := mapping
					if m.reason == "" && (md.CLISessionID == "" || md.CLISessionID != s.SessionID) {
						m.reason = "native session does not match metadata cliSessionId"
					}
					out.Sessions = append(out.Sessions, DiscoveredSession{Session: s, Link: Link{NativeSessionID: s.SessionID, ProjectDir: s.ProjectDir, SessionID: id, CLISessionID: md.CLISessionID, MetadataPath: metaPath, ProjectsRoot: projectsRoot, Mapping: m}})
				}
				if len(out.Sessions) == before {
					out.MetadataOnly++
				}
			}
		}
	}
	out.WatchDirs = sortedKeys(watch)
	return out, nil
}

func canonicalCLIIdentity(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u.String() == id && u != uuid.Nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func readMetadata(root, path, id string) (metadata, Mapping) {
	var md metadata
	unknown := Mapping{reason: "missing or malformed metadata"}
	if !safe(root, path, false) {
		return md, unknown
	}
	f, err := OpenFile(root, path)
	if err != nil {
		return md, unknown
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() > maxMetadataBytes {
		return md, unknown
	}
	d := json.NewDecoder(io.LimitReader(f, maxMetadataBytes+1))
	if err = d.Decode(&md); err != nil {
		return metadata{}, unknown
	}
	var extra any
	if err = d.Decode(&extra); !errors.Is(err, io.EOF) {
		return metadata{}, unknown
	}
	m := Mapping{}
	if md.Malformed {
		m.reason = "malformed mapping or session identity"
	}
	seen := map[string]bool{}
	for _, paths := range [][]string{md.Selected, md.Approved} {
		for _, p := range paths {
			if !filepath.IsAbs(p) || filepath.Clean(p) != p || strings.ContainsAny(p, "\t\n\r\x00") || p == "/" || p == "/sessions" || strings.HasPrefix(p, "/sessions/") {
				m.reason = "unresolved or non-host folder mapping"
				continue
			}
			if !seen[p] {
				seen[p] = true
				m.hostPaths = append(m.hostPaths, p)
			}
		}
	}
	// Deletion mounts are opaque identifiers in observed app metadata. Even an
	// absolute-looking value has no verified host-folder semantics.
	if len(md.DeleteMounts) > 0 {
		m.reason = "unresolved deletion mount mapping"
	}
	if len(m.hostPaths) == 0 && m.reason == "" {
		m.reason = "no verified host folders"
	}
	if md.SessionID != id || md.CLISessionID == "" {
		m.reason = "missing or mismatched session identity"
	}
	sort.Strings(m.hostPaths)
	return md, m
}

// safe checks lexical containment and every component inside the configured
// boundary. It deliberately uses Lstat, including for companion meta sidecars.
func safe(root, path string, dir bool) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	p := root
	parts := []string{}
	if rel != "." {
		parts = strings.Split(rel, string(filepath.Separator))
	}
	for i := 0; i <= len(parts); i++ {
		if i > 0 {
			p = filepath.Join(p, parts[i-1])
		}
		st, err := os.Lstat(p)
		if err != nil || st.Mode()&os.ModeSymlink != 0 {
			return false
		}
		if i < len(parts) || dir {
			if !st.IsDir() {
				return false
			}
		} else if !st.Mode().IsRegular() {
			return false
		}
	}
	return true
}

// SafeFile rechecks regular-file containment immediately before an agent reads
// an earlier discovered source. Symlinks below or at root are rejected.
func SafeFile(root, path string) bool {
	if root == "" || !filepath.IsAbs(root) || !filepath.IsAbs(path) {
		return false
	}
	return safe(filepath.Clean(root), filepath.Clean(path), false)
}

func filterSession(root string, s *claude.Session, out *Result) {
	if s.Transcript != "" && !safe(root, s.Transcript, false) {
		s.Transcript = ""
		out.Excluded++
	}
	subagents := s.Subagents[:0]
	for _, sa := range s.Subagents {
		if !safe(root, sa.Path, false) {
			out.Excluded++
			continue
		}
		if sa.MetaPath != "" && !safe(root, sa.MetaPath, false) {
			sa.MetaPath = ""
			out.Excluded++
		}
		subagents = append(subagents, sa)
	}
	s.Subagents = subagents
	companions := s.Companions[:0]
	for _, c := range s.Companions {
		if safe(root, c.Path, false) {
			companions = append(companions, c)
		} else {
			out.Excluded++
		}
	}
	s.Companions = companions
}
