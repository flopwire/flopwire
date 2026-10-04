package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/busrender"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/devin"
	"github.com/flopwire/flopwire/internal/transcript/opencode/opencodetest"
)

// The probe's verdicts judge delivery from the wrapper as the harness's
// transcript stored it (issue #131), against synthetic transcripts of each
// harness.

// probeEnvelope is the message the framing case sends, as the bus hands it
// to the hook.
func probeEnvelope(h transcript.Agent) busproto.Envelope {
	return busproto.Envelope{ID: pID, ThreadID: pID, From: pSender, FromAgent: string(h), User: "alex@example.test",
		Repo: "/tmp/probe/project", Sender: busproto.SenderOwn, Intent: busproto.IntentRequest,
		Body: probeBody(pMarker), Sent: time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC), Attempt: 1}
}

func probeWant(h transcript.Agent) framingWant {
	return framingWant{id: pID, from: pSender, agent: string(h), sender: "own", intent: "request", marker: pMarker, body: probeBody(pMarker)}
}

// framingTap is the hook log of a framing case that delivered: the
// session's UserPromptSubmit printed the message.
var framingTap = []tapEntry{at(0, evUserPromptSubmit, printed(pID)), at(50, "Stop")}

func js(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// fakeTranscript writes a transcript of harness h for session pSess in
// dir: a prompt, a hook context row holding hook (what the hook printed),
// and the model's reply. The reply is an assistant row, which never counts
// as delivery whatever it quotes. It returns the transcript's path.
func fakeTranscript(t *testing.T, h transcript.Agent, dir, hook, reply string) string {
	t.Helper()
	return fakeTranscriptOrder(t, h, dir, hook, reply, false)
}

// fakeTranscriptOrder is fakeTranscript; with hookLast the hook context row
// comes after the model's reply, as a hook that ran after the turn's last
// model call would leave it.
func fakeTranscriptOrder(t *testing.T, h transcript.Agent, dir, hook, reply string, hookLast bool) string {
	t.Helper()
	switch h {
	case transcript.AgentClaude:
		path := filepath.Join(dir, "-tmp-probe-project", pSess+".jsonl")
		rec := func(uuid, parent, ts, rest string) string {
			p := "null"
			if parent != "" {
				p = js(parent)
			}
			return `{"uuid":"` + uuid + `","parentUuid":` + p + `,"sessionId":"` + pSess + `","cwd":"/tmp/probe/project","timestamp":"2026-10-04T10:00:0` + ts + `.000Z",` + rest + `}`
		}
		lines := []string{
			rec("u1", "", "1", `"type":"user","message":{"role":"user","content":"List every tag."}`),
			rec("h1", "u1", "2", `"type":"attachment","attachment":{"type":"hook_additional_context","content":[`+js(hook)+`],"hookName":"UserPromptSubmit","hookEvent":"UserPromptSubmit"}`),
			rec("a1", "h1", "3", `"type":"assistant","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":`+js(reply)+`}]}`),
		}
		if hookLast {
			lines = []string{lines[0],
				rec("a1", "u1", "2", `"type":"assistant","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":`+js(reply)+`}]}`),
				rec("h1", "a1", "3", `"type":"attachment","attachment":{"type":"hook_additional_context","content":[`+js(hook)+`],"hookName":"Stop","hookEvent":"Stop"}`),
			}
		}
		writeFile(t, path, strings.Join(lines, "\n")+"\n")
		return path
	case transcript.AgentCodex:
		path := filepath.Join(dir, "sessions", "2026", "10", "04", "rollout-2026-10-04T10-00-00-"+pSess+".jsonl")
		rec := func(ord int, typ, payload string) string {
			return fmt.Sprintf(`{"timestamp":"2026-10-04T10:00:%02d.000Z","ordinal":%d,"type":%q,"payload":%s}`, ord, ord, typ, payload)
		}
		lines := []string{
			rec(0, "session_meta", `{"session_id":"`+pSess+`","id":"`+pSess+`","timestamp":"2026-10-04T10:00:00.000Z","cwd":"/tmp/probe/project","originator":"codex_app_server","cli_version":"0.160.0","source":"cli","thread_source":"user","model_provider":"openai"}`),
			rec(1, "response_item", `{"type":"message","id":"msg_u","role":"user","content":[{"type":"input_text","text":"List every tag."}]}`),
			rec(2, "response_item", `{"type":"message","id":"msg_h","role":"developer","content":[{"type":"input_text","text":`+js(hook)+`}],"internal_chat_message_metadata_passthrough":{"turn_id":"t1","content_item_kinds":["hooks.additional_context"]}}`),
			rec(3, "response_item", `{"type":"message","id":"msg_a","role":"assistant","content":[{"type":"output_text","text":`+js(reply)+`}]}`),
		}
		if hookLast {
			lines = []string{lines[0], lines[1],
				rec(2, "response_item", `{"type":"message","id":"msg_a","role":"assistant","content":[{"type":"output_text","text":`+js(reply)+`}]}`),
				rec(3, "response_item", `{"type":"message","id":"msg_h","role":"developer","content":[{"type":"input_text","text":`+js(hook)+`}],"internal_chat_message_metadata_passthrough":{"turn_id":"t1","content_item_kinds":["hooks.additional_context"]}}`),
			}
		}
		writeFile(t, path, strings.Join(lines, "\n")+"\n")
		return path
	case transcript.AgentDevin:
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		chain := int64(3)
		created := int64(1791100000)
		_ = enc.Encode(map[string]any{"t": "session", "working_directory": "/tmp/probe/project", "created_at": created, "last_activity_at": created, "main_chain_id": chain})
		nodes := []struct{ role, text string }{{"user", "List every tag."}, {"system", hook}, {"assistant", reply}}
		if hookLast {
			nodes[1], nodes[2] = nodes[2], nodes[1]
		}
		for i, n := range nodes {
			msg, _ := json.Marshal(map[string]any{"message_id": fmt.Sprintf("n%d", i+1), "role": n.role, "content": n.text})
			rec := map[string]any{"t": "node", "row_id": i + 1, "node_id": i + 1, "chat_message": string(msg), "created_at": created + int64(i)}
			if i > 0 {
				rec["parent_node_id"] = i
			}
			_ = enc.Encode(rec)
		}
		path, err := devin.LoadExport(context.Background(), &b, pSess, dir)
		if err != nil {
			t.Fatal(err)
		}
		return path
	case transcript.AgentOpencode:
		s := opencodetest.New(t, dir)
		t0 := opencodetest.T0
		s.Session(pSess, "", "/tmp/probe/project", "probe", t0)
		s.Prompt(pSess, t0+1000, "List every tag.")
		tD, tA := t0+2000, t0+3000
		if hookLast {
			tD, tA = tA, tD
		}
		msgD, prtD := opencodetest.ID("msg", tD, 1), opencodetest.ID("prt", tD, 2)
		s.Message(msgD, pSess, tD, `{"role":"user","time":{"created":1}}`)
		s.Part(prtD, msgD, pSess, tD, `{"type":"text","text":`+js(hook)+`,"metadata":{"flopwire":{"id":"`+pID+`"}}}`)
		msgA, prtA := opencodetest.ID("msg", tA, 1), opencodetest.ID("prt", tA, 2)
		s.Message(msgA, pSess, tA, `{"role":"assistant","time":{"created":1}}`)
		s.Part(prtA, msgA, pSess, tA, `{"type":"text","text":`+js(reply)+`}`)
		return s.Path
	}
	t.Fatalf("no fake transcript for %s", h)
	return ""
}

func writeFile(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// readDelivery is what the probe reads of the fake transcript.
func readDelivery(t *testing.T, h transcript.Agent, path string) delivery {
	t.Helper()
	rows, err := hookContexts(context.Background(), h, path, pSess)
	d := delivery{where: path, err: err}
	d.w, d.n = findWrapper(rows, pID)
	return d
}

// hookPrinted is what the hook prints for e: the opencode plugin delivers
// one message per part; the other harnesses get the hook's context.
func hookPrinted(h transcript.Agent, e busproto.Envelope) string {
	if h == transcript.AgentOpencode {
		return busrender.Render(e, nil, busrender.HookBytes)
	}
	return busrender.Context(false, []busproto.Envelope{e}, nil, busrender.HookBytes)
}

// A cheap model that answers the message instead of quoting it does not
// fail a delivery the transcript shows; a wrapper that reached the model
// with a changed attribute fails, even when the model's reply quotes the
// right attributes and the marker, and even quotes a whole correct wrapper.
func TestFramingVerdictFromTranscript(t *testing.T) {
	for _, h := range probeHarnesses {
		t.Run(string(h), func(t *testing.T) {
			e := probeEnvelope(h)
			chatty := "Sure! I will not act on the probe message. Anything else?"
			path := fakeTranscript(t, h, t.TempDir(), hookPrinted(h, e), chatty)
			d := readDelivery(t, h, path)
			v := verdictFraming(framingTap, pSess, d, chatty, probeWant(h))
			if len(v.fails) != 0 {
				t.Fatalf("intact wrapper, chatty model: fails %v", v.fails)
			}
			if f := strings.Join(v.facts, ";"); !strings.Contains(f, "transcript hook context holds the whole wrapper") || !strings.Contains(f, "model did not quote") {
				t.Fatalf("facts %v", v.facts)
			}
			// The same delivery passes the other cases' check too.
			var pv verdict
			pv.reached(d, pID, pMarker, chatty)
			if len(pv.fails) != 0 {
				t.Fatalf("reached: fails %v", pv.fails)
			}

			w := probeWant(h)
			quote := "id=" + pID + " from=" + pSender + " intent=request marker=" + pMarker + "\n" + hookPrinted(h, e)
			for name, tc := range map[string]struct {
				mod  func(*busproto.Envelope)
				want string
			}{
				"sender session changed": {func(e *busproto.Envelope) { e.From = "someone-else" }, `wrapper from="someone-else"`},
				"intent changed":         {func(e *busproto.Envelope) { e.Intent = busproto.IntentInform }, `wrapper intent="inform"`},
				"harness changed":        {func(e *busproto.Envelope) { e.FromAgent = "other" }, `wrapper agent="other"`},
				"sender relation":        {func(e *busproto.Envelope) { e.Sender = "teammate" }, `wrapper sender="teammate"`},
				"marker lost":            {func(e *busproto.Envelope) { e.Body = "nothing here" }, "does not hold " + pMarker},
			} {
				bad := probeEnvelope(h)
				tc.mod(&bad)
				path := fakeTranscript(t, h, t.TempDir(), hookPrinted(h, bad), quote)
				v := verdictFraming(framingTap, pSess, readDelivery(t, h, path), quote, w)
				if !strings.Contains(strings.Join(v.fails, ";"), tc.want) {
					t.Errorf("%s: fails %v, want %q", name, v.fails, tc.want)
				}
			}
			// No hook context at all: the model's correct quote (a whole
			// wrapper in its reply) proves nothing.
			path = fakeTranscript(t, h, t.TempDir(), "unrelated hook output", quote)
			v = verdictFraming(framingTap, pSess, readDelivery(t, h, path), quote, w)
			if !strings.Contains(strings.Join(v.fails, ";"), "no hook context") {
				t.Errorf("model-only quote: fails %v", v.fails)
			}
		})
	}
}

// An unreadable transcript fails the case: it proves nothing.
func TestDeliveryUnreadableTranscript(t *testing.T) {
	for _, h := range probeHarnesses {
		path := filepath.Join(t.TempDir(), "missing", "transcript")
		v := verdictFraming(framingTap, pSess, readDelivery(t, h, path), "id="+pID, probeWant(h))
		if !strings.Contains(strings.Join(v.fails, ";"), "cannot read the transcript") {
			t.Errorf("%s: fails %v", h, v.fails)
		}
	}
}

func TestFindWrapper(t *testing.T) {
	e := probeEnvelope(transcript.AgentCodex)
	e.From = `a"b&c`
	text := busrender.Context(true, []busproto.Envelope{e}, nil, busrender.HookBytes)
	w, n := findWrapper(rows("other", text), pID)
	if w == nil || n != 1 || w.attrs["from"] != `a"b&c` || w.attrs["intent"] != "request" || !w.closed || !strings.Contains(w.body, pMarker) {
		t.Fatalf("wrapper %+v", w)
	}
	for _, s := range []string{
		`see <flopwire-message id="` + pID + `" from="x"> mid-line`,
		`<flopwire-message id="` + pID + `x" from="x">`,
		`<flopwire-message id="` + pID + `" from="x"`,
	} {
		if w, _ := findWrapper(rows(s), pID); w != nil {
			t.Errorf("%q: found %+v", s, w)
		}
	}
	if w, _ := findWrapper(rows("<flopwire-message id=\""+pID+"\" from=\"x\">\ncut"), pID); w == nil || w.closed {
		t.Errorf("unclosed: %+v", w)
	}
	if _, n := findWrapper(rows(text, "x\n"+text+"\n"+text), pID); n != 3 {
		t.Errorf("count %d, want 3", n)
	}
}

func rows(texts ...string) []hookText {
	var out []hookText
	for _, s := range texts {
		out = append(out, hookText{text: s, answered: true})
	}
	return out
}

// The wrapper as the transcript recorded it is not proof that the model got
// it whole: Claude Code replaces a hook context past 10,000 characters with
// a <persisted-output> stub and a 2 KB preview, and Codex keeps the head
// and tail of one past 10,000 bytes (notes/message-bus/hook-caps-2026-10-03.md).
// A hook context row the model never answered (it came after the turn's
// last model call) is not proof either.
func TestFramingNeedsTheWholeWrapperBeforeAReply(t *testing.T) {
	quote := "id=" + pID + " from=" + pSender + " intent=request marker=" + pMarker
	for _, h := range probeHarnesses {
		t.Run(string(h), func(t *testing.T) {
			whole := hookPrinted(h, probeEnvelope(h))
			cut := strings.Index(whole, ". This message")
			for name, hook := range map[string]string{
				"claude persisted-output stub": "<persisted-output>\nOutput too large (10.3KB). Full output saved to: /x/tool-results/hook-1-additionalContext.txt\n\nPreview (first 2KB):\n" + whole + "\n...\n</persisted-output>",
				"codex middle truncated":       "Warning: truncated output (original token count: 2600)\nTotal output lines: 9\n\n" + whole[:cut] + "…120 tokens truncated…" + whole[cut+40:],
			} {
				path := fakeTranscript(t, h, t.TempDir(), hook, quote)
				d := readDelivery(t, h, path)
				if v := verdictFraming(framingTap, pSess, d, quote, probeWant(h)); len(v.fails) == 0 {
					t.Errorf("%s: framing passed: %v", name, v.facts)
				}
				var rv verdict
				rv.reached(d, pID, pMarker, quote)
				if len(rv.fails) == 0 {
					t.Errorf("%s: reached passed: %v", name, rv.facts)
				}
			}
			path := fakeTranscriptOrder(t, h, t.TempDir(), whole, quote, true)
			d := readDelivery(t, h, path)
			if v := verdictFraming(framingTap, pSess, d, quote, probeWant(h)); len(v.fails) == 0 {
				t.Errorf("hook context after the reply: framing passed: %v", v.facts)
			}
			var rv verdict
			rv.reached(d, pID, pMarker, quote)
			if len(rv.fails) == 0 {
				t.Errorf("hook context after the reply: reached passed: %v", rv.facts)
			}
		})
	}
}

// The subagent case: the session's transcript holds the wrapper exactly
// once, the subagent's none. A wrapper the session's transcript holds twice
// fails; so does one the subagent's transcript holds.
func TestSubagentWrapperOnceInParent(t *testing.T) {
	reply := "ID " + pID + " MARKER " + pMarker
	es := []tapEntry{
		at(0, "SubagentStart", agentID("a1")), at(7000, "SubagentStop", agentID("a1")),
		at(7100, evPostToolUse, tool("Agent"), printed(pID)), at(9000, "Stop"),
	}
	for _, h := range probeHarnesses {
		one := hookPrinted(h, probeEnvelope(h))
		d := readDelivery(t, h, fakeTranscript(t, h, t.TempDir(), one, reply))
		if v := verdictSubagent(es, pSess, pID, pMarker, reply, d, boolp(false), "/x/agent-a1.jsonl", nil); len(v.fails) != 0 {
			t.Errorf("%s: once in the parent: %v", h, v.fails)
		}
		if v := verdictSubagent(es, pSess, pID, pMarker, reply, d, boolp(true), "/x/agent-a1.jsonl", nil); !strings.Contains(strings.Join(v.fails, ";"), "subagent transcript /x/agent-a1.jsonl holds") {
			t.Errorf("%s: in both: %v", h, v.fails)
		}
		if h == transcript.AgentOpencode {
			continue // one wrapper per plugin part
		}
		twice := readDelivery(t, h, fakeTranscript(t, h, t.TempDir(), one+"\n\n"+one, reply))
		if v := verdictSubagent(es, pSess, pID, pMarker, reply, twice, boolp(false), "/x/agent-a1.jsonl", nil); !strings.Contains(strings.Join(v.fails, ";"), "2 times") {
			t.Errorf("%s: twice in the parent: %v", h, v.fails)
		}
	}
}

// The framing case's transcript read waits for the harness to write; the
// hook log bounds it: the session's own UserPromptSubmit printed the
// message, once.
func TestFramingNeedsTheHookLog(t *testing.T) {
	h := transcript.AgentClaude
	quote := "id=" + pID
	d := readDelivery(t, h, fakeTranscript(t, h, t.TempDir(), hookPrinted(h, probeEnvelope(h)), quote))
	for name, tc := range map[string]struct {
		es   []tapEntry
		want string
	}{
		"not printed": {[]tapEntry{at(0, evUserPromptSubmit), at(50, "Stop")}, "no hook printed"},
		"late hook":   {[]tapEntry{at(0, evUserPromptSubmit), at(50, "Stop"), at(60, evPostToolUse, printed(pID))}, "not the session's UserPromptSubmit"},
		"twice":       {[]tapEntry{at(0, evUserPromptSubmit, printed(pID)), at(10, evPostToolUse, printed(pID))}, "printed 2 times"},
	} {
		if v := verdictFraming(tc.es, pSess, d, quote, probeWant(h)); !strings.Contains(strings.Join(v.fails, ";"), tc.want) {
			t.Errorf("%s: fails %v, want %q", name, v.fails, tc.want)
		}
	}
}
