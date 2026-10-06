// Package desktopcode discovers Claude Desktop Code-tab session-scoped native
// Claude history. Ordinary chat and application metadata are never sources.
package desktopcode

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/flopwire/flopwire/internal/fsprobe"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/google/uuid"
)

const maxMetadataBytes = 8 << 20

// DefaultRoot returns the macOS Code metadata container. Other platforms need
// an explicitly configured container. An empty Discover root disables discovery.
func DefaultRoot(home string) string {
	if runtime.GOOS != "darwin" || home == "" {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", "Claude", "claude-code-sessions")
}

// Link is metadata linkage, never uploadable evidence. Counts refer only to the
// caller's normal projects root and the bounded session-scoped candidate root.
type Link struct {
	SessionID         string
	CLISessionIDs     []string
	MetadataPath      string
	ProjectsRoot      string
	NormalTranscripts int
	ScopedTranscripts int
}

// CoworkOriginAlias preserves imported Cowork provenance after its original
// container disappears. It is policy evidence only: Code cwd supplies no Cowork
// host-folder proof. Consumers must hold matching native IDs with unknown mapping
// unless durable Cowork policy evidence already supplies the stricter decision.
type CoworkOriginAlias struct {
	SessionID         string
	CLISessionIDs     []string
	MetadataPath      string
	ImportedFrom      string
	NormalTranscripts int
}

type DiscoveredSession struct {
	Session *claude.Session
	Link    Link
}

// RootState distinguishes a missing normal root from linkage that could not
// be checked. Scoped discovery continues when the normal root is unreadable.
type RootState string

const (
	RootDisabled   RootState = "disabled"
	RootMissing    RootState = "missing"
	RootUnreadable RootState = "unreadable"
	RootAvailable  RootState = "available"
)

type Result struct {
	CoworkOriginAliases []CoworkOriginAlias
	NormalRootState     RootState
	Sessions            []DiscoveredSession // scoped sessions only; normal discovery stays separate
	IdentityLinks       []Link              // verified metadata identities, including missing history
	WatchDirs           []string
	MetadataOnly        int // valid metadata with no linked main transcript in either root
	Unavailable         int // missing container
	OutOfScope          int // explicit non-Code/non-local or unsupported metadata shape
	UnreadableMetadata  int
	Excluded            int // rejected layout paths or returned native files
}

type metadata struct {
	SessionID              string          `json:"sessionId"`
	Cwd                    string          `json:"cwd"`
	SessionType            string          `json:"sessionType"`
	ImportedFrom           string          `json:"importedFrom"`
	Backend                json.RawMessage `json:"backend"`
	SSHConfig              json.RawMessage `json:"sshConfig"`
	WSLConfig              json.RawMessage `json:"wslConfig"`
	CLISessionID           string          `json:"cliSessionId"`
	UnarchivedCLISessionID string          `json:"unarchivedCliSessionId"`
}

// Discover honors normalProjectsRoot as supplied; it never adds ~/.claude as an
// alternate root or follows stagedTranscriptPath. Layout candidates were checked
// against Claude Desktop 2.19675.1 application code. Verified metadata identity
// and local Code shape are required before inspecting scoped native history;
// fresh runtime qualification
// is a separate check. Enumeration is bounded to account/org/session directories.
func Discover(root, normalProjectsRoot string) (Result, error) {
	var out Result
	if root == "" {
		return out, nil
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return out, err
	}
	watch := map[string]bool{}
	finish := func() { out.WatchDirs = sortedKeys(watch) }
	st, err := os.Lstat(root)
	if os.IsNotExist(err) {
		out.Unavailable++
		for p := filepath.Dir(root); ; p = filepath.Dir(p) {
			if s, e := os.Stat(p); e == nil && s.IsDir() {
				watch[p] = true
				break
			}
			if filepath.Dir(p) == p {
				break
			}
		}
		finish()
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		out.Excluded++
		finish()
		return out, nil
	}
	boundary, err := os.OpenRoot(root)
	if err != nil {
		return out, err
	}
	defer boundary.Close()
	// Reject substitution of the declared root between Lstat and OpenRoot.
	current, err := boundary.Stat(".")
	if err != nil {
		return out, err
	}
	if !os.SameFile(st, current) {
		out.Excluded++
		finish()
		return out, nil
	}
	normalIDs, normalState := normalIdentityCounts(normalProjectsRoot)
	out.NormalRootState = normalState
	watch[root] = true
	accounts, err := readDirs(boundary, ".")
	if err != nil {
		return out, err
	}
	for _, account := range accounts {
		if !account.IsDir() {
			if account.Type()&os.ModeSymlink != 0 {
				out.Excluded++
			}
			continue
		}
		a := account.Name()
		if !safe(boundary, a, true) {
			out.Excluded++
			continue
		}
		watch[filepath.Join(root, a)] = true
		orgs, e := readDirs(boundary, a)
		if e != nil {
			if os.IsNotExist(e) {
				continue
			}
			return out, e
		}
		for _, org := range orgs {
			if !org.IsDir() {
				if org.Type()&os.ModeSymlink != 0 {
					out.Excluded++
				}
				continue
			}
			o := filepath.Join(a, org.Name())
			if !safe(boundary, o, true) {
				out.Excluded++
				continue
			}
			watch[filepath.Join(root, o)] = true
			entries, e := readDirs(boundary, o)
			if e != nil {
				if os.IsNotExist(e) {
					continue
				}
				return out, e
			}
			ids := map[string]bool{}
			for _, entry := range entries {
				if entry.Type()&os.ModeSymlink != 0 {
					out.Excluded++
					continue
				}
				if entry.IsDir() {
					if sessionIdentity(entry.Name()) {
						ids[entry.Name()] = true
					}
				} else if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".json") {
					id := strings.TrimSuffix(entry.Name(), ".json")
					if sessionIdentity(id) {
						ids[id] = true
					}
				}
			}
			for _, id := range sortedKeys(ids) {
				meta := filepath.Join(o, id+".json")
				md, e := readMetadata(boundary, meta)
				verified := e == nil && md.SessionID == id
				nativeIDs := map[string]bool{}
				if verified {
					for _, s := range []string{md.CLISessionID, md.UnarchivedCLISessionID} {
						if s != "" {
							if !canonicalIdentity(s) {
								verified = false
								break
							}
							nativeIDs[s] = true
						}
					}
					verified = verified && len(nativeIDs) > 0
				}
				if !verified {
					out.UnreadableMetadata++
					continue
				}
				if !localCodeMetadata(md) {
					out.OutOfScope++
					if coworkOrigin(md.ImportedFrom) {
						alias := CoworkOriginAlias{SessionID: id, CLISessionIDs: sortedKeys(nativeIDs), MetadataPath: filepath.Join(root, meta), ImportedFrom: md.ImportedFrom}
						for _, nativeID := range alias.CLISessionIDs {
							alias.NormalTranscripts += normalIDs[nativeID]
						}
						out.CoworkOriginAliases = append(out.CoworkOriginAliases, alias)
					}
					continue
				}
				scoped := filepath.Join(o, id, ".claude", "projects")
				link := Link{SessionID: id, MetadataPath: filepath.Join(root, meta), ProjectsRoot: filepath.Join(root, scoped)}
				if verified {
					link.CLISessionIDs = sortedKeys(nativeIDs)
					for _, s := range link.CLISessionIDs {
						link.NormalTranscripts += normalIDs[s]
					}
				}
				complete := true
				for _, p := range []string{filepath.Join(o, id), filepath.Join(o, id, ".claude"), scoped} {
					if !safe(boundary, p, true) {
						complete = false
						if _, e := boundary.Lstat(p); e == nil {
							out.Excluded++
						}
						break
					}
					watch[filepath.Join(root, p)] = true
				}
				var discovered []*claude.Session
				if complete {
					// Native discovery uses absolute paths and directory entries. Its
					// final containment filters reject unsafe returned files; callers
					// must still use a contained secure opener for transcript reads.
					// These checks do not make directory enumeration atomic against
					// a concurrent replacement of the absolute candidate path.
					discovered, e = claude.Discover(filepath.Join(root, scoped))
					if e != nil {
						if !os.IsNotExist(e) {
							return out, e
						}
					}
					filtered := discovered[:0]
					for _, s := range discovered {
						// The app resolves history by its metadata CLI identity.
						// Rewinds or unrelated parent sessions deposited beside
						// it do not establish Code provenance. Matched parent
						// subagents and companions remain part of that session.
						if !nativeIDs[s.SessionID] {
							continue
						}
						rel, e := filepath.Rel(root, s.ProjectDir)
						if e != nil || !safe(boundary, rel, true) {
							out.Excluded++
							continue
						}
						watch[s.ProjectDir] = true
						filterSession(boundary, root, s, &out)
						if s.Transcript == "" && len(s.Subagents) == 0 && len(s.Companions) == 0 {
							continue
						}
						if s.Transcript != "" && nativeIDs[s.SessionID] && verified {
							link.ScopedTranscripts++
						}
						filtered = append(filtered, s)
					}
					discovered = filtered
				}
				if verified {
					out.IdentityLinks = append(out.IdentityLinks, link)
					if normalState != RootUnreadable && link.NormalTranscripts+link.ScopedTranscripts == 0 {
						out.MetadataOnly++
					}
				}
				for _, s := range discovered {
					out.Sessions = append(out.Sessions, DiscoveredSession{Session: s, Link: cloneLink(link)})
				}
			}
		}
	}
	finish()
	return out, nil
}

