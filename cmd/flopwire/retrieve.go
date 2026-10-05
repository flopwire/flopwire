package main

// The retrieval tools (spec §8): grep, sessions, read and search, the
// same four in the CLI and over MCP (mcp.go). They read the local index by
// default; --server queries the team server instead. Both backends answer
// in the shapes of internal/retrieval/format, and one renderer prints
// them, so every surface shows the same output and every printed address
// (SESSION/ORDINAL[:LINE]) round-trips through read.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/grep"
	"github.com/flopwire/flopwire/internal/retrieval/local"
)

// backend is a retrieval store: the local index or the team server.
type backend interface {
	Grep(ctx context.Context, q format.GrepQuery, f format.Filters) (*format.Page, error)
	Search(ctx context.Context, q format.SearchQuery, f format.Filters) (*format.Page, error)
	Sessions(ctx context.Context, glob, cursor string, f format.Filters) (*format.Sessions, error)
	Read(ctx context.Context, q format.ReadQuery, f format.Filters) (*format.Context, error)
	RawAt(ctx context.Context, address string) ([]byte, error)
	Raw(ctx context.Context, sourceID string, generation, offset, length int64) ([]byte, error)
}

// retriever is an open backend plus what self-session exclusion and live
// sessions need.
type retriever struct {
	backend
	caller func(ctx context.Context) (local.Caller, bool)
	// live lists the sessions this machine's harnesses hold open
	// (local.Detector.Live); nil detects none.
	live  func(codex bool) map[string]time.Time
	close func() error
	// instructions replace mcpInstructions when set (a sync-only device).
	instructions string
	// indexHint, set for the MCP server on a local index, is a note each
	// retrieval tool adds to its answer ("" for none): the index is empty.
	indexHint func(context.Context) string
	// busSocket is the device agent's control socket for the message bus
	// tools; "" is the default beside the client config.
	busSocket string
	// underCodex: Codex launched this MCP server (local.Detector.UnderCodex),
	// so a request's _meta names the calling Codex thread; otherwise _meta
	// is ignored.
	underCodex bool
	// teamRepo, set for the server, expands a --repo argument on this
	// device (local.ExpandRepo with the local index's placements), since
	// the server cannot read this device's git files.
	teamRepo func(ctx context.Context, repo string) (local.Repo, error)
}

// whoCalls is the calling session: the one an MCP request's _meta names
// (Codex, when it launched the server), else what the detector finds.
func (r *retriever) whoCalls(ctx context.Context) (local.Caller, bool) {
	if c, ok := metaCaller(ctx); ok {
		return c, true
	}
	if r.caller == nil {
		return local.Caller{}, false
	}
	return r.caller(ctx)
}

// openRetriever opens the local index at indexPath, or the server client
// when server is set. The local backend also gets the server client, when
// one is configured, for raw reads of files that are gone.
func openRetriever(server bool, indexPath string) (*retriever, error) {
	det := local.NewDetector()
	if server {
		c, err := serverClient()
		if err != nil {
			return nil, fmt.Errorf("--server: %w (run flopwire login and enroll first)", err)
		}
		if indexPath == "" {
			indexPath = local.IndexPath()
		}
		team := func(ctx context.Context, repo string) (local.Repo, error) {
			return local.ServerRepo(repo, localRepoDirs(ctx, indexPath), deviceUploads())
		}
		return &retriever{backend: c, caller: det.Detect, live: det.Live, close: func() error { return nil }, teamRepo: team}, nil
	}
	if indexPath == "" {
		indexPath = local.IndexPath()
	}
	if _, err := os.Stat(indexPath); err != nil {
		return nil, &noIndexError{path: indexPath}
	}
	lb, err := openLocalBackend(indexPath)
	if err != nil {
		return nil, err
	}
	return &retriever{backend: lb, caller: det.Detect, live: det.Live, close: lb.Store.Close}, nil
}

// localRepoDirs is what the local index at indexPath knows of where
// sessions ran (localindex.Store.RepoDirs), nil when there is no index.
func localRepoDirs(ctx context.Context, indexPath string) []localindex.RepoDir {
	if _, err := os.Stat(indexPath); err != nil {
		return nil
	}
	s, err := localindex.Open(indexPath, localindex.Options{ReadOnly: true})
	if err != nil {
		return nil
	}
	defer s.Close()
	dirs, _ := s.RepoDirs(ctx)
	return dirs
}

