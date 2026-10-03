package opencode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

// messageData is message.data, reduced to what rows need. Unknown fields
// are ignored.
type messageData struct {
	Role string `json:"role"`
	Path *struct {
		Cwd string `json:"cwd"`
	} `json:"path"`
	Error *struct {
		Name string `json:"name"`
		Data *struct {
			Message string `json:"message"`
		} `json:"data"`
	} `json:"error"`
}

// partData is part.data, reduced to what rows need.
type partData struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Synthetic bool            `json:"synthetic"`
	Ignored   bool            `json:"ignored"`
	Metadata  json.RawMessage `json:"metadata"`
	Time      *struct {
		Start int64 `json:"start"`
	} `json:"time"`

	// tool
	Tool   string `json:"tool"`
	CallID string `json:"callID"`
	State  *struct {
		Status   string          `json:"status"`
		Input    json.RawMessage `json:"input"`
		Output   string          `json:"output"`
		Error    string          `json:"error"`
		Title    string          `json:"title"`
		Metadata json.RawMessage `json:"metadata"`
	} `json:"state"`

	// file
	Mime     string `json:"mime"`
	Filename string `json:"filename"`
	URL      string `json:"url"`

	// agent, subtask
	Name        string `json:"name"`
	Agent       string `json:"agent"`
	Description string `json:"description"`
	Prompt      string `json:"prompt"`

	// retry
	Attempt  int             `json:"attempt"`
	RetryErr json.RawMessage `json:"error"`
}

// partRow is one part row joined with its message's data.
type partRow struct {
	id, messageID    string
	created, updated int64
	data, message    string
}

// rowContext is what a part's rows need beyond the part.
type rowContext struct {
	sessionID string
	caps      map[transcript.Kind]transcript.CapConfig
}

// idOrder decodes an opencode ascending id ("prt_" or "msg_" + 12 hex
// digits + random) into the millisecond and per-millisecond counter it
// was made at. The id keeps only the low 48 bits of ms*4096+counter, so
// the millisecond is taken modulo 2^36 and unwrapped to the value nearest
// ref (the row's time_created). An id of another shape orders by ref.
func idOrder(id string, ref int64) (ms int64, counter int) {
	_, rest, ok := strings.Cut(id, "_")
	if !ok || len(rest) < 12 {
		return ref, 0
	}
	v, err := strconv.ParseUint(rest[:12], 16, 64)
	if err != nil {
		return ref, 0
	}
	low := int64(v >> 12)
	counter = int(v & 0xfff)
	const span = int64(1) << 36
	ms = ref - ref%span + low
	switch {
	case ms-ref > span/2:
		ms -= span
	case ref-ms > span/2:
		ms += span
	}
	if ms < 0 {
		return ref, 0
	}
	return ms, counter
}

// ordinal is a row's ordinal: OrdinalAt(the id's millisecond, counter*2 +
// sub). sub is 0 for a part's first row and 1 for a tool result.
func ordinal(id string, created int64, sub int) int64 {
	ms, c := idOrder(id, created)
	return transcript.OrdinalAt(ms, min(c, 2047)*2+sub)
}