func cloneLink(l Link) Link { l.CLISessionIDs = append([]string(nil), l.CLISessionIDs...); return l }
func canonicalIdentity(s string) bool {
	u, e := uuid.Parse(s)
	return e == nil && u != uuid.Nil && u.String() == s
}
func sessionIdentity(s string) bool { return canonicalIdentity(strings.TrimPrefix(s, "local_")) }
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
func readDirs(r *os.Root, p string) ([]os.DirEntry, error) {
	fsprobe.Note(fsprobe.OpList, filepath.Join(r.Name(), p))
	f, e := r.Open(p)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	v, e := f.ReadDir(-1)
	sort.Slice(v, func(i, j int) bool { return v[i].Name() < v[j].Name() })
	return v, e
}
func safe(r *os.Root, p string, dir bool) bool {
	if filepath.IsAbs(p) || p == ".." || strings.HasPrefix(p, ".."+string(filepath.Separator)) {
		return false
	}
	parts := strings.Split(filepath.Clean(p), string(filepath.Separator))
	cur := "."
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		fsprobe.Note(fsprobe.OpStat, filepath.Join(r.Name(), cur))
		s, e := r.Lstat(cur)
		if e != nil || s.Mode()&os.ModeSymlink != 0 {
			return false
		}
		if i < len(parts)-1 || dir {
			if !s.IsDir() {
				return false
			}
		} else if !s.Mode().IsRegular() {
			return false
		}
	}
	return true
}
func readMetadata(r *os.Root, p string) (metadata, error) {
	var md metadata
	if !safe(r, p, false) {
		return md, os.ErrInvalid
	}
	fsprobe.Note(fsprobe.OpOpen, filepath.Join(r.Name(), p))
	f, e := openMetadata(r, p)
	if e != nil {
		return md, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return md, e
	}
	if !st.Mode().IsRegular() || st.Size() > maxMetadataBytes {
		return md, os.ErrInvalid
	}
	d := json.NewDecoder(io.LimitReader(f, maxMetadataBytes+1))
	if e = d.Decode(&md); e != nil {
		return md, e
	}
	var extra any
	if e = d.Decode(&extra); !errors.Is(e, io.EOF) {
		return md, os.ErrInvalid
	}
	return md, nil
}
func filterSession(r *os.Root, root string, s *claude.Session, out *Result) {
	regular := func(p string) bool { rel, e := filepath.Rel(root, p); return e == nil && safe(r, rel, false) }
	if s.Transcript != "" && !regular(s.Transcript) {
		s.Transcript = ""
		out.Excluded++
	}
	agents := s.Subagents[:0]
	for _, a := range s.Subagents {
		if !regular(a.Path) {
			out.Excluded++
			continue
		}
		if a.MetaPath != "" && !regular(a.MetaPath) {
			a.MetaPath = ""
			out.Excluded++
		}
		agents = append(agents, a)
	}
	s.Subagents = agents
	companions := s.Companions[:0]
	for _, c := range s.Companions {
		if regular(c.Path) {
			companions = append(companions, c)
		} else {
			out.Excluded++
		}
	}
	s.Companions = companions
}

