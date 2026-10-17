package localindex

import (
	"reflect"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

// Check the entire persisted projection, including provenance, ingestion IDs and
// cursor revision. A normal batch must already equal a forced retained replay.
func assertChangeReplay(t *testing.T, s *Store, want int) {
	t.Helper()
	snapshot := func() ([]string, int64) {
		t.Helper()
		rows, err := s.DB().Query(`SELECT json_array(id,source_id,source_generation,conversation_id,event_key,observed_hash,command_id,result_id,reveal_command_id,reveal_result_id,evidence) FROM change_events ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for rows.Next() {
			var row string
			if err := rows.Scan(&row); err != nil {
				t.Fatal(err)
			}
			out = append(out, row)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		state, err := s.ChangeState(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return out, state.Revision
	}
	before, revision := snapshot()
	if len(before) != want {
		t.Fatalf("events=%d, want %d", len(before), want)
	}
	var conv int64
	if err := s.DB().QueryRow(`SELECT id FROM conversations WHERE session_id='refresh'`).Scan(&conv); err != nil {
		t.Fatal(err)
	}
	if err := s.write(ctx, func(w *writeTx) error { return w.refreshChangeEvents(conv) }); err != nil {
		t.Fatal(err)
	}
	after, next := snapshot()
	if !reflect.DeepEqual(before, after) || revision != next {
		t.Fatalf("batch differs from retained replay: revision %d -> %d\nbefore %v\nafter %v", revision, next, before, after)
	}
}

func TestChangeRefreshToolTouches(t *testing.T) {
	for _, mode := range []string{"call-address", "result-address", "generation", "source", "noop", "mixed"} {
		t.Run(mode, func(t *testing.T) {
			s := openTest(t, DetailColumn)
			src := source(t, s, transcript.AgentClaude, "/p/refresh.jsonl")
			pair := changePair("refresh", 0, "abcdef0")
			apply(t, s, Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "refresh", Cwd: "/repo"}}, Messages: pair})
			assertChangeReplay(t, s, 1)
			gen := int64(1)
			sourceID := src.ID
			touched := pair[:1]
			want := 1
			switch mode {
			case "result-address":
				touched = pair[1:]
				fallthrough
			case "call-address", "mixed":
				touched[0].LineNo = 23
				touched[0].ByteOffset = 42
				touched[0].ByteLen = 99
				touched[0].Locator = "moved"
			case "generation":
				gen = 2
				want = 0
			case "source":
				sourceID = source(t, s, transcript.AgentClaude, "/p/other.jsonl").ID
				want = 0
			}
			if mode == "mixed" {
				touched = append(touched, msg("refresh", "assistant", 3, transcript.KindAssistant, "ordinary prose"))
			}
			apply(t, s, Batch{SourceID: sourceID, Generation: gen, Messages: touched})
			assertChangeReplay(t, s, want)
			if mode == "generation" || mode == "source" {
				apply(t, s, Batch{SourceID: sourceID, Generation: gen, Messages: pair[1:]})
				assertChangeReplay(t, s, 1)
			}
		})
	}
}

func TestChangeRefreshArrivalAndDecodedOutput(t *testing.T) {
	for _, mode := range []string{"result-first", "delayed-result", "escaped-sha"} {
		t.Run(mode, func(t *testing.T) {
			s := openTest(t, DetailColumn)
			src := source(t, s, transcript.AgentClaude, "/p/arrival.jsonl")
			pair := changePair("refresh", 0, "abcdef0")
			init := Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "refresh", Cwd: "/repo"}}}
			switch mode {
			case "result-first":
				init.Messages = pair[1:]
			case "delayed-result":
				init.Messages = pair[:1]
			case "escaped-sha":
				pair[1].Text = `{"exit_code":0,"output":"[main \u0061\u0062\u0063\u0064\u0065\u0066\u0030] x"}`
				init.Messages = pair[:1]
			}
			apply(t, s, init)
			assertChangeReplay(t, s, 0)
			remaining := pair[1:]
			if mode == "result-first" {
				remaining = pair[:1]
			}
			apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: remaining})
			assertChangeReplay(t, s, 1)
		})
	}
}

func TestChangeRefreshPollArrival(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentCodex, "/p/poll.jsonl")
	pair := changePair("refresh", 0, strings.Repeat("a", 40))
	pair[1].Text = "Chunk ID: synthetic\nProcess running with session ID 42\nOutput:\n"
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentCodex, SessionID: "refresh", Cwd: "/repo"}}, Messages: pair})
	assertChangeReplay(t, s, 0)
	call := msg("refresh", "poll", 3, transcript.KindToolCall, `{"session_id":42}`)
	call.ToolName = "write_stdin"
	call.ToolCallID = "poll"
	result := msg("refresh", "poll-result", 4, transcript.KindToolResult, "[main "+strings.Repeat("a", 40)+"] fix")
	result.ToolCallID = "poll"
	// The retained poll result is resolved when its earlier-ordinal call arrives.
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{result}})
	assertChangeReplay(t, s, 0)
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{call}})
	assertChangeReplay(t, s, 1)
}

func TestChangeRefreshQuietBarrier(t *testing.T) {
	for _, barrier := range []bool{false, true} {
		t.Run(map[bool]string{false: "quiet-positive", true: "late-barrier"}[barrier], func(t *testing.T) {
			s := openTest(t, DetailColumn)
			src := source(t, s, transcript.AgentClaude, "/p/quiet.jsonl")
			pair := changePair("refresh", 0, strings.Repeat("a", 40))
			pair[0].Text = `{"command":"git -C /repo commit -qm fix"}`
			pair[1].Text = ""
			apply(t, s, Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "refresh", Cwd: "/repo"}}, Messages: pair})
			assertChangeReplay(t, s, 0)
			reveal := changePair("refresh", 2, strings.Repeat("a", 40))
			reveal[0].Text = `{"command":"git -C /repo rev-parse HEAD"}`
			reveal[1].Text = strings.Repeat("a", 40)
			apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: reveal})
			assertChangeReplay(t, s, 1)
			if barrier {
				m := msg("refresh", "barrier", 3, transcript.KindToolCall, `{"command":"git -C /repo reset --hard HEAD~1"}`)
				m.ToolName = "Bash"
				m.ToolCallID = "barrier"
				apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{m}})
				assertChangeReplay(t, s, 0)
			}
		})
	}
}
