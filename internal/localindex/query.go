package localindex

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/flopwire/flopwire/internal/transcript"
)

// Filter narrows search, find, context and listing (spec §8). The zero
// value is the default view: every agent, repo, device, kind and time, with
// subagents, without superseded rows and without off-path branches.
type Filter struct {
	Agents    []transcript.Agent
	Repos     []string // repo_root or cwd equals, or lies under, one of these
	RepoLikes []string // or its repo root (its cwd without one) matches one of these LIKE patterns (backslash escapes)
	Devices   []string
	// Branches keeps conversations one of whose git branches matches one
	// of these LIKE patterns (backslash escapes).
	Branches []string
	// IdleBefore, when set, keeps conversations whose last activity is
	// before it (leaving out live ones).
	IdleBefore   time.Time
	Kinds        []transcript.Kind
	ExcludeKinds []transcript.Kind
	Tools        []string  // tool names, case-insensitive
	Since        time.Time // ts >= Since
	Until        time.Time // ts < Until

	ExcludeSubagents  bool // depth > 0 conversations are dropped
	IncludeSuperseded bool // drop the "not superseded" condition
	IncludeBranches   bool // drop the "on_active_path is not false" condition

	// Self-session exclusion (decision 11) and scoping.
	ExcludeConversations []int64
	ExcludeSessions      []string // native session ids
	Conversations        []int64  // when set, only these conversations
}

// where renders f as SQL over messages m and conversations c.
func (f *Filter) where() (string, []any) {
	var conds []string
	var args []any
	in := func(col string, n int) string {
		return col + " IN (" + strings.TrimSuffix(strings.Repeat("?,", n), ",") + ")"
	}
	if !f.IncludeSuperseded {
		conds = append(conds, "m.superseded = 0")
	}
	if !f.IncludeBranches {
		conds = append(conds, "m.on_active_path IS NOT 0")
	}
	if len(f.Agents) > 0 {
		conds = append(conds, in("c.agent", len(f.Agents)))
		for _, a := range f.Agents {
			args = append(args, string(a))
		}
	}
	if len(f.Repos)+len(f.RepoLikes) > 0 {
		var ors []string
		for _, r := range f.Repos {
			r = strings.TrimSuffix(r, "/")
			ors = append(ors, "(c.repo_root = ? OR substr(c.repo_root, 1, ?) = ? OR c.cwd = ? OR substr(c.cwd, 1, ?) = ?)")
			args = append(args, r, len(r)+1, r+"/", r, len(r)+1, r+"/")
		}
		for _, p := range f.RepoLikes {
			ors = append(ors, `ifnull(c.repo_root, c.cwd) LIKE ? ESCAPE '\'`)
			args = append(args, p)
		}
		conds = append(conds, "("+strings.Join(ors, " OR ")+")")
	}
	if len(f.Devices) > 0 {
		conds = append(conds, in("c.device_id", len(f.Devices)))
		for _, d := range f.Devices {
			args = append(args, d)
		}
	}
	if !f.IdleBefore.IsZero() {
		conds = append(conds, "ifnull(c.last_activity_at, 0) < ?")
		args = append(args, f.IdleBefore.UnixMilli())
	}
	if len(f.Branches) > 0 {
		var ors []string
		for _, b := range f.Branches {
			ors = append(ors, `b.value LIKE ? ESCAPE '\'`)
			args = append(args, b)
		}
		conds = append(conds, "EXISTS (SELECT 1 FROM json_each(ifnull(c.branches, '[]')) b WHERE "+strings.Join(ors, " OR ")+")")
	}
	if len(f.Kinds) > 0 {
		conds = append(conds, in("m.kind", len(f.Kinds)))
		for _, k := range f.Kinds {
			args = append(args, k.String())
		}
	}
	if len(f.ExcludeKinds) > 0 {
		conds = append(conds, "NOT "+in("m.kind", len(f.ExcludeKinds)))
		for _, k := range f.ExcludeKinds {
			args = append(args, k.String())
		}
	}
	if len(f.Tools) > 0 {
		conds = append(conds, in("lower(m.tool_name)", len(f.Tools)))
		for _, t := range f.Tools {
			args = append(args, strings.ToLower(t))
		}
	}
	if !f.Since.IsZero() {
		conds = append(conds, "m.ts >= ?")
		args = append(args, f.Since.UnixMilli())
	}
	if !f.Until.IsZero() {
		conds = append(conds, "m.ts < ?")
		args = append(args, f.Until.UnixMilli())
	}
	if f.ExcludeSubagents {
		conds = append(conds, "c.depth = 0 AND c.parent_session_id IS NULL")
	}
	if len(f.ExcludeConversations) > 0 {
		conds = append(conds, "NOT "+in("m.conversation_id", len(f.ExcludeConversations)))
		for _, id := range f.ExcludeConversations {
			args = append(args, id)
		}
	}
	if len(f.ExcludeSessions) > 0 {
		conds = append(conds, "NOT "+in("c.session_id", len(f.ExcludeSessions)))
		for _, id := range f.ExcludeSessions {
			args = append(args, id)
		}
	}
	if len(f.Conversations) > 0 {
		conds = append(conds, in("m.conversation_id", len(f.Conversations)))
		for _, id := range f.Conversations {
			args = append(args, id)
		}
	}
	if len(conds) == 0 {
		return "1", nil
	}
	return strings.Join(conds, " AND "), args
}

// KindInjected is the kind of text a harness injected into a user turn
// (CLAUDE.md, AGENTS.md, system reminders). Search and find leave these
// rows out unless the filter names kinds (decision D6).
const KindInjected = "injected"

// hitWhere is where for search and find: it also leaves out injected rows
// unless Kinds is set.
func (f *Filter) hitWhere() (string, []any) {
	where, args := f.where()
	if len(f.Kinds) == 0 {
		where += " AND m.kind <> '" + KindInjected + "'"
	}
	return where, args
}

// convJoin joins conversations c when the filter reads its columns.
func (f *Filter) convJoin() string {
	if f.onConversations() {
		return ` JOIN conversations c ON c.id = m.conversation_id `
	}
	return ` `
}

func (f *Filter) onConversations() bool {
	return len(f.Agents)+len(f.Repos)+len(f.RepoLikes)+len(f.Devices)+len(f.Branches)+len(f.ExcludeSessions) > 0 || f.ExcludeSubagents || !f.IdleBefore.IsZero()
}