// SafeFile checks containment and rejects symlinks before a discovered source
// is read. Callers must use their existing secure transcript-open machinery.
func SafeFile(root, path string) bool {
	if root == "" || !filepath.IsAbs(root) || !filepath.IsAbs(path) {
		return false
	}
	st, e := os.Lstat(root)
	if e != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return false
	}
	r, e := os.OpenRoot(root)
	if e != nil {
		return false
	}
	defer r.Close()
	current, e := r.Stat(".")
	if e != nil || !os.SameFile(st, current) {
		return false
	}
	rel, e := filepath.Rel(root, path)
	return e == nil && safe(r, rel, false)
}

// Only inspect main filenames when correlating normal history. Native collection
// already discovers companions separately; status must not recursively rescan
// their trees for each Desktop linkage refresh.
func normalIdentityCounts(root string) (map[string]int, RootState) {
	ids := map[string]int{}
	if root == "" {
		return ids, RootDisabled
	}
	projects, err := fsprobe.ReadDir(root)
	if os.IsNotExist(err) {
		return ids, RootMissing
	}
	if err != nil {
		return ids, RootUnreadable
	}
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		entries, err := fsprobe.ReadDir(filepath.Join(root, project.Name()))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return ids, RootUnreadable
		}
		for _, entry := range entries {
			if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".jsonl") {
				ids[strings.TrimSuffix(entry.Name(), ".jsonl")]++
			}
		}
	}
	return ids, RootAvailable
}

