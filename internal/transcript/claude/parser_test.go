package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

const fixtureHome = "../../../testdata/oracle/home"

var (
	projects  = filepath.Join(fixtureHome, ".claude", "projects")
	betaID    = "0b7e2c1a-0000-4000-8000-000000000002"
	betaDir   = filepath.Join(projects, "-tmp-oracle-beta", betaID)
	betaMain  = betaDir + ".jsonl"
	alphaMain = filepath.Join(projects, "-tmp-oracle-alpha", "0b7e2c1a-0000-4000-8000-000000000001.jsonl")
)

func u(n int) string { return "d1000000-0000-4000-8000-" + pad12(n) }

func pad12(n int) string {
	s := strconv.Itoa(n)
	return strings.Repeat("0", 12-len(s)) + s
}

func parseFile(t testing.TB, p *Parser, path string) (*transcript.Collector, transcript.Cursor) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return parseBytes(t, p, path, b, transcript.Cursor{})
}

func parseBytes(t testing.TB, p *Parser, path string, b []byte, cur transcript.Cursor) (*transcript.Collector, transcript.Cursor) {
	t.Helper()
	src := newSource(path, "")
	var c transcript.Collector
	next, err := p.Parse(context.Background(), transcript.Input{Source: &src, R: bytes.NewReader(b), Size: int64(len(b))}, cur, &c)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return &c, next
}

func lastConv(t testing.TB, c *transcript.Collector) *transcript.Conversation {
	t.Helper()
	if len(c.Conversations) == 0 {
		t.Fatal("no conversation emitted")
	}
	return c.Conversations[len(c.Conversations)-1]
}

type rowSpec struct {
	kind   transcript.Kind
	native string
	text   string // prefix
}

