package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

// Unit tests of `flopwire probe`: its parsing of harness output and its
// verdicts, against fake harness output. The live run is not a test.

const (
	pSess   = "e8917dc1-31ba-4e2b-8c2e-c87679befcf5"
	pSender = "16370068-405f-4a0b-922b-a6b42749bfc2"
	pID     = "mfd241bef6fb04e64"
	pMarker = "PROBE-MIDTURN-73f054"
)

// at builds a tap entry at ms (relative), printed or not.
func at(ms int64, event string, mod ...func(*tapEntry)) tapEntry {
	e := tapEntry{At: ms, Done: ms + 20, Event: event, Session: pSess}
	for _, m := range mod {
		m(&e)
	}
	return e
}

func printed(ids ...string) func(*tapEntry) { return func(e *tapEntry) { e.Printed = ids } }
func tool(t string) func(*tapEntry)         { return func(e *tapEntry) { e.Tool = t } }
func agentID(a string) func(*tapEntry)      { return func(e *tapEntry) { e.AgentID = a } }

func TestPrintedIDs(t *testing.T) {
	ctx := "<flopwire-instructions>\nFlopwire messaging…\n</flopwire-instructions>\n\n" +
		`<flopwire-message id="m1" from="a" intent="inform">x</flopwire-message>` + "\n" +
		`<flopwire-message id="m2" from="a" intent="request">y</flopwire-message>`
	out, _ := json.Marshal(hookOutput{HookSpecificOutput: hookSpecific{HookEventName: evPostToolUse, AdditionalContext: ctx}})
	ids, ins := printedIDs(out)
	if !slices.Equal(ids, []string{"m1", "m2"}) || !ins {
		t.Fatalf("got %v %v", ids, ins)
	}
	if ids, ins := printedIDs(nil); ids != nil || ins {
		t.Fatalf("empty output: %v %v", ids, ins)
	}
	// A message body that quotes a wrapper is escaped by busrender, so
	// only real wrappers start with `<flopwire-message id="`.
	out, _ = json.Marshal(hookOutput{SystemMessage: "Flopwire: 2 messages held"})
	if ids, _ := printedIDs(out); ids != nil {
		t.Fatalf("systemMessage only: %v", ids)
	}
}

func TestTapLogRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tap.jsonl")
	want := []tapEntry{at(1, evSessionStart, func(e *tapEntry) { e.Instruction = true }), at(2, evPostToolUse, tool("Bash"), printed(pID))}
	for _, e := range want {
		if err := appendTap(path, e); err != nil {
			t.Fatal(err)
		}
	}
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	f.WriteString("{\"at\":3,\"ev") // a torn line
	f.Close()
	got, err := readTap(path)
	if err != nil || len(got) != 2 || got[1].Printed[0] != pID || !got[0].Instruction {
		t.Fatalf("got %+v %v", got, err)
	}
	if got, err := readTap(filepath.Join(t.TempDir(), "none")); got != nil || err != nil {
		t.Fatalf("missing log: %v %v", got, err)
	}
}

// The tap runs the real hook for the plugin events only, and logs every
// event with the ids its input carries.
func TestProbeTapObservesWithoutHooking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tap.jsonl")
	in := `{"hook_event_name":"PreToolUse","session_id":"` + pSess + `","tool_name":"Agent","tool_use_id":"t1","agent_id":"a1","agent_type":"general-purpose"}`
	var out, errOut strings.Builder
	sock := filepath.Join(t.TempDir(), "none.sock")
	if err := probeTap(t.Context(), []string{"--log", path, "--socket", sock}, strings.NewReader(in), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || errOut.Len() != 0 {
		t.Fatalf("PreToolUse ran the hook: out %q err %q", out.String(), errOut.String())
	}
	es, _ := readTap(path)
	if len(es) != 1 || es[0].Event != "PreToolUse" || es[0].Tool != "Agent" || es[0].AgentID != "a1" || es[0].AgentType != "general-purpose" || es[0].ToolUseID != "t1" {
		t.Fatalf("entry: %+v", es)
	}
	// A hooked event runs the real hook: with no agent it prints nothing
	// and says why on stderr.
	in = `{"hook_event_name":"PostToolUse","session_id":"` + pSess + `","transcript_path":"/tmp/x/` + pSess + `.jsonl"}`
	errOut.Reset()
	if err := probeTap(t.Context(), []string{"--log", path, "--socket", sock}, strings.NewReader(in), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut.String(), "device agent is not running") {
		t.Fatalf("PostToolUse did not run the hook: %q", errOut.String())
	}
}

