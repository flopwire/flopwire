// Package local answers the retrieval tools (spec §7.3, §8) from the
// device's own SQLite index, in the shapes of internal/retrieval/format, so
// the CLI and the MCP server print the same output whichever store answers.
//
//   - grep: the trigram index selects candidates (a substring, or the
//     regexq plan of a regex), newest message first; internal/retrieval/grep
//     verifies them and collects the page.
//   - search: ranked BM25 over the token table, with phrases and an
//     any-term retry.
//   - sessions: conversations by last activity.
//   - read: a message and its neighbours by ordinal, located by address;
//     raw bytes from the transcript file, or from the team server when the
//     file is gone or replaced.
//
// Message, conversation and source ids are the local index's integer ids,
// rendered as decimal strings.
package local

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/grep"
	"github.com/flopwire/flopwire/internal/transcript"
)

const (
	MaxRaw     = 16 << 20
	maxContext = 200
	// sessionAfter is how many messages read shows after the first when
	// the address names a whole session.
	sessionAfter = 19
	// maxScan bounds an unindexed grep (a pattern under three characters,
	// or a regex with no required trigram): the newest rows passing the
	// filters.
	maxScan = 20000
)

// IndexPath is the local index location: $FLOPWIRE_INDEX, else
// <user cache dir>/flopwire/index.db.
func IndexPath() string {
	if p := os.Getenv("FLOPWIRE_INDEX"); p != "" {
		return p
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "flopwire", "index.db")
}

// Remote is the team server, used for raw bytes the local file no longer
// holds. client.HTTP implements it.
type Remote interface {
	RawByPath(ctx context.Context, path, fileID string, generation, offset, length int64) ([]byte, error)
}

// Backend serves the tools from a local index.
type Backend struct {
	Store  *localindex.Store
	Remote Remote // optional
}

func bad(format_ string, args ...any) error {
	return fmt.Errorf("%w: %s", format.ErrBadRequest, fmt.Sprintf(format_, args...))
}

func limit(n int) int {
	switch {
	case n <= 0:
		return grep.DefaultLimit
	case n > grep.MaxLimit:
		return grep.MaxLimit
	}
	return n
}

// filter translates the shared filters. Session keeps one session (a
// unique prefix of its id) and its subagents; ExcludeSession drops the session's
// conversation and every subagent conversation below it. User is ignored:
// the local index holds only the owner's data.
func (b *Backend) filter(ctx context.Context, f format.Filters) (localindex.Filter, error) {
	out := localindex.Filter{Since: f.Since, Until: f.Until, ExcludeSubagents: f.ExcludeSubagents,
		IncludeSuperseded: f.IncludeSuperseded, IncludeBranches: f.IncludeBranches, Tools: f.Tools}
	for _, a := range format.List(f.Agent) {
		out.Agents = append(out.Agents, transcript.Agent(a))
	}
	if f.Repo != "" || len(f.RepoRoots) > 0 {
		repo, roots := f.Repo, f.RepoRoots
		if len(roots) == 0 {
			dirs, err := b.Store.RepoDirs(ctx)
			if err != nil {
				return out, err
			}
			if repo, roots, _, err = ExpandRepo(repo, dirs, false); err != nil {
				return out, err
			}
		}
		prefix, like := format.RepoMatch(repo)
		if prefix != "" {
			out.Repos = []string{prefix}
		}
		out.Repos = format.FitRoots(append(out.Repos, roots...), localindex.MaxRepos)
		if like != "" {
			out.RepoLikes = []string{like}
		}
	}
	out.Devices = format.List(f.Device)
	if f.Branch != "" {
		out.Branches = []string{format.BranchMatch(f.Branch)}
	}
	if f.ExcludeLive {
		out.IdleBefore = time.Now().Add(-format.LiveWindow)
		for _, sid := range f.Live {
			// Held open counts while the session wrote within LiveCap, as
			// MarkLive has it.
			if active, err := b.Store.SessionActiveSince(ctx, sid, time.Now().Add(-LiveCap)); err != nil {
				return out, err
			} else if !active {
				continue
			}
			ids, err := b.Store.SessionConversationIDs(ctx, "", sid)
			if err != nil {
				return out, err
			}
			out.ExcludeConversations = append(out.ExcludeConversations, ids...)
		}
	}
	for _, set := range []struct {
		in  []string
		out *[]transcript.Kind
	}{{f.Kinds, &out.Kinds}, {f.ExcludeKinds, &out.ExcludeKinds}} {
		for _, k := range set.in {
			kind, err := transcript.ParseKind(strings.TrimSpace(k))
			if err != nil {
				return out, bad("unknown kind %q (user, assistant, tool_call, tool_result, thinking, system, injected, agent_message)", k)
			}
			*set.out = append(*set.out, kind)
		}
	}
	if f.ExcludeConversation != "" {
		id, err := strconv.ParseInt(f.ExcludeConversation, 10, 64)
		if err != nil {
			return out, bad("exclude_conversation %q is not a local id", f.ExcludeConversation)
		}
		out.ExcludeConversations = append(out.ExcludeConversations, id)
	}
	if f.Session != "" {
		sid := f.Session
		if !f.Self { // the caller's own session is its exact id, never a prefix
			var err error
			if sid, err = b.resolveSession(ctx, f.Session); err != nil {
				return out, err
			}
		}
		var err error
		if out.Conversations, err = b.Store.SessionConversationIDs(ctx, "", sid); err != nil {
			return out, err
		}
		if f.Self && len(out.Conversations) == 0 {
			return out, fmt.Errorf("%w: your session %s is not indexed yet", format.ErrNotFound, sid)
		}
	}
	if f.ExcludeSession != "" {
		ids, err := b.Store.SessionConversationIDs(ctx, "", f.ExcludeSession)
		if err != nil {
			return out, err
		}
		out.ExcludeSessions = []string{f.ExcludeSession}
		out.ExcludeConversations = append(out.ExcludeConversations, ids...)
	}
	return out, nil
}