func TestBetaRows(t *testing.T) {
	stats := &Stats{}
	c, _ := parseFile(t, &Parser{Stats: stats}, betaMain)
	want := []rowSpec{
		{transcript.KindUser, u(1) + "#0", "the upload retry test is flaky"},
		// line 2 is isMeta: skipped
		{transcript.KindThinking, u(3) + "#0", "Look at retry and the log."},
		{transcript.KindAssistant, u(3) + "#1", "Two things to check in parallel."},
		{transcript.KindToolCall, u(4) + "#0", `{"pattern":"retry","path":"src"}`},
		{transcript.KindToolCall, u(5) + "#0", `{"command":"cat logs/upload.log"`},
		{transcript.KindToolResult, u(6) + "#0", "src/upload.ts:42: retry(3)"},
		{transcript.KindToolResult, u(7) + "#0", "line 0000 ERROR upload failed"},
		{transcript.KindToolCall, u(8) + "#0", `{"pattern":"src/**/*.ts"}`},
		{transcript.KindToolCall, u(8) + "#1", `{"command":"bun test upload"`},
		{transcript.KindToolResult, u(9) + "#0", "src/a.ts\nsrc/upload.ts"},
		{transcript.KindToolResult, u(9) + "#1", "upload.test.ts:9 timeout\nexit code 2"},
		{transcript.KindUser, u(9) + "#3", "also check the CI config"},
		{transcript.KindUser, u(10) + "#0", "use exponential backoff"},
		{transcript.KindAssistant, u(13) + "#0", "Switching to exponential backoff."},
		{transcript.KindToolCall, u(14) + "#0", `{"workflow":"review-retry"`},
		{transcript.KindToolResult, u(15) + "#0", "Workflow review-retry launched"},
		{transcript.KindToolCall, u(16) + "#0", `{"description":"Scan retries"`},
		{transcript.KindToolResult, u(17) + "#0", "Three call sites use fixed delays."},
		{transcript.KindToolCall, u(18) + "#0", `{"todos":`},
		{transcript.KindToolResult, u(19) + "#0", ""},
		{transcript.KindSystem, u(20) + "#0", "Conversation compacted"},
		{transcript.KindSystem, u(21) + "#0", "This session is being continued"},
		{transcript.KindSystem, u(23) + "#0", "Recap: backoff change in progress."},
		{transcript.KindAssistant, u(24) + "#1", "Done: backoff added."},
	}
	if len(c.Messages) != len(want) {
		for _, m := range c.Messages {
			t.Logf("%v %s %.40q", m.Kind, m.NativeID, m.Text)
		}
		t.Fatalf("got %d rows, want %d", len(c.Messages), len(want))
	}
	for i, w := range want {
		m := c.Messages[i]
		if m.Kind != w.kind || m.NativeID != w.native || !strings.HasPrefix(m.Text, w.text) {
			t.Errorf("row %d = %v %s %.60q, want %v %s %.60q", i, m.Kind, m.NativeID, m.Text, w.kind, w.native, w.text)
		}
		if m.SessionID != betaID || m.Parser != ParserName {
			t.Errorf("row %d session %q parser %q", i, m.SessionID, m.Parser)
		}
	}
	byID := map[string]*transcript.Message{}
	for _, m := range c.Messages {
		byID[m.NativeID] = m
	}

	// Parallel calls on separate lines resolve their tool names.
	if r := byID[u(6)+"#0"]; r.ToolCallID != "toolu_b1" || r.ToolName != "Grep" || r.ParentNativeID != u(4) {
		t.Errorf("grep result %+v", r)
	}
	// Multi-block user line: error flag, image skipped, text keeps block index.
	if r := byID[u(9)+"#1"]; !r.IsError || r.ToolName != "Bash" || r.Part != 1 {
		t.Errorf("bash result %+v", r)
	}
	if r := byID[u(9)+"#3"]; r.Part != 3 || r.Ordinal != transcript.OrdinalAt(r.ByteOffset, 3) {
		t.Errorf("text part %+v", r)
	}

	// Persisted output: preview replaced by the capped file.
	full, _ := os.ReadFile(filepath.Join(betaDir, "tool-results", "bpersist01.txt"))
	p := byID[u(7)+"#0"]
	wantText, _ := transcript.Cap(string(full), transcript.ToolCap)
	if p.Text != wantText || p.FullLen != len(full) || p.Enrichment["persisted_output"] != "tool-results/bpersist01.txt" {
		t.Errorf("persisted row: len %d full %d enrichment %v", len(p.Text), p.FullLen, p.Enrichment)
	}
	if strings.Contains(p.Text, persistedMarker) || !strings.Contains(p.Text, "ERROR upload failed") {
		t.Errorf("persisted text not replaced: %.200q", p.Text)
	}
	if stats.Persisted.Load() != 1 || stats.Malformed.Load() != 1 {
		t.Errorf("stats persisted %d malformed %d", stats.Persisted.Load(), stats.Malformed.Load())
	}

	// Subagent linkage facts on the parent's tool results.
	if byID[u(15)+"#0"].Enrichment["workflow_run_id"] != "wf_beta-001" || byID[u(17)+"#0"].Enrichment["agent_id"] != "b000000000000002" {
		t.Errorf("linkage enrichment %v %v", byID[u(15)+"#0"].Enrichment, byID[u(17)+"#0"].Enrichment)
	}
	// Compaction: boundary points at its logical parent; summary indexed.
	if b := byID[u(20)+"#0"]; b.ParentNativeID != u(19) || b.Enrichment["subtype"] != "compact_boundary" {
		t.Errorf("boundary %+v", b)
	}
	if s := byID[u(21)+"#0"]; s.Enrichment["subtype"] != "compact_summary" || s.Role != "system" {
		t.Errorf("summary %+v", s)
	}
	if q := byID[u(10)+"#0"]; q.Enrichment["queued_command"] != "human" {
		t.Errorf("queued %+v", q)
	}
	// Assistant token fields: usage on the first row of a line only.
	th, tx := byID[u(3)+"#0"], byID[u(3)+"#1"]
	if th.Enrichment["message_id"] != "msg_b1" || th.Enrichment["request_id"] != "req_b1" || th.Enrichment["usage"] == nil || tx.Enrichment["usage"] != nil {
		t.Errorf("token fields %v / %v", th.Enrichment, tx.Enrichment)
	}

	conv := lastConv(t, c)
	if conv.SessionID != betaID || conv.Cwd != "/tmp/oracle-beta" || conv.Title != "Exponential backoff for upload retries" ||
		conv.ParentSessionID != "" || conv.Extra["git_branch"] != "fix/retry" {
		t.Errorf("conversation %+v", conv)
	}
	if conv.StartedAt.Format("15:04:05") != "11:00:00" || conv.LastActivityAt.Format("15:04:05") != "11:00:34" {
		t.Errorf("bounds %v %v", conv.StartedAt, conv.LastActivityAt)
	}
	// The conversation precedes the first message: emitted before it and
	// updated at the end.
	if len(c.Conversations) != 2 || c.Conversations[0].SessionID != betaID {
		t.Errorf("conversation records %d", len(c.Conversations))
	}
}

