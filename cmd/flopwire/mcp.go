package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/local"
)

// mcp serves the retrieval tools over MCP stdio: the local index by
// default, the team server with --server.
func mcp(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	server := fs.Bool("server", false, "query the team server instead of the local index")
	index := fs.String("index", "", "local index path (default $FLOPWIRE_INDEX or the user cache dir)")
	call := fs.String("call", "", "run this one tool with the JSON arguments on stdin, print its answer and exit (the opencode plugin's tools)")
	socket := fs.String("socket", "", "device agent control socket for the messaging tools (default <config dir>/agent.sock)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r, err := openRetriever(*server, *index)
	var noIndex *noIndexError
	if err != nil && !*server && !errors.As(err, &noIndex) && !errors.Is(err, localindex.ErrSyncOnly) {
		// The index exists but does not open: another process is creating
		// or rebuilding it (a second flopwire mcp, or the agent), or it is
		// damaged. Serve anyway, as below: each retrieval call opens it
		// again and reports why it cannot.
		path := *index
		if path == "" {
			path = local.IndexPath()
		}
		det := local.NewDetector()
		lazy := &lazyIndexBackend{path: path}
		r, err = &retriever{backend: lazy, caller: det.Detect, live: det.Live, close: lazy.Close}, nil
	}
	if errors.As(err, &noIndex) {
		// The device agent has never run. Serve anyway, so the harness
		// does not mark the server failed (Claude Code then skips it for
		// 15 minutes): create the empty index as the agent would and
		// serve it, so the retrieval tools answer empty with a hint until
		// the agent fills it. The messaging tools, which talk to the
		// agent, work as soon as it runs.
		path := noIndex.path
		if cerr := createEmptyIndex(path); cerr == nil {
			r, err = openRetriever(false, path)
		}
		if err != nil && !errors.Is(err, localindex.ErrSyncOnly) {
			// The agent holds the index (it is creating it now), or the
			// create failed: open it on the first call that finds it.
			det := local.NewDetector()
			lazy := &lazyIndexBackend{path: path}
			r, err = &retriever{backend: lazy, caller: det.Detect, live: det.Live, close: lazy.Close}, nil
		}
	}
	if errors.Is(err, localindex.ErrSyncOnly) {
		// Start anyway, so the client sees why: the instructions say so
		// and every tool answers with the same error.
		r, err = &retriever{backend: syncOnlyBackend{}, caller: local.NewDetector().Detect, close: func() error { return nil },
			instructions: mcpSyncOnly + "\n\n" + mcpInstructions}, nil
	}
	if err != nil {
		return err
	}
	defer r.close()
	if lb, ok := r.backend.(*local.Backend); ok {
		r.indexHint = emptyIndexHint(lb.Store)
	}
	r.caller = cachedCaller(r.caller, 10*time.Second)
	r.underCodex = local.NewDetector().UnderCodex()
	r.busSocket = *socket
	if r.busSocket == "" {
		if r.busSocket, err = defaultSocket(); err != nil {
			return err
		}
	}
	if *call != "" {
		return mcpCallOnce(ctx, r, *call, os.Stdin, os.Stdout)
	}
	return serveMCP(ctx, r, os.Stdin, os.Stdout)
}

// createEmptyIndex creates the local index at path as the agent would: in
// the client config's mode, with the agent's defaults. It holds the index
// lock only while it creates the schema; when the agent holds the lock it
// fails with *localindex.LockedError, and the agent creates the index.
func createEmptyIndex(path string) error {
	syncOnly := false
	cc, ccErr := client.Load()
	if err := resolveSyncOnly(flag.NewFlagSet("mcp", flag.ContinueOnError), &syncOnly, cc, ccErr); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	s, err := localindex.Open(path, localindex.Options{SyncOnly: syncOnly})
	if err != nil {
		return err
	}
	return s.Close()
}

// mcpEmptyIndex is the hint a retrieval tool adds while the local index
// holds no transcript.
const mcpEmptyIndex = "Flopwire's local index is empty: the device agent has not indexed this device's transcripts yet, so nothing can match. Start it with `flopwire agent run` (its first pass takes a few minutes), then call again. The messaging tools work once the agent runs."

// emptyIndexHint returns mcpEmptyIndex while s holds no source. Once the
// agent has indexed a transcript it stops asking.
func emptyIndexHint(s *localindex.Store) func(context.Context) string {
	var filled atomic.Bool
	return func(ctx context.Context) string {
		if filled.Load() {
			return ""
		}
		ok, err := s.HasSources(ctx)
		if err != nil || ok {
			filled.Store(ok)
			return ""
		}
		return mcpEmptyIndex
	}
}

// cachedCaller remembers the detected session for ttl: detection may run
// lsof, and a session changes only on /clear or restart.
func cachedCaller(detect func(context.Context) (local.Caller, bool), ttl time.Duration) func(context.Context) (local.Caller, bool) {
	var mu sync.Mutex
	var c local.Caller
	var ok bool
	var at time.Time
	return func(ctx context.Context) (local.Caller, bool) {
		mu.Lock()
		defer mu.Unlock()
		if at.IsZero() || time.Since(at) > ttl {
			c, ok = detect(ctx)
			at = time.Now()
		}
		return c, ok
	}
}

// MCP limits: a request line may be at most maxLine bytes (a longer one
// gets a JSON-RPC error and the server reads on), and at most
// maxConcurrent tool calls run at once (the rest wait their turn).
const (
	maxLine       = 4 << 20
	maxConcurrent = 8
)

// mcpSyncOnly heads the instructions on a sync-only device.
const mcpSyncOnly = `This device is sync-only: it uploads transcripts but keeps no local index, so these tools answer with an error. Configure the Flopwire MCP server with --server (flopwire mcp --server) to search the team server instead.`

// syncOnlyBackend answers every call on a sync-only device with
// localindex.ErrSyncOnly.
type syncOnlyBackend struct{}

func (syncOnlyBackend) Grep(context.Context, format.GrepQuery, format.Filters) (*format.Page, error) {
	return nil, localindex.ErrSyncOnly
}
func (syncOnlyBackend) Search(context.Context, format.SearchQuery, format.Filters) (*format.Page, error) {
	return nil, localindex.ErrSyncOnly
}
func (syncOnlyBackend) Sessions(context.Context, string, string, format.Filters) (*format.Sessions, error) {
	return nil, localindex.ErrSyncOnly
}
func (syncOnlyBackend) Read(context.Context, format.ReadQuery, format.Filters) (*format.Context, error) {
	return nil, localindex.ErrSyncOnly
}
func (syncOnlyBackend) RawAt(context.Context, string) ([]byte, error) {
	return nil, localindex.ErrSyncOnly
}
func (syncOnlyBackend) Raw(context.Context, string, int64, int64, int64) ([]byte, error) {
	return nil, localindex.ErrSyncOnly
}

// lazyIndexBackend is the local index for an MCP server that started
// before the index existed. Each call opens the index if it now exists;
// until then every call fails with errNoIndexYet's text.
type lazyIndexBackend struct {
	path string
	mu   sync.Mutex
	lb   *local.Backend
}

func (b *lazyIndexBackend) open() (backend, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lb != nil {
		return b.lb, nil
	}
	if _, err := os.Stat(b.path); err != nil {
		return nil, fmt.Errorf("no index yet: start the device agent with `flopwire agent run` (it builds the index at %s), then call again; the messaging tools work once the agent runs", b.path)
	}
	lb, err := openLocalBackend(b.path)
	if err != nil {
		return nil, err
	}
	b.lb = lb
	return lb, nil
}

// Close closes the index if a call opened it.
func (b *lazyIndexBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lb == nil {
		return nil
	}
	return b.lb.Store.Close()
}

func (b *lazyIndexBackend) Grep(ctx context.Context, q format.GrepQuery, f format.Filters) (*format.Page, error) {
	be, err := b.open()
	if err != nil {
		return nil, err
	}
	return be.Grep(ctx, q, f)
}
func (b *lazyIndexBackend) Search(ctx context.Context, q format.SearchQuery, f format.Filters) (*format.Page, error) {
	be, err := b.open()
	if err != nil {
		return nil, err
	}
	return be.Search(ctx, q, f)
}
func (b *lazyIndexBackend) Sessions(ctx context.Context, glob, cursor string, f format.Filters) (*format.Sessions, error) {
	be, err := b.open()
	if err != nil {
		return nil, err
	}
	return be.Sessions(ctx, glob, cursor, f)
}
func (b *lazyIndexBackend) Read(ctx context.Context, q format.ReadQuery, f format.Filters) (*format.Context, error) {
	be, err := b.open()
	if err != nil {
		return nil, err
	}
	return be.Read(ctx, q, f)
}
func (b *lazyIndexBackend) RawAt(ctx context.Context, address string) ([]byte, error) {
	be, err := b.open()
	if err != nil {
		return nil, err
	}
	return be.RawAt(ctx, address)
}
func (b *lazyIndexBackend) Raw(ctx context.Context, sourceID string, generation, offset, length int64) ([]byte, error) {
	be, err := b.open()
	if err != nil {
		return nil, err
	}
	return be.Raw(ctx, sourceID, generation, offset, length)
}

// mcpInstructions is the server's instructions: the workflow, the address
// form, how to read the output, and the filters every tool shares.
const mcpInstructions = `Flopwire searches past coding-agent transcripts (Claude Code, Codex, Devin, opencode): prompts, replies, tool calls and their output, across sessions and repos. Use it to find what was done, decided, run or seen before.

Workflow: flopwire_grep for exact strings and regexes (like rg: error text, identifiers, commands, paths); flopwire_search for fuzzy natural-language questions; flopwire_sessions to list sessions by repo, agent or time; then flopwire_read on any address a result prints to see the full message and its neighbours. Hits are excerpts: read before you rely on one. If the hits don't answer the question, say the transcripts don't hold it rather than guess.

Layout: grep and search group hits under one header per session: "## SESSION agent=claude ended=DATE (or live=4m) repo=NAME branch=B commits=N failed=N intent=\"...\""; a value with a space, quote or = is a JSON string. Under it each hit is "ORDINAL:LINE kind/tool: text"; its address for flopwire_read is SESSION/ORDINAL:LINE. no_heading=true prints one line per hit with the full address instead. flopwire_read's header has the same form. flopwire_read address=SESSION outline=true shows a session's digest and skeleton (every prompt, every tool call as tool(args)) without tool output: read it before reading messages one by one.

Addresses: flopwire_read accepts SESSION/ORDINAL[:LINE], SESSION (read from its start), a message id, a transcript /path.jsonl:LINE, or "self" (your own session). Ordinals are stable ids, not positions: use messages_before/messages_after to step through a session.

grep, search and read answer text (format="json": compact JSON); flopwire_sessions answers JSON (format="text": rows). Lines in [brackets] before the hits qualify them (a partial scan, an any-term retry); the footer after them gives totals and the exact arguments of the next page. Times are UTC. An answer stops at about 24000 bytes and the footer says where to go on. A query stops after 10s by default (timeout, up to 60s) and returns what it found with a note, never an error. Your own session is left out unless include_self is true; session="self" searches only your own session. A session is live while active in the last 10 minutes or open in its harness; exclude_live leaves live sessions out.

Shared filters (grep, search; sessions takes those that apply): agent, repo, branch, since, until, kind, exclude_kind, tool, session, device, user, exclude_subagents, exclude_live, include_superseded, include_branches, include_self, sort, limit (default 20, max 500), offset (grep, search) or cursor (sessions: its next_cursor).

` + mcpBusInstructions

func prop(typ, desc string) map[string]any {
	p := map[string]any{"type": typ}
	if desc != "" {
		p["description"] = desc
	}
	return p
}

// filterDesc describes the shared filters, briefly: a client may drop the
// server instructions, and the schema alone must say what a value looks
// like.
var filterDesc = map[string]string{
	"agent":              "claude, codex, devin or opencode; a comma list",
	"repo":               `"." (this repo: every checkout and worktree of it), /abs/path, a repo name or owner/name, or a glob like "team*"`,
	"since":              `"7d", "24h", "2026-09-01", "2026-09-23 10:00Z" (the form hits print) or RFC 3339; times are UTC`,
	"until":              `exclusive; same forms as since`,
	"kind":               "comma list of user, assistant, tool_call, tool_result, thinking, system, agent_message, injected (injected CLAUDE.md/AGENTS.md text is hidden unless named)",
	"exclude_kind":       "comma list of kinds to leave out",
	"tool":               "tool name, e.g. Bash, Edit, exec_command; a comma list",
	"session":            `only this session (any unique prefix of its id, as printed) and its subagents; "self" is your own session`,
	"branch":             `only sessions that ran on this git branch; * and ? are wildcards ("feat/*")`,
	"sort":               "newest or oldest (message time; sessions: last activity), or relevance (search's default rank; grep: most matches per message first). Default: grep and sessions newest, search relevance",
	"exclude_live":       "leave out live sessions: active in the last 10 minutes, or still open in their harness (with their subagents)",
	"no_heading":         "print one line per hit with its full address and attribution instead of grouping hits under a header per session",
	"device":             "device name or id",
	"user":               "user email or id (team server only)",
	"exclude_subagents":  "leave out subagent sessions",
	"include_superseded": "include rows a later version of the transcript replaced",
	"include_branches":   "include rows off the active conversation path (rewinds, forks)",
	"include_self":       "include your own session, left out by default",
	"limit":              "hits (sessions for flopwire_sessions) per page; default 20, max 500",
	"offset":             "skip this many; the footer prints the next offset",
	"cursor":             "where the next page starts: next_cursor of the previous answer",
	"format":             "text (default) or json (compact)",
	"detail":             "each session's whole description and digest (files, tools, tokens, last reply) instead of the brief row",
}

// formatProp is a retrieval tool's format argument: sessions answers JSON
// by default, as the message bus tools do; the others text.
func formatProp(verb string) map[string]any {
	if verb == "sessions" {
		return map[string]any{"type": "string", "enum": []string{"json", "text"}, "description": "json (default): compact JSON with named fields; text: a readable form"}
	}
	return map[string]any{"type": "string", "enum": []string{"text", "json"}, "description": filterDesc["format"]}
}

// filterProps are the shared filter properties of a verb.
func filterProps(verb string) map[string]any {
	names := []string{"limit", "include_self", "agent", "repo", "branch", "since", "until", "device", "user", "exclude_subagents", "exclude_live", "sort"}
	if verb == "sessions" {
		names = append(names, "cursor", "detail")
	} else {
		names = append(names, "offset", "kind", "exclude_kind", "tool", "session", "include_superseded", "include_branches", "no_heading")
	}
	props := map[string]any{"format": formatProp(verb)}
	for _, n := range names {
		typ := "string"
		switch n {
		case "limit", "offset":
			typ = "integer"
		case "include_self", "exclude_subagents", "include_superseded", "include_branches", "exclude_live", "no_heading", "detail":
			typ = "boolean"
		}
		props[n] = prop(typ, filterDesc[n])
	}
	sorts := []string{"newest", "oldest", "relevance"}
	if verb == "sessions" {
		sorts = sorts[:2]
	}
	props["sort"] = map[string]any{"type": "string", "enum": sorts, "description": filterDesc["sort"]}
	if verb != "sessions" {
		props["timeout"] = prop("number", "seconds, default 10, up to 60")
	}
	return props
}

func mcpTool(name, title, desc, verb string, own map[string]any, required ...string) any {
	props := map[string]any{}
	if verb != "read" {
		props = filterProps(verb)
	} else {
		props["format"] = formatProp(verb)
		props["include_superseded"], props["include_branches"] = prop("boolean", filterDesc["include_superseded"]), prop("boolean", filterDesc["include_branches"])
	}
	for k, v := range own {
		props[k] = v
	}
	schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	// Every tool only reads a local index (or the team server's), so a
	// client may run them without asking and in parallel.
	ann := map[string]any{"title": title, "readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}
	return map[string]any{"name": name, "title": title, "description": desc, "inputSchema": schema, "annotations": ann}
}

// mcpTools describes the four retrieval tools and the three message bus
// tools (mcp_bus.go).
func mcpTools() []any {
	return append([]any{
		mcpTool("flopwire_grep", "Grep transcripts", "Grep past coding-agent transcripts, like rg: an RE2 regex (smart case) or a literal (fixed_strings). Use it for exact text: an error message, an identifier, a command, a path, a config key. Hits are grouped under a header per session (## SESSION agent=A ended=DATE repo=R branch=B … intent=\"…\"); each matching line prints as ORDINAL:LINE kind/tool: text, newest first (sort=oldest|relevance), cut to 300 bytes around the match, identical messages shown once (+N copies), then a totals footer with the next offset. The address for flopwire_read is SESSION/ORDINAL:LINE. output_mode=sessions lists the sessions with matches instead. For a fuzzy question use flopwire_search.", "grep",
			map[string]any{
				"pattern":         prop("string", "RE2 regex; ^ and $ match at line breaks; needs a run of 3 letters or digits every match contains"),
				"fixed_strings":   prop("boolean", "pattern is a literal string"),
				"ignore_case":     prop("boolean", "default: smart case (case-sensitive only when the pattern has an uppercase letter)"),
				"case_sensitive":  prop("boolean", "match case even without an uppercase letter"),
				"word":            prop("boolean", "whole words only"),
				"output_mode":     map[string]any{"type": "string", "enum": []string{"content", "sessions", "count"}, "description": "content (default): matching lines; sessions: one line per session with matches; count: hits per session"},
				"before":          prop("integer", "lines of context before each match, within the message"),
				"after":           prop("integer", "lines of context after each match, within the message"),
				"context":         prop("integer", "lines of context both sides"),
				"max_per_session": prop("integer", "at most this many hits per session"),
				"only_matching":   prop("boolean", "print only the matched text of each match (like rg -o)"),
				"multiline":       prop("boolean", "let . match newlines so a match may span lines of one message (never messages); the hit shows every line of the match"),
			}, "pattern"),
		mcpTool("flopwire_search", "Search transcripts", "Ranked (BM25) search of past coding-agent transcripts for fuzzy or natural-language questions: what was decided, why something failed, how a thing was done. Quoted phrases must match. Hits are grouped under a header line per session (as flopwire_grep's); each prints as ORDINAL:LINE kind/tool: snippet, best first (sort=newest|oldest orders by time); the address for flopwire_read is SESSION/ORDINAL:LINE. When no message has every word it ranks messages with any of them and says so first: those hits may be unrelated, so check them with flopwire_read. For exact text use flopwire_grep.", "search",
			map[string]any{"query": prop("string", "words and \"quoted phrases\"")}, "query"),
		mcpTool("flopwire_sessions", "List sessions", `List past coding-agent sessions, newest activity first, filtered by repo, branch, agent, time or a glob, as JSON: {"kind":"sessions","sessions":[{"session_id" (full),"agent","user","repo","branches","live","last_activity_at","messages","title","commits","files","failed",…}],"has_more","next_cursor"}; detail=true adds the whole digest. Use it to see who worked where, when and on what. Pass session_id to flopwire_read with outline=true for its digest and skeleton, as session= to flopwire_grep and flopwire_search to search inside it, or to flopwire_peers session= to see whether it is live. has_more=true: pass next_cursor as cursor; a session active again during a walk moves (newest first it may be skipped, oldest first shown twice).`, "sessions",
			map[string]any{"glob": prop("string", "matches session id, title, repo or cwd; * and ?; a bare word matches anywhere")}),
		mcpTool("flopwire_read", "Read a message", "Read the message at an address that flopwire_grep, flopwire_search or flopwire_sessions printed, with its neighbours in conversation order. The header is # FULL_ID agent=A repo=PATH branch=B start=T active=T msgs=N title=\"…\". The focus text is numbered by line; long text is cut at max_chars and says which line_offset reads on; the hints name the messages_before/messages_after call for more messages. outline=true instead shows the session's digest and skeleton: every prompt, every tool call as tool(args) with failed calls and spawned subagents marked, no tool output; limit sets the page size and the footer prints the cursor that reads on.", "read",
			map[string]any{
				"address":         prop("string", "SESSION/ORDINAL[:LINE], SESSION, a message id, or /path/transcript.jsonl:LINE"),
				"messages_before": prop("integer", "whole messages before the focus, in conversation order; a tool call and its result are two messages (default 0)"),
				"messages_after":  prop("integer", "whole messages after the focus, in conversation order; a tool call and its result are two messages (default 0; a SESSION address shows 20)"),
				"max_chars":       prop("integer", "bytes of the focus text (default 4000, at most 24000; neighbours get a quarter)"),
				"line_offset":     prop("integer", "first line of the focus text to show"),
				"raw":             prop("boolean", "the transcript record's raw bytes instead, whatever format says"),
				"outline":         prop("boolean", "the session's digest and skeleton (prompts and tool calls, no output) instead of messages"),
				"cursor":          prop("string", "outline: where the next page starts; the footer prints it"),
				"limit":           prop("integer", "outline: entries per page (default 200, max 2000)"),
			}, "address"),
	}, mcpBusTools()...)
}

// mcpVerbs maps tool names to verbs.
var mcpVerbs = map[string]string{"flopwire_grep": "grep", "flopwire_search": "search", "flopwire_sessions": "sessions", "flopwire_read": "read"}

// mcpArgNames maps MCP argument names to option names where they differ.
var mcpArgNames = map[string]map[string]string{
	"grep": {"fixed_strings": "fixed-strings", "ignore_case": "ignore-case", "case_sensitive": "case-sensitive", "word": "word-regexp",
		"before": "before-context", "after": "after-context", "context": "context", "max_per_session": "max-count"},
}

// readLineArgs are grep's line-context arguments, which flopwire_read
// does not take: its neighbours are whole messages.
var readLineArgs = map[string]bool{"before": true, "after": true, "context": true}

// mcpOpts turns MCP arguments into the options the CLI parses, so both
// run the same code.
func mcpOpts(name string, args map[string]any) (*opts, bool, error) {
	verb, ok := mcpVerbs[name]
	if !ok {
		return nil, false, fmt.Errorf("unknown tool %q; tools: %s", name, strings.Join(mcpToolNames(), ", "))
	}
	o := newOpts(verb)
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := args[k]
		switch {
		case k == "format":
			s, _ := v.(string)
			if s != "" && s != "text" && s != "json" {
				return nil, false, fmt.Errorf("format: want text or json, not %q", s)
			}
			o.on[s] = s != ""
			continue
		case k == "pattern" && verb == "grep", k == "query" && verb == "search", k == "glob" && verb == "sessions", k == "address" && verb == "read":
			s, ok := v.(string)
			if !ok {
				return nil, false, fmt.Errorf("%s: want a string", k)
			}
			if s != "" {
				o.pos = append(o.pos, s)
			}
			continue
		case k == "output_mode" && verb == "grep":
			switch s, _ := v.(string); s {
			case "", "content":
			case "sessions":
				o.on["files-with-matches"] = true
			case "count":
				o.on["count"] = true
			default:
				return nil, false, fmt.Errorf("output_mode: want content, sessions or count, not %q", s)
			}
			continue
		}
		if verb == "read" && readLineArgs[k] {
			return nil, false, fmt.Errorf("%s: unknown argument %q; use messages_before and messages_after, which count whole messages, not lines (a tool call and its result are two messages; line_offset for lines of the focus)", name, k)
		}
		opt := strings.ReplaceAll(k, "_", "-")
		if m, ok := mcpArgNames[verb][k]; ok {
			opt = m
		}
		d, ok := lookupFlag(verb, opt, false)
		if !ok || opt == "json" || opt == "text" || opt == "max-bytes" || opt == "server" || opt == "index" || opt == "help" || opt == "regexp" || opt == "files-with-matches" || opt == "count" {
			return nil, false, fmt.Errorf("%s: unknown argument %q; it takes %s", name, k, strings.Join(mcpArgs(name), ", "))
		}
		switch d.kind {
		case fBool:
			b, ok := v.(bool)
			if !ok {
				return nil, false, fmt.Errorf("%s: want true or false", k)
			}
			if b {
				o.on[opt] = true
			}
		case fInt:
			n, ok := v.(float64)
			if !ok || n != float64(int(n)) {
				return nil, false, fmt.Errorf("%s: want an integer", k)
			}
			o.vals[opt] = strconv.Itoa(int(n))
		default:
			switch x := v.(type) {
			case string:
				o.vals[opt] = x
			case float64:
				o.vals[opt] = strconv.FormatFloat(x, 'f', -1, 64)
			case []any:
				var parts []string
				for _, p := range x {
					s, _ := p.(string)
					parts = append(parts, s)
				}
				o.vals[opt] = strings.Join(parts, ",")
			default:
				return nil, false, fmt.Errorf("%s: want a string", k)
			}
		}
	}
	return o, jsonMode(o), nil
}

// mcpArgs lists a tool's argument names, required first, for an unknown
// argument's error.
func mcpArgs(name string) []string {
	for _, t := range mcpTools() {
		tm := t.(map[string]any)
		if tm["name"] != name {
			continue
		}
		schema := tm["inputSchema"].(map[string]any)
		req, _ := schema["required"].([]string)
		var rest []string
		for k := range schema["properties"].(map[string]any) {
			if len(req) == 0 || k != req[0] {
				rest = append(rest, k)
			}
		}
		sort.Strings(rest)
		return append(req, rest...)
	}
	return nil
}

// mcpCall runs one tool and returns its answer: one text block.
func mcpCall(ctx context.Context, r *retriever, name string, args map[string]any) (string, error) {
	if _, ok := busToolNames[name]; ok {
		return busMCPCall(ctx, r, name, args)
	}
	return mcpRetrievalCall(ctx, r, name, args)
}

// mcpRetrievalCall runs one retrieval tool and returns its answer: text,
// or in JSON mode compact JSON within the budget. In JSON mode a
// failure's text is the JSON error object.
func mcpRetrievalCall(ctx context.Context, r *retriever, name string, args map[string]any) (string, error) {
	o, asJSON, err := mcpOpts(name, args)
	if err != nil {
		err = badArg(err)
	} else if len(o.pos) == 0 && o.verb != "sessions" {
		err = badArg(fmt.Errorf("%s: missing %s", name, map[string]string{"grep": "pattern", "search": "query", "read": "address"}[o.verb]))
	}
	var b bytes.Buffer
	if err == nil {
		err = runTool(ctx, r, o, &b, format.Style{MCP: true, Budget: format.MaxOutput}, selfMCP)
	}
	if err != nil {
		if o == nil {
			// The arguments did not parse: JSON when the tool answers JSON
			// by default, or format says so.
			f, _ := args["format"].(string)
			asJSON = f == "json" || f != "text" && name == "flopwire_sessions"
		}
		if asJSON {
			var eb bytes.Buffer
			_ = writeErrorJSON(&eb, retrievalErr(mcpVerbs[name], err, true))
			return "", &mcpBusError{text: strings.TrimRight(eb.String(), "\n")}
		}
		return "", err
	}
	text := strings.ToValidUTF8(b.String(), "�")
	if asJSON && !o.on["raw"] {
		text = strings.TrimRight(text, "\n")
	}
	return text, nil
}

// mcpError is the short isError text of a failed call, with a hint.
func mcpError(name string, err error) string {
	var be *mcpBusError
	if errors.As(err, &be) {
		return be.text
	}
	msg := shortError(err)
	switch {
	case errors.Is(err, format.ErrNotFound) && name == "flopwire_read":
		msg += " (addresses come from flopwire_grep, flopwire_search or flopwire_sessions output)"
	case errors.Is(err, format.ErrBadRequest) && strings.Contains(msg, "regex"):
		msg += " (RE2 syntax; set fixed_strings=true for a literal)"
	case errors.Is(err, context.Canceled):
		msg = "cancelled"
	}
	return msg
}

// serveMCP answers JSON-RPC requests, one per line, until in ends. Tool
// calls run concurrently (at most maxConcurrent at once), so a slow query
// never blocks ping or other calls; notifications/cancelled cancels a
// call, which then sends no response. A line longer than maxLine gets a
// JSON-RPC error and the server reads on.
// mcpInstructions is r's MCP instructions: mcpInstructions unless r
// carries its own.
func (r *retriever) mcpInstructions() string {
	if r.instructions != "" {
		return r.instructions
	}
	return mcpInstructions
}

func serveMCP(ctx context.Context, r *retriever, in io.Reader, out io.Writer) error {
	br := bufio.NewReaderSize(in, 64<<10)
	var wmu sync.Mutex
	enc := json.NewEncoder(out)
	var werr error
	send := func(v any) {
		wmu.Lock()
		defer wmu.Unlock()
		if werr == nil {
			werr = enc.Encode(v)
		}
	}
	var mu sync.Mutex
	running := map[string]context.CancelFunc{}
	var wg sync.WaitGroup
	slots := make(chan struct{}, maxConcurrent)
	defer wg.Wait()
	for {
		line, err := readLine(br, maxLine)
		if errors.Is(err, errLineTooLong) {
			send(map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32600, "message": fmt.Sprintf("request line over %d bytes", maxLine)}})
			continue
		}
		if len(bytes.TrimSpace(line)) > 0 {
			handleMCP(ctx, r, line, send, &mu, running, &wg, slots)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		wmu.Lock()
		e := werr
		wmu.Unlock()
		if e != nil {
			return e
		}
	}
}

