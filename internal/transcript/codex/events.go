// codex-events@1: best-effort enrichment from event_msg/item_completed
// CommandExecution and FileChange records (spec §5.4, decisions item 9).
//
// Codex 0.15x writes one CommandExecution per shell command a tool call ran
// and one FileChange per patch it applied, between the call's response_item
// and its output. This sub-parser pairs each event with the call that is
// open in the same turn (called, output not yet written) and attaches the
// command, exit code, duration and changed paths to that call's row. An
// event with no open call in its turn (a user shell command, or a
// background command that finished after its exec script returned) becomes
// its own tool_result row. Event output text is never indexed: it is
// already in the call's output record.
//
// The pairing is Flopwire's own. apply_patch path extraction follows
// kenn-io/agentsview internal/parser/codex.go extractPatchedFiles at commit
// 563023de1d7b7f5af50ad5967c101341a44a2bfc (MIT, see third_party/agentsview)
// and entireio/cli cmd/entire/cli/agent/codex/transcript.go at commit
// 2dbea8bff1db7188158420b1c863dfa2d9cd7b4a (MIT, see third_party/entire).
//
// Nothing here can fail a parse: a record that does not decode is counted
// and dropped, and the rows from response_item are unaffected.

package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// EventsVersion names this sub-parser; it is recorded on the rows it
// creates and on the call rows it enriches.
const EventsVersion = "codex-events@1.0"