func TestParseClaudeLine(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"init","session_id":"` + pSess + `","model":"claude-haiku"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"READY"}]},"session_id":"` + pSess + `"}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"READY","session_id":"` + pSess + `","num_turns":1}`,
		`not json`,
	}
	var got []claudeEvent
	for _, l := range lines {
		if e, ok := parseClaudeLine([]byte(l)); ok {
			got = append(got, e)
		}
	}
	if len(got) != 3 || got[0].SessionID != pSess || got[2].Type != "result" || got[2].Result != "READY" || got[2].IsError {
		t.Fatalf("got %+v", got)
	}
}

// Codex app-server notifications as 0.160.0 sends them: the session keeps
// the turn's agent messages, ignores a subagent thread's, and records the
// auto-review pass.
func TestCodexNotes(t *testing.T) {
	const thread, child = "01a10264-28f9-7691-b3c5-8fee906ebfb2", "01a10275-d2d0-7000-8000-000000000000"
	s := &codexSession{id: thread, done: make(chan string, 1)}
	notes := []string{
		`{"method":"turn/started","params":{"threadId":"` + thread + `","turn":{"id":"t1","status":"inProgress"}}}`,
		`{"method":"item/completed","params":{"threadId":"` + thread + `","item":{"type":"agentMessage","text":"I'll run it.","phase":"commentary"}}}`,
		`{"method":"item/autoApprovalReview/started","params":{"threadId":"` + thread + `","startedAtMs":1000,"review":{"status":"inProgress"}}}`,
		`{"method":"item/autoApprovalReview/completed","params":{"threadId":"` + thread + `","startedAtMs":1000,"completedAtMs":4765,"review":{"status":"approved"}}}`,
		`{"method":"item/completed","params":{"threadId":"` + child + `","item":{"type":"agentMessage","text":"child's answer","phase":"final_answer"}}}`,
		`{"method":"turn/completed","params":{"threadId":"` + child + `","turn":{"status":"completed"}}}`,
		`{"method":"item/completed","params":{"threadId":"` + thread + `","item":{"type":"agentMessage","text":"ID m1 MARKER PROBE-X","phase":"final_answer"}}}`,
		`{"method":"turn/completed","params":{"threadId":"` + thread + `","turn":{"status":"completed"}}}`,
	}
	done := s.done
	for _, n := range notes {
		var m rpcMsg
		if err := json.Unmarshal([]byte(n), &m); err != nil {
			t.Fatal(err)
		}
		s.note(m)
	}
	if st := <-done; st != "completed" {
		t.Fatalf("turn status %q", st)
	}
	if !slices.Equal(s.replies, []string{"I'll run it.", "ID m1 MARKER PROBE-X"}) {
		t.Fatalf("replies %q", s.replies)
	}
	if r := s.Reviews(); len(r) != 1 || r[0] != (codexReview{Start: 1000, End: 4765, Status: "approved"}) {
		t.Fatalf("reviews %+v", r)
	}
	if s.TurnAt() == 0 {
		t.Fatal("no turn activity recorded")
	}
}

// Devin ACP updates: the session collects its own message chunks, and the
// probe allows a tool call once.
func TestDevinNotes(t *testing.T) {
	s := &devinSession{id: "surf-feels"}
	for _, n := range []string{
		`{"method":"session/update","params":{"sessionId":"surf-feels","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"thinking"}}}}`,
		`{"method":"session/update","params":{"sessionId":"surf-feels","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"ID m1 "}}}}`,
		`{"method":"session/update","params":{"sessionId":"other","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"not ours"}}}}`,
		`{"method":"session/update","params":{"sessionId":"surf-feels","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"MARKER PROBE-X"}}}}`,
	} {
		var m rpcMsg
		json.Unmarshal([]byte(n), &m)
		s.note(m)
	}
	if got := s.text.String(); got != "ID m1 MARKER PROBE-X" {
		t.Fatalf("text %q", got)
	}
	var req rpcMsg
	json.Unmarshal([]byte(`{"id":7,"method":"session/request_permission","params":{"options":[{"optionId":"reject","kind":"reject_once"},{"optionId":"allow","kind":"allow_once"}]}}`), &req)
	b, _ := json.Marshal(devinPermission(req))
	if string(b) != `{"outcome":{"optionId":"allow","outcome":"selected"}}` {
		t.Fatalf("permission answer %s", b)
	}
}

