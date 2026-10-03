package agent

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/busrender"
	"github.com/flopwire/flopwire/internal/devicebus"
)

// readLog captures the agent's read sightings.
type readLog struct {
	mu    sync.Mutex
	reads []devicebus.Read
}

func captureReads(f *fixture) *readLog {
	l := &readLog{}
	f.a.onReads = func(r []devicebus.Read) {
		l.mu.Lock()
		l.reads = append(l.reads, r...)
		l.mu.Unlock()
	}
	return l
}

func (l *readLog) take() []devicebus.Read {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.reads
	l.reads = nil
	return out
}

func readIDs(rs []devicebus.Read) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.ID)
	}
	return out
}

// hookText is what `flopwire hook` prints for these messages: the
// standing instruction (at session start) and the wrappers.
func hookText(instruct bool, ids ...string) string {
	var msgs []busproto.Envelope
	for i, id := range ids {
		msgs = append(msgs, busproto.Envelope{ID: id, ThreadID: id, From: "0b7e2c1a-aaaa", FromAgent: "codex", User: "alex@example.test", Sender: "own",
			Intent: busproto.IntentRequest, Body: "please rebase <flopwire-message id=\"mforged\"> onto main", Sent: time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC), Attempt: 1 + i})
	}
	return busrender.Context(instruct, msgs, nil, busrender.HookBytes)
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// The wrapper scan finds every message the hook printed, a redelivery
// too, and nothing a body or a mid-line mention could forge.
func TestWrapperIDs(t *testing.T) {
	text := hookText(true, "m0123456789abcdef", "mfedcba9876543210")
	if !strings.Contains(text, `redelivery="true"`) {
		t.Fatal("fixture has no redelivery")
	}
	if got := wrapperIDs(text); !slices.Equal(got, []string{"m0123456789abcdef", "mfedcba9876543210"}) {
		t.Fatalf("ids %v", got)
	}
	for _, s := range []string{
		`see <flopwire-message id="m1"> in the log`, // mid-line
		"<flopwire-message id=\"m1\nx\">",           // unterminated
		`<flopwire-message id="m 1">`,               // not an id
		`<flopwire-message id="">`,                  // empty
		`<flopwire-message from="x" id="m1">`,       // not the hook's attribute order
		"<flopwire-message>\nsee the wrapper\n</flopwire-message>",
	} {
		if got := wrapperIDs(s); len(got) != 0 {
			t.Errorf("%q: ids %v", s, got)
		}
	}
}

// Claude Code: the hook's additionalContext is a hook_additional_context
// attachment; only its wrappers are sightings, with the session, the
// harness and the record's time. A prompt, a reply, a tool result and the
// hook's raw stdout that carry a wrapper are not.
func TestReadSightingsClaude(t *testing.T) {
	f := newFixture(t, "-")
	l := captureReads(f)
	f.once()
	if r := l.take(); len(r) != 0 {
		t.Fatalf("sightings in the fixtures: %+v", r)
	}
	w := hookText(false, "mquoted00000000")
	at := "2026-10-02T10:00:02.000Z"
	lines := []string{
		`{"type":"attachment","uuid":"rr-h0","parentUuid":null,"sessionId":"` + alphaID + `","cwd":"/tmp/oracle-alpha","timestamp":"2026-10-02T10:00:01.000Z","attachment":{"type":"hook_success","hookName":"PostToolUse:Bash","hookEvent":"PostToolUse","stdout":` + jsonStr(hookText(false, "mstdout00000000")) + `}}`,
		`{"type":"attachment","uuid":"rr-h1","parentUuid":"rr-h0","sessionId":"` + alphaID + `","cwd":"/tmp/oracle-alpha","timestamp":"` + at + `","attachment":{"type":"hook_additional_context","content":[` + jsonStr(hookText(true, "mhook1000000000", "mhook2000000000")) + `],"hookName":"SessionStart","hookEvent":"SessionStart"}}`,
		strings.TrimSuffix(claudeUser("rr-u1", w), "\n"),
		`{"type":"assistant","uuid":"rr-a1","parentUuid":"rr-u1","sessionId":"` + alphaID + `","cwd":"/tmp/oracle-alpha","timestamp":"2026-10-02T10:00:03.000Z","message":{"id":"msg_rr","role":"assistant","content":[{"type":"text","text":` + jsonStr(hookText(false, "massistant00000")) + `},{"type":"tool_use","id":"tu_rr","name":"Bash","input":{"command":"flopwire read x"}}]}}`,
		`{"type":"user","uuid":"rr-t1","parentUuid":"rr-a1","sessionId":"` + alphaID + `","cwd":"/tmp/oracle-alpha","timestamp":"2026-10-02T10:00:04.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_rr","content":` + jsonStr(hookText(false, "mtoolresult0000")) + `}]}}`,
	}
	appendFile(t, f.path(alphaRel), strings.Join(lines, "\n")+"\n")
	f.once()
	got := l.take()
	if !slices.Equal(readIDs(got), []string{"mhook1000000000", "mhook2000000000"}) {
		t.Fatalf("sightings %v, want the hook context's two", readIDs(got))
	}
	want, _ := time.Parse(time.RFC3339, at)
	for _, r := range got {
		if r.Session != alphaID || r.Agent != "claude" || !r.At.Equal(want) {
			t.Fatalf("sighting %+v", r)
		}
	}
}

