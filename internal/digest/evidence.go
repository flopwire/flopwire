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

// MayContainCommitEvidence reports whether stored result text could supply a
// hash to ExtractCommitEvidence. False proves that this text cannot supply one;
// true includes ordinary output and undecoded JSON chunks conservatively.
// Keep the JSON marker aligned with shellOut's decoding entry condition.
// This classification is persisted by the local index. Widening the extractor's
// hash or decoding grammar requires reclassifying stored false values through
// a schema migration. False positives are safe; false negatives lose evidence.
func MayContainCommitEvidence(text string) bool {
	if strings.Contains(text, `"exit_code":`) {
		return true
	}
	run := 0
	for _, c := range text {
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' {
			run++
			if run >= 7 {
				return true
			}
		} else {
			run = 0
		}
	}
	return false
}

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
	var state CommitEvidenceState
	var events []CommitEvidence
	for _, m := range messages {
		events = append(events, state.Step(m)...)
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
