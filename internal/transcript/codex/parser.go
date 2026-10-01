// Package codex parses Codex CLI rollouts (~/.codex/sessions and
// archived_sessions) into the transcript model (spec §4, §5.4).
//
// response_item records are canonical for every row. Format knowledge for
// them is ported by hand from franken-agent-detection 0.3.1
// src/connectors/codex.rs (git c06d1cb14e0edb1cd82f1c58021d86b6beacd2de,
// MIT, see third_party/fad): the tool-call and tool-output shapes, content
// flattening, the injected-context title rule and user prompt dedupe
// (prompts.go). Unlike FAD it keeps every payload.id as the native id, reads
// session_meta and subagent linkage (subagent.go), follows
// archived_sessions/, streams files of any size, and resumes from a cursor.
//
// event_msg/item_completed CommandExecution and FileChange records are read
// by the separate codex-events@1 sub-parser (events.go) as enrichment.
package codex

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

// Parser names. Rows from response_item carry RowParser; rows created by
// the enrichment sub-parser carry EventsVersion. Name is the source's
// parser version: bump the major in RowParser or EventsVersion when emitted
// rows change. Minor bumps preserve existing rows.
//
// codex@2: empty tool outputs are emitted (is_error from the exit code);
// kinds injected and agent_message; stringified JSON tool outputs are
// decoded; forks without a history marker skip the parent history they
// copied; minimal cursor state.
//
// codex@3: the conversation lists every working directory the session
// named (turn_context cwd, <cwd> tags) in OtherCwds, for path rules.
const (
	RowParser = "codex@3.0"
	Name      = RowParser + "+" + EventsVersion
)

// stateVersion is the Cursor.State format. A state of any other version,
// or one that does not decode, restarts the parse from the beginning of the
// file (see Parse).
const stateVersion = 2

// maxOversized bounds a line decoded whole; longer lines are skipped and
// counted in Stats.TooLarge.
const maxOversized = 256 << 20

const (
	kindUser         = transcript.KindUser
	kindAssistant    = transcript.KindAssistant
	kindToolCall     = transcript.KindToolCall
	kindToolResult   = transcript.KindToolResult
	kindThinking     = transcript.KindThinking
	kindSystem       = transcript.KindSystem
	kindInjected     = transcript.KindInjected
	kindAgentMessage = transcript.KindAgentMessage
)

// Stats counts what a parser saw. All fields are safe for concurrent use,
// so one Stats may be shared by parsers running in parallel.
type Stats struct {
	Lines, Malformed          atomic.Int64
	TypeErrors                atomic.Int64 // records kept with a field of unexpected type left zero
	InheritedSkipped          atomic.Int64 // replayed parent history below subagent_history_start_ordinal
	PromptDupes               atomic.Int64 // event_msg/user_message copies dropped
	EventsPaired, EventsLoose atomic.Int64 // codex-events@1 records attached to an open call / emitted as their own rows
	EventsLate                atomic.Int64 // attached to a closed call of the same turn by command text
	EventErrors               atomic.Int64 // enrichment records that did not decode
	CompactionSummaries       atomic.Int64
	CompactionReplaySkipped   atomic.Int64 // replacement_history items already seen
	CompactionReplayIndexed   atomic.Int64 // replacement_history items first seen in the compaction
	CallsReemitted            atomic.Int64
	ForkPrefixSkipped         atomic.Int64 // parent history a fork without a history marker copied (D11)
	StateResets               atomic.Int64 // unreadable cursor states that restarted a parse
	TooLarge                  atomic.Int64 // lines over 256MiB, skipped
}

// Parser is the Codex rollout parser. The zero value is ready to use.
type Parser struct {
	// Caps overrides transcript.DefaultCaps.
	Caps map[transcript.Kind]transcript.CapConfig
	// LineOptions tunes the line reader.
	LineOptions transcript.LineReaderOptions
	// Stats, when set, receives counters.
	Stats *Stats
	// OpenRollout opens the rollout of a session id, so a fork that copied
	// its parent's history without a history marker can skip it (D11).
	// nil looks under the Codex home that holds the child rollout
	// (FindRollout). A parent that is not there (an error wrapping
	// fs.ErrNotExist) leaves the copied history indexed; any other error
	// fails the parse, to be retried.
	OpenRollout func(childPath, sessionID string) (File, error)
}

// File is an open rollout.
type File interface {
	io.ReaderAt
	io.Closer
	Size() int64
}

var _ transcript.Parser = (*Parser)(nil)

func (p *Parser) Name() string                        { return Name }
func (p *Parser) Agent() transcript.Agent             { return transcript.AgentCodex }
func (p *Parser) StorageKind() transcript.StorageKind { return transcript.StorageJSONLAppend }