// deviceUploads judges a directory by this device's path rules, as its
// agent does (agent.DevicePolicy): true when a session placed there may
// reach the server. A --repo request to the server names only such
// checkouts. Without a config directory nothing is judged allowed.
func deviceUploads() func(pathpolicy.Placement) bool {
	dir, err := configDir()
	if err != nil {
		return func(pathpolicy.Placement) bool { return false }
	}
	cfg := agent.Config{}
	cfg.UserRules, cfg.AdminRulesCache = pathRuleFiles(dir)
	if cc, err := client.Load(); err == nil {
		cfg.UserRuleList, cfg.Unplaceable = cc.Denylist, cc.Unplaceable
	}
	pol := agent.DevicePolicy(cfg)
	return func(pl pathpolicy.Placement) bool { return agent.Uploads(pol, pl) }
}

// noIndexError: the local index does not exist yet, because the device
// agent has never run on this machine (or FLOPWIRE_INDEX points elsewhere).
type noIndexError struct{ path string }

func (e *noIndexError) Error() string {
	return fmt.Sprintf("no local index at %s (run the device agent, set FLOPWIRE_INDEX, or pass --server)", e.path)
}

// openLocalBackend opens the local index read-only, with the server client
// for raw reads of files that are gone when a server is configured.
func openLocalBackend(indexPath string) (*local.Backend, error) {
	s, err := localindex.Open(indexPath, localindex.Options{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	lb := &local.Backend{Store: s}
	if c, err := serverClient(); err == nil && c.Server != "" {
		lb.Remote = c
	}
	return lb, nil
}

func serverClient() (client.HTTP, error) {
	cfg, err := client.Load()
	if err != nil {
		return client.HTTP{}, err
	}
	return cfg.API(cfg.Token), nil
}

// --- options ---

// Flag kinds.
const (
	fBool = iota
	fInt
	fString
	fList // repeatable string
)

// flagDef is one option. Verbs is the set of tools that take it: g grep,
// s search, l sessions (list), r read; the message bus verbs p peers, m
// send (message), i inbox.
type flagDef struct {
	long  string
	short byte
	kind  int
	verbs string
}

var flagDefs = []flagDef{
	{"regexp", 'e', fList, "g"},
	{"only-matching", 'o', fBool, "g"},
	{"multiline", 'U', fBool, "g"},
	{"fixed-strings", 'F', fBool, "g"},
	{"ignore-case", 'i', fBool, "g"},
	{"case-sensitive", 's', fBool, "g"},
	{"word-regexp", 'w', fBool, "g"},
	{"files-with-matches", 'l', fBool, "g"},
	{"count", 'c', fBool, "g"},
	{"line-number", 'n', fBool, "g"}, // accepted, a no-op: lines are always numbered
	{"recursive", 'r', fBool, "g"},   // accepted, a no-op
	{"after-context", 'A', fInt, "g"},
	{"before-context", 'B', fInt, "g"},
	{"context", 'C', fInt, "g"},
	{"messages-before", 0, fInt, "r"},
	{"messages-after", 0, fInt, "r"},
	{"max-count", 'm', fInt, "g"},
	{"limit", 0, fInt, "gslrpi"},
	{"offset", 0, fInt, "gs"},
	{"cursor", 0, fString, "lri"},
	{"sort", 0, fString, "gsl"},
	{"no-heading", 0, fBool, "gs"},
	{"timeout", 0, fString, "gs"},
	{"agent", 0, fString, "gslp"},
	{"repo", 0, fString, "gslpm"},
	{"kind", 0, fString, "gs"},
	{"exclude-kind", 0, fString, "gs"},
	{"tool", 0, fString, "gs"},
	{"session", 0, fString, "gsp"},
	{"branch", 0, fString, "gsl"},
	{"since", 0, fString, "gsl"},
	{"until", 0, fString, "gsl"},
	{"device", 0, fString, "gsl"},
	{"user", 0, fString, "gslp"},
	{"exclude-subagents", 0, fBool, "gsl"},
	{"exclude-live", 0, fBool, "gsl"},
	{"include-superseded", 0, fBool, "gsr"},
	{"include-branches", 0, fBool, "gsr"},
	{"include-self", 0, fBool, "gsl"},
	{"max-chars", 0, fInt, "r"},
	{"line-offset", 0, fInt, "r"},
	{"raw", 0, fBool, "r"},
	{"outline", 0, fBool, "r"},
	{"detail", 0, fBool, "l"},
	{"json", 0, fBool, "gslrpmi"},
	{"max-bytes", 0, fInt, "gslrpmi"},
	{"server", 0, fBool, "gslr"},
	{"index", 0, fString, "gslr"},
	{"help", 'h', fBool, "gslrpmi"},
	{"intent", 0, fString, "m"},
	{"reply-to", 0, fString, "m"},
	{"ref", 0, fList, "m"},
	{"sent", 0, fBool, "i"},
	{"thread", 0, fString, "i"},
	{"socket", 0, fString, "pmi"},
	{"text", 0, fBool, "gslrpmi"},
}

// filterKeys are the flags that become format.Filters.
var filterKeys = []string{"agent", "repo", "kind", "exclude-kind", "tool", "session", "branch", "sort", "since", "until", "device", "user",
	"exclude-subagents", "exclude-live", "include-superseded", "include-branches"}

// opts are a tool's parsed options: from the command line or from MCP
// arguments.
type opts struct {
	verb string
	pos  []string
	vals map[string]string   // string and int options by long name, as given
	list map[string][]string // -e
	on   map[string]bool     // bool options
}

func newOpts(verb string) *opts {
	return &opts{verb: verb, vals: map[string]string{}, list: map[string][]string{}, on: map[string]bool{}}
}

func (o *opts) int(name string) (int, error) {
	s, ok := o.vals[name]
	if !ok {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("--%s wants a number >= 0, not %q", name, s)
	}
	return n, nil
}

func verbLetter(verb string) byte {
	switch verb {
	case "grep":
		return 'g'
	case "search":
		return 's'
	case "sessions":
		return 'l'
	case "peers":
		return 'p'
	case "send":
		return 'm'
	case "inbox":
		return 'i'
	}
	return 'r'
}

func lookupFlag(verb, name string, short bool) (flagDef, bool) {
	v := verbLetter(verb)
	for _, d := range flagDefs {
		if strings.IndexByte(d.verbs, v) < 0 {
			continue
		}
		if short && len(name) == 1 && d.short == name[0] || !short && d.long == name {
			return d, true
		}
	}
	return flagDef{}, false
}

// parseArgs parses a tool's command line: flags before, between and after
// positional arguments; --name=value, --name value, -x value, -xVALUE and
// grouped short booleans (-il); "--" ends the flags, so a pattern may start
// with a dash (or use -e).
func parseArgs(verb string, args []string) (*opts, error) {
	o := newOpts(verb)
	set := func(d flagDef, val string) {
		switch d.kind {
		case fBool:
			o.on[d.long] = true
		case fList:
			o.list[d.long] = append(o.list[d.long], val)
		default:
			o.vals[d.long] = val
		}
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			o.pos = append(o.pos, args[i+1:]...)
			return o, nil
		case strings.HasPrefix(a, "--"):
			name, val, hasVal := strings.Cut(a[2:], "=")
			d, ok := lookupFlag(verb, name, false)
			if !ok {
				return nil, unknownFlag(verb, "--"+name)
			}
			if d.kind == fBool {
				if hasVal {
					b, err := strconv.ParseBool(val)
					if err != nil {
						return nil, fmt.Errorf("--%s takes no value", name)
					}
					if !b {
						continue
					}
				}
				set(d, "")
				continue
			}
			if !hasVal {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("--%s needs a value", name)
				}
				i++
				val = args[i]
			}
			set(d, val)
		case len(a) > 1 && a[0] == '-':
			for j := 1; j < len(a); j++ {
				d, ok := lookupFlag(verb, a[j:j+1], true)
				if !ok {
					return nil, unknownFlag(verb, "-"+a[j:j+1])
				}
				if d.kind == fBool {
					set(d, "")
					continue
				}
				val := a[j+1:]
				if val == "" {
					if i+1 >= len(args) {
						return nil, fmt.Errorf("-%c needs a value", d.short)
					}
					i++
					val = args[i]
				}
				set(d, val)
				break
			}
		default:
			o.pos = append(o.pos, a)
		}
	}
	return o, nil
}

