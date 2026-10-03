package opencode

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/opencode/opencodetest"
)

const t0 = opencodetest.T0

var oid = opencodetest.ID

type fixture struct {
	*opencodetest.Store
	path string
}

func newFixture(t *testing.T) *fixture {
	s := opencodetest.New(t, "")
	return &fixture{Store: s, path: s.Path}
}

func (f *fixture) exec(q string, args ...any) { f.Exec(q, args...) }
func (f *fixture) session(id, parent, dir, title string, at int64) {
	f.Session(id, parent, dir, title, at)
}
func (f *fixture) message(id, session string, at int64, data string) {
	f.Message(id, session, at, data)
}
func (f *fixture) part(id, msg, session string, at int64, d string) { f.Part(id, msg, session, at, d) }
func (f *fixture) updatePart(id string, at int64, data string)      { f.UpdatePart(id, at, data) }

func parse(t *testing.T, path string, cur transcript.Cursor) (*transcript.Collector, transcript.Cursor) {
	t.Helper()
	c := &transcript.Collector{}
	p := &Parser{}
	src := &transcript.Source{Agent: transcript.AgentOpencode, Path: path, StorageKind: transcript.StorageSQLite, Parser: p.Name()}
	next, err := p.Parse(context.Background(), transcript.Input{Source: src}, cur, c)
	if err != nil {
		t.Fatal(err)
	}
	return c, next
}

const (
	sesA = "ses_synthetic0000000000000A"
	sesB = "ses_synthetic0000000000000B"
	cwd  = "/work/demo"
)

var (
	msgU1 = oid("msg", t0, 1)
	msgA1 = oid("msg", t0+10000, 1)
	prtU1 = oid("prt", t0, 2)
	prtU2 = oid("prt", t0, 3)
	prtU3 = oid("prt", t0, 4)
	prtA1 = oid("prt", t0+20000, 1)
	prtA2 = oid("prt", t0+21000, 1)
	prtA3 = oid("prt", t0+22000, 1)
	prtA4 = oid("prt", t0+23000, 1)
	prtA5 = oid("prt", t0+24000, 1)
	prtA6 = oid("prt", t0+40000, 1)
)

const assistantData = `{"parentID":"x","role":"assistant","mode":"build","agent":"build","path":{"cwd":"/work/demo","root":"/work/demo"},"modelID":"big-pickle","providerID":"opencode","time":{"created":1}}`

// seed writes one session: a prompt with a synthetic file read and an
// attachment, then an assistant turn with reasoning, text, a failed bash
// call, a running read and a step-finish.
func seed(f *fixture) {
	f.session(sesA, "", cwd, "Fix the flaky test", t0)
	f.message(msgU1, sesA, t0, `{"role":"user","time":{"created":1},"agent":"build","model":{"providerID":"opencode","modelID":"big-pickle"}}`)
	f.part(prtU1, msgU1, sesA, t0, `{"type":"text","text":"why does the widget test flake?"}`)
	f.part(prtU2, msgU1, sesA, t0, `{"type":"text","text":"Called the Read tool with widget_test.go","synthetic":true}`)
	f.part(prtU3, msgU1, sesA, t0, `{"type":"file","mime":"text/plain","filename":"widget_test.go","url":"file:///work/demo/widget_test.go"}`)
	f.message(msgA1, sesA, t0+10000, assistantData)
	f.part(prtA1, msgA1, sesA, t0+20000, `{"type":"step-start","snapshot":"abc"}`)
	f.part(prtA2, msgA1, sesA, t0+21000, `{"type":"reasoning","text":"Look at the timer.","time":{"start":1791000021000}}`)
	f.part(prtA3, msgA1, sesA, t0+22000, `{"type":"text","text":"Running the test."}`)
	f.part(prtA4, msgA1, sesA, t0+23000, `{"type":"tool","tool":"bash","callID":"call_1","state":{"status":"completed","input":{"command":"go test ./widget"},"output":"FAIL widget","metadata":{"exit":1},"title":"go test"}}`)
	f.part(prtA5, msgA1, sesA, t0+24000, `{"type":"tool","tool":"read","callID":"call_2","state":{"status":"running","input":{"filePath":"/work/demo/widget.go"}}}`)
	f.part(prtA6, msgA1, sesA, t0+40000, `{"type":"step-finish","reason":"tool-calls"}`)
}