// split separates the conditions on conversation columns (cf, nil when
// there are none) from those on message columns (mf).
func (f *Filter) split() (mf Filter, cf *Filter) {
	mf = *f
	mf.Agents, mf.Repos, mf.RepoLikes, mf.Devices, mf.Branches, mf.ExcludeSessions, mf.ExcludeSubagents = nil, nil, nil, nil, nil, nil, false
	mf.IdleBefore = time.Time{}
	if !f.onConversations() {
		return mf, nil
	}
	return mf, &Filter{Agents: f.Agents, Repos: f.Repos, RepoLikes: f.RepoLikes, Devices: f.Devices, Branches: f.Branches, IdleBefore: f.IdleBefore, ExcludeSessions: f.ExcludeSessions,
		ExcludeSubagents: f.ExcludeSubagents, IncludeSuperseded: true, IncludeBranches: true}
}

// Row is one message with its provenance.
type Row struct {
	ID             int64
	ConversationID int64
	SessionID      string
	Agent          transcript.Agent
	DeviceID       string
	RepoRoot       string
	Cwd            string
	Depth          int
	SourcePath     string

	NativeID       string
	Part           int
	ParentNativeID string
	Ordinal        int64
	Kind           transcript.Kind
	Role           string
	ToolName       string
	ToolCallID     string
	IsError        bool
	TS             time.Time
	TextLen        int
	FullLen        int
	Version        int
	Superseded     bool
	OnActivePath   *bool
	LineNo         int64
	ByteOffset     int64
	ByteLen        int64
	Locator        string
	ContentSHA     [32]byte // sha256 of the message's whole text: equal for identical text

	Text string // decompressed stored text
}

const rowCols = `m.id, m.conversation_id, c.session_id, c.agent, c.device_id, ifnull(c.repo_root, ''), ifnull(c.cwd, ''), c.depth,
	ifnull(s.path, ''), ifnull(m.native_id, ''), m.part, ifnull(m.parent_native_id, ''), m.ordinal, m.kind, ifnull(m.role, ''),
	ifnull(m.tool_name, ''), ifnull(m.tool_call_id, ''), ifnull(m.is_error, 0), m.ts, m.text_len, m.full_len, m.version,
	m.superseded, m.on_active_path, ifnull(m.line_no, 0), ifnull(m.byte_offset, 0), ifnull(m.byte_len, 0), ifnull(m.locator, ''), m.content_sha, m.text`

const rowFrom = ` FROM messages m JOIN conversations c ON c.id = m.conversation_id LEFT JOIN sources s ON s.id = m.source_id `

type scanner interface{ Scan(...any) error }

func scanRow(sc scanner, extra ...any) (*Row, error) {
	var (
		r      Row
		agent  string
		kind   string
		isErr  int
		ts     sql.NullInt64
		sup    int
		onPath sql.NullInt64
		text   []byte
		sha    []byte
	)
	dest := []any{&r.ID, &r.ConversationID, &r.SessionID, &agent, &r.DeviceID, &r.RepoRoot, &r.Cwd, &r.Depth,
		&r.SourcePath, &r.NativeID, &r.Part, &r.ParentNativeID, &r.Ordinal, &kind, &r.Role,
		&r.ToolName, &r.ToolCallID, &isErr, &ts, &r.TextLen, &r.FullLen, &r.Version,
		&sup, &onPath, &r.LineNo, &r.ByteOffset, &r.ByteLen, &r.Locator, &sha, &text}
	if err := sc.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	r.Agent = transcript.Agent(agent)
	r.Kind, _ = transcript.ParseKind(kind)
	copy(r.ContentSHA[:], sha)
	r.IsError, r.Superseded = isErr != 0, sup != 0
	if ts.Valid {
		r.TS = time.UnixMilli(ts.Int64)
	}
	if onPath.Valid {
		b := onPath.Int64 != 0
		r.OnActivePath = &b
	}
	var err error
	if r.Text, err = decompress(text); err != nil {
		return nil, fmt.Errorf("row %d: %w", r.ID, err)
	}
	return &r, nil
}

// SearchOptions tune Search.
type SearchOptions struct {
	Filter
	Limit      int  // default 20
	Offset     int  // in collapsed hits
	AnyTerm    bool // OR the terms instead of AND
	SnippetLen int  // bytes of context around the first hit; default 160
	// Phrases must each appear in a hit's text (a case-insensitive
	// substring, through the trigram index); they rank by their tokens,
	// which the query should also hold.
	Phrases []string
	// ByTime, "newest" or "oldest", orders the matches by message time
	// instead of by rank (no rank cap applies).
	ByTime string
	Budget
}

// Hit is a ranked search result.
type Hit struct {
	Row
	Score   float64 // bm25; lower is better
	Snippet string
	Line    int // the line of the text the snippet starts on, 1-based
	// Highlights are byte ranges of matched terms within Snippet.
	Highlights [][2]int
	// Copies counts lower-ranked hits with identical text folded into
	// this one (decision D6).
	Copies int
}

// SearchResult is one page of ranked hits.
type SearchResult struct {
	Hits []Hit
	// RankCapped: more than SearchRankCap rows match the query and pass
	// the filters, so hits were ranked among the newest SearchRankCap of
	// them by message time (decision D2). Total is then the number of rows
	// matching the query (before filters); otherwise it is -1.
	RankCapped bool
	Total      int
	Cap        int // the rank cap in force (SearchRankCap)
	Partial
}

// Search is Rank returning only the hits.
func (s *Store) Search(ctx context.Context, query string, o SearchOptions) ([]Hit, error) {
	r, err := s.Rank(ctx, query, o)
	return r.Hits, err
}

// Rank runs ranked BM25 over fts_tok. The query is free text: each token
// (split and trimmed like the index) must match, or any when AnyTerm; a
// trailing '*' makes a prefix term. Hits with identical text collapse into
// the best-ranked one. Ranking reads only the index; text is decompressed
// for the returned hits only. When the budget's timeout ends the query,
// the result is empty and flagged, not an error.
func (s *Store) Rank(ctx context.Context, query string, o SearchOptions) (SearchResult, error) {
	res := SearchResult{Total: -1, Cap: searchRankCap}
	res.Partial.Total = -1
	expr, terms := tokQuery(query, o.AnyTerm)
	if expr == "" {
		return res, nil
	}
	if o.Limit <= 0 {
		o.Limit = 20
	}
	if o.SnippetLen <= 0 {
		o.SnippetLen = 160
	}
	d := newDeadline(ctx, o.Budget.norm())
	defer d.cancel()
	var groups []group
	var err error
	if o.ByTime != "" {
		groups, err = s.timeGroups(d.ctx, expr, &o)
	} else {
		groups, err = s.rankedGroups(d.ctx, expr, &o, &res)
	}
	if err == nil {
		var loaded []*Row
		if loaded, err = s.Messages(d.ctx, groupIDs(groups)); err == nil {
			byID := make(map[int64]group, len(groups))
			for _, g := range groups {
				byID[g.id] = g
			}
			for _, r := range loaded {
				g := byID[r.ID]
				h := Hit{Row: *r, Score: g.score, Copies: g.copies}
				h.Snippet, h.Highlights, h.Line = snippet(r.Text, terms, o.SnippetLen)
				res.Hits = append(res.Hits, h)
			}
		}
	}
	if err = d.settle(&res.Partial, err); err != nil || res.Truncated {
		res.Hits = nil
		return res, err
	}
	return res, nil
}

