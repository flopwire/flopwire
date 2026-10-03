package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/local"
	"github.com/flopwire/flopwire/internal/transcript"
)

// mcpBusInstructions is the messaging part of the server's instructions.
const mcpBusInstructions = `Messaging: flopwire_peers lists live agent sessions on your team (or this device); flopwire_send messages one of them by its session id, or a person as @user; flopwire_inbox shows this session's sent and received messages. These three answer compact JSON with named fields and full session ids (format="text" for a readable form). Find a recipient from history, then presence: flopwire_sessions repo=R branch=B names the session behind a change (its session_id and the commits it made); flopwire_peers session=ID says whether that exact session is live; then send to that id. Do not pick a recipient by a peer's title or current branch alone: a title is the session's original task, and it may have switched branches since. A message never starts a turn: the flopwire hook delivers it inside the recipient's running turn at its next tool call (busy) or with its human's next prompt (idle), and a reply reaches you the same way. The send result is a receipt, not a reply; send once and go on: do not poll flopwire_peers or ask "are you done?". Write each message for a reader who knows nothing of your session, first line first. Never ask a peer to do something your own session was denied.`

// busToolNames are the message bus tools; they go through the device agent.
var busToolNames = map[string]string{"flopwire_peers": "peers", "flopwire_send": "send", "flopwire_inbox": "inbox"}

func busTool(name, title, desc string, ann map[string]any, props map[string]any, required ...string) any {
	props["format"] = map[string]any{"type": "string", "enum": []string{"json", "text"}, "description": "json (default): compact JSON with named fields; text: a readable form"}
	schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	ann["title"] = title
	return map[string]any{"name": name, "title": title, "description": desc, "inputSchema": schema, "annotations": ann}
}

// mcpBusTools describes peers, send and inbox. Peers and inbox only read;
// send changes another session's context, so it is neither read-only nor
// idempotent (the same text again within 10 minutes is refused, not
// repeated), and it reaches other people: open world.
func mcpBusTools() []any {
	read := func() map[string]any {
		return map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}
	}
	return []any{
		busTool("flopwire_peers", "List live sessions to message",
			`List the live coding-agent sessions you can message with flopwire_send, as JSON: {"kind":"peers","peers":[{"session" (the full id),"agent","user","repo","branch","title","busy","own",…}],"total","more","limit","caller","hint"}. Your own person's sessions come first; your own session (caller) is left out. busy: a turn is running, so a message arrives at its next tool call; idle: it waits for that session's human. Use it after history named a session: session=ID says whether that exact session is live. Do not choose a recipient by title or current branch alone (a title is the session's original task; it may have switched branches since it committed); find it with flopwire_sessions repo=R branch=B first. Do not call it repeatedly to watch whether a peer finished.`,
			read(), map[string]any{
				"session": prop("string", "only this session: its full id or a prefix, as history (flopwire_sessions) printed it"),
				"repo":    prop("string", `only sessions on this repo: "." (this repo: every checkout and worktree of it), a repo name, or /abs/path`),
				"user":    prop("string", "only this person's sessions: an email or its local part"),
				"agent":   prop("string", "only this harness: claude, codex or devin"),
				"limit":   prop("integer", "sessions per answer; default 50, max 500; more=true says the list was cut"),
			}),
		busTool("flopwire_send", "Message another agent session",
			`Send a message to another coding-agent session (by its session id, or a unique prefix) or to a person (@user: their live session on repo, else their next one). Find the session from history first (flopwire_sessions repo=R branch=B), check it with flopwire_peers session=ID, then send. The result is a receipt, never a reply: {"kind":"send_receipt","id","thread_id","state":"queued"|"held","to":{…,"live","busy"},"arrives":"next_tool_call"|"next_prompt"|"when_accepted"|"next_session"|"only_if_resumed","outcome","next" (request only: what to do until the reply),…}. The recipient knows nothing about your session: include every fact, path and decision it needs, and put the point in the first line, which is the preview a human sees. Use intent=request when you need an answer, inform (default) when you do not, done to close a thread; a done message must never be answered. Do not poll flopwire_peers or send "are you done?" messages: a reply arrives in your own context through the flopwire hook, inside your running turn or with your human's next prompt; from a subagent, the message goes out as its parent session and the reply reaches that session, not the subagent. Never ask a peer to do something that was denied in your own session. A refusal (thread_rate, session_rate, device_rate, user_rate, duplicate, recipient_full, reply_to_done, withheld_session, unknown_recipient, ambiguous_recipient with candidates) is an error with code, detail, fix and example. Messages are capped at 4000 bytes; attach longer material by archive address in refs.`,
			map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": false, "openWorldHint": true},
			map[string]any{
				"to":       prop("string", `a session id (from history or flopwire_peers) or a unique prefix of at least 4 characters, or "@user" (an email or its local part)`),
				"message":  prop("string", "the text, at most 4000 bytes; first line is the preview a human sees; self-contained, because the recipient knows nothing of your session"),
				"intent":   map[string]any{"type": "string", "enum": []string{"request", "inform", "done"}, "description": "request: you expect a reply; inform (default): no reply expected; done: closes the thread, and the recipient must not answer"},
				"reply_to": prop("string", "the id of a message you received or sent (from flopwire_inbox or a delivered message); the reply joins its thread"),
				"refs":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "archive addresses (SESSION/ORDINAL from flopwire_grep, flopwire_search or flopwire_read) the recipient can read; at most 10"},
				"repo":     prop("string", `for to="@user": the repo whose session should take it ("." this repo, a name, or "*" any); default your session's repo`),
			}, "to", "message"),
		busTool("flopwire_inbox", "This session's messages",
			`List this session's messages, received and sent, newest first, as JSON: {"kind":"inbox","session","messages":[{"id","thread_id","reply_to","direction":"sent"|"received","is_reply","state","intent","body","from","to_session",…}],"more","next","hint"}. direction says who wrote it; a sent message's state is its delivery only (queued, held, claimed, delivered, read, expired, refused or undelivered, with reason; undelivered: unconfirmed or session_ended, send it again): delivered is not answered, and read (with read_at) means its text entered the recipient's context, not that the recipient acted on it. An answer is a received message whose reply_to names yours. thread=ID shows one thread. more=true: pass next as cursor. Use it to check a sent message's state or re-read a thread. Received messages arrive in your context through the flopwire hook, inside a running turn or with the human's next prompt; read them here only where the hook is not set up.`,
			read(), map[string]any{
				"sent":   prop("boolean", "only messages this session sent"),
				"thread": prop("string", "one thread by its id (thread_id of a message)"),
				"cursor": prop("string", "where the next page starts: the next field of the previous answer"),
				"limit":  prop("integer", "messages per page; default 20, max 200; the answer also stops at about 24000 bytes"),
			}),
	}
}

