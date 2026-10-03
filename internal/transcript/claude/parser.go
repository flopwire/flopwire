// Package claude parses Claude Code transcripts (~/.claude/projects) into
// the transcript model: discovery of sessions, subagents and companion
// files, and an incremental JSONL parser (spec §4, §4.3, §5.3, §5.4).
//
// Row rules. Every physical line yields zero or more rows, one per content
// block worth indexing:
//
//   - user: text blocks → user; tool_result blocks → tool_result
//     (tool_call_id, is_error, tool name from the pending call); image and
//     document blocks are skipped. isMeta lines are skipped whole.
//   - user with isCompactSummary → one system row (subtype compact_summary),
//     indexed once per distinct text within a source.
//   - assistant: text → assistant; thinking → thinking (text only, the
//     signature is dropped); tool_use → tool_call (compact JSON input);
//     redacted_thinking and unknown blocks are skipped.
//   - system: compact_boundary, away_summary, local_command,
//     informational, scheduled_task_fire and model_refusal_* → system;
//     other subtypes (turn_duration, stop_hook_summary, api_error, ...) are
//     skipped.
//   - attachment: queued_command prompts typed by a person (FAD 0.3.1
//     rule) → user; hook_additional_context (what hooks added to the
//     model's context) → injected, marked transcript.EnrichHookContext.
//     Everything else is skipped.
//   - ai-title sets the conversation title. Every other type, known
//     (mode, permission-mode, last-prompt, queue-operation, file-history-*,
//     atis-latch, bridge-session, pr-link, agent-name, artifact-*,
//     frame-link, cost-state, ...) or unknown, is skipped. Malformed lines
//     are skipped.
//
// Native ids. Every row's id is "<uuid>#<i>", where i is the index of the
// source block in message.content (0 for records without content blocks),
// and its Part is i too (see NativeID). Its ordinal is OrdinalAt(line
// offset, i). A line's bytes never change, so ids and ordinals are fixed by
// the file alone: a parser version that starts or stops emitting a sibling
// block never renames a row.
//
// Parent. parent_native_id is the parent line's uuid (parentUuid, or
// logicalParentUuid for compact_boundary lines whose parentUuid is null):
// a line id, never a row id, since the parser cannot know which blocks of
// an earlier line produced rows. Resolve it to a row with ParentRow: the
// row of that line with the greatest ordinal (its last block) in the same
// conversation, i.e. native_id LIKE parent || '#%' ORDER BY ordinal DESC
// LIMIT 1. A parent line that yielded no rows (a skipped record type)
// resolves to nothing.
//
// Injected context. Text the harness puts into a user turn (a
// <system-reminder> block: CLAUDE.md, hook output, environment notes; a
// <task-notification> block: background task results) is kind injected,
// never user. A text block that mixes a prompt with such
// blocks yields a user row with the prompt and an injected row
// "<uuid>#<i>:injected" (Part i|InjectedSlot) with the injected blocks.
// Lines sharing message.id are never merged; message_id, request_id and
// (on the first row of a line) usage are kept in Enrichment so token totals
// can be deduplicated (see Usage).
package claude

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/flopwire/flopwire/internal/transcript"
)

// ParserName is recorded on every row and source. Bump the major when rows change and need re-derivation.
// Minor bumps preserve existing rows.
//
// claude@2: ids always "<uuid>#<block>", part and ordinal slot = block
// index; <system-reminder> and <task-notification> text in user turns is
// kind injected; oversized
// lines reserve their size from the LineBudget.
// claude@3: the conversation lists every working directory the session
// recorded (OtherCwds), for path rules.
// claude@4: rows unchanged; bumped so every source is re-parsed and its
// digest re-folded with the commit evidence of issue #80.
const ParserName = "claude@4.0"

// stateVersion is the Cursor.State format. A state of any other version,
// or one that does not decode, restarts the parse from the beginning of the
// file (see Parse).
const stateVersion = 2

