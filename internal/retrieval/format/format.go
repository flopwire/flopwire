// Package format is the shape of Flopwire's retrieval tools (spec §7.3, §8):
// grep, search, sessions and read. The server API, the CLI and the MCP
// server share it, and so does the local index, so an agent sees one
// output whichever store answered.
//
//   - Filters is the query surface; Values and ParseFilters are its URL
//     form.
//   - GrepQuery, SearchQuery and ReadQuery are the requests; Page, Sessions
//     and Context the answers (MCP format:"json" and --json print them).
//   - Every hit, message and session carries an address,
//     SESSION/ORDINAL[:LINE] (address.go), that read accepts.
//   - WriteGrep, WriteSearch, WriteSessions and WriteRead render the
//     compact text form (render.go), with control characters made visible
//     (Clean).
package format

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/digest"
)

// Errors a retrieval backend returns; callers map them to 404 and 400.
var (
	ErrNotFound   = errors.New("retrieval: not found")
	ErrBadRequest = errors.New("retrieval: bad request")
	ErrForbidden  = errors.New("retrieval: forbidden")
)

// Filters narrow grep, search and sessions. The zero value is the default
// view: live rows (not superseded) on the active path, every subagent
// included, injected text (CLAUDE.md, AGENTS.md, system reminders) left out
// unless Kinds names it.
type Filters struct {
	Agent        string   `json:"agent,omitempty"`
	Repo         string   `json:"repo,omitempty"`   // an absolute path (the repo or a directory under it), a repo name, or a glob
	Device       string   `json:"device,omitempty"` // device id or name
	User         string   `json:"user,omitempty"`   // user id or email
	Kinds        []string `json:"kind,omitempty"`   // user, assistant, tool_call, tool_result, thinking, system, injected, agent_message
	ExcludeKinds []string `json:"exclude_kind,omitempty"`
	Tools        []string `json:"tool,omitempty"` // tool names (Bash, exec_command, ...), case-insensitive
	// Session scopes hits to one session (any unique prefix of its id)
	// and its subagents. "self" is resolved by the caller (the CLI and
	// the MCP server) to the calling agent's own session.
	Session string `json:"session,omitempty"`
	// Self marks Session as the caller's own session (--session self):
	// matched exactly, never as a prefix, and on the server only among
	// the caller's own sessions (Owner).
	Self bool `json:"self,omitempty"`
	// Owner is the calling user's id, which the server sets from the
	// credential for Self; it never travels.
	Owner string `json:"-"`
	// Branch keeps sessions that ran on a matching git branch: a name, or
	// a glob with * and ?.
	Branch string `json:"branch,omitempty"`
	// Sort orders the answer: newest or oldest (by message time; for
	// sessions by last activity) or relevance (search's rank; grep's
	// matches per message). "" is the tool's default (SortDefault).
	Sort  string    `json:"sort,omitempty"`
	Since time.Time `json:"since,omitzero"` // message time, inclusive
	Until time.Time `json:"until,omitzero"` // message time, exclusive

	ExcludeSubagents bool `json:"exclude_subagents,omitempty"`
	// ExcludeLive leaves out live sessions: active within LiveWindow, or
	// named in Live (sessions the caller's harness still holds open) with
	// their subagents.
	ExcludeLive       bool     `json:"exclude_live,omitempty"`
	Live              []string `json:"live,omitempty"`
	IncludeSuperseded bool     `json:"include_superseded,omitempty"`
	IncludeBranches   bool     `json:"include_branches,omitempty"`
	// Self-session exclusion (decision D4): the caller's own conversation
	// (id) or session (harness session id).
	ExcludeConversation string `json:"exclude_conversation,omitempty"`
	ExcludeSession      string `json:"exclude_session,omitempty"`

	Limit int `json:"limit,omitempty"`
}

