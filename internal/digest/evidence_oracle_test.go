package digest

import (
	"github.com/flopwire/flopwire/internal/transcript"
	"slices"
	"strings"
)

// Frozen pre-streaming extractor. Keep independent during qualification.
func frozenExtractCommitEvidence(messages []transcript.Message) []CommitEvidence {
	type pending struct {
		m              transcript.Message
		g              callGit
		operation, cwd string
		known          bool
		launchResult   *EvidenceRef
		poll           *EvidenceRef
		handle         string
		reveal         *CommitEvidence
	}
	calls := map[string]pending{}
	handles := map[string]pending{}
	lastMover := int64(-1)
	var events []CommitEvidence
	var open *CommitEvidence
	var openWhere string
	for _, m := range messages {
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
				base := CommitEvidence{Operation: op, Command: evidenceRef(m), Result: evidenceRef(m), CommandDirectory: cwd, DirectoryKnown: known, Outcome: outcome}
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
						ref := evidenceRef(m)
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
					openWhere = g.where
				}
			}

			continue
		}
		if m.Kind == transcript.KindToolCall {
			if m.ToolCallID != "" {
				if h := pollHandle(m); h != "" {
					if p, ok := handles[h]; ok {
						ref := evidenceRef(m)
						p.poll = &ref
						calls[m.ToolCallID] = p
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
			p := pending{m: m, g: g, operation: op, cwd: cwd, known: known}
			if !g.self && g.reveal != "" && open != nil && g.revWhere == openWhere {
				copy := *open
				p.reveal = &copy
			}
			calls[m.ToolCallID] = p
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
			ref := evidenceRef(m)
			if p.launchResult == nil {
				p.launchResult = &ref
			}
			p.handle = h
			handles[h] = p
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
		base := CommitEvidence{Operation: p.operation, Command: evidenceRef(p.m), Result: evidenceRef(m), CommandDirectory: p.cwd, DirectoryKnown: p.known, Outcome: outcome}
		if p.launchResult != nil {
			base.Result = *p.launchResult
			base.RevealCommand = p.poll
			ref := evidenceRef(m)
			base.RevealResult = &ref
		}
		printed := false
		if p.g.printed && printedOutputSafe(p.m) {
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
				} else if p.reveal != nil && lastMover < p.m.Ordinal {
					e := *p.reveal
					e.Hash = hash
					cr, rr := evidenceRef(p.m), evidenceRef(m)
					e.RevealCommand = &cr
					e.RevealResult = &rr
					events = append(events, e)
					open = nil
				}
			}
		}
		if p.g.commit != okNone && success && !printed && p.m.Ordinal >= lastMover && !(p.g.self && p.g.reveal != "") {
			e := base
			open = &e
			openWhere = p.g.where
		}
	}
	return events
}