// InjectedSlot is added to the block index for the part and ordinal slot of
// an injected row split out of a user text block.
const InjectedSlot = 1 << 11

const (
	// DefaultMaxPersisted bounds how much of a tool-results/ file is read
	// for one tool_result row (agentsview uses the same 16MiB bound).
	DefaultMaxPersisted = 16 << 20
	// maxOversized bounds an oversized line read whole for decoding.
	maxOversized = 256 << 20
	titleRunes   = TitleRunes
)

// TitleRunes is where a title taken from the first prompt's first line is
// cut (local redactions mask titles cut there).
const TitleRunes = 100

// Stats counts what a parser skipped. Safe for concurrent use.
type Stats struct {
	Lines       atomic.Int64
	Malformed   atomic.Int64
	Oversized   atomic.Int64 // lines above LineReaderOptions.MaxLine (decoded anyway up to 256MiB)
	TooLarge    atomic.Int64 // lines above 256MiB, skipped
	Persisted   atomic.Int64 // tool_result rows filled from tool-results/
	PersistMiss atomic.Int64 // persisted-output previews whose file is gone
	StateResets atomic.Int64 // unreadable cursor states that restarted a parse
}

// Parser is the Claude Code JSONL parser. The zero value is ready to use,
// and one Parser may parse many sources concurrently.
type Parser struct {
	Caps         map[transcript.Kind]transcript.CapConfig // nil: transcript.DefaultCaps
	MaxPersisted int64                                    // 0: DefaultMaxPersisted
	Lines        transcript.LineReaderOptions
	Stats        *Stats // optional
	// FS reads companion files and parent transcripts; nil is the local
	// file system.
	FS FS

	spawn sync.Map // parent transcript path → *spawnIndex
}

var _ transcript.Parser = (*Parser)(nil)

func (p *Parser) Name() string                        { return ParserName }
func (p *Parser) Agent() transcript.Agent             { return transcript.AgentClaude }
func (p *Parser) StorageKind() transcript.StorageKind { return transcript.StorageJSONLAppend }

// Parse implements transcript.Parser. in.Source.Path locates the session
// directory (for tool-results/ and meta.json); without a path, companions
// are not consulted.
//
// Conversation records are upserts: Parse emits the conversation before
// its first message if it was never emitted, and again at the end of the
// call whenever it changed (title, activity bounds, subagent link). A
// resumed parse therefore emits the same messages as a full parse, and its
// last conversation record equals the full parse's last one.
//
// A cursor whose State does not decode, or has another stateVersion, is
// treated as the start of the file: the parse starts over at offset 0 and
// re-emits every row, which the store upserts by native id. A source is
// never left stuck behind a state it cannot read (P3).
func (p *Parser) Parse(ctx context.Context, in transcript.Input, cur transcript.Cursor, sink transcript.Sink) (transcript.Cursor, error) {
	return p.parse(ctx, in, cur, sink, nil)
}

func (p *Parser) ExtractionContract() string {
	limit := p.MaxPersisted
	if limit <= 0 {
		limit = DefaultMaxPersisted
	}
	return transcript.Contract(p.Name()+"/companions-v2", p.Caps, maxOversized, limit)
}

func (p *Parser) ParseWithReport(ctx context.Context, in transcript.Input, cur transcript.Cursor, sink transcript.Sink) (transcript.ParseResult, error) {
	var diagnostics transcript.Diagnostics
	next, err := p.parse(ctx, in, cur, sink, &diagnostics)
	if err != nil {
		return transcript.ParseResult{}, err
	}
	report := diagnostics.Report()
	return transcript.ParseResult{FromOffset: diagnostics.FromOffset, Cursor: next, Report: &report}, nil
}