// Values encodes the filters as URL query parameters.
func (f Filters) Values() url.Values {
	v := url.Values{}
	set := func(k, s string) {
		if s != "" {
			v.Set(k, s)
		}
	}
	flag := func(k string, b bool) {
		if b {
			v.Set(k, "true")
		}
	}
	set("agent", f.Agent)
	set("repo", f.Repo)
	set("device", f.Device)
	set("user", f.User)
	set("kind", strings.Join(f.Kinds, ","))
	set("exclude_kind", strings.Join(f.ExcludeKinds, ","))
	set("tool", strings.Join(f.Tools, ","))
	set("session", f.Session)
	set("branch", f.Branch)
	set("sort", f.Sort)
	if !f.Since.IsZero() {
		v.Set("since", f.Since.UTC().Format(time.RFC3339Nano))
	}
	if !f.Until.IsZero() {
		v.Set("until", f.Until.UTC().Format(time.RFC3339Nano))
	}
	flag("exclude_subagents", f.ExcludeSubagents)
	flag("exclude_live", f.ExcludeLive)
	flag("self", f.Self)
	set("live", strings.Join(f.Live, ","))
	flag("include_superseded", f.IncludeSuperseded)
	flag("include_branches", f.IncludeBranches)
	set("exclude_conversation", f.ExcludeConversation)
	set("exclude_session", f.ExcludeSession)
	if f.Limit > 0 {
		v.Set("limit", strconv.Itoa(f.Limit))
	}
	return v
}

// ParseFilters is the inverse of Values. Times accept RFC 3339, a date
// (2026-09-01) or a duration back from now ("24h", "7d").
func ParseFilters(v url.Values) (Filters, error) {
	f := Filters{Agent: v.Get("agent"), Repo: v.Get("repo"), Device: v.Get("device"), User: v.Get("user"), Session: v.Get("session"),
		Branch: v.Get("branch"), Sort: v.Get("sort"), Live: List(v.Get("live")),
		ExcludeConversation: v.Get("exclude_conversation"), ExcludeSession: v.Get("exclude_session"),
		Kinds: List(v.Get("kind")), ExcludeKinds: List(v.Get("exclude_kind")), Tools: List(v.Get("tool"))}
	var err error
	for _, p := range []struct {
		key string
		dst *time.Time
	}{{"since", &f.Since}, {"until", &f.Until}} {
		if s := v.Get(p.key); s != "" {
			if *p.dst, err = ParseTime(s, time.Now()); err != nil {
				return f, fmt.Errorf("%s: %w", p.key, err)
			}
		}
	}
	for _, p := range []struct {
		key string
		dst *bool
	}{{"exclude_subagents", &f.ExcludeSubagents}, {"exclude_live", &f.ExcludeLive}, {"self", &f.Self}, {"include_superseded", &f.IncludeSuperseded}, {"include_branches", &f.IncludeBranches}} {
		if s := v.Get(p.key); s != "" {
			if *p.dst, err = strconv.ParseBool(s); err != nil {
				return f, fmt.Errorf("%s: %w", p.key, err)
			}
		}
	}
	if s := v.Get("limit"); s != "" {
		if f.Limit, err = strconv.Atoi(s); err != nil || f.Limit < 0 {
			return f, fmt.Errorf("limit: bad value %q", s)
		}
	}
	switch f.Sort {
	case "", SortNewest, SortOldest, SortRelevance:
	default:
		return f, fmt.Errorf("sort: want newest, oldest or relevance, not %q", f.Sort)
	}
	return f, nil
}

// LiveWindow is how recently a session must have been active to count as
// live.
const LiveWindow = 10 * time.Minute

// Sort orders.
const (
	SortNewest    = "newest"
	SortOldest    = "oldest"
	SortRelevance = "relevance"
)

// SortFor returns the order a tool answers in: sort, or the tool's
// default (grep and sessions newest, search relevance). Sessions have no
// relevance order.
func SortFor(tool, sort string) (string, error) {
	switch {
	case sort == "" && tool == "search":
		return SortRelevance, nil
	case sort == "":
		return SortNewest, nil
	case sort == SortRelevance && tool == "sessions":
		return "", fmt.Errorf("%w: sessions sort by last activity: newest or oldest", ErrBadRequest)
	case sort == SortNewest, sort == SortOldest, sort == SortRelevance:
		return sort, nil
	}
	return "", fmt.Errorf("%w: sort: want newest, oldest or relevance, not %q", ErrBadRequest, sort)
}