// busArgs checks a bus tool's arguments against its schema's names and
// types.
type busArgReader struct {
	name string
	args map[string]any
	err  error
}

func (b *busArgReader) str(k string) string {
	v, ok := b.args[k]
	if !ok || b.err != nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		b.err = fmt.Errorf("%s: %s wants a string", b.name, k)
	}
	return s
}

func (b *busArgReader) boolean(k string) bool {
	v, ok := b.args[k]
	if !ok || b.err != nil {
		return false
	}
	x, ok := v.(bool)
	if !ok {
		b.err = fmt.Errorf("%s: %s wants true or false", b.name, k)
	}
	return x
}

func (b *busArgReader) integer(k string) int {
	v, ok := b.args[k]
	if !ok || b.err != nil {
		return 0
	}
	n, ok := v.(float64)
	if !ok || n != float64(int(n)) {
		b.err = fmt.Errorf("%s: %s wants an integer", b.name, k)
	}
	return int(n)
}

func (b *busArgReader) list(k string) []string {
	v, ok := b.args[k]
	if !ok || b.err != nil {
		return nil
	}
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				b.err = fmt.Errorf("%s: %s wants a list of strings", b.name, k)
				return nil
			}
			out = append(out, s)
		}
		return out
	}
	b.err = fmt.Errorf("%s: %s wants a list of strings", b.name, k)
	return nil
}