func TestSubagentLinks(t *testing.T) {
	sub := filepath.Join(betaDir, "subagents")
	cases := []struct {
		path, session, parent, spawnedBy, title string
		depth                                   int
		rows                                    int
	}{
		// meta.json has no toolUseId: fallback through toolUseResult.runId.
		{filepath.Join(sub, "workflows", "wf_beta-001", "agent-b000000000000001.jsonl"), "agent-b000000000000001", betaID, "toolu_b5", "Review the retry change for correctness.", 1, 2},
		// no toolUseId, no spawnDepth: fallback through toolUseResult.agentId.
		{filepath.Join(sub, "agent-b000000000000002.jsonl"), "agent-b000000000000002", betaID, "toolu_b7", "Scan retries", 1, 4},
		// nested: parent is the other subagent.
		{filepath.Join(sub, "agent-b000000000000003.jsonl"), "agent-b000000000000003", "agent-b000000000000002", "toolu_n1", "Nested check", 2, 2},
		// alpha fixture: meta.json toolUseId.
		{filepath.Join(filepath.Dir(alphaMain), "0b7e2c1a-0000-4000-8000-000000000001", "subagents", "agent-a1b2c3.jsonl"), "agent-a1b2c3", "0b7e2c1a-0000-4000-8000-000000000001", "toolu_oracle_1", "Read login test", 1, 4},
	}
	for _, tc := range cases {
		c, _ := parseFile(t, &Parser{}, tc.path)
		conv := lastConv(t, c)
		if conv.SessionID != tc.session || conv.ParentSessionID != tc.parent || conv.SpawnedByToolCallID != tc.spawnedBy ||
			conv.Depth != tc.depth || conv.Title != tc.title {
			t.Errorf("%s: %+v", filepath.Base(tc.path), conv)
		}
		if len(c.Messages) != tc.rows {
			t.Errorf("%s: %d rows", filepath.Base(tc.path), len(c.Messages))
		}
		for _, m := range c.Messages {
			if m.SessionID != tc.session {
				t.Errorf("%s: row session %q", filepath.Base(tc.path), m.SessionID)
			}
		}
	}
	wf, _ := parseFile(t, &Parser{}, cases[0].path)
	if e := lastConv(t, wf).Extra; e["workflow_run_id"] != "wf_beta-001" || e["agent_type"] != "workflow-subagent" {
		t.Errorf("workflow extra %v", e)
	}
}