func (p *Parser) parse(ctx context.Context, in transcript.Input, cur transcript.Cursor, sink transcript.Sink, diagnostics *transcript.Diagnostics) (transcript.Cursor, error) {
	st := state{V: stateVersion}
	if len(cur.State) > 0 || !cur.IsStart() {
		st = state{}
		if err := json.Unmarshal(cur.State, &st); err != nil || st.V != stateVersion {
			if p.Stats != nil {
				p.Stats.StateResets.Add(1)
			}
			st, cur = state{V: stateVersion}, transcript.Cursor{}
		}
	}
	if diagnostics != nil {
		diagnostics.FromOffset = cur.Offset
	}
	path := ""
	if in.Source != nil {
		path = in.Source.Path
	}
	run := &run{p: p, sink: sink, loc: locate(path), st: &st, diagnostics: diagnostics}
	run.init()

	next, err := transcript.ScanJSONL(ctx, in, cur, p.Lines, run.line)
	if err != nil {
		return cur, err
	}
	run.resolveSpawn()
	if err := run.emitConversation(); err != nil {
		return cur, err
	}
	next.State, err = json.Marshal(&st)
	return next, err
}

// state is carried between Parse calls in Cursor.State.
type state struct {
	V       int               `json:"v"`
	Conv    convState         `json:"c"`
	Emitted *convState        `json:"e,omitempty"`
	Pending map[string]string `json:"p,omitempty"` // open tool_use id → tool name
	Summary []string          `json:"s,omitempty"` // compaction summaries seen (sha256 prefix)
	// ParentSize is the parent transcript size at the last failed
	// spawned-by fallback scan; the scan reruns only when it grows.
	ParentSize int64 `json:"ps,omitempty"`
}

type convState struct {
	SessionID string `json:"id"`
	Cwd       string `json:"cwd,omitempty"`
	Cwds      string `json:"cwds,omitempty"` // other cwds, transcript.AddCwd
	Branch    string `json:"br,omitempty"`
	// Branches is every git branch the lines name, first seen first,
	// newline-separated (a string keeps convState comparable).
	Branches string `json:"bs,omitempty"`
	// Current is the branch the latest line names.
	Current     string `json:"cb,omitempty"`
	AITitle     string `json:"t,omitempty"`
	FirstPrompt string `json:"fp,omitempty"`
	Started     int64  `json:"s,omitempty"` // unix ns
	Last        int64  `json:"l,omitempty"`

	AgentID     string `json:"a,omitempty"`
	RunID       string `json:"r,omitempty"`
	AgentType   string `json:"at,omitempty"`
	Description string `json:"d,omitempty"`
	Parent      string `json:"pa,omitempty"`
	SpawnedBy   string `json:"sb,omitempty"`
	Depth       int    `json:"dp,omitempty"`
}

// location is what the source path says about a file.
type location struct {
	path       string
	sessionID  string // main session id (directory or file stem)
	sessionDir string // <project>/<session>
	agentID    string // subagent files only
	runID      string // workflow subagents only
}

func (l location) subagent() bool { return l.agentID != "" }

func locate(path string) location {
	if path == "" {
		return location{}
	}
	clean := filepath.Clean(path)
	sep := string(filepath.Separator)
	if i := strings.LastIndex(clean, sep+"subagents"+sep); i > 0 {
		loc := location{path: clean, sessionDir: clean[:i]}
		loc.sessionID = filepath.Base(loc.sessionDir)
		name := filepath.Base(clean)
		loc.agentID = strings.TrimSuffix(strings.TrimPrefix(name, "agent-"), ".jsonl")
		rest := strings.Split(clean[i+len(sep+"subagents"+sep):], sep)
		if len(rest) >= 3 && rest[0] == "workflows" {
			loc.runID = rest[1]
		}
		return loc
	}
	dir := strings.TrimSuffix(clean, ".jsonl")
	return location{path: clean, sessionID: filepath.Base(dir), sessionDir: dir}
}

type run struct {
	typeMismatch bool
	diagnostics  *transcript.Diagnostics
	p            *Parser
	sink         transcript.Sink
	loc          location
	st           *state
	rows         []*transcript.Message
	slots        []int  // per row: block index in message.content, plus InjectedSlot for a split
	buf          []byte // oversized line buffer
}