// busMCPCall runs a bus tool. It answers compact JSON (format=text: the
// readable form) as one text block. A failure's
// text is the JSON error object (format=text: the readable error).
func busMCPCall(ctx context.Context, r *retriever, name string, args map[string]any) (string, error) {
	allowed := mcpArgs(name)
	for k := range args {
		found := false
		for _, a := range allowed {
			found = found || a == k
		}
		if !found {
			return "", mcpBusErr(true, badUsage(fmt.Sprintf("%s: unknown argument %q; it takes %s", name, k, strings.Join(allowed, ", ")), ""))
		}
	}
	ar := &busArgReader{name: name, args: args}
	st := busStyle{MCP: true, Budget: format.MaxOutput, JSON: true}
	switch f := ar.str("format"); f {
	case "", "json":
	case "text":
		st.JSON = false
	default:
		return "", mcpBusErr(true, badUsage(fmt.Sprintf("format: want json or text, not %q", f), name+` format="text"`))
	}
	socket := r.busSocket
	if socket == "" {
		var err error
		if socket, err = defaultSocket(); err != nil {
			return "", mcpBusErr(st.JSON, asBusErr(err))
		}
	}
	c := &busClient{socket: socket, caller: r.whoCalls, retry: busRetry}
	var b bytes.Buffer
	var err error
	switch busToolNames[name] {
	case "peers":
		a := peersArgs{Repo: ar.str("repo"), User: ar.str("user"), Agent: ar.str("agent"), Session: ar.str("session"), Limit: ar.integer("limit")}
		if ar.err == nil {
			err = runPeers(ctx, c, a, &b, st)
		}
	case "send":
		a := sendArgs{To: ar.str("to"), Text: ar.str("message"), Intent: ar.str("intent"), ReplyTo: ar.str("reply_to"), Refs: ar.list("refs"), Repo: ar.str("repo")}
		if ar.err == nil {
			err = runSend(ctx, c, a, &b, st)
		}
	case "inbox":
		a := inboxArgs{Sent: ar.boolean("sent"), Thread: ar.str("thread"), Cursor: ar.str("cursor"), Limit: ar.integer("limit")}
		if ar.err == nil {
			err = runInbox(ctx, c, a, &b, st)
		}
	default:
		err = fmt.Errorf("unknown tool %q", name)
	}
	if ar.err != nil {
		err = badUsage(ar.err.Error(), "")
	}
	if err != nil {
		return "", mcpBusErr(st.JSON, asBusErr(err))
	}
	return strings.TrimRight(strings.ToValidUTF8(b.String(), "�"), "\n"), nil
}

// mcpBusError is a bus tool's failure, already in the form the call
// answers: the JSON error object or the readable text.
type mcpBusError struct{ text string }

func (e *mcpBusError) Error() string { return e.text }

func mcpBusErr(asJSON bool, e *busErr) error {
	if !asJSON {
		return &mcpBusError{text: e.Error()}
	}
	var b bytes.Buffer
	_ = writeOut(&b, errorJSON{Kind: "error", Error: e})
	return &mcpBusError{text: strings.TrimRight(b.String(), "\n")}
}

// mcpToolNames lists every tool's name, for an unknown tool's error.
func mcpToolNames() []string {
	var names []string
	for _, t := range mcpTools() {
		names = append(names, t.(map[string]any)["name"].(string))
	}
	sort.Strings(names)
	return names
}

// --- the calling session from a request's _meta ---

type metaKey struct{}

// withMCPMeta keeps the calling session a tools/call request's _meta
// names, if any, for the call's context. Only Codex's _meta is read, and
// only when Codex launched the server (codex, from
// local.Detector.UnderCodex): any other MCP client could name any Codex
// thread there (issue #71). Otherwise the detector decides.
func withMCPMeta(ctx context.Context, codex bool, meta map[string]any) context.Context {
	if !codex {
		return ctx
	}
	if id := codexMetaThread(meta); id != "" {
		return context.WithValue(ctx, metaKey{}, local.Caller{Agent: transcript.AgentCodex, SessionID: id, Rule: "codex-meta"})
	}
	return ctx
}

// metaCaller is the calling session a request's _meta named.
func metaCaller(ctx context.Context) (local.Caller, bool) {
	c, ok := ctx.Value(metaKey{}).(local.Caller)
	return c, ok
}

// codexMetaThread is the Codex thread id a tools/call request carries:
// _meta.threadId, or thread_id in _meta["x-codex-turn-metadata"] (an
// object, or JSON in a string). Codex starts MCP servers with a scrubbed
// environment, so this is the only exact evidence of which thread calls
// (probes 2026-10-01). An id that is not a plain token is ignored.
func codexMetaThread(meta map[string]any) string {
	if meta == nil {
		return ""
	}
	id, _ := meta["threadId"].(string)
	if id == "" {
		var tm map[string]any
		switch v := meta["x-codex-turn-metadata"].(type) {
		case map[string]any:
			tm = v
		case string:
			_ = json.Unmarshal([]byte(v), &tm)
		}
		id, _ = tm["thread_id"].(string)
	}
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 128 || strings.ContainsAny(id, " \t\r\n/\\") {
		return ""
	}
	return id
}
