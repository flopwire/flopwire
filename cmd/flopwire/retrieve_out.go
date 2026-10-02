package main

// How the retrieval tools answer in JSON (issue #64, the decision on
// #55). sessions is record-shaped: it answers compact JSON by default on
// the CLI and over MCP, as peers, send and inbox do; --text (format=text)
// prints the readable rows. grep, search and read are text-shaped: they
// print text by default, and --json prints the answer as before. Over MCP
// every retrieval tool also returns its answer as structuredContent,
// declared by an outputSchema, bounded like the text: whole hits,
// sessions, messages or outline entries until the next would pass the
// budget, and fields that say where the next page starts.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/retrieval/format"
)

// jsonMode reports whether a retrieval verb answers JSON: sessions unless
// --text (format=text) asks for text, the others only with --json
// (format=json). --text wins over --json, as for the message bus verbs.
func jsonMode(o *opts) bool {
	if o.on["text"] {
		return false
	}
	return o.on["json"] || o.verb == "sessions"
}

// jsonModeArgs is jsonMode for a command line the parser rejected: the
// flags it holds before "--".
func jsonModeArgs(verb string, args []string) bool {
	flags := textFlags(args)
	if slices.Contains(flags, "--text") {
		return false
	}
	return verb == "sessions" || slices.Contains(flags, "--json")
}

// sessionsJSON is sessions' JSON answer: format.Sessions with the kind and
// the paging fields always present. has_more says sessions follow;
// next_cursor is then the cursor that reads them.
type sessionsJSON struct {
	Kind     string                    `json:"kind"` // "sessions"
	Sessions []format.ConversationInfo `json:"sessions"`
	HasMore  bool                      `json:"has_more"`
	Next     string                    `json:"next_cursor,omitempty"`
	Notes    []string                  `json:"notes,omitempty"`
	Excluded string                    `json:"excluded,omitempty"`
	Hint     string                    `json:"hint,omitempty"`
}

// pageJSON is grep's and search's structured answer: format.Page (the
// --json shape) with its kind, and a hint when the budget cut the page.
type pageJSON struct {
	Kind string `json:"kind"` // "grep" or "search"
	*format.Page
	Hint string `json:"hint,omitempty"`
}

// readJSON is read's structured answer: format.Context with its kind.
type readJSON struct {
	Kind string `json:"kind"` // "read"
	*format.Context
	Hint string `json:"hint,omitempty"`
}

// rawRecordJSON is read raw=true's structured answer: the record's bytes as a
// string, left out (Omitted) when they pass the budget.
type rawRecordJSON struct {
	Kind    string `json:"kind"` // "raw"
	Address string `json:"address"`
	Bytes   int    `json:"bytes"`
	Raw     string `json:"raw,omitempty"`
	Omitted bool   `json:"omitted,omitempty"`
	Hint    string `json:"hint,omitempty"`
}

// jsonSize is v's size as compact JSON.
func jsonSize(v any) int {
	var b bytes.Buffer
	if writeOut(&b, v) != nil {
		return 1 << 30
	}
	return b.Len()
}

// fitsBudget reports whether v encodes within budget bytes (0: no budget).
func fitsBudget(v any, budget int) bool {
	return budget <= 0 || jsonSize(v) <= budget
}

// most is the largest k in [1, n] for which fits(k) holds, assuming fits
// is monotone, or 1 when none does: one whole entry is always kept.
func most(n int, fits func(k int) bool) int {
	k := sort.Search(n+1, func(k int) bool { return k > 0 && !fits(k) })
	return max(k-1, min(n, 1))
}

// arg is the CLI flag or the MCP argument that pages on.
func arg(mcp bool, cli, name, v string) string {
	if mcp {
		return name + "=" + v
	}
	return cli + " " + v
}

// hintRoom is the room a cut answer keeps for its hint.
const hintRoom = 400

// room is the budget the entries of a cut answer fill: the budget less
// the hint's room.
func room(budget int) int { return max(budget-hintRoom, 1) }