func (r *run) caps(k transcript.Kind) transcript.CapConfig {
	if r.p.Caps != nil {
		return transcript.CapFor(r.p.Caps, k)
	}
	return transcript.CapFor(transcript.DefaultCaps, k)
}

func (r *run) init() {
	c := &r.st.Conv
	if c.SessionID == "" {
		switch {
		case r.loc.subagent():
			c.SessionID = SubagentSessionID(r.loc.agentID)
		default:
			c.SessionID = r.loc.sessionID // "" without a path: set from the first line
		}
	}
	if r.loc.subagent() {
		c.AgentID, c.RunID = r.loc.agentID, r.loc.runID
		r.readMeta()
	}
}

// readMeta applies agent-<id>.meta.json. It is re-read on every call: it is
// tiny, and a live subagent may gain it after its first lines.
func (r *run) readMeta() {
	c := &r.st.Conv
	if c.Parent == "" {
		c.Parent = r.loc.sessionID
	}
	if c.Depth == 0 {
		c.Depth = 1
	}
	b, err := readFile(r.p.fs(), strings.TrimSuffix(r.loc.path, ".jsonl")+".meta.json", 1<<20)
	if err != nil {
		return
	}
	var m struct {
		AgentType     string `json:"agentType"`
		Description   string `json:"description"`
		ToolUseID     string `json:"toolUseId"`
		SpawnDepth    int    `json:"spawnDepth"`
		ParentAgentID string `json:"parentAgentId"`
	}
	if json.Unmarshal(b, &m) != nil {
		return
	}
	c.AgentType, c.Description = m.AgentType, m.Description
	if m.ToolUseID != "" {
		c.SpawnedBy = m.ToolUseID
	}
	if m.ParentAgentID != "" {
		c.Parent = SubagentSessionID(m.ParentAgentID)
		c.Depth = 2
	}
	if m.SpawnDepth > 0 {
		c.Depth = m.SpawnDepth
	}
}

func (r *run) line(lr *transcript.LineReader, l *transcript.Line) error {
	if r.p.Stats != nil {
		r.p.Stats.Lines.Add(1)
	}
	data := l.Data
	if l.Oversized {
		if r.p.Stats != nil {
			r.p.Stats.Oversized.Add(1)
		}
		if l.ContentLen > maxOversized {
			r.diagnostics.Record(transcript.RecordTooLarge, l.No, l.Offset)
			if r.p.Stats != nil {
				r.p.Stats.TooLarge.Add(1)
			}
			return nil
		}
		buf, err := lr.Materialize(l, r.buf)
		if err != nil {
			return err
		}
		r.buf, data = buf, buf
	}
	data = bytes.TrimPrefix(bytes.TrimSpace(data), []byte("\xef\xbb\xbf"))
	if len(data) == 0 {
		return nil
	}
	var rec record
	r.typeMismatch = false
	if err := decodeRecordChecked(data, &rec, r.typeFlag()); err != nil {
		r.diagnostics.Record(transcript.MalformedRecord, l.No, l.Offset)
		if r.p.Stats != nil {
			r.p.Stats.Malformed.Add(1)
		}
		return nil
	}
	if !knownRecordType(rec.Type) {
		r.diagnostics.Record(transcript.UnknownRecordType, l.No, l.Offset)
	}
	r.rows, r.slots = r.rows[:0], r.slots[:0]
	r.extract(&rec)
	if r.typeMismatch {
		r.diagnostics.Record(transcript.FieldTypeMismatch, l.No, l.Offset)
	}
	if len(r.rows) == 0 {
		return nil
	}
	// A directory the session names for the first time is emitted before
	// the rows of its line, so path rules see it before they are stored.
	if r.st.Emitted == nil || r.st.Emitted.Cwds != r.st.Conv.Cwds {
		if err := r.emitConversation(); err != nil {
			return err
		}
	}
	for i, m := range r.rows {
		slot := r.slots[i]
		m.Part = slot
		m.Ordinal = transcript.OrdinalAt(l.Offset, slot)
		m.LineNo, m.ByteOffset, m.ByteLen = l.No, l.Offset, l.Len
		m.Parser = ParserName
		m.SessionID = r.st.Conv.SessionID
		if rec.UUID != "" {
			m.NativeID = NativeID(rec.UUID, slot)
		}
		if len(m.Enrichment) == 0 {
			m.Enrichment = nil
		}
		if !m.TS.IsZero() {
			r.touch(m.TS)
		}
		if truncated, _ := m.Enrichment["text_truncated"].(bool); truncated {
			r.diagnostics.Record(transcript.TextTruncated, l.No, l.Offset)
		}
		if err := r.sink.Message(m); err != nil {
			return err
		}
	}
	r.rows, r.slots = r.rows[:0], r.slots[:0]
	return nil
}

