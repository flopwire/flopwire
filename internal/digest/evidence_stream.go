package digest

import (
	"slices"
	"strings"

	"github.com/flopwire/flopwire/internal/transcript"
)

type pendingCommitEvidence struct {
	ref            EvidenceRef
	printedSafe    bool
	g              callGit
	operation, cwd string
	known          bool
	launchResult   *EvidenceRef
	poll           *EvidenceRef
	handle         string
	reveal         *CommitEvidence
}

// CommitEvidenceState incrementally extracts commit evidence. Its zero value is
// ready to use. It retains unresolved calls and handles, but no message text or
// previously emitted evidence. A state must not be used concurrently.
type CommitEvidenceState struct {
	calls                  map[string]pendingCommitEvidence
	handles                map[string]pendingCommitEvidence
	lastMover              int64
	open                   *CommitEvidence
	openWhere              string
	peakCalls, peakHandles int
}

// Drop unused parsed fields and clone strings to avoid retaining command text.
func compactEvidenceGit(g callGit) callGit {
	return callGit{
		printed: g.printed, commit: g.commit, reveal: strings.Clone(g.reveal), style: strings.Clone(g.style), self: g.self, where: strings.Clone(g.where),
	}
}

// Step consumes the next retained message and returns only newly observed evidence.
// Call in transcript order within one source generation and session. Outputs are
// owned by the caller and may be changed without affecting this state.
func (s *CommitEvidenceState) Step(message transcript.Message) []CommitEvidence {
	if s.calls == nil {
		s.calls = make(map[string]pendingCommitEvidence)
		s.handles = make(map[string]pendingCommitEvidence)
		s.lastMover = -1
	}
	calls, handles, lastMover, open, openWhere := s.calls, s.handles, s.lastMover, s.open, s.openWhere
	defer func() { s.lastMover, s.open, s.openWhere = lastMover, open, openWhere }()
	var events []CommitEvidence
	for _, m := range []transcript.Message{message} {
		if m.Superseded {
			lastMover = m.Ordinal
			open = nil
			continue
		}
		if m.Kind == transcript.KindToolResult && m.Enrichment["command_completion"] == true {
			cmds := shellCmds(&m)
			g, ok := readCall(cmds)
			uncertainPlacement := placementUnsupported(cmds)
			if uncertainPlacement {
				g.commit = okNone
				g.reveal = ""
			}
			if !ok || g.mover {
				open = nil
				lastMover = m.Ordinal
			}
			if ok {
				op, cwd, known := evidenceOperation(cmds)
				if uncertainPlacement {
					cwd = ""
					known = false
				}
				out, _ := shellOut(m.Text)
				outcome := "unknown"
				success := len(cmds) == 1 && cmds[0].exit != nil && *cmds[0].exit == 0 && !m.IsError
				if len(cmds) == 1 && cmds[0].exit != nil {
					outcome = "succeeded"
					if *cmds[0].exit != 0 || m.IsError {
						outcome = "failed"
					}
				}
				base := CommitEvidence{Operation: strings.Clone(op), Command: compactEvidenceRef(m), Result: compactEvidenceRef(m), CommandDirectory: strings.Clone(cwd), DirectoryKnown: known, Outcome: outcome}
				printed := false
				if g.printed && printedCommandsSafe(cmds) {
					for _, hash := range printedEvidenceHashes(out) {
						e := base
						e.Hash = hash
						events = append(events, e)
						printed = true
					}
				}
				if !printed && success && !g.self && g.reveal != "" && g.revOK == okExit && open != nil && g.revWhere == openWhere {
					if hash := revealed(g.reveal, g.style, out, ""); hash != "" {
						e := *open
						e.Hash = hash
						ref := compactEvidenceRef(m)
						e.RevealCommand = &ref
						e.RevealResult = &ref
						events = append(events, e)
						open = nil
						printed = true
					}
				}
				if !printed && success && g.commit == okExit && g.self && g.reveal != "" {
					if hash := revealed(g.reveal, g.style, out, ""); hash != "" {
						e := base
						e.Hash = hash
						events = append(events, e)
						printed = true
					}
				}
				if !printed && success && g.commit == okExit && g.reveal == "" {
					copy := base
					open = &copy
					openWhere = strings.Clone(g.where)
				}
			}

			continue
		}
		if m.Kind == transcript.KindToolCall {
			if m.ToolCallID != "" {
				if h := pollHandle(m); h != "" {
					if p, ok := handles[h]; ok {
						ref := compactEvidenceRef(m)
						p.poll = &ref
						calls[strings.Clone(m.ToolCallID)] = p
					}
					continue
				}
			}
			if m.ToolCallID == "" || !IsShell(m.ToolName) {
				continue
			}
			// An arbitrary exec script can print text unrelated to its shell output.
			// Exact self-contained completion records cover its shell executions.
			if strings.Contains(m.Text, "exec_command(") {
				// A fully literal wrapper can establish a HEAD-move barrier without
				// supplying output evidence. Never trust heuristic event enrichment.
				cmds, literal := literalExecCommands(m.Text)
				g, parsed := readCall(cmds)
				if !literal || !parsed || !revealCommandsOnly(cmds) || placementUnsupported(cmds) || g.mover || slices.ContainsFunc(g.ends, func(e [2]string) bool { return e[0] != e[1] }) {
					open = nil
					lastMover = m.Ordinal
				}
				continue
			}
			// Commands enrichment is best effort, including heuristic Codex event joins.
			// Parse the addressed call itself to establish command ownership.
			bare := m
			bare.Enrichment = nil
			cmds := shellCmds(&bare)
			g, ok := readCall(cmds)
			if !ok || len(cmds) == 0 {
				lastMover = m.Ordinal
				open = nil
				continue
			}
			op, cwd, known := evidenceOperation(cmds)
			if placementUnsupported(cmds) {
				cwd = ""
				known = false
				g.commit = okNone
				g.reveal = ""
				g.mover = true
			}
			if g.mover || slices.ContainsFunc(g.ends, func(e [2]string) bool { return e[0] != e[1] }) {
				open = nil
				lastMover = m.Ordinal
			}
			p := pendingCommitEvidence{ref: compactEvidenceRef(m), printedSafe: printedOutputSafe(m), g: compactEvidenceGit(g), operation: strings.Clone(op), cwd: strings.Clone(cwd), known: known}
			if !g.self && g.reveal != "" && open != nil && g.revWhere == openWhere {
				copy := *open
				p.reveal = &copy
			}
			calls[strings.Clone(m.ToolCallID)] = p
			continue
		}
		if m.Kind != transcript.KindToolResult || m.ToolCallID == "" {
			continue
		}
		p, ok := calls[m.ToolCallID]
		if !ok {
			continue
		}
		delete(calls, m.ToolCallID)
		if h := runningHandle(m.Text); h != "" {
			ref := compactEvidenceRef(m)
			if p.launchResult == nil {
				p.launchResult = &ref
			}
			p.handle = strings.Clone(h)
			handles[p.handle] = p
			continue
		}
		if p.handle != "" {
			delete(handles, p.handle)
		}
		out, failed := shellOut(m.Text)
		success := !failed && !m.IsError && !unknownExecution(m.Text)
		outcome := "succeeded"
		if unknownExecution(m.Text) {
			outcome = "unknown"
		}
		if failed || m.IsError {
			outcome = "failed"
		}
		base := CommitEvidence{Operation: p.operation, Command: p.ref, Result: compactEvidenceRef(m), CommandDirectory: p.cwd, DirectoryKnown: p.known, Outcome: outcome}
		if p.launchResult != nil {
			base.Result = *p.launchResult
			base.RevealCommand = p.poll
			ref := compactEvidenceRef(m)
			base.RevealResult = &ref
		}
		printed := false
		if p.g.printed && p.printedSafe {
			for _, hash := range printedEvidenceHashes(out) {
				e := base
				e.Hash = hash
				events = append(events, e)
				printed = true
			}
		}
		if p.g.reveal != "" && success {
			hash := revealed(p.g.reveal, p.g.style, out, "")
			if hash != "" {
				if p.g.self && p.g.commit != okNone {
					e := base
					e.Hash = hash
					if !printed {
						events = append(events, e)
					}
					open = nil
				} else if p.reveal != nil && lastMover < p.ref.Ordinal {
					e := *p.reveal
					e.Hash = hash
					cr, rr := p.ref, compactEvidenceRef(m)
					e.RevealCommand = &cr
					e.RevealResult = &rr
					events = append(events, e)
					open = nil
				}
			}
		}
		if p.g.commit != okNone && success && !printed && p.ref.Ordinal >= lastMover && !(p.g.self && p.g.reveal != "") {
			e := base
			open = &e
			openWhere = p.g.where
		}
	}
	s.peakCalls = max(s.peakCalls, len(calls))
	s.peakHandles = max(s.peakHandles, len(handles))
	for i := range events {
		events[i] = cloneCommitEvidence(events[i])
	}
	return events
}