// state is carried across calls in Cursor.State.
type state struct {
	V       int         `json:"v"`
	Meta    meta        `json:"m"`
	Started bool        `json:"st,omitempty"` // conversation emitted
	Turn    string      `json:"turn,omitempty"`
	Prompt  *promptMark `json:"p,omitempty"`
	Open    []*openCall `json:"open,omitempty"`
	Closed  []*openCall `json:"closed,omitempty"` // recently closed calls, for late events
	Seen    []byte      `json:"seen,omitempty"`   // message keys already indexed: sorted little-endian uint64s
	Windows []string    `json:"win,omitempty"`    // compaction windows already summarized
	// ForkPrefix is true while a fork without a history marker is still
	// in the parent history it copied (D11).
	ForkPrefix bool `json:"fp,omitempty"`

	seen map[uint64]struct{}
}

// decodeState reads a cursor state. ok is false when b does not decode or
// has another stateVersion.
func decodeState(b []byte) (st *state, ok bool) {
	st = &state{V: stateVersion, seen: map[uint64]struct{}{}}
	if len(b) == 0 {
		return st, true
	}
	got := &state{}
	if err := json.Unmarshal(b, got); err != nil || got.V != stateVersion || len(got.Seen)%8 != 0 {
		return st, false
	}
	got.seen = make(map[uint64]struct{}, len(got.Seen)/8)
	for i := 0; i < len(got.Seen); i += 8 {
		got.seen[binary.LittleEndian.Uint64(got.Seen[i:])] = struct{}{}
	}
	got.Seen = nil
	return got, true
}

func (st *state) encode() ([]byte, error) {
	keys := make([]uint64, 0, len(st.seen))
	for k := range st.seen {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	st.Seen = make([]byte, 0, 8*len(keys))
	for _, k := range keys {
		st.Seen = binary.LittleEndian.AppendUint64(st.Seen, k)
	}
	b, err := json.Marshal(st)
	st.Seen = nil
	return b, err
}

// Parse implements transcript.Parser. On error it returns cur unchanged:
// the caller retries from its last saved cursor and the re-emitted rows
// upsert by native id or locator.
//
// A cursor whose State does not decode, or has another stateVersion, is
// treated as the start of the file: the parse starts over at offset 0 and
// re-emits every row, which the store upserts by native id or locator. A
// source is never left stuck behind a state it cannot read (P3).
func (p *Parser) Parse(ctx context.Context, in transcript.Input, cur transcript.Cursor, sink transcript.Sink) (transcript.Cursor, error) {
	return p.parse(ctx, in, cur, sink, nil)
}

func (p *Parser) ExtractionContract() string {
	return transcript.Contract(p.Name(), p.Caps, maxOversized, 0)
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
	stats := p.Stats
	if stats == nil {
		stats = &Stats{}
	}
	st, ok := decodeState(cur.State)
	if !ok || (len(cur.State) == 0 && !cur.IsStart()) {
		stats.StateResets.Add(1)
		cur = transcript.Cursor{}
	}
	if diagnostics != nil {
		diagnostics.FromOffset = cur.Offset
	}
	r := &run{diagnostics: diagnostics, p: p, st: st, sink: sink, stats: stats, in: in, ctx: ctx, cache: newCallCache()}
	if in.Source != nil {
		r.path = in.Source.Path
	}
	next, err := transcript.ScanJSONL(ctx, in, cur, p.LineOptions, r.line)
	if err != nil {
		return cur, err
	}
	if next.State, err = st.encode(); err != nil {
		return cur, err
	}
	return next, nil
}

type run struct {
	diagnostics *transcript.Diagnostics
	p           *Parser
	st          *state
	sink        transcript.Sink
	stats       *Stats
	path        string
	in          transcript.Input
	ctx         context.Context

	l      *transcript.Line // current line
	cache  callCache        // rows and events of tracked calls read in this call
	buf    []byte           // oversized line buffer
	parent *parentHistory   // the fork parent's ids, loaded on demand
}

// errTooLarge marks a line over maxOversized.
var errTooLarge = errors.New("codex: line too large")

// cwdTag is the directory an <environment_context> block names.
var cwdTag = regexp.MustCompile(`<cwd>([^<]+)</cwd>`)

func (r *run) decode(lr *transcript.LineReader, l *transcript.Line, v any) error {
	if !l.Oversized {
		return json.Unmarshal(l.Data, v)
	}
	if l.ContentLen > maxOversized {
		r.stats.TooLarge.Add(1)
		r.diagnostics.Record(transcript.RecordTooLarge, l.No, l.Offset)
		return errTooLarge
	}
	// Decoded whole: the reservation covers the line's full size (P5).
	buf, err := lr.Materialize(l, r.buf)
	if err != nil {
		return err
	}
	r.buf = buf
	return json.Unmarshal(buf, v)
}

// decodeLenient is decode for canonical records: a field of an unexpected
// type (a future format change) leaves that field zero and keeps the rest,
// rather than dropping the record.
func (r *run) decodeLenient(lr *transcript.LineReader, l *transcript.Line, v any) (bool, error) {
	return r.decodeRecord(lr, l, v, true)
}
func (r *run) decodeRecord(lr *transcript.LineReader, l *transcript.Line, v any, lenient bool) (bool, error) {
	err := r.decode(lr, l, v)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, errTooLarge) {
		return false, nil
	}
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		if lenient {
			r.stats.TypeErrors.Add(1)
		}
		r.diagnostics.Record(transcript.FieldTypeMismatch, l.No, l.Offset)
		return lenient, nil
	}
	var se *json.SyntaxError
	if errors.As(err, &se) {
		r.stats.Malformed.Add(1)
		r.diagnostics.Record(transcript.MalformedRecord, l.No, l.Offset)
		return false, nil
	}
	return false, err // materialization/read errors are execution failures
}

