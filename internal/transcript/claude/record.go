// Claude Code JSONL record shapes.
//
// Field knowledge from franken-agent-detection 0.3.1
// src/connectors/claude_code.rs (git c06d1cb, MIT): queued_command
// attachments, ai-title, tool_result blocks; and kenn-io/agentsview
// internal/parser/claude.go (commit 563023de1d7b7f5af50ad5967c101341a44a2bfc,
// MIT): isCompactSummary, isMeta, toolUseResult.agentId and
// persistedOutputPath. No code copied.

package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// rawRef is a borrowed slice of the line being decoded, valid only while
// that line is processed.
type rawRef []byte

func (r rawRef) isNull() bool { return len(r) == 0 || string(r) == "null" }

// record is one JSONL line: only the fields the parser reads. Everything
// else (toolUseResult bulk, snapshots, rendered attachments) is skipped
// without being decoded.
type record struct {
	Type              string
	Subtype           string
	UUID              string
	ParentUUID        string
	LogicalParentUUID string
	SessionID         string
	Cwd               string
	GitBranch         string
	Timestamp         string
	IsMeta            bool
	IsCompactSummary  bool
	IsAPIError        bool
	RequestID         string
	AITitle           string // ai-title records: "aiTitle" (Claude) or "title" (FAD's reading)
	Content           rawRef // system records
	Message           *apiMessage
	ToolUseResult     toolUseResult
	Attachment        *attachment
}

type apiMessage struct {
	ID      string
	Model   string
	Content rawRef
	Usage   *usage
}

type usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// toolUseResult keeps only the linkage fields of the harness-side mirror
// of a tool result; its bulk duplicates the tool_result block.
type toolUseResult struct {
	AgentID             string
	RunID               string
	PersistedOutputPath string
}

type attachment struct {
	Type        string
	CommandMode string
	Prompt      rawRef
	Origin      rawRef
	IsMeta      bool
	Timestamp   string
	Content     rawRef // hook_additional_context: one string per hook
	HookEvent   string
}

// decodeRecord fills rec from one line. It fails only when the line is not
// a structurally valid JSON object; fields of an unexpected type are left
// zero.
func decodeRecord(b []byte, rec *record) error { return decodeRecordChecked(b, rec, nil) }
func decodeRecordChecked(b []byte, rec *record, mismatch *bool) error {
	return eachField(b, func(k, v []byte) {
		checkFieldType(k, v, mismatch)
		switch string(k) {
		case "type":
			rec.Type = str(v)
		case "subtype":
			rec.Subtype = str(v)
		case "uuid":
			rec.UUID = str(v)
		case "parentUuid":
			rec.ParentUUID = str(v)
		case "logicalParentUuid":
			rec.LogicalParentUUID = str(v)
		case "sessionId":
			rec.SessionID = str(v)
		case "cwd":
			rec.Cwd = str(v)
		case "gitBranch":
			rec.GitBranch = str(v)
		case "timestamp":
			rec.Timestamp = str(v)
		case "isMeta":
			rec.IsMeta = isTrue(v)
		case "isCompactSummary":
			rec.IsCompactSummary = isTrue(v)
		case "isApiErrorMessage":
			rec.IsAPIError = isTrue(v)
		case "requestId":
			rec.RequestID = str(v)
		case "aiTitle":
			rec.AITitle = str(v)
		case "title":
			if rec.AITitle == "" {
				rec.AITitle = str(v)
			}
		case "content":
			rec.Content = v
		case "message":
			if len(v) > 0 && v[0] == '{' {
				rec.Message = decodeMessage(v, mismatch)
			}
		case "toolUseResult":
			if len(v) > 0 && v[0] == '{' {
				_ = eachField(v, func(k, v []byte) {
					switch string(k) {
					case "agentId":
						rec.ToolUseResult.AgentID = str(v)
					case "runId":
						rec.ToolUseResult.RunID = str(v)
					case "persistedOutputPath":
						rec.ToolUseResult.PersistedOutputPath = str(v)
					}
				})
			}
		case "attachment":
			if len(v) > 0 && v[0] == '{' {
				rec.Attachment = decodeAttachment(v, mismatch)
			}
		}
	})
}

func decodeAttachment(b []byte, flags ...*bool) *attachment {
	mismatch := typeFlag(flags)
	a := &attachment{}
	_ = eachField(b, func(k, v []byte) {
		checkFieldType(k, v, mismatch)
		switch string(k) {
		case "type":
			a.Type = str(v)
		case "commandMode":
			a.CommandMode = str(v)
		case "prompt":
			a.Prompt = v
		case "origin":
			a.Origin = v
		case "isMeta":
			a.IsMeta = isTrue(v)
		case "timestamp":
			a.Timestamp = str(v)
		case "content":
			a.Content = v
		case "hookEvent":
			a.HookEvent = str(v)
		}
	})
	return a
}