// NativeID is the id of the row a line yields for one slot: the line uuid,
// "#", and the index of the source block in message.content, with
// ":injected" for injected text split out of that block.
func NativeID(uuid string, slot int) string {
	id := uuid + "#" + strconv.Itoa(slot&^InjectedSlot)
	if slot&InjectedSlot != 0 {
		id += ":injected"
	}
	return id
}

// LineID returns the line uuid of a row id ("<uuid>#<i>[:injected]").
func LineID(nativeID string) string {
	if i := strings.LastIndexByte(nativeID, '#'); i >= 0 {
		return nativeID[:i]
	}
	return nativeID
}

// ParentRow resolves a row's parent_native_id to the row it points at
// among rows (one conversation's): the parent line's row with the greatest
// ordinal. It returns nil when the parent line yielded no rows.
func ParentRow(parent string, rows []*transcript.Message) *transcript.Message {
	var best *transcript.Message
	if parent == "" {
		return nil
	}
	for _, m := range rows {
		if LineID(m.NativeID) == parent && (best == nil || m.Ordinal > best.Ordinal) {
			best = m
		}
	}
	return best
}

func (r *run) touch(ts time.Time) {
	c := &r.st.Conv
	n := ts.UnixNano()
	if c.Started == 0 || n < c.Started {
		c.Started = n
	}
	if n > c.Last {
		c.Last = n
	}
}

// extract appends the rows of one record to r.rows.
func (r *run) extract(rec *record) {
	c := &r.st.Conv
	if c.SessionID == "" && rec.SessionID != "" {
		c.SessionID = rec.SessionID
	}
	switch rec.Type {
	case "user", "assistant", "system", "attachment":
		if c.Cwd == "" && rec.Cwd != "" {
			c.Cwd = rec.Cwd
		} else if rec.Cwd != c.Cwd {
			transcript.AddCwd(&c.Cwds, c.Cwd, rec.Cwd)
		}
		if c.Branch == "" && rec.GitBranch != "" {
			c.Branch = rec.GitBranch
		}
		c.addBranch(rec.GitBranch)
	case "ai-title":
		if t := strings.TrimSpace(rec.AITitle); t != "" {
			c.AITitle = t
		}
		return
	default:
		return
	}

	ts := parseTime(rec.Timestamp)
	parent := rec.ParentUUID
	newRow := func(kind transcript.Kind, role string, block int) *transcript.Message {
		m := &transcript.Message{Kind: kind, Role: role, TS: ts, ParentNativeID: parent, Enrichment: map[string]any{}}
		r.rows = append(r.rows, m)
		r.slots = append(r.slots, block)
		return m
	}

	switch rec.Type {
	case "user":
		if rec.IsMeta || rec.Message == nil {
			return
		}
		if rec.IsCompactSummary {
			r.compactSummary(rec, newRow)
			return
		}
		r.user(rec, newRow)
	case "assistant":
		if rec.Message == nil {
			return
		}
		r.assistant(rec, newRow)
	case "system":
		r.system(rec, newRow)
	case "attachment":
		r.queuedCommand(rec, ts, newRow)
		r.hookContext(rec, ts, newRow)
	}
}