// BranchMatch turns a --branch value into a LIKE pattern (backslash
// escapes): a name matches exactly, * and ? are wildcards.
func BranchMatch(branch string) string {
	var b strings.Builder
	for _, r := range branch {
		switch r {
		case '*':
			b.WriteByte('%')
		case '?':
			b.WriteByte('_')
		case '%', '_', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// List splits a comma list, dropping empty items.
func List(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ParseTime accepts RFC 3339, a date, the UTC minute hits print
// ("2026-09-23 10:00Z"), or a duration before now (Go durations, plus Nd
// for days and Nw for weeks).
func ParseTime(s string, now time.Time) (time.Time, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	if n := len(s); n > 1 && (s[n-1] == 'd' || s[n-1] == 'w') {
		if k, err := strconv.Atoi(s[:n-1]); err == nil && k >= 0 {
			days := k
			if s[n-1] == 'w' {
				days *= 7
			}
			return now.AddDate(0, 0, -days), nil
		}
	}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(stampLayout, s); err == nil {
		return t, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return t, fmt.Errorf("bad time %q: want 24h, 7d, 2026-09-01, \"2026-09-23 10:00Z\" (UTC) or RFC 3339", s)
	}
	return t, nil
}

// Provenance locates a row in its raw evidence: raw(source_id, generation,
// byte_offset, byte_len) returns the record's bytes.
type Provenance struct {
	SourceID   string `json:"source_id,omitempty"`
	Path       string `json:"path"`
	FileID     string `json:"file_id,omitempty"`
	Generation int64  `json:"generation"`
	LineNo     int64  `json:"line_no,omitempty"` // JSONL line, 1-based
	ByteOffset *int64 `json:"byte_offset,omitempty"`
	ByteLen    int64  `json:"byte_len,omitempty"`
	Locator    string `json:"locator,omitempty"` // non-JSONL sources: rowid, pointer
}

// Line is one line of a message's text: a matching line, or context
// around one.
type Line struct {
	N     int    `json:"n"` // 1-based line number within the message text
	Text  string `json:"text"`
	Match bool   `json:"match,omitempty"`
}

// Hit is one grep or search result: a message.
type Hit struct {
	// Address is SESSION/ORDINAL, the message's address for read.
	Address        string `json:"address"`
	MessageID      string `json:"message_id"`
	ConversationID string `json:"conversation_id"`
	Agent          string `json:"agent"`
	SessionID      string `json:"session_id"`
	Ordinal        int64  `json:"ordinal"`
	Title          string `json:"title,omitempty"`
	Repo           string `json:"repo,omitempty"`
	Device         string `json:"device,omitempty"`
	User           string `json:"user,omitempty"`
	// Branches are the session's git branches, first seen first.
	Branches []string `json:"branches,omitempty"`
	Kind     string   `json:"kind"`
	ToolName string   `json:"tool_name,omitempty"`
	// IsError marks a tool call or result the harness recorded as failed.
	IsError bool       `json:"is_error,omitempty"`
	TS      *time.Time `json:"ts,omitempty"`
	Score   float64    `json:"score,omitempty"` // search: bm25 (local) or ts_rank_cd (server)
	// Snippet is an excerpt around the first match (search). TextLine is
	// the line it starts on, 1-based.
	Snippet  string `json:"snippet,omitempty"`
	TextLine int    `json:"text_line,omitempty"`
	// Lines are grep's matching lines, with context lines around them.
	// MoreLines counts matching lines left out past the per-hit cap.
	Lines      []Line `json:"lines,omitempty"`
	MoreLines  int    `json:"more_lines,omitempty"`
	Superseded bool   `json:"superseded,omitempty"`
	OffPath    bool   `json:"off_active_path,omitempty"`
	// Copies counts other hits with identical text folded into this one.
	Copies     int        `json:"copies,omitempty"`
	Provenance Provenance `json:"provenance"`
}

// Message is one row as read returns it.
type Message struct {
	Address    string     `json:"address"`
	ID         string     `json:"id"`
	NativeID   string     `json:"native_id,omitempty"`
	Ordinal    int64      `json:"ordinal"`
	Kind       string     `json:"kind"`
	Role       string     `json:"role,omitempty"`
	ToolName   string     `json:"tool_name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	IsError    bool       `json:"is_error,omitempty"`
	TS         *time.Time `json:"ts,omitempty"`
	// Text is the stored text, or the excerpt of it that read asked for:
	// lines LineFrom..LineTo of Lines, cut at the character budget when
	// Clipped. TextLen is the original length in bytes; StoredLen, when
	// set, is the stored length of a local row the index capped.
	Text       string     `json:"text"`
	TextLen    int        `json:"text_len"`
	StoredLen  int        `json:"stored_len,omitempty"`
	Lines      int        `json:"lines,omitempty"`
	LineFrom   int        `json:"line_from,omitempty"`
	LineTo     int        `json:"line_to,omitempty"`
	Clipped    bool       `json:"clipped,omitempty"`
	Version    int        `json:"version,omitempty"`
	Superseded bool       `json:"superseded,omitempty"`
	OffPath    bool       `json:"off_active_path,omitempty"`
	Provenance Provenance `json:"provenance"`
}

// ConversationInfo describes a conversation (a session).
type ConversationInfo struct {
	// Address is the session's shortest unique id prefix; read accepts it.
	Address              string     `json:"address"`
	ID                   string     `json:"id"`
	Agent                string     `json:"agent"`
	SessionID            string     `json:"session_id"`
	Title                string     `json:"title,omitempty"`
	Cwd                  string     `json:"cwd,omitempty"`
	Repo                 string     `json:"repo,omitempty"`
	Device               string     `json:"device,omitempty"`
	User                 string     `json:"user,omitempty"`
	StartedAt            *time.Time `json:"started_at,omitempty"`
	LastActivityAt       *time.Time `json:"last_activity_at,omitempty"`
	ParentConversationID string     `json:"parent_conversation_id,omitempty"`
	ParentSession        string     `json:"parent_session,omitempty"` // the parent's address
	SpawnedByMessageID   string     `json:"spawned_by_message_id,omitempty"`
	Depth                int        `json:"depth,omitempty"`
	Messages             int        `json:"messages,omitempty"` // live rows on the active path
	Hits                 int        `json:"hits,omitempty"`     // grep -l/-c: matching messages
	// Branches are the git branches the session ran on, first seen
	// first.
	Branches []string `json:"branches,omitempty"`
	// Digest summarizes what the session did (internal/digest,
	// docs/search.md).
	Digest *digest.Digest `json:"digest,omitempty"`
	// Live marks a session active within LiveWindow or still open in its
	// harness (the CLI and MCP server set it).
	Live bool `json:"live,omitempty"`
}

// ParseDigest decodes a stored digest for output, without the fold's
// bookkeeping; nil when there is none.
func ParseDigest(b []byte) *digest.Digest {
	if len(b) == 0 {
		return nil
	}
	d := digest.Parse(b)
	d.State = nil
	return d
}

// Context is read's answer: a focus message with its neighbours in
// conversation order.
type Context struct {
	Conversation ConversationInfo `json:"conversation"`
	Focus        string           `json:"focus"`          // the focus message id
	Line         int              `json:"line,omitempty"` // the addressed line of the focus
	Messages     []Message        `json:"messages"`
	// MoreBefore and MoreAfter report messages beyond the ones returned.
	MoreBefore bool `json:"more_before,omitempty"`
	MoreAfter  bool `json:"more_after,omitempty"`
	// Outline is read --outline's answer (Messages is then empty): one
	// page of entries. OutlineMore says entries follow; OutlineNext is the
	// cursor that reads them (ReadQuery.Cursor). An outline answer's
	// Outline is never nil, so an empty page (omitzero keeps it on the
	// wire) still renders as an outline.
	Outline     []OutlineEntry `json:"outline,omitzero"`
	OutlineMore bool           `json:"outline_more,omitempty"`
	OutlineNext string         `json:"outline_next,omitempty"`
}

// OutlineEntry is one line of a session's outline: a user prompt, or a
// tool call as tool(args summary).
type OutlineEntry struct {
	Address string `json:"address"`
	// ID and Ordinal are the message's id and ordinal, the entry's place
	// in the outline's order (OutlineCursor).
	ID      string     `json:"id"`
	Ordinal int64      `json:"ordinal"`
	TS      *time.Time `json:"ts,omitempty"`
	Kind    string     `json:"kind"` // user or tool_call
	Tool    string     `json:"tool,omitempty"`
	// Text is the prompt (trimmed, one line) or the call's args summary.
	Text  string `json:"text"`
	Error bool   `json:"error,omitempty"`
	// Subagents are the addresses of the sessions the call spawned.
	Subagents []string `json:"subagents,omitempty"`
}

// OutlinePage checks an outline request's limit and returns the page
// size.
func OutlinePage(q ReadQuery) (int, error) {
	switch {
	case q.Limit <= 0:
		return OutlineLimit, nil
	case q.Limit > MaxOutlineLimit:
		return 0, fmt.Errorf("%w: outline limit is at most %d", ErrBadRequest, MaxOutlineLimit)
	}
	return q.Limit, nil
}

// NewOutlineEntry renders a user or tool_call row as an outline entry:
// the prompt trimmed to one line, or the call's args summary (paths under
// root relative to it).
func NewOutlineEntry(addr, id string, ordinal int64, ts *time.Time, kind, tool, text, root string) OutlineEntry {
	e := OutlineEntry{Address: addr, ID: id, Ordinal: ordinal, TS: ts, Kind: kind, Tool: tool}
	if kind == "tool_call" {
		e.Text = digest.CallSummary(tool, text, root, OutlineArgs)
	} else {
		e.Text = ClipAround(strings.Join(strings.Fields(text), " "), 0, OutlinePrompt)
	}
	return e
}

// Outline limits: entries per page by default and at most, and the
// bytes of a prompt or an args summary.
const (
	OutlineLimit    = 200
	MaxOutlineLimit = 2000
	OutlinePrompt   = 200
	OutlineArgs     = 120
)

// Page is a grep or search answer. A page cut short by the query budget
// is still an answer, never an error: Truncated is set and Reason says
// what was checked.
type Page struct {
	Hits []Hit `json:"hits"`
	// Sessions is grep's -l/-c answer: sessions with matches, newest hit
	// first, Hits set on each.
	Sessions []ConversationInfo `json:"sessions,omitempty"`
	// Total and TotalSessions count the matching messages and their
	// sessions (grep); they are lower bounds unless Exact.
	Total         int  `json:"total,omitempty"`
	TotalSessions int  `json:"total_sessions,omitempty"`
	Exact         bool `json:"exact,omitempty"`
	Offset        int  `json:"offset,omitempty"`
	// Next is the offset of the next page, 0 when there is none.
	Next      int    `json:"next_offset,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Reason    string `json:"reason,omitempty"`
	// Notes explain the answer: an unindexed scan, the rank cap (D2), an
	// any-term retry, the excluded calling session (D4).
	Notes []string `json:"notes,omitempty"`
	// Excluded names the calling session left out (D4), and how to
	// include it.
	Excluded string `json:"excluded,omitempty"`
	// SessionInfo describes the sessions of the content hits, for the
	// grouped layout's headers.
	SessionInfo []ConversationInfo `json:"session_info,omitempty"`
}

// Sessions is the sessions answer: one page of conversations, newest
// activity first. HasMore says sessions follow; Next is the cursor that
// reads them (see SessionCursor).
type Sessions struct {
	Sessions []ConversationInfo `json:"sessions"`
	HasMore  bool               `json:"has_more,omitempty"`
	Next     string             `json:"next_cursor,omitempty"`
	Notes    []string           `json:"notes,omitempty"`
	Excluded string             `json:"excluded,omitempty"`
}

// Attribution names whose evidence a raw read returns: the user and
// device that uploaded it, the harness, the session and the repository.
type Attribution struct {
	User       string `json:"user,omitempty"`
	Device     string `json:"device,omitempty"`
	Agent      string `json:"agent,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	Repo       string `json:"repo,omitempty"`
	Path       string `json:"path,omitempty"`
	SourceID   string `json:"source_id,omitempty"`
	Generation int64  `json:"generation"`
}

// Grep output modes.
const (
	ModeContent  = "content"  // matching lines (default)
	ModeSessions = "sessions" // -l: sessions with matches
	ModeCount    = "count"    // -c: matching messages per session
)

// GrepQuery is a grep request. Pattern is an RE2 regular expression
// (multi-line: ^ and $ match at line breaks), or a literal when Fixed.
type GrepQuery struct {
	Pattern       string `json:"pattern"`
	Fixed         bool   `json:"fixed,omitempty"`
	CaseSensitive bool   `json:"case_sensitive,omitempty"`
	Mode          string `json:"mode,omitempty"`
	Offset        int    `json:"offset,omitempty"`
	Limit         int    `json:"limit,omitempty"`           // hits (content) or sessions (-l/-c) per page; default 20
	MaxPerSession int    `json:"max_per_session,omitempty"` // -m: matching messages counted per session
	Before        int    `json:"before,omitempty"`          // lines of context within the message
	After         int    `json:"after,omitempty"`
	// OnlyMatching (-o) keeps only the matched text of each match.
	OnlyMatching bool `json:"only_matching,omitempty"`
	// Multiline (-U) lets . match a newline, so a match may span lines
	// (never messages); a hit shows every line of the match.
	Multiline bool `json:"multiline,omitempty"`
	// Sort is the backend's copy of Filters.Sort (it travels with the
	// filters).
	Sort string `json:"-"`
	// Timeout is the query budget (D8); 0 is the default 10s, at most 60s.
	Timeout time.Duration `json:"timeout,omitempty"`
}

// Values encodes q as URL query parameters (the filters go separately).
func (q GrepQuery) Values(v url.Values) url.Values {
	v.Set("pattern", q.Pattern)
	v.Set("regex", strconv.FormatBool(!q.Fixed))
	v.Set("case_sensitive", strconv.FormatBool(q.CaseSensitive))
	setNum(v, "offset", q.Offset)
	setNum(v, "limit", q.Limit)
	setNum(v, "max_per_session", q.MaxPerSession)
	setNum(v, "before", q.Before)
	setNum(v, "after", q.After)
	if q.Mode != "" && q.Mode != ModeContent {
		v.Set("mode", q.Mode)
	}
	if q.OnlyMatching {
		v.Set("only_matching", "true")
	}
	if q.Multiline {
		v.Set("multiline", "true")
	}
	if q.Timeout > 0 {
		v.Set("timeout", q.Timeout.String())
	}
	return v
}

// ParseGrepQuery is the inverse of Values.
func ParseGrepQuery(v url.Values) (GrepQuery, error) {
	q := GrepQuery{Pattern: v.Get("pattern"), Mode: v.Get("mode")}
	regex, _ := strconv.ParseBool(v.Get("regex"))
	q.Fixed = !regex
	q.CaseSensitive, _ = strconv.ParseBool(v.Get("case_sensitive"))
	q.OnlyMatching, _ = strconv.ParseBool(v.Get("only_matching"))
	q.Multiline, _ = strconv.ParseBool(v.Get("multiline"))
	var err error
	for _, p := range []struct {
		k string
		d *int
	}{{"offset", &q.Offset}, {"limit", &q.Limit}, {"max_per_session", &q.MaxPerSession}, {"before", &q.Before}, {"after", &q.After}} {
		if *p.d, err = num(v, p.k); err != nil {
			return q, err
		}
	}
	switch q.Mode {
	case "", ModeContent, ModeSessions, ModeCount:
	default:
		return q, fmt.Errorf("mode: want content, sessions or count, not %q", q.Mode)
	}
	return q, nil
}

// SearchQuery is a ranked search request.
type SearchQuery struct {
	Query   string        `json:"query"`
	Offset  int           `json:"offset,omitempty"`
	Limit   int           `json:"limit,omitempty"` // default 20
	Timeout time.Duration `json:"timeout,omitempty"`
}

// ReadQuery is a read request.
type ReadQuery struct {
	Address    string `json:"address"`
	Before     int    `json:"before,omitempty"` // messages before the focus
	After      int    `json:"after,omitempty"`
	MaxChars   int    `json:"max_chars,omitempty"`   // text per focus message; neighbours get a quarter; default 4000
	LineOffset int    `json:"line_offset,omitempty"` // first line of the focus text to show
	// Outline reads the session's skeleton instead (Context.Outline):
	// Limit entries (default OutlineLimit) after Cursor (from the start
	// when empty).
	Outline bool   `json:"outline,omitempty"`
	Cursor  string `json:"cursor,omitempty"`
	Limit   int    `json:"limit,omitempty"`
	// Self marks Address as the caller's own session id (read self):
	// matched exactly, and on the server only among the caller's own
	// sessions (Filters.Owner).
	Self bool `json:"self,omitempty"`
}

// DefaultMaxChars is read's default character budget for the focus.
const DefaultMaxChars = 4000

// Values encodes q as URL query parameters.
func (q ReadQuery) Values(v url.Values) url.Values {
	v.Set("address", q.Address)
	setNum(v, "before", q.Before)
	setNum(v, "after", q.After)
	setNum(v, "max_chars", q.MaxChars)
	setNum(v, "line_offset", q.LineOffset)
	if q.Self {
		v.Set("self", "true")
	}
	if q.Outline {
		v.Set("outline", "true")
		if q.Cursor != "" {
			v.Set("cursor", q.Cursor)
		}
		setNum(v, "limit", q.Limit)
	}
	return v
}

// ParseReadQuery is the inverse of Values.
func ParseReadQuery(v url.Values) (ReadQuery, error) {
	q := ReadQuery{Address: v.Get("address")}
	q.Outline, _ = strconv.ParseBool(v.Get("outline"))
	q.Self, _ = strconv.ParseBool(v.Get("self"))
	var err error
	for _, p := range []struct {
		k string
		d *int
	}{{"before", &q.Before}, {"after", &q.After}, {"max_chars", &q.MaxChars}, {"line_offset", &q.LineOffset}} {
		if *p.d, err = num(v, p.k); err != nil {
			return q, err
		}
	}
	if q.Outline { // cursor and limit page the outline
		q.Cursor = v.Get("cursor")
		if q.Limit, err = num(v, "limit"); err != nil {
			return q, err
		}
	}
	return q, nil
}

func setNum(v url.Values, k string, n int) {
	if n != 0 {
		v.Set(k, strconv.Itoa(n))
	}
}

func num(v url.Values, k string) (int, error) {
	s := v.Get(k)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s: bad value %q", k, s)
	}
	return n, nil
}

// RedactRequest asks to redact a message after the fact
// (notes/redaction.md). Address is a message address, optionally ending in
// :L1-L2 (lines of its text as read numbers them).
type RedactRequest struct {
	Address   string `json:"address"`
	AllCopies bool   `json:"all_copies,omitempty"`
}

// RedactResult reports a redaction: rows rewritten, archived chunks and
// provisional tails rewritten, records whose text could not be located in
// the raw bytes (masked whole instead), and the job purging old chunks.
type RedactResult struct {
	ID        string `json:"id"`
	Messages  int    `json:"messages"`
	Chunks    int    `json:"chunks"`
	Tails     int    `json:"tails"`
	Fallbacks int    `json:"fallbacks,omitempty"`
	JobID     string `json:"job_id,omitempty"`
	// Skipped lists other users' byte-identical copies left unredacted:
	// they stored the record first, so only an admin may redact them.
	Skipped []SkippedCopies `json:"skipped,omitempty"`
}

// SkippedCopies are the rows of one source a redaction left out.
type SkippedCopies struct {
	SourceID string `json:"source_id"`
	User     string `json:"user"`
	Device   string `json:"device"`
	Messages int    `json:"messages"`
}
