// Discovery of Claude Code transcripts and their companion files.
//
// Layout knowledge (recursive subagents, workflow runs, meta.json sidecars,
// tool-results/) follows spec §4.3 and §5.4 and was cross-checked against
// kenn-io/agentsview internal/parser/claude.go (claudeToolResultDirs,
// claudeCompanionParentSessionID) at commit
// 563023de1d7b7f5af50ad5967c101341a44a2bfc (MIT) and
// franken-agent-detection 0.3.1 src/connectors/claude_code.rs
// (projects_root_candidates) at git c06d1cb (MIT). No code copied.

package claude

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/flopwire/flopwire/internal/transcript"
)

// ProjectsRoot returns $CLAUDE_CONFIG_DIR/projects when CLAUDE_CONFIG_DIR is
// set, else ~/.claude/projects. getenv is os.Getenv in production.
func ProjectsRoot(getenv func(string) string, home string) string {
	if dir := getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "projects")
	}
	return filepath.Join(home, ".claude", "projects")
}

// CompanionRole says what a companion file is for.
type CompanionRole string

const (
	// CompanionMeta is a subagent's agent-<id>.meta.json sidecar.
	CompanionMeta CompanionRole = "meta"
	// CompanionToolResultText is a tool-results/*.txt (or other text) file
	// whose content replaces a <persisted-output> preview.
	CompanionToolResultText CompanionRole = "tool_result_text"
	// CompanionToolResultBinary is a tool-results/ binary (jpg, pdf, png,
	// docx, ...): archived, never indexed.
	CompanionToolResultBinary CompanionRole = "tool_result_binary"
	// CompanionOther is any other file under the session directory
	// (workflow journal.jsonl, workflows/<run>.json, scripts, ...):
	// archived, never indexed.
	CompanionOther CompanionRole = "other"
)

// Companion is a file uploaded whole beside its session's transcripts.
type Companion struct {
	Path string
	Role CompanionRole
}

// Indexed reports whether the companion's text feeds a message row.
func (c Companion) Indexed() bool { return c.Role == CompanionToolResultText }

// SubagentFile is one subagent transcript.
type SubagentFile struct {
	Path     string // .../subagents[/workflows/<runId>]/agent-<id>.jsonl
	MetaPath string // sibling agent-<id>.meta.json, "" when absent
	AgentID  string
	RunID    string // workflow run id, "" for ordinary subagents
}

// Session is one Claude Code session: its main transcript (absent when
// Claude's cleanup deleted it), subagent transcripts, and companions.
type Session struct {
	SessionID  string
	ProjectDir string // <projects>/<encoded cwd>
	Transcript string // <ProjectDir>/<SessionID>.jsonl, "" when orphaned
	Subagents  []SubagentFile
	Companions []Companion
}

// Orphaned reports whether the main transcript is gone.
func (s *Session) Orphaned() bool { return s.Transcript == "" }

// Sources lists the transcript sources of the session, main first.
func (s *Session) Sources() []transcript.Source {
	var out []transcript.Source
	if s.Transcript != "" {
		out = append(out, newSource(s.Transcript, s.SessionID))
	}
	for _, sa := range s.Subagents {
		out = append(out, newSource(sa.Path, SubagentSessionID(sa.AgentID)))
	}
	return out
}

// Stub is the conversation record for an orphaned session, so its
// companions have a conversation to hang from. Nil when not orphaned.
func (s *Session) Stub() *transcript.Conversation {
	if !s.Orphaned() {
		return nil
	}
	return &transcript.Conversation{
		Agent:     transcript.AgentClaude,
		SessionID: s.SessionID,
		Extra:     map[string]any{"orphaned": true},
	}
}

func newSource(path, sessionKey string) transcript.Source {
	return transcript.Source{
		Agent:       transcript.AgentClaude,
		Path:        path,
		SessionKey:  sessionKey,
		StorageKind: transcript.StorageJSONLAppend,
		Parser:      ParserName,
	}
}