// TestIncrementalMatchesFull resumes at every line boundary, and from
// sizes cut mid-line, and requires the messages of the two calls to equal
// a full parse, with the same final conversation record.
func TestIncrementalMatchesFull(t *testing.T) {
	paths := []string{betaMain, alphaMain}
	subs, _ := filepath.Glob(filepath.Join(betaDir, "subagents", "*.jsonl"))
	paths = append(paths, subs...)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		full, fullCur := parseBytes(t, &Parser{}, path, data, transcript.Cursor{})
		if fullCur.Offset != int64(len(data)) {
			t.Fatalf("%s: full cursor %d of %d", path, fullCur.Offset, len(data))
		}
		for cut := 0; cut <= len(data); cut++ {
			if cut > 0 && cut < len(data) && data[cut-1] != '\n' && cut%97 != 0 {
				continue // every line boundary, plus a sample of mid-line cuts
			}
			first, cur := parseBytes(t, &Parser{}, path, data[:cut], transcript.Cursor{})
			if cur.Offset > int64(cut) || (cut > 0 && cur.Offset > 0 && data[cur.Offset-1] != '\n') {
				t.Fatalf("%s cut %d: cursor %d not at a line boundary", path, cut, cur.Offset)
			}
			rest, end := parseBytes(t, &Parser{}, path, data, cur)
			got := append(append([]*transcript.Message{}, first.Messages...), rest.Messages...)
			if !reflect.DeepEqual(got, full.Messages) {
				t.Fatalf("%s cut %d: incremental messages differ from full parse", path, cut)
			}
			all := append(append([]*transcript.Conversation{}, first.Conversations...), rest.Conversations...)
			if !reflect.DeepEqual(all[len(all)-1], lastConv(t, full)) {
				t.Fatalf("%s cut %d: final conversation %+v, full %+v", path, cut, all[len(all)-1], lastConv(t, full))
			}
			if end.Offset != fullCur.Offset || end.LineNo != fullCur.LineNo || !bytes.Equal(end.State, fullCur.State) {
				t.Fatalf("%s cut %d: end cursor %+v, full %+v", path, cut, end, fullCur)
			}
		}
	}
}

func TestSkipsNonConversationTypes(t *testing.T) {
	lines := []string{
		`{"type":"mode","mode":"normal","sessionId":"s"}`,
		`{"type":"permission-mode","permissionMode":"plan","sessionId":"s"}`,
		`{"type":"last-prompt","lastPrompt":"x","sessionId":"s"}`,
		`{"type":"queue-operation","operation":"enqueue","content":"x","sessionId":"s"}`,
		`{"type":"file-history-snapshot","messageId":"m","snapshot":{}}`,
		`{"type":"file-history-delta","sessionId":"s"}`,
		`{"type":"atis-latch","sessionId":"s"}`, `{"type":"bridge-session","sessionId":"s"}`,
		`{"type":"pr-link","sessionId":"s"}`, `{"type":"agent-name","sessionId":"s"}`,
		`{"type":"artifact-autoreact-ledger","sessionId":"s"}`, `{"type":"artifact-comment-monitor","sessionId":"s"}`,
		`{"type":"frame-link","sessionId":"s"}`, `{"type":"cost-state","sessionId":"s"}`,
		`{"type":"brand-new-type","uuid":"x","message":{"role":"user","content":"hidden"}}`,
		`{"type":"attachment","uuid":"a1","attachment":{"type":"queued_command","prompt":"peer text","commandMode":"prompt","origin":{"kind":"peer"}}}`,
		`{"type":"attachment","uuid":"a2","attachment":{"type":"queued_command","prompt":"old build","commandMode":"prompt"}}`,
		`{"type":"system","subtype":"turn_duration","uuid":"s1","durationMs":5}`,
		`{"type":"user","uuid":"u1","isMeta":true,"message":{"role":"user","content":"meta"}}`,
		`{"type":"user","uuid":"u2","message":{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"AAAA"}}]}}`,
		`[1,2,3]`, `"string line"`, `{"type":`, ``,
		`{"type":"assistant","uuid":"x1","message":{"role":"assistant","content":[{"type":"fallback"},{"type":"text","text":"kept"}]}}`,
		`{"type":"assistant","uuid":"e1","error":"rate_limit","isApiErrorMessage":true,"message":{"role":"assistant","content":[{"type":"text","text":"API Error"}]}}`,
	}
	c, _ := parseBytes(t, &Parser{}, "", []byte(strings.Join(lines, "\n")+"\n"), transcript.Cursor{})
	if len(c.Messages) != 3 || c.Messages[0].NativeID != "a2#0" || c.Messages[0].Enrichment["queued_command"] != "unknown" ||
		c.Messages[1].NativeID != "x1#1" || c.Messages[1].Text != "kept" ||
		c.Messages[2].NativeID != "e1#0" || c.Messages[2].Enrichment["api_error"] != true {
		for _, m := range c.Messages {
			t.Logf("%v %s %q", m.Kind, m.NativeID, m.Text)
		}
		t.Fatalf("got %d rows", len(c.Messages))
	}
}