func budgetHint(budget int, next string) string {
	return fmt.Sprintf("output budget of %d bytes reached; next: %s", budget, next)
}

// boundSessions is the sessions answer in JSON, within budget: whole
// sessions, the cursor after the last one kept.
func boundSessions(s *format.Sessions, budget int, mcp bool) sessionsJSON {
	out := sessionsJSON{Kind: "sessions", Sessions: s.Sessions, HasMore: s.HasMore || s.Next != "", Next: s.Next, Notes: s.Notes, Excluded: s.Excluded}
	if out.Sessions == nil {
		out.Sessions = []format.ConversationInfo{}
	}
	all := out.Sessions
	if !fitsBudget(out, budget) && len(all) > 1 {
		n := most(len(all), func(k int) bool {
			c := out
			c.Sessions, c.HasMore, c.Next = all[:k], true, format.SessionCursor(all[k-1])
			return fitsBudget(c, room(budget))
		})
		if n < len(all) {
			out.Sessions, out.HasMore, out.Next = all[:n], true, format.SessionCursor(all[n-1])
			out.Hint = budgetHint(budget, arg(mcp, "--cursor", "cursor", out.Next))
			return out
		}
	}
	if out.HasMore {
		out.Hint = "pass next_cursor as " + arg(mcp, "--cursor", "cursor", "C") + " for the next page"
	}
	return out
}

// boundPage is a grep or search page as structured content, within
// budget: whole hits (or -l/-c sessions), the next offset after the last
// one kept, and the session descriptions of the hits kept. One hit that
// alone passes the budget keeps its first lines.
func boundPage(kind string, p *format.Page, mode string, budget int, mcp bool) pageJSON {
	cp := *p
	if cp.Hits == nil {
		cp.Hits = []format.Hit{}
	}
	out := pageJSON{Kind: kind, Page: &cp}
	if fitsBudget(out, budget) {
		return out
	}
	cut := func(next int) {
		cp.Next = next
		out.Hint = budgetHint(budget, arg(mcp, "--offset", "offset", strconv.Itoa(next)))
	}
	if mode == format.ModeSessions || mode == format.ModeCount {
		all := p.Sessions
		n := most(len(all), func(k int) bool {
			c := cp
			c.Sessions = all[:k]
			return fitsBudget(pageJSON{Kind: kind, Page: &c}, room(budget))
		})
		if n < len(all) {
			cp.Sessions = all[:n]
			cut(p.Offset + n)
		}
		return out
	}
	all := cp.Hits
	keep := func(k int) {
		cp.Hits = all[:k]
		cp.SessionInfo = nil
		for _, c := range p.SessionInfo {
			if slices.ContainsFunc(cp.Hits, func(h format.Hit) bool { return h.SessionID == c.SessionID }) {
				cp.SessionInfo = append(cp.SessionInfo, c)
			}
		}
	}
	n := most(len(all), func(k int) bool {
		keep(k)
		return fitsBudget(out, room(budget))
	})
	keep(n)
	if n < len(all) {
		cut(p.Offset + n)
	}
	if n == 1 && !fitsBudget(out, room(budget)) {
		h := all[0]
		lines := h.Lines
		k := most(len(lines), func(k int) bool {
			h.Lines = lines[:k]
			cp.Hits = []format.Hit{h}
			return fitsBudget(out, room(budget))
		})
		h.Lines = lines[:k]
		for _, l := range lines[k:] {
			if l.Match {
				h.MoreLines++
			}
		}
		cp.Hits = []format.Hit{h}
		if out.Hint == "" {
			out.Hint = fmt.Sprintf("output budget of %d bytes reached; %s reads the whole message", budget, arg(mcp, "flopwire read", "flopwire_read address", h.Address))
		}
	}
	return out
}

