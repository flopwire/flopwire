package main

// `flopwire probe tap` is the hook command of a probe's scratch project
// (probe.go). For the events the Flopwire plugins hook (probeHookEvents) it
// runs the real `flopwire hook` in process, prints what it printed, and
// logs one line: the event, the ids the hook input carries, and the
// message ids it printed. For the other events it only logs: they mark
// the time a tool or a subagent started, which the verdicts need.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
)

// probeHookEvents are the events the Flopwire plugins hook (plugins/*/
// hooks/hooks.json); the tap runs the real hook for them only.
var probeHookEvents = []string{evSessionStart, evUserPromptSubmit, evPostToolUse, "Stop", evSessionEnd}

// probeObserveEvents are hooked only to log when they fire (Claude Code,
// Codex, Devin).
var probeObserveEvents = []string{"PreToolUse", "SubagentStart", "SubagentStop"}

// tapEntry is one line of a tap log.
type tapEntry struct {
	At              int64    `json:"at"`   // unix ms, the hook started
	Done            int64    `json:"done"` // unix ms, its output was written
	Event           string   `json:"event"`
	Session         string   `json:"session,omitempty"`
	AgentID         string   `json:"agent_id,omitempty"`
	AgentType       string   `json:"agent_type,omitempty"`
	Tool            string   `json:"tool,omitempty"`
	ToolUseID       string   `json:"tool_use_id,omitempty"`
	Transcript      string   `json:"transcript,omitempty"`
	AgentTranscript string   `json:"agent_transcript,omitempty"`
	Printed         []string `json:"printed,omitempty"` // message ids in the additionalContext
	Instruction     bool     `json:"instruction,omitempty"`
	// Binary is the flopwire that ran the hook and Via how the plugin's
	// shim found it (FLOPWIRE_HOOK_VIA: recorded, path or known); both are
	// set only under probe --as-installed, where the plugin's own hook
	// command runs `flopwire hook` and the hook taps itself.
	Binary string `json:"binary,omitempty"`
	Via    string `json:"via,omitempty"`
}

// tapInput is the part of a hook's input the tap logs.
type tapInput struct {
	hookInput
	AgentType string `json:"agent_type"`
	ToolName  string `json:"tool_name"`
}

var wrapperID = regexp.MustCompile(`<flopwire-message id="([^"]+)"`)

// printedIDs reads a hook's stdout (Claude-format hook JSON) for the
// message ids it delivered and whether it carried the standing
// instruction.
func printedIDs(out []byte) (ids []string, instruction bool) {
	var o hookOutput
	if len(bytes.TrimSpace(out)) == 0 || json.Unmarshal(out, &o) != nil {
		return nil, false
	}
	text := o.HookSpecificOutput.AdditionalContext
	if text == "" {
		// The opencode plugin's form (hook_opencode.go): the instruction
		// goes into its system prompt, not the text.
		var oc opencodeOutput
		if json.Unmarshal(out, &oc) == nil {
			for _, m := range oc.Messages {
				text += m.Text + "\n"
			}
			instruction = oc.Instruction
		}
	}
	for _, m := range wrapperID.FindAllStringSubmatch(text, -1) {
		ids = append(ids, m[1])
	}
	return ids, instruction || strings.Contains(text, "<flopwire-instructions>")
}

// envProbeTap, set by probe --as-installed in a harness's environment,
// makes `flopwire hook` run as the tap with this log, so the plugin's own
// hook command (shim, shell and all) is what the probe exercises.
const envProbeTap = "FLOPWIRE_PROBE_TAP"

// envHookVia is how the plugin's shim found the binary it ran.
const envHookVia = "FLOPWIRE_HOOK_VIA"

func probeTap(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	signal.Ignore(syscall.SIGPIPE)
	fs := flag.NewFlagSet("probe tap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	logPath := fs.String("log", "", "tap log (JSON lines)")
	socket := fs.String("socket", "", "device agent control socket")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *logPath == "" || *socket == "" {
		return errors.New("probe tap: --log and --socket are required")
	}
	start := time.Now()
	raw, _ := io.ReadAll(io.LimitReader(stdin, hookInputMax+1))
	var in tapInput
	_ = json.Unmarshal(raw, &in)
	e := tapEntry{At: start.UnixMilli(), Event: in.Event, Session: in.SessionID, AgentID: in.AgentID, AgentType: in.AgentType,
		Tool: in.ToolName, ToolUseID: in.ToolUseID, Transcript: in.TranscriptPath, AgentTranscript: in.AgentTranscriptPath}
	if os.Getenv(envProbeTap) != "" {
		e.Binary, _ = selfPath()
		e.Via = os.Getenv(envHookVia)
	}
	if in.Harness == "opencode" && in.Event == evHello {
		// The plugin loading: answered, never logged.
		return hookCmd(ctx, []string{"--socket", *socket}, bytes.NewReader(raw), stdout, stderr, os.Getenv)
	}
	// The opencode plugin runs this tap for every event it sends; each
	// must reach the hook (PreToolUse marks the turn busy, Confirm
	// confirms a delivery).
	if slices.Contains(probeHookEvents, in.Event) || in.Harness == "opencode" {
		var out bytes.Buffer
		_ = hookCmd(ctx, []string{"--socket", *socket}, bytes.NewReader(raw), &out, stderr, os.Getenv)
		_, _ = stdout.Write(out.Bytes())
		e.Printed, e.Instruction = printedIDs(out.Bytes())
	}
	e.Done = time.Now().UnixMilli()
	return appendTap(*logPath, e)
}

// appendTap appends one entry; a line under PIPE_BUF is written whole
// even when hooks run at once.
func appendTap(path string, e tapEntry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(b, '\n'))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// readTap reads a tap log; a torn or foreign line is skipped.
func readTap(path string) ([]tapEntry, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseTap(b), nil
}

func parseTap(b []byte) []tapEntry {
	var out []tapEntry
	for line := range bytes.SplitSeq(b, []byte{'\n'}) {
		var e tapEntry
		if len(bytes.TrimSpace(line)) == 0 || json.Unmarshal(line, &e) != nil || e.Event == "" {
			continue
		}
		out = append(out, e)
	}
	return out
}