func TestOversizedLineIsDecoded(t *testing.T) {
	text := strings.Repeat("x", 300)
	line := `{"type":"user","uuid":"big","sessionId":"s","message":{"role":"user","content":"` + text + `"}}` + "\n"
	c, _ := parseBytes(t, &Parser{Lines: transcript.LineReaderOptions{MaxLine: 64, HeadSize: 16}}, "", []byte(line), transcript.Cursor{})
	if len(c.Messages) != 1 || c.Messages[0].Text != text || c.Messages[0].SessionID != "s" {
		t.Fatalf("oversized: %+v", c.Messages)
	}
}

func TestCompactSummaryIndexedOnce(t *testing.T) {
	sum := `{"type":"user","uuid":"%s","isCompactSummary":true,"message":{"role":"user","content":"Summary: same text"}}`
	data := strings.ReplaceAll(sum, "%s", "c1") + "\n" + strings.ReplaceAll(sum, "%s", "c2") + "\n"
	c, _ := parseBytes(t, &Parser{}, "", []byte(data), transcript.Cursor{})
	if len(c.Messages) != 1 || c.Messages[0].NativeID != "c1#0" {
		t.Fatalf("summaries: %d", len(c.Messages))
	}
}

func TestPersistedMissingKeepsPreview(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "0b7e2c1a-0000-4000-8000-0000000000aa.jsonl")
	preview := `<persisted-output>\nOutput too large. Full output saved to: /x/tool-results/gone.txt\n\nPreview (first 2KB):\nhead\n</persisted-output>`
	data := `{"type":"user","uuid":"t1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"` + preview + `"}]}}` + "\n"
	c, _ := parseBytes(t, &Parser{}, path, []byte(data), transcript.Cursor{})
	m := c.Messages[0]
	if !strings.Contains(m.Text, "Preview (first 2KB)") || m.Enrichment["persisted_output_missing"] != "tool-results/gone.txt" {
		t.Fatalf("missing persisted: %+v", m)
	}
}

func TestSumUsage(t *testing.T) {
	c, _ := parseFile(t, &Parser{}, betaMain)
	u := SumUsage(c.Messages)
	// msg_b1 appears on three lines (output 30, 40, 50): counted once at 50.
	// Distinct message ids: b1..b6 = 6 calls.
	if u.APICalls != 6 || u.InputTokens != 600 {
		t.Fatalf("usage %+v", u)
	}
	var out int64
	for _, n := range []int64{50, 80, 140, 160, 180, 240} {
		out += n
	}
	if u.OutputTokens != out {
		t.Fatalf("output tokens %d, want %d", u.OutputTokens, out)
	}
}

// D16: every row id is "<uuid>#<block>", and part and ordinal slot are the
// block index, whether the line yields one row or several.
func TestIDsAndOrdinalsFixedByBlock(t *testing.T) {
	lines := []string{
		`{"type":"user","uuid":"one","sessionId":"s","message":{"role":"user","content":"hello"}}`,
		`{"type":"user","uuid":"img","sessionId":"s","message":{"role":"user","content":[{"type":"image","source":{}},{"type":"text","text":"see image"}]}}`,
		`{"type":"assistant","uuid":"two","sessionId":"s","message":{"role":"assistant","content":[{"type":"redacted_thinking"},{"type":"text","text":"a"},{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}`,
	}
	data := []byte(strings.Join(lines, "\n") + "\n")
	c, _ := parseBytes(t, &Parser{}, "", data, transcript.Cursor{})
	want := []struct {
		id    string
		block int
		line  int
	}{{"one#0", 0, 0}, {"img#1", 1, 1}, {"two#1", 1, 2}, {"two#2", 2, 2}}
	if len(c.Messages) != len(want) {
		t.Fatalf("got %d rows", len(c.Messages))
	}
	off := 0
	offsets := []int64{}
	for _, l := range lines {
		offsets = append(offsets, int64(off))
		off += len(l) + 1
	}
	for i, w := range want {
		m := c.Messages[i]
		if m.NativeID != w.id || m.Part != w.block || m.Ordinal != offsets[w.line]<<12|int64(w.block) {
			t.Errorf("row %d: id %s part %d ordinal %d, want %s %d %d", i, m.NativeID, m.Part, m.Ordinal, w.id, w.block, offsets[w.line]<<12|int64(w.block))
		}
	}
}