type rowFunc func(kind transcript.Kind, role string, block int) *transcript.Message

func (r *run) user(rec *record, newRow rowFunc) {
	blocks := contentBlocks(rec.Message.Content, r.typeFlag())
	nResults := 0
	for _, b := range blocks {
		if b.Type == "tool_result" {
			nResults++
		}
	}
	for i, b := range blocks {
		switch b.Type {
		case "text":
			r.userText(b.Text, i, newRow)
		case "tool_result":
			m := newRow(transcript.KindToolResult, "user", i)
			m.ToolCallID, m.IsError = b.ToolUseID, b.IsError
			if name, ok := r.st.Pending[b.ToolUseID]; ok {
				m.ToolName = name
				delete(r.st.Pending, b.ToolUseID)
			}
			text := flattenText(b.Content, r.typeFlag())
			if nResults == 1 {
				if rec.ToolUseResult.AgentID != "" {
					m.Enrichment["agent_id"] = rec.ToolUseResult.AgentID
				}
				if rec.ToolUseResult.RunID != "" {
					m.Enrichment["workflow_run_id"] = rec.ToolUseResult.RunID
				}
			}
			if full, ref, ok, truncated := r.persisted(text, rec.ToolUseResult.PersistedOutputPath, nResults); ok {
				text = full
				m.Enrichment["persisted_output"] = ref
				if truncated {
					m.Enrichment["persisted_output_truncated"] = true
				}
			} else if ref != "" {
				m.Enrichment["persisted_output_missing"] = ref
			}
			m.SetText(text, r.caps(transcript.KindToolResult))
		}
	}
}

func (r *run) compactSummary(rec *record, newRow rowFunc) {
	text := flattenText(rec.Message.Content, r.typeFlag())
	if strings.TrimSpace(text) == "" {
		return
	}
	sum := sha256.Sum256([]byte(text))
	key := hex.EncodeToString(sum[:12])
	for _, s := range r.st.Summary {
		if s == key {
			return // indexed once per source
		}
	}
	r.st.Summary = append(r.st.Summary, key)
	m := newRow(transcript.KindSystem, "system", 0)
	m.Enrichment["subtype"] = "compact_summary"
	m.SetText(text, r.caps(transcript.KindSystem))
}

func (r *run) assistant(rec *record, newRow rowFunc) {
	first := len(r.rows)
	for i, b := range contentBlocks(rec.Message.Content, r.typeFlag()) {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			m := newRow(transcript.KindAssistant, "assistant", i)
			m.SetText(b.Text, r.caps(transcript.KindAssistant))
		case "thinking":
			if strings.TrimSpace(b.Thinking) == "" {
				continue
			}
			m := newRow(transcript.KindThinking, "assistant", i)
			m.SetText(b.Thinking, r.caps(transcript.KindThinking))
		case "tool_use":
			m := newRow(transcript.KindToolCall, "assistant", i)
			m.ToolName, m.ToolCallID = b.Name, b.ID
			m.SetText(compactJSON(b.Input), r.caps(transcript.KindToolCall))
			if b.ID != "" {
				if r.st.Pending == nil {
					r.st.Pending = map[string]string{}
				}
				r.st.Pending[b.ID] = b.Name
			}
		}
	}
	for j, m := range r.rows[first:] {
		e := m.Enrichment
		if rec.Message.ID != "" {
			e["message_id"] = rec.Message.ID
		}
		if rec.RequestID != "" {
			e["request_id"] = rec.RequestID
		}
		if rec.Message.Model != "" {
			e["model"] = rec.Message.Model
		}
		if rec.IsAPIError {
			e["api_error"] = true
		}
		if j == 0 && rec.Message.Usage != nil {
			u := rec.Message.Usage
			e["usage"] = map[string]int64{
				"input_tokens":                u.InputTokens,
				"output_tokens":               u.OutputTokens,
				"cache_creation_input_tokens": u.CacheCreationInputTokens,
				"cache_read_input_tokens":     u.CacheReadInputTokens,
			}
		}
	}
}

