package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

const fixtureHome = "../../../testdata/oracle/home/.codex"

const (
	sidA = "019a0000-0000-7000-8000-0000000000a1" // March 2026: exec_command / apply_patch function calls, no ids
	sidB = "019a0000-0000-7000-8000-0000000000a2" // August 2026: exec custom tool, ids, mirrors
	sidC = "019a0000-0000-7000-8000-0000000000a3" // September 2026: exec + codex-events@1, compacted, turn_aborted
	sidD = "019a0000-0000-7000-8000-0000000000a4" // thread_spawn child forked with history
	sidE = "019a0000-0000-7000-8000-0000000000a5" // codex exec review child, archived
	sidF = "019a0000-0000-7000-8000-0000000000a6" // fork with history_base
	sidG = "019a0000-0000-7000-8000-0000000000a7" // legacy August 2025, no envelope
)

func fixturePath(t *testing.T, sid string) string {
	t.Helper()
	srcs, err := Discover(fixtureHome)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range srcs {
		if s.SessionKey == sid {
			return s.Path
		}
	}
	t.Fatalf("fixture %s not discovered", sid)
	return ""
}

// parseFile parses a whole file and returns the collected output.
func parseFile(t *testing.T, path string, p *Parser) *transcript.Collector {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if p == nil {
		p = &Parser{}
	}
	var c transcript.Collector
	src := &transcript.Source{Agent: transcript.AgentCodex, Path: path}
	cur, err := p.Parse(context.Background(), transcript.Input{Source: src, R: bytes.NewReader(data), Size: int64(len(data))}, transcript.Cursor{}, &c)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Offset != int64(len(data)) {
		t.Fatalf("cursor offset %d, file %d", cur.Offset, len(data))
	}
	return &c
}

// upsert collapses re-emitted rows the way the index does: by native id,
// else by locator; the latest value wins at the first position.
func upsert(msgs []*transcript.Message) []*transcript.Message {
	type key struct {
		id   string
		line int64
		part int
	}
	idx := map[key]int{}
	var out []*transcript.Message
	for _, m := range msgs {
		k := key{id: m.NativeID}
		if k.id == "" {
			k.line, k.part = m.LineNo, m.Part
		}
		if i, ok := idx[k]; ok {
			out[i] = m
			continue
		}
		idx[k] = len(out)
		out = append(out, m)
	}
	return out
}

func lastConv(c *transcript.Collector) *transcript.Conversation {
	return c.Conversations[len(c.Conversations)-1]
}

type rowView struct {
	Kind     string
	ID       string
	Tool     string
	Text     string
	IsError  bool
	Enriched bool
}

func view(msgs []*transcript.Message) []rowView {
	var out []rowView
	for _, m := range msgs {
		out = append(out, rowView{m.Kind.String(), m.NativeID, m.ToolName, m.Text, m.IsError, m.Enrichment["enrichment"] != nil})
	}
	return out
}

// sameRows compares views; a wanted Text is a prefix of the row text.
func sameRows(got, want []rowView) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		g, w := got[i], want[i]
		if !strings.HasPrefix(g.Text, w.Text) || w.Text == "" && g.Text != "" {
			return false
		}
		g.Text = w.Text
		if g != w {
			return false
		}
	}
	return true
}