func TestVerdictPromptSubmit(t *testing.T) {
	reply := "ID " + pID + " MARKER " + pMarker
	ok := []tapEntry{at(0, evUserPromptSubmit, printed("mOther", pID)), at(50, "Stop")}
	if v := verdictPromptSubmit(ok, pSess, pID, pMarker, reply); len(v.fails) != 0 {
		t.Fatalf("pass case failed: %v", v.fails)
	}
	for name, tc := range map[string]struct {
		es    []tapEntry
		reply string
		want  string
	}{
		"late hook":   {[]tapEntry{at(0, evUserPromptSubmit), at(10, evPostToolUse, printed(pID))}, reply, "not the session's UserPromptSubmit"},
		"not printed": {[]tapEntry{at(0, evUserPromptSubmit)}, reply, "no hook printed"},
		"twice":       {[]tapEntry{at(0, evUserPromptSubmit, printed(pID)), at(10, evPostToolUse, printed(pID))}, reply, "printed 2 times"},
		"not quoted":  {ok, "NONE", "did not quote"},
	} {
		v := verdictPromptSubmit(tc.es, pSess, pID, pMarker, tc.reply)
		if !strings.Contains(strings.Join(v.fails, ";"), tc.want) {
			t.Errorf("%s: fails %v, want %q", name, v.fails, tc.want)
		}
	}
}

func TestVerdictMidTurn(t *testing.T) {
	reply := "ID " + pID + " MARKER " + pMarker + " after sleep 8"
	ok := []tapEntry{at(100, "PreToolUse", tool("Bash")), at(8100, evPostToolUse, tool("Bash"), printed(pID)), at(9000, "PreToolUse", tool("Bash")),
		at(9100, evPostToolUse, tool("Bash")), at(15000, "Stop")}
	v := verdictMidTurn(ok, pSess, pID, pMarker, reply, 150)
	if len(v.fails) != 0 || !strings.Contains(strings.Join(v.facts, ";"), "PostToolUse Bash printed") {
		t.Fatalf("pass case: %+v", v)
	}
	for name, tc := range map[string]struct {
		es   []tapEntry
		want string
	}{
		"after the turn":  {[]tapEntry{at(15000, "Stop"), at(20000, evUserPromptSubmit, printed(pID))}, "not the session's PostToolUse"},
		"after the Stop":  {[]tapEntry{at(15000, "Stop"), at(20000, evPostToolUse, printed(pID))}, "after the turn's Stop"},
		"by a subagent":   {[]tapEntry{at(8100, evPostToolUse, agentID("a1"), printed(pID)), at(15000, "Stop")}, "not the session's PostToolUse"},
		"turn never ends": {[]tapEntry{at(8100, evPostToolUse, printed(pID))}, "no Stop after the send"},
		"never printed":   {[]tapEntry{at(15000, "Stop")}, "printed 0 times"},
	} {
		v := verdictMidTurn(tc.es, pSess, pID, pMarker, reply, 150)
		if !strings.Contains(strings.Join(v.fails, ";"), tc.want) {
			t.Errorf("%s: fails %v, want %q", name, v.fails, tc.want)
		}
	}
}

func boolp(b bool) *bool { return &b }

func TestVerdictSubagent(t *testing.T) {
	reply := "ID " + pID + " MARKER " + pMarker + "\nSUBAGENT-SAW: NONE"
	// Claude Code 2.1.288: the subagent's hooks carry agent_id; the parent's
	// PostToolUse of the Agent tool follows SubagentStop.
	claude := []tapEntry{
		at(0, "PreToolUse", tool("Agent")), at(100, "SubagentStart", agentID("a1")),
		at(200, "PreToolUse", tool("Bash"), agentID("a1")), at(6200, evPostToolUse, tool("Bash"), agentID("a1")),
		at(7000, "SubagentStop", agentID("a1")), at(7100, evPostToolUse, tool("Agent"), printed(pID)), at(9000, "Stop"),
	}
	if v := verdictSubagent(claude, pSess, pID, pMarker, reply, boolp(false), "/x/agent-a1.jsonl", nil); len(v.fails) != 0 {
		t.Fatalf("claude pass case: %v", v.fails)
	}
	// Devin 3000.11.1: nothing marks the subagent's hooks; the run_subagent
	// call's own PostToolUse is the parent's.
	devinOK := []tapEntry{
		at(0, "PreToolUse", tool("run_subagent")), at(100, "PreToolUse", tool("exec")), at(6100, evPostToolUse, tool("exec")),
		at(6500, "Stop"), at(7000, evPostToolUse, tool("run_subagent"), printed(pID)),
	}
	if v := verdictSubagent(devinOK, pSess, pID, pMarker, reply, boolp(false), "sessions.db", nil); len(v.fails) != 0 {
		t.Fatalf("devin pass case: %v", v.fails)
	}
	stolen := slices.Clone(devinOK)
	stolen[2].Printed, stolen[4].Printed = []string{pID}, nil
	for name, tc := range map[string]struct {
		es   []tapEntry
		seen *bool
		want string
	}{
		"claude subagent hook took it": {func() []tapEntry {
			es := slices.Clone(claude)
			es[3].Printed, es[5].Printed = []string{pID}, nil
			return es
		}(), boolp(false), "printed inside the subagent"},
		"devin subagent hook took it": {stolen, boolp(false), "printed inside the subagent"},
		"in the subagent transcript":  {claude, boolp(true), "transcript /x/agent-a1.jsonl holds"},
		"no subagent":                 {[]tapEntry{at(0, evPostToolUse, printed(pID))}, nil, "no subagent ran"},
		"printed twice":               {append(slices.Clone(claude), at(9500, evPostToolUse, printed(pID))), boolp(false), "printed 2 times"},
	} {
		v := verdictSubagent(tc.es, pSess, pID, pMarker, reply, tc.seen, "/x/agent-a1.jsonl", nil)
		if !strings.Contains(strings.Join(v.fails, ";"), tc.want) {
			t.Errorf("%s: fails %v, want %q", name, v.fails, tc.want)
		}
	}
}