// boundRead is read's answer as structured content, within budget: an
// outline keeps whole entries and gives the cursor after the last one
// kept; messages keep the focus and the nearest neighbours, and a focus
// that alone passes the budget keeps its first lines.
func boundRead(cx *format.Context, budget int, mcp bool) readJSON {
	cp := *cx
	if cp.Messages == nil {
		cp.Messages = []format.Message{}
	}
	out := readJSON{Kind: "read", Context: &cp}
	if fitsBudget(out, budget) {
		return out
	}
	session := cx.Conversation.Address
	if session == "" {
		session = cx.Conversation.SessionID
	}
	if cx.Outline != nil {
		all := cx.Outline
		n := most(len(all), func(k int) bool {
			cp.Outline = all[:k]
			return fitsBudget(out, room(budget))
		})
		cp.Outline = all[:n]
		if n < len(all) {
			cp.OutlineMore, cp.OutlineNext = true, format.OutlineCursor(all[n-1])
			out.Hint = budgetHint(budget, arg(mcp, "flopwire read "+session+" --outline --cursor", "flopwire_read address="+session+" outline=true cursor", cp.OutlineNext))
		}
		return out
	}
	all := cx.Messages
	at := slices.IndexFunc(all, func(m format.Message) bool { return m.ID == cx.Focus })
	if at < 0 {
		at = 0
	}
	lo, hi := at, at+1
	set := func() { cp.Messages = all[lo:hi] }
	set()
	for grew := true; grew; {
		grew = false
		if lo > 0 {
			lo--
			if set(); fitsBudget(out, room(budget)) {
				grew = true
			} else {
				lo++
			}
		}
		if hi < len(all) {
			hi++
			if set(); fitsBudget(out, room(budget)) {
				grew = true
			} else {
				hi--
			}
		}
	}
	set()
	cp.MoreBefore = cx.MoreBefore || lo > 0
	cp.MoreAfter = cx.MoreAfter || hi < len(all)
	more := func(addr, dir, flag string) string {
		if mcp {
			return fmt.Sprintf("flopwire_read address=%s %s=10", addr, dir)
		}
		return fmt.Sprintf("flopwire read %s %s 10", addr, flag)
	}
	if lo > 0 || hi < len(all) {
		var hints []string
		if lo > 0 {
			hints = append(hints, more(all[lo].Address, "before", "-B"))
		}
		if hi < len(all) {
			hints = append(hints, more(all[hi-1].Address, "after", "-A"))
		}
		out.Hint = fmt.Sprintf("output budget of %d bytes reached; more: %s", budget, strings.Join(hints, "; "))
	}
	if len(cp.Messages) == 1 && !fitsBudget(out, room(budget)) {
		m := cp.Messages[0]
		text := m.Text
		lines := strings.Split(text, "\n")
		from := max(m.LineFrom, 1)
		try := func(s string, to int) bool {
			m.Text, m.LineTo, m.Clipped = s, to, true
			cp.Messages = []format.Message{m}
			return fitsBudget(out, room(budget))
		}
		k := sort.Search(len(lines)+1, func(k int) bool { return k > 0 && !try(strings.Join(lines[:k], "\n"), from+k-1) }) - 1
		if k >= 1 {
			try(strings.Join(lines[:k], "\n"), from+k-1)
		} else {
			first := lines[0]
			n := sort.Search(len(first)+1, func(n int) bool { return !try(strings.ToValidUTF8(first[:n], ""), from) }) - 1
			try(strings.ToValidUTF8(first[:max(n, 0)], ""), from)
			k = 1
		}
		if from+k-1 < max(m.Lines, from+len(lines)-1) {
			out.Hint = fmt.Sprintf("output budget of %d bytes reached; more: %s", budget, arg(mcp, "flopwire read "+m.Address+" --line-offset", "flopwire_read address="+m.Address+" line_offset", strconv.Itoa(from+k)))
		}
	}
	return out
}