func TestMarchShape(t *testing.T) {
	c := parseFile(t, fixturePath(t, sidA), nil)
	conv := lastConv(c)
	if conv.SessionID != sidA || conv.Cwd != "/tmp/oracle-beta" || conv.Title != "add a health check endpoint" {
		t.Fatalf("conversation %+v", conv)
	}
	if g, ok := conv.Extra["git"].(Git); !ok || g.Branch != "main" || len(conv.Branches) != 1 || conv.Branches[0] != "main" {
		t.Fatalf("git %v", conv.Extra["git"])
	}
	msgs := upsert(c.Messages)
	want := []rowView{
		{Kind: "injected", Text: "# AGENTS.md instructions for /tmp/oracle"},
		{Kind: "user", Text: "add a health check endpoint"},
		{Kind: "thinking", Text: "Look at the router first."},
		{Kind: "tool_call", Tool: "exec_command", Text: `{"cmd": "rg -n NewRouter", "workdir": "/`},
		{Kind: "tool_result", Tool: "exec_command", Text: "internal/api/server.go:31:\tr := chi.New"},
		{Kind: "tool_call", Tool: "apply_patch", Text: `{"input": "*** Begin Patch\n*** Update`},
		{Kind: "tool_result", Tool: "apply_patch", Text: "Success. Updated the following files:\nM "},
		{Kind: "tool_call", Tool: "exec_command", Text: `{"cmd": "go test ./internal/api/"}`},
		{Kind: "tool_result", Tool: "exec_command", Text: "--- FAIL: TestHealthz (0.00s)\n    health"},
		{Kind: "assistant", Text: "Added /healthz; the test still fails wit"},
		{Kind: "user", Text: "why 404?"},
		{Kind: "assistant", Text: "Middleware order: auth rejects the unauth"},
	}
	if got := view(msgs); !sameRows(got, want) {
		t.Fatalf("rows\n got %+v\nwant %+v", got, want)
	}
	for _, m := range msgs {
		if m.NativeID != "" {
			t.Errorf("March rollouts carry no payload ids; got %q", m.NativeID)
		}
		if m.LineNo == 0 || m.ByteLen == 0 || m.Parser != RowParser {
			t.Errorf("locator/parser missing: %+v", m)
		}
	}
	if got := msgs[5].Enrichment["changed_paths"]; !reflect.DeepEqual(got, []string{"internal/api/server.go", "internal/api/health.go"}) {
		t.Errorf("apply_patch paths %v", got)
	}
	if msgs[4].ToolCallID != "call_a1_1" || msgs[3].ToolCallID != "call_a1_1" {
		t.Errorf("call ids %q %q", msgs[3].ToolCallID, msgs[4].ToolCallID)
	}
}

func TestAugustShape(t *testing.T) {
	stats := &Stats{}
	c := parseFile(t, fixturePath(t, sidB), &Parser{Stats: stats})
	msgs := upsert(c.Messages)
	want := []rowView{
		{Kind: "system", ID: "msg_b_dev", Text: "<permissions instructions>sandbox: works"},
		{Kind: "user", ID: "msg_b_u1", Text: "summarize the failing CI job"},
		{Kind: "assistant", ID: "msg_b_a1", Text: "Checking the CI logs."},
		{Kind: "tool_call", ID: "ctc_b_1", Tool: "exec", Text: `text(await tools.exec_command({cmd:"gh r`},
		{Kind: "tool_result", ID: "ctco_b_1", Tool: "exec", Text: "Script completed\nWall time 1.2 seconds\n"},
		{Kind: "assistant", ID: "msg_b_a2", Text: "CI fails on lint: an unused variable in "},
	}
	if got := view(msgs); !sameRows(got, want) {
		t.Fatalf("rows\n got %+v\nwant %+v", got, want)
	}
	for i, m := range msgs {
		if o, ok := m.Enrichment["codex_ordinal"].(int64); !ok || o <= 0 {
			t.Errorf("row %d: codex_ordinal %v", i, m.Enrichment["codex_ordinal"])
		}
	}
	if len(c.Messages) != len(msgs) {
		t.Errorf("no enrichment events in August rollouts, yet %d re-emissions", len(c.Messages)-len(msgs))
	}
	if stats.EventsPaired.Load()+stats.EventsLoose.Load() != 0 {
		t.Error("mirrors must not reach codex-events@1")
	}
}