func TestVerdictIdle(t *testing.T) {
	if v := verdictIdle(3, nil, false, "queued", pID, time.Minute); len(v.fails) != 0 {
		t.Fatalf("pass case: %v", v.fails)
	}
	v := verdictIdle(3, []tapEntry{at(5, evUserPromptSubmit, printed(pID))}, true, "delivered", pID, time.Minute)
	if len(v.fails) != 3 {
		t.Fatalf("woken session: %v", v.fails)
	}
}

// The idle case passed for a session whose hooks never ran: a quiet wait
// proves nothing then.
func TestVerdictIdleNeedsHookEvidence(t *testing.T) {
	v := verdictIdle(0, nil, false, "queued", pID, time.Minute)
	if !strings.Contains(strings.Join(v.fails, ";"), "no hook ever ran") {
		t.Fatalf("fails %v", v.fails)
	}
}

// The Devin subagent case passed ("no subagent transcript file to check")
// when Devin's store could not be read.
func TestVerdictSubagentUnreadableTranscript(t *testing.T) {
	seen, path, err := devinSubagentSeen(filepath.Join(t.TempDir(), "missing", "sessions.db"), "surf-feels", pMarker)
	if err == nil || seen != nil {
		t.Fatalf("missing store: %v %v", seen, err)
	}
	es := []tapEntry{at(0, "PreToolUse", tool("run_subagent")), at(100, "PreToolUse", tool("exec")),
		at(7000, evPostToolUse, tool("run_subagent"), printed(pID))}
	v := verdictSubagent(es, pSess, pID, pMarker, "ID "+pID+" MARKER "+pMarker, seen, path, err)
	if !strings.Contains(strings.Join(v.fails, ";"), "cannot read the subagent transcript") {
		t.Fatalf("fails %v", v.fails)
	}
}

func TestVerdictFraming(t *testing.T) {
	good := "Here they are:\nid=" + pID + " from=" + pSender + " intent=request marker=" + pMarker + "\n"
	if v := verdictFraming(good, pID, pSender, "request", pMarker); len(v.fails) != 0 {
		t.Fatalf("pass case: %v", v.fails)
	}
	// Models format a little differently: quotes, colons, backticks.
	loose := "- id: `" + pID + "`, from: \"" + pSender + "\", intent: request, marker: " + pMarker + "."
	if v := verdictFraming(loose, pID, pSender, "request", pMarker); len(v.fails) != 0 {
		t.Fatalf("loose quoting: %v", v.fails)
	}
	for name, tc := range map[string]struct{ reply, want string }{
		"sender lost":  {"id=" + pID + " from=unknown intent=request marker=" + pMarker, `from: quoted "unknown"`},
		"intent lost":  {"id=" + pID + " from=" + pSender + " marker=" + pMarker, `intent: quoted ""`},
		"marker only":  {"I saw " + pMarker, "did not quote the message id"},
		"wrong intent": {"id=" + pID + " from=" + pSender + " intent=inform marker=" + pMarker, `want "request"`},
	} {
		v := verdictFraming(tc.reply, pID, pSender, "request", pMarker)
		if !strings.Contains(strings.Join(v.fails, ";"), tc.want) {
			t.Errorf("%s: fails %v, want %q", name, v.fails, tc.want)
		}
	}
}

