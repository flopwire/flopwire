package digest

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/flopwire/flopwire/internal/transcript"
)

// EvidenceRef identifies a retained message. Ordinal orders records; it is not an identity.
// The caller supplies the source generation and session scope.
type EvidenceRef struct {
	NativeID   string `json:"native_id,omitempty"`
	Ordinal    int64  `json:"ordinal"`
	Part       int    `json:"part"`
	LineNo     int64  `json:"line_no,omitempty"`
	ByteOffset int64  `json:"byte_offset,omitempty"`
	ByteLen    int64  `json:"byte_len,omitempty"`
	Locator    string `json:"locator,omitempty"`
}

// CommitEvidence is an observed commit, with the records supporting that observation.
// Hash may be abbreviated. DirectoryKnown describes captured directory placement,
// never repository identity. Outcome describes the whole execution, not commit creation.
type CommitEvidence struct {
	Hash             string       `json:"hash"`
	Operation        string       `json:"operation"`
	Command          EvidenceRef  `json:"command"`
	Result           EvidenceRef  `json:"result"`
	RevealCommand    *EvidenceRef `json:"reveal_command,omitempty"`
	RevealResult     *EvidenceRef `json:"reveal_result,omitempty"`
	CommandDirectory string       `json:"command_directory,omitempty"`
	DirectoryKnown   bool         `json:"directory_known"`
	Outcome          string       `json:"outcome"`
}

func evidenceRef(m transcript.Message) EvidenceRef {
	return EvidenceRef{m.NativeID, m.Ordinal, m.Part, m.LineNo, m.ByteOffset, m.ByteLen, m.Locator}
}

// ExtractCommitEvidence replays retained messages from one source generation and
// session. Re-emitted rows must already have been replaced by the storage adapter.
// It deliberately has no summary cap and never pairs by text containing a call ID.
func ExtractCommitEvidence(messages []transcript.Message) []CommitEvidence {
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

// Execution handles are accepted only from harness result envelopes and typed
// poll arguments. An occurrence in arbitrary output cannot establish ownership.
var runningSession = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?m)^Process running with session ID ([0-9]+)$`) })

func runningHandle(text string) string {
	if strings.HasPrefix(text, "Chunk ID: ") {
		if head, _, ok := strings.Cut(text, "\nOutput:"); ok {
			if m := runningSession().FindStringSubmatch(head); m != nil {
				return "session:" + m[1]
			}
		}
	}
	var v struct {
		SessionID *int64  `json:"session_id"`
		ExitCode  *int    `json:"exit_code"`
		Output    *string `json:"output"`
	}
	if json.Unmarshal([]byte(text), &v) == nil && v.SessionID != nil && v.ExitCode == nil && v.Output != nil {
		return fmt.Sprintf("session:%d", *v.SessionID)
	}
	return ""
}
func pollHandle(m transcript.Message) string {
	if m.ToolName != "write_stdin" {
		return ""
	}
	var v struct {
		SessionID *int64 `json:"session_id"`
	}
	if json.Unmarshal([]byte(m.Text), &v) == nil && v.SessionID != nil {
		return fmt.Sprintf("session:%d", *v.SessionID)
	}
	return ""
}

var evidenceCommitOut = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?m)^\[(?:detached HEAD|[^\]\s]+)(?: \([^)\n]*\))? ([0-9a-f]{7,64})\] `)
})

// Repeated banners within one addressed output are one observation. Separate
// executions and distinct poll evidence retain their own provenance.
func printedEvidenceHashes(out string) []string {
	seen := map[string]bool{}
	var hashes []string
	for _, hit := range evidenceCommitOut().FindAllStringSubmatch(out, -1) {
		if !seen[hit[1]] {
			seen[hit[1]] = true
			hashes = append(hashes, hit[1])
		}
	}
	return hashes
}

func evidenceOperation(cmds []shellCmd) (string, string, bool) {
	op, cwd := "", ""
	known := false
	for _, c := range cmds {
		segs, ok := splitShell(c.line)
		if !ok {
			return "unknown", "", false
		}
		where := c.cwd
		for _, s := range segs {
			w := stripPrefix(s.words)
			if len(w) == 0 {
				continue
			}
			if w[0] == "cd" && len(w) > 1 {
				where = joinDir(where, w[1])
				continue
			}
			if w[0] != "git" && !strings.HasSuffix(w[0], "/git") {
				continue
			}
			dir, sub, _ := gitArgs(w[1:])
			at := joinDir(where, dir)
			if sub == "commit" || sub == "cherry-pick" || sub == "revert" {
				next := strings.ReplaceAll(sub, "-", "_")
				if op == "" {
					op = next
					cwd = at
					known = strings.HasPrefix(at, "/") && !strings.ContainsAny(at, "$~`")
				} else if op != next || cwd != at {
					return "unknown", "", false
				}
			}
		}
	}
	if op == "" {
		op = "unknown"
	}
	return op, cwd, known
}