// Ranked search reads the best candidates from fts_tok alone first (FTS5
// sorts by rank itself for ORDER BY rank LIMIT), then applies the filters
// to those rows only. Joining every match to messages and conversations
// before sorting cost 0.4-0.9s for a term in 100k rows ("error", "test").
// When the filters leave too few of the candidates, the candidate pool
// grows by searchGrowth up to searchMaxCandidates, and past that the query
// ranks every match that passes the filters.
//
// bm25 itself costs about 2µs a row even in native SQLite (0.22s for
// "error", 109k rows; 0.31s for "test", 160k). An expression whose matches
// passing the filters number more than SearchRankCap is therefore ranked
// among the newest SearchRankCap of them by message time (decision D7),
// not all of them: recall of old rows for very common terms, traded for
// latency.
const SearchRankCap = 20000

// Variables so tests can drive every path on a small index.
var (
	searchMinCandidates = 256
	searchGrowth        = 8
	searchMaxCandidates = 1 << 15
	searchRankCap       = SearchRankCap
	searchMaxList       = 1 << 13 // scored rows read to fill a page of collapsed hits
)

// SetSearchRankCap sets the rank cap and returns a function restoring
// it. For tests of callers that report the cap (decision D2).
func SetSearchRankCap(n int) (restore func()) {
	old := searchRankCap
	searchRankCap = n
	return func() { searchRankCap = old }
}

// group is one collapsed hit: the best-ranked row of its text.
type group struct {
	id     int64
	score  float64
	copies int
}

func groupIDs(gs []group) []int64 {
	ids := make([]int64, len(gs))
	for i, g := range gs {
		ids[i] = g.id
	}
	return ids
}

// rankedGroups returns one page of collapsed hits, best first. It reads
// scored rows in growing lists until the list holds enough distinct texts
// for the page, or holds every match.
func (s *Store) rankedGroups(ctx context.Context, expr string, o *SearchOptions, res *SearchResult) ([]group, error) {
	need := o.Offset + o.Limit
	var n int
	if err := s.rdb.QueryRowContext(ctx, `SELECT count(*) FROM fts_tok WHERE fts_tok MATCH ?`, expr).Scan(&n); err != nil {
		return nil, err
	}
	// Over the cap, rank only the newest matches that pass the filters
	// (all of them when they number no more than the cap). Phrases
	// restrict the ranking to the rows holding them, the same way.
	var only []int64
	var phraseIDs []int64
	if len(o.Phrases) > 0 {
		ids, err := s.phraseIDs(ctx, o.Phrases)
		if err != nil {
			return nil, err
		}
		phraseIDs, n = ids, len(ids)
	}
	if n > searchRankCap || phraseIDs != nil {
		ids := phraseIDs
		if ids == nil {
			var err error
			if ids, err = s.ftsIDs(ctx, "fts_tok", "fts_tok", expr); err != nil {
				return nil, err
			}
		}
		only = []int64{}
		err := s.newest(ctx, ids, &o.Filter, func(id int64) bool {
			only = append(only, id)
			return len(only) <= searchRankCap
		})
		if err != nil {
			return nil, err
		}
		if len(only) > searchRankCap {
			only, res.RankCapped, res.Total = only[:searchRankCap], true, n
		}
	}
	for want := max(2*need, 64); ; want *= 4 {
		list, err := s.rankedList(ctx, expr, o, want, only)
		if err != nil {
			return nil, err
		}
		groups, err := s.collapse(ctx, list)
		if err != nil {
			return nil, err
		}
		if len(groups) >= need || len(list) < want || want >= searchMaxList {
			if o.Offset >= len(groups) {
				return nil, nil
			}
			return groups[o.Offset:min(len(groups), need)], nil
		}
	}
}

// timeGroups returns one page of collapsed hits in message time order
// (o.ByTime): the matches of expr (holding every phrase) that pass the
// filters, read in growing lists until the page is full.
func (s *Store) timeGroups(ctx context.Context, expr string, o *SearchOptions) ([]group, error) {
	need := o.Offset + o.Limit
	ids, err := s.ftsIDs(ctx, "fts_tok", "fts_tok", expr)
	if err != nil {
		return nil, err
	}
	if len(o.Phrases) > 0 {
		pids, err := s.phraseIDs(ctx, o.Phrases)
		if err != nil {
			return nil, err
		}
		if pids != nil {
			set := newIDSet(pids)
			keep := ids[:0]
			for _, id := range ids {
				if set.has(id) {
					keep = append(keep, id)
				}
			}
			ids = keep
		}
	}
	for want := max(2*need, 64); ; want *= 4 {
		var list []scored
		err := s.inTimeOrder(ctx, ids, &o.Filter, o.ByTime == "oldest", func(id int64) bool {
			list = append(list, scored{id: id})
			return len(list) < want
		})
		if err != nil {
			return nil, err
		}
		groups, err := s.collapse(ctx, list)
		if err != nil {
			return nil, err
		}
		if len(groups) >= need || len(list) < want || want >= searchMaxList {
			if o.Offset >= len(groups) {
				return nil, nil
			}
			return groups[o.Offset:min(len(groups), need)], nil
		}
	}
}

// rankedList returns up to want matches passing the filters, best first
// (ties: newer row first). With only non-nil, it ranks just those rows
// (already filtered).
func (s *Store) rankedList(ctx context.Context, expr string, o *SearchOptions, want int, only []int64) ([]scored, error) {
	if only != nil {
		if len(only) == 0 {
			return nil, nil
		}
		// bm25 runs only for the listed rows; the unary + keeps the IN test
		// out of FTS5's plan (it would run the MATCH once per id), and the
		// rowid range lets FTS5 skip the rest of each doclist.
		lo, hi := slices.Min(only), slices.Max(only)
		q := `SELECT t.rowid, bm25(fts_tok) * ` + boostSQL + ` AS score FROM fts_tok t
			JOIN messages m INDEXED BY messages_meta ON m.id = t.rowid
			WHERE fts_tok MATCH ? AND t.rowid BETWEEN ? AND ? AND +t.rowid IN (SELECT value FROM json_each(?))
			ORDER BY score, t.rowid DESC LIMIT ?`
		return s.scoredList(ctx, q, []any{expr, lo, hi, int64JSON(only), want})
	}
	where, args := o.Filter.hitWhere()
	for k := max(searchMinCandidates, want*4); k <= searchMaxCandidates; k *= searchGrowth {
		cand, err := s.topCandidates(ctx, expr, k)
		if err != nil {
			return nil, err
		}
		keep, err := s.filterIDs(ctx, cand, &o.Filter)
		if err != nil {
			return nil, err
		}
		// keep is in boosted order; a row past the candidates scores no
		// better than the last candidate boosted, so the list is final
		// when its last row beats that.
		if len(cand) < k || len(keep) >= want && keep[want-1].score < conversationBoost*cand[len(cand)-1].score {
			return keep[:min(len(keep), want)], nil
		}
	}
	// Selective filters: rank every match after filtering.
	q := `SELECT m.id, f.score * ` + boostSQL + ` AS score FROM (SELECT rowid, rank AS score FROM fts_tok WHERE fts_tok MATCH ?) f
		JOIN messages m ON m.id = f.rowid ` + o.Filter.convJoin() + `
		WHERE ` + where + ` ORDER BY score, m.id DESC LIMIT ?`
	return s.scoredList(ctx, q, append(append([]any{expr}, args...), want))
}

