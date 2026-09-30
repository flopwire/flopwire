// Package oracle loads expected parser output produced by reference
// implementations and diffs a transcript.Parser's output against it, with
// documented per-agent divergence rules. Test-time only.
//
// The first oracle is franken_agent_detection 0.3.1 via tools/fad-dump
// (scripts/regen-oracle.sh writes testdata/oracle/expected/*.json). The
// comparison approach is adapted from gbasin/agentboard
// src/server/__tests__/fadParity.test.ts (MIT) at commit
// 4fb640dd9b539413d2ba2fdcf5385a7b51478797.
package oracle

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

// File is one expected file: every conversation FAD emitted for one source.
type File struct {
	Name          string         `json:"-"`
	Connector     string         `json:"connector"`
	SourcePath    string         `json:"source_path"` // relative to the fixture home
	Conversations []Conversation `json:"conversations"`
}

// Conversation mirrors FAD's NormalizedConversation. Strings that held the
// fixture home path read "$HOME".
type Conversation struct {
	AgentSlug  string          `json:"agent_slug"`
	ExternalID *string         `json:"external_id"`
	Title      *string         `json:"title"`
	Workspace  *string         `json:"workspace"`
	SourcePath string          `json:"source_path"`
	StartedAt  *int64          `json:"started_at"`
	EndedAt    *int64          `json:"ended_at"`
	Metadata   json.RawMessage `json:"metadata"`
	Messages   []Message       `json:"messages"`
}

// Message mirrors FAD's NormalizedMessage.
type Message struct {
	Idx         int64           `json:"idx"`
	Role        string          `json:"role"` // user | assistant | tool | system
	Author      *string         `json:"author"`
	CreatedAt   *int64          `json:"created_at"` // epoch ms
	Content     string          `json:"content"`
	Extra       json.RawMessage `json:"extra"`
	Snippets    json.RawMessage `json:"snippets"`
	Invocations []Invocation    `json:"invocations"`
}

// Invocation mirrors FAD's NormalizedInvocation.
type Invocation struct {
	Kind      string          `json:"kind"`
	Name      string          `json:"name"`
	RawName   *string         `json:"raw_name"`
	CallID    *string         `json:"call_id"`
	Arguments json.RawMessage `json:"arguments"`
}

// Load reads every expected file in dir, sorted by name.
func Load(dir string) ([]File, error) {
	names, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	files := make([]File, 0, len(names))
	for _, name := range names {
		b, err := os.ReadFile(name)
		if err != nil {
			return nil, err
		}
		var f File
		if err := json.Unmarshal(b, &f); err != nil {
			return nil, fmt.Errorf("oracle: %s: %w", name, err)
		}
		f.Name = filepath.Base(name)
		files = append(files, f)
	}
	return files, nil
}

// ForConnector filters files to one FAD connector slug.
func ForConnector(files []File, connector string) []File {
	var out []File
	for _, f := range files {
		if f.Connector == connector {
			out = append(out, f)
		}
	}
	return out
}

// RealPath resolves a File's source path under home. FAD gives
// multi-conversation stores a virtual path (<sessions.db>/<session id>), so
// it walks up to the first path that exists.
func RealPath(home string, f File) string {
	p := filepath.Join(home, filepath.FromSlash(f.SourcePath))
	for p != home && p != filepath.Dir(p) {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		p = filepath.Dir(p)
	}
	return p
}

// SessionIDs lists the ids a FAD conversation can be matched by: its
// external id and the session id keys connectors put in metadata.
func (c Conversation) SessionIDs() []string {
	var ids []string
	if c.ExternalID != nil {
		ids = append(ids, *c.ExternalID)
	}
	var meta map[string]any
	_ = json.Unmarshal(c.Metadata, &meta)
	for _, k := range []string{"sessionId", "session_id", "id"} {
		if s, ok := meta[k].(string); ok && s != "" {
			ids = append(ids, s)
		}
	}
	return ids
}

// Matches reports whether sessionID identifies c. FAD derives external ids
// from paths for file agents, so containment counts.
func (c Conversation) Matches(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	for _, id := range c.SessionIDs() {
		if id == sessionID || strings.Contains(id, sessionID) {
			return true
		}
	}
	return false
}

// Row is the comparable projection of one message on either side.
type Row struct {
	Role      string
	Content   string
	CreatedAt int64 // epoch ms; 0 when unknown
}

func (r Row) String() string {
	c := r.Content
	if len(c) > 80 {
		c = c[:80] + "…"
	}
	return fmt.Sprintf("%s@%d %q", r.Role, r.CreatedAt, c)
}

// Divergence is a documented, named difference between our parser and the
// oracle. Drop functions remove rows from one side before comparison.
type Divergence struct {
	Name     string
	DropFAD  func(Message) bool
	DropOurs func(*transcript.Message) bool
}