func TestVerdictGuardian(t *testing.T) {
	reply := "ID " + pID + " MARKER " + pMarker
	es := []tapEntry{at(0, "PreToolUse", tool("Bash")), at(4000, evPostToolUse, tool("Bash"), printed(pID)), at(6000, "Stop")}
	v := verdictGuardian(es, pSess, pID, pMarker, reply, 100, 3800)
	if len(v.fails) != 0 || !strings.Contains(strings.Join(v.facts, ";"), "hooks during it: none") {
		t.Fatalf("pass case: %+v", v)
	}
	stolen := []tapEntry{at(0, "PreToolUse", tool("Bash")), at(1000, evPostToolUse, tool("Bash"), printed(pID)), at(4000, evPostToolUse, tool("Bash"))}
	if v := verdictGuardian(stolen, pSess, pID, pMarker, reply, 100, 3800); !strings.Contains(strings.Join(v.fails, ";"), "printed during the review") {
		t.Fatalf("stolen: %v", v.fails)
	}
	if v := verdictGuardian(es, pSess, pID, pMarker, reply, 0, 0); !strings.Contains(strings.Join(v.fails, ";"), "no auto-review pass ran") {
		t.Fatalf("no review: %v", v.fails)
	}
}

func TestParseProbeFlags(t *testing.T) {
	var errOut strings.Builder
	o, err := parseProbeFlags([]string{"--harness", "codex", "--model", "gpt-x", "--case", "mid-turn,subagent", "--local"}, &errOut)
	if err != nil || o.models[transcript.AgentCodex] != "gpt-x" || !slices.Equal(o.cases, []string{"mid-turn", "subagent"}) || !o.local {
		t.Fatalf("got %+v %v", o, err)
	}
	o, err = parseProbeFlags(nil, &errOut)
	if err != nil || !slices.Equal(o.cases, probeCases) || o.idleWait != time.Minute {
		t.Fatalf("defaults: %+v %v", o, err)
	}
	for _, args := range [][]string{
		{"--model", "haiku"}, // bare model, no single harness
		{"--harness", "opencode"},
		{"--case", "wake"},
		{"--local", "--socket", "/tmp/a.sock"},
		{"--model", "cursor=x", "--harness", "claude"},
	} {
		if _, err := parseProbeFlags(args, &errOut); err == nil {
			t.Errorf("%v: accepted", args)
		}
	}
	o, err = parseProbeFlags([]string{"--model", "claude=sonnet,devin=swe-2-high"}, &errOut)
	if err != nil || o.models[transcript.AgentClaude] != "sonnet" || o.models[transcript.AgentDevin] != "swe-2-high" {
		t.Fatalf("pairs: %+v %v", o.models, err)
	}
}

func TestProbeEnv(t *testing.T) {
	in := []string{"PATH=/bin", "HOME=/u", "CLAUDECODE=1", "CLAUDE_CODE_ENTRYPOINT=cli", "CLAUDE_CODE_OAUTH_TOKEN=t", "CLAUDE_CONFIG_DIR=/c",
		"CODEX_THREAD_ID=x", "CODEX_HOME=/h", "DEVIN_PROJECT_DIR=/p", "CHISEL_SESSION_DB=/d", "FLOPWIRE_SESSION_ID=s", "XDG_DATA_HOME=/x"}
	got := probeEnv(in, false)
	want := []string{"PATH=/bin", "HOME=/u", "CLAUDE_CODE_OAUTH_TOKEN=t", "CLAUDE_CONFIG_DIR=/c", "XDG_DATA_HOME=/x"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v", got)
	}
	if got := probeEnv(in, true); slices.Contains(got, "HOME=/u") || slices.Contains(got, "XDG_DATA_HOME=/x") {
		t.Fatalf("scratch home kept the user's: %v", got)
	}
}

func TestProbeReportOutputs(t *testing.T) {
	rep := probeReport{Date: time.Date(2026, 10, 3, 15, 46, 0, 0, time.UTC), Flopwire: "dev", Mode: "local",
		Harness: []probeVersion{{Name: "claude", Version: "2.1.288 (Claude Code)", Model: "haiku"}},
		Results: []probeResult{{Harness: "claude", Case: caseIdle, Pass: true, Evidence: "no turn in 1m0s"},
			{Harness: "claude", Case: caseFraming, Evidence: "from: quoted \"a|b\""}}}
	var tbl strings.Builder
	if err := writeProbeTable(&tbl, rep); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tbl.String(), "claude   idle     PASS") || !strings.Contains(tbl.String(), "1 passed, 1 failed") {
		t.Fatalf("table:\n%s", tbl.String())
	}
	path := filepath.Join(t.TempDir(), "probe-runs.md")
	for range 2 {
		if err := appendProbeNotes(path, rep); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(path)
	s := string(b)
	if strings.Count(s, "# `flopwire probe` runs") != 1 || strings.Count(s, "## 2026-10-03 15:46Z") != 2 ||
		!strings.Contains(s, "claude: 2.1.288 (Claude Code), model haiku.") || !strings.Contains(s, `quoted "a\|b"`) {
		t.Fatalf("notes:\n%s", s)
	}
	if err := appendProbeNotes(filepath.Join(t.TempDir(), "no", "dir.md"), rep); err == nil {
		t.Fatal("appended into a missing directory")
	}
}