// ResolveRepo turns a relative repo path ("." or "./sub", "../x") into
// the absolute git root that contains it; anything else (an absolute
// path, a repo name, a glob) passes through.
func ResolveRepo(repo string) string {
	if filepath.IsAbs(repo) {
		return filepath.Clean(repo)
	}
	if repo != "." && repo != ".." && !strings.HasPrefix(repo, "./") && !strings.HasPrefix(repo, "../") {
		return repo
	}
	abs, err := filepath.Abs(repo)
	if err != nil {
		return repo
	}
	if root := localindex.FindRepoRoot(abs); root != "" {
		return root
	}
	return abs
}

// Grep answers a grep query: messages matching the pattern, newest first,
// with every matching line (and context lines), collapsed by identical
// text, counted for the footer.
func (b *Backend) Grep(ctx context.Context, q format.GrepQuery, f format.Filters) (*format.Page, error) {
	lf, err := b.filter(ctx, f)
	if err != nil {
		return nil, err
	}
	plan, err := grep.Compile(q)
	if err != nil {
		return nil, err
	}
	if q.Sort, err = format.SortFor("grep", f.Sort); err != nil {
		return nil, err
	}
	q.Limit = limit(q.Limit)
	var tq *localindex.TrigramQuery
	if q.Fixed {
		tq = b.Store.SubstringQuery(q.Pattern)
	} else {
		tq = plan.Query.TrigramQuery()
	}
	col := grep.NewCollector(plan.Re, q)
	part, err := b.Store.Scan(ctx, tq, localindex.ScanOptions{Filter: lf, MaxScan: maxScan, Oldest: q.Sort == format.SortOldest, Budget: localindex.Budget{Timeout: q.Timeout}},
		func(r *localindex.Row) bool {
			return col.Add(grep.Row{Session: r.SessionID, SHA: r.ContentSHA, Text: r.Text, Ref: r})
		})
	if err != nil {
		return nil, err
	}
	res := col.Result()
	page := &format.Page{Hits: []format.Hit{}, Total: res.Total, TotalSessions: res.TotalSessions, Offset: q.Offset, Next: res.Next,
		Exact: !res.Capped && !part.Truncated}
	_, all := tq.MatchExpr()
	switch {
	case part.Reason == localindex.ReasonTimeout || part.Reason == localindex.ReasonVerifyBudget:
		page.Truncated, page.Reason = true, budgetNote(part)
	case part.Reason == localindex.ReasonScanLimit:
		page.Truncated = true
		page.Reason = fmt.Sprintf("unindexed: the pattern has no run of 3 characters every match must contain, so only the newest %d messages passing the filters were checked; add a longer literal or narrow with --agent, --repo, --kind or --since", maxScan)
	case all:
		page.Notes = append(page.Notes, "unindexed: the pattern has no run of 3 characters every match must contain, so messages were scanned newest first")
	}
	if q.Sort == format.SortRelevance && page.Truncated {
		page.Notes = append(page.Notes, "ranked by matches per message among the messages checked")
	}
	rows := make([]*localindex.Row, len(res.Hits))
	for i, h := range res.Hits {
		rows[i] = h.Ref.(*localindex.Row)
	}
	if page.Hits, page.SessionInfo, err = b.hits(ctx, rows); err != nil {
		return nil, err
	}
	for i, h := range res.Hits {
		page.Hits[i].Lines, page.Hits[i].MoreLines, page.Hits[i].Copies = h.Lines, h.MoreLines, h.Copies
	}
	if len(res.Sessions) > 0 {
		var convIDs []int64
		for _, s := range res.Sessions {
			convIDs = append(convIDs, s.Ref.(*localindex.Row).ConversationID)
		}
		infos, err := b.infos(ctx, convIDs)
		if err != nil {
			return nil, err
		}
		for _, s := range res.Sessions {
			info := infos[s.Ref.(*localindex.Row).ConversationID]
			info.Hits = s.Hits
			page.Sessions = append(page.Sessions, info)
		}
	}
	return page, nil
}