// Rules configure the comparison for one agent.
type Rules struct {
	// Project maps one of our messages to comparable rows. Default:
	// DefaultProject.
	Project func(*transcript.Message) []Row
	// Normalize is applied to Content on both sides. Default: collapse
	// whitespace.
	Normalize func(string) string
	// IgnoreTimes skips CreatedAt comparison.
	IgnoreTimes bool
	Divergences []Divergence
}

// DefaultProject maps kinds to FAD roles. FAD folds tool calls into the
// assistant message text plus invocations and drops thinking, so those
// kinds project to nothing unless an agent's Rules say otherwise.
func DefaultProject(m *transcript.Message) []Row {
	role := map[transcript.Kind]string{
		transcript.KindUser:       "user",
		transcript.KindAssistant:  "assistant",
		transcript.KindToolResult: "tool",
		transcript.KindSystem:     "system",
		// FAD keeps harness-injected context and inter-agent messages as
		// user messages; Flopwire splits them into their own kinds.
		transcript.KindInjected:     "user",
		transcript.KindAgentMessage: "user",
	}[m.Kind]
	if role == "" {
		return nil
	}
	var ts int64
	if !m.TS.IsZero() {
		ts = m.TS.UnixMilli()
	}
	return []Row{{Role: role, Content: m.Text, CreatedAt: ts}}
}

// CollapseSpace trims and collapses runs of whitespace.
func CollapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// Diff compares our conversation and messages with a FAD conversation and
// returns human-readable problems; empty means parity.
func Diff(exp Conversation, conv *transcript.Conversation, msgs []*transcript.Message, rules Rules) []string {
	return Compare(exp, conv, msgs, rules).Problems
}

// Comparison is Diff's result with the first differing rows kept whole.
type Comparison struct {
	Problems   []string
	SessionID  bool // the session id did not match
	Cwd        bool // the cwd did not match the FAD workspace
	Row        int  // index of the first differing row, -1 when rows match
	FAD, Ours  Row  // the first differing rows (zero Row past a side's end)
	NFAD, NOur int  // rows compared on each side
}

// Compare is Diff, keeping the first differing rows for classification.
func Compare(exp Conversation, conv *transcript.Conversation, msgs []*transcript.Message, rules Rules) Comparison {
	project, norm := rules.Project, rules.Normalize
	if project == nil {
		project = DefaultProject
	}
	if norm == nil {
		norm = CollapseSpace
	}
	c := Comparison{Row: -1}
	if !exp.Matches(conv.SessionID) {
		c.SessionID = true
		c.Problems = append(c.Problems, fmt.Sprintf("session id %q not among FAD ids %q", conv.SessionID, exp.SessionIDs()))
	}
	if exp.Workspace != nil && *exp.Workspace != conv.Cwd {
		c.Cwd = true
		c.Problems = append(c.Problems, fmt.Sprintf("cwd %q, FAD workspace %q", conv.Cwd, *exp.Workspace))
	}

	var theirs, ours []Row
fad:
	for _, m := range exp.Messages {
		for _, d := range rules.Divergences {
			if d.DropFAD != nil && d.DropFAD(m) {
				continue fad
			}
		}
		r := Row{Role: m.Role, Content: norm(m.Content)}
		if m.CreatedAt != nil {
			r.CreatedAt = *m.CreatedAt
		}
		theirs = append(theirs, r)
	}
own:
	for _, m := range msgs {
		for _, d := range rules.Divergences {
			if d.DropOurs != nil && d.DropOurs(m) {
				continue own
			}
		}
		for _, r := range project(m) {
			r.Content = norm(r.Content)
			ours = append(ours, r)
		}
	}
	if rules.IgnoreTimes {
		for i := range theirs {
			theirs[i].CreatedAt = 0
		}
		for i := range ours {
			ours[i].CreatedAt = 0
		}
	}

	c.NFAD, c.NOur = len(theirs), len(ours)
	n := max(len(theirs), len(ours))
	for i := 0; i < n; i++ {
		var a, b Row
		if i < len(theirs) {
			a = theirs[i]
		}
		if i < len(ours) {
			b = ours[i]
		}
		if a != b {
			c.Row, c.FAD, c.Ours = i, a, b
			c.Problems = append(c.Problems, fmt.Sprintf("row %d differs (FAD %d rows, ours %d)\n  FAD:  %v\n  ours: %v", i, len(theirs), len(ours), a, b))
			break
		}
	}
	return c
}

// Assert reports Diff's problems on t, prefixed with the expected file name.
func Assert(t testing.TB, f File, exp Conversation, conv *transcript.Conversation, msgs []*transcript.Message, rules Rules) {
	t.Helper()
	for _, p := range Diff(exp, conv, msgs, rules) {
		t.Errorf("%s: %s", f.Name, p)
	}
}
