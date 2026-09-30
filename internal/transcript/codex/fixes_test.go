package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

func parseLines(t *testing.T, p *Parser, path string, lines []string, cur transcript.Cursor) (*transcript.Collector, transcript.Cursor) {
	t.Helper()
	data := []byte(strings.Join(lines, "\n") + "\n")
	return parseData(t, p, path, data, cur)
}

func parseData(t *testing.T, p *Parser, path string, data []byte, cur transcript.Cursor) (*transcript.Collector, transcript.Cursor) {
	t.Helper()
	if p == nil {
		p = &Parser{}
	}
	var c transcript.Collector
	src := &transcript.Source{Agent: transcript.AgentCodex, Path: path}
	next, err := p.Parse(context.Background(), transcript.Input{Source: src, R: bytes.NewReader(data), Size: int64(len(data))}, cur, &c)
	if err != nil {
		t.Fatal(err)
	}
	return &c, next
}

func jsonString(s string) (string, error) {
	b, err := json.Marshal(s)
	return string(b), err
}

func rec(ord int, typ, payload string) string {
	return fmt.Sprintf(`{"timestamp":"2026-09-15T10:00:%02d.000Z","ordinal":%d,"type":%q,"payload":%s}`, ord%60, ord, typ, payload)
}

// D19: an empty tool output is still a tool_result row, with is_error from
// the exit code when the output carries one.
func TestEmptyToolOutputsAreKept(t *testing.T) {
	lines := []string{
		rec(0, "session_meta", `{"id":"s-empty","cwd":"/x","source":"cli"}`),
		rec(1, "response_item", `{"type":"function_call","name":"shell","arguments":"{\"command\":[\"grep\",\"nomatch\"]}","call_id":"c1"}`),
		rec(2, "response_item", `{"type":"function_call_output","call_id":"c1","output":"{\"output\":\"\",\"metadata\":{\"exit_code\":1}}"}`),
		rec(3, "response_item", `{"type":"function_call","name":"exec_command","arguments":"{}","call_id":"c2"}`),
		rec(4, "response_item", `{"type":"function_call_output","call_id":"c2","output":""}`),
		rec(5, "response_item", `{"type":"custom_tool_call_output","call_id":"c3","output":"  "}`),
	}
	c, _ := parseLines(t, nil, "", lines, transcript.Cursor{})
	var results []*transcript.Message
	for _, m := range c.Messages {
		if m.Kind == transcript.KindToolResult {
			results = append(results, m)
		}
	}
	if len(results) != 3 {
		t.Fatalf("got %d tool_result rows, want 3: %+v", len(results), view(c.Messages))
	}
	if r := results[0]; r.ToolCallID != "c1" || !r.IsError || r.Text != "" || r.ToolName != "shell" {
		t.Errorf("failed empty output %+v", r)
	}
	if r := results[1]; r.ToolCallID != "c2" || r.IsError || r.ToolName != "exec_command" {
		t.Errorf("empty output %+v", r)
	}
}

// P6: tool output that is JSON written into a string is stored decoded.
func TestStringifiedToolOutputsAreDecoded(t *testing.T) {
	cases := []struct{ output, want string }{
		{`[{"type":"text","text":"## Search\n- line \"a\""}]`, "## Search\n- line \"a\""},
		{`[{"text":"one"},{"type":"image","image_url":"x"},{"text":"two"}]`, "one\ntwo"},
		{`{"agent_id":"a1","status":{"completed":"Summary:\n- ok"},"n":3}`, "agent_id: a1\nstatus.completed: Summary:\n- ok\nn: 3"},
		{`{"accepted":true,"id":"x"}`, `{"accepted":true,"id":"x"}`},           // single-line JSON stays as is
		{"Exit code: 0\n" + `{"a":"b\nc"}`, "Exit code: 0\n" + `{"a":"b\nc"}`}, // shell output printing JSON
		{`[1, 2`, `[1, 2`},
	}
	for _, tc := range cases {
		q, _ := jsonString(tc.output)
		lines := []string{
			rec(0, "session_meta", `{"id":"s-json","cwd":"/x","source":"cli"}`),
			rec(1, "response_item", `{"type":"function_call_output","call_id":"c1","output":`+q+`}`),
		}
		c, _ := parseLines(t, nil, "", lines, transcript.Cursor{})
		if len(c.Messages) != 1 || c.Messages[0].Text != tc.want {
			t.Errorf("output %q: got %+v, want %q", tc.output, view(c.Messages), tc.want)
		}
	}
}