type row struct {
	kind   transcript.Kind
	native string
	part   int
	text   string
}

func rows(ms []*transcript.Message) []row {
	ms = slices.Clone(ms)
	slices.SortStableFunc(ms, func(a, b *transcript.Message) int { return int(a.Ordinal - b.Ordinal) })
	var out []row
	for _, m := range ms {
		out = append(out, row{m.Kind, m.NativeID, m.Part, m.Text})
	}
	return out
}

func TestParseSession(t *testing.T) {
	f := newFixture(t)
	seed(f)
	c, _ := parse(t, f.path, transcript.Cursor{})
	if len(c.Conversations) != 1 {
		t.Fatalf("conversations = %d", len(c.Conversations))
	}
	conv := c.Conversations[0]
	if conv.Agent != transcript.AgentOpencode || conv.SessionID != sesA || conv.Cwd != cwd || conv.Title != "Fix the flaky test" ||
		conv.Extra["model"] != "opencode/big-pickle" || conv.StartedAt.UnixMilli() != t0 || conv.ParentSessionID != "" {
		t.Fatalf("conversation = %+v", conv)
	}
	want := []row{
		{transcript.KindUser, prtU1, 0, "why does the widget test flake?"},
		{transcript.KindInjected, prtU2, 0, "Called the Read tool with widget_test.go"},
		{transcript.KindInjected, prtU3, 0, "[file] widget_test.go file:///work/demo/widget_test.go (text/plain)"},
		{transcript.KindThinking, prtA2, 0, "Look at the timer."},
		{transcript.KindAssistant, prtA3, 0, "Running the test."},
		{transcript.KindToolCall, prtA4, 0, "bash\n{\"command\":\"go test ./widget\"}"},
		{transcript.KindToolResult, prtA4, 1, "FAIL widget"},
		{transcript.KindToolCall, prtA5, 0, "read\n{\"filePath\":\"/work/demo/widget.go\"}"},
	}
	if got := rows(c.Messages); !slices.Equal(got, want) {
		t.Fatalf("rows:\n got %v\nwant %v", got, want)
	}
	for _, m := range c.Messages {
		switch {
		case m.NativeID == prtA4:
			if !m.IsError || m.ToolName != "bash" || m.ToolCallID != "call_1" {
				t.Errorf("bash row %+v", m)
			}
			if m.Kind == transcript.KindToolCall {
				cmds := m.Enrichment["commands"].([]map[string]any)
				if cmds[0]["cwd"] != cwd || cmds[0]["exit_code"] != int64(1) {
					t.Errorf("commands = %v", cmds)
				}
			}
		case m.NativeID == prtA5:
			if m.Enrichment["status"] != "running" || !slices.Equal(m.Enrichment["paths"].([]string), []string{"/work/demo/widget.go"}) {
				t.Errorf("read enrichment = %v", m.Enrichment)
			}
		case m.NativeID == prtA2:
			if m.TS.UnixMilli() != t0+21000 {
				t.Errorf("thinking ts = %v", m.TS)
			}
		}
		if m.Parser != Name || m.SessionID != sesA {
			t.Errorf("row %s: parser %q session %q", m.NativeID, m.Parser, m.SessionID)
		}
	}
}