// budgetNote is the one line a grep that ran out of budget ends with
// (decision D8).
func budgetNote(p localindex.Partial) string {
	what := fmt.Sprintf("timed out after %s", p.Elapsed.Round(100*time.Millisecond))
	if p.Reason == localindex.ReasonVerifyBudget {
		what = "stopped at the verification budget"
	}
	total := "?"
	if p.Total >= 0 {
		total = strconv.Itoa(p.Total)
	}
	return fmt.Sprintf("%s: checked %d of %s candidates, newest first; narrow with --agent, --repo, --kind or --since, or a longer literal", what, p.Checked, total)
}

// Search is ranked BM25 over the token index. Quoted phrases must appear
// as written. When no message holds every term, it ranks messages holding
// any of the terms, stopwords dropped, and says so. Notes carry the rank
// cap (decision D2) and the budget.
func (b *Backend) Search(ctx context.Context, q format.SearchQuery, f format.Filters) (*format.Page, error) {
	lf, err := b.filter(ctx, f)
	if err != nil {
		return nil, err
	}
	words, phrases := format.SplitQuery(q.Query)
	if len(words)+len(phrases) == 0 {
		return nil, bad("query is empty")
	}
	// One hit past the page says whether a next page exists.
	n := limit(q.Limit)
	sort, err := format.SortFor("search", f.Sort)
	if err != nil {
		return nil, err
	}
	o := localindex.SearchOptions{Filter: lf, Limit: n + 1, Offset: q.Offset, Phrases: phrases, Budget: localindex.Budget{Timeout: q.Timeout}}
	if sort != format.SortRelevance {
		o.ByTime = sort
	}
	text := strings.Join(append(append([]string{}, words...), phrases...), " ")
	res, err := b.Store.Rank(ctx, text, o)
	if err != nil {
		return nil, err
	}
	page := &format.Page{Hits: []format.Hit{}, Offset: q.Offset}
	if len(res.Hits) == 0 && !res.Truncated && len(words) > 1 {
		none := q.Offset == 0
		if !none { // a later page: did the every-term query match at all?
			first, err := b.Store.Rank(ctx, text, localindex.SearchOptions{Filter: lf, Limit: 1, Phrases: phrases, ByTime: o.ByTime})
			none = err == nil && len(first.Hits) == 0 && !first.Truncated
		}
		if none {
			kept := format.DropStopwords(words)
			o.AnyTerm = true
			if res, err = b.Store.Rank(ctx, strings.Join(append(kept, phrases...), " "), o); err != nil {
				return nil, err
			}
			if len(res.Hits) > 0 {
				page.Notes = append(page.Notes, "no message has every term; ranked messages with any of: "+strings.Join(kept, " ")+" (they may not answer the question)")
			}
		}
	}
	switch {
	case res.Truncated:
		page.Truncated = true
		page.Reason = fmt.Sprintf("timed out after %s; narrow with more terms or --agent, --repo, --kind or --since", res.Elapsed.Round(100*time.Millisecond))
	case res.RankCapped:
		page.Notes = append(page.Notes, fmt.Sprintf("ranked newest %s of %d matches — add terms or filters", thousands(res.Cap), res.Total))
	}
	hits := res.Hits
	more := len(hits) > n
	if more {
		hits = hits[:n]
	}
	rows := make([]*localindex.Row, len(hits))
	for i := range hits {
		rows[i] = &hits[i].Row
	}
	if page.Hits, page.SessionInfo, err = b.hits(ctx, rows); err != nil {
		return nil, err
	}
	for i := range page.Hits {
		page.Hits[i].Score, page.Hits[i].Snippet, page.Hits[i].TextLine, page.Hits[i].Copies = hits[i].Score, hits[i].Snippet, hits[i].Line, hits[i].Copies
	}
	if more {
		page.Next = q.Offset + len(hits)
	}
	return page, nil
}