// The current app serializes sshConfig/wslConfig and reconstructs its runtime
// backend from them. Code records with absent backend are therefore legitimate;
// an absolute host cwd supplies the legacy local shape. Explicit origin/type
// markers must still exclude ordinary chat and other surfaces.
func localCodeMetadata(m metadata) bool {
	if m.SessionType != "" && m.SessionType != "code" {
		return false
	}
	switch m.ImportedFrom {
	case "", "local-1p-code", "terminal-cli", "sibling-1p-code", "sibling-3p-code", "previous-profile-code":
	default:
		return false
	}
	present := func(v json.RawMessage) bool { return len(v) > 0 && string(v) != "null" }
	if present(m.SSHConfig) || present(m.WSLConfig) {
		return false
	}
	if present(m.Backend) {
		var kind string
		if json.Unmarshal(m.Backend, &kind) != nil {
			var backend struct {
				Kind string `json:"kind"`
			}
			if json.Unmarshal(m.Backend, &backend) != nil {
				return false
			}
			kind = backend.Kind
		}
		if kind != "local" {
			return false
		}
	}
	return filepath.IsAbs(m.Cwd) && filepath.Clean(m.Cwd) == m.Cwd && !strings.ContainsAny(m.Cwd, "\t\n\r\x00") && m.Cwd != "/sessions" && !strings.HasPrefix(m.Cwd, "/sessions/")
}

// These tags are defined by the current app's oG import-origin enumeration and
// its sibling/previous-profile surface variants. Chat origins are never aliases.
func coworkOrigin(origin string) bool {
	switch origin {
	case "local-1p-cowork", "sibling-1p-cowork", "sibling-3p-cowork", "previous-profile-cowork":
		return true
	default:
		return false
	}
}