func decodeMessage(b []byte, flags ...*bool) *apiMessage {
	mismatch := typeFlag(flags)
	m := &apiMessage{}
	_ = eachField(b, func(k, v []byte) {
		checkFieldType(k, v, mismatch)
		switch string(k) {
		case "id":
			m.ID = str(v)
		case "model":
			m.Model = str(v)
		case "content":
			m.Content = v
		case "usage":
			if len(v) > 0 && v[0] == '{' {
				var u usage
				if err := json.Unmarshal(v, &u); err == nil {
					m.Usage = &u
				} else if mismatch != nil {
					var te *json.UnmarshalTypeError
					if errors.As(err, &te) {
						*mismatch = true
					}
				}
			}
		}
	})
	return m
}

// block is one content block. Image and document blocks keep their type
// only; their base64 source is skipped.
type block struct {
	Type      string
	Text      string
	Thinking  string
	ID        string
	Name      string
	Input     rawRef
	ToolUseID string
	Content   rawRef
	IsError   bool
}

func decodeBlock(b []byte, flags ...*bool) block {
	mismatch := typeFlag(flags)
	if mismatch != nil && len(b) > 0 && b[0] != '{' && b[0] != '"' && string(b) != "null" {
		*mismatch = true
	}
	if s, ok := unquote(b); ok { // bare string item
		return block{Type: "text", Text: s}
	}
	var out block
	err := eachField(b, func(k, v []byte) {
		checkFieldType(k, v, mismatch)
		switch string(k) {
		case "type":
			out.Type = str(v)
		case "text":
			out.Text = str(v)
		case "thinking":
			out.Thinking = str(v)
		case "id":
			out.ID = str(v)
		case "name":
			out.Name = str(v)
		case "input":
			out.Input = v
		case "tool_use_id":
			out.ToolUseID = str(v)
		case "content":
			out.Content = v
		case "is_error":
			out.IsError = isTrue(v)
		}
	})
	if err != nil {
		return block{Type: "invalid"}
	}
	return out
}

// contentBlocks decodes a message content value: a plain string becomes a
// single text block; an array yields its blocks.
func contentBlocks(raw rawRef, flags ...*bool) []block {
	mismatch := typeFlag(flags)
	raw = bytes.TrimSpace(raw)
	if raw.isNull() {
		return nil
	}
	if mismatch != nil && raw[0] != '"' && raw[0] != '[' {
		*mismatch = true
	}
	switch raw[0] {
	case '"':
		if s, ok := unquote(raw); ok {
			return []block{{Type: "text", Text: s}}
		}
	case '[':
		var out []block
		_ = eachElem(raw, func(v []byte) { out = append(out, decodeBlock(v, mismatch)) })
		return out
	}
	return nil
}

// flattenText renders a content value (string, or array of text parts) as
// text: text parts joined by newlines, everything else (images, documents,
// tool references) dropped. It is FAD's rule for tool_result content.
func flattenText(raw rawRef, flags ...*bool) string {
	mismatch := typeFlag(flags)
	var parts []string
	for _, b := range contentBlocks(raw, mismatch) {
		if (b.Type == "text" || b.Type == "") && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// compactJSON renders a tool input as compact JSON text.
func compactJSON(raw rawRef) string {
	if raw.isNull() {
		return ""
	}
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return string(raw)
	}
	return b.String()
}

func typeFlag(flags []*bool) *bool {
	if len(flags) > 0 {
		return flags[0]
	}
	return nil
}

// Check only fields the decoder interprets. Null is a valid absent value.
func checkFieldType(k, v []byte, mismatch *bool) {
	if mismatch == nil || len(v) == 0 || string(v) == "null" {
		return
	}
	expected := ""
	switch string(k) {
	case "type", "subtype", "uuid", "parentUuid", "logicalParentUuid", "sessionId", "cwd", "gitBranch", "timestamp", "requestId", "aiTitle", "title", "id", "model", "text", "thinking", "name", "tool_use_id", "agentId", "runId", "persistedOutputPath", "commandMode", "hookEvent":
		expected = "\""
	case "isMeta", "isCompactSummary", "isApiErrorMessage", "is_error":
		expected = "tf"
	case "message", "attachment", "usage":
		expected = "{"
	case "content":
		expected = "\"["
	}
	if expected != "" && !strings.ContainsRune(expected, rune(v[0])) {
		*mismatch = true
	}
}