// Sessions lists conversations by last activity, newest first: those
// matching glob (session id, title, repo or cwd; see format.GlobLike) and
// the filters' agent, repo, device, time (on last activity) and subagent
// conditions. One page of f.Limit after cursor (format.SessionCursor).
func (b *Backend) Sessions(ctx context.Context, glob, cursor string, f format.Filters) (*format.Sessions, error) {
	lf, err := b.filter(ctx, f)
	if err != nil {
		return nil, err
	}
	sort, err := format.SortFor("sessions", f.Sort)
	if err != nil {
		return nil, err
	}
	n := limit(f.Limit)
	o := localindex.ListOptions{Filter: lf, Like: format.GlobLike(glob), Limit: n + 1, Oldest: sort == format.SortOldest}
	if cursor != "" {
		k, err := format.ParseSessionCursor(cursor)
		if err != nil {
			return nil, err
		}
		id, err := strconv.ParseInt(k.ID, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: cursor %q: not a sessions cursor", format.ErrBadRequest, cursor)
		}
		o.After = &localindex.ListKey{At: k.At / 1000, Undated: k.Undated, ID: id}
	}
	rows, err := b.Store.ListConversations(ctx, o)
	if err != nil {
		return nil, err
	}
	out := &format.Sessions{Sessions: []format.ConversationInfo{}}
	if len(rows) > n {
		rows, out.HasMore = rows[:n], true
	}
	var ids []string
	for _, c := range rows {
		ids = append(ids, c.SessionID, c.ParentSessionID)
	}
	short, err := b.addresses(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, c := range rows {
		out.Sessions = append(out.Sessions, info(c, short))
	}
	if out.HasMore {
		out.Next = format.SessionCursor(out.Sessions[n-1])
	}
	return out, nil
}

// resolveSession returns the one session id starting with prefix (an
// exact id wins over longer ones).
func (b *Backend) resolveSession(ctx context.Context, prefix string) (string, error) {
	ids, err := b.Store.SessionsWithPrefix(ctx, prefix, 6)
	if err != nil {
		return "", err
	}
	switch {
	case len(ids) == 0:
		return "", fmt.Errorf("%w: no session starts with %q", format.ErrNotFound, prefix)
	case len(ids) == 1 || ids[0] == prefix:
		return ids[0], nil
	}
	return "", format.AmbiguousError(prefix, ids)
}

// locate finds the focus row of an address, the addressed line, and
// whether the address names a whole session.
func (b *Backend) locate(ctx context.Context, address string) (*localindex.Row, int, bool, error) {
	a, err := format.ParseAddress(address)
	if err != nil {
		return nil, 0, false, err
	}
	switch a.Kind {
	case format.AddrMessage:
		sid, err := b.resolveSession(ctx, a.Session)
		if err != nil {
			return nil, 0, false, err
		}
		r, err := b.Store.MessageByOrdinal(ctx, sid, a.Ordinal)
		if err == nil && r == nil {
			err = fmt.Errorf("%w: session %s has no message at ordinal %d", format.ErrNotFound, sid, a.Ordinal)
		}
		return r, a.Line, false, err
	case format.AddrPath:
		p := a.Path
		if strings.HasPrefix(p, "~/") {
			home, _ := os.UserHomeDir()
			p = filepath.Join(home, p[2:])
		}
		rows, err := b.Store.MessagesAtLine(ctx, p, a.PathNo)
		if err != nil {
			return nil, 0, false, err
		}
		if len(rows) == 0 {
			return nil, 0, false, fmt.Errorf("%w: no indexed message at %s:%d", format.ErrNotFound, p, a.PathNo)
		}
		return rows[0], 0, false, nil
	}
	// A bare token: a message id (short digit strings), else a session
	// prefix, else a message id.
	id, numErr := strconv.ParseInt(a.Token, 10, 64)
	byID := func() (*localindex.Row, error) {
		rows, err := b.Store.Messages(ctx, []int64{id})
		if err != nil || len(rows) == 0 {
			return nil, err
		}
		return rows[0], nil
	}
	if numErr == nil && id > 0 && len(a.Token) < format.MinPrefix {
		r, err := byID()
		if err != nil || r != nil {
			return r, 0, false, err
		}
	}
	sid, serr := b.resolveSession(ctx, a.Token)
	if serr == nil {
		r, err := b.Store.FirstMessage(ctx, sid)
		if err == nil && r == nil {
			err = fmt.Errorf("%w: session %s has no messages", format.ErrNotFound, sid)
		}
		return r, 0, true, err
	}
	if numErr == nil && id > 0 {
		if r, err := byID(); err != nil || r != nil {
			return r, 0, false, err
		}
	}
	return nil, 0, false, serr
}

// Read returns the message an address names with its neighbours (for a
// session address: its first messages), the focus text cut to the line
// offset and character budget, neighbours to a quarter of it.
func (b *Backend) Read(ctx context.Context, q format.ReadQuery, f format.Filters) (*format.Context, error) {
	var focus *localindex.Row
	var line int
	var session bool
	var err error
	if q.Self { // the caller's own session: its exact id, never a prefix
		session = true
		focus, err = b.Store.FirstMessage(ctx, q.Address)
		if err == nil && focus == nil {
			err = fmt.Errorf("%w: your session %s is not indexed yet", format.ErrNotFound, q.Address)
		}
	} else {
		focus, line, session, err = b.locate(ctx, q.Address)
	}
	if err != nil {
		return nil, err
	}
	if q.Outline {
		return b.outline(ctx, focus.ConversationID, q)
	}
	before, after := q.Before, q.After
	if session && before == 0 && after == 0 {
		after = sessionAfter
	}
	if before < 0 || after < 0 || before > maxContext || after > maxContext {
		return nil, bad("messages before and after must each be between 0 and %d", maxContext)
	}
	rows, err := b.Store.Context(ctx, focus.ID, before+1, after+1, localindex.Filter{IncludeSuperseded: f.IncludeSuperseded, IncludeBranches: f.IncludeBranches})
	if err != nil {
		return nil, err
	}
	at := 0
	for i, r := range rows {
		if r.ID == focus.ID {
			at = i
		}
	}
	cx := &format.Context{Focus: id(focus.ID), Line: line}
	if at > before {
		rows, at, cx.MoreBefore = rows[at-before:], before, true
	}
	if len(rows)-at-1 > after {
		rows, cx.MoreAfter = rows[:at+after+1], true
	}
	msgs, err := b.messages(ctx, rows)
	if err != nil {
		return nil, err
	}
	budget := q.MaxChars
	if budget <= 0 {
		budget = format.DefaultMaxChars
	}
	for i := range msgs {
		m := &msgs[i]
		from, chars := 1, max(budget/4, 200)
		if m.ID == cx.Focus {
			from, chars = q.LineOffset, budget
			if from == 0 && line > 0 {
				from = max(1, line-5)
			}
		}
		stored := rows[i]
		e := format.Cut(m.Text, from, chars)
		m.Text, m.Lines, m.LineFrom, m.LineTo, m.Clipped = e.Text, e.Lines, e.From, e.To, e.Clipped
		if stored.FullLen > len(stored.Text) {
			m.StoredLen, m.Clipped = len(stored.Text), true
		}
	}
	cx.Messages = msgs
	infos, err := b.infos(ctx, []int64{focus.ConversationID})
	if err != nil {
		return nil, err
	}
	cx.Conversation = infos[focus.ConversationID]
	return cx, nil
}

// outline answers read --outline for a conversation.
func (b *Backend) outline(ctx context.Context, convID int64, q format.ReadQuery) (*format.Context, error) {
	n, err := format.OutlinePage(q)
	if err != nil {
		return nil, err
	}
	var after *localindex.OutlineKey
	if q.Cursor != "" {
		ord, sid, err := format.ParseOutlineCursor(q.Cursor)
		if err != nil {
			return nil, err
		}
		id, err := strconv.ParseInt(sid, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: cursor %q: not an outline cursor", format.ErrBadRequest, q.Cursor)
		}
		after = &localindex.OutlineKey{Ordinal: ord, ID: id}
	}
	o, err := b.Store.Outline(ctx, convID, after, n+1)
	if err != nil {
		return nil, err
	}
	more := len(o.Rows) > n
	if more {
		o.Rows = o.Rows[:n]
	}
	infos, err := b.infos(ctx, []int64{convID})
	if err != nil {
		return nil, err
	}
	cx := &format.Context{Conversation: infos[convID], Messages: []format.Message{}, Outline: []format.OutlineEntry{}, OutlineMore: more}
	var sids []string
	for _, r := range o.Rows {
		sids = append(sids, r.SessionID)
	}
	for _, kids := range o.Spawned {
		sids = append(sids, kids...)
	}
	short, err := b.addresses(ctx, sids)
	if err != nil {
		return nil, err
	}
	root := cx.Conversation.Repo
	for _, r := range o.Rows {
		e := format.NewOutlineEntry(format.MessageAddress(short[r.SessionID], r.Ordinal), id(r.ID), r.Ordinal, tsPtr(r.TS), r.Kind.String(), r.ToolName, r.Text, root)
		e.Error = r.IsError || r.ToolCallID != "" && o.Failed[r.ToolCallID]
		for _, k := range o.Spawned[r.ID] {
			e.Subagents = append(e.Subagents, short[k])
		}
		cx.Outline = append(cx.Outline, e)
	}
	if more {
		cx.OutlineNext = format.OutlineCursor(cx.Outline[len(cx.Outline)-1])
	}
	return cx, nil
}

// RawAt returns the transcript bytes of the message an address names.
func (b *Backend) RawAt(ctx context.Context, address string) ([]byte, error) {
	r, _, _, err := b.locate(ctx, address)
	if err != nil {
		return nil, err
	}
	return b.rawRow(ctx, r)
}

// addresses maps session ids to their shortest unique prefixes.
func (b *Backend) addresses(ctx context.Context, ids []string) (map[string]string, error) {
	var want []string
	for _, id := range ids {
		if id != "" {
			want = append(want, id)
		}
	}
	nb, err := b.Store.SessionNeighbours(ctx, want)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(nb))
	for id, others := range nb {
		out[id] = format.ShortPrefix(id, others)
	}
	return out, nil
}