func TestIncremental(t *testing.T) {
	f := newFixture(t)
	seed(f)
	_, cur := parse(t, f.path, transcript.Cursor{})

	c, cur := parse(t, f.path, cur)
	if len(c.Messages)+len(c.Conversations)+len(c.SupersededSessions) != 0 {
		t.Fatalf("unchanged store emitted %d rows, %d conversations", len(c.Messages), len(c.Conversations))
	}

	// The running read completes well after the watermark: only its rows.
	f.updatePart(prtA5, t0+60000, `{"type":"tool","tool":"read","callID":"call_2","state":{"status":"completed","input":{"filePath":"/work/demo/widget.go"},"output":"package widget"}}`)
	c, cur = parse(t, f.path, cur)
	want := []row{
		{transcript.KindToolCall, prtA5, 0, "read\n{\"filePath\":\"/work/demo/widget.go\"}"},
		{transcript.KindToolResult, prtA5, 1, "package widget"},
	}
	if got := rows(c.Messages); !slices.Equal(got, want) {
		t.Fatalf("after update:\n got %v\nwant %v", got, want)
	}

	// A new prompt: its rows only (and the read again, inside the lag).
	msgU2, prtU4 := oid("msg", t0+61000, 1), oid("prt", t0+61000, 2)
	f.message(msgU2, sesA, t0+61000, `{"role":"user","time":{"created":1}}`)
	f.part(prtU4, msgU2, sesA, t0+61000, `{"type":"text","text":"now fix it"}`)
	c, _ = parse(t, f.path, cur)
	var natives []string
	for _, m := range c.Messages {
		natives = append(natives, m.NativeID)
	}
	if !slices.Contains(natives, prtU4) || slices.Contains(natives, prtU1) || slices.Contains(natives, prtA4) {
		t.Fatalf("after a new prompt emitted %v", natives)
	}
}

// A part written with an older time_updated than the watermark, but within
// the lag, is still picked up.
func TestIncrementalLateCommit(t *testing.T) {
	f := newFixture(t)
	seed(f)
	_, cur := parse(t, f.path, transcript.Cursor{})
	f.updatePart(prtA3, t0+39000, `{"type":"text","text":"Running the test now."}`)
	c, _ := parse(t, f.path, cur)
	found := false
	for _, m := range c.Messages {
		found = found || m.NativeID == prtA3 && m.Text == "Running the test now."
	}
	if !found {
		t.Fatal("late commit inside the lag was missed")
	}
}

func TestRevertSupersedes(t *testing.T) {
	f := newFixture(t)
	seed(f)
	_, cur := parse(t, f.path, transcript.Cursor{})
	f.exec(`DELETE FROM part WHERE id = ?`, prtA5)
	c, _ := parse(t, f.path, cur)
	if !slices.Equal(c.SupersededSessions, []string{sesA}) {
		t.Fatalf("superseded = %v", c.SupersededSessions)
	}
	var natives []string
	for _, m := range c.Messages {
		natives = append(natives, m.NativeID)
		if m.Superseded {
			t.Errorf("re-emitted row %s is superseded", m.NativeID)
		}
	}
	if slices.Contains(natives, prtA5) || !slices.Contains(natives, prtU1) || !slices.Contains(natives, prtA4) {
		t.Fatalf("re-emitted %v", natives)
	}
}

func TestVanishedSession(t *testing.T) {
	f := newFixture(t)
	seed(f)
	_, cur := parse(t, f.path, transcript.Cursor{})
	f.exec(`DELETE FROM part`)
	f.exec(`DELETE FROM message`)
	f.exec(`DELETE FROM session`)
	c, _ := parse(t, f.path, cur)
	if !slices.Equal(c.SupersededSessions, []string{sesA}) || len(c.Messages) != 0 {
		t.Fatalf("superseded %v, rows %d", c.SupersededSessions, len(c.Messages))
	}
}

func TestMessageError(t *testing.T) {
	f := newFixture(t)
	seed(f)
	f.exec(`UPDATE message SET data = ?, time_updated = ? WHERE id = ?`,
		`{"role":"assistant","error":{"name":"MessageAbortedError","data":{"message":"The operation was aborted."}}}`, t0+50000, msgA1)
	c, _ := parse(t, f.path, transcript.Cursor{})
	got := rows(c.Messages)
	last := got[len(got)-1]
	if (last != row{transcript.KindSystem, msgA1, 0, "MessageAbortedError: The operation was aborted."}) {
		t.Fatalf("last row = %+v", last)
	}
}