// boundRaw is a raw record as structured content: its bytes when they fit
// the budget.
func boundRaw(addr string, data []byte, budget int, mcp bool) rawRecordJSON {
	out := rawRecordJSON{Kind: "raw", Address: addr, Bytes: len(data), Raw: string(data)}
	if !fitsBudget(out, budget) {
		out.Raw, out.Omitted = "", true
		out.Hint = fmt.Sprintf("the record is %d bytes, past the output budget of %d bytes; the text answer holds it whole", len(data), budget)
		if !mcp {
			out.Hint = fmt.Sprintf("the record is %d bytes, past --max-bytes %d", len(data), budget)
		}
	}
	return out
}

// --- errors ---

// Error codes of the retrieval tools, beside busproto's bad_request and
// not_found.
const (
	codeForbidden = "forbidden"
	codeNoIndex   = "no_index"
	codeSyncOnly  = "sync_only"
	codeError     = "error"
)

// retrievalErr is a failed retrieval call as the JSON error object the
// message bus verbs print: a stable code, the cause, a fix and an example.
func retrievalErr(verb string, err error, mcp bool) *busErr {
	var be *busErr
	if errors.As(err, &be) {
		return be
	}
	var noIndex *noIndexError
	e := &busErr{Code: codeError, Detail: shortError(err)}
	ex := map[string][2]string{
		"grep":     {"flopwire grep 'exit status 1' --since 7d", `flopwire_grep pattern="exit status 1" since="7d"`},
		"search":   {"flopwire search exponential backoff", `flopwire_search query="exponential backoff"`},
		"sessions": {"flopwire sessions --repo . --branch 'feat/*'", `flopwire_sessions repo="." branch="feat/*"`},
		"read":     {"flopwire read 0b7e2c1a/28672 -A 2", `flopwire_read address="0b7e2c1a/28672" after=2`},
	}[verb]
	example := ex[0]
	help := "flopwire " + verb + " --help"
	if mcp {
		example, help = ex[1], "the tool's description"
	}
	switch {
	case errors.Is(err, format.ErrBadRequest):
		e.Code, e.Status, e.Fix, e.Example = busproto.CodeBadRequest, 400, "check the arguments against "+help, example
	case errors.Is(err, format.ErrNotFound):
		e.Code, e.Status, e.Fix = busproto.CodeNotFound, 404, "addresses and session ids come from grep, search or sessions output"
		e.Example = map[bool]string{false: "flopwire sessions --repo .", true: `flopwire_sessions repo="."`}[mcp]
	case errors.Is(err, format.ErrForbidden):
		e.Code, e.Status = codeForbidden, 403
	case errors.As(err, &noIndex) || strings.HasPrefix(e.Detail, "no index yet"):
		e.Code, e.Fix, e.Example = codeNoIndex, "start the device agent, which builds the local index, or query the team server with --server", "flopwire agent status"
	case errors.Is(err, localindex.ErrSyncOnly):
		e.Code, e.Fix, e.Example = codeSyncOnly, "this device keeps no local index: query the team server", "flopwire "+verb+" --server"
	}
	return e
}

// writeErrorJSON writes a failure as one JSON object, as the message bus
// verbs do.
func writeErrorJSON(w io.Writer, e *busErr) error {
	return writeOut(w, errorJSON{Kind: "error", Error: e})
}

// --- output schemas ---