// partMessages turns one part into rows:
//
//   - text: a user row (an injected row when opencode marked it synthetic,
//     e.g. a file it read in for an @mention), or an assistant row.
//   - reasoning: a thinking row.
//   - tool: a tool_call row (Part 0: the tool name and its input as
//     compact JSON), and once the call settled a tool_result row (Part 1:
//     the output, or the error with IsError).
//   - file, agent, subtask: an injected row naming what the user attached.
//   - compaction, retry: a system row.
//
// step-start, step-finish, snapshot and patch parts carry no text and
// yield nothing. Every row's native id is the part id; malformed JSON
// yields no rows.
func partMessages(p partRow, rc rowContext) []*transcript.Message {
	var md messageData
	if json.Unmarshal([]byte(p.message), &md) != nil || md.Role == "" {
		return nil
	}
	var pd partData
	if json.Unmarshal([]byte(p.data), &pd) != nil {
		return nil
	}
	ts := p.created
	if pd.Time != nil && pd.Time.Start > 0 {
		ts = pd.Time.Start
	}
	var out []*transcript.Message
	add := func(kind transcript.Kind, part int, role, text string) *transcript.Message {
		m := &transcript.Message{
			SessionID: rc.sessionID,
			NativeID:  p.id,
			Ordinal:   ordinal(p.id, p.created, part),
			Part:      part,
			Kind:      kind,
			Role:      role,
			TS:        time.UnixMilli(ts).UTC(),
			Locator:   rc.sessionID + "/" + p.id,
			Parser:    Name,
		}
		m.SetText(text, transcript.CapFor(rc.caps, kind))
		out = append(out, m)
		return m
	}
	switch pd.Type {
	case "text":
		if strings.TrimSpace(pd.Text) == "" {
			break
		}
		kind := transcript.KindAssistant
		delivered := md.Role == "user" && flopwireDelivery(pd.Metadata)
		if md.Role == "user" {
			kind = transcript.KindUser
			if pd.Synthetic || delivered {
				kind = transcript.KindInjected
			}
		}
		m := add(kind, 0, md.Role, pd.Text)
		if delivered {
			// The Flopwire plugin's promptAsync(noReply) delivery: only the
			// plugin, not a prompt or the model, can set part metadata.
			m.Enrichment = map[string]any{transcript.EnrichHookContext: DeliveryHook}
		}
		if pd.Synthetic || pd.Ignored {
			if m.Enrichment == nil {
				m.Enrichment = map[string]any{}
			}
			if pd.Synthetic {
				m.Enrichment["synthetic"] = true
			}
			if pd.Ignored {
				m.Enrichment["ignored"] = true
			}
		}
	case "reasoning":
		if strings.TrimSpace(pd.Text) != "" {
			add(transcript.KindThinking, 0, md.Role, pd.Text)
		}
	case "tool":
		if pd.State == nil {
			break
		}
		call := add(transcript.KindToolCall, 0, md.Role, callText(pd.Tool, pd.State.Input))
		call.ToolName, call.ToolCallID = pd.Tool, pd.CallID
		e, isErr := toolEnrichment(&pd, md)
		call.Enrichment, call.IsError = e, isErr
		switch pd.State.Status {
		case "completed", "error":
			text := pd.State.Output
			if pd.State.Status == "error" {
				text = pd.State.Error
			}
			res := add(transcript.KindToolResult, 1, "tool", text)
			res.ToolName, res.ToolCallID, res.IsError = pd.Tool, pd.CallID, isErr
		}
	case "file":
		add(transcript.KindInjected, 0, md.Role, fileText(&pd))
	case "agent":
		if pd.Name != "" {
			add(transcript.KindInjected, 0, md.Role, "[agent] "+pd.Name)
		}
	case "subtask":
		text := strings.TrimSpace(fmt.Sprintf("[subtask %s] %s\n%s", pd.Agent, pd.Description, pd.Prompt))
		add(transcript.KindInjected, 0, md.Role, text)
	case "compaction":
		add(transcript.KindSystem, 0, md.Role, "[compaction]")
	case "retry":
		add(transcript.KindSystem, 0, md.Role, strings.TrimSpace(fmt.Sprintf("[retry %d] %s", pd.Attempt, errorText(pd.RetryErr))))
	}
	return out
}

// DeliveryHook is the hook_context value of a message the Flopwire plugin
// delivered.
const DeliveryHook = "flopwire-plugin"

// flopwireDelivery reports whether part metadata carries the object the
// Flopwire plugin sets on a message it delivers: {"flopwire": {"id": ...}}.
func flopwireDelivery(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var md struct {
		Flopwire *struct {
			ID string `json:"id"`
		} `json:"flopwire"`
	}
	return json.Unmarshal(raw, &md) == nil && md.Flopwire != nil && md.Flopwire.ID != ""
}