// SubagentSessionID is the conversation session id of a subagent:
// "agent-<agentId>", the name of its transcript file (agentsview uses the
// same id). Agent ids are 17 random hex digits, unique in practice.
func SubagentSessionID(agentID string) string { return "agent-" + agentID }

// Discover walks a projects root and returns every session, sorted by
// project directory then session id. It reads directory entries only. A
// missing root yields no sessions and no error.
func Discover(root string) ([]*Session, error) {
	projects, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var out []*Session
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		sessions, err := discoverProject(filepath.Join(root, p.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, sessions...)
	}
	return out, nil
}

// DiscoverProject lists the sessions of one project directory
// (<projects>/<encoded cwd>), as Discover does for every project. An
// indexer calls it when a directory event names that project.
func DiscoverProject(dir string) ([]*Session, error) { return discoverProject(dir) }

func discoverProject(dir string) ([]*Session, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	byID := map[string]*Session{}
	get := func(id string) *Session {
		s := byID[id]
		if s == nil {
			s = &Session{SessionID: id, ProjectDir: dir}
			byID[id] = s
		}
		return s
	}
	for _, e := range entries {
		name := e.Name()
		switch {
		case e.Type().IsRegular() && strings.HasSuffix(name, ".jsonl"):
			id := strings.TrimSuffix(name, ".jsonl")
			get(id).Transcript = filepath.Join(dir, name)
		case e.IsDir() && isUUID(name):
			if err := discoverSessionDir(get(name), filepath.Join(dir, name)); err != nil {
				return nil, err
			}
		}
	}
	out := make([]*Session, 0, len(byID))
	for _, s := range byID {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out, nil
}

// discoverSessionDir collects subagent transcripts anywhere under
// <session>/subagents (flat, workflows/<runId>/, or deeper) and every other
// file as a companion.
func discoverSessionDir(s *Session, dir string) error {
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil // removed during the walk
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		name := d.Name()
		switch {
		case parts[0] == "subagents" && isAgentFile(name, ".jsonl"):
			sa := SubagentFile{Path: path, AgentID: strings.TrimSuffix(strings.TrimPrefix(name, "agent-"), ".jsonl")}
			if len(parts) >= 4 && parts[1] == "workflows" {
				sa.RunID = parts[2]
			}
			meta := strings.TrimSuffix(path, ".jsonl") + ".meta.json"
			if st, err := os.Stat(meta); err == nil && st.Mode().IsRegular() {
				sa.MetaPath = meta
			}
			s.Subagents = append(s.Subagents, sa)
		case parts[0] == "subagents" && isAgentFile(name, ".meta.json"):
			s.Companions = append(s.Companions, Companion{Path: path, Role: CompanionMeta})
		case parts[0] == "tool-results" && len(parts) == 2:
			role := CompanionToolResultBinary
			if isTextResult(name) {
				role = CompanionToolResultText
			}
			s.Companions = append(s.Companions, Companion{Path: path, Role: role})
		default:
			s.Companions = append(s.Companions, Companion{Path: path, Role: CompanionOther})
		}
		return nil
	})
	sort.Slice(s.Subagents, func(i, j int) bool { return s.Subagents[i].Path < s.Subagents[j].Path })
	sort.Slice(s.Companions, func(i, j int) bool { return s.Companions[i].Path < s.Companions[j].Path })
	return err
}

func isAgentFile(name, suffix string) bool {
	return strings.HasPrefix(name, "agent-") && strings.HasSuffix(name, suffix) && len(name) > len("agent-")+len(suffix)
}

// isTextResult reports whether a tool-results file holds text Claude
// persisted instead of inlining it. Claude writes .txt; .json and .md are
// accepted for robustness.
func isTextResult(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".txt", ".json", ".md", ".log":
		return true
	}
	return false
}

// isUUID matches the 8-4-4-4-12 hex form of session directory names, so
// project-level directories such as memory/ are not taken for sessions.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
				return false
			}
		}
	}
	return true
}
