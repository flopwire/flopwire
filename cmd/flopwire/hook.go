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
//
// Delivery has two steps. The pending request leases the messages to this
// hook; after the output is written, the hook confirms them (the confirm
// request). A hook that dies in between (a harness timeout, a kill, a
// broken stdout) confirms nothing, and the agent offers the messages again
// at the session's next hook, marked redelivery="true", once the lease
// ends (devicebus.LeaseFor). A hook older than hookLate takes nothing and
// confirms nothing: its harness may have stopped waiting for it.
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
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/busrender"
	"github.com/flopwire/flopwire/internal/retrieval/local"
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
	// hookConfirmBudget bounds the confirm request. A confirmation that
	// does not arrive leaves the lease to end: the messages are offered
	// again, marked.
	hookConfirmBudget = 100 * time.Millisecond
	// hookLate is the age (since the process was created) after which a
	// hook takes and confirms nothing. A harness that times a hook out
	// (5 s in the Flopwire plugins) may stop reading its output without
	// killing it: `flopwire hook || true` runs under a shell, and killing
	// the shell leaves the hook running. Its output then reaches no model,
	// so it must not confirm. A hook normally exits within tens of
	// milliseconds (docs/agent.md).
	hookLate = 3 * time.Second
	// hookStart is when this hook started: the process's creation, else
	// now. Tests replace it.
	hookStart = func() time.Time {
		if t := processStart(); !t.IsZero() {
			return t
		}
		return time.Now()
	}
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

// hookOutput is the Claude-format hook JSON. SystemMessage is shown to the
// person and not to the model, on Claude Code and Codex (see heldNotice).
type hookOutput struct {
	SystemMessage      string       `json:"systemMessage,omitempty"`
	HookSpecificOutput hookSpecific `json:"hookSpecificOutput,omitzero"`
}

type hookSpecific struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

func hookMain(ctx context.Context, args []string) error {
	// A write to a closed stdout (the harness stopped reading) returns
	// EPIPE instead of killing the process with SIGPIPE, so the hook still
	// exits 0, and it does not confirm what it could not print.
	signal.Ignore(syscall.SIGPIPE)
	return hookCmd(ctx, args, os.Stdin, os.Stdout, os.Stderr, os.Getenv)
}

