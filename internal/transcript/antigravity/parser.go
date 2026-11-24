// Package antigravity parses the canonical Antigravity CLI trajectory JSONL.
package antigravity

import (
	"context"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

const ParserName = "antigravity@1"

var conversationID = regexp.MustCompile(`(?i)^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)

// SessionID extracts the UUID from a canonical brain/<uuid> transcript path.
func SessionID(path string) string {
	if filepath.Base(path) != "transcript.jsonl" {
		return ""
	}
	logs := filepath.Dir(path)
	generated := filepath.Dir(logs)
	conversation := filepath.Dir(generated)
	id := filepath.Base(conversation)
	if filepath.Base(logs) != "logs" || filepath.Base(generated) != ".system_generated" || !conversationID.MatchString(id) {
		return ""
	}
	return id
}

// Parser reads complete trajectory records. Its zero value uses the default
// transcript text and physical line limits.
type Parser struct {
	Caps  map[transcript.Kind]transcript.CapConfig
	Lines transcript.LineReaderOptions
	// LookupMetadata reads from the device's shared metadata snapshot.
	// Archive parsers leave it nil.
	LookupMetadata func(context.Context, string) (Metadata, error)
}

var _ transcript.Parser = (*Parser)(nil)

func (*Parser) Name() string                        { return ParserName }
func (*Parser) Agent() transcript.Agent             { return transcript.AgentAntigravity }
func (*Parser) StorageKind() transcript.StorageKind { return transcript.StorageJSONLAppend }

type step struct {
	Index     *int64     `json:"step_index"`
	Type      string     `json:"type"`
	Source    string     `json:"source"`
	Status    string     `json:"status"`
	CreatedAt string     `json:"created_at"`
	Content   string     `json:"content"`
	Thinking  string     `json:"thinking"`
	Error     string     `json:"error"`
	Calls     []toolCall `json:"tool_calls"`
}

type toolCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type cursorState struct {
	Version uint8            `json:"v"`
	Seen    bool             `json:"seen,omitempty"`
	Title   string           `json:"title,omitempty"`
	Start   time.Time        `json:"start"`
	End     time.Time        `json:"end"`
	Pending map[int64]string `json:"pending,omitempty"`
}

const stateVersion = 1

func (p *Parser) Parse(ctx context.Context, in transcript.Input, cur transcript.Cursor, sink transcript.Sink) (transcript.Cursor, error) {
	if in.Source == nil {
		return cur, nil
	}
	id := in.Source.SessionKey
	if id == "" {
		id = SessionID(in.Source.Path)
	}
	if id == "" {
		return cur, nil
	}

	state := cursorState{Version: stateVersion}
	if !cur.IsStart() {
		if err := json.Unmarshal(cur.State, &state); err != nil || state.Version != stateVersion {
			cur = transcript.Cursor{}
			state = cursorState{Version: stateVersion}
		}
	}
	caps := p.Caps
	if caps == nil {
		caps = transcript.DefaultCaps
	}

	metadata := Metadata{}
	if p.LookupMetadata != nil {
		metadata, _ = p.LookupMetadata(ctx, id)

	}
	conversation := func(s cursorState) error {
		title := metadata.Title
		if title == "" {
			title = s.Title
		}
		return sink.Conversation(&transcript.Conversation{
			Agent: transcript.AgentAntigravity, SessionID: id,
			Title: title, StartedAt: s.Start, LastActivityAt: s.End,
			Cwd: metadata.Cwd, OtherCwds: append([]string(nil), metadata.OtherCwds...),
			ParentSessionID: metadata.ParentSessionID, Depth: metadata.Depth,
		})
	}
	changed := false
	next, err := transcript.ScanJSONL(ctx, in, cur, p.Lines, func(_ *transcript.LineReader, line *transcript.Line) error {
		var s step
		if line.Oversized || json.Unmarshal(line.Data, &s) != nil || s.Index == nil || *s.Index < 0 {
			return nil
		}
		ts, _ := time.Parse(time.RFC3339Nano, s.CreatedAt)
		// Keep identities when an archive is copied, but avoid making every
		// conversation's step 0 a candidate for archival redaction.
		native := strconv.FormatInt(*s.Index, 10)
		if !ts.IsZero() {
			native = ts.UTC().Format(time.RFC3339Nano) + "#" + native
		}

		emit := func(kind transcript.Kind, part int, role, text, tool, callID string, isError bool) error {
			row := &transcript.Message{
				SessionID: id, NativeID: native + "#" + strconv.Itoa(part),
				Ordinal: transcript.OrdinalAt(line.Offset, part), Part: part, Kind: kind, Role: role,
				ToolName: tool, ToolCallID: callID, IsError: isError, TS: ts,
				LineNo: line.No, ByteOffset: line.Offset, ByteLen: line.Len, Parser: ParserName,
			}
			row.SetText(text, transcript.CapFor(caps, kind))
			return sink.Message(row)
		}

		updated := state
		updated.Seen = true
		if !ts.IsZero() && (updated.Start.IsZero() || ts.Before(updated.Start)) {
			updated.Start = ts
		}
		if ts.After(updated.End) {
			updated.End = ts
		}

		var prompt string
		isUser := s.Type == "USER_INPUT" && s.Source == "USER_EXPLICIT" && s.Status == "DONE"
		if isUser {
			prompt = userRequest(s.Content)
			if updated.Title == "" {
				updated.Title = promptTitle(prompt)
			}
		}
		if !state.Seen {
			if err := conversation(updated); err != nil {
				return err
			}
		}
		switch {
		case isUser:
			if prompt != "" {
				if err := emit(transcript.KindUser, 0, "user", prompt, "", "", false); err != nil {
					return err
				}
			}
			updated.Pending = nil
		case s.Type == "PLANNER_RESPONSE" && s.Source == "MODEL" && s.Status == "DONE":
			if s.Thinking != "" {
				if err := emit(transcript.KindThinking, 0, "assistant", s.Thinking, "", "", false); err != nil {
					return err
				}
			}
			if s.Content != "" {
				if err := emit(transcript.KindAssistant, 1, "assistant", s.Content, "", "", false); err != nil {
					return err
				}
			}
			updated.Pending = nil
			for i, call := range s.Calls {
				if call.Name == "" {
					continue
				}
				// Antigravity numbers results immediately after the planner step,
				// in the same order as its list of tool calls.
				resultStep := *s.Index + 1 + int64(i)
				callID := strconv.FormatInt(resultStep, 10)
				if err := emit(transcript.KindToolCall, i+2, "assistant", string(call.Args), call.Name, callID, false); err != nil {
					return err
				}
				// Bound the pending names stored with a cursor.
				if i < 256 && len(call.Name) <= 256 {
					if updated.Pending == nil {
						updated.Pending = make(map[int64]string)
					}
					updated.Pending[resultStep] = call.Name
				}
			}
		case (s.Type == "GENERIC" || s.Type == "RUN_COMMAND") && s.Source == "MODEL" && (s.Status == "DONE" || s.Status == "ERROR"):
			if name, ok := state.Pending[*s.Index]; ok {
				result := s.Content
				if result == "" {
					result = s.Error
				}
				if err := emit(transcript.KindToolResult, 0, "tool", result, name, strconv.FormatInt(*s.Index, 10), s.Status == "ERROR"); err != nil {
					return err
				}
				delete(updated.Pending, *s.Index)
			}
		}

		changed = changed || (state.Seen && (updated.Title != state.Title || updated.Start != state.Start || updated.End != state.End))
		state = updated
		return nil
	})
	if err != nil {
		return cur, err
	}
	if changed {
		if err := conversation(state); err != nil {
			return cur, err
		}
	}
	if next.Offset != cur.Offset {
		// This state contains only JSON-safe scalar values and their containers.
		next.State, _ = json.Marshal(state)
	}
	return next, nil
}

func userRequest(content string) string {
	body, wrapped := strings.CutPrefix(content, "<USER_REQUEST>")
	if !wrapped {
		return strings.TrimSpace(content)
	}
	const closeTag = "</USER_REQUEST>"
	end := strings.LastIndex(body, closeTag+"\n<ADDITIONAL_METADATA>")
	if end < 0 {
		if trimmed := strings.TrimSpace(body); strings.HasSuffix(trimmed, closeTag) {
			end = strings.LastIndex(body, closeTag)
		} else {
			return ""
		}
	}
	return strings.TrimSuffix(strings.TrimPrefix(body[:end], "\n"), "\n")
}

func promptTitle(prompt string) string {
	title, _, _ := strings.Cut(strings.TrimSpace(prompt), "\n")
	title = strings.TrimSpace(title)
	const maxRunes = 100
	count := 0
	for i := range title {
		if count == maxRunes {
			return title[:i]
		}
		count++
	}
	return title
}