// D6: user messages that only hold injected context are kind injected;
// agent_message items are kind agent_message.
func TestInjectedAndAgentMessageKinds(t *testing.T) {
	lines := []string{
		rec(0, "session_meta", `{"id":"s-inj","cwd":"/x","source":"cli"}`),
		rec(1, "response_item", `{"type":"message","role":"user","content":[{"type":"input_text","text":"<recommended_plugins>\nx</recommended_plugins>"},{"type":"input_text","text":"# AGENTS.md instructions for /x\n\nbe terse"},{"type":"input_text","text":"<environment_context>\n<cwd>/x</cwd></environment_context>"}]}`),
		rec(2, "response_item", `{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>x</environment_context>"},{"type":"input_text","text":"real prompt"}]}`),
		rec(3, "response_item", `{"type":"agent_message","id":"am1","author":"/root/a","recipient":"/root","content":[{"type":"text","text":"done"}]}`),
		rec(4, "response_item", `{"type":"message","role":"user","content":[{"type":"input_text","text":"<subagent_notification>\n{\"agent\":\"a\",\"status\":\"completed\"}\n</subagent_notification>"}]}`),
		rec(5, "response_item", `{"type":"message","role":"user","content":[{"type":"input_text","text":"<turn_aborted>\nThe user interrupted the previous turn.\n</turn_aborted>"}]}`),
		rec(6, "response_item", `{"type":"message","role":"user","content":[{"type":"input_text","text":"<user_action>\n<action>review</action>\n</user_action>"}]}`),
	}
	c, _ := parseLines(t, nil, "", lines, transcript.Cursor{})
	got := []transcript.Kind{}
	for _, m := range c.Messages {
		got = append(got, m.Kind)
	}
	want := []transcript.Kind{transcript.KindInjected, transcript.KindUser, transcript.KindAgentMessage,
		transcript.KindInjected, transcript.KindInjected, transcript.KindInjected}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds %v, want %v", got, want)
	}
}

// P3: a cursor state the parser cannot read restarts from the start.
func TestUnreadableStateRestarts(t *testing.T) {
	path := fixturePath(t, sidC)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	full, fullCur := parseData(t, nil, path, data, transcript.Cursor{})
	mid := int64(bytes.IndexByte(data[len(data)/2:], '\n') + len(data)/2 + 1)
	for name, st := range map[string][]byte{
		"corrupt":       []byte("{"),
		"older version": []byte(`{"v":1,"m":{"sid":"x"},"seen":[1,2]}`),
		"missing":       nil,
	} {
		stats := &Stats{}
		c, next := parseData(t, &Parser{Stats: stats}, path, data, transcript.Cursor{Offset: mid, LineNo: 3, State: st})
		if stats.StateResets.Load() != 1 {
			t.Errorf("%s: resets %d", name, stats.StateResets.Load())
		}
		if !reflect.DeepEqual(view(c.Messages), view(full.Messages)) || next.Offset != fullCur.Offset || !bytes.Equal(next.State, fullCur.State) {
			t.Errorf("%s: restarted parse differs from a full parse", name)
		}
	}
}

// P4: cursor state holds where calls are, not their text, so it stays
// small however large the calls are; resumed parses still match.
func TestCursorStateStaysSmall(t *testing.T) {
	big := strings.Repeat("x", 20000)
	lines := []string{rec(0, "session_meta", `{"id":"s-state","cwd":"/x","source":"cli"}`)}
	ord := 1
	for turn := range 3 {
		tid := fmt.Sprintf("turn-%d", turn)
		lines = append(lines, rec(ord, "event_msg", `{"type":"task_started","turn_id":"`+tid+`"}`))
		ord++
		for i := range 10 {
			cid := fmt.Sprintf("c%d_%d", turn, i)
			lines = append(lines,
				rec(ord, "response_item", `{"type":"function_call","id":"fc_`+cid+`","name":"exec_command","arguments":"{\"cmd\":\"echo `+cid+` `+big+`\"}","call_id":"`+cid+`","internal_chat_message_metadata_passthrough":{"turn_id":"`+tid+`"}}`),
				rec(ord+1, "event_msg", `{"type":"item_completed","turn_id":"`+tid+`","item":{"type":"CommandExecution","id":"e_`+cid+`","command":["zsh","-lc","echo `+cid+` `+big+`"],"exit_code":0}}`),
				rec(ord+2, "response_item", `{"type":"function_call_output","id":"fco_`+cid+`","call_id":"`+cid+`","output":"ok"}`),
			)
			ord += 3
		}
		// A background command finishes after its call's output: late event.
		lines = append(lines, rec(ord, "event_msg", `{"type":"item_completed","turn_id":"`+tid+`","item":{"type":"CommandExecution","id":"late_`+tid+`","command":["zsh","-lc","echo c`+fmt.Sprint(turn)+`_3 `+big+`"],"exit_code":2}}`))
		ord++
	}
	data := []byte(strings.Join(lines, "\n") + "\n")
	full, cur := parseData(t, nil, "", data, transcript.Cursor{})
	if len(cur.State) > 4096 {
		t.Fatalf("cursor state %d bytes for 30 calls of 20KB", len(cur.State))
	}
	// The late event re-emits its call with both commands.
	var late *transcript.Message
	for _, m := range full.Messages {
		if m.NativeID == "fc_c1_3" {
			late = m
		}
	}
	if cmds, _ := late.Enrichment["commands"].([]Command); len(cmds) != 2 || !late.IsError || !strings.HasPrefix(late.Text, `{"cmd":"echo c1_3`) {
		t.Fatalf("late call %+v", late.Enrichment)
	}
	src := transcript.Source{Agent: transcript.AgentCodex}
	for off := 0; off < len(data); off += len(data) / 7 {
		cut := int64(bytes.LastIndexByte(data[:off], '\n') + 1)
		assertResumeMatches(t, &src, data, []int64{cut})
	}
}