func TestMirrorDir(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "m")
	os.MkdirAll(filepath.Join(src, "s1", "subagents"), 0o700)
	os.WriteFile(filepath.Join(src, "s1.jsonl"), []byte("a\n"), 0o600)
	mirrorDir(src, dst)
	f, _ := os.OpenFile(filepath.Join(src, "s1.jsonl"), os.O_WRONLY|os.O_APPEND, 0o600)
	f.WriteString("b\n")
	f.Close()
	os.WriteFile(filepath.Join(src, "s1", "subagents", "agent-a1.jsonl"), []byte("c\n"), 0o600)
	mirrorDir(src, dst)
	if b, _ := os.ReadFile(filepath.Join(dst, "s1.jsonl")); string(b) != "a\nb\n" {
		t.Fatalf("appended copy %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "s1", "subagents", "agent-a1.jsonl")); string(b) != "c\n" {
		t.Fatalf("subagent copy %q", b)
	}
	os.WriteFile(filepath.Join(src, "s1.jsonl"), []byte("z\n"), 0o600) // rewritten shorter
	mirrorDir(src, dst)
	if b, _ := os.ReadFile(filepath.Join(dst, "s1.jsonl")); string(b) != "z\n" {
		t.Fatalf("shrunk source %q", b)
	}
}

// A case judges only its own window of the tap log, so a message the bus
// re-delivers on a later turn (or to another session) passed its case.
// The run-wide check fails it.
func TestProbeDeliveredOnceAcrossCases(t *testing.T) {
	reply := "ID " + pID + " MARKER " + pMarker
	window := []tapEntry{at(0, evUserPromptSubmit, printed(pID)), at(50, "Stop")}
	r := verdictPromptSubmit(window, pSess, pID, pMarker, reply).result(probeResult{Case: casePromptSubmit, Message: pID, session: pSess})
	if !r.Pass {
		t.Fatalf("window verdict: %s", r.Evidence)
	}
	clean := checkDeliveredOnce([]probeResult{r}, window)
	if !clean[0].Pass || clean[0].Evidence != r.Evidence {
		t.Fatalf("a single delivery changed the verdict: %+v", clean[0])
	}
	for name, tc := range map[string]struct {
		later tapEntry
		want  string
	}{
		"next turn":     {at(100, evUserPromptSubmit, printed(pID)), "printed 2 times in the run"},
		"other session": {at(100, evPostToolUse, printed(pID), func(e *tapEntry) { e.Session = pSender }), "printed 2 times in the run"},
		"subagent":      {at(100, evPostToolUse, printed(pID), agentID("a1")), "printed 2 times in the run"},
	} {
		got := checkDeliveredOnce([]probeResult{r}, append(slices.Clone(window), tc.later))
		if got[0].Pass || !strings.Contains(got[0].Evidence, tc.want) {
			t.Errorf("%s: %+v", name, got[0])
		}
	}
	// The framing case has no hook check of its own: a single delivery by
	// a hook of another session fails it here.
	f := probeResult{Case: caseFraming, Pass: true, Message: pID, session: pSess, Evidence: "model quoted id"}
	got := checkDeliveredOnce([]probeResult{f}, []tapEntry{at(0, evUserPromptSubmit, printed(pID), func(e *tapEntry) { e.Session = pSender })})
	if got[0].Pass || !strings.Contains(got[0].Evidence, "not the session's own hook") {
		t.Fatalf("framing delivered elsewhere: %+v", got[0])
	}
}

// A run in which no case applies to the chosen harnesses proved nothing
// and exited 0.
func TestRunProbeNoApplicableCase(t *testing.T) {
	o := probeOpts{harnesses: []transcript.Agent{transcript.AgentClaude}, cases: []string{caseGuardian}}
	if _, err := runProbe(t.Context(), o, io.Discard); err == nil || !strings.Contains(err.Error(), "no case") {
		t.Fatalf("got %v", err)
	}
}

// The notes file is committed: no home path, scratch path or full session
// id may reach it, even from a FAIL row's evidence.
func TestProbeNotesScrubbed(t *testing.T) {
	home, dir := "/Users/somebody", "/private/var/folders/xy/T/flopwire-probe-123"
	rep := probeReport{Date: time.Date(2026, 10, 3, 15, 46, 0, 0, time.UTC), Flopwire: "dev", Mode: "local", Dir: dir, home: home,
		Results: []probeResult{
			{Harness: "claude", Case: caseSubagent, session: pSess, sender: pSender,
				Evidence: "the subagent transcript " + home + "/.claude/projects/p/" + pSess + "/subagents/agent-a.jsonl holds PROBE-X"},
			{Harness: "claude", Case: caseFraming, session: pSess, sender: pSender, Evidence: `from: quoted "x", want "` + pSender + `"`},
			{Harness: "codex", Case: casePromptSubmit, Evidence: "sender session: boom (see " + dir + "/codex/sender.stderr)"},
		}}
	md := probeMarkdown(rep)
	for _, bad := range []string{home, dir, pSess, pSender, "somebody"} {
		if strings.Contains(md, bad) {
			t.Errorf("notes hold %q:\n%s", bad, md)
		}
	}
	for _, want := range []string{"~/.claude/projects", "<scratch>/codex/sender.stderr", clip(pSender, 8)} {
		if !strings.Contains(md, want) {
			t.Errorf("notes lack %q:\n%s", want, md)
		}
	}
}