func TestSeptemberShape(t *testing.T) {
	stats := &Stats{}
	c := parseFile(t, fixturePath(t, sidC), &Parser{Stats: stats})
	msgs := upsert(c.Messages)
	byID := map[string]*transcript.Message{}
	for _, m := range msgs {
		if m.NativeID != "" {
			byID[m.NativeID] = m
		}
	}

	exec1 := byID["ctc_c_1"]
	cmds, _ := exec1.Enrichment["commands"].([]Command)
	if len(cmds) != 2 || cmds[0].Cmd != "go test ./..." || *cmds[0].ExitCode != 1 || *cmds[0].DurationMS != 2500 ||
		cmds[0].Cwd != "/tmp/oracle-delta" || cmds[1].Cmd != "go vet ./..." || *cmds[1].ExitCode != 0 {
		t.Fatalf("exec commands %+v", cmds)
	}
	if !exec1.IsError || exec1.Enrichment["enrichment"] != EventsVersion {
		t.Errorf("exec with a failing command: is_error=%v enrichment=%v", exec1.IsError, exec1.Enrichment["enrichment"])
	}
	if strings.Contains(exec1.Text, "FAIL") {
		t.Error("event output text must not be indexed on the call row")
	}
	patch := byID["ctc_c_2"]
	if got := patch.Enrichment["changed_paths"]; !reflect.DeepEqual(got, []string{"parse.go", "/tmp/oracle-delta/parse.go"}) {
		t.Errorf("patch paths %v", got)
	}
	late := byID["ctc_c_3"]
	if cmds, _ := late.Enrichment["commands"].([]Command); len(cmds) != 1 || cmds[0].Cmd != "go build -o bin/app ./cmd/app" {
		t.Errorf("background command not attached late: %v", late.Enrichment)
	}
	loose := byID["exec-c-5"]
	if loose == nil || loose.Kind != transcript.KindToolResult || loose.ToolName != "shell" || loose.Text != "git status --short" || loose.Parser != EventsVersion {
		t.Fatalf("loose event row %+v", loose)
	}
	aborted := byID["ctc_c_4"]
	if cmds, _ := aborted.Enrichment["commands"].([]Command); len(cmds) != 1 || *cmds[0].ExitCode != 130 || !aborted.IsError {
		t.Errorf("turn_aborted must flush the open call's enrichment: %+v", aborted.Enrichment)
	}
	if ws := byID["ws_c_1"]; ws == nil || ws.ToolName != "web_search" || ws.Text != "go test count flag" {
		t.Errorf("web search row %+v", ws)
	}
	if am := byID["amsg_c_1"]; am == nil || am.Kind != transcript.KindAgentMessage || am.Role != "agent" || strings.Contains(am.Text, "gAAAA") || am.Enrichment["author"] != "/root/checker" {
		t.Errorf("agent message row %+v", am)
	}

	// Compaction: one summary for win-c-1 (the repeat and win-c-2 add no
	// text), one row for the unseen prompt, nothing for seen items.
	var summaries, replayed []*transcript.Message
	for _, m := range msgs {
		if m.Role == "compaction" {
			summaries = append(summaries, m)
		}
		if m.Enrichment["from_compaction"] == true {
			replayed = append(replayed, m)
		}
	}
	if len(summaries) != 1 || summaries[0].Kind != transcript.KindSystem || summaries[0].Text != "Tests run with -count=1" {
		t.Errorf("compaction summaries %+v", view(summaries))
	}
	if len(replayed) != 1 || replayed[0].NativeID != "msg_c_u0" {
		t.Errorf("replayed %+v", view(replayed))
	}
	if n := stats.CompactionReplaySkipped.Load(); n != 5 {
		t.Errorf("replay skipped %d, want 5", n)
	}
	if p, l, late := stats.EventsPaired.Load(), stats.EventsLoose.Load(), stats.EventsLate.Load(); p != 4 || l != 1 || late != 1 {
		t.Errorf("paired=%d loose=%d late=%d", p, l, late)
	}
	// Mirrors and bookkeeping records produce no rows.
	for _, m := range msgs {
		if m.NativeID == "u-mirror" || m.NativeID == "cc-1" || m.NativeID == "cmp_c_1" {
			t.Errorf("mirror indexed: %+v", m)
		}
	}
	if conv := lastConv(c); conv.Title != "run the tests and fix what breaks" || conv.Extra["link"] != nil {
		t.Errorf("conversation %+v", conv)
	}
}

func TestThreadSpawnChildSkipsInheritedHistory(t *testing.T) {
	stats := &Stats{}
	c := parseFile(t, fixturePath(t, sidD), &Parser{Stats: stats})
	conv := lastConv(c)
	if conv.SessionID != sidD || conv.ParentSessionID != sidC || conv.Depth != 1 || conv.Extra["link"] != LinkThreadSpawn ||
		conv.Extra["agent_path"] != "/root/checker" || conv.Extra["agent_nickname"] != "Noether" || conv.Extra["forked_from_id"] != sidC ||
		conv.Extra["root_session_id"] != sidC {
		t.Fatalf("child conversation %+v", conv)
	}
	if conv.Title != "Check that parse.go handles empty input." {
		t.Errorf("title %q", conv.Title)
	}
	ids := []string{}
	for _, m := range c.Messages {
		ids = append(ids, m.NativeID)
	}
	if want := []string{"msg_d_dev", "amsg_d_1", "msg_d_a1"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids %v, want %v (inherited parent rows must be skipped)", ids, want)
	}
	if n := stats.InheritedSkipped.Load(); n != 2 {
		t.Errorf("inherited skipped %d", n)
	}
	for _, conv := range c.Conversations {
		if conv.SessionID != sidD {
			t.Errorf("copied parent session_meta emitted a conversation: %s", conv.SessionID)
		}
	}
}