// Command is one shell command a tool call ran, from a CommandExecution
// event. It is the element type of Message.Enrichment["commands"].
type Command struct {
	Cmd        string `json:"cmd"`
	Cwd        string `json:"cwd,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	DurationMS *int64 `json:"duration_ms,omitempty"`
}

// eventItem is item_completed.item, reduced to the enrichment fields.
type eventItem struct {
	Type     string                `json:"type"`
	ID       string                `json:"id"`
	Command  json.RawMessage       `json:"command"`
	Cwd      string                `json:"cwd"`
	ExitCode *int                  `json:"exit_code"`
	Duration json.RawMessage       `json:"duration"`
	Status   string                `json:"status"`
	Changes  map[string]fileChange `json:"changes"`
}

type fileChange struct {
	Type     string  `json:"type"`
	MovePath *string `json:"move_path"`
}

// event is one decoded enrichment record.
type event struct {
	ID      string
	Kind    string // CommandExecution or FileChange
	Command *Command
	Paths   []string
	Failed  bool
}

var errNotEnrichment = errors.New("codex-events: not an enrichment item")

// isEnrichmentType reports whether an item_completed item type is read by
// codex-events@1. Reasoning, AgentMessage and UserMessage mirror
// response_item records and are skipped, as are UI-only items.
func isEnrichmentType(t string) bool { return t == "CommandExecution" || t == "FileChange" }

// parseEvent decodes an item. It never panics outward.
func parseEvent(it *eventItem) (ev *event, err error) {
	defer func() {
		if r := recover(); r != nil {
			ev, err = nil, fmt.Errorf("codex-events: %v", r)
		}
	}()
	if it == nil || !isEnrichmentType(it.Type) {
		return nil, errNotEnrichment
	}
	ev = &event{ID: it.ID, Kind: it.Type}
	switch it.Type {
	case "CommandExecution":
		c := &Command{Cmd: commandString(it.Command), Cwd: cwdPath(it.Cwd), ExitCode: it.ExitCode}
		if ms, ok := durationMS(it.Duration); ok {
			c.DurationMS = &ms
		}
		ev.Command = c
		ev.Failed = it.ExitCode != nil && *it.ExitCode != 0
	case "FileChange":
		for p, ch := range it.Changes {
			ev.Paths = append(ev.Paths, p)
			if ch.MovePath != nil && *ch.MovePath != "" && *ch.MovePath != "None" {
				ev.Paths = append(ev.Paths, *ch.MovePath)
			}
		}
		sort.Strings(ev.Paths)
		ev.Failed = it.Status == "failed"
	}
	return ev, nil
}

// commandString renders argv. A shell wrapper ("/bin/zsh -lc <script>")
// yields the script.
func commandString(raw json.RawMessage) string {
	raw = trimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		return rawString(raw)
	}
	var argv []string
	if json.Unmarshal(raw, &argv) != nil {
		return ""
	}
	if len(argv) == 3 && (argv[1] == "-lc" || argv[1] == "-c") {
		return argv[2]
	}
	return strings.Join(argv, " ")
}

// cwdPath turns "file:///a%20b" into "/a b".
func cwdPath(s string) string {
	if !strings.HasPrefix(s, "file://") {
		return s
	}
	if u, err := url.Parse(s); err == nil && u.Path != "" {
		return u.Path
	}
	return strings.TrimPrefix(s, "file://")
}

// durationMS reads {"secs":N,"nanos":N} or a bare number of milliseconds.
func durationMS(raw json.RawMessage) (int64, bool) {
	raw = trimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	if raw[0] == '{' {
		var d struct {
			Secs  int64 `json:"secs"`
			Nanos int64 `json:"nanos"`
		}
		if json.Unmarshal(raw, &d) != nil {
			return 0, false
		}
		return d.Secs*1000 + d.Nanos/1e6, true
	}
	var f float64
	if json.Unmarshal(raw, &f) != nil {
		return 0, false
	}
	return int64(f), true
}

// openCall is a tool call whose output has not been written yet, or one
// recently closed, in cursor state. The state keeps where the call and its
// events are in the rollout, not their content: a re-emission reads them
// back (enrichedCall), so the state stays a few hundred bytes per call
// however large the call text or its commands are (P4).
type openCall struct {
	CallID string    `json:"c,omitempty"`
	Turn   string    `json:"t,omitempty"`
	Name   string    `json:"n,omitempty"` // tool name, for the output row
	Line   lineRef   `json:"l"`
	Events []lineRef `json:"ev,omitempty"` // item_completed records attached, in order
}

const (
	maxOpenCalls   = 256
	maxClosedCalls = 16
)

// commandInText reports whether a call's text names cmd: its first line,
// verbatim or with JSON/JS string escaping (exec scripts embed commands in
// string literals).
func commandInText(text, cmd string) bool {
	key := strings.TrimSpace(cmd)
	if i := strings.IndexByte(key, '\n'); i >= 0 {
		key = strings.TrimSpace(key[:i])
	}
	if len(key) > 60 {
		key = key[:60]
	}
	if len(key) < 4 {
		return false
	}
	if strings.Contains(text, key) {
		return true
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(key) != nil {
		return false
	}
	esc := strings.TrimSpace(b.String())
	return len(esc) > 2 && strings.Contains(text, esc[1:len(esc)-1])
}

// unpairedRow renders an event that no open call claimed.
func unpairedRow(ev *event) row {
	r := row{NativeID: ev.ID, Kind: kindToolResult, Parser: EventsVersion, Enriched: EventsVersion, IsError: ev.Failed}
	var text string
	switch ev.Kind {
	case "CommandExecution":
		r.ToolName = "shell"
		text = ev.Command.Cmd
		r.Commands = []Command{*ev.Command}
	case "FileChange":
		r.ToolName = "file_change"
		text = strings.Join(ev.Paths, "\n")
		r.Paths = append([]string(nil), ev.Paths...)
	}
	r.Text = text // caller binds the locator and applies the effective caps
	return r
}

// patchPaths lists the files an apply_patch body names, in patch order.
func patchPaths(patch string) []string {
	if !strings.Contains(patch, "*** Begin Patch") {
		return nil
	}
	var files []string
	for _, ln := range strings.Split(patch, "\n") {
		for _, prefix := range []string{"*** Add File: ", "*** Update File: ", "*** Delete File: ", "*** Move to: "} {
			if strings.HasPrefix(ln, prefix) {
				if f := strings.TrimSpace(strings.TrimPrefix(ln, prefix)); f != "" {
					files = appendUnique(files, f)
				}
				break
			}
		}
	}
	return files
}

// callPatchText finds a patch body in an apply_patch call: custom tool
// input, or a function_call's "input"/"patch" argument.
func callPatchText(name, text string) string {
	if name != "apply_patch" && !strings.Contains(text, "*** Begin Patch") {
		return ""
	}
	if strings.HasPrefix(strings.TrimSpace(text), "{") {
		var args map[string]any
		if json.Unmarshal([]byte(text), &args) == nil {
			for _, k := range []string{"input", "patch"} {
				if s, ok := args[k].(string); ok {
					return s
				}
			}
			// exec_command / shell with an apply_patch heredoc
			for _, k := range []string{"cmd", "command"} {
				switch v := args[k].(type) {
				case string:
					return v
				case []any:
					if len(v) > 0 {
						if s, ok := v[len(v)-1].(string); ok {
							return s
						}
					}
				}
			}
		}
	}
	return text
}

func appendUnique(dst []string, add ...string) []string {
	for _, a := range add {
		dup := false
		for _, d := range dst {
			if d == a {
				dup = true
				break
			}
		}
		if !dup {
			dst = append(dst, a)
		}
	}
	return dst
}
