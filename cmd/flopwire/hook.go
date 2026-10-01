package main

// `flopwire hook` is the one command harness hooks run (Claude Code, Codex,
// Devin CLI). It reads the hook's JSON on stdin and, by hook_event_name:
//
//	SessionStart                   the standing instruction, plus pending messages
//	UserPromptSubmit, PostToolUse  pending messages
//	anything else (Stop, …)        nothing printed
//
// On every event it also asks the device agent to index and upload this
// transcript now (the `agent flush` request), without waiting for it.
// Messages print as Claude-format hook JSON, which all three harnesses
// take for these events:
//
//	{"hookSpecificOutput":{"hookEventName":EVENT,"additionalContext":TEXT}}
//
// It never fails a harness turn: it exits 0 with nothing on stdout when
// there is nothing to deliver, the agent is not running or slower than
// hookPendingBudget, the input is not hook JSON, or anything goes wrong.
// Reasons go to stderr, and never the environment or a message body.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/busrender"
	"github.com/flopwire/flopwire/internal/transcript"
)

// Hook events.
const (
	evSessionStart     = "SessionStart"
	evUserPromptSubmit = "UserPromptSubmit"
	evPostToolUse      = "PostToolUse"
)

var (
	// hookPendingBudget bounds the pending request (dial, ask, answer). An
	// answer that arrives later is not read; the agent finds the closed
	// connection and queues the messages again (agent.Requeue).
	hookPendingBudget = 200 * time.Millisecond
	// hookFlushBudget bounds handing the flush request to the agent. The
	// hook does not wait for the flush itself: the agent finishes it after
	// the hook exits.
	hookFlushBudget = 100 * time.Millisecond
)

// hookInputMax bounds the hook input read. PostToolUse carries the tool's
// output, which can be large; the fields the hook needs come first in
// every harness's input but the JSON must parse whole.
const hookInputMax = 64 << 20

// hookInput is the part of a hook's stdin the hook uses.
type hookInput struct {
	Event          string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Source         string `json:"source"`  // SessionStart: startup, resume, clear, compact
	TurnID         string `json:"turn_id"` // Codex
}

// hookOutput is the Claude-format hook JSON.
type hookOutput struct {
	HookSpecificOutput hookSpecific `json:"hookSpecificOutput"`
}

type hookSpecific struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

func hookMain(ctx context.Context, args []string) error {
	return hookCmd(ctx, args, os.Stdin, os.Stdout, os.Stderr, os.Getenv)
}

// hookCmd runs `flopwire hook`. It always returns nil.
func hookCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) (err error) {
	warn := func(format string, a ...any) { fmt.Fprintf(stderr, "flopwire hook: "+format+"\n", a...) }
	defer func() {
		if r := recover(); r != nil {
			warn("internal error; nothing delivered")
		}
		err = nil
	}()
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	socket := fs.String("socket", "", "control socket (default <config dir>/agent.sock)")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		warn("usage: flopwire hook [--socket PATH] < hook JSON")
		return nil
	}
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		warn("expects hook JSON on stdin (it is run by a harness hook)")
		return nil
	}
	raw, rerr := io.ReadAll(io.LimitReader(stdin, hookInputMax+1))
	var in hookInput
	if rerr != nil || len(raw) > hookInputMax || json.Unmarshal(raw, &in) != nil {
		warn("the input is not hook JSON; nothing delivered")
		return nil
	}
	if *socket == "" {
		dir, err := configDir()
		if err != nil {
			warn("no config directory: %v", err)
			return nil
		}
		*socket = filepath.Join(dir, "agent.sock")
	}

	harness := hookHarness(in, getenv)
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		hookFlush(ctx, *socket, in, harness)
	}()
	defer func() { <-flushed }()

	start := ""
	switch in.Event {
	case evSessionStart:
		start = in.Source
		if start == "" {
			start = "startup"
		}
	case evUserPromptSubmit, evPostToolUse:
	default:
		return nil // Stop, PreToolUse (Devin does not show its context), …: flush only
	}
	if in.SessionID == "" {
		warn("the input names no session_id; nothing delivered")
		return nil
	}
	pctx, cancel := context.WithTimeout(ctx, hookPendingBudget)
	defer cancel()
	resp, err := agent.Call(pctx, *socket, agent.Request{Op: "pending", Session: in.SessionID,
		Limit: busrender.HookMessages, MaxBytes: busrender.HookBytes, Start: start})
	if err != nil {
		warn("%s; nothing delivered", hookReason(err))
		return nil
	}
	// resp.Held (senders waiting for the user's acceptance) is for the
	// user-visible notice of the accept and revoke work (#61); it is not
	// model context, so nothing here prints it.
	text := busrender.Context(resp.Instruct, resp.Messages, resp.Excerpts, busrender.HookBytes)
	if text == "" {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(hookOutput{hookSpecific{HookEventName: in.Event, AdditionalContext: text}}) != nil {
		warn("could not encode the output")
		return nil
	}
	if _, err := stdout.Write(buf.Bytes()); err != nil {
		warn("could not write the output: %d messages were marked delivered", len(resp.Messages))
	}
	return nil
}