// The probe never writes a user harness file: a login the harness
// refreshed in the scratch copy is deleted with the copy and reported, and
// the user's file keeps what it held.
func TestLoginCopyRelease(t *testing.T) {
	d := t.TempDir()
	src, dst := filepath.Join(d, "user", "auth.json"), filepath.Join(d, "scratch", "home", "auth.json")
	os.MkdirAll(filepath.Dir(src), 0o700)
	write := func(p, s string) { t.Helper(); os.WriteFile(p, []byte(s), 0o600) }
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }

	write(src, "v1")
	l, err := copyLogin(src, dst)
	if err != nil || read(dst) != "v1" {
		t.Fatalf("copy: %v %q", err, read(dst))
	}
	if err := l.release(); err != nil || read(src) != "v1" {
		t.Fatalf("unchanged release: %v %q", err, read(src))
	}
	if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("copy kept: %v", err)
	}

	l, _ = copyLogin(src, dst)
	write(dst, "v2-refreshed")
	err = l.release()
	if err == nil || !strings.Contains(err.Error(), "codex login") || read(src) != "v1" {
		t.Fatalf("refreshed copy: %v, user's file %q", err, read(src))
	}
	if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("copy kept: %v", err)
	}
}

// fakeJWT is an unsigned token with this exp claim.
func fakeJWT(exp time.Time) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(fmt.Sprintf(`{"exp":%d,"sub":"x"}`, exp.Unix()))) + ".sig"
}

