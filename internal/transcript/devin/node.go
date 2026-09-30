package devin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

// chatMessage is message_nodes.chat_message. Unknown fields are ignored.
type chatMessage struct {
	MessageID  string            `json:"message_id"`
	Role       string            `json:"role"`
	Content    json.RawMessage   `json:"content"`
	ToolCallID string            `json:"tool_call_id"`
	ToolCalls  []toolCall        `json:"tool_calls"`
	Thinking   json.RawMessage   `json:"thinking"`
	Images     []json.RawMessage `json:"images"`
	Metadata   *struct {
		// Absent, null or a bool; see userKind.
		IsUserInput json.RawMessage `json:"is_user_input"`
		Telemetry   *struct {
			Source    string `json:"source"`
			Operation string `json:"operation"`
		} `json:"telemetry"`
	} `json:"metadata"`
}

type toolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// nodeRow is one full message_nodes row.
type nodeRow struct {
	rowID, nodeID, createdAt int64
	chat                     string
}

// rowContext is what a node's rows need beyond the node itself.
type rowContext struct {
	sessionID  string
	parser     string
	caps       map[transcript.Kind]transcript.CapConfig
	pick       pick
	firstRowID int64
	firstTS    int64
	parentMID  string
	tools      func(callID string) *toolState // nil result: no tool_call_state row
}

// nodeMessages turns one node into rows, in slot order:
//
//   - system → one system row.
//   - user → one user row, or a system row (role still "user") when
//     metadata.is_user_input is present and not true: cache keepalive
//     "continue" prompts and compaction summarization requests. They are
//     kept (keep everything) but out of user-prompt searches.
//   - assistant → thinking row (NativeID "<message_id>#thinking"), text row
//     (NativeID message_id), then one tool_call row per call (NativeID
//     "<message_id>#call:<call id>").
//   - tool → one tool_result row.
//
// Unknown roles and unparseable payloads yield no rows. Image payloads
// never reach the text; an "[image]" marker stands in for each (FAD's rule).
//
// Each row has a fixed slot: thinking 0, text 1, tool call i at 2+i, and 0
// for the single row of other roles. The ordinal is OrdinalAt(first copy's
// row_id, slot). A row with a native id has Part 0, since the id alone
// names it; a node without a message_id (one copy only) uses the slot as
// Part. So a row's identity never depends on which sibling rows the chosen
// copy of a message has (P1).
func nodeMessages(n nodeRow, rc rowContext) ([]*transcript.Message, error) {
	var cm chatMessage
	if err := json.Unmarshal([]byte(n.chat), &cm); err != nil {
		return nil, nil // malformed: ignore, never fail
	}
	var out []*transcript.Message
	slot := 0
	add := func(kind transcript.Kind, nativeSuffix, text string) *transcript.Message {
		part := slot
		if cm.MessageID != "" {
			part = 0
		}
		m := &transcript.Message{
			SessionID:      rc.sessionID,
			NativeID:       cm.MessageID,
			ParentNativeID: rc.parentMID,
			Ordinal:        transcript.OrdinalAt(rc.firstRowID, slot),
			Part:           part,
			Kind:           kind,
			Role:           cm.Role,
			TS:             time.Unix(rc.firstTS, 0).UTC(),
			Locator:        fmt.Sprintf("%s/%d/%d", rc.sessionID, n.nodeID, n.rowID),
			Parser:         rc.parser,
		}
		if cm.MessageID != "" && nativeSuffix != "" {
			m.NativeID = cm.MessageID + nativeSuffix
		}
		switch rc.pick.onPath {
		case 0:
			m.OnActivePath = transcript.BoolPtr(false)
		case 1:
			m.OnActivePath = transcript.BoolPtr(true)
		}
		m.SetText(text, transcript.CapFor(rc.caps, kind))
		out = append(out, m)
		return m
	}
	content := joinNonEmpty(imageMarkers(len(cm.Images)), textContent(cm.Content))

	switch cm.Role {
	case "system":
		if content != "" {
			add(transcript.KindSystem, "", content)
		}
	case "user":
		if content == "" {
			break
		}
		kind := userKind(&cm)
		m := add(kind, "", content)
		if kind == transcript.KindSystem && cm.Metadata != nil && cm.Metadata.Telemetry != nil {
			m.Enrichment = map[string]any{"telemetry_source": cm.Metadata.Telemetry.Source}
		}
	case "assistant":
		if th := strings.TrimSpace(thinkingText(cm.Thinking)); th != "" {
			add(transcript.KindThinking, "#thinking", th)
		}
		if content != "" {
			slot = 1
			add(transcript.KindAssistant, "", content)
		}
		for i, call := range cm.ToolCalls {
			slot = 2 + i
			m := add(transcript.KindToolCall, "#call:"+call.ID, callText(call))
			m.ToolName = call.Name
			m.ToolCallID = call.ID
			if ts := rc.tools(call.ID); ts != nil {
				m.IsError = ts.isError
				m.Enrichment = ts.enrichment
			}
		}
	case "tool":
		// An empty result is still a result: it pairs the call and carries
		// is_error.
		m := add(transcript.KindToolResult, "", content)
		m.ToolCallID = cm.ToolCallID
		m.ToolName = toolName(&cm)
		if ts := rc.tools(cm.ToolCallID); ts != nil {
			m.IsError = ts.isError
			if m.ToolName == "" {
				m.ToolName = ts.toolName
			}
		}
	}
	return out, nil
}

