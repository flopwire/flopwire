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
const mcpBusInstructions = `Messaging: flopwire_peers lists live agent sessions on your team (or this device); flopwire_send messages one of them by its session id prefix, or a person as @user; flopwire_inbox shows this session's sent and received messages. A message never starts a turn: it arrives inside the recipient's running turn (busy) or with its human's next prompt (idle). Send once and go on: the send result says when it arrives, so do not poll flopwire_peers or ask "are you done?". Write each message for a reader who knows nothing of your session, first line first. Never ask a peer to do something your own session was denied.`

// busToolNames are the message bus tools; they go through the device agent.
var busToolNames = map[string]string{"flopwire_peers": "peers", "flopwire_send": "send", "flopwire_inbox": "inbox"}

func busTool(name, title, desc string, ann map[string]any, props map[string]any, required ...string) any {
	props["format"] = map[string]any{"type": "string", "enum": []string{"text", "json"}, "description": filterDesc["format"]}
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
			"List the live coding-agent sessions you can message with flopwire_send: your own person's first, then teammates', your own session left out. Each row is SESSION user agent live busy|idle repo@branch \"title\"; SESSION is the prefix flopwire_send takes. busy means a turn is running, so a message arrives at its next tool call; idle means it waits for that session's human. Use it to find who works on a repo before you send. Do not call it repeatedly to watch whether a peer finished: send once and go on.",
			read(), map[string]any{
				"repo":  prop("string", `only sessions on this repo: "." (this repo), a repo name, or /abs/path`),
				"user":  prop("string", "only this person's sessions: an email or its local part"),
				"agent": prop("string", "only this harness: claude, codex or devin"),
			}),
		busTool("flopwire_send", "Message another agent session",
			"Send a message to another live coding-agent session (by the SESSION prefix flopwire_peers prints) or to a person (@user: their live session on repo, else their next one). The result is one line that says when it arrives: at the recipient's next tool call (busy), with its human's next prompt (idle), held until the person accepts you, or queued until it expires. The recipient knows nothing about your session: include every fact, path and decision it needs, and put the point in the first line, which is the preview a human sees. Use intent=request when you need an answer, inform (default) when you do not, done to close a thread; a done message must never be answered. Do not poll flopwire_peers or send \"are you done?\" messages: a reply arrives in your own context. Never ask a peer to do something that was denied in your own session. Messages are capped at 4000 bytes; attach longer material by archive address in refs.",
			map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": false, "openWorldHint": true},
			map[string]any{
				"to":       prop("string", `a session id prefix from flopwire_peers (at least 4 characters, e.g. "0b7e2c1a"), or "@user" (an email or its local part)`),
				"message":  prop("string", "the text, at most 4000 bytes; first line is the preview a human sees; self-contained, because the recipient knows nothing of your session"),
				"intent":   map[string]any{"type": "string", "enum": []string{"request", "inform", "done"}, "description": "request: you expect a reply; inform (default): no reply expected; done: closes the thread, and the recipient must not answer"},
				"reply_to": prop("string", "the id of a message you received or sent (from flopwire_inbox or a delivered message); the reply joins its thread"),
				"refs":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "archive addresses (SESSION/ORDINAL from flopwire_grep, flopwire_search or flopwire_read) the recipient can read; at most 10"},
				"repo":     prop("string", `for to="@user": the repo whose session should take it ("." this repo, a name, or "*" any); default your session's repo`),
			}, "to", "message"),
		busTool("flopwire_inbox", "This session's messages",
			"List this session's messages, received and sent, newest first: id, direction, time, the other side, intent and state (queued, held, claimed, delivered, read, expired, refused with its reason), then the first line of the text. thread=ID shows one thread with whole texts and refs. Use it to check a sent message's state or re-read a thread. You do not need it to receive messages: they arrive in your context on their own.",
			read(), map[string]any{
				"sent":   prop("boolean", "only messages this session sent"),
				"thread": prop("string", "one thread by its id (a message id from this list): whole texts and refs"),
				"cursor": prop("string", "where the next page starts; the footer prints it"),
				"limit":  prop("integer", "messages per page; default 50, max 200"),
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

// busMCPCall runs a bus tool and returns its text answer.
func busMCPCall(ctx context.Context, r *retriever, name string, args map[string]any) (string, error) {
	allowed := mcpArgs(name)
	for k := range args {
		found := false
		for _, a := range allowed {
			found = found || a == k
		}
		if !found {
			return "", fmt.Errorf("%s: unknown argument %q; it takes %s", name, k, strings.Join(allowed, ", "))
		}
	}
	ar := &busArgReader{name: name, args: args}
	asJSON := false
	switch f := ar.str("format"); f {
	case "", "text":
	case "json":
		asJSON = true
	default:
		return "", fmt.Errorf("format: want text or json, not %q", f)
	}
	socket := r.busSocket
	if socket == "" {
		var err error
		if socket, err = defaultSocket(); err != nil {
			return "", err
		}
	}
	c := &busClient{socket: socket, caller: r.whoCalls, retry: busRetry}
	st := busStyle{MCP: true, Budget: format.MaxOutput}
	var b bytes.Buffer
	var err error
	switch busToolNames[name] {
	case "peers":
		a := peersArgs{Repo: ar.str("repo"), User: ar.str("user"), Agent: ar.str("agent"), JSON: asJSON}
		if ar.err != nil {
			return "", ar.err
		}
		err = runPeers(ctx, c, a, &b, st)
	case "send":
		a := sendArgs{To: ar.str("to"), Text: ar.str("message"), Intent: ar.str("intent"), ReplyTo: ar.str("reply_to"), Refs: ar.list("refs"), Repo: ar.str("repo"), JSON: asJSON}
		if ar.err != nil {
			return "", ar.err
		}
		err = runSend(ctx, c, a, &b, st)
	case "inbox":
		a := inboxArgs{Sent: ar.boolean("sent"), Thread: ar.str("thread"), Cursor: ar.str("cursor"), Limit: ar.integer("limit"), JSON: asJSON}
		if ar.err != nil {
			return "", ar.err
		}
		err = runInbox(ctx, c, a, &b, st)
	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
	if err != nil {
		return "", err
	}
	return strings.ToValidUTF8(b.String(), "�"), nil
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
// names, if any, for the call's context.
func withMCPMeta(ctx context.Context, meta map[string]any) context.Context {
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