var errLineTooLong = errors.New("line too long")

// readLine reads one line (without its newline) of at most max bytes; a
// longer line is read to its end, dropped, and reported as errLineTooLong.
func readLine(br *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	long := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !long {
			if len(buf)+len(chunk) > max+1 {
				long, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if long {
			if err == nil {
				return nil, errLineTooLong
			}
			return nil, err
		}
		return bytes.TrimSuffix(buf, []byte("\n")), err
	}
}

func handleMCP(ctx context.Context, r *retriever, line []byte, send func(any), mu *sync.Mutex, running map[string]context.CancelFunc, wg *sync.WaitGroup, slots chan struct{}) {
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if json.Unmarshal(line, &req) != nil {
		send(map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "parse error"}})
		return
	}
	if len(req.ID) == 0 || string(req.ID) == "null" { // a notification: no response
		if req.Method == "notifications/cancelled" {
			var p struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			if json.Unmarshal(req.Params, &p) == nil {
				mu.Lock()
				if cancel := running[string(p.RequestID)]; cancel != nil {
					cancel()
				}
				mu.Unlock()
			}
		}
		return
	}
	id := req.ID
	reply := func(key string, v any) { send(map[string]any{"jsonrpc": "2.0", "id": id, key: v}) }
	switch req.Method {
	case "initialize":
		reply("result", map[string]any{"protocolVersion": "2025-06-18", "serverInfo": map[string]string{"name": "flopwire", "version": version},
			"capabilities": map[string]any{"tools": map[string]any{}}, "instructions": r.mcpInstructions()})
	case "ping":
		reply("result", map[string]any{})
	case "tools/list":
		reply("result", map[string]any{"tools": mcpTools()})
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
			Meta      map[string]any `json:"_meta"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			reply("error", map[string]any{"code": -32602, "message": "invalid params: " + err.Error()})
			return
		}
		// Codex names the calling thread in each call's _meta.
		cctx, cancel := context.WithCancel(withMCPMeta(ctx, r.underCodex, p.Meta))
		key := string(id)
		mu.Lock()
		running[key] = cancel
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				mu.Lock()
				delete(running, key)
				mu.Unlock()
				cancel()
			}()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-cctx.Done():
				return
			}
			text, err := mcpCall(cctx, r, p.Name, p.Arguments)
			if cctx.Err() != nil && ctx.Err() == nil {
				return // cancelled by the client: no response
			}
			isErr := err != nil
			if isErr {
				text = mcpError(p.Name, err)
			}
			content := []any{map[string]string{"type": "text", "text": text}}
			if _, retrieval := mcpVerbs[p.Name]; retrieval && r.indexHint != nil {
				// A second block, so a JSON answer stays JSON.
				if h := r.indexHint(cctx); h != "" {
					content = append(content, map[string]string{"type": "text", "text": h})
				}
			}
			res := map[string]any{"content": content}
			if isErr {
				res["isError"] = true
			}
			reply("result", res)
		}()
	default:
		reply("error", map[string]any{"code": -32601, "message": "method not found: " + req.Method})
	}
}

// mcpCallOnce runs one tool, as tools/call would, with the JSON object on
// in as its arguments, and prints its answer. A failed call prints the
// same text the MCP answer's isError block holds and returns
// errReported, so the command exits 1. The opencode plugin serves the
// MCP tools as plugin tools this way: each call runs with the calling
// session in FLOPWIRE_SESSION_ID, which an MCP server there cannot learn.
func mcpCallOnce(ctx context.Context, r *retriever, name string, in io.Reader, out io.Writer) error {
	if !slices.Contains(mcpToolNames(), name) {
		return fmt.Errorf("mcp --call: unknown tool %q (%s)", name, strings.Join(mcpToolNames(), ", "))
	}
	raw, err := io.ReadAll(io.LimitReader(in, 1<<20))
	if err != nil {
		return err
	}
	args := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return fmt.Errorf("mcp --call: the arguments are not a JSON object: %w", err)
		}
	}
	text, err := mcpCall(ctx, r, name, args)
	if err != nil {
		fmt.Fprintln(out, mcpError(name, err))
		return errReported
	}
	_, err = fmt.Fprintln(out, text)
	return err
}