var (
	strArr     = arr(typ("string"))
	convSchema = obj([]string{"address", "id", "agent", "session_id"}, map[string]any{
		"address": typ("string"), "id": typ("string"), "agent": typ("string"), "session_id": typ("string"), "title": typ("string"),
		"cwd": typ("string"), "repo": typ("string"), "device": typ("string"), "user": typ("string"), "started_at": typ("string"),
		"last_activity_at": typ("string"), "parent_conversation_id": typ("string"), "parent_session": typ("string"),
		"spawned_by_message_id": typ("string"), "depth": typ("integer"), "messages": typ("integer"), "hits": typ("integer"),
		"branches": strArr, "digest": typ("object"), "live": typ("boolean")})
	provSchema = obj([]string{"path", "generation"}, map[string]any{
		"source_id": typ("string"), "path": typ("string"), "file_id": typ("string"), "generation": typ("integer"), "line_no": typ("integer"),
		"byte_offset": typ("integer"), "byte_len": typ("integer"), "locator": typ("string")})
	hitSchema = obj([]string{"address", "message_id", "conversation_id", "agent", "session_id", "ordinal", "kind", "provenance"}, map[string]any{
		"address": typ("string"), "message_id": typ("string"), "conversation_id": typ("string"), "agent": typ("string"), "session_id": typ("string"),
		"ordinal": typ("integer"), "title": typ("string"), "repo": typ("string"), "device": typ("string"), "user": typ("string"), "branches": strArr,
		"kind": typ("string"), "tool_name": typ("string"), "is_error": typ("boolean"), "ts": typ("string"), "score": typ("number"),
		"snippet": typ("string"), "text_line": typ("integer"),
		"lines":      arr(obj([]string{"n", "text"}, map[string]any{"n": typ("integer"), "text": typ("string"), "match": typ("boolean")})),
		"more_lines": typ("integer"), "superseded": typ("boolean"), "off_active_path": typ("boolean"), "copies": typ("integer"), "provenance": provSchema})
	pageProps = func(kind string) map[string]any {
		return map[string]any{
			"kind": map[string]any{"type": "string", "enum": []string{kind}}, "hits": arr(hitSchema), "sessions": arr(convSchema),
			"total": typ("integer"), "total_sessions": typ("integer"), "exact": typ("boolean"), "offset": typ("integer"), "next_offset": typ("integer"),
			"truncated": typ("boolean"), "reason": typ("string"), "notes": strArr, "excluded": typ("string"), "session_info": arr(convSchema), "hint": typ("string")}
	}
	messageSchema = obj([]string{"address", "id", "ordinal", "kind", "text", "text_len", "provenance"}, map[string]any{
		"address": typ("string"), "id": typ("string"), "native_id": typ("string"), "ordinal": typ("integer"), "kind": typ("string"), "role": typ("string"),
		"tool_name": typ("string"), "tool_call_id": typ("string"), "is_error": typ("boolean"), "ts": typ("string"), "text": typ("string"),
		"text_len": typ("integer"), "stored_len": typ("integer"), "lines": typ("integer"), "line_from": typ("integer"), "line_to": typ("integer"),
		"clipped": typ("boolean"), "version": typ("integer"), "superseded": typ("boolean"), "off_active_path": typ("boolean"), "provenance": provSchema})
	outlineSchema = obj([]string{"address", "id", "ordinal", "kind", "text"}, map[string]any{
		"address": typ("string"), "id": typ("string"), "ordinal": typ("integer"), "ts": typ("string"), "kind": typ("string"), "tool": typ("string"),
		"text": typ("string"), "error": typ("boolean"), "subagents": strArr})
)

// retrievalOutputSchema is a retrieval tool's outputSchema. read's covers
// both of its answers: messages or an outline (kind "read"), and a raw
// record (kind "raw").
func retrievalOutputSchema(verb string) map[string]any {
	switch verb {
	case "grep", "search":
		return obj([]string{"kind", "hits"}, pageProps(verb))
	case "sessions":
		return obj([]string{"kind", "sessions", "has_more"}, map[string]any{
			"kind": map[string]any{"type": "string", "enum": []string{"sessions"}}, "sessions": arr(convSchema), "has_more": typ("boolean"),
			"next_cursor": typ("string"), "notes": strArr, "excluded": typ("string"), "hint": typ("string")})
	}
	return obj([]string{"kind"}, map[string]any{
		"kind": map[string]any{"type": "string", "enum": []string{"read", "raw"}}, "conversation": convSchema, "focus": typ("string"),
		"line": typ("integer"), "messages": arr(messageSchema), "more_before": typ("boolean"), "more_after": typ("boolean"),
		"outline": arr(outlineSchema), "outline_more": typ("boolean"), "outline_next": typ("string"),
		"address": typ("string"), "bytes": typ("integer"), "raw": typ("string"), "omitted": typ("boolean"), "hint": typ("string")})
}
