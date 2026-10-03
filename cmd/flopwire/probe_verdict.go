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

// quoted checks that the model's reply quotes the marker.
func (v *verdict) quoted(reply, marker string) {
	if strings.Contains(reply, marker) {
		v.fact("model quoted %s", marker)
		return
	}
	v.fail("model did not quote %s (reply: %q)", marker, clip(oneLine(reply), 160))
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
// the session's UserPromptSubmit hook, once, and the model quotes it.
func verdictPromptSubmit(entries []tapEntry, session, id, marker, reply string) verdict {
	var v verdict
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
	v.quoted(reply, marker)
	return v
}

// verdictMidTurn: a message queued while a tool runs is printed by the
// session's next PostToolUse, before the turn's Stop, and the model quotes
// it.
func verdictMidTurn(entries []tapEntry, session, id, marker, reply string, sentAt int64) verdict {
	var v verdict
	ps := printers(entries, id)
	if len(ps) != 1 {
		v.fail("%s printed %d times (%s), want once", id, len(ps), describeAll(ps))
		v.quoted(reply, marker)
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
	v.quoted(reply, marker)
	return v
}

// subagentWindow is when a subagent ran, from the tap log: SubagentStart
// to SubagentStop (Claude Code, Codex), or the run_subagent call's
// PreToolUse to its PostToolUse (Devin, whose subagent hooks carry no
// agent id). Zero when none ran.
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
		isStart := e.Event == "SubagentStart" || (e.Event == "PreToolUse" && e.Tool == "run_subagent")
		isEnd := e.Event == "SubagentStop" || (e.Event == evPostToolUse && e.Tool == "run_subagent")
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
// the model quotes it. subSeen is whether the subagent's transcript holds
// the marker, nil when the harness has no transcript file to check.
func verdictSubagent(entries []tapEntry, session, id, marker, reply string, subSeen *bool, subPath string) verdict {
	var v verdict
	w, ok := findSubagent(entries, session)
	if !ok {
		v.fail("no subagent ran to completion (no SubagentStart/SubagentStop or run_subagent pair in the hooks)")
		v.quoted(reply, marker)
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
	case subSeen == nil:
		v.fact("no subagent transcript file to check")
	case *subSeen:
		v.fail("the subagent transcript %s holds %s", subPath, marker)
	default:
		v.fact("subagent transcript clean")
	}
	v.quoted(reply, marker)
	return v
}

// verdictIdle: a message sent to an idle session starts no turn within the
// window and stays queued.
func verdictIdle(during []tapEntry, harnessTurn bool, state, id string, window time.Duration) verdict {
	var v verdict
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

// framingLine finds the reply's line for the message: one holding the id
// attribute's value.
var framingAttr = regexp.MustCompile(`(?i)\b(id|from|intent|marker)\s*[=:]\s*["']?([^\s"',;]+)`)

// verdictFraming: the model can quote the wrapper's sender, intent and
// message id, not just the marker.
func verdictFraming(reply, id, from, intent, marker string) verdict {
	var v verdict
	line := ""
	for l := range strings.SplitSeq(reply, "\n") {
		if strings.Contains(l, id) {
			line = l
			break
		}
	}
	if line == "" {
		v.fail("model did not quote the message id %s (reply: %q)", id, clip(oneLine(reply), 200))
		return v
	}
	got := map[string]string{}
	for _, m := range framingAttr.FindAllStringSubmatch(line, -1) {
		got[strings.ToLower(m[1])] = strings.Trim(m[2], "`.")
	}
	want := map[string]string{"id": id, "from": from, "intent": intent, "marker": marker}
	for _, k := range []string{"id", "from", "intent", "marker"} {
		if got[k] != want[k] {
			v.fail("%s: quoted %q, want %q", k, got[k], want[k])
		}
	}
	if len(v.fails) == 0 {
		v.fact("model quoted id, from=%s, intent=%s and the marker", clip(from, 8), intent)
	}
	return v
}

// verdictGuardian: a message queued during a Codex auto-review pass is
// not taken by a hook during the pass (the reviewer runs in its own
// ephemeral thread), and reaches the session once.
func verdictGuardian(entries []tapEntry, session, id, marker, reply string, start, end int64) verdict {
	var v verdict
	if start == 0 {
		v.fail("no auto-review pass ran (the model did not ask for an escalation)")
		v.quoted(reply, marker)
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
	v.quoted(reply, marker)
	return v
}