// keptSystem are the system subtypes that carry conversation text.
var keptSystem = map[string]bool{
	"compact_boundary":          true,
	"away_summary":              true,
	"local_command":             true,
	"informational":             true,
	"scheduled_task_fire":       true,
	"model_refusal_fallback":    true,
	"model_refusal_no_fallback": true,
}

func (r *run) system(rec *record, newRow rowFunc) {
	if !keptSystem[rec.Subtype] || rec.IsMeta {
		return
	}
	var text string
	if len(rec.Content) > 0 && rec.Content[0] == '"' {
		_ = json.Unmarshal(rec.Content, &text)
	}
	if strings.TrimSpace(text) == "" && rec.Subtype != "compact_boundary" {
		return
	}
	m := newRow(transcript.KindSystem, "system", 0)
	if m.ParentNativeID == "" && rec.LogicalParentUUID != "" {
		m.ParentNativeID = rec.LogicalParentUUID
	}
	m.Enrichment["subtype"] = rec.Subtype
	m.SetText(text, r.caps(transcript.KindSystem))
}

// queuedCommand keeps a prompt the user typed while the agent was mid-turn.
// Rule from FAD 0.3.1 queued_command_message: prompt mode only, not meta,
// and origin absent (older builds) or origin.kind == "human".
func (r *run) queuedCommand(rec *record, ts time.Time, newRow rowFunc) {
	a := rec.Attachment
	if a == nil || a.Type != "queued_command" || a.CommandMode != "prompt" || rec.IsMeta || a.IsMeta {
		return
	}
	authorship := "unknown"
	if !a.Origin.isNull() {
		var o struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(a.Origin, &o) != nil || o.Kind != "human" {
			return
		}
		authorship = "human"
	}
	for _, m := range r.userText(flattenText(a.Prompt, r.typeFlag()), 0, newRow) {
		if ts.IsZero() {
			m.TS = parseTime(a.Timestamp)
		}
		m.Enrichment["queued_command"] = authorship
	}
}

// hookContext keeps the context hooks added to the model's context (their
// additionalContext): an attachment of type hook_additional_context whose
// content holds one string per hook. It is one injected row, marked with
// transcript.EnrichHookContext and the hook event, with the strings joined
// by blank lines. The hooks' raw output (hook_success and the other
// hook_* attachments) is not what the model saw and is skipped.
func (r *run) hookContext(rec *record, ts time.Time, newRow rowFunc) {
	a := rec.Attachment
	if a == nil || a.Type != "hook_additional_context" || rec.IsMeta {
		return
	}
	var parts []string
	if len(a.Content) > 0 && a.Content[0] == '[' {
		var all []json.RawMessage
		_ = json.Unmarshal(a.Content, &all)
		for _, v := range all {
			var s string
			if json.Unmarshal(v, &s) == nil && strings.TrimSpace(s) != "" {
				parts = append(parts, s)
			}
		}
	} else if len(a.Content) > 0 && a.Content[0] == '"' {
		var s string
		if json.Unmarshal(a.Content, &s) == nil && strings.TrimSpace(s) != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return
	}
	m := newRow(transcript.KindInjected, "system", 0)
	if ts.IsZero() {
		m.TS = parseTime(a.Timestamp)
	}
	m.Enrichment[transcript.EnrichHookContext] = a.HookEvent
	m.SetText(strings.Join(parts, "\n\n"), r.caps(transcript.KindInjected))
}

