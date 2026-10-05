package main

// The probe's verdicts (probe.go): pure functions of the tap log
// (probe_tap.go), the harness's final reply and the bus state, so they can
// be tested against fake harness output.

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Probe cases.
const (
	casePromptSubmit = "prompt-submit"
	caseMidTurn      = "mid-turn"
	caseSubagent     = "subagent"
	caseIdle         = "idle"
	caseFraming      = "framing"
	caseGuardian     = "guardian" // Codex only: a message queued during an auto-review pass
)

// probeCases is every case in the order the probe runs them: idle leaves
// its message queued, and the prompt-submit turn then delivers it too.
var probeCases = []string{caseIdle, casePromptSubmit, caseFraming, caseMidTurn, caseSubagent, caseGuardian}

// probeResult is one case's verdict.
type probeResult struct {
	Harness  string `json:"harness"`
	Case     string `json:"case"`
	Pass     bool   `json:"pass"`
	Evidence string `json:"evidence"`
	Message  string `json:"message_id,omitempty"`
	Marker   string `json:"marker,omitempty"`

	session string // the session the message was sent to
	sender  string // the session that sent it
}

func (r probeResult) verdict() string {
	if r.Pass {
		return "PASS"
	}
	return "FAIL"
}

// verdict collects a case's checks: each failed check is a reason, each
// fact a piece of evidence.
type verdict struct {
	fails, facts []string
}

func (v *verdict) fact(format string, a ...any) { v.facts = append(v.facts, fmt.Sprintf(format, a...)) }
func (v *verdict) fail(format string, a ...any) { v.fails = append(v.fails, fmt.Sprintf(format, a...)) }

func (v verdict) result(r probeResult) probeResult {
	r.Pass = len(v.fails) == 0
	r.Evidence = strings.Join(append(v.fails, v.facts...), "; ")
	return r
}

// printers are the entries that printed the message id.
func printers(entries []tapEntry, id string) []tapEntry {
	var out []tapEntry
	for _, e := range entries {
		for _, p := range e.Printed {
			if p == id {
				out = append(out, e)
				break
			}
		}
	}
	return out
}

// describe names a hook entry for evidence: its event, tool and, inside a
// subagent, the agent id.
func describe(e tapEntry) string {
	s := e.Event
	if e.Tool != "" {
		s += " " + e.Tool
	}
	if e.AgentID != "" {
		s += " agent_id=" + clip(e.AgentID, 12)
	}
	return s
}

func describeAll(es []tapEntry) string {
	if len(es) == 0 {
		return "none"
	}
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = describe(e)
	}
	return strings.Join(parts, ", ")
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// reached checks that the message reached the model: the recipient's
// transcript holds its wrapper, with the marker, in a row the harness
// records as hook context (probe_wrapper.go). The model quoting the marker
// is corroboration, shown as evidence: a cheap model that answers the
// message instead of quoting it does not turn a delivery into a FAIL.
func (v *verdict) reached(d delivery, id, marker, reply string) {
	switch {
	case d.err != nil:
		v.fail("cannot read the transcript %s: %v", d.where, d.err)
	case d.w == nil:
		v.fail("no hook context in the transcript %s holds %s's wrapper", d.where, id)
	case !strings.Contains(d.w.body, marker):
		v.fail("%s's wrapper in the transcript does not hold %s", id, marker)
	case d.w.whole(id) != nil:
		v.fail("%v", d.w.whole(id))
	default:
		v.fact("transcript hook context holds %s with %s", id, marker)
	}
	v.corroborate(reply, marker)
}