// userKind: a user node is a real prompt unless its metadata carries
// is_user_input with a value other than true. Devin writes null there for
// cache keepalive "continue" prompts and compaction summarization requests;
// real prompts carry true. Nodes without the key (older stores) count as
// prompts.
func userKind(cm *chatMessage) transcript.Kind {
	if cm.Metadata == nil || len(cm.Metadata.IsUserInput) == 0 {
		return transcript.KindUser
	}
	if bytes.Equal(bytes.TrimSpace(cm.Metadata.IsUserInput), []byte("true")) {
		return transcript.KindUser
	}
	return transcript.KindSystem
}

// toolName: telemetry operation for tool results, else the call id prefix
// ("exec:0#..." → "exec").
func toolName(cm *chatMessage) string {
	if md := cm.Metadata; md != nil && md.Telemetry != nil && md.Telemetry.Source == "tool_result" && md.Telemetry.Operation != "" {
		return md.Telemetry.Operation
	}
	if i := strings.IndexByte(cm.ToolCallID, ':'); i > 0 {
		return cm.ToolCallID[:i]
	}
	return ""
}

// callText is what a tool call row indexes: the tool name, then its
// arguments as compact JSON.
func callText(c toolCall) string {
	var b bytes.Buffer
	b.WriteString(c.Name)
	if len(c.Arguments) > 0 && string(c.Arguments) != "null" {
		b.WriteByte('\n')
		if json.Compact(&b, c.Arguments) != nil {
			b.Write(c.Arguments)
		}
	}
	return b.String()
}

// textContent reads content as a plain string, or an OpenAI-style parts
// array whose text parts are joined and whose image parts become markers.
func textContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var chunks []string
	for _, p := range parts {
		switch p.Type {
		case "text":
			chunks = append(chunks, p.Text)
		case "image", "image_url":
			chunks = append(chunks, "[image]")
		}
	}
	return strings.Join(chunks, "\n")
}

// thinkingText accepts {"thinking": "..."} (current) or a bare string.
func thinkingText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var obj struct {
		Thinking string `json:"thinking"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.Thinking
	}
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func imageMarkers(n int) string {
	if n == 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("[image]\n", n), "\n")
}

func joinNonEmpty(parts ...string) string {
	var keep []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, "\n")
}