// scoredList runs a query returning (id, score) rows.
func (s *Store) scoredList(ctx context.Context, q string, args []any) ([]scored, error) {
	rows, err := s.rdb.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []scored
	for rows.Next() {
		var c scored
		if err := rows.Scan(&c.id, &c.score); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type scored struct {
	id    int64
	score float64
}

// topCandidates returns the k best-ranked rows of fts_tok for expr (ties:
// newer first, as the page is ordered).
func (s *Store) topCandidates(ctx context.Context, expr string, k int) ([]scored, error) {
	return s.scoredList(ctx, `SELECT rowid, rank FROM fts_tok WHERE fts_tok MATCH ? ORDER BY rank, rowid DESC LIMIT ?`, []any{expr, k})
}

// filterIDs keeps the candidates that pass the filter, their scores
// boosted by kind (see conversationBoost), sorted best first.
func (s *Store) filterIDs(ctx context.Context, cand []scored, f *Filter) ([]scored, error) {
	if len(cand) == 0 {
		return nil, nil
	}
	ids := make([]int64, len(cand))
	for i, c := range cand {
		ids[i] = c.id
	}
	where, args := f.hitWhere()
	q := `SELECT m.id, m.kind FROM messages m ` + f.convJoin() + `
		WHERE m.id IN (SELECT value FROM json_each(?)) AND ` + where
	rows, err := s.rdb.QueryContext(ctx, q, append([]any{int64JSON(ids)}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	boost := make(map[int64]float64, len(cand))
	for rows.Next() {
		var id int64
		var kind string
		if err := rows.Scan(&id, &kind); err != nil {
			return nil, err
		}
		boost[id] = kindBoost(kind)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []scored
	for _, c := range cand {
		if b, ok := boost[c.id]; ok {
			out = append(out, scored{id: c.id, score: c.score * b})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score < out[j].score
		}
		return out[i].id > out[j].id
	})
	return out, nil
}

// conversationBoost weighs the bm25 score of prompts and replies (user,
// assistant, agent_message) against tool calls and their output, so a
// conversation turn outranks a log dump that matches as well. bm25 is
// negative, lower better: the boost multiplies it.
const conversationBoost = 1.5

// boostSQL is conversationBoost in SQL, over messages m.
const boostSQL = `(CASE WHEN m.kind IN ('user','assistant','agent_message') THEN 1.5 ELSE 1 END)`

func kindBoost(kind string) float64 {
	switch kind {
	case "user", "assistant", "agent_message":
		return conversationBoost
	}
	return 1
}

// collapse folds the rows of a ranked list (already in boosted order, see
// conversationBoost) with identical text (content_sha) into the first.
func (s *Store) collapse(ctx context.Context, list []scored) ([]group, error) {
	if len(list) == 0 {
		return nil, nil
	}
	ids := make([]int64, len(list))
	for i, c := range list {
		ids[i] = c.id
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, content_sha FROM messages WHERE id IN (SELECT value FROM json_each(?))`, int64JSON(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sha := make(map[int64]string, len(list))
	for rows.Next() {
		var id int64
		var h []byte
		if err := rows.Scan(&id, &h); err != nil {
			return nil, err
		}
		sha[id] = string(h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []group
	at := map[string]int{}
	for _, c := range list {
		h, ok := sha[c.id]
		if !ok { // purged since ranking
			continue
		}
		if i, dup := at[h]; dup {
			out[i].copies++
			continue
		}
		at[h] = len(out)
		out = append(out, group{id: c.id, score: c.score})
	}
	return out, nil
}

// snippet cuts a window of about n bytes around the first term occurrence
// (case-insensitive) and reports every term occurrence inside it.
func snippet(text string, terms []string, n int) (string, [][2]int, int) {
	re := termsRegexp(terms)
	first := 0
	if re != nil {
		if loc := re.FindStringIndex(text); loc != nil {
			first = loc[0]
		}
	}
	start := max(0, first-n/3)
	end := min(len(text), start+n)
	for start > 0 && !utf8.RuneStart(text[start]) {
		start--
	}
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end++
	}
	out := text[start:end]
	pre := ""
	if start > 0 {
		pre = "…"
	}
	suf := ""
	if end < len(text) {
		suf = "…"
	}
	var hl [][2]int
	if re != nil {
		for _, loc := range re.FindAllStringIndex(out, -1) {
			hl = append(hl, [2]int{loc[0] + len(pre), loc[1] + len(pre)})
		}
	}
	return pre + out + suf, hl, strings.Count(text[:start], "\n") + 1
}

// phraseIDs returns the rows whose text holds every phrase, through the
// trigram index; phrases under three characters constrain nothing.
func (s *Store) phraseIDs(ctx context.Context, phrases []string) ([]int64, error) {
	var out []int64
	first := true
	for _, p := range phrases {
		if runeLen(p) < 3 {
			continue
		}
		ids, err := s.triIDs(ctx, s.substringExpr(p))
		if err != nil {
			return nil, err
		}
		if first {
			out, first = ids, false
			continue
		}
		set := newIDSet(ids)
		keep := out[:0]
		for _, id := range out {
			if set.has(id) {
				keep = append(keep, id)
			}
		}
		out = keep
	}
	if first {
		return nil, nil
	}
	if out == nil {
		out = []int64{}
	}
	return out, nil
}

func termsRegexp(terms []string) *regexp.Regexp {
	if len(terms) == 0 {
		return nil
	}
	ts := append([]string(nil), terms...)
	sort.Slice(ts, func(i, j int) bool { return len(ts[i]) > len(ts[j]) })
	for i, t := range ts {
		ts[i] = regexp.QuoteMeta(t)
	}
	return regexp.MustCompile(`(?i)` + strings.Join(ts, "|"))
}

// FindOptions tune Find.
type FindOptions struct {
	Filter
	CaseSensitive bool
	Limit         int // matching messages returned (after collapsing copies); default 50
	// MaxScan bounds the rows read when the pattern is shorter than three
	// characters and the trigram index cannot help. Default 20000.
	MaxScan int
	// MaxLinesPerHit bounds the matching lines reported per message; default 5.
	MaxLinesPerHit int
	Budget
}

// LineMatch is one matching line inside a message's text.
type LineMatch struct {
	Line int    // 1-based line number within the message text
	Col  int    // byte column of the match within Line
	Text string // the line, truncated to 400 bytes around the match
}

// FindHit is a message containing the pattern.
type FindHit struct {
	Row
	Matches []LineMatch
	// Copies counts older matching messages with identical text folded
	// into this one (decision D6).
	Copies int
}

// FindResult is what Grep found and how much of the index it checked.
type FindResult struct {
	Hits []FindHit
	Partial
}

// ErrScanLimit is returned by Find (with partial results) when a
// short-pattern scan stopped at MaxScan rows.
var ErrScanLimit = errors.New("localindex: scan limit reached; narrow the filters")

// Find is Grep returning the hits, and ErrScanLimit with them when an
// unindexed scan stopped at its bound.
func (s *Store) Find(ctx context.Context, pattern string, o FindOptions) ([]FindHit, error) {
	r, err := s.Grep(ctx, pattern, o)
	if err == nil && r.Reason == ReasonScanLimit {
		err = ErrScanLimit
	}
	return r.Hits, err
}

// Grep returns messages whose text contains pattern, newest message first
// (by message time), stopping at Limit. Case-insensitive by default via
// the trigram index; CaseSensitive post-filters the candidates. Patterns
// under three characters fall back to a filtered scan bounded by MaxScan.
// Messages with identical text collapse into the newest.
func (s *Store) Grep(ctx context.Context, pattern string, o FindOptions) (FindResult, error) {
	if pattern == "" {
		return FindResult{}, errors.New("localindex: empty pattern")
	}
	if o.Limit <= 0 {
		o.Limit = 50
	}
	if o.MaxLinesPerHit <= 0 {
		o.MaxLinesPerHit = 5
	}
	var re *regexp.Regexp
	if !o.CaseSensitive {
		re = regexp.MustCompile(`(?i)` + regexp.QuoteMeta(pattern))
	}
	match := func(text string) [][]int {
		if re != nil {
			return re.FindAllStringIndex(text, -1)
		}
		var locs [][]int
		for off := 0; ; {
			i := strings.Index(text[off:], pattern)
			if i < 0 {
				return locs
			}
			locs = append(locs, []int{off + i, off + i + len(pattern)})
			off += i + len(pattern)
		}
	}
	var res FindResult
	var seen Collapser
	visit := func(r *Row) bool {
		if locs := match(r.Text); len(locs) > 0 {
			if i, dup := seen.Add(r); dup {
				res.Hits[i].Copies++
			} else {
				res.Hits = append(res.Hits, FindHit{Row: *r, Matches: lineMatches(r.Text, locs, o.MaxLinesPerHit)})
			}
		}
		return len(res.Hits) < o.Limit
	}
	q := &TrigramQuery{Op: TrigramAll}
	if runeLen(pattern) >= 3 {
		q = &TrigramQuery{Op: TrigramAnd, expr: s.substringExpr(pattern)}
	}
	var err error
	res.Partial, err = s.Scan(ctx, q, ScanOptions{Filter: o.Filter, MaxScan: o.MaxScan, Budget: o.Budget}, visit)
	return res, err
}

// Collapser folds rows with identical text (decision D6): Add reports the
// index of the first row added with the same text, or registers r as the
// next one.
type Collapser struct {
	at map[[32]byte]int
	n  int
}

// Add returns (index of the earlier row, true) for a copy, else
// (index of r, false).
func (c *Collapser) Add(r *Row) (int, bool) {
	if c.at == nil {
		c.at = map[[32]byte]int{}
	}
	if i, ok := c.at[r.ContentSHA]; ok {
		return i, true
	}
	c.at[r.ContentSHA] = c.n
	c.n++
	return c.n - 1, false
}

// maxSubstringTrigrams bounds the trigrams ANDed for one substring at
// detail=column; candidates are verified, so a sample is enough.
const maxSubstringTrigrams = 16

// substringExpr is the fts_tri expression for a substring of at least
// three characters: a phrase at detail=full; at detail=column (no
// positions) an AND over its distinct trigrams, sampled evenly.
func (s *Store) substringExpr(pattern string) string {
	if s.details.Tri == DetailFull {
		return ftsString(rareWindow(pattern, phraseWindow))
	}
	runes := []rune(pattern)
	seen := map[string]bool{}
	var tris []string
	for i := 0; i+3 <= len(runes); i++ {
		t := strings.ToLower(string(runes[i : i+3]))
		if !seen[t] {
			seen[t] = true
			tris = append(tris, t)
		}
	}
	if len(tris) > maxSubstringTrigrams {
		step := float64(len(tris)-1) / float64(maxSubstringTrigrams-1)
		var sample []string
		for i := range maxSubstringTrigrams {
			sample = append(sample, tris[int(float64(i)*step+0.5)])
		}
		tris = sample
	}
	parts := make([]string, len(tris))
	for i, t := range tris {
		parts[i] = ftsString(t)
	}
	return strings.Join(parts, " AND ")
}

// phraseWindow is the most characters of a substring pattern the trigram
// phrase query uses. FTS5 reads the doclist of every trigram in a phrase,
// and long patterns are full of common ones ("ing", "re ", " th"): the
// phrase "missing required scope read:project" took about 200ms against
// the real corpus where its window "read:project" takes about 20ms.
// Candidates are verified against the whole pattern, so any window is
// sound.
const phraseWindow = 12

// rareWindow returns the window of n runes of pattern whose characters are
// least common in English prose and code identifiers, by a fixed rank;
// patterns of at most n runes come back whole.
func rareWindow(pattern string, n int) string {
	r := []rune(pattern)
	if len(r) <= n {
		return pattern
	}
	score := func(c rune) int {
		c = unicode.ToLower(c)
		switch {
		case strings.ContainsRune(" etaoinsrhl", c):
			return 0
		case strings.ContainsRune("dcumfpgwybv", c):
			return 2
		case c >= 'a' && c <= 'z': // k x j q z
			return 4
		case c >= '0' && c <= '9':
			return 3
		default: // punctuation, non-ASCII
			return 3
		}
	}
	best, bestAt, cur := -1, 0, 0
	for i, c := range r {
		cur += score(c)
		if i >= n {
			cur -= score(r[i-n])
		}
		if i >= n-1 && cur > best {
			best, bestAt = cur, i-n+1
		}
	}
	return string(r[bestAt : bestAt+n])
}

// ScanOptions tune Scan.
type ScanOptions struct {
	Filter
	// MaxScan bounds the rows read by the unindexed fallback (a query with
	// no trigram constraint). Default 20000.
	MaxScan int
	// Oldest visits the oldest message first instead of the newest.
	Oldest bool
	Budget
}

// scanChunk is how many candidate rows Scan loads at a time.
const scanChunk = 64

// Scan calls visit with each row that may match the trigram query q,
// newest message first (by message time, then row id), until visit
// returns false or the candidates or the budget run out. The caller
// verifies each row's Text. With no trigram constraint it scans the rows
// the filters allow, newest first, at most MaxScan of them.
//
// Indexed, Scan reads the candidate row ids from the trigram table (Total
// counts them, before filters), orders those that pass the filters by
// message time (newest), and loads and visits them in chunks. The
// budget's timeout, or its byte budget for the text visited, stops it
// early with a truncated result.
func (s *Store) Scan(ctx context.Context, q *TrigramQuery, o ScanOptions, visit func(*Row) bool) (Partial, error) {
	b := o.Budget.norm()
	if o.MaxScan <= 0 {
		o.MaxScan = 20000
	}
	d := newDeadline(ctx, b)
	defer d.cancel()
	p := Partial{Total: -1}
	var bytes int64
	// take reports whether r may be visited, stopping on the budget.
	take := func(r *Row) bool {
		if d.ctx.Err() != nil {
			return false
		}
		if bytes >= b.VerifyBytes {
			p.stop(ReasonVerifyBudget)
			return false
		}
		bytes += int64(len(r.Text))
		p.Checked++
		return true
	}
	expr, all := q.matchExpr()
	var err error
	switch {
	case all:
		where, args := o.Filter.hitWhere()
		order := ` ORDER BY m.ts DESC, m.id DESC LIMIT ?`
		if o.Oldest {
			order = ` ORDER BY m.ts, m.id LIMIT ?`
		}
		query := `SELECT ` + rowCols + rowFrom + ` WHERE ` + where + order
		n, limited := 0, true
		err = s.stream(d.ctx, query, append(args, o.MaxScan), func(r *Row) bool {
			n++
			if !take(r) || !visit(r) {
				limited = false
				return false
			}
			return true
		})
		if err == nil && limited && n >= o.MaxScan {
			p.stop(ReasonScanLimit)
		}
	case expr == "":
		p.Total = 0
	default:
		var ids []int64
		if ids, err = s.triIDs(d.ctx, expr); err != nil {
			break
		}
		p.Total = len(ids)
		err = s.visitNewest(d.ctx, ids, &o.Filter, o.Oldest, func(r *Row) bool { return take(r) && visit(r) })
	}
	return p, d.settle(&p, err)
}

// visitNewest loads the rows of ids that pass f, newest message first (or
// oldest), a chunk at a time, and visits them until visit returns false.
func (s *Store) visitNewest(ctx context.Context, ids []int64, f *Filter, oldest bool, visit func(*Row) bool) error {
	var chunk []int64
	done := false
	flush := func() error {
		rows, err := s.Messages(ctx, chunk)
		chunk = chunk[:0]
		for _, r := range rows {
			if !visit(r) {
				done = true
				break
			}
		}
		return err
	}
	var ferr error
	err := s.inTimeOrder(ctx, ids, f, oldest, func(id int64) bool {
		if chunk = append(chunk, id); len(chunk) < scanChunk {
			return true
		}
		ferr = flush()
		return ferr == nil && !done
	})
	if err == nil && ferr == nil && !done && len(chunk) > 0 {
		ferr = flush()
	}
	return errors.Join(err, ferr)
}

// cand is a candidate row and its message time (0 when unknown).
type cand struct{ id, ts int64 }

// sortNewest orders candidates by message time, newest first; ties and
// rows without a time go by row id, newest first.
func sortNewest(c []cand) {
	slices.SortFunc(c, func(a, b cand) int {
		if a.ts != b.ts {
			return cmp.Compare(b.ts, a.ts)
		}
		return cmp.Compare(b.id, a.id)
	})
}

// directLookup: up to this many candidates are ordered by looking up each
// one's time in messages_meta (about 1.5µs each, warm); more, by walking
// messages_ts newest first and keeping the candidates (about 0.4µs a row
// walked), which stops as soon as the caller has enough. A find of a
// pattern with 26k candidates in the real corpus walked 676k rows to
// reach its 50th hit (280ms) where lookups took 40ms; a very common
// pattern stops within a few thousand rows.
var directLookup = 50000

// newest calls yield with each row of ids that passes f, newest message
// first (ties: newer row id first), until yield returns false.
func (s *Store) newest(ctx context.Context, ids []int64, f *Filter, yield func(id int64) bool) error {
	return s.inTimeOrder(ctx, ids, f, false, yield)
}

// inTimeOrder is newest, or with oldest the reverse order.
func (s *Store) inTimeOrder(ctx context.Context, ids []int64, f *Filter, oldest bool, yield func(id int64) bool) error {
	if len(ids) == 0 {
		return nil
	}
	if len(ids) <= directLookup {
		where, args := f.hitWhere()
		q := `SELECT m.id, ifnull(m.ts, 0) FROM messages m INDEXED BY messages_meta ` + f.convJoin() + `
			WHERE m.id IN (SELECT value FROM json_each(?)) AND ` + where
		rows, err := s.rdb.QueryContext(ctx, q, append([]any{int64JSON(ids)}, args...)...)
		if err != nil {
			return err
		}
		var cands []cand
		for rows.Next() {
			var c cand
			if err := rows.Scan(&c.id, &c.ts); err != nil {
				rows.Close()
				return err
			}
			cands = append(cands, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		sortNewest(cands)
		if oldest {
			slices.Reverse(cands)
		}
		for _, c := range cands {
			if !yield(c.id) {
				return nil
			}
		}
		return nil
	}
	// The walk reads only messages_ts: conversation filters become a set
	// of conversation ids checked here, not a join per row walked.
	mf, cf := f.split()
	var convs idSet
	if cf != nil {
		cw, cargs := cf.where()
		cids, err := s.ids(ctx, `SELECT c.id FROM conversations c WHERE `+cw, cargs...)
		if err != nil {
			return err
		}
		convs = newIDSet(cids)
	}
	set := newIDSet(ids)
	where, args := mf.hitWhere()
	// Only the columns checked here: each column read costs on every row
	// walked (about 250k rows to the newest 20k matches of "error").
	cols, dest := `m.id`, 1
	if cf != nil {
		cols, dest = `m.id, m.conversation_id`, 2
	}
	order := ` ORDER BY m.ts DESC, m.id DESC`
	if oldest {
		order = ` ORDER BY m.ts, m.id`
	}
	q := `SELECT ` + cols + ` FROM messages m INDEXED BY messages_ts WHERE ` + where + order
	rows, err := s.rdb.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	var id, conv int64
	vals := []any{&id, &conv}[:dest]
	for rows.Next() {
		if err := rows.Scan(vals...); err != nil {
			return err
		}
		if set.has(id) && (cf == nil || convs.has(conv)) && !yield(id) {
			return nil
		}
	}
	return rows.Err()
}

// idSet is a bitmap of row ids.
type idSet []uint64

func newIDSet(ids []int64) idSet {
	top := int64(0)
	for _, id := range ids {
		top = max(top, id)
	}
	b := make(idSet, top/64+1)
	for _, id := range ids {
		if id >= 0 {
			b[id/64] |= 1 << (id % 64)
		}
	}
	return b
}

func (b idSet) has(id int64) bool {
	return id >= 0 && id/64 < int64(len(b)) && b[id/64]&(1<<(id%64)) != 0
}

// triIDs returns the row ids matching a fts_tri expression, before
// filters. The trigram parts are read in parallel.
func (s *Store) triIDs(ctx context.Context, expr string) ([]int64, error) {
	parts := make([][]int64, len(s.tri))
	errs := make([]error, len(s.tri))
	var wg sync.WaitGroup
	for i, sh := range s.tri {
		wg.Add(1)
		go func() {
			defer wg.Done()
			parts[i], errs[i] = s.ftsIDs(ctx, sh.schema+".fts_tri", "fts_tri", expr)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	var out []int64
	for _, p := range parts {
		out = append(out, p...)
	}
	return out, nil
}

// ftsIDs returns the row ids of FTS table (as named in FROM; col is its
// own name) matching expr. It reads the index only.
func (s *Store) ftsIDs(ctx context.Context, table, col, expr string) ([]int64, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT t.rowid FROM `+table+` t WHERE t.`+col+` MATCH ?`, expr)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) stream(ctx context.Context, q string, args []any, visit func(*Row) bool) error {
	rows, err := s.rdb.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return err
		}
		if !visit(r) {
			return nil
		}
	}
	return rows.Err()
}

func lineMatches(text string, locs [][]int, maxLines int) []LineMatch {
	var out []LineMatch
	lastLine := -1
	for _, loc := range locs {
		ls := strings.LastIndexByte(text[:loc[0]], '\n') + 1
		le := strings.IndexByte(text[loc[0]:], '\n')
		if le < 0 {
			le = len(text)
		} else {
			le += loc[0]
		}
		lineNo := strings.Count(text[:ls], "\n") + 1
		if lineNo == lastLine {
			continue
		}
		lastLine = lineNo
		line, col := text[ls:le], loc[0]-ls
		if len(line) > 400 {
			from := max(0, col-150)
			for from > 0 && !utf8.RuneStart(line[from]) {
				from--
			}
			to := min(len(line), from+400)
			for to < len(line) && !utf8.RuneStart(line[to]) {
				to++
			}
			line, col = line[from:to], col-from
		}
		out = append(out, LineMatch{Line: lineNo, Col: col, Text: line})
		if len(out) >= maxLines {
			break
		}
	}
	return out
}

// TrigramOp is the operator of a TrigramQuery node.
type TrigramOp int

const (
	TrigramAll  TrigramOp = iota // matches everything (no index constraint)
	TrigramNone                  // matches nothing
	TrigramAnd                   // all Trigrams and all Sub
	TrigramOr                    // any Trigram or any Sub
)

// TrigramQuery is a boolean query over trigrams, the shape
// google/codesearch's RegexpQuery planner produces (ported in A3).
type TrigramQuery struct {
	Op       TrigramOp
	Trigrams []string
	Sub      []*TrigramQuery

	expr string // a ready fts_tri expression (substring find)
}

// matchExpr renders q as an fts_tri MATCH expression. ok is false for a
// query that matches everything (no constraint); expr is "" and ok true
// for one that matches nothing.
func (q *TrigramQuery) matchExpr() (expr string, all bool) {
	if q.expr != "" {
		return q.expr, false
	}
	switch q.Op {
	case TrigramAll:
		return "", true
	case TrigramNone:
		return "", false
	}
	var parts []string
	for _, t := range q.Trigrams {
		parts = append(parts, ftsString(t))
	}
	for _, sub := range q.Sub {
		e, subAll := sub.matchExpr()
		switch {
		case subAll && q.Op == TrigramOr:
			return "", true
		case subAll: // AND with everything: no constraint from this branch
		case e == "" && q.Op == TrigramAnd:
			return "", false
		case e == "": // OR with nothing
		default:
			parts = append(parts, "("+e+")")
		}
	}
	if len(parts) == 0 {
		return "", q.Op == TrigramAnd
	}
	sep := " AND "
	if q.Op == TrigramOr {
		sep = " OR "
	}
	return strings.Join(parts, sep), false
}

// CandidateIDs returns the ids of rows matching the trigram query q that
// pass the filters, newest message first, at most limit (0: all).
func (s *Store) CandidateIDs(ctx context.Context, q *TrigramQuery, f Filter, limit int) ([]int64, error) {
	expr, all := q.matchExpr()
	if all {
		return nil, errors.New("localindex: trigram query has no constraint; use Scan with a scan bound")
	}
	if expr == "" {
		return nil, nil
	}
	all_, err := s.triIDs(ctx, expr)
	if err != nil {
		return nil, err
	}
	var ids []int64
	err = s.newest(ctx, all_, &f, func(id int64) bool {
		ids = append(ids, id)
		return limit <= 0 || len(ids) < limit
	})
	return ids, err
}

// Messages loads rows by id, in the order given; missing ids are skipped.
func (s *Store) Messages(ctx context.Context, ids []int64) ([]*Row, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT `+rowCols+rowFrom+` WHERE m.id IN (SELECT value FROM json_each(?))`, int64JSON(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := map[int64]*Row{}
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		byID[r.ID] = r
	}
	out := make([]*Row, 0, len(ids))
	for _, id := range ids {
		if r := byID[id]; r != nil {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

func int64JSON(ids []int64) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(itoa(id))
	}
	b.WriteByte(']')
	return b.String()
}

// Context returns up to before rows preceding and after rows following the
// message in its conversation, by ordinal, plus the message itself. The
// visibility flags of f (superseded, branches) apply to the neighbours; the
// target is always returned.
func (s *Store) Context(ctx context.Context, messageID int64, before, after int, f Filter) ([]*Row, error) {
	target, err := s.Messages(ctx, []int64{messageID})
	if err != nil {
		return nil, err
	}
	if len(target) == 0 {
		return nil, &NotFoundError{What: "message", ID: messageID}
	}
	t := target[0]
	vis := Filter{IncludeSuperseded: f.IncludeSuperseded, IncludeBranches: f.IncludeBranches}
	where, args := vis.where()
	load := func(cmp, order string, n int) ([]*Row, error) {
		if n <= 0 {
			return nil, nil
		}
		q := `SELECT ` + rowCols + rowFrom + ` WHERE m.conversation_id = ? AND (m.ordinal, m.id) ` + cmp + ` (?, ?) AND ` + where +
			` ORDER BY m.ordinal ` + order + `, m.id ` + order + ` LIMIT ?`
		var out []*Row
		err := s.stream(ctx, q, append(append([]any{t.ConversationID, t.Ordinal, t.ID}, args...), n), func(r *Row) bool {
			out = append(out, r)
			return true
		})
		return out, err
	}
	prev, err := load("<", "DESC", before)
	if err != nil {
		return nil, err
	}
	next, err := load(">", "ASC", after)
	if err != nil {
		return nil, err
	}
	out := make([]*Row, 0, len(prev)+1+len(next))
	for i := len(prev) - 1; i >= 0; i-- {
		out = append(out, prev[i])
	}
	out = append(out, t)
	return append(out, next...), nil
}

// ConversationRow is one conversation in a listing.
type ConversationRow struct {
	ID                   int64
	Agent                transcript.Agent
	SessionID            string
	DeviceID             string
	Cwd                  string
	RepoRoot             string
	Title                string
	StartedAt            time.Time
	LastActivityAt       time.Time
	ParentConversationID int64
	ParentSessionID      string
	SpawnedByMessageID   int64
	Depth                int
	Deleted              bool
	SourcePath           string
	Messages             int      // rows visible under the default filter
	Branches             []string // git branches, first seen first
	Digest               []byte   // the stored digest (internal/digest JSON)
}

// ListOptions tune ListConversations. Filter fields that apply to
// conversations (agents, repos, devices, time range on last activity,
// subagents, conversation scope) are honoured.
type ListOptions struct {
	Filter
	// Like, when set, keeps conversations whose session id, title, repo
	// root or cwd (or their last path element) matches this LIKE pattern
	// (escape \), case-insensitively for ASCII.
	Like           string
	IncludeDeleted bool
	Limit          int // default 50
	Offset         int
	// Oldest lists the least recent activity first.
	Oldest bool
}

func (o *ListOptions) where() (string, []any) {
	f := Filter{Agents: o.Agents, Repos: o.Repos, RepoLikes: o.RepoLikes, Devices: o.Devices, Branches: o.Branches, IdleBefore: o.IdleBefore, ExcludeSubagents: o.ExcludeSubagents,
		ExcludeSessions: o.ExcludeSessions, ExcludeConversations: o.ExcludeConversations, IncludeSuperseded: true, IncludeBranches: true}
	where, args := f.where()
	where = strings.ReplaceAll(where, "m.conversation_id", "c.id")
	if len(o.Conversations) > 0 {
		where += " AND c.id IN (SELECT value FROM json_each(?))"
		args = append(args, int64JSON(o.Conversations))
	}
	if !o.Since.IsZero() {
		where += " AND c.last_activity_at >= ?"
		args = append(args, o.Since.UnixMilli())
	}
	if !o.Until.IsZero() {
		where += " AND c.last_activity_at < ?"
		args = append(args, o.Until.UnixMilli())
	}
	if !o.IncludeDeleted {
		where += " AND c.deleted_in_generation IS NULL"
	}
	if o.Like != "" {
		where += ` AND (c.session_id LIKE ?1x ESCAPE '\' OR ifnull(c.title, '') LIKE ?1x ESCAPE '\'
			OR ifnull(c.repo_root, ifnull(c.cwd, '')) LIKE ?1x ESCAPE '\' OR ifnull(c.repo_root, ifnull(c.cwd, '')) LIKE '%/' || ?1x ESCAPE '\')`
		where = strings.ReplaceAll(where, "?1x", "?")
		args = append(args, o.Like, o.Like, o.Like, o.Like)
	}
	return where, args
}

// CountConversations counts the conversations ListConversations would
// list without its limit and offset.
func (s *Store) CountConversations(ctx context.Context, o ListOptions) (int, error) {
	where, args := o.where()
	var n int
	err := s.rdb.QueryRowContext(ctx, `SELECT count(*) FROM conversations c WHERE `+where, args...).Scan(&n)
	return n, err
}

// ListConversations returns conversations by most recent activity.
func (s *Store) ListConversations(ctx context.Context, o ListOptions) ([]ConversationRow, error) {
	if o.Limit <= 0 {
		o.Limit = 50
	}
	where, args := o.where()
	order := `c.last_activity_at DESC NULLS LAST, c.id DESC`
	if o.Oldest {
		order = `c.last_activity_at NULLS LAST, c.id`
	}
	q := `SELECT c.id, c.agent, c.session_id, c.device_id, ifnull(c.cwd, ''), ifnull(c.repo_root, ''), ifnull(c.title, ''),
		c.started_at, c.last_activity_at, ifnull(c.parent_conversation_id, 0), ifnull(c.parent_session_id, ''),
		ifnull(c.spawned_by_message_id, 0), c.depth, c.deleted_in_generation IS NOT NULL, ifnull(s.path, ''),
		(SELECT count(*) FROM messages m WHERE m.conversation_id = c.id AND m.superseded = 0 AND m.on_active_path IS NOT 0),
		ifnull(c.branches, ''), ifnull(c.digest, '')
		FROM conversations c LEFT JOIN sources s ON s.id = c.source_id
		WHERE ` + where + ` ORDER BY ` + order + ` LIMIT ? OFFSET ?`
	rows, err := s.rdb.QueryContext(ctx, q, append(args, o.Limit, o.Offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConversationRow
	for rows.Next() {
		var (
			c            ConversationRow
			agent        string
			start, la    sql.NullInt64
			branches, dg string
		)
		if err := rows.Scan(&c.ID, &agent, &c.SessionID, &c.DeviceID, &c.Cwd, &c.RepoRoot, &c.Title, &start, &la,
			&c.ParentConversationID, &c.ParentSessionID, &c.SpawnedByMessageID, &c.Depth, &c.Deleted, &c.SourcePath, &c.Messages,
			&branches, &dg); err != nil {
			return nil, err
		}
		if branches != "" {
			_ = json.Unmarshal([]byte(branches), &c.Branches)
		}
		if dg != "" {
			c.Digest = []byte(dg)
		}
		c.Agent = transcript.Agent(agent)
		if start.Valid {
			c.StartedAt = time.UnixMilli(start.Int64)
		}
		if la.Valid {
			c.LastActivityAt = time.UnixMilli(la.Int64)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