// responseTypes are the item types a legacy (unenveloped) rollout writes
// at the top level.
var responseTypes = map[string]bool{
	"message": true, "reasoning": true, "function_call": true, "function_call_output": true,
	"custom_tool_call": true, "custom_tool_call_output": true, "web_search_call": true,
	"agent_message": true, "tool_search_call": true, "tool_search_output": true,
	"image_generation_call": true,
}

func (r *run) line(lr *transcript.LineReader, l *transcript.Line) error {
	r.stats.Lines.Add(1)
	r.l = l
	if l.ContentLen == 0 {
		return nil
	}
	typ, ok := transcript.PeekLine(l, "type")
	if !ok {
		if !r.st.Started {
			return r.legacyHeader(lr, l)
		}
		// Validate only the fallback path; canonical records already decode once.
		var raw json.RawMessage
		_, err := r.decodeRecord(lr, l, &raw, false)
		return err
	}
	switch typ {
	case "session_meta":
		var ln line[sessionMeta]
		if ok, err := r.decodeLenient(lr, l, &ln); err != nil || !ok {
			return err
		}
		return r.sessionMeta(&ln)
	case "response_item":
		var ln line[item]
		if ok, err := r.decodeLenient(lr, l, &ln); err != nil || !ok {
			return err
		}
		return r.responseItem(parseTS(ln.Timestamp), ln.Ordinal, &ln.Payload)
	case "compacted":
		var ln line[compacted]
		if ok, err := r.decodeLenient(lr, l, &ln); err != nil || !ok {
			return err
		}
		return r.compacted(parseTS(ln.Timestamp), ln.Ordinal, &ln.Payload)
	case "event_msg":
		// Classify from the line prefix; decode only what the parser reads.
		// A peek can fail on an oversized line whose head is cut short; the
		// line is then decoded and classified in full.
		ptype, ok := transcript.PeekLine(l, "payload", "type")
		switch {
		case !ok:
		case ptype == "item_completed":
			if it, ok := transcript.PeekLine(l, "payload", "item", "type"); ok && !isEnrichmentType(it) {
				if !knownIgnoredItem(it) {
					r.diagnostics.Record(transcript.UnknownRecordType, l.No, l.Offset)
				}
				return nil // known mirrors and UI items
			}
		case ptype == "user_message", ptype == "task_started", ptype == "task_complete", ptype == "turn_aborted":
		default:
			if !knownIgnoredEvent(ptype) {
				r.diagnostics.Record(transcript.UnknownRecordType, l.No, l.Offset)
			}
			return nil // known mirrors and metadata
		}
		var ln line[struct {
			eventMsg
			Item *eventItem `json:"item"`
		}]
		if ok, err := r.decodeRecord(lr, l, &ln, false); err != nil || !ok {
			if !ok && err == nil && ptype == "item_completed" {
				r.stats.EventErrors.Add(1)
			}
			return err
		}
		return r.event(parseTS(ln.Timestamp), ln.Ordinal, &ln.Payload.eventMsg, ln.Payload.Item)
	case "turn_context":
		var ln line[turnContext]
		if ok, err := r.decodeRecord(lr, l, &ln, false); err != nil || !ok {
			return err
		}
		if err := r.addCwd(ln.Payload.Cwd); err != nil {
			return err
		}
		if err := r.forkTurn(ln.Payload.TurnID); err != nil {
			return err
		}
		if !r.inherited(ln.Ordinal) && ln.Payload.TurnID != "" {
			r.st.Turn = ln.Payload.TurnID
		}
		return nil
	default:
		if responseTypes[typ] { // legacy rollout: bare items, no envelope
			var it item
			if ok, err := r.decodeLenient(lr, l, &it); err != nil || !ok {
				return err
			}
			return r.responseItem(parseTS(it.Timestamp), nil, &it)
		}
		// token_count, token_usage_record, world_state,
		// inter_agent_communication_metadata and unknown types.
		switch typ {
		case "token_count", "token_usage_record", "world_state", "inter_agent_communication_metadata":
		default:
			r.diagnostics.Record(transcript.UnknownRecordType, l.No, l.Offset)
		}
		return nil
	}
}