// D11: a fork with no history marker skips the parent history it copied,
// by the parent's payload ids and turn ids, and indexes its own turns.
func TestForkWithoutMarkerSkipsCopiedHistory(t *testing.T) {
	home := t.TempDir()
	parentID, childID := "019f0000-0000-7000-8000-00000000f001", "019f0000-0000-7000-8000-00000000f002"
	parent := []string{
		rec(0, "session_meta", `{"id":"`+parentID+`","cwd":"/x","source":"cli"}`),
		rec(1, "event_msg", `{"type":"task_started","turn_id":"pt1"}`),
		rec(2, "response_item", `{"type":"message","role":"user","content":[{"type":"input_text","text":"parent prompt"}]}`),
		rec(3, "event_msg", `{"type":"user_message","message":"parent prompt","turn_id":"pt1"}`),
		rec(4, "response_item", `{"type":"function_call","id":"fc_p1","name":"exec_command","arguments":"{}","call_id":"cp1"}`),
		rec(5, "response_item", `{"type":"function_call_output","call_id":"cp1","output":"parent output"}`),
		rec(6, "response_item", `{"type":"message","id":"msg_p1","role":"assistant","content":[{"type":"output_text","text":"parent answer"}]}`),
		rec(7, "event_msg", `{"type":"task_complete","turn_id":"pt1"}`),
	}
	child := []string{rec(0, "session_meta", `{"id":"`+childID+`","forked_from_id":"`+parentID+`","cwd":"/x","source":"cli"}`)}
	child = append(child, parent[1:]...)
	child = append(child,
		rec(8, "event_msg", `{"type":"task_started","turn_id":"ct1"}`),
		rec(9, "turn_context", `{"turn_id":"ct1"}`),
		rec(10, "response_item", `{"type":"message","role":"user","content":[{"type":"input_text","text":"child prompt"}]}`),
		rec(11, "response_item", `{"type":"message","id":"msg_c1","role":"assistant","content":[{"type":"output_text","text":"child answer"}]}`),
	)
	dir := filepath.Join(home, SessionsDir, "2026", "09", "15")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	parentPath := filepath.Join(dir, "rollout-2026-09-15T10-00-00-"+parentID+".jsonl")
	childPath := filepath.Join(dir, "rollout-2026-09-15T11-00-00-"+childID+".jsonl")
	if err := os.WriteFile(parentPath, []byte(strings.Join(parent, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	data := []byte(strings.Join(child, "\n") + "\n")
	stats := &Stats{}
	c, _ := parseData(t, &Parser{Stats: stats}, childPath, data, transcript.Cursor{})
	var texts []string
	for _, m := range c.Messages {
		texts = append(texts, m.Text)
	}
	if want := []string{"child prompt", "child answer"}; !reflect.DeepEqual(texts, want) {
		t.Fatalf("rows %q, want %q", texts, want)
	}
	if lastConv(c).Title != "child prompt" || stats.ForkPrefixSkipped.Load() == 0 {
		t.Errorf("title %q skipped %d", lastConv(c).Title, stats.ForkPrefixSkipped.Load())
	}
	src := transcript.Source{Agent: transcript.AgentCodex, Path: childPath}
	for i := range data {
		if data[i] == '\n' {
			assertResumeMatches(t, &src, data, []int64{int64(i + 1)})
		}
	}
	// Without the parent the copied history stays indexed.
	os.Remove(parentPath)
	c, _ = parseData(t, nil, childPath, data, transcript.Cursor{})
	if len(c.Messages) < 5 {
		t.Errorf("parent missing: %d rows", len(c.Messages))
	}
}

// Codex can archive a fork parent between the locate and the open: the
// open then finds nothing at the located path, and the parent must be
// located again (now under archived_sessions) before it counts as missing.
func TestOpenLocalRolloutRelocatesArchivedParent(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".codex")
	day := filepath.Join(home, SessionsDir, "2026", "09", "15")
	arch := filepath.Join(home, ArchivedDir)
	for _, d := range []string{day, arch} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	const id = "0199aaaa-bbbb-7ccc-8ddd-eeeeeeeeeeee"
	name := "rollout-2026-09-15T10-00-00-" + id + ".jsonl"
	child := filepath.Join(day, "rollout-2026-09-15T11-00-00-child.jsonl")
	if err := os.WriteFile(filepath.Join(arch, name), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := 0
	defer func(f func(string, string) string) { findRollout = f }(findRollout)
	findRollout = func(c, s string) string {
		if calls++; calls == 1 {
			return filepath.Join(day, name) // located before the move
		}
		return FindRollout(c, s)
	}
	f, err := openLocalRollout(child, id)
	if err != nil {
		t.Fatalf("parent archived between locate and open: %v", err)
	}
	f.Close()
}
