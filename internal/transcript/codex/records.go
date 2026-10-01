package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Typed views of Codex rollout records. Fields a parser never reads (base
// instructions, encrypted content, image data, command stdout, file
// contents) are left out of the structs, so encoding/json scans past them
// without allocating.

// line is the common envelope: {"timestamp","ordinal","type","payload"}.
type line[P any] struct {
	Timestamp string `json:"timestamp"`
	Ordinal   *int64 `json:"ordinal"`
	Type      string `json:"type"`
	Payload   P      `json:"payload"`
}

// item is a response_item payload. Legacy rollouts (August 2025) write the
// same object bare, without the envelope.
type item struct {
	Type      string          `json:"type"`
	ID        *string         `json:"id"` // null in legacy rollouts
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Input     json.RawMessage `json:"input"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
	Summary   []part          `json:"summary"`
	Action    *searchAction   `json:"action"`
	Author    string          `json:"author"`
	Recipient string          `json:"recipient"`
	Revised   string          `json:"revised_prompt"`
	Tools     []namedTool     `json:"tools"`
	Meta      *struct {
		TurnID string `json:"turn_id"`
	} `json:"internal_chat_message_metadata_passthrough"`

	// Legacy header line fields (no "type").
	Timestamp string `json:"timestamp"`
}

func (it *item) id() string {
	if it.ID == nil {
		return ""
	}
	return *it.ID
}

func (it *item) turnID() string {
	if it.Meta == nil {
		return ""
	}
	return it.Meta.TurnID
}

type part struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Encrypted string `json:"encrypted_content"`
}

type searchAction struct {
	Type    string   `json:"type"`
	Query   string   `json:"query"`
	Queries []string `json:"queries"`
	URL     string   `json:"url"`
	Pattern string   `json:"pattern"`
}

type namedTool struct {
	Name  string      `json:"name"`
	Tools []namedTool `json:"tools"`
}

// sessionMeta is the session_meta payload.
type sessionMeta struct {
	ID             string          `json:"id"`
	SessionID      string          `json:"session_id"`
	Timestamp      string          `json:"timestamp"`
	Cwd            string          `json:"cwd"`
	Originator     string          `json:"originator"`
	CLIVersion     string          `json:"cli_version"`
	Source         json.RawMessage `json:"source"`
	ForkedFromID   string          `json:"forked_from_id"`
	ParentThreadID string          `json:"parent_thread_id"`
	ThreadSource   string          `json:"thread_source"`
	AgentNickname  string          `json:"agent_nickname"`
	AgentRole      string          `json:"agent_role"`
	AgentPath      string          `json:"agent_path"`
	ModelProvider  string          `json:"model_provider"`
	Git            *Git            `json:"git"`
	HistoryStart   *int64          `json:"subagent_history_start_ordinal"`
	HistoryBase    *HistoryBase    `json:"history_base"`
}

// Git is session_meta.git.
type Git struct {
	CommitHash    string `json:"commit_hash,omitempty"`
	Branch        string `json:"branch,omitempty"`
	RepositoryURL string `json:"repository_url,omitempty"`
}

// HistoryBase is session_meta.history_base: a fork that references its base
// thread instead of copying it.
type HistoryBase struct {
	ThreadID           string `json:"thread_id,omitempty"`
	EndOrdinalExcluded int64  `json:"end_ordinal_exclusive,omitempty"`
	EndByteOffset      int64  `json:"end_byte_offset,omitempty"`
}

// compacted is the compacted payload.
type compacted struct {
	Message            string          `json:"message"`
	ReplacementHistory []item          `json:"replacement_history"`
	RetainedContext    json.RawMessage `json:"retained_context"`
	WindowID           string          `json:"window_id"`
	WindowNumber       *int64          `json:"window_number"`
}

// eventMsg is the event_msg payload, reduced to what the parser reads.
type eventMsg struct {
	Type    string `json:"type"`
	TurnID  string `json:"turn_id"`
	Message string `json:"message"`
}

// turnContext is the turn_context payload.
type turnContext struct {
	TurnID string `json:"turn_id"`
	Cwd    string `json:"cwd"`
}

func parseTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(0, t.UnixNano()).UTC()
}

// contentText flattens a content value the way FAD's flatten_content does:
// a string as is, or the text of every text-bearing part joined by "\n".
// Images, encrypted parts and unknown parts are skipped.
func contentText(raw json.RawMessage) string {
	return strings.Join(contentParts(raw), "\n")
}

// contentParts returns the non-empty texts of a content value: the string
// itself, or one entry per text-bearing part.
func contentParts(raw json.RawMessage) []string {
	raw = trimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var out []string
	switch raw[0] {
	case '"':
		var s string
		_ = json.Unmarshal(raw, &s)
		if s != "" {
			out = append(out, s)
		}
	case '[':
		var typed []part
		if json.Unmarshal(raw, &typed) == nil {
			for i := range typed {
				if t := typed[i].text(); t != "" {
					out = append(out, t)
				}
			}
			return out
		}
		var parts []json.RawMessage // mixed strings and objects
		if json.Unmarshal(raw, &parts) != nil {
			return nil
		}
		for _, pr := range parts {
			if t := partText(pr); t != "" {
				out = append(out, t)
			}
		}
	}
	return out
}

// injectedContent reports whether every text part of a user message is
// context the harness injected (isInjectedContext), and there is one.
func injectedContent(raw json.RawMessage) bool {
	found := false
	for _, t := range contentParts(raw) {
		if strings.TrimSpace(t) == "" {
			continue
		}
		if !isInjectedContext(t) {
			return false
		}
		found = true
	}
	return found
}

func partText(raw json.RawMessage) string {
	raw = trimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	var p part
	if json.Unmarshal(raw, &p) != nil {
		return ""
	}
	return p.text()
}

func (p *part) text() string {
	switch p.Type {
	case "", "text", "input_text", "output_text", "Text":
		return p.Text
	}
	return ""
}

// agentMessageText joins the text parts of an agent_message, plus
// encrypted_content parts that hold plaintext. Current multi-agent tools
// write either plaintext or a Fernet token there (agentsview
// internal/parser/codex.go isCodexEncryptedToolContent, see enrich.go
// header); Fernet tokens start with "gAAAAA".
func agentMessageText(raw json.RawMessage) string {
	var parts []part
	if json.Unmarshal(raw, &parts) != nil {
		return contentText(raw)
	}
	var texts []string
	for _, p := range parts {
		switch {
		case p.Type == "encrypted_content":
			if p.Encrypted != "" && !strings.HasPrefix(p.Encrypted, "gAAAAA") {
				texts = append(texts, p.Encrypted)
			}
		case p.Text != "" && (p.Type == "" || p.Type == "text" || p.Type == "input_text" || p.Type == "output_text"):
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// outputText extracts a tool result: a string, a content array, or an
// object whose "content" is an array (FAD tool_output_text).
func outputText(raw json.RawMessage) string {
	raw = trimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '{' {
		var o struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &o) == nil {
			if t := contentText(o.Content); strings.TrimSpace(t) != "" {
				return t
			}
		}
		return ""
	}
	text := contentText(raw)
	if raw[0] == '"' {
		return decodeStringified(text)
	}
	return text
}

// decodeStringified reads a tool output that is JSON written into a
// string (P6), so the stored text has real newlines and quotes instead of
// \n and \" escapes:
//
//   - a content array [{"type":"text","text":...}] (MCP tools) becomes its
//     text parts joined by "\n";
//   - an object whose string values hold newlines (agent tools:
//     {"status":{"<id>":{"completed":"..."}}}) becomes one "path: value"
//     line per value, with the value unescaped;
//   - anything else, including the legacy shell {"output","metadata"}
//     object (legacyShellOutput reads it), is returned as is.
func decodeStringified(text string) string {
	t := strings.TrimSpace(text)
	if len(t) < 2 || (t[0] != '[' && t[0] != '{') || !json.Valid([]byte(t)) {
		return text
	}
	if t[0] == '[' {
		var parts []map[string]json.RawMessage
		if json.Unmarshal([]byte(t), &parts) != nil || len(parts) == 0 {
			return text
		}
		var texts []string
		for _, p := range parts {
			var typ, s string
			if raw, ok := p["type"]; ok && json.Unmarshal(raw, &typ) != nil {
				return text
			}
			raw, ok := p["text"]
			if !ok || json.Unmarshal(raw, &s) != nil {
				if typ == "text" || typ == "" {
					return text
				}
				continue // an image or other non-text part
			}
			switch typ {
			case "", "text", "input_text", "output_text":
				texts = append(texts, s)
			}
		}
		if len(texts) == 0 {
			return text
		}
		return strings.Join(texts, "\n")
	}
	if strings.HasPrefix(t, `{"output":`) {
		return text // legacy shell output, unwrapped by legacyShellOutput
	}
	var v any
	dec := json.NewDecoder(strings.NewReader(t))
	dec.UseNumber()
	if dec.Decode(&v) != nil || !hasMultilineString(v) {
		return text
	}
	var b strings.Builder
	flattenJSON(&b, "", []byte(t))
	return strings.TrimSuffix(b.String(), "\n")
}

func hasMultilineString(v any) bool {
	switch x := v.(type) {
	case string:
		return strings.ContainsAny(x, "\n\"")
	case []any:
		for _, e := range x {
			if hasMultilineString(e) {
				return true
			}
		}
	case map[string]any:
		for _, e := range x {
			if hasMultilineString(e) {
				return true
			}
		}
	}
	return false
}

// flattenJSON writes one "path: value" line per scalar of a JSON document,
// in document order. Strings are written unescaped; other scalars as JSON.
func flattenJSON(b *strings.Builder, path string, raw []byte) {
	raw = trimSpace(raw)
	if len(raw) == 0 {
		return
	}
	join := func(k string) string {
		if path == "" {
			return k
		}
		return path + "." + k
	}
	switch raw[0] {
	case '{':
		dec := json.NewDecoder(bytes.NewReader(raw))
		if _, err := dec.Token(); err != nil {
			return
		}
		for dec.More() {
			tok, err := dec.Token()
			if err != nil {
				return
			}
			key, _ := tok.(string)
			var val json.RawMessage
			if dec.Decode(&val) != nil {
				return
			}
			flattenJSON(b, join(key), val)
		}
	case '[':
		var elems []json.RawMessage
		if json.Unmarshal(raw, &elems) != nil {
			return
		}
		for i, e := range elems {
			flattenJSON(b, fmt.Sprintf("%s[%d]", path, i), e)
		}
	case '"':
		var s string
		_ = json.Unmarshal(raw, &s)
		if path != "" {
			b.WriteString(path)
			b.WriteString(": ")
		}
		b.WriteString(s)
		b.WriteByte('\n')
	default:
		if path != "" {
			b.WriteString(path)
			b.WriteString(": ")
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
}

// legacyShellOutput unwraps the 2025 shell tool output, a JSON string
// {"output": "...", "metadata": {"exit_code": N}}.
func legacyShellOutput(text string) (string, *int, bool) {
	if !strings.HasPrefix(text, `{"output":`) {
		return "", nil, false
	}
	var o struct {
		Output   *string `json:"output"`
		Metadata struct {
			ExitCode *int `json:"exit_code"`
		} `json:"metadata"`
	}
	if json.Unmarshal([]byte(text), &o) != nil || o.Output == nil {
		return "", nil, false
	}
	return *o.Output, o.Metadata.ExitCode, true
}

// rawString returns a JSON string's value, or the raw JSON for any other
// value (function_call arguments are usually a JSON-encoded string).
func rawString(raw json.RawMessage) string {
	raw = trimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	return string(raw)
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\n' || b[0] == '\t' || b[0] == '\r') {
		b = b[1:]
	}
	return b
}

// injectedPrefixes mark user-role records that are harness context rather
// than prompts (FAD 0.3.1 src/connectors/utils.rs is_injected_context_message).
// Flopwire adds <recommended_plugins> (current Codex puts it before the
// AGENTS.md part of the same message) and the harness notices
// <subagent_notification>, <turn_aborted> and <user_action>: none is typed
// by a person.
var injectedPrefixes = []string{"# AGENTS.md instructions", "<environment_context>", "<session_context>", "<user_instructions>",
	"<recommended_plugins>", "<subagent_notification>", "<turn_aborted>", "<user_action>"}

func isInjectedContext(s string) bool {
	s = strings.TrimLeft(s, " \t\r\n")
	for _, p := range injectedPrefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// titleOf is FAD's title rule: the first line, at most 100 characters.
// TitleRunes is where a title taken from the first prompt's first line is
// cut (local redactions mask titles cut there).
const TitleRunes = 100

func titleOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > TitleRunes {
		r = r[:TitleRunes]
	}
	return strings.TrimSpace(string(r))
}