// legacyHeader reads the August 2025 header line {"id","timestamp",...}.
func (r *run) legacyHeader(lr *transcript.LineReader, l *transcript.Line) error {
	var h struct {
		ID        string `json:"id"`
		Timestamp string `json:"timestamp"`
		Git       *Git   `json:"git"`
	}
	if ok, err := r.decodeRecord(lr, l, &h, false); err != nil || !ok {
		return err
	}
	if h.ID == "" {
		return nil
	}
	m := meta{SessionID: h.ID, Git: h.Git, Legacy: true}
	if ts := parseTS(h.Timestamp); !ts.IsZero() {
		m.StartedNS = ts.UnixNano()
	}
	r.st.Meta = m
	return r.emitConversation()
}

func (r *run) sessionMeta(ln *line[sessionMeta]) error {
	if r.st.Started {
		return nil // a fork's copied parent meta, or a repeat
	}
	cwds, first := r.st.Meta.Cwds, r.st.Meta.Cwd
	r.st.Meta = metaFrom(&ln.Payload, ln.Timestamp)
	// Directories a turn named before the meta line stay recorded.
	for _, c := range append([]string{first}, transcript.SplitCwds(cwds)...) {
		transcript.AddCwd(&r.st.Meta.Cwds, r.st.Meta.Cwd, c)
	}
	if r.st.Meta.SessionID == "" {
		r.st.Meta.SessionID = SessionIDFromPath(r.path)
	}
	m := &r.st.Meta
	if m.ForkedFrom != "" && m.HistoryStart == 0 && m.HistoryBase == nil {
		// A fork with no marker of where its own history starts: it copied
		// the parent's items first. Skip them while they match the parent.
		ph, err := r.parentHistory()
		if err != nil {
			return err
		}
		r.st.ForkPrefix = ph != nil
	}
	return r.emitConversation()
}

// addCwd records a working directory the session named after its first
// (a turn_context cwd, a <cwd> tag), and re-emits the conversation once it
// has started. The first directory a session names (session_meta, or an
// older rollout's first tag) stays its Cwd as placement reads it.
func (r *run) addCwd(cwd string) error {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return nil
	}
	m := &r.st.Meta
	if m.Cwd == "" && m.Cwds == "" && !r.st.Started {
		// No directory yet: the conversation's first. Not for a legacy
		// rollout's header, which never names one: its first tag stays
		// in OtherCwds, where placement also finds it.
		m.Cwd = cwd
		return nil
	}
	if !transcript.AddCwd(&m.Cwds, m.Cwd, cwd) || !r.st.Started {
		return nil
	}
	return r.emitConversation()
}

func (r *run) emitConversation() error {
	r.st.Started = true
	return r.sink.Conversation(r.st.Meta.conversation())
}

func (r *run) ensureConversation() error {
	if r.st.Started {
		return nil
	}
	r.st.Meta.SessionID = SessionIDFromPath(r.path)
	return r.emitConversation()
}

// inherited reports whether a line is parent history a fork copied in
// before its own first record: below subagent_history_start_ordinal, or in
// the copied prefix of a fork without that marker (D11).
func (r *run) inherited(ord *int64) bool {
	if r.st.Meta.HistoryStart > 0 && ord != nil && *ord < r.st.Meta.HistoryStart {
		r.stats.InheritedSkipped.Add(1)
		return true
	}
	if r.st.ForkPrefix {
		r.stats.ForkPrefixSkipped.Add(1)
		return true
	}
	return false
}

// forkTurn ends a fork's copied prefix at the first turn the parent does
// not have.
func (r *run) forkTurn(turn string) error {
	if !r.st.ForkPrefix || turn == "" {
		return nil
	}
	ph, err := r.parentHistory()
	if err != nil {
		return err
	}
	if ph == nil || !ph.has(ph.turns, turn) {
		r.st.ForkPrefix = false
	}
	return nil
}

// forkItem ends a fork's copied prefix at the first item whose payload id
// the parent does not have.
func (r *run) forkItem(id string) error {
	if !r.st.ForkPrefix || id == "" {
		return nil
	}
	ph, err := r.parentHistory()
	if err != nil {
		return err
	}
	if ph == nil || !ph.has(ph.ids, id) {
		r.st.ForkPrefix = false
	}
	return nil
}