// A subagent session links to its parent's task call, also when the call
// names the child only after the child's first parse.
func TestSubagentLink(t *testing.T) {
	f := newFixture(t)
	seed(f)
	f.session(sesB, sesA, cwd, "Explore the widget (@explore subagent)", t0+30)
	msgB, prtB := oid("msg", t0+31, 1), oid("prt", t0+31, 2)
	f.message(msgB, sesB, t0+31, `{"role":"user","time":{"created":1}}`)
	f.part(prtB, msgB, sesB, t0+31, `{"type":"text","text":"explore widget.go"}`)
	c, cur := parse(t, f.path, transcript.Cursor{})
	child := conv(c, sesB)
	if child == nil || child.ParentSessionID != sesA || child.Depth != 1 || child.SpawnedByToolCallID != "" {
		t.Fatalf("child before the call = %+v", child)
	}

	prtTask := oid("prt", t0+29, 1)
	f.part(prtTask, msgA1, sesA, t0+40, `{"type":"tool","tool":"task","callID":"call_t","state":{"status":"running","input":{"prompt":"explore"},"metadata":{"sessionId":"`+sesB+`"}}}`)
	prtB2 := oid("prt", t0+50, 1)
	f.part(prtB2, msgB, sesB, t0+50, `{"type":"text","text":"more"}`)
	c, cur = parse(t, f.path, cur)
	child = conv(c, sesB)
	if child == nil || child.SpawnedByToolCallID != prtTask {
		t.Fatalf("child after the call = %+v", child)
	}
	for _, m := range c.Messages {
		if m.NativeID == prtTask && m.Kind == transcript.KindToolCall && m.Enrichment["subagent_session"] != sesB {
			t.Errorf("task enrichment = %v", m.Enrichment)
		}
	}
	// Found once: an unchanged child is not looked up or re-emitted again.
	c, _ = parse(t, f.path, cur)
	if conv(c, sesB) != nil {
		t.Fatal("unchanged child re-emitted")
	}
}

func conv(c *transcript.Collector, id string) *transcript.Conversation {
	var out *transcript.Conversation
	for _, v := range c.Conversations {
		if v.SessionID == id {
			out = v
		}
	}
	return out
}