// readLineFlags are grep's line-context flags, which read does not take:
// read's neighbours are whole messages.
var readLineFlags = map[string]string{"-A": "--messages-after N", "--after-context": "--messages-after N",
	"-B": "--messages-before N", "--before-context": "--messages-before N",
	"-C": "--messages-before N --messages-after N", "--context": "--messages-before N --messages-after N"}

// unknownFlag is one line naming the nearest flag the tool takes.
func unknownFlag(verb, flag string) error {
	if fix, ok := readLineFlags[flag]; ok && verb == "read" {
		return fmt.Errorf("read: %s is grep's line context; read counts whole messages, not lines: use %s (a tool call and its result are two messages; --line-offset N for lines of the focus)", flag, fix)
	}
	name := strings.TrimLeft(flag, "-")
	v := verbLetter(verb)
	best, bestD := "", 1<<30
	for _, d := range flagDefs {
		if strings.IndexByte(d.verbs, v) < 0 {
			continue
		}
		cands := []string{"--" + d.long}
		if d.short != 0 {
			cands = append(cands, "-"+string(d.short))
		}
		for _, c := range cands {
			if dist := editDistance(name, strings.TrimLeft(c, "-")); dist < bestD {
				best, bestD = c, dist
			}
		}
	}
	return fmt.Errorf("%s: unknown flag %s; did you mean %s? (flopwire %s --help)", verb, flag, best, verb)
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// filters builds format.Filters from the options. A relative --repo (".",
// "./x") becomes the absolute git root holding it; the backend (or, for
// the server, teamRepo) expands it to the repository's checkouts.
func (o *opts) filters() (format.Filters, error) {
	v := url.Values{}
	for _, k := range filterKeys {
		key := strings.ReplaceAll(k, "-", "_")
		if s, ok := o.vals[k]; ok {
			if k == "repo" {
				s = local.ResolveRepo(s)
			}
			v.Set(key, s)
		}
		if o.on[k] {
			v.Set(key, "true")
		}
	}
	f, err := format.ParseFilters(v)
	if err != nil {
		return f, err
	}
	f.Limit, err = o.int("limit")
	return f, err
}

func (o *opts) timeout() (time.Duration, error) {
	s, ok := o.vals["timeout"]
	if !ok {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		n, nerr := strconv.ParseFloat(s, 64)
		if nerr != nil || n <= 0 {
			return 0, fmt.Errorf("--timeout wants a duration (30s) or seconds, not %q", s)
		}
		d = time.Duration(n * float64(time.Second))
	}
	return min(d, localindex.MaxTimeout), nil
}

// --- running a tool ---

// selfPolicy says when to leave out the calling session: always over MCP
// (the caller is an agent), on the CLI only off a terminal (decision D4:
// a human at a TTY sees everything).
type selfPolicy int

const (
	selfCLI selfPolicy = iota
	selfMCP
)

// excludeSelf fills f.ExcludeSession with the calling session, unless the
// caller asked for its own rows, named a session already, or is a human
// at a terminal, and returns the note naming what it left out.
func (r *retriever) excludeSelf(ctx context.Context, f *format.Filters, includeSelf bool, p selfPolicy) string {
	if includeSelf || f.ExcludeSession != "" || f.Session != "" {
		return ""
	}
	if p == selfCLI && term.IsTerminal(int(os.Stdin.Fd())) {
		return ""
	}
	c, ok := r.whoCalls(ctx)
	if !ok {
		return ""
	}
	f.ExcludeSession = c.SessionID
	flag := "--include-self"
	if p == selfMCP {
		flag = "include_self=true"
	}
	return fmt.Sprintf("left out your own session %s and its subagents; %s includes them", c.SessionID, flag)
}

// self resolves "self" (--session self, read self) to the calling
// agent's session, found by exact evidence only (D4).
func (r *retriever) self(ctx context.Context, p selfPolicy) (string, error) {
	if c, ok := r.whoCalls(ctx); ok {
		return c.SessionID, nil
	}
	how := "set FLOPWIRE_SESSION_ID to your session id"
	if p == selfMCP {
		how = "set FLOPWIRE_SESSION_ID in the MCP server's environment"
	}
	return "", badArg(fmt.Errorf(`session "self": cannot identify the calling session exactly (no Claude Code session file, no single open Codex rollout, no FLOPWIRE_SESSION_ID); %s, or pass the session's id from flopwire sessions`, how))
}

// markLive sets Live on the answer's sessions: active within
// format.LiveWindow, or held open by a harness on this machine. Claude
// and Devin sessions are checked by their session and lock files (cheap);
// Codex rollouts by lsof, only when a Codex session in the answer was
// active within local.LiveCap.
func (r *retriever) markLive(infos ...*format.ConversationInfo) {
	if len(infos) == 0 {
		return
	}
	now := time.Now()
	codex := false
	for _, c := range infos {
		if c.Agent == "codex" && c.LastActivityAt != nil && now.Sub(*c.LastActivityAt) < local.LiveCap {
			codex = true
		}
	}
	var live map[string]time.Time
	if r.live != nil {
		live = r.live(codex)
	}
	for _, c := range infos {
		local.MarkLive(c, live, now)
	}
}

// liveIDs lists the sessions to leave out for --exclude-live beyond those
// active within format.LiveWindow: the ones this machine's harnesses hold
// open.
func (r *retriever) liveIDs() []string {
	if r.live == nil {
		return nil
	}
	var ids []string
	for id := range r.live(true) {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func pageInfos(p *format.Page) []*format.ConversationInfo {
	var out []*format.ConversationInfo
	for i := range p.SessionInfo {
		out = append(out, &p.SessionInfo[i])
	}
	for i := range p.Sessions {
		out = append(out, &p.Sessions[i])
	}
	return out
}

// runTool runs one tool and writes its text (or JSON) answer to w.
func runTool(ctx context.Context, r *retriever, o *opts, w io.Writer, st format.Style, p selfPolicy) error {
	f, err := o.filters()
	if err != nil {
		return badArg(err)
	}
	if r.teamRepo != nil && f.Repo != "" {
		rp, err := r.teamRepo(ctx, f.Repo)
		if err != nil {
			return err // a format.ErrBadRequest
		}
		f.Repo, f.RepoRoots, f.RepoMains, f.RepoRemotes = rp.Repo, rp.Roots, rp.Mains, rp.Remotes
	}
	if f.Session == "self" {
		if f.Session, err = r.self(ctx, p); err != nil {
			return err
		}
		f.Self = true
	}
	if f.ExcludeLive {
		f.Live = r.liveIDs()
	}
	st.Flat = o.on["no-heading"]
	asJSON := jsonMode(o)
	// emit writes the answer: the readable text, the --json form (grep,
	// search and read on the CLI: indented, as before), or compact JSON
	// within the budget (sessions, and every tool over MCP).
	emit := func(full any, bounded func() any, text func() error) error {
		switch {
		case !asJSON:
			return text()
		case st.MCP || o.verb == "sessions":
			return writeOut(w, bounded())
		}
		return writeJSON(w, full)
	}
	switch o.verb {
	case "grep":
		spec := grep.Spec{Patterns: append([]string{}, o.list["regexp"]...), Fixed: o.on["fixed-strings"], IgnoreCase: o.on["ignore-case"],
			CaseSensitive: o.on["case-sensitive"], Word: o.on["word-regexp"]}
		if len(spec.Patterns) == 0 {
			if len(o.pos) == 0 {
				return badArg(errors.New("grep needs a PATTERN (flopwire grep --help)"))
			}
			spec.Patterns, o.pos = o.pos[:1], o.pos[1:]
		}
		if len(o.pos) > 0 {
			return badArg(fmt.Errorf("grep takes one PATTERN; got also %q (quote the pattern, or use -e for each)", strings.Join(o.pos, " ")))
		}
		var q format.GrepQuery
		if err := spec.Query(&q); err != nil {
			return err
		}
		q.OnlyMatching, q.Multiline = o.on["only-matching"], o.on["multiline"]
		if q.Timeout, err = o.timeout(); err != nil {
			return badArg(err)
		}
		ctxN, err := o.int("context")
		if err != nil {
			return badArg(err)
		}
		q.Before, q.After = ctxN, ctxN
		for name, dst := range map[string]*int{"before-context": &q.Before, "after-context": &q.After, "max-count": &q.MaxPerSession, "offset": &q.Offset} {
			if _, ok := o.vals[name]; ok {
				if *dst, err = o.int(name); err != nil {
					return badArg(err)
				}
			}
		}
		q.Limit = f.Limit
		switch {
		case o.on["files-with-matches"]:
			q.Mode = format.ModeSessions
		case o.on["count"]:
			q.Mode = format.ModeCount
		}
		note := r.excludeSelf(ctx, &f, o.on["include-self"], p)
		page, err := r.Grep(ctx, q, f)
		if err != nil {
			return err
		}
		r.markLive(pageInfos(page)...)
		page.Excluded = note
		return emit(page, func() any { return boundPage("grep", page, q.Mode, st.Budget, st.MCP) }, func() error { return format.WriteGrep(w, page, q.Mode, st) })
	case "search":
		if len(o.pos) == 0 {
			return badArg(errors.New("search needs a QUERY (flopwire search --help)"))
		}
		q := format.SearchQuery{Query: strings.Join(o.pos, " "), Limit: f.Limit}
		if q.Offset, err = o.int("offset"); err != nil {
			return badArg(err)
		}
		if q.Timeout, err = o.timeout(); err != nil {
			return badArg(err)
		}
		note := r.excludeSelf(ctx, &f, o.on["include-self"], p)
		page, err := r.Search(ctx, q, f)
		if err != nil {
			return err
		}
		r.markLive(pageInfos(page)...)
		page.Excluded = note
		return emit(page, func() any { return boundPage("search", page, "", st.Budget, st.MCP) }, func() error { return format.WriteSearch(w, page, st) })
	case "sessions":
		if len(o.pos) > 1 {
			return badArg(fmt.Errorf("sessions takes one GLOB; got %q", strings.Join(o.pos, " ")))
		}
		glob := ""
		if len(o.pos) == 1 {
			glob = o.pos[0]
		}
		note := r.excludeSelf(ctx, &f, o.on["include-self"], p)
		out, err := r.Sessions(ctx, glob, o.vals["cursor"], f)
		if err != nil {
			return err
		}
		var infos []*format.ConversationInfo
		for i := range out.Sessions {
			infos = append(infos, &out.Sessions[i])
		}
		r.markLive(infos...)
		out.Excluded = note
		return emit(out, func() any { return boundSessions(out, st.Budget, st.MCP, o.on["detail"]) }, func() error { return format.WriteSessions(w, out, st) })
	case "read":
		if len(o.pos) != 1 {
			return badArg(errors.New("read needs one ADDRESS: SESSION/ORDINAL[:LINE], SESSION, a message id, or /path/file.jsonl:LINE"))
		}
		self := o.pos[0] == "self"
		if self {
			if o.pos[0], err = r.self(ctx, p); err != nil {
				return err
			}
		}
		if o.on["raw"] && self {
			// The session's exact id, never a prefix: find its first
			// message, then read that one's bytes.
			cx, err := r.Read(ctx, format.ReadQuery{Address: o.pos[0], Self: true}, f)
			if err != nil {
				return err
			}
			for _, m := range cx.Messages {
				if m.ID == cx.Focus {
					o.pos[0] = m.Address
				}
			}
		}
		if o.on["raw"] {
			data, err := r.RawAt(ctx, o.pos[0])
			if err != nil {
				return err
			}
			return writeRaw(w, data, o.on["json"])
		}
		q := format.ReadQuery{Address: o.pos[0], Outline: o.on["outline"], Cursor: o.vals["cursor"], Limit: f.Limit, Self: self}
		if (q.Cursor != "" || q.Limit > 0) && !q.Outline {
			step := "--messages-before/--messages-after"
			if st.MCP {
				step = "messages_before/messages_after"
			}
			return badArg(fmt.Errorf("--cursor and --limit page an outline; add --outline, or use %s to step through messages", step))
		}
		for name, dst := range map[string]*int{"messages-before": &q.Before, "messages-after": &q.After, "max-chars": &q.MaxChars, "line-offset": &q.LineOffset} {
			if _, ok := o.vals[name]; ok {
				if *dst, err = o.int(name); err != nil {
					return badArg(err)
				}
			}
		}
		if st.Budget > 0 && !asJSON && q.MaxChars > st.Budget {
			q.MaxChars = st.Budget // the rest reads on by line_offset
		}
		cx, err := r.Read(ctx, q, f)
		if err != nil {
			return err
		}
		r.markLive(&cx.Conversation)
		return emit(cx, func() any { return boundRead(cx, st.Budget, st.MCP) }, func() error { return format.WriteRead(w, cx, st) })
	}
	return fmt.Errorf("unknown tool %q", o.verb)
}

// terminalOut reports whether w is a terminal; tests stand one in.
var terminalOut = func(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// writeRaw writes archived record bytes: exact into a pipe or file or
// when exact is set (--json), with control characters cleaned (D17) on a
// terminal.
func writeRaw(w io.Writer, data []byte, exact bool) error {
	if !exact && terminalOut(w) {
		data = []byte(format.Clean(string(data)))
	}
	_, err := w.Write(data)
	return err
}

// badArg marks an error as the caller's (a usage error).
func badArg(err error) error { return fmt.Errorf("%w: %v", format.ErrBadRequest, err) }

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// shortError is the one line a failed tool call prints: the error without
// the internal prefixes.
func shortError(err error) string {
	s := err.Error()
	for _, p := range []string{"retrieval: bad request: ", "retrieval: not found: ", "retrieval: bad request", "retrieval: not found"} {
		s = strings.ReplaceAll(s, p, "")
	}
	if errors.Is(err, format.ErrNotFound) && !strings.Contains(s, "no ") {
		s = "not found: " + s
	}
	return format.Clean(strings.TrimSpace(s))
}

// cliStyle is the CLI's output style. The CLI prints everything unless
// --max-bytes asks for a budget (ripgrep's convention): a human
// redirecting to a file expects all of it, and an agent's host already
// caps a command's output. MCP answers are always budgeted.
func cliStyle(o *opts) (format.Style, error) {
	n, err := o.int("max-bytes")
	if err != nil {
		return format.Style{}, badArg(err)
	}
	return format.Style{Budget: n}, nil
}

// toolCmd is the CLI entry of a tool: parse, print help, open, run.
// In JSON mode (sessions by default, any verb with --json) a failure is
// one JSON object on stderr, as the message bus verbs print it, and the
// command exits 1.
func toolCmd(ctx context.Context, verb string, args []string) error {
	return toolCmdIO(ctx, verb, args, os.Stdout, os.Stderr)
}

func toolCmdIO(ctx context.Context, verb string, args []string, stdout, stderr io.Writer) error {
	o, err := parseArgs(verb, args)
	asJSON := false
	if err != nil {
		asJSON, err = jsonModeArgs(verb, args), badArg(err)
	} else {
		asJSON = jsonMode(o)
		err = runToolCmd(ctx, verb, o, stdout, stderr, asJSON)
	}
	if err == nil {
		return nil
	}
	if !asJSON {
		return errors.New(shortError(err))
	}
	if werr := writeErrorJSON(stderr, retrievalErr(verb, err, false)); werr != nil {
		return errors.New(shortError(err))
	}
	return errReported
}

func runToolCmd(ctx context.Context, verb string, o *opts, stdout, stderr io.Writer, asJSON bool) error {
	if o.on["help"] {
		_, err := io.WriteString(stdout, toolHelp[verb])
		return err
	}
	if len(o.pos) == 0 && len(o.list["regexp"]) == 0 && verb != "sessions" {
		if !asJSON {
			fmt.Fprint(stderr, toolHelp[verb])
		}
		return badArg(fmt.Errorf("%s: missing argument", verb))
	}
	r, err := openRetriever(o.on["server"], o.vals["index"])
	if err != nil {
		return err
	}
	defer r.close()
	st, err := cliStyle(o)
	if err != nil {
		return err
	}
	return runTool(ctx, r, o, stdout, st, selfCLI)
}

// raw is plumbing: the archived bytes of a source generation range, by
// provenance (source_id generation offset length).
func raw(ctx context.Context, args []string) error {
	o := newOpts("raw")
	server, index := false, ""
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--server":
			server = true
		case a == "--index" && i+1 < len(args):
			i++
			index = args[i]
		default:
			o.pos = append(o.pos, a)
		}
	}
	if len(o.pos) != 4 {
		return errors.New("usage: flopwire raw [--server] SOURCE_ID GENERATION OFFSET LENGTH (for a message's record: flopwire read --raw ADDRESS)")
	}
	var n [3]int64
	for i := range n {
		var err error
		if n[i], err = strconv.ParseInt(o.pos[i+1], 10, 64); err != nil {
			return fmt.Errorf("%s is not a number", o.pos[i+1])
		}
	}
	r, err := openRetriever(server, index)
	if err != nil {
		return err
	}
	defer r.close()
	data, err := r.Raw(ctx, o.pos[0], n[0], n[1], n[2])
	if err != nil {
		return err
	}
	return writeRaw(os.Stdout, data, false)
}

// flagsOf lists a tool's flags, for tests.
func flagsOf(verb string) []string {
	var out []string
	v := verbLetter(verb)
	for _, d := range flagDefs {
		if strings.IndexByte(d.verbs, v) >= 0 {
			out = append(out, d.long)
		}
	}
	sort.Strings(out)
	return out
}

// toolHelp is each tool's help: three examples first, then every flag, on
// one screen.
var toolHelp = map[string]string{
	"grep": `flopwire grep — regex search over agent transcripts (Claude Code, Codex, Devin, opencode), like rg

  flopwire grep 'upload\.test.*timeout'               RE2 regex; smart case
  flopwire grep -F 'exit status 1' --since 7d -C 2    literal; 2 lines of context
  flopwire grep -l flaky --repo flopwire --agent codex  sessions with matches

Hits group under a header per session: its id, then key=value fields (a value with a space,
quote or = is a JSON string), newest first; the address for read is SESSION/ORDINAL:LINE.
  ## SESSION agent=claude ended=2026-09-23 repo=api branch=main failed=1 intent="..."
  ORDINAL:LINE kind/tool: text        (--no-heading: SESSION/ORDINAL:LINE: [attribution] text)
Pattern  -e PAT (repeat)  -F literal  -i/-s case  -w words  -U multiline (a match spans
         lines of one message)  (-n, -r accepted)
Output   -o matched text only  -l sessions  -c counts  -A/-B/-C N context  -m N per session
         --limit N (20)  --offset N  --sort newest|oldest|relevance  --no-heading  --json
         (--text: the default)  --timeout 30s (max 60s)  --max-bytes N (whole hits)
Filters  --agent claude,codex,devin,opencode  --repo .|PATH|NAME|GLOB  --branch NAME|GLOB
         --since 7d  --until T  --kind K,..  --exclude-kind K,..  --tool Bash
         --session SESSION|self  --exclude-subagents  --exclude-live  --include-superseded
         --include-branches  --include-self  --device D  --user U
Source   the local index; --server for the team server; --index PATH
Kinds    user assistant tool_call tool_result thinking system agent_message injected (hidden
         unless --kind names it). Times are UTC: 7d, 24h, 2026-09-23, '2026-09-23 10:00Z'.
Live: active in the last 10 minutes or open in its harness. An agent's own session is left
out (a human at a terminal sees all); --session self searches only it.
`,
	"search": `flopwire search — ranked search (BM25) for fuzzy questions; use grep for exact strings

  flopwire search sqlite trigram tokenizer
  flopwire search '"exponential backoff" retry' --repo flopwire   quoted phrases must match
  flopwire search papercut --agent codex --since 30d --sort newest

Hits are grouped under one labeled header per session (as grep's), best first:
  ORDINAL:LINE kind/tool: snippet
The address for flopwire read is SESSION/ORDINAL:LINE. When no message has every word,
it ranks messages with any of them (common words dropped) and says so. Identical texts
show once, "+N copies".

Output   --limit N (20)  --offset N  --sort relevance|newest|oldest  --no-heading  --json
         (--text: the default)  --timeout 30s (max 60s)  --max-bytes N (whole hits)
Filters  --agent  --repo .|PATH|NAME|GLOB  --branch NAME|GLOB  --since  --until  --kind
         --exclude-kind  --tool  --session SESSION|self  --exclude-subagents  --exclude-live
         --include-superseded  --include-branches  --include-self  --device  --user
Times    --since/--until take 7d, 24h, 2026-09-23, '2026-09-23 10:00Z' or RFC 3339 (UTC)
Source   the local index; --server for the team server; --index PATH
`,
	"sessions": `flopwire sessions — list sessions, newest activity first, as JSON (--text: readable)

  flopwire sessions                         the latest sessions
  flopwire sessions 'flopwire*' --since 7d    by repo name, title or session id
  flopwire sessions --agent codex --repo . --branch 'feat/*'

JSON: {"kind":"sessions","sessions":[{"session_id":FULL ID,"address","agent","user","repo",
"branches","live","last_activity_at","messages","title","intent","parent_session",
"commits","files","failed"}…],"has_more":bool,"next_cursor":C}. --detail: every
field and the whole digest. has_more: pass next_cursor as --cursor. --text: one row each:
  SESSION agent=claude ended=2026-09-23 repo=api branch=main msgs=24 intent="..."
Pass session_id to flopwire read --outline (digest and skeleton), as --session to grep and
search, or to flopwire peers --session (is it live?). A bare GLOB word matches anywhere.
--since/--until take 7d, 24h, 2026-09-23, '2026-09-23 10:00Z' or RFC 3339; times are UTC.

Output   --limit N (20)  --cursor C (next_cursor)  --sort newest|oldest  --detail  --text
         --max-bytes N  (--json: the default)
Filters  --agent  --repo .|PATH|NAME|GLOB  --branch NAME|GLOB  --since/--until (last
         activity)  --exclude-subagents  --exclude-live  --include-self  --device  --user
Errors   JSON on stderr: {"kind":"error","error":{"code","detail","fix","example"}}; exit 1
Source   the local index; --server for the team server; --index PATH
`,
	"read": `flopwire read — read a message (and its neighbours) at an address from grep, search or sessions

  flopwire read 0b7e2c1a/28672:14          a message, from line 14's neighbourhood
  flopwire read 0b7e2c1a/28672 --messages-before 2 --messages-after 2   two messages either side
  flopwire read 0b7e2c1a --outline         the session's digest and skeleton

ADDRESS is SESSION/ORDINAL[:LINE], SESSION (any unique prefix of the id), a message
id, /path/to/transcript.jsonl:LINE, or self (the calling agent's session). The header is
grep's form: # FULL_ID agent=A repo=PATH branch=B start=T active=T msgs=N title="..."
(times UTC). The focus prints with line numbers; long text is cut at --max-chars and says
how to read on. --max-bytes N keeps the focus and nearest neighbours, and says what it cut.

--outline prints the session's digest (intent, repos, branches, duration, messages by
kind, subagents, commands, failed calls, tools, files edited, PRs, commits, issues,
tokens, last reply) and its skeleton: every prompt, every tool call as tool(args) with
failed calls and spawned subagents marked, no tool output. --limit (200) sets the page
size; the footer prints the --cursor that reads on.

Messages --messages-before N / --messages-after N: whole messages before/after the focus, in
         conversation order; a tool call and its result are two messages (SESSION: 20 after)
Output   --max-chars N (4000; neighbours get a quarter)  --line-offset N (first line of the
         focus)  --raw (the record's bytes)  --outline [--cursor C --limit N]  --json (--text)
Rows     --include-superseded  --include-branches
Source   the local index; --server for the team server; --index PATH
`,
}