// row is one message before it is bound to a session.
type row struct {
	NativeID   string          `json:"id,omitempty"`
	Kind       transcript.Kind `json:"k"`
	Role       string          `json:"ro,omitempty"`
	ToolName   string          `json:"tn,omitempty"`
	ToolCallID string          `json:"tc,omitempty"`
	IsError    bool            `json:"e,omitempty"`
	TSns       int64           `json:"ts,omitempty"`
	Text       string          `json:"x,omitempty"`
	FullLen    int             `json:"fl,omitempty"`
	SHA        [32]byte        `json:"h"`
	LineNo     int64           `json:"ln"`
	Offset     int64           `json:"o"`
	Len        int64           `json:"bl"`
	Part       int             `json:"pt,omitempty"`
	Parser     string          `json:"pa,omitempty"`
	Ordinal    *int64          `json:"co,omitempty"` // Codex's own top-level ordinal

	Commands       []Command `json:"cmds,omitempty"`
	Paths          []string  `json:"paths,omitempty"`
	Author         string    `json:"au,omitempty"`
	Recipient      string    `json:"rc,omitempty"`
	Window         string    `json:"w,omitempty"`
	FromCompaction bool      `json:"fc,omitempty"`
	Enriched       string    `json:"en,omitempty"`
}

func (w *row) setText(full string) {
	w.FullLen = len(full)
	w.SHA = sha256.Sum256([]byte(full))
	w.Text = full
	if w.Kind == kindToolCall || w.Kind == kindToolResult {
		w.Text, _ = transcript.Cap(full, transcript.ToolCap)
	}
}

func (r *run) setText(w *row, full string) {
	w.setText(full)
	if r.p.Caps != nil {
		w.Text, _ = transcript.Cap(full, transcript.CapFor(r.p.Caps, w.Kind))
	}
	if w.Text != full && r.l != nil && w.Offset == r.l.Offset {
		r.diagnostics.Record(transcript.TextTruncated, w.LineNo, w.Offset)
	}
}

func (w *row) message(sessionID string) *transcript.Message {
	m := &transcript.Message{
		SessionID:  sessionID,
		NativeID:   w.NativeID,
		Ordinal:    transcript.OrdinalAt(w.Offset, w.Part),
		Part:       w.Part,
		Kind:       w.Kind,
		Role:       w.Role,
		ToolName:   w.ToolName,
		ToolCallID: w.ToolCallID,
		IsError:    w.IsError,
		TS:         tsFromNS(w.TSns),
		Text:       w.Text,
		FullLen:    w.FullLen,
		ContentSHA: w.SHA,
		LineNo:     w.LineNo,
		ByteOffset: w.Offset,
		ByteLen:    w.Len,
		Parser:     w.Parser,
	}
	if m.Parser == "" {
		m.Parser = RowParser
	}
	e := map[string]any{}
	if w.Ordinal != nil {
		e["codex_ordinal"] = *w.Ordinal
	}
	if len(w.Commands) > 0 {
		e["commands"] = append([]Command(nil), w.Commands...)
	}
	if len(w.Paths) > 0 {
		e["changed_paths"] = append([]string(nil), w.Paths...)
	}
	if w.Author != "" {
		e["author"] = w.Author
	}
	if w.Recipient != "" {
		e["recipient"] = w.Recipient
	}
	if w.Window != "" {
		e["compaction_window"] = w.Window
	}
	if w.FromCompaction {
		e["from_compaction"] = true
	}
	if w.Enriched != "" {
		e["enrichment"] = w.Enriched
	}
	if len(e) > 0 {
		m.Enrichment = e
	}
	return m
}