func TestExportRoundTrip(t *testing.T) {
	f := newFixture(t)
	seed(f)
	f.session(sesB, sesA, cwd, "child", t0+30)
	msgB, prtB := oid("msg", t0+31, 1), oid("prt", t0+31, 2)
	f.message(msgB, sesB, t0+31, `{"role":"user","time":{"created":1}}`)
	f.part(prtB, msgB, sesB, t0+31, `{"type":"text","text":"explore widget.go"}`)
	prtTask := oid("prt", t0+29, 1)
	f.part(prtTask, msgA1, sesA, t0+29, `{"type":"tool","tool":"task","callID":"call_t","state":{"status":"running","input":{},"metadata":{"sessionId":"`+sesB+`"}}}`)
	direct, _ := parse(t, f.path, transcript.Cursor{})
	ctx := context.Background()
	ids, err := ListSessions(ctx, f.path)
	if err != nil || !slices.Equal(ids, []string{sesA, sesB}) {
		t.Fatalf("sessions = %v, %v", ids, err)
	}
	for _, id := range ids {
		b, err := Export(ctx, f.path, id)
		if err != nil {
			t.Fatal(err)
		}
		db, err := LoadExport(ctx, bytes.NewReader(b), id, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		got, _ := parse(t, db, transcript.Cursor{})
		if !slices.Equal(rows(got.Messages), rows(direct.MessagesFor(id))) {
			t.Fatalf("%s: export rows differ:\n got %v\nwant %v", id, rows(got.Messages), rows(direct.MessagesFor(id)))
		}
		dc, gc := conv(direct, id), conv(got, id)
		if gc == nil || gc.ParentSessionID != dc.ParentSessionID || gc.SpawnedByToolCallID != dc.SpawnedByToolCallID ||
			gc.Cwd != dc.Cwd || gc.Title != dc.Title || gc.Extra["model"] != dc.Extra["model"] {
			t.Fatalf("%s: conversation %+v, want %+v", id, gc, dc)
		}
	}
	if conv(direct, sesB).SpawnedByToolCallID != prtTask {
		t.Fatal("direct parse lost the spawning call")
	}
	b, err := Export(ctx, f.path, "ses_gone")
	if err != nil || !strings.Contains(string(b), `"t":"gone"`) {
		t.Fatalf("gone export = %q, %v", b, err)
	}
}

func TestIDOrder(t *testing.T) {
	for _, ms := range []int64{t0, 1786706395136 - 5, 1786706395136 + 5} { // around a 2^36 ms wrap
		id := oid("prt", ms, 7)
		for _, ref := range []int64{ms - 700, ms + 700} {
			if got, c := idOrder(id, ref); got != ms || c != 7 {
				t.Errorf("idOrder(%s, %d) = %d, %d; want %d, 7", id, ref, got, c, ms)
			}
		}
	}
	if ms, c := idOrder("prt_custom", 42); ms != 42 || c != 0 {
		t.Errorf("custom id = %d, %d", ms, c)
	}
}

func TestPathFrom(t *testing.T) {
	env := func(kv ...string) func(string) string {
		return func(k string) string {
			for i := 0; i+1 < len(kv); i += 2 {
				if kv[i] == k {
					return kv[i+1]
				}
			}
			return ""
		}
	}
	for _, c := range []struct {
		env  func(string) string
		want string
	}{
		{env(), "/h/.local/share/opencode/opencode.db"},
		{env("XDG_DATA_HOME", "/x"), "/x/opencode/opencode.db"},
		{env("OPENCODE_DB", ":memory:", "XDG_DATA_HOME", "/x"), "/x/opencode/opencode.db"},
		{env("OPENCODE_DB", "/o.db", "XDG_DATA_HOME", "/x"), "/o.db"},
		{env(EnvDB, "/f.db", "OPENCODE_DB", "/o.db"), "/f.db"},
	} {
		if got := PathFrom(c.env, "/h"); got != c.want {
			t.Errorf("PathFrom = %q, want %q", got, c.want)
		}
	}
}

// A message the Flopwire plugin delivered (part metadata flopwire.id) is
// hook context; the same text typed as a prompt, or in a reply, is not.
func TestDeliveredMessageIsHookContext(t *testing.T) {
	f := newFixture(t)
	seed(f)
	wrapper := `<flopwire-message id=\"m1\" from=\"bo\">ping</flopwire-message>`
	msgD, prtD := oid("msg", t0+60000, 1), oid("prt", t0+60000, 2)
	f.message(msgD, sesA, t0+60000, `{"role":"user"}`)
	f.part(prtD, msgD, sesA, t0+60000, `{"type":"text","text":"`+wrapper+`","metadata":{"flopwire":{"id":"m1"}}}`)
	msgT, prtT := oid("msg", t0+61000, 1), oid("prt", t0+61000, 2)
	f.message(msgT, sesA, t0+61000, `{"role":"user"}`)
	f.part(prtT, msgT, sesA, t0+61000, `{"type":"text","text":"`+wrapper+`"}`)
	prtR := oid("prt", t0+62000, 1)
	f.part(prtR, msgA1, sesA, t0+62000, `{"type":"text","text":"`+wrapper+`","metadata":{"flopwire":{"id":"m1"}}}`)
	c, _ := parse(t, f.path, transcript.Cursor{})
	for _, m := range c.Messages {
		want := m.NativeID == prtD
		if got := transcript.HookContext(transcript.AgentOpencode, m); got != want {
			t.Errorf("%s (%s): HookContext = %v, want %v", m.NativeID, m.Kind, got, want)
		}
		if m.NativeID == prtD && (m.Kind != transcript.KindInjected || m.Enrichment[transcript.EnrichHookContext] != DeliveryHook) {
			t.Errorf("delivered row = %s %v", m.Kind, m.Enrichment)
		}
	}
}