// D6: <system-reminder> text in a user turn is kind injected; a block that
// mixes it with a prompt is split so the prompt stays kind user.
func TestInjectedContextSplitFromPrompt(t *testing.T) {
	lines := []string{
		`{"type":"user","uuid":"r1","sessionId":"s","message":{"role":"user","content":"<system-reminder>\nAs you answer the user's questions, you can use the following context:\n# claudeMd\nbe terse\n</system-reminder>"}}`,
		`{"type":"user","uuid":"r2","sessionId":"s","message":{"role":"user","content":[{"type":"text","text":"<system-reminder>hook said hi</system-reminder>\nfix the flaky test\n<system-reminder>second</system-reminder>"}]}}`,
		`{"type":"user","uuid":"r3","sessionId":"s","message":{"role":"user","content":"plain prompt"}}`,
	}
	c, _ := parseBytes(t, &Parser{}, "", []byte(strings.Join(lines, "\n")+"\n"), transcript.Cursor{})
	type row struct {
		kind transcript.Kind
		id   string
		part int
		text string
	}
	want := []row{
		{transcript.KindInjected, "r1#0", 0, "<system-reminder>\nAs you answer"},
		{transcript.KindUser, "r2#0", 0, "fix the flaky test"},
		{transcript.KindInjected, "r2#0:injected", InjectedSlot, "<system-reminder>hook said hi</system-reminder>\n\n<system-reminder>second</system-reminder>"},
		{transcript.KindUser, "r3#0", 0, "plain prompt"},
	}
	if len(c.Messages) != len(want) {
		t.Fatalf("got %d rows", len(c.Messages))
	}
	for i, w := range want {
		m := c.Messages[i]
		if m.Kind != w.kind || m.NativeID != w.id || m.Part != w.part || !strings.HasPrefix(m.Text, w.text) {
			t.Errorf("row %d = %v %s part %d %q, want %v %s %d %q", i, m.Kind, m.NativeID, m.Part, m.Text, w.kind, w.id, w.part, w.text)
		}
	}
	if c.Messages[1].Text != "fix the flaky test" {
		t.Errorf("prompt text %q", c.Messages[1].Text)
	}
	if c.Messages[1].Ordinal == c.Messages[2].Ordinal {
		t.Error("split rows share an ordinal")
	}
	if title := lastConv(t, c).Title; title != "fix the flaky test" {
		t.Errorf("title %q comes from injected text", title)
	}
}

// P3: a cursor state the parser cannot read (corrupt, or an older state
// version) restarts the parse from the start instead of failing forever.
func TestUnreadableStateRestartsFromStart(t *testing.T) {
	data, err := os.ReadFile(betaMain)
	if err != nil {
		t.Fatal(err)
	}
	full, fullCur := parseBytes(t, &Parser{}, betaMain, data, transcript.Cursor{})
	half := transcript.Cursor{Offset: int64(bytes.IndexByte(data[len(data)/2:], '\n') + len(data)/2 + 1), LineNo: 1}
	for name, st := range map[string][]byte{
		"corrupt":     []byte("{not json"),
		"old version": []byte(`{"c":{"id":"x"}}`),
		"empty":       nil,
	} {
		stats := &Stats{}
		cur := half
		cur.State = st
		c, next := parseBytes(t, &Parser{Stats: stats}, betaMain, data, cur)
		if stats.StateResets.Load() != 1 {
			t.Errorf("%s: resets %d", name, stats.StateResets.Load())
		}
		if len(c.Messages) != len(full.Messages) || next.Offset != fullCur.Offset || !bytes.Equal(next.State, fullCur.State) {
			t.Errorf("%s: %d rows to %d, want the full parse's %d rows to %d", name, len(c.Messages), next.Offset, len(full.Messages), fullCur.Offset)
		}
	}
}