func TestReviewChildAndFork(t *testing.T) {
	c := parseFile(t, fixturePath(t, sidE), nil)
	conv := lastConv(c)
	if conv.ParentSessionID != sidB || conv.Extra["link"] != LinkReview || conv.Depth != 1 {
		t.Fatalf("review child %+v", conv)
	}
	if got := view(c.Messages); len(got) != 2 || got[0].ID != "msg_e_u1" {
		t.Fatalf("review rows %+v", got)
	}

	c = parseFile(t, fixturePath(t, sidF), nil)
	conv = lastConv(c)
	if conv.ParentSessionID != "" || conv.Extra["link"] != LinkFork || conv.Extra["forked_from_id"] != sidB {
		t.Fatalf("fork %+v", conv)
	}
	if hb, ok := conv.Extra["history_base"].(HistoryBase); !ok || hb.ThreadID != sidB {
		t.Errorf("history_base %v", conv.Extra["history_base"])
	}
	if len(c.Messages) != 2 {
		t.Errorf("fork rows %d", len(c.Messages))
	}
}

func TestLegacyRollout(t *testing.T) {
	c := parseFile(t, fixturePath(t, sidG), nil)
	conv := lastConv(c)
	if conv.SessionID != sidG || conv.Extra["format"] != "legacy" || conv.Title != "list the repo" || conv.StartedAt.IsZero() {
		t.Fatalf("legacy conversation %+v", conv)
	}
	want := []rowView{
		{Kind: "user", Text: "list the repo"},
		{Kind: "thinking", ID: "rs_legacy_1", Text: "**Listing files**"},
		{Kind: "tool_call", ID: "fc_legacy_1", Tool: "shell", Text: `{"command": ["bash", "-lc", "ls"]}`},
		{Kind: "tool_result", Tool: "shell", Text: "README.md\ngo.mod\n"},
		{Kind: "tool_call", ID: "fc_legacy_2", Tool: "shell", Text: `{"command": ["bash", "-lc", "cat missing`},
		{Kind: "tool_result", Tool: "shell", Text: "cat: missing.txt: No such file or direct", IsError: true},
		{Kind: "assistant", Text: "The repo has README.md and go.mod."},
	}
	if got := view(c.Messages); !sameRows(got, want) {
		t.Fatalf("rows\n got %+v\nwant %+v", got, want)
	}
}

// TestResumeMatchesFullParse cuts every fixture at every line boundary and
// in the middle of every line, and at pairs of cut points, and checks that
// the resumed parses emit exactly what one full parse emits.
func TestResumeMatchesFullParse(t *testing.T) {
	srcs, err := Discover(fixtureHome)
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range srcs {
		t.Run(filepath.Base(src.Path), func(t *testing.T) {
			data, err := os.ReadFile(src.Path)
			if err != nil {
				t.Fatal(err)
			}
			var cuts []int64
			start := 0
			for i, b := range data {
				if b == '\n' {
					cuts = append(cuts, int64(start+(i-start)/2), int64(i+1))
					start = i + 1
				}
			}
			for _, c := range cuts {
				assertResumeMatches(t, &src, data, []int64{c})
			}
			for i := 0; i+3 < len(cuts); i += 3 {
				assertResumeMatches(t, &src, data, []int64{cuts[i], cuts[i+1], cuts[i+3]})
			}
		})
	}
}

// TestOversizedLines drives the streaming path for lines above the reader's
// in-memory limit: the rows match a parse that materializes every line.
func TestOversizedLines(t *testing.T) {
	path := fixturePath(t, sidC)
	full := parseFile(t, path, nil)
	small := parseFile(t, path, &Parser{LineOptions: transcript.LineReaderOptions{MaxLine: 256, HeadSize: 128}})
	if !reflect.DeepEqual(full.Messages, small.Messages) || !reflect.DeepEqual(full.Conversations, small.Conversations) {
		t.Fatal("oversized-line parse differs from in-memory parse")
	}
}

