package antigravity

import (
	"bytes"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

const id = "12345678-1234-4567-8910-123456789abc"

const trajectory = `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-01-01T00:00:00.123Z","content":"<USER_REQUEST>\nBuild a sample widget\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nSynthetic context\n</ADDITIONAL_METADATA>\n"}
{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-01-01T00:00:01Z","thinking":"Check the defaults","content":"I will check the defaults","tool_calls":[{"name":"read_file","args":{"path":"widget.txt"}},{"name":"run_command","args":{"CommandLine":"echo widget"}}]}
{"step_index":2,"source":"MODEL","type":"GENERIC","status":"DONE","created_at":"2026-01-01T00:00:02Z","content":"widget=true"}
{"step_index":3,"source":"MODEL","type":"RUN_COMMAND","status":"ERROR","created_at":"2026-01-01T00:00:03Z","error":"Command failed"}
{"step_index":4,"source":"SYSTEM","type":"CHECKPOINT","status":"DONE","created_at":"2026-01-01T00:00:04Z","content":"Repeated prompt"}
{"step_index":5,"source":"SYSTEM","type":"USER_INPUT","status":"DONE","content":"<USER_REQUEST>\nGenerated prompt\n</USER_REQUEST>"}
{"step_index":6,"source":"MODEL","type":"GENERIC","status":"RUNNING","content":"Provisional result"}
{"step_index":7,"source":"MODEL","type":"GENERIC","status":"DONE","content":"Unmatched result"}
malformed line
{"step_index":8,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-01-01T00:00:05Z","content":"The sample is ready"}
`

func TestParse(t *testing.T) {
	p := &Parser{}
	input := transcript.Input{Source: &transcript.Source{
		Path: filepath.Join("custom-brain", id, ".system_generated", "logs", "transcript.jsonl"),
	}, R: strings.NewReader(trajectory), Size: int64(len(trajectory))}
	var full transcript.Collector
	next, err := p.Parse(t.Context(), input, transcript.Cursor{}, &full)
	if err != nil {
		t.Fatal(err)
	}
	if next.Offset != input.Size || next.LineNo != 10 {
		t.Fatalf("cursor = %+v", next)
	}
	wantKinds := []transcript.Kind{
		transcript.KindUser, transcript.KindThinking, transcript.KindAssistant, transcript.KindToolCall,
		transcript.KindToolCall, transcript.KindToolResult, transcript.KindToolResult, transcript.KindAssistant,
	}
	wantText := []string{"Build a sample widget", "Check the defaults", "I will check the defaults",
		`{"path":"widget.txt"}`, `{"CommandLine":"echo widget"}`, "widget=true", "Command failed", "The sample is ready"}
	if len(full.Messages) != len(wantText) {
		t.Fatalf("got %d messages, want %d", len(full.Messages), len(wantText))
	}
	for i, m := range full.Messages {
		if m.SessionID != id || m.Parser != ParserName || m.Kind != wantKinds[i] || m.Text != wantText[i] {
			t.Errorf("message %d = %+v", i, m)
		}
		physical := trajectory[m.ByteOffset : m.ByteOffset+m.ByteLen]
		if !strings.HasSuffix(physical, "\n") || int64(strings.Count(trajectory[:m.ByteOffset], "\n")+1) != m.LineNo {
			t.Errorf("invalid locator for message %d = %+v", i, m)
		}
	}
	for i, call := range full.Messages[3:5] {
		result := full.Messages[5+i]
		if call.ToolCallID == "" || call.ToolCallID != result.ToolCallID || call.ToolName != result.ToolName || result.IsError != (i == 1) {
			t.Errorf("call %+v does not match result %+v", call, result)
		}
	}
	conv := full.Conversations[len(full.Conversations)-1]
	if conv.SessionID != id || conv.Title != wantText[0] || conv.Agent != transcript.AgentAntigravity ||
		conv.StartedAt.Format(time.RFC3339Nano) != "2026-01-01T00:00:00.123Z" ||
		conv.LastActivityAt.Format(time.RFC3339Nano) != "2026-01-01T00:00:05Z" {
		t.Fatalf("conversation = %+v", conv)
	}

	// Pause immediately before the planner newline, then immediately before
	// each subsequent result. Partial physical records never advance the cursor.
	var incremental transcript.Collector
	var cursor transcript.Cursor
	var offset int64
	for _, line := range strings.SplitAfter(trajectory, "\n") {
		if line == "" {
			continue
		}
		offset += int64(len(line))
		input.Size = offset - 1
		incomplete, err := p.Parse(t.Context(), input, cursor, &incremental)
		if err != nil || !reflect.DeepEqual(incomplete, cursor) {
			t.Fatalf("partial line left cursor %+v, want %+v, err %v", incomplete, cursor, err)
		}
		input.Size = offset
		cursor, err = p.Parse(t.Context(), input, cursor, &incremental)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(incremental.Messages, full.Messages) ||
		!reflect.DeepEqual(incremental.Conversations[len(incremental.Conversations)-1], conv) ||
		!reflect.DeepEqual(cursor, next) {
		t.Fatal("incremental parse differed from full parse")
	}
}

func TestCanonicalSessionID(t *testing.T) {
	prefix := filepath.Join("brain", id, ".system_generated", "logs")
	for suffix, want := range map[string]string{
		"transcript.jsonl": id, "transcript_full.jsonl": "", "chunks/transcript.jsonl": "",
	} {
		if got := SessionID(filepath.Join(prefix, suffix)); got != want {
			t.Errorf("%s: id %q, want %q", suffix, got, want)
		}
	}
	if got := SessionID(filepath.Join("brain", "not-an-id", ".system_generated", "logs", "transcript.jsonl")); got != "" {
		t.Errorf("invalid session id accepted: %q", got)
	}
}

func TestUserRequestPreservesQuotedTag(t *testing.T) {
	content := "<USER_REQUEST>\n  Explain </USER_REQUEST> literally.\n\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nTool context\n</ADDITIONAL_METADATA>\n"
	if got, want := userRequest(content), "  Explain </USER_REQUEST> literally.\n"; got != want {
		t.Fatalf("request = %q, want %q", got, want)
	}
	if got := userRequest("<USER_REQUEST>\nunfinished envelope"); got != "" {
		t.Fatalf("incomplete request = %q", got)
	}
}

type failedSink struct {
	transcript.Collector
	err error
}

func (s *failedSink) Message(*transcript.Message) error { return s.err }

func TestSinkFailurePreservesCursor(t *testing.T) {
	p := &Parser{}
	data := bytes.NewReader([]byte(trajectory))
	in := transcript.Input{Source: &transcript.Source{SessionKey: id}, R: data, Size: data.Size()}
	failure := errors.New("cannot write message")
	cur, err := p.Parse(t.Context(), in, transcript.Cursor{}, &failedSink{err: failure})
	if err != failure || !cur.IsStart() {
		t.Fatalf("parse returned cursor %+v and error %v", cur, err)
	}
}