// D6: <task-notification> blocks are harness output, kind injected, alone
// or next to a system-reminder or a prompt.
func TestTaskNotificationIsInjected(t *testing.T) {
	lines := []string{
		`{"type":"user","uuid":"n1","sessionId":"s","message":{"role":"user","content":"<task-notification>\n<task-id>b1</task-id>\n<status>completed</status>\n</task-notification>"}}`,
		`{"type":"user","uuid":"n2","sessionId":"s","message":{"role":"user","content":"<task-notification>\n<summary>Stop hook feedback</summary>\n</task-notification>\n<system-reminder>\nhook said stop\n</system-reminder>"}}`,
		`{"type":"user","uuid":"n3","sessionId":"s","message":{"role":"user","content":[{"type":"text","text":"<task-notification>done</task-notification>\nnow ship it"}]}}`,
	}
	c, _ := parseBytes(t, &Parser{}, "", []byte(strings.Join(lines, "\n")+"\n"), transcript.Cursor{})
	want := []struct {
		kind transcript.Kind
		id   string
		text string
	}{
		{transcript.KindInjected, "n1#0", "<task-notification>\n<task-id>b1"},
		{transcript.KindInjected, "n2#0", "<task-notification>\n<summary>Stop hook feedback"},
		{transcript.KindUser, "n3#0", "now ship it"},
		{transcript.KindInjected, "n3#0:injected", "<task-notification>done</task-notification>"},
	}
	if len(c.Messages) != len(want) {
		t.Fatalf("got %d rows", len(c.Messages))
	}
	for i, w := range want {
		m := c.Messages[i]
		if m.Kind != w.kind || m.NativeID != w.id || !strings.HasPrefix(m.Text, w.text) {
			t.Errorf("row %d = %v %s %q, want %v %s %q", i, m.Kind, m.NativeID, m.Text, w.kind, w.id, w.text)
		}
	}
	if title := lastConv(t, c).Title; title != "now ship it" {
		t.Errorf("title %q", title)
	}
}

// A person who types a tag name is not the harness: the harness always
// writes a closed block at the start of a line. An unclosed or mid-line tag
// stays in the prompt, which is shown by default and gives the title.
func TestPromptMentioningInjectedTagStaysPrompt(t *testing.T) {
	for _, text := range []string{
		"why does the output contain <system-reminder> tags? remove them from the export",
		"strip <system-reminder>x</system-reminder> blocks before indexing",
		"<task-notification> never arrives when the job ends, find out why",
	} {
		b, _ := json.Marshal(text)
		line := `{"type":"user","uuid":"u1","sessionId":"s","message":{"role":"user","content":` + string(b) + `}}` + "\n"
		c, _ := parseBytes(t, &Parser{}, "", []byte(line), transcript.Cursor{})
		if len(c.Messages) != 1 || c.Messages[0].Kind != transcript.KindUser || c.Messages[0].Text != text {
			for _, m := range c.Messages {
				t.Logf("  %v %q", m.Kind, m.Text)
			}
			t.Errorf("%q: want one user row with the whole text", text)
		}
	}
}