func TestPartialFinalLineIsNotConsumed(t *testing.T) {
	data, err := os.ReadFile(fixturePath(t, sidB))
	if err != nil {
		t.Fatal(err)
	}
	cut := len(data) - 10
	var c transcript.Collector
	cur, err := (&Parser{}).Parse(context.Background(), transcript.Input{R: bytes.NewReader(data[:cut]), Size: int64(cut)}, transcript.Cursor{}, &c)
	if err != nil {
		t.Fatal(err)
	}
	if last := bytes.LastIndexByte(data[:cut], '\n') + 1; cur.Offset != int64(last) {
		t.Fatalf("cursor %d, want %d", cur.Offset, last)
	}
}

func TestEnrichmentFailureNeverFailsTheParse(t *testing.T) {
	lines := []string{
		`{"timestamp":"2026-09-15T10:00:00.000Z","ordinal":0,"type":"session_meta","payload":{"id":"s-bad","cwd":"/x","source":"cli"}}`,
		`{"timestamp":"2026-09-15T10:00:01.000Z","ordinal":1,"type":"response_item","payload":{"type":"custom_tool_call","id":"ctc_1","call_id":"c1","name":"exec","input":"x","internal_chat_message_metadata_passthrough":{"turn_id":"t"}}}`,
		`{"timestamp":"2026-09-15T10:00:02.000Z","ordinal":2,"type":"event_msg","payload":{"type":"item_completed","turn_id":"t","item":{"type":"CommandExecution","id":"e1","command":42,"exit_code":"x","duration":[1]}}}`,
		`{"timestamp":"2026-09-15T10:00:03.000Z","ordinal":3,"type":"event_msg","payload":{"type":"item_completed","turn_id":"t","item":{"type":"FileChange","id":"e2","changes":"not a map"}}}`,
		`{"timestamp":"2026-09-15T10:00:04.000Z","ordinal":4,"type":"event_msg","payload":{"type":"item_completed","turn_id":"t","item":{"type":"CommandExecution","id":"e3","command":["ls"],"duration":"soon"}}}`,
		`{"timestamp":"2026-09-15T10:00:05.000Z","ordinal":5,"type":"response_item","payload":{"type":"custom_tool_call_output","id":"ctco_1","call_id":"c1","output":"ok"}}`,
		`{"timestamp":"2026-09-15T10:00:06.000Z","ordinal":6,"type":"response_item","payload":{"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"done"}]}}`,
		`{"timestamp":"2026-09-15T10:00:07.000Z","ordinal":7,"type":"response_item","payload":{"type":"message","id":"m2","role":"assistant","summary":"a string","content":[{"type":"output_text","text":"kept despite a mistyped field"}]}}`,
		`not json at all`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":7}}`,
	}
	data := []byte(strings.Join(lines, "\n") + "\n")
	stats := &Stats{}
	var c transcript.Collector
	if _, err := (&Parser{Stats: stats}).Parse(context.Background(), transcript.Input{R: bytes.NewReader(data), Size: int64(len(data))}, transcript.Cursor{}, &c); err != nil {
		t.Fatal(err)
	}
	if stats.EventErrors.Load() != 2 {
		t.Errorf("event errors %d, want 2", stats.EventErrors.Load())
	}
	ids := []string{}
	for _, m := range upsert(c.Messages) {
		ids = append(ids, m.NativeID)
	}
	if stats.TypeErrors.Load() != 1 {
		t.Errorf("type errors %d, want 1", stats.TypeErrors.Load())
	}
	if !reflect.DeepEqual(ids, []string{"ctc_1", "ctco_1", "m1", "m2"}) {
		t.Fatalf("rows %v", ids)
	}
}