// The probe skips Codex when a refresh could happen during the run: the
// refresh would use up the single-use refresh token the user's file holds.
func TestCodexRefreshDue(t *testing.T) {
	now := time.Date(2026, 10, 3, 16, 0, 0, 0, time.UTC)
	auth := func(lastRefresh string, access string) []byte {
		b, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt", "last_refresh": lastRefresh,
			"tokens": map[string]any{"access_token": access, "refresh_token": "rt-1", "id_token": "x"}})
		return b
	}
	fresh := now.Add(-24 * time.Hour).Format(time.RFC3339Nano)
	for name, tc := range map[string]struct {
		auth []byte
		due  bool
		why  string
	}{
		"fresh":                 {auth(fresh, fakeJWT(now.Add(5*24*time.Hour))), false, ""},
		"access expires in 20m": {auth(fresh, fakeJWT(now.Add(20*time.Minute))), true, "access token expires"},
		"access expires in 40m": {auth(fresh, fakeJWT(now.Add(40*time.Minute))), false, ""},
		"8-day refresh in 10m":  {auth(now.Add(-8*24*time.Hour+10*time.Minute).Format(time.RFC3339Nano), fakeJWT(now.Add(5*24*time.Hour))), true, "8-day refresh"},
		"8-day refresh past":    {auth(now.Add(-9*24*time.Hour).Format(time.RFC3339Nano), fakeJWT(now.Add(5*24*time.Hour))), true, "8-day refresh"},
		"no last_refresh":       {auth("", fakeJWT(now.Add(5*24*time.Hour))), true, "last_refresh"},
		"unreadable access":     {auth(fresh, "opaque"), true, "expiry is unreadable"},
		"api key, no tokens":    {[]byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"sk-x","tokens":null}`), false, ""},
		"not json":              {[]byte("{"), true, "not JSON"},
	} {
		due, why := codexRefreshDue(tc.auth, now, codexRefreshWindow)
		if due != tc.due || !strings.Contains(why, tc.why) {
			t.Errorf("%s: due %v (%q), want %v (%q)", name, due, why, tc.due, tc.why)
		}
	}
}

// A run asked only for Codex, with its refresh due, stops before touching
// anything and says how to fix it.
func TestRunProbeSkipsCodexWhenRefreshDue(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex is not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	b, _ := json.Marshal(map[string]any{"last_refresh": time.Now().Add(-10 * 24 * time.Hour).Format(time.RFC3339Nano),
		"tokens": map[string]any{"access_token": fakeJWT(time.Now().Add(time.Hour)), "refresh_token": "rt"}})
	os.WriteFile(filepath.Join(home, "auth.json"), b, 0o600)
	var log strings.Builder
	rep, err := runProbe(t.Context(), probeOpts{harnesses: []transcript.Agent{transcript.AgentCodex}, cases: probeCases, local: true, dir: t.TempDir()}, &log)
	if err == nil || !strings.Contains(err.Error(), "run `codex` once to refresh, then rerun the probe") || len(rep.Skipped) != 1 {
		t.Fatalf("err %v, skipped %v", err, rep.Skipped)
	}
	if !strings.Contains(log.String(), "SKIP codex") {
		t.Fatalf("log %q", log.String())
	}
}

// A setup that failed after copying the login file returned before the
// deferred delete was registered, leaving the copy behind.
func TestProbeSetupFailureRemovesLogin(t *testing.T) {
	user := t.TempDir()
	os.WriteFile(filepath.Join(user, "auth.json"), []byte("secret"), 0o600)
	t.Setenv("CODEX_HOME", user)
	p := &prober{o: probeOpts{cases: []string{casePromptSubmit}}, dir: t.TempDir(), exe: "/bin/true", sock: "/nonexistent.sock", home: t.TempDir(), log: io.Discard}
	// config.toml cannot be written: setup fails after the copy.
	os.MkdirAll(filepath.Join(p.dir, "codex", "home", "config.toml"), 0o700)
	res := p.runHarness(t.Context(), transcript.AgentCodex, "m")
	if len(res) != 1 || res[0].Pass || !strings.Contains(res[0].Evidence, "setup failed") {
		t.Fatalf("results %+v", res)
	}
	if _, err := os.Stat(filepath.Join(p.dir, "codex", "home", "auth.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("login copy left behind: %v", err)
	}
}

type panicWriter struct{}

func (panicWriter) Write([]byte) (int, error) { panic("boom") }

// The harnesses run in goroutines: a panic in one ended the process before
// the others deleted their login copies. It is now that harness's failure.
func TestProbeHarnessPanicIsAFailure(t *testing.T) {
	p := &prober{o: probeOpts{cases: []string{caseIdle, casePromptSubmit}}, dir: t.TempDir(), log: panicWriter{}}
	res := p.runHarness(t.Context(), transcript.AgentClaude, "m")
	if len(res) != 2 || res[0].Pass || !strings.Contains(res[0].Evidence, "panic: boom") {
		t.Fatalf("results %+v", res)
	}
}

// `codex --version` run with the user's environment wrote the user's
// CODEX_HOME (tmp/arg0). It runs in the probe's scratch home.
func TestHarnessVersionScratchHome(t *testing.T) {
	bin, dir := t.TempDir(), t.TempDir()
	out := filepath.Join(bin, "env.txt")
	script := "#!/bin/sh\necho \"CODEX_HOME=$CODEX_HOME HOME=$HOME\" > '" + out + "'\necho 'codex-cli 9.9'\n"
	for _, name := range []string{"codex", "devin"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CODEX_HOME", "/user/codex")
	p := &prober{dir: dir, home: "/user"}
	if v := p.harnessVersion(t.Context(), transcript.AgentCodex); v != "codex-cli 9.9" {
		t.Fatalf("version %q", v)
	}
	if b, _ := os.ReadFile(out); !strings.Contains(string(b), "CODEX_HOME="+filepath.Join(dir, "codex", "home")+" ") {
		t.Fatalf("codex ran with %s", b)
	}
	p.harnessVersion(t.Context(), transcript.AgentDevin)
	if b, _ := os.ReadFile(out); !strings.Contains(string(b), "HOME="+filepath.Join(dir, "devin", "home")) {
		t.Fatalf("devin ran with %s", b)
	}
}

// Skipping Codex for a due refresh could leave harnesses with no case
// that applies (claude + guardian): the run then proved nothing and
// exited 0.
func TestRunProbeSkipLeavesNoCase(t *testing.T) {
	for _, bin := range []string{"claude", "codex"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " is not installed")
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	b, _ := json.Marshal(map[string]any{"last_refresh": time.Now().Add(-10 * 24 * time.Hour).Format(time.RFC3339Nano),
		"tokens": map[string]any{"access_token": fakeJWT(time.Now().Add(time.Hour)), "refresh_token": "rt"}})
	os.WriteFile(filepath.Join(home, "auth.json"), b, 0o600)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	o := probeOpts{harnesses: []transcript.Agent{transcript.AgentClaude, transcript.AgentCodex}, cases: []string{caseGuardian}, local: true, dir: t.TempDir()}
	if _, err := runProbe(ctx, o, io.Discard); err == nil || !strings.Contains(err.Error(), "no case") {
		t.Fatalf("got %v", err)
	}
}
