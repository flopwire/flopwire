// Package transcript is the harness-neutral transcript model (spec §4 of
// notes/local-search/README.md) and the plumbing shared by every harness
// parser: the incremental Parser contract, a JSONL line reader with exact
// byte offsets, text capping, and append-versus-rewrite change detection.
//
// Harness parsers (Claude Code, Codex, Devin) live in sub-packages and emit
// Conversation and Message values into a Sink. Storage (SQLite locally,
// Postgres centrally) is not this package's concern.
package transcript

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"time"
)

// Agent names a harness: claude, codex, devin, ...
type Agent string

const (
	AgentClaude Agent = "claude"
	AgentCodex  Agent = "codex"
	AgentDevin  Agent = "devin"
)

// StorageKind is how a harness stores a source (spec §4.1, §5.3).
type StorageKind string

const (
	StorageJSONLAppend StorageKind = "jsonl_append"
	StorageJSONDoc     StorageKind = "json_doc"
	StorageSQLite      StorageKind = "sqlite"
	StorageDir         StorageKind = "dir"
	StorageMarkdown    StorageKind = "markdown"
	// StorageCompanion is a file archived beside a transcript and read by
	// its parser (Claude tool-results/*, agent-*.meta.json), never parsed
	// as a transcript itself.
	StorageCompanion StorageKind = "companion"
)

// FileID is a file's identity on its volume: device + inode on POSIX.
// The zero value means unknown (unsupported platform).
type FileID struct {
	Dev uint64
	Ino uint64
}

// String renders the identity as the sources.file_id column value.
func (id FileID) String() string { return fmt.Sprintf("%d:%d", id.Dev, id.Ino) }

// IsZero reports whether the identity is unknown.
func (id FileID) IsZero() bool { return id == FileID{} }

// Source is one file (or database, or directory) a parser reads.
type Source struct {
	Agent       Agent
	Path        string
	FileID      FileID
	SessionKey  string // session id read from content, for rewrite-prone formats
	StorageKind StorageKind
	Parser      string // parser name and version, e.g. "claude@1"
}

// Generation is one version of a source. A rewrite, truncation, or identity
// change starts a new generation; appends extend the current one.
type Generation struct {
	Generation int64
	Size       int64
	ChangeTime time.Time
	CapturedAt time.Time
	Complete   bool // false while the tail is provisional
}

// Conversation is one harness session. A source usually holds one; a
// SQLite source (Devin) holds many.
type Conversation struct {
	Agent     Agent
	SessionID string // harness-native session or thread id
	Cwd       string
	// OtherCwds are the other working directories the session named after
	// Cwd (Claude's per-record cwd, Codex turn_context cwd and <cwd> tags),
	// each once, in order of appearance. Path rules apply the most
	// restrictive verdict across Cwd and these (D18).
	OtherCwds      []string
	Title          string
	StartedAt      time.Time
	LastActivityAt time.Time
	// Branches are the git branches the session ran on, first seen first
	// (Claude records one per line; Codex one per session). A change of
	// branch mid-session adds an entry; the last entry is the current
	// branch (a→b→a is [a b a]).
	Branches []string

	// Subagent linkage by native ids. The index resolves them to row ids
	// when the parent arrives, in either order (spec §4.3).
	ParentSessionID     string
	SpawnedByToolCallID string // native id of the parent's spawning tool call
	Depth               int
	Extra               map[string]any // harness-specific conversation facts
}

// Kind classifies a message row.
type Kind uint8

const (
	KindUnknown Kind = iota
	KindUser
	KindAssistant
	KindToolCall
	KindToolResult
	KindThinking
	KindSystem
	// KindInjected is context the harness injects into a user turn rather
	// than text a person typed: CLAUDE.md / AGENTS.md instructions,
	// <system-reminder> blocks, <environment_context>. Readers hide it by
	// default; the string value "injected" is the contract.
	KindInjected
	// KindAgentMessage is a message one agent sends another (Codex
	// agent_message items). String value "agent_message".
	KindAgentMessage
)