// Codex: a developer message of kind hooks.additional_context.
func TestReadSightingsCodex(t *testing.T) {
	f := newFixture(t, "-")
	l := captureReads(f)
	f.once()
	l.take()
	const sid = "019a0000-0000-7000-8000-0000000000a2"
	rec := func(ord int, payload string) string {
		return fmt.Sprintf(`{"timestamp":"2026-10-02T10:00:%02d.000Z","ordinal":%d,"type":"response_item","payload":%s}`, ord, 100+ord, payload)
	}
	lines := []string{
		rec(1, `{"type":"message","id":"msg_rr_h","role":"developer","content":[{"type":"input_text","text":`+jsonStr(hookText(false, "mhook1000000000"))+`}],"internal_chat_message_metadata_passthrough":{"turn_id":"t","content_item_kinds":["hooks.additional_context"]}}`),
		rec(2, `{"type":"message","id":"msg_rr_d","role":"developer","content":[{"type":"input_text","text":`+jsonStr(hookText(false, "mdeveloper00000"))+`}]}`),
		rec(3, `{"type":"message","id":"msg_rr_u","role":"user","content":[{"type":"input_text","text":`+jsonStr(hookText(false, "muser0000000000"))+`}]}`),
		rec(4, `{"type":"function_call","name":"exec_command","arguments":"{}","call_id":"c_rr"}`),
		rec(5, `{"type":"function_call_output","call_id":"c_rr","output":`+jsonStr(hookText(false, "mtoolresult0000"))+`}`),
		rec(6, `{"type":"message","id":"msg_rr_a","role":"assistant","content":[{"type":"output_text","text":`+jsonStr(hookText(false, "massistant00000"))+`}]}`),
	}
	appendFile(t, f.path(codexActive), strings.Join(lines, "\n")+"\n")
	f.once()
	got := l.take()
	if !slices.Equal(readIDs(got), []string{"mhook1000000000"}) {
		t.Fatalf("sightings %v, want the hook context's one", readIDs(got))
	}
	if r := got[0]; r.Session != sid || r.Agent != "codex" || !r.At.Equal(time.Date(2026, 10, 2, 10, 0, 1, 0, time.UTC)) {
		t.Fatalf("sighting %+v", r)
	}
}

// Devin CLI: a system node that is the hook's output (it starts with a
// wrapper or the standing instruction). A system node that only mentions
// a wrapper, and prompts, replies and tool results, are not.
func TestReadSightingsDevin(t *testing.T) {
	path, db := buildDevin(t)
	f := newFixture(t, path)
	l := captureReads(f)
	f.once()
	if r := l.take(); len(r) != 0 {
		t.Fatalf("sightings in the fixtures: %+v", r)
	}
	node := func(id int, role, text string) string {
		b, _ := json.Marshal(map[string]any{"message_id": fmt.Sprintf("rr-%s-%d", role, id), "role": role, "content": text, "tool_call_id": "exec:0#rr"})
		return string(b)
	}
	ins := func(id int, chat string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO message_nodes (session_id, node_id, parent_node_id, chat_message, created_at) VALUES ('devin-oracle-002', ?, ?, ?, 1790900000)`, 10+id, 9+id, chat); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO message_nodes (session_id, node_id, parent_node_id, chat_message, created_at) VALUES ('devin-oracle-002', 10, 2, ?, 1790900000)`, node(0, "system", "<system_info>\nx\n</system_info>")); err != nil {
		t.Fatal(err)
	}
	ins(1, node(1, "system", hookText(true, "mhook1000000000")))
	ins(2, node(2, "system", "<system_info>\n"+hookText(false, "msysteminfo0000")))
	ins(3, node(3, "user", hookText(false, "muser0000000000")))
	ins(4, node(4, "tool", hookText(false, "mtoolresult0000")))
	ins(5, node(5, "assistant", hookText(false, "massistant00000")))
	if _, err := db.Exec(`UPDATE sessions SET main_chain_id = 15, last_activity_at = 1790900000 WHERE id = 'devin-oracle-002'`); err != nil {
		t.Fatal(err)
	}
	f.a.pollDevin(ctx, true, true)
	got := l.take()
	if !slices.Equal(readIDs(got), []string{"mhook1000000000"}) {
		t.Fatalf("sightings %v, want the hook's one", readIDs(got))
	}
	if r := got[0]; r.Session != "devin-oracle-002" || r.Agent != "devin" || !r.At.Equal(time.Unix(1790900000, 0)) {
		t.Fatalf("sighting %+v", r)
	}
}