// hits converts rows to format hits with provenance, titles and
// addresses, and describes their sessions, in order of first hit.
func (b *Backend) hits(ctx context.Context, rows []*localindex.Row) ([]format.Hit, []format.ConversationInfo, error) {
	out := []format.Hit{}
	if len(rows) == 0 {
		return out, nil, nil
	}
	ids := make([]int64, len(rows))
	var convIDs []int64
	var sessions []string
	seen := map[int64]bool{}
	for i, r := range rows {
		ids[i] = r.ID
		if !seen[r.ConversationID] {
			seen[r.ConversationID] = true
			convIDs = append(convIDs, r.ConversationID)
			sessions = append(sessions, r.SessionID)
		}
	}
	srcs, err := b.Store.RowSources(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	infos, err := b.infos(ctx, convIDs)
	if err != nil {
		return nil, nil, err
	}
	short, err := b.addresses(ctx, sessions)
	if err != nil {
		return nil, nil, err
	}
	for _, r := range rows {
		h := format.Hit{Address: format.MessageAddress(short[r.SessionID], r.Ordinal), MessageID: id(r.ID), ConversationID: id(r.ConversationID),
			Agent: string(r.Agent), SessionID: r.SessionID, Ordinal: r.Ordinal, Title: infos[r.ConversationID].Title, Repo: repoOf(r.RepoRoot, r.Cwd),
			Device: r.DeviceID, Branches: infos[r.ConversationID].Branches, Kind: r.Kind.String(), ToolName: r.ToolName, IsError: r.IsError, TS: tsPtr(r.TS),
			Superseded: r.Superseded, OffPath: offPath(r), Provenance: provenance(r, srcs[r.ID])}
		out = append(out, h)
	}
	list := make([]format.ConversationInfo, 0, len(convIDs))
	for _, c := range convIDs {
		list = append(list, infos[c])
	}
	return out, list, nil
}

func repoOf(root, cwd string) string {
	if root != "" {
		return root
	}
	return cwd
}

func info(c localindex.ConversationRow, short map[string]string) format.ConversationInfo {
	i := format.ConversationInfo{Address: short[c.SessionID], ID: id(c.ID), Agent: string(c.Agent), SessionID: c.SessionID, Title: c.Title, Cwd: c.Cwd,
		Repo: repoOf(c.RepoRoot, c.Cwd), Device: c.DeviceID, StartedAt: tsPtr(c.StartedAt), LastActivityAt: tsPtr(c.LastActivityAt), Depth: c.Depth,
		Messages: c.Messages, ParentSession: short[c.ParentSessionID], Branches: c.Branches, Digest: format.ParseDigest(c.Digest)}
	if c.ParentConversationID != 0 {
		i.ParentConversationID = id(c.ParentConversationID)
	}
	if c.SpawnedByMessageID != 0 {
		i.SpawnedByMessageID = id(c.SpawnedByMessageID)
	}
	return i
}

func (b *Backend) infos(ctx context.Context, convIDs []int64) (map[int64]format.ConversationInfo, error) {
	out := map[int64]format.ConversationInfo{}
	if len(convIDs) == 0 {
		return out, nil
	}
	cs, err := b.Store.ListConversations(ctx, localindex.ListOptions{Filter: localindex.Filter{Conversations: convIDs}, IncludeDeleted: true, Limit: len(convIDs)})
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, c := range cs {
		ids = append(ids, c.SessionID, c.ParentSessionID)
	}
	short, err := b.addresses(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, c := range cs {
		out[c.ID] = info(c, short)
	}
	return out, nil
}

func (b *Backend) messages(ctx context.Context, rows []*localindex.Row) ([]format.Message, error) {
	ids := make([]int64, len(rows))
	var sessions []string
	for i, r := range rows {
		ids[i] = r.ID
		sessions = append(sessions, r.SessionID)
	}
	srcs, err := b.Store.RowSources(ctx, ids)
	if err != nil {
		return nil, err
	}
	short, err := b.addresses(ctx, sessions)
	if err != nil {
		return nil, err
	}
	out := make([]format.Message, 0, len(rows))
	for _, r := range rows {
		out = append(out, format.Message{Address: format.MessageAddress(short[r.SessionID], r.Ordinal), ID: id(r.ID), NativeID: r.NativeID,
			Ordinal: r.Ordinal, Kind: r.Kind.String(), Role: r.Role, ToolName: r.ToolName, ToolCallID: r.ToolCallID, IsError: r.IsError,
			TS: tsPtr(r.TS), Text: r.Text, TextLen: r.FullLen, Version: r.Version, Superseded: r.Superseded, OffPath: offPath(r),
			Provenance: provenance(r, srcs[r.ID])})
	}
	return out, nil
}

func id(v int64) string { return strconv.FormatInt(v, 10) }

func tsPtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func offPath(r *localindex.Row) bool { return r.OnActivePath != nil && !*r.OnActivePath }

func provenance(r *localindex.Row, src localindex.RowSource) format.Provenance {
	p := format.Provenance{SourceID: id(src.SourceID), Path: r.SourcePath, Generation: src.Generation, LineNo: r.LineNo, Locator: r.Locator}
	if r.ByteLen > 0 {
		off := r.ByteOffset
		p.ByteOffset, p.ByteLen = &off, r.ByteLen
	}
	return p
}

// thousands renders 20000 as 20k.
func thousands(n int) string {
	if n >= 1000 && n%1000 == 0 {
		return strconv.Itoa(n/1000) + "k"
	}
	return strconv.Itoa(n)
}