func TestPromptPairing(t *testing.T) {
	// Cases from FAD user_prompts.rs tests: neighbouring opposite-stream
	// records pair once; same-stream repeats and distant copies stay.
	run := func(streams []stream) []int {
		var last *promptMark
		ts := int64(1000)
		var kept []int
		for i, s := range streams {
			if !observePrompt(&last, s, int64(i+1), "continue", &ts, "") {
				kept = append(kept, i)
			}
		}
		return kept
	}
	R, E := streamResponse, streamEvent
	cases := []struct {
		in   []stream
		want []int
	}{
		{[]stream{E, R}, []int{0}},
		{[]stream{R, E}, []int{0}},
		{[]stream{E, E, R}, []int{0, 1}},
		{[]stream{E, R, E}, []int{0, 2}},
		{[]stream{R, E, E}, []int{0, 2}},
		{[]stream{E, E}, []int{0, 1}},
		{[]stream{R, R}, []int{0, 1}},
		{[]stream{E, R, E, R}, []int{0, 2}},
	}
	for _, c := range cases {
		if got := run(c.in); !slices.Equal(got, c.want) {
			t.Errorf("%v: kept %v, want %v", c.in, got, c.want)
		}
	}
	var last *promptMark
	a, b := int64(0), int64(1001)
	observePrompt(&last, E, 1, "x", &a, "")
	if observePrompt(&last, R, 2, "x", &b, "") {
		t.Error("timestamps over 1s apart must not pair")
	}
	last = nil
	observePrompt(&last, E, 1, "x", nil, "t1")
	if observePrompt(&last, R, 2, "x", nil, "t2") {
		t.Error("different turns must not pair")
	}
	last = nil
	observePrompt(&last, E, 1, "  日本語\n", &a, "")
	if !observePrompt(&last, R, 2, "日本語", &a, "") {
		t.Error("trimmed text must pair")
	}
	last = nil
	observePrompt(&last, E, 1, "x", &a, "")
	if observePrompt(&last, R, 3, "x", &a, "") {
		t.Error("non-adjacent lines must not pair")
	}
}

func TestMetaLinks(t *testing.T) {
	meta := func(js string) meta {
		var p sessionMeta
		if err := json.Unmarshal([]byte(js), &p); err != nil {
			t.Fatal(err)
		}
		return metaFrom(&p, "")
	}
	cases := []struct {
		js     string
		link   string
		parent string
		depth  int
	}{
		{`{"id":"a","source":"cli"}`, "", "", 0},
		{`{"id":"a","source":"exec","originator":"codex_exec"}`, LinkExec, "", 0},
		{`{"id":"a","source":"vscode"}`, "", "", 0},
		{`{"id":"a","source":{"subagent":"review"}}`, LinkReview, "", 0},
		{`{"id":"a","parent_thread_id":"p","source":{"subagent":"review"}}`, LinkReview, "p", 1},
		{`{"id":"a","source":{"subagent":{"thread_spawn":{"parent_thread_id":"p","depth":2,"agent_path":null}}}}`, LinkThreadSpawn, "p", 2},
		{`{"id":"a","parent_thread_id":"q","source":{"subagent":{"thread_spawn":{"depth":1}}}}`, LinkThreadSpawn, "q", 1},
		{`{"id":"a","forked_from_id":"f","source":"cli"}`, LinkFork, "", 0},
		{`{"id":"a","forked_from_id":"p","source":{"subagent":{"thread_spawn":{"parent_thread_id":"p"}}}}`, LinkThreadSpawn, "p", 1},
		{`{"id":"a","parent_thread_id":"p","thread_source":"subagent","source":"cli"}`, "subagent", "p", 1},
	}
	for _, c := range cases {
		m := meta(c.js)
		if m.Link != c.link || m.Parent != c.parent || m.Depth != c.depth {
			t.Errorf("%s: link=%q parent=%q depth=%d", c.js, m.Link, m.Parent, m.Depth)
		}
	}
}

func TestDiscoverAndFollowMoves(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	if Home() != home {
		t.Fatalf("Home() = %s", Home())
	}
	src := fixturePath(t, sidC)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(home, "sessions", "2026", "09", "15", filepath.Base(src))
	if err := os.MkdirAll(filepath.Dir(live), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(home, "sessions", "notes.txt"), []byte("x"), 0o644)
	before, err := Discover(home)
	if err != nil || len(before) != 1 {
		t.Fatalf("discover %v %v", before, err)
	}
	full := parseFile(t, live, nil)

	archived := filepath.Join(home, "archived_sessions", filepath.Base(src))
	if err := os.MkdirAll(filepath.Dir(archived), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(live, archived); err != nil {
		t.Fatal(err)
	}
	after, err := Discover(home)
	if err != nil || len(after) != 1 || after[0].Path != archived {
		t.Fatalf("discover after move %v %v", after, err)
	}
	if after[0].SessionKey != before[0].SessionKey || after[0].SessionKey != sidC {
		t.Errorf("session key changed across the move: %s -> %s", before[0].SessionKey, after[0].SessionKey)
	}
	if after[0].FileID != before[0].FileID {
		t.Errorf("rename changed file identity")
	}
	moved := parseFile(t, archived, nil)
	if !reflect.DeepEqual(full.Messages, moved.Messages) || lastConv(moved).SessionID != sidC {
		t.Error("the moved rollout parses to different rows")
	}
}