// parent_native_id names the parent line; ParentRow resolves it to that
// line's last row, a real row id.
func TestParentResolvesToRow(t *testing.T) {
	data := strings.Join([]string{
		`{"type":"user","uuid":"u1","sessionId":"s","message":{"role":"user","content":"hello"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"s","message":{"role":"assistant","content":[{"type":"text","text":"run it"},{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}`,
		`{"type":"user","uuid":"r1","parentUuid":"a1","sessionId":"s","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`,
		`{"type":"assistant","uuid":"a2","parentUuid":"r1","sessionId":"s","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`,
	}, "\n") + "\n"
	c, _ := parseBytes(t, &Parser{}, "/p/s.jsonl", []byte(data), transcript.Cursor{})
	want := map[string]string{"u1#0": "", "a1#0": "u1#0", "a1#1": "u1#0", "r1#0": "a1#1", "a2#0": "r1#0"}
	if len(c.Messages) != len(want) {
		t.Fatalf("%d rows", len(c.Messages))
	}
	for _, m := range c.Messages {
		got := ""
		if p := ParentRow(m.ParentNativeID, c.Messages); p != nil {
			got = p.NativeID
		}
		if w, ok := want[m.NativeID]; !ok || got != w {
			t.Errorf("%s: parent %q (from %q), want %q", m.NativeID, got, m.ParentNativeID, w)
		}
	}
}

func TestPersistedTruncationAndRepair(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "0b7e2c1a-0000-4000-8000-0000000000aa.jsonl")
	preview := `<persisted-output>\nFull output saved to: /x/tool-results/gone.txt\n</persisted-output>`
	data := `{"type":"user","uuid":"t1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"` + preview + `"}]}}` + "\n"
	p := &Parser{MaxPersisted: 4, Caps: map[transcript.Kind]transcript.CapConfig{}}
	missing, _ := parseBytes(t, p, path, []byte(data), transcript.Cursor{})
	if missing.Messages[0].Enrichment["persisted_output_missing"] == nil {
		t.Fatal("missing not reported")
	}
	if err := os.MkdirAll(filepath.Join(dir, "0b7e2c1a-0000-4000-8000-0000000000aa", "tool-results"), 0700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "0b7e2c1a-0000-4000-8000-0000000000aa", "tool-results", "gone.txt")
	if err := os.WriteFile(output, []byte("abcdefgh"), 0600); err != nil {
		t.Fatal(err)
	}
	truncated, _ := parseBytes(t, p, path, []byte(data), transcript.Cursor{})
	m := truncated.Messages[0]
	if m.Text != "abcd" || m.Enrichment["persisted_output_truncated"] != true || m.Enrichment["persisted_output_missing"] != nil {
		t.Fatalf("truncated: %+v", m)
	}
	if err := os.WriteFile(output, []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	repaired, _ := parseBytes(t, p, path, []byte(data), transcript.Cursor{})
	if repaired.Messages[0].Enrichment["persisted_output_truncated"] != nil {
		t.Fatal("repair kept truncation flag")
	}
}

// Claude records the git branch on every line: the conversation keeps
// each branch the session ran on, first seen first, across incremental
// parses, and still names the first as git_branch.
func TestBranchesFollowTheSession(t *testing.T) {
	line := func(id, branch string) string {
		return `{"type":"user","uuid":"` + id + `","sessionId":"s","cwd":"/r","gitBranch":"` + branch + `","timestamp":"2026-09-01T00:00:00Z","message":{"role":"user","content":"hi ` + id + `"}}` + "\n"
	}
	first := line("u1", "main") + line("u2", "") + line("u3", "main")
	c, cur := parseBytes(t, &Parser{}, "", []byte(first), transcript.Cursor{})
	if got := lastConv(t, c); fmt.Sprint(got.Branches) != "[main]" || got.Extra["git_branch"] != "main" {
		t.Fatalf("branches %v extra %v", got.Branches, got.Extra)
	}
	all := first + line("u4", "feat/x") + line("u5", "main") + line("u6", "feat/y")
	c, _ = parseBytes(t, &Parser{}, "", []byte(all), cur)
	if got := lastConv(t, c); fmt.Sprint(got.Branches) != "[main feat/x feat/y]" || got.Extra["git_branch"] != "main" {
		t.Fatalf("branches after a switch %v extra %v", got.Branches, got.Extra)
	}
	// Back on an earlier branch: the list ends with the current one.
	c, _ = parseBytes(t, &Parser{}, "", []byte(all+line("u7", "feat/x")), cur)
	if got := lastConv(t, c); fmt.Sprint(got.Branches) != "[main feat/x feat/y feat/x]" {
		t.Fatalf("branches after a switch back %v", got.Branches)
	}
}