// messageError is the system row of an assistant message that ended in an
// error (an aborted turn, a provider error). Its native id is the message
// id; it orders after the message's parts.
func messageError(id string, updated int64, data string, rc rowContext) *transcript.Message {
	var md messageData
	if json.Unmarshal([]byte(data), &md) != nil || md.Error == nil {
		return nil
	}
	text := md.Error.Name
	if md.Error.Data != nil && md.Error.Data.Message != "" {
		text = strings.TrimSpace(text + ": " + md.Error.Data.Message)
	}
	if text == "" {
		return nil
	}
	m := &transcript.Message{
		SessionID: rc.sessionID,
		NativeID:  id,
		Ordinal:   transcript.OrdinalAt(updated, 4095),
		Kind:      transcript.KindSystem,
		Role:      md.Role,
		IsError:   true,
		TS:        time.UnixMilli(updated).UTC(),
		Locator:   rc.sessionID + "/" + id,
		Parser:    Name,
	}
	m.SetText(text, transcript.CapFor(rc.caps, transcript.KindSystem))
	return m
}

// toolEnrichment is the filter fields of a tool call, in the shapes the
// other parsers use: status, commands [{cmd, cwd, exit_code}] for bash,
// changed_paths for edits and writes, paths for reads and searches. A call
// is an error when its status is "error" or its command exited nonzero.
func toolEnrichment(pd *partData, md messageData) (map[string]any, bool) {
	e := map[string]any{"status": pd.State.Status}
	var in struct {
		Command  string `json:"command"`
		FilePath string `json:"filePath"`
		Path     string `json:"path"`
	}
	_ = json.Unmarshal(pd.State.Input, &in)
	var meta struct {
		Exit      *int64 `json:"exit"`
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(pd.State.Metadata, &meta)
	isErr := pd.State.Status == "error"
	switch pd.Tool {
	case "bash":
		if in.Command != "" {
			cmd := map[string]any{"cmd": in.Command}
			if md.Path != nil && md.Path.Cwd != "" {
				cmd["cwd"] = md.Path.Cwd
			}
			if meta.Exit != nil {
				cmd["exit_code"] = *meta.Exit
				isErr = isErr || *meta.Exit != 0
			}
			e["commands"] = []map[string]any{cmd}
		}
	case "edit", "write", "multiedit", "patch":
		if in.FilePath != "" {
			e["changed_paths"] = []string{in.FilePath}
		}
	case "read", "glob", "grep", "list":
		if p := firstNonEmpty(in.FilePath, in.Path); p != "" {
			e["paths"] = []string{p}
		}
	case "task":
		if meta.SessionID != "" {
			e["subagent_session"] = meta.SessionID
		}
	}
	return e, isErr
}

// callText is what a tool call row indexes: the tool name, then its input
// as compact JSON.
func callText(tool string, input json.RawMessage) string {
	var b bytes.Buffer
	b.WriteString(tool)
	if len(input) > 0 && string(input) != "null" && string(input) != "{}" {
		b.WriteByte('\n')
		if json.Compact(&b, input) != nil {
			b.Write(input)
		}
	}
	return b.String()
}

// fileText names an attached file. Inline data URLs never reach the text.
func fileText(pd *partData) string {
	var b strings.Builder
	b.WriteString("[file]")
	if pd.Filename != "" {
		b.WriteString(" " + pd.Filename)
	}
	if pd.URL != "" && !strings.HasPrefix(pd.URL, "data:") && pd.URL != pd.Filename {
		b.WriteString(" " + pd.URL)
	}
	if pd.Mime != "" {
		b.WriteString(" (" + pd.Mime + ")")
	}
	return b.String()
}

// errorText reads an opencode error object {name, data {message}}, or a
// plain string.
func errorText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var e struct {
		Name string `json:"name"`
		Data *struct {
			Message string `json:"message"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return ""
	}
	if e.Data != nil && e.Data.Message != "" {
		return e.Data.Message
	}
	return e.Name
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