func tsFromNS(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

// newRow starts a row located at the current line.
func (r *run) newRow(kind transcript.Kind, ts time.Time, ord *int64, part int) row {
	return newRowAt(kind, ts, ord, part, r.lineRef())
}

// newRowAt starts a row located at ref.
func newRowAt(kind transcript.Kind, ts time.Time, ord *int64, part int, ref lineRef) row {
	w := row{Kind: kind, LineNo: ref.No, Offset: ref.Off, Len: ref.Len, Part: part, Ordinal: ord}
	if !ts.IsZero() {
		w.TSns = ts.UnixNano()
	}
	return w
}

func (r *run) emit(w row) error {
	if err := r.ensureConversation(); err != nil {
		return err
	}
	if err := r.sink.Message(w.message(r.st.Meta.SessionID)); err != nil {
		return err
	}
	// A subagent's first prompt is the agent_message its parent sent.
	if (w.Kind == kindUser || w.Kind == kindAgentMessage) && r.st.Meta.Title == "" && !w.FromCompaction && !isInjectedContext(w.Text) {
		if t := titleOf(w.Text); t != "" {
			r.st.Meta.Title = t
			return r.emitConversation()
		}
	}
	return nil
}

// seenKey identifies a message item for compaction replay detection: its
// payload id, or its role and text when it has none.
func seenKey(id, role, text string) uint64 {
	var s [32]byte
	if id != "" {
		s = sha256.Sum256([]byte("id\x00" + id))
	} else {
		s = sha256.Sum256([]byte("tx\x00" + role + "\x00" + text))
	}
	return binary.LittleEndian.Uint64(s[:8])
}

func (r *run) markSeen(it *item) {
	switch it.Type {
	case "message":
		r.st.seen[seenKey(it.id(), it.Role, contentText(it.Content))] = struct{}{}
	case "agent_message":
		r.st.seen[seenKey(it.id(), "agent", agentMessageText(it.Content))] = struct{}{}
	}
}

func msPtr(ts time.Time) *int64 {
	if ts.IsZero() {
		return nil
	}
	v := ts.UnixMilli()
	return &v
}

// messageKind is roleKind, except that a user message holding only context
// the harness injected (AGENTS.md, <environment_context>, ...) is kind
// injected (D6).
func messageKind(it *item) transcript.Kind {
	if it.Role == "user" && injectedContent(it.Content) {
		return kindInjected
	}
	return roleKind(it.Role)
}

func roleKind(role string) transcript.Kind {
	switch role {
	case "user":
		return kindUser
	case "developer", "system":
		return kindSystem
	}
	return kindAssistant
}

func (r *run) responseItem(ts time.Time, ord *int64, it *item) error {
	if !responseTypes[it.Type] {
		r.diagnostics.Record(transcript.UnknownRecordType, r.l.No, r.l.Offset)
	}
	if err := r.forkItem(it.id()); err != nil {
		return err
	}
	if r.inherited(ord) {
		r.markSeen(it)
		return nil
	}
	switch it.Type {
	case "message":
		text := contentText(it.Content)
		if strings.TrimSpace(text) == "" {
			return nil
		}
		if it.Role == "user" && isInjectedContext(text) {
			if sm := cwdTag.FindStringSubmatch(text); sm != nil {
				if err := r.addCwd(sm[1]); err != nil {
					return err
				}
			}
		}
		r.st.seen[seenKey(it.id(), it.Role, text)] = struct{}{}
		if it.Role == "user" && observePrompt(&r.st.Prompt, streamResponse, r.l.No, text, msPtr(ts), "") {
			r.stats.PromptDupes.Add(1)
			return nil
		}
		w := r.newRow(messageKind(it), ts, ord, 0)
		w.NativeID, w.Role = it.id(), it.Role
		r.setText(&w, text)
		return r.emit(w)

	case "reasoning":
		var parts []string
		for _, s := range it.Summary {
			if strings.TrimSpace(s.Text) != "" {
				parts = append(parts, s.Text)
			}
		}
		var content []part
		if json.Unmarshal(it.Content, &content) == nil {
			for _, c := range content {
				if (c.Type == "reasoning_text" || c.Type == "text") && strings.TrimSpace(c.Text) != "" {
					parts = append(parts, c.Text)
				}
			}
		}
		if len(parts) == 0 {
			return nil // encrypted only
		}
		w := r.newRow(kindThinking, ts, ord, 0)
		w.NativeID, w.Role = it.id(), "assistant"
		r.setText(&w, strings.Join(parts, "\n\n"))
		return r.emit(w)

	case "function_call", "custom_tool_call":
		w := r.callRow(ts, ord, it, r.lineRef())
		r.openCall(w, firstNonEmpty(it.turnID(), r.st.Turn))
		r.cache.rows[w.Offset] = w
		return r.emit(w)

	case "function_call_output", "custom_tool_call_output":
		text := outputText(it.Output)
		var exit *int
		if out, code, ok := legacyShellOutput(text); ok {
			text, exit = out, code
		}
		oc := r.closeCall(it.CallID)
		var callName string
		if oc != nil {
			callName = oc.Name
			if len(oc.Events) > 0 {
				if err := r.reemit(oc); err != nil {
					return err
				}
			}
		}
		// An empty output is still a result: it pairs the call and carries
		// is_error from the exit code (D19).
		w := r.newRow(kindToolResult, ts, ord, 0)
		w.NativeID, w.Role, w.ToolCallID, w.ToolName = it.id(), "tool", it.CallID, callName
		w.IsError = exit != nil && *exit != 0
		r.setText(&w, text)
		return r.emit(w)

	case "agent_message":
		text := agentMessageText(it.Content)
		if strings.TrimSpace(text) == "" {
			return nil
		}
		r.st.seen[seenKey(it.id(), "agent", text)] = struct{}{}
		w := r.newRow(kindAgentMessage, ts, ord, 0)
		w.NativeID, w.Role, w.Author, w.Recipient = it.id(), "agent", it.Author, it.Recipient
		r.setText(&w, text)
		return r.emit(w)

	case "web_search_call":
		var text string
		if a := it.Action; a != nil {
			text = firstNonEmpty(a.Query, strings.Join(a.Queries, "\n"), a.URL, a.Pattern)
		}
		w := r.newRow(kindToolCall, ts, ord, 0)
		w.NativeID, w.Role, w.ToolName = it.id(), "assistant", "web_search"
		r.setText(&w, text)
		return r.emit(w)

	case "tool_search_call":
		w := r.newRow(kindToolCall, ts, ord, 0)
		w.NativeID, w.Role, w.ToolName, w.ToolCallID = it.id(), "assistant", "tool_search", it.CallID
		r.setText(&w, rawString(it.Arguments))
		return r.emit(w)

	case "tool_search_output":
		var names []string
		var walk func([]namedTool)
		walk = func(ts []namedTool) {
			for _, t := range ts {
				if t.Name != "" {
					names = append(names, t.Name)
				}
				walk(t.Tools)
			}
		}
		walk(it.Tools)
		if len(names) == 0 {
			return nil
		}
		w := r.newRow(kindToolResult, ts, ord, 0)
		w.NativeID, w.Role, w.ToolName, w.ToolCallID = it.id(), "tool", "tool_search", it.CallID
		r.setText(&w, strings.Join(names, "\n"))
		return r.emit(w)

	case "image_generation_call":
		w := r.newRow(kindToolCall, ts, ord, 0)
		w.NativeID, w.Role, w.ToolName = it.id(), "assistant", "image_generation"
		r.setText(&w, it.Revised)
		return r.emit(w)
	}
	return nil
}

func (r *run) openCall(w row, turn string) {
	r.st.Open = append(r.st.Open, &openCall{CallID: w.ToolCallID, Turn: turn, Name: w.ToolName, Line: lineRef{No: w.LineNo, Off: w.Offset, Len: w.Len}})
	if len(r.st.Open) > maxOpenCalls {
		// Oldest call never got an output; stop tracking it. Its events,
		// if any, were already counted; the row itself stays as emitted.
		r.cache.forget(r.st.Open[0])
		r.st.Open = r.st.Open[1:]
	}
}

func (r *run) closeCall(callID string) *openCall {
	if callID == "" {
		return nil
	}
	for i := len(r.st.Open) - 1; i >= 0; i-- {
		if oc := r.st.Open[i]; oc.CallID == callID {
			r.st.Open = append(r.st.Open[:i], r.st.Open[i+1:]...)
			r.retire(oc)
			return oc
		}
	}
	return nil
}

// retire keeps a closed call for late events. A command a tool call
// started in the background (exec_command with a yield time, an exec
// script that returned early) completes after the call's output is written;
// its CommandExecution event then names a call that is no longer open.
func (r *run) retire(oc *openCall) {
	r.st.Closed = append(r.st.Closed, oc)
	if len(r.st.Closed) > maxClosedCalls {
		for _, old := range r.st.Closed[:len(r.st.Closed)-maxClosedCalls] {
			r.cache.forget(old)
		}
		r.st.Closed = r.st.Closed[len(r.st.Closed)-maxClosedCalls:]
	}
}

// lateCall finds the most recently closed call of turn whose text contains
// the event's command. Call text is read back from the rollout.
func (r *run) lateCall(turn string, e *event) (*openCall, error) {
	if e.Command == nil || turn == "" {
		return nil, nil
	}
	for i := len(r.st.Closed) - 1; i >= 0; i-- {
		oc := r.st.Closed[i]
		if oc.Turn != turn {
			continue
		}
		w, ok, err := r.readCall(oc.Line)
		if err != nil {
			return nil, err
		}
		if ok && commandInText(w.Text, e.Command.Cmd) {
			return oc, nil
		}
	}
	return nil, nil
}

func (r *run) reemit(oc *openCall) error {
	w, ok, err := r.enrichedCall(oc)
	if err != nil || !ok {
		return err
	}
	r.stats.CallsReemitted.Add(1)
	return r.emit(w)
}

// closeTurn drops the open calls of a finished or aborted turn, re-emitting
// those that collected events.
func (r *run) closeTurn(turn string) error {
	if turn == "" {
		return nil
	}
	kept := r.st.Open[:0]
	var done []*openCall
	for _, oc := range r.st.Open {
		if oc.Turn == turn {
			done = append(done, oc)
		} else {
			kept = append(kept, oc)
		}
	}
	r.st.Open = kept
	for _, oc := range done {
		r.retire(oc)
		if len(oc.Events) > 0 {
			if err := r.reemit(oc); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *run) event(ts time.Time, ord *int64, ev *eventMsg, it *eventItem) error {
	switch ev.Type {
	case "user_message", "task_started", "task_complete", "turn_aborted", "item_completed":
	default:
		if !knownIgnoredEvent(ev.Type) {
			r.diagnostics.Record(transcript.UnknownRecordType, r.l.No, r.l.Offset)
		}
	}
	if ev.Type == "item_completed" && it != nil && !isEnrichmentType(it.Type) && !knownIgnoredItem(it.Type) {
		r.diagnostics.Record(transcript.UnknownRecordType, r.l.No, r.l.Offset)
	}
	if ev.Type == "task_started" {
		if err := r.forkTurn(ev.TurnID); err != nil {
			return err
		}
	}
	if r.inherited(ord) {
		return nil
	}
	switch ev.Type {
	case "user_message":
		if strings.TrimSpace(ev.Message) == "" {
			return nil
		}
		if observePrompt(&r.st.Prompt, streamEvent, r.l.No, ev.Message, msPtr(ts), ev.TurnID) {
			r.stats.PromptDupes.Add(1)
			return nil
		}
		w := r.newRow(kindUser, ts, ord, 0)
		w.Role = "user"
		r.setText(&w, ev.Message)
		return r.emit(w)
	case "task_started":
		if ev.TurnID != "" {
			r.st.Turn = ev.TurnID
		}
	case "task_complete", "turn_aborted":
		return r.closeTurn(ev.TurnID)
	case "item_completed":
		if it == nil || !isEnrichmentType(it.Type) {
			return nil
		}
		e, err := parseEvent(it)
		if err != nil {
			r.stats.EventErrors.Add(1)
			return nil
		}
		for i := len(r.st.Open) - 1; i >= 0; i-- {
			if oc := r.st.Open[i]; oc.Turn == ev.TurnID {
				oc.Events = append(oc.Events, r.lineRef())
				r.cache.events[r.l.Offset] = e
				r.stats.EventsPaired.Add(1)
				return nil
			}
		}
		oc, err := r.lateCall(ev.TurnID, e)
		if err != nil {
			return err
		}
		if oc != nil {
			oc.Events = append(oc.Events, r.lineRef())
			r.cache.events[r.l.Offset] = e
			r.stats.EventsLate.Add(1)
			return r.reemit(oc)
		}
		r.stats.EventsLoose.Add(1)
		w := unpairedRow(e)
		loc := r.newRow(kindToolResult, ts, ord, 0)
		w.TSns, w.LineNo, w.Offset, w.Len, w.Ordinal = loc.TSns, loc.LineNo, loc.Offset, loc.Len, loc.Ordinal
		r.setText(&w, w.Text)
		return r.emit(w)
	}
	return nil
}

func (r *run) compacted(ts time.Time, ord *int64, c *compacted) error {
	if r.inherited(ord) {
		for i := range c.ReplacementHistory {
			r.markSeen(&c.ReplacementHistory[i])
		}
		return nil
	}
	part := 0
	summary := strings.TrimSpace(strings.Join(nonEmpty(c.Message, retainedText(c.RetainedContext)), "\n\n"))
	window := c.WindowID
	if window == "" && summary != "" {
		s := sha256.Sum256([]byte(summary))
		window = fmt.Sprintf("sum:%x", s[:8])
	}
	if summary != "" && !slices.Contains(r.st.Windows, window) {
		r.st.Windows = append(r.st.Windows, window)
		w := r.newRow(kindSystem, ts, ord, part)
		w.Role, w.Window = "compaction", window
		r.setText(&w, summary)
		r.stats.CompactionSummaries.Add(1)
		if err := r.emit(w); err != nil {
			return err
		}
		part++
	}
	for i := range c.ReplacementHistory {
		it := &c.ReplacementHistory[i]
		var text, role string
		var kind transcript.Kind
		switch it.Type {
		case "message":
			text, role, kind = contentText(it.Content), it.Role, messageKind(it)
		case "agent_message":
			text, role, kind = agentMessageText(it.Content), "agent", kindAgentMessage
		default:
			continue // encrypted compaction summary, tool items
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		key := seenKey(it.id(), role, text)
		if _, ok := r.st.seen[key]; ok {
			r.stats.CompactionReplaySkipped.Add(1)
			continue
		}
		r.st.seen[key] = struct{}{}
		r.stats.CompactionReplayIndexed.Add(1)
		w := r.newRow(kind, ts, ord, part)
		w.NativeID, w.Role, w.FromCompaction, w.Window = it.id(), role, true, c.WindowID
		w.Author, w.Recipient = it.Author, it.Recipient
		r.setText(&w, text)
		if err := r.emit(w); err != nil {
			return err
		}
		part++
	}
	return nil
}

// retainedText collects the strings of compacted.retained_context
// (verified answers and user messages carried across the window).
func retainedText(raw json.RawMessage) string {
	if len(trimSpace(raw)) == 0 {
		return ""
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			if strings.TrimSpace(x) != "" {
				out = append(out, x)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			for _, k := range keys {
				walk(x[k])
			}
		}
	}
	walk(v)
	return strings.Join(out, "\n")
}

func nonEmpty(v ...string) []string {
	var out []string
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

func knownIgnoredEvent(typ string) bool {
	switch typ {
	case "token_count", "agent_message", "agent_reasoning", "agent_reasoning_raw_content", "agent_reasoning_section_break", "agent_message_delta", "agent_reasoning_delta", "item_started", "item_updated", "turn_started", "turn_complete", "session_configured", "context_compacted", "warning", "error":
		return true
	}
	return false
}

func knownIgnoredItem(typ string) bool {
	switch typ {
	case "AgentMessage", "UserMessage", "Reasoning", "AgentReasoning", "WebSearch", "McpToolCall", "CollabToolCall", "ImageGeneration", "TodoList", "Error":
		return true
	}
	return false
}