var kindNames = [...]string{"unknown", "user", "assistant", "tool_call", "tool_result", "thinking", "system", "injected", "agent_message"}

func (k Kind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return fmt.Sprintf("kind(%d)", uint8(k))
}

// ParseKind is the inverse of Kind.String.
func ParseKind(s string) (Kind, error) {
	for i, n := range kindNames {
		if n == s {
			return Kind(i), nil
		}
	}
	return KindUnknown, fmt.Errorf("transcript: unknown kind %q", s)
}

func (k Kind) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

func (k *Kind) UnmarshalText(b []byte) error {
	v, err := ParseKind(string(b))
	*k = v
	return err
}

// Message is one extracted record: a prompt, a reply, a tool call, a tool
// result, a thinking block, or a system record. One physical JSONL line may
// yield several messages; Part numbers them within the line.
type Message struct {
	SessionID      string // the Conversation this row belongs to
	NativeID       string // Claude uuid, Codex payload id, Devin message_id, ...
	ParentNativeID string // Claude: the parent line's uuid, see claude.ParentRow
	Ordinal        int64  // see OrdinalAt; ordering only, never a key
	Part           int    // index of this message within its physical record
	Kind           Kind
	Role           string
	ToolName       string
	ToolCallID     string
	IsError        bool
	TS             time.Time

	Text       string   // extracted text after capping
	FullLen    int      // length in bytes of the uncapped text
	ContentSHA [32]byte // sha256 of the uncapped text

	// Locator. JSONL parsers set LineNo/ByteOffset/ByteLen for the physical
	// line; other storage kinds set Locator (rowid, JSON pointer, ...).
	LineNo     int64
	ByteOffset int64
	ByteLen    int64
	Locator    string

	Parser       string
	Superseded   bool
	OnActivePath *bool // set only from a harness's own pointer; nil otherwise

	// Enrichment holds best-effort filter fields (commands, changed paths,
	// exit codes, ...). Absent fields degrade filtering only.
	Enrichment map[string]any
}

// SetText stores text capped by cfg and records the uncapped length and
// hash, so a change in the full text is detectable even when the capped
// text is identical.
func (m *Message) SetText(full string, cfg CapConfig) {
	m.FullLen = len(full)
	m.ContentSHA = sha256.Sum256([]byte(full))
	var cut bool
	m.Text, cut = Cap(full, cfg)
	if cut {
		if m.Enrichment == nil {
			m.Enrichment = map[string]any{}
		}
		m.Enrichment["text_truncated"] = true
	} else {
		delete(m.Enrichment, "text_truncated")
	}
}

// OrdinalAt derives a message ordinal from its locator: the byte offset (or
// rowid, or array index) at first sight, times 4096, plus a slot within the
// record. Parsers choose slots that are fixed per row (Claude: the content
// block index), so an ordinal never depends on which sibling rows a parser
// version emits. Slots beyond 4095 saturate; ordering among them then falls
// back to the order of emission.
func OrdinalAt(locator int64, part int) int64 {
	if part > 4095 {
		part = 4095
	}
	return locator<<12 | int64(part)
}

// BoolPtr returns a pointer to b, for OnActivePath.
func BoolPtr(b bool) *bool { return &b }

// AddCwd appends cwd to the newline-separated set of working directories
// set unless it is empty, equal to first or already present, and reports
// whether it did. Parsers keep a session's OtherCwds this way in their
// cursor state.
func AddCwd(set *string, first, cwd string) bool {
	if cwd == "" || cwd == first || strings.ContainsRune(cwd, '\n') {
		return false
	}
	for rest := *set; rest != ""; {
		var l string
		l, rest, _ = strings.Cut(rest, "\n")
		if l == cwd {
			return false
		}
	}
	if *set == "" {
		*set = cwd
	} else {
		*set += "\n" + cwd
	}
	return true
}

// SplitCwds is the list a set kept by AddCwd holds.
func SplitCwds(set string) []string {
	if set == "" {
		return nil
	}
	return strings.Split(set, "\n")
}