// corroborate records whether the model quoted the marker. It never fails
// a case.
func (v *verdict) corroborate(reply, marker string) {
	if strings.Contains(reply, marker) {
		v.fact("model quoted %s", marker)
		return
	}
	v.fact("model did not quote %s (reply: %q)", marker, clip(oneLine(reply), 80))
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// checkDeliveredOnce judges every case's message against the whole run's
// tap log: each case reads only its own window, so a message the bus
// delivers again on a later turn, or a second time to another session or
// a subagent, would otherwise pass. A message printed more than once, or
// by a hook other than its session's own, fails its case.
func checkDeliveredOnce(results []probeResult, entries []tapEntry) []probeResult {
	out := slices.Clone(results)
	for i, r := range out {
		if r.Message == "" || r.session == "" {
			continue
		}
		ps := printers(entries, r.Message)
		var why string
		switch {
		case len(ps) > 1:
			why = fmt.Sprintf("%s printed %d times in the run (%s)", r.Message, len(ps), describeAll(ps))
		case len(ps) == 1 && (ps[0].Session != r.session || ps[0].AgentID != ""):
			why = fmt.Sprintf("%s printed by %s, not the session's own hook", r.Message, describe(ps[0]))
		default:
			continue
		}
		out[i].Pass = false
		out[i].Evidence = why + "; " + r.Evidence
	}
	return out
}

// verdictPromptSubmit: a message queued before the prompt is printed by
// the session's UserPromptSubmit hook, once, and reaches the model: its
// wrapper is in the transcript's hook context (verdict.reached).
func verdictPromptSubmit(entries []tapEntry, session, id, marker, reply string, d delivery) verdict {
	var v verdict
	v.promptSubmitPrinted(entries, session, id)
	v.reached(d, id, marker, reply)
	return v
}

// promptSubmitPrinted checks the hook log: the session's own
// UserPromptSubmit printed id, once. It bounds the transcript read, which
// waits for the harness to write: only the turn's first hook printed it.
func (v *verdict) promptSubmitPrinted(entries []tapEntry, session, id string) {
	ps := printers(entries, id)
	switch {
	case len(ps) == 0:
		v.fail("no hook printed %s", id)
	case len(ps) > 1:
		v.fail("%s printed %d times (%s)", id, len(ps), describeAll(ps))
	case ps[0].Event != evUserPromptSubmit || ps[0].Session != session || ps[0].AgentID != "":
		v.fail("%s printed by %s, not the session's UserPromptSubmit", id, describe(ps[0]))
	default:
		v.fact("UserPromptSubmit printed %s", id)
	}
}

// verdictMidTurn: a message queued while a tool runs is printed by the
// session's next PostToolUse, before the turn's Stop, and reaches the
// model (verdict.reached).
func verdictMidTurn(entries []tapEntry, session, id, marker, reply string, sentAt int64, d delivery) verdict {
	var v verdict
	ps := printers(entries, id)
	if len(ps) != 1 {
		v.fail("%s printed %d times (%s), want once", id, len(ps), describeAll(ps))
		v.reached(d, id, marker, reply)
		return v
	}
	p := ps[0]
	if p.Event != evPostToolUse || p.Session != session || p.AgentID != "" {
		v.fail("%s printed by %s, not the session's PostToolUse", id, describe(p))
	}
	var stop *tapEntry
	for i, e := range entries {
		if e.Event == "Stop" && e.Session == session && e.AgentID == "" && e.At >= sentAt {
			stop = &entries[i]
			break
		}
	}
	switch {
	case stop == nil:
		v.fail("no Stop after the send: the turn's end is unknown")
	case p.Done > stop.At:
		v.fail("%s printed after the turn's Stop", id)
	default:
		v.fact("%s printed %s, %s before the turn's Stop", describe(p), id, time.Duration(stop.At-p.Done)*time.Millisecond)
	}
	v.reached(d, id, marker, reply)
	return v
}

// subagentWindow is when a subagent ran, from the tap log: SubagentStart
// to SubagentStop (Claude Code, Codex), or the run_subagent (Devin, whose
// subagent hooks carry no agent id) or task (opencode) call's PreToolUse
// to its PostToolUse. Zero when none ran.
type subagentWindow struct {
	start, end   int64
	startE, endE tapEntry
}

func findSubagent(entries []tapEntry, session string) (w subagentWindow, ok bool) {
	started := false
	for _, e := range entries {
		if e.Session != session {
			continue
		}
		isStart := e.Event == "SubagentStart" || (e.Event == "PreToolUse" && (e.Tool == "run_subagent" || e.Tool == "task"))
		isEnd := e.Event == "SubagentStop" || (e.Event == evPostToolUse && (e.Tool == "run_subagent" || e.Tool == "task"))
		switch {
		case !started && isStart:
			started, w.start, w.startE = true, e.At, e
		case started && isEnd:
			w.end, w.endE = e.At, e
			return w, true
		}
	}
	return w, false
}

// insideSubagent reports whether a hook ran in a subagent: it has an
// agent id, or it is not the run_subagent call's own hook and ran while
// that call did (Devin).
func (w subagentWindow) inside(e tapEntry) bool {
	if e.AgentID != "" {
		return true
	}
	if w.startE.Tool != "run_subagent" {
		return false
	}
	return e.At > w.start && e.At < w.end
}

// verdictSubagent: a message queued while a subagent runs is printed once,
// by a hook of the session itself after the subagent returned, never by a
// hook inside the subagent; the subagent's transcript does not hold it;
// it reaches the model (verdict.reached). subSeen is whether the subagent's transcript holds
// the marker, nil when the harness has no transcript file to check;
// subErr is why the transcript could not be read, which fails the case:
// an unread transcript proves nothing.
func verdictSubagent(entries []tapEntry, session, id, marker, reply string, d delivery, subSeen *bool, subPath string, subErr error) verdict {
	var v verdict
	w, ok := findSubagent(entries, session)
	if !ok {
		v.fail("no subagent ran to completion (no SubagentStart/SubagentStop or run_subagent pair in the hooks)")
		v.reached(d, id, marker, reply)
		return v
	}
	var inSub int
	for _, e := range entries {
		if w.inside(e) {
			inSub++
		}
	}
	v.fact("%d hooks ran inside the subagent", inSub)
	ps := printers(entries, id)
	switch {
	case len(ps) == 0:
		v.fail("no hook printed %s", id)
	case len(ps) > 1:
		v.fail("%s printed %d times (%s)", id, len(ps), describeAll(ps))
	case w.inside(ps[0]):
		v.fail("%s printed inside the subagent by %s", id, describe(ps[0]))
	case ps[0].Session != session:
		v.fail("%s printed for session %s", id, ps[0].Session)
	case ps[0].At < w.end:
		v.fail("%s printed by the session's %s before the subagent returned", id, describe(ps[0]))
	default:
		v.fact("the session's %s printed %s after %s", describe(ps[0]), id, describe(w.endE))
	}
	switch {
	case subErr != nil:
		v.fail("cannot read the subagent transcript %s: %v", subPath, subErr)
	case subSeen == nil:
		v.fact("no subagent transcript file to check")
	case *subSeen:
		v.fail("the subagent transcript %s holds %s", subPath, marker)
	default:
		v.fact("subagent transcript clean")
	}
	if d.n > 1 {
		v.fail("the session's transcript holds %s's wrapper %d times, want once", id, d.n)
	}
	v.reached(d, id, marker, reply)
	return v
}

// verdictIdle: a message sent to an idle session starts no turn within the
// window and stays queued. hooksBefore counts the session's hooks before
// the wait: with none, the hooks are not wired, and a quiet wait proves
// nothing.
func verdictIdle(hooksBefore int, during []tapEntry, harnessTurn bool, state, id string, window time.Duration) verdict {
	var v verdict
	if hooksBefore == 0 {
		v.fail("no hook ever ran for the session before the wait: the hooks are not wired, so a quiet wait proves nothing")
	}
	if len(during) > 0 {
		v.fail("hooks ran during the wait (%s): a turn started", describeAll(during))
	}
	if harnessTurn {
		v.fail("the harness started a turn during the wait")
	}
	if state != "queued" {
		v.fail("%s is %q after %s, want queued", id, state, window)
	}
	if len(v.fails) == 0 {
		v.fact("no turn in %s; %s still queued", window, id)
	}
	return v
}

// framingAttr reads one attribute of the model's quote of a wrapper.
var framingAttr = regexp.MustCompile(`(?i)\b(id|from|intent|marker)\s*[=:]\s*["']?([^\s"',;]+)`)

// framingWant is what the framing case's wrapper must carry: the sent
// message's attributes, in the order the evidence names them.
//
// sender is the relation the bus sets: "own" when the recipient's person
// is the sender's (internal/devicebus/local.go; internal/bus/bus.go for a
// server). The probe's sessions are always one person's, in --local mode
// and against the running agent alike; a probe between two people would
// want "teammate".
type framingWant struct {
	id, from, agent, sender, intent, marker, body string
}

// verdictFraming: the <flopwire-message> wrapper reached the model intact.
// The recipient's transcript holds it in hook context, closed, with the
// sent message's id, sender session, harness, sender relation and intent,
// and the marker in its text. The model's quote of those attributes is
// corroboration only.
func verdictFraming(entries []tapEntry, session string, d delivery, reply string, want framingWant) verdict {
	var v verdict
	v.promptSubmitPrinted(entries, session, want.id)
	hookFails := len(v.fails)
	switch {
	case d.err != nil:
		v.fail("cannot read the transcript %s: %v", d.where, d.err)
	case d.w == nil:
		v.fail("no hook context in the transcript %s holds %s's wrapper", d.where, want.id)
	default:
		for _, kv := range [][2]string{{"id", want.id}, {"from", want.from}, {"agent", want.agent}, {"sender", want.sender}, {"intent", want.intent}} {
			if got, ok := d.w.attrs[kv[0]]; !ok || got != kv[1] {
				v.fail("wrapper %s=%q, want %q", kv[0], got, kv[1])
			}
		}
		if err := d.w.whole(want.id); err != nil {
			v.fail("%v", err)
		}
		if !strings.Contains(d.w.body, want.marker) {
			v.fail("the wrapper of %s does not hold %s", want.id, want.marker)
		} else if got := strings.TrimSpace(d.w.body); got != want.body {
			v.fail("the wrapper of %s holds %q, want the sent text %q", want.id, clip(got, 120), want.body)
		}
		if len(v.fails) == hookFails {
			v.fact("transcript hook context holds the whole wrapper: id, from=%s, agent=%s, sender=%s, intent=%s and the marker", clip(want.from, 8), want.agent, want.sender, want.intent)
		}
	}
	v.fact("%s", framingQuote(reply, want))
	return v
}

// framingQuote describes the model's quote of the wrapper: corroboration,
// never a reason to fail.
func framingQuote(reply string, want framingWant) string {
	line := ""
	for l := range strings.SplitSeq(reply, "\n") {
		if strings.Contains(l, want.id) {
			line = l
			break
		}
	}
	if line == "" {
		return fmt.Sprintf("model did not quote the message id (reply: %q)", clip(oneLine(reply), 80))
	}
	got := map[string]string{}
	for _, m := range framingAttr.FindAllStringSubmatch(line, -1) {
		got[strings.ToLower(m[1])] = strings.Trim(m[2], "`.")
	}
	var off []string
	for _, kv := range [][2]string{{"id", want.id}, {"from", want.from}, {"intent", want.intent}, {"marker", want.marker}} {
		if got[kv[0]] != kv[1] {
			off = append(off, fmt.Sprintf("%s=%q", kv[0], got[kv[0]]))
		}
	}
	if len(off) > 0 {
		return "model's quote differs: " + strings.Join(off, ", ")
	}
	return "model quoted id, from, intent and the marker"
}

// verdictGuardian: a message queued during a Codex auto-review pass is
// not taken by a hook during the pass (the reviewer runs in its own
// ephemeral thread), and reaches the session once (verdict.reached).
func verdictGuardian(entries []tapEntry, session, id, marker, reply string, d delivery, start, end int64) verdict {
	var v verdict
	if start == 0 {
		v.fail("no auto-review pass ran (the model did not ask for an escalation)")
		v.reached(d, id, marker, reply)
		return v
	}
	var during []tapEntry
	for _, e := range entries {
		if e.At >= start && e.At <= end {
			during = append(during, e)
		}
	}
	v.fact("review %s; hooks during it: %s", time.Duration(end-start)*time.Millisecond, describeAll(during))
	ps := printers(entries, id)
	switch {
	case len(ps) == 0:
		v.fail("no hook printed %s", id)
	case len(ps) > 1:
		v.fail("%s printed %d times (%s)", id, len(ps), describeAll(ps))
	case ps[0].At >= start && ps[0].At <= end:
		v.fail("%s printed during the review by %s (session %s, transcript %s)", id, describe(ps[0]), ps[0].Session, ps[0].Transcript)
	case ps[0].Session != session || ps[0].AgentID != "":
		v.fail("%s printed by %s of %s, not the session's own hook", id, describe(ps[0]), ps[0].Session)
	default:
		v.fact("the session's %s printed %s after the review", describe(ps[0]), id)
	}
	v.reached(d, id, marker, reply)
	return v
}
