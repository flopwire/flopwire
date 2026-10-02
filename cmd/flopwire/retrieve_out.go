package main

// How the retrieval tools answer in JSON (issue #64, the decision on
// #55). sessions is record-shaped: it answers compact JSON by default on
// the CLI and over MCP, as peers, send and inbox do; --text (format=text)
// prints the readable rows. grep, search and read are text-shaped: they
// print text by default, and --json prints the answer as before. Over MCP
// (and for sessions on the CLI) the JSON is compact and bounded like the
// text: whole hits, sessions, messages or outline entries until the next
// would pass the budget, and fields that say where the next page starts.
// Every MCP answer is one text block: no structuredContent, because
// Claude Code shows a model only that and Codex shows both (#84).

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

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

// sessionsJSON is sessions' JSON answer: one page of sessions (brief
// by default, format.ConversationInfo with --detail), the kind, and the
// paging fields always present. has_more says sessions follow;
// next_cursor is then the cursor that reads them.
type sessionsJSON struct {
	Kind     string   `json:"kind"` // "sessions"
	Sessions any      `json:"sessions"`
	HasMore  bool     `json:"has_more"`
	Next     string   `json:"next_cursor,omitempty"`
	Notes    []string `json:"notes,omitempty"`
	Excluded string   `json:"excluded,omitempty"`
	Hint     string   `json:"hint,omitempty"`
}

// sessionBrief is a session in sessions' concise default: what an agent
// needs to pick a session and match it to presence (the full id, repo,
// branches, commit ids), with counts instead of lists and no reply text.
// --detail prints format.ConversationInfo with the whole digest instead.
type sessionBrief struct {
	SessionID string `json:"session_id"`
	// Address is the shortest unique prefix read takes, when it is not
	// the whole id.
	Address        string     `json:"address,omitempty"`
	Agent          string     `json:"agent"`
	User           string     `json:"user,omitempty"`
	Device         string     `json:"device,omitempty"`
	Repo           string     `json:"repo,omitempty"`
	Branches       []string   `json:"branches,omitempty"`
	Live           bool       `json:"live"`
	LastActivityAt *time.Time `json:"last_activity_at,omitempty"`
	Messages       int        `json:"messages"`
	Title          string     `json:"title,omitempty"`
	// Intent is the digest's intent when it is not the title.
	Intent        string   `json:"intent,omitempty"`
	ParentSession string   `json:"parent_session,omitempty"`
	Commits       []string `json:"commits,omitempty"`
	Files         int      `json:"files,omitempty"`
	Failed        int      `json:"failed,omitempty"`
}

// briefTitle is the most of a title the concise sessions answer prints.
const briefTitle = 160

func brief(c format.ConversationInfo) sessionBrief {
	b := sessionBrief{SessionID: c.SessionID, Address: c.Address, Agent: c.Agent, User: c.User, Repo: c.Repo, Branches: c.Branches,
		Live: c.Live, LastActivityAt: c.LastActivityAt, Messages: c.Messages, Title: format.ClipAround(c.Title, 0, briefTitle), ParentSession: c.ParentSession}
	if b.Repo == "" {
		b.Repo = c.Cwd
	}
	if b.Address == b.SessionID {
		b.Address = ""
	}
	if c.Device != "local" {
		b.Device = c.Device
	}
	if d := c.Digest; d != nil {
		if d.Intent != "" && d.Intent != c.Title {
			b.Intent = format.ClipAround(d.Intent, 0, briefTitle)
		}
		b.Commits, b.Files, b.Failed = d.Commits, len(d.FilesEdited), d.Failed
	}
	return b
}

// boundSessions is the sessions answer in JSON, within budget: whole
// sessions, the cursor after the last one kept. Each session is brief
// unless detail asks for the whole description.
func boundSessions(s *format.Sessions, budget int, mcp, detail bool) sessionsJSON {
	all := s.Sessions
	items := make([]any, len(all))
	for i, c := range all {
		if detail {
			items[i] = c
		} else {
			items[i] = brief(c)
		}
	}
	out := sessionsJSON{Kind: "sessions", Sessions: items, HasMore: s.HasMore || s.Next != "", Next: s.Next, Notes: s.Notes, Excluded: s.Excluded}
	if !fitsBudget(out, budget) && len(all) > 1 {
		n := most(len(all), func(k int) bool {
			c := out
			c.Sessions, c.HasMore, c.Next = items[:k], true, format.SessionCursor(all[k-1])
			return fitsBudget(c, room(budget))
		})
		if n < len(all) {
			out.Sessions, out.HasMore, out.Next = items[:n], true, format.SessionCursor(all[n-1])
			out.Hint = budgetHint(budget, arg(mcp, "--cursor", "cursor", out.Next))
			return out
		}
	}
	if out.HasMore {
		out.Hint = "pass next_cursor as " + arg(mcp, "--cursor", "cursor", "C") + " for the next page"
	}
	return out
}

// pageJSON is grep's and search's bounded JSON answer: format.Page (the
// --json shape) with its kind, and a hint when the budget cut the page.
type pageJSON struct {
	Kind string `json:"kind"` // "grep" or "search"
	*format.Page
	Hint string `json:"hint,omitempty"`
}

// readJSON is read's bounded JSON answer: format.Context with its kind.
type readJSON struct {
	Kind string `json:"kind"` // "read"
	*format.Context
	Hint string `json:"hint,omitempty"`
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

// boundPage is a grep or search page as bounded JSON, within
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

// boundRead is read's answer as bounded JSON, within budget: an
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