// Other programs can print convincing Git output. Reject such compound calls.
func printedOutputSafe(m transcript.Message) bool {
	m.Enrichment = nil
	return printedCommandsSafe(shellCmds(&m))
}
func printedCommandsSafe(cmds []shellCmd) bool {
	for _, c := range cmds {
		segs, ok := splitShell(c.line)
		if !ok {
			return false
		}
		for _, s := range segs {
			w := stripPrefix(s.words)
			if len(w) == 0 {
				continue
			}
			if w[0] == "cd" {
				continue
			}
			if w[0] != "git" && !strings.HasSuffix(w[0], "/git") {
				if literalFilePrint(s) {
					continue
				}
				return false
			}
			_, sub, _ := gitArgs(w[1:])
			switch sub {
			case "commit", "cherry-pick", "revert", "add", "push":
			default:
				return false
			}
		}
	}
	return len(cmds) > 0
}

func unknownExecution(text string) bool {
	if strings.HasPrefix(text, "Chunk ID: ") {
		head, _, ok := strings.Cut(text, "\nOutput:")
		return ok && !strings.Contains(head, "\nProcess exited with code ")
	}
	var v map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &v) == nil {
		if code, ok := v["exit_code"]; ok && string(code) == "null" {
			return true
		}
	}
	return false
}

// Placement-changing wrappers and repository overrides need their own context
// model. Keep printed observations, but never infer HEAD continuity through them.
func placementUnsupported(cmds []shellCmd) bool {
	for _, c := range cmds {
		segs, ok := splitShell(c.line)
		if !ok {
			return true
		}
		for _, s := range segs {
			for i, w := range s.words {
				if strings.HasPrefix(w, "GIT_DIR=") || strings.HasPrefix(w, "GIT_WORK_TREE=") || strings.HasPrefix(w, "GIT_COMMON_DIR=") || w == "--git-dir" || strings.HasPrefix(w, "--git-dir=") || w == "--work-tree" || strings.HasPrefix(w, "--work-tree=") {
					return true
				}
				if w == "env" {
					for _, a := range s.words[i+1:] {
						if strings.HasPrefix(a, "-C") || strings.HasPrefix(a, "-S") || a == "--chdir" || a == "--split-string" || strings.HasPrefix(a, "--chdir=") || strings.HasPrefix(a, "--split-string=") {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

// Only literal file preparation can precede Git's printed output. Quoted or
// escaped greater-than characters are arguments, not redirections.
func literalFilePrint(s seg) bool {
	w := stripPrefix(s.words)
	if len(w) == 0 || (w[0] != "printf" && w[0] != "echo") || strings.ContainsAny(s.raw, "$`") || s.op == "|" || s.op == "&" {
		return false
	}
	if w[0] == "printf" && (len(w) < 2 || strings.HasPrefix(w[1], "-")) {
		return false
	}
	raw := s.raw
	redirect := -1
	for i := 0; i < len(raw); i++ {
		switch raw[i] {
		case '\\':
			i++
		case '\'', '"':
			quote := raw[i]
			for i++; i < len(raw) && raw[i] != quote; i++ {
				if quote == '"' && raw[i] == '\\' {
					i++
				}
			}
		case '<':
			return false
		case '>':
			redirect = i
			if i > 0 && raw[i-1] >= '0' && raw[i-1] <= '9' {
				return false
			}
			i = len(raw)
		}
	}
	if redirect < 0 {
		return false
	}
	if strings.HasPrefix(raw[redirect:], ">>>") {
		return false
	}
	target := strings.TrimSpace(strings.TrimLeft(raw[redirect:], ">"))
	if !strings.HasPrefix(target, "/") && slices.Contains(strings.Split(target, "/"), "..") {
		return false
	}
	target = path.Clean(target)
	if target == "" || strings.ContainsAny(target, "&<>\\\"' \t\r\n") || strings.HasPrefix(target, "/dev/") || strings.HasPrefix(target, "/proc/") || target == ".." || strings.HasPrefix(target, "../") {
		return false
	}
	return true
}

func revealCommandsOnly(cmds []shellCmd) bool {
	if len(cmds) != 1 {
		return false
	}
	segs, ok := splitShell(cmds[0].line)
	if !ok || len(segs) != 1 {
		return false
	}
	w := stripPrefix(segs[0].words)
	if len(w) < 2 || (w[0] != "git" && !strings.HasSuffix(w[0], "/git")) {
		return false
	}
	_, sub, args := gitArgs(w[1:])
	kind, _ := revealOf(sub, args)
	return kind != "" && sub != "push" && !strings.ContainsAny(cmds[0].line, "$`<>")
}