// hookReason is a pending failure as a short reason for stderr. Agent
// errors name the socket and the cause, never a message.
func hookReason(err error) string {
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "agent not running"):
		return "the device agent is not running"
	case msg == "messaging is off in this agent":
		return "messaging is off in the device agent"
	case strings.HasPrefix(msg, "unknown op "):
		return "the device agent predates messaging; restart it"
	case isTimeout(err):
		return fmt.Sprintf("the device agent did not answer within %s", hookPendingBudget)
	}
	return "the device agent refused: " + firstLine(msg, 200)
}

func isTimeout(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return strings.Contains(err.Error(), "deadline exceeded") || strings.Contains(err.Error(), "i/o timeout")
}

func firstLine(s string, n int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > n {
		s = s[:n]
	}
	return s
}

// hookFlush hands the agent the flush request for this transcript and
// leaves: the agent indexes and uploads it whether or not anyone reads its
// answer, and the hook must not wait on indexing. A Devin session has no
// transcript file; the agent finds it by session id.
func hookFlush(ctx context.Context, socket string, in hookInput, harness transcript.Agent) {
	req := agent.Request{Op: "flush", Path: in.TranscriptPath, Session: in.SessionID}
	if harness == transcript.AgentDevin {
		req.Path = ""
	}
	if req.Path == "" && req.Session == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, hookFlushBudget)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return // the pending request reports a stopped agent
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	b, _ := json.Marshal(req)
	c.Write(append(b, '\n'))
}

var codexRollout = regexp.MustCompile(`(^|/)rollout-[^/]*\.jsonl$`)

// hookHarness says which harness ran the hook, from its input and, where
// the input does not tell, the variables the harness sets for hooks. Only
// variable names are checked; no value is read or printed. Devin runs
// hooks from .claude/settings.json too, with the same input as its own
// hooks, so Devin is recognised by its variables first. Delivery does not
// depend on it: messages are taken by session id, so whichever config's
// hook asks first gets each message, once.
func hookHarness(in hookInput, getenv func(string) string) transcript.Agent {
	switch {
	case getenv("DEVIN_PROJECT_DIR") != "" || getenv("CHISEL_SESSION_DB") != "":
		return transcript.AgentDevin
	case codexRollout.MatchString(in.TranscriptPath) || in.TurnID != "" || (in.SessionID != "" && getenv("CODEX_THREAD_ID") == in.SessionID):
		return transcript.AgentCodex
	case in.TranscriptPath != "" && strings.HasSuffix(in.TranscriptPath, in.SessionID+".jsonl"), getenv("CLAUDECODE") != "":
		return transcript.AgentClaude
	case in.TranscriptPath == "" && in.SessionID != "":
		return transcript.AgentDevin // Devin's input carries no transcript path
	}
	return ""
}