// userText adds the rows of one user text block: a user row for what the
// person typed and an injected row for <system-reminder> blocks the
// harness added (split into two rows when the block holds both).
func (r *run) userText(text string, block int, newRow rowFunc) []*transcript.Message {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	prompt, injected := splitInjected(text)
	var out []*transcript.Message
	switch {
	case injected == "":
		m := newRow(transcript.KindUser, "user", block)
		m.SetText(text, r.caps(transcript.KindUser))
		out = append(out, m)
	case prompt == "":
		m := newRow(transcript.KindInjected, "user", block)
		m.SetText(text, r.caps(transcript.KindInjected))
		return append(out, m)
	default:
		m := newRow(transcript.KindUser, "user", block)
		m.SetText(prompt, r.caps(transcript.KindUser))
		inj := newRow(transcript.KindInjected, "user", block|InjectedSlot)
		inj.SetText(injected, r.caps(transcript.KindInjected))
		out = append(out, m, inj)
	}
	if r.st.Conv.FirstPrompt == "" {
		r.st.Conv.FirstPrompt = firstLine(out[0].Text)
	}
	return out
}

func (r *run) emitConversation() error {
	c := r.st.Conv
	if c.SessionID == "" {
		return nil
	}
	if r.st.Emitted != nil && *r.st.Emitted == c {
		return nil
	}
	r.st.Emitted = &c
	return r.sink.Conversation(c.conversation())
}

// maxBranches bounds the branches a session records.
const maxBranches = 8

// addBranch records b as the current branch, and in Branches when the
// session has not been on it yet.
func (c *convState) addBranch(b string) {
	if b == "" {
		return
	}
	c.Current = b
	if c.Branches == b {
		return
	}
	list := strings.Split(c.Branches, "\n")
	if c.Branches == "" {
		list = nil
	}
	if len(list) >= maxBranches || slices.Contains(list, b) {
		return
	}
	c.Branches = strings.Join(append(list, b), "\n")
}

func (c convState) conversation() *transcript.Conversation {
	v := &transcript.Conversation{
		Agent:     transcript.AgentClaude,
		SessionID: c.SessionID,
		Cwd:       c.Cwd,
		OtherCwds: transcript.SplitCwds(c.Cwds),
		Title:     c.AITitle,
	}
	if v.Title == "" {
		v.Title = c.Description
	}
	if v.Title == "" {
		v.Title = c.FirstPrompt
	}
	if c.Branches != "" {
		v.Branches = strings.Split(c.Branches, "\n")
	} else if c.Branch != "" {
		v.Branches = []string{c.Branch}
	}
	// A session back on an earlier branch (a→b→a) ends the list with the
	// branch it is on now.
	if n := len(v.Branches); c.Current != "" && (n == 0 || v.Branches[n-1] != c.Current) {
		v.Branches = append(v.Branches, c.Current)
	}
	if c.Started != 0 {
		v.StartedAt = time.Unix(0, c.Started).UTC()
	}
	if c.Last != 0 {
		v.LastActivityAt = time.Unix(0, c.Last).UTC()
	}
	extra := map[string]any{}
	put := func(k, s string) {
		if s != "" {
			extra[k] = s
		}
	}
	put("git_branch", c.Branch)
	if c.AgentID != "" {
		v.ParentSessionID, v.SpawnedByToolCallID, v.Depth = c.Parent, c.SpawnedBy, c.Depth
		put("agent_id", c.AgentID)
		put("agent_type", c.AgentType)
		put("description", c.Description)
		put("workflow_run_id", c.RunID)
	}
	if len(extra) > 0 {
		v.Extra = extra
	}
	return v
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if utf8.RuneCountInString(s) > titleRunes {
		s = string([]rune(s)[:titleRunes])
	}
	return s
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// Known metadata is intentionally excluded. Future types are coverage observations.
func knownRecordType(typ string) bool {
	switch typ {
	case "user", "assistant", "system", "attachment", "ai-title", "mode", "permission-mode", "last-prompt", "queue-operation", "atis-latch", "bridge-session", "pr-link", "agent-name", "frame-link", "cost-state", "summary", "progress":
		return true
	}
	return strings.HasPrefix(typ, "file-history-") || strings.HasPrefix(typ, "artifact-")
}

func (r *run) typeFlag() *bool {
	if r.diagnostics != nil {
		return &r.typeMismatch
	}
	return nil
}