func cloneCommitEvidence(e CommitEvidence) CommitEvidence {
	if e.RevealCommand != nil {
		ref := *e.RevealCommand
		e.RevealCommand = &ref
	}
	if e.RevealResult != nil {
		ref := *e.RevealResult
		e.RevealResult = &ref
	}
	return e
}

// Clone strings because parser tokens and source locators can refer to larger
// input buffers. Pending state must retain only the text it actually needs.
func compactEvidenceRef(m transcript.Message) EvidenceRef {
	ref := evidenceRef(m)
	ref.NativeID = strings.Clone(ref.NativeID)
	ref.Locator = strings.Clone(ref.Locator)
	return ref
}

// RetainedSize estimates retained bytes, conservatively counting shared values
// more than once. It is a cache budgeting aid, not a measurement of process RSS.
// It walks unresolved calls and handles; check once per request.
// Map overhead uses their peak entry counts because deleting does not shrink a
// Go map. Map overhead includes a conservative allowance for growth.
func (s *CommitEvidenceState) RetainedSize() int64 {
	size := int64(512) + int64(s.peakCalls+s.peakHandles)*2048
	for key, p := range s.calls {
		size += int64(len(key)) + pendingEvidenceSize(p)
	}
	for key, p := range s.handles {
		size += int64(len(key)) + pendingEvidenceSize(p)
	}
	size += int64(len(s.openWhere))
	if s.open != nil {
		size += commitEvidenceSize(*s.open)
	}
	return size
}

func evidenceRefSize(ref EvidenceRef) int64 {
	return 128 + int64(len(ref.NativeID)+len(ref.Locator))
}

func commitEvidenceSize(e CommitEvidence) int64 {
	size := int64(256+len(e.Hash)+len(e.Operation)+len(e.CommandDirectory)+len(e.Outcome)) + evidenceRefSize(e.Command) + evidenceRefSize(e.Result)
	if e.RevealCommand != nil {
		size += evidenceRefSize(*e.RevealCommand)
	}
	if e.RevealResult != nil {
		size += evidenceRefSize(*e.RevealResult)
	}
	return size
}

func pendingEvidenceSize(p pendingCommitEvidence) int64 {
	size := int64(512+len(p.g.reveal)+len(p.g.style)+len(p.g.where)+len(p.operation)+len(p.cwd)+len(p.handle)) + evidenceRefSize(p.ref)
	if p.launchResult != nil {
		size += evidenceRefSize(*p.launchResult)
	}
	if p.poll != nil {
		size += evidenceRefSize(*p.poll)
	}
	if p.reveal != nil {
		size += commitEvidenceSize(*p.reveal)
	}
	return size
}