// hookCmd runs `flopwire hook`. It always returns nil.
func hookCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) (err error) {
	warn := func(format string, a ...any) { fmt.Fprintf(stderr, "flopwire hook: "+format+"\n", a...) }
	started := hookStart()
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
	// A Devin hook whose session another devin process holds comes from a
	// `devin -r` that Devin refuses after its SessionStart hooks: nothing
	// it prints reaches a model, and its event says nothing about the
	// running session's turn. It delivers nothing and only flushes.
	elsewhere := harness == transcript.AgentDevin && devinHeldElsewhere(in.SessionID, getenv)
	// A hook that starts this late (its exec waited, see hookLate) carries
	// a stale event: a Stop may have come since. It delivers nothing, and
	// its flush carries no event, so it cannot mark the session busy.
	age := time.Since(started)
	late := age+hookPendingBudget > hookLate
	flushIn := in
	if elsewhere || late {
		flushIn.Event = ""
	}
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		hookFlush(ctx, *socket, flushIn, harness)
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
	if elsewhere {
		warn("another devin process holds this session; nothing delivered")
		return nil
	}
	if late {
		warn("started %s ago, too late to deliver; nothing delivered", age.Round(time.Millisecond))
		return nil
	}
	pctx, cancel := context.WithTimeout(ctx, hookPendingBudget)
	defer cancel()
	notice := in.Event == evUserPromptSubmit && noticeChannel(harness)
	resp, err := agent.Call(pctx, *socket, agent.Request{Op: "pending", Session: in.SessionID,
		Limit: busrender.HookMessages, MaxBytes: busrender.HookBytes, Start: start, Notice: notice})
	if err != nil {
		warn("%s; nothing delivered", hookReason(err))
		return nil
	}
	// resp.Held and resp.Notice (senders whose messages wait for the
	// person's acceptance) never go into model context: not their names,
	// not that they exist. The notice goes to the person alone.
	out := hookOutput{}
	if notice {
		out.SystemMessage = heldNotice(resp.Notice, resp.Console)
	}
	if text := busrender.Context(resp.Instruct, resp.Messages, resp.Excerpts, busrender.HookBytes); text != "" {
		out.HookSpecificOutput = hookSpecific{HookEventName: in.Event, AdditionalContext: text}
	}
	if out == (hookOutput{}) {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(out) != nil {
		warn("could not encode the output")
		return nil
	}
	if _, err := stdout.Write(buf.Bytes()); err != nil {
		warn("could not write the output; %d messages wait for the next hook", len(resp.Messages))
		return nil
	}
	if len(resp.Messages) > 0 {
		hookConfirm(ctx, *socket, in.SessionID, resp.Messages, started, warn)
	}
	return nil
}

// hookConfirm tells the agent that the messages were printed. stdout is
// unbuffered: once Write returned, the whole output is in the pipe. A hook
// past hookLate does not confirm, and a confirmation that fails is not
// retried: either way the lease ends and the messages come again, marked.
func hookConfirm(ctx context.Context, socket, session string, msgs []busproto.Envelope, started time.Time, warn func(string, ...any)) {
	if age := time.Since(started); age >= hookLate {
		warn("printed %s after the start, too late to confirm; %d messages will be offered again", age.Round(time.Millisecond), len(msgs))
		return
	}
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	cctx, cancel := context.WithTimeout(ctx, hookConfirmBudget)
	defer cancel()
	if _, err := agent.Call(cctx, socket, agent.Request{Op: "confirm", Session: session, IDs: ids}); err != nil {
		reason := hookReason(err)
		if isTimeout(err) {
			reason = fmt.Sprintf("no answer within %s", hookConfirmBudget)
		}
		warn("could not confirm the delivery (%s); %d messages will be offered again", reason, len(msgs))
	}
}

// noticeChannel reports whether the harness shows a hook's systemMessage
// to the person without giving it to the model, on UserPromptSubmit:
//
//   - Claude Code: yes. A probe (claude 2.1.287, Haiku) asked the model to
//     quote every marker in its context: it quoted the additionalContext
//     markers and not the systemMessage ones, also after --resume; the
//     stream showed the systemMessage as an informational notice.
//   - Codex: yes. codex-rs/hooks parses systemMessage into a Warning
//     entry, which core/hook_runtime.rs sends as a Warning event to the
//     UI; only additionalContext becomes a developer message.
//   - Devin CLI: no documented user-only field (its hook docs list
//     decision, reason and additionalContext only), so nothing is shown:
//     the notice must never fall back to model context.
//   - Unknown: nothing.
func noticeChannel(h transcript.Agent) bool {
	return h == transcript.AgentClaude || h == transcript.AgentCodex
}

// heldNotice is the person's notice of senders whose messages are held
// until they accept them: who and how many, and where to review. It never
// carries a message's text.
func heldNotice(held []busproto.HeldSender, console string) string {
	if len(held) == 0 {
		return ""
	}
	var who []string
	n := 0
	for _, h := range held {
		who = append(who, fmt.Sprintf("%s (%d)", busproto.Preview(h.User), h.Count))
		n += h.Count
	}
	review := "run flopwire accepts --text in a terminal"
	if console != "" {
		review = "open " + console + " or " + review
	}
	return fmt.Sprintf("Flopwire: %d %s from %s %s held until you accept the sender; your agents have not seen %s. To review, %s.",
		n, plural(n, "message", "messages"), strings.Join(who, ", "), plural(n, "is", "are"), plural(n, "it", "them"), review)
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
	req := agent.Request{Op: "flush", Path: in.TranscriptPath, Session: in.SessionID, Event: in.Event}
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

// devinHeldElsewhere reports whether another devin process holds the
// session: its lock, beside the sessions.db Devin names for hooks
// (CHISEL_SESSION_DB), names a running devin that is not an ancestor of
// this hook.
func devinHeldElsewhere(session string, getenv func(string) string) bool {
	db := getenv("CHISEL_SESSION_DB")
	if db == "" {
		db = getenv("FLOPWIRE_DEVIN_DB")
	}
	d := local.NewDetector()
	if db == "" {
		db = filepath.Join(d.Home, ".local", "share", "devin", "cli", "sessions.db")
	}
	return d.DevinHeldElsewhere(db, session)
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
