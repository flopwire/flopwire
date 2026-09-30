// Package grep is the shared core of the grep tool: it turns grep's flags
// into one RE2 pattern with smart case, and collects the matches of rows
// fed to it newest first into a page (collapsing identical texts,
// counting hits and sessions, applying -m and the offset). The local index
// and the server feed it their candidate rows, so both answer with the
// same semantics.
package grep

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/regexq"
)

// Spec is grep's pattern flags.
type Spec struct {
	Patterns      []string // the positional pattern and every -e; a match of any
	Fixed         bool     // -F: literal strings
	IgnoreCase    bool     // -i
	CaseSensitive bool     // -s
	Word          bool     // -w: whole words
}

// Query fills q's pattern fields from the spec: one pattern (literal when
// the spec is a single fixed string without -w, else an RE2 alternation)
// and its case sensitivity. Case is smart by default: sensitive when the
// pattern holds an uppercase letter.
func (s Spec) Query(q *format.GrepQuery) error {
	if len(s.Patterns) == 0 {
		return fmt.Errorf("%w: a pattern is required", format.ErrBadRequest)
	}
	for _, p := range s.Patterns {
		if p == "" {
			return fmt.Errorf("%w: empty pattern", format.ErrBadRequest)
		}
	}
	q.CaseSensitive = s.CaseSensitive || !s.IgnoreCase && hasUpper(s.Patterns, s.Fixed)
	if s.Fixed && !s.Word && len(s.Patterns) == 1 {
		q.Pattern, q.Fixed = s.Patterns[0], true
		return nil
	}
	parts := make([]string, len(s.Patterns))
	for i, p := range s.Patterns {
		if s.Fixed {
			p = regexp.QuoteMeta(p)
		}
		parts[i] = p
	}
	pat := parts[0]
	if len(parts) > 1 {
		pat = "(?:" + strings.Join(parts, ")|(?:") + ")"
	}
	if s.Word {
		pat = `\b(?:` + pat + `)\b`
	}
	q.Pattern, q.Fixed = pat, false
	return nil
}

// hasUpper reports an uppercase letter in the patterns, skipping regex
// escapes (\S, \W, \p{Lu}) in non-fixed ones.
func hasUpper(patterns []string, fixed bool) bool {
	for _, p := range patterns {
		rs := []rune(p)
		for i := 0; i < len(rs); i++ {
			if !fixed && rs[i] == '\\' {
				i++
				if i+1 < len(rs) && (rs[i] == 'p' || rs[i] == 'P') && rs[i+1] == '{' {
					for i < len(rs) && rs[i] != '}' {
						i++
					}
				}
				continue
			}
			if unicode.IsUpper(rs[i]) {
				return true
			}
		}
	}
	return false
}

// Compile compiles q's pattern with the index's semantics: multi-line, so
// ^ and $ match at line breaks; case-insensitive unless q says otherwise;
// with q.Multiline (-U), . matches a newline too, so a match may span the
// lines of one message.
func Compile(q format.GrepQuery) (*regexq.Plan, error) {
	src := q.Pattern
	if q.Fixed {
		src = regexp.QuoteMeta(src)
	}
	if q.Multiline {
		src = "(?s)" + src
	}
	p, err := regexq.Compile(src, q.CaseSensitive)
	if err != nil {
		return nil, fmt.Errorf("%w: regex: %v", format.ErrBadRequest, err)
	}
	return p, nil
}

// Limits.
const (
	DefaultLimit = 20
	MaxLimit     = 500
	// MaxLines is the matching lines (with -o, the matches) kept per hit;
	// MoreLines counts the rest.
	MaxLines = 10
	// MaxMultiLines is the matched lines a -U hit keeps: a match spanning
	// more is cut, and MoreLines counts the lines left out.
	MaxMultiLines = 50
	// LineWidth is the bytes of a line kept around its first match.
	LineWidth = 300
	// countExtra and countTime bound the counting that goes on after a
	// content page is full, for the footer's totals: at most countExtra
	// more hits, for at most countTime. Past either the totals are lower
	// bounds.
	countExtra = 200
	countTime  = 250 * time.Millisecond
	// maxCount bounds the hits counted for -l and -c.
	maxCount = 100000
)

// Row is a candidate for the collector: a message's session, text hash
// and text, and the caller's reference to it.
type Row struct {
	Session string
	SHA     [32]byte
	Text    string
	Ref     any
}

// Found is a hit on the page.
type Found struct {
	Ref       any
	Lines     []format.Line
	MoreLines int
	Copies    int
}

// SessionHits is a session with matches (-l, -c), with the reference of
// its newest hit.
type SessionHits struct {
	Session string
	Ref     any
	Hits    int
}

// Collector gathers one page of grep results from rows fed newest first.
type Collector struct {
	re  *regexp.Regexp
	q   format.GrepQuery
	now func() time.Time

	page     []Found
	seen     map[[32]byte]int // text hash -> page index, or -1
	seenIn   map[string]bool  // -l, -c: session and text hash
	total    int
	sessions map[string]int // session -> index in order
	order    []SessionHits
	fullAt   time.Time
	capped   bool
	seq      int
	ranked   []ranked // relevance: the best hits so far, best first
}

// ranked is a hit held for a relevance page: its score (matches in the
// message), the order it came in, and its page entry.
type ranked struct {
	score, seq int
	f          Found
}

// NewCollector returns a collector for q, whose pattern re verifies.
func NewCollector(re *regexp.Regexp, q format.GrepQuery) *Collector {
	if q.Limit <= 0 {
		q.Limit = DefaultLimit
	}
	q.Limit = min(q.Limit, MaxLimit)
	return &Collector{re: re, q: q, now: time.Now, seen: map[[32]byte]int{}, seenIn: map[string]bool{}, sessions: map[string]int{}}
}

// SetClock replaces the collector's clock (tests).
func (c *Collector) SetClock(now func() time.Time) { c.now = now }

// Add verifies r and records it; it returns false when the collector has
// what it needs and the caller should stop feeding rows.
func (c *Collector) Add(r Row) bool {
	content := c.q.Mode == "" || c.q.Mode == format.ModeContent
	relevance := c.relevance()
	if content && !relevance && !c.fullAt.IsZero() && c.now().Sub(c.fullAt) > countTime {
		c.capped = true
		return false
	}
	locs := c.re.FindAllStringIndex(r.Text, 1000)
	if len(locs) == 0 {
		return true
	}
	// Content collapses identical texts into one hit; -l and -c count
	// them once per session, so every session with a match is listed.
	if !content {
		k := r.Session + "\x00" + string(r.SHA[:])
		if c.seenIn[k] {
			return true
		}
		c.seenIn[k] = true
	} else if i, dup := c.seen[r.SHA]; dup {
		switch {
		case relevance:
			for j := range c.ranked {
				if c.ranked[j].seq == i {
					c.ranked[j].f.Copies++
				}
			}
		case i >= 0:
			c.page[i].Copies++
		}
		return true
	}
	si, known := c.sessions[r.Session]
	if known && c.q.MaxPerSession > 0 && c.order[si].Hits >= c.q.MaxPerSession {
		return true
	}
	if !known {
		si = len(c.order)
		c.sessions[r.Session] = si
		c.order = append(c.order, SessionHits{Session: r.Session, Ref: r.Ref})
	}
	c.order[si].Hits++
	c.total++
	idx := -1
	switch {
	case content && relevance:
		c.seq++
		idx = c.seq
		c.rank(ranked{score: len(locs), seq: c.seq}, r, locs)
	case content && c.total > c.q.Offset && len(c.page) < c.q.Limit:
		f := Found{Ref: r.Ref}
		f.Lines, f.MoreLines = MatchLines(r.Text, locs, c.lineOpts())
		c.page = append(c.page, f)
		idx = len(c.page) - 1
		if len(c.page) == c.q.Limit {
			c.fullAt = c.now()
		}
	}
	if content {
		c.seen[r.SHA] = idx
	}
	switch {
	case content && relevance:
		if c.total >= maxCount {
			c.capped = true
			return false
		}
	case content && len(c.page) == c.q.Limit && c.total >= c.q.Offset+c.q.Limit+countExtra:
		c.capped = true
		return false
	case !content && c.total >= maxCount:
		c.capped = true
		return false
	}
	return true
}

// relevance reports a content page ranked by matches per message.
func (c *Collector) relevance() bool { return c.q.Sort == format.SortRelevance }

func (c *Collector) lineOpts() LineOpts {
	return LineOpts{Before: c.q.Before, After: c.q.After, Only: c.q.OnlyMatching, Multi: c.q.Multiline}
}

// rank keeps h among the best offset+limit hits (most matches first; ties
// in the order they came, so newest first by default). Lines are
// rendered only for hits that make the cut.
func (c *Collector) rank(h ranked, r Row, locs [][]int) {
	keep := c.q.Offset + c.q.Limit
	at := sort.Search(len(c.ranked), func(i int) bool { return c.ranked[i].score < h.score })
	if at >= keep {
		return
	}
	h.f = Found{Ref: r.Ref}
	h.f.Lines, h.f.MoreLines = MatchLines(r.Text, locs, c.lineOpts())
	c.ranked = slices.Insert(c.ranked, at, h)
	if len(c.ranked) > keep {
		c.ranked = c.ranked[:keep]
	}
}

// Result is what the collector gathered.
type Result struct {
	Hits          []Found
	Sessions      []SessionHits // -l, -c: this page of sessions
	Total         int           // matching messages counted
	TotalSessions int
	Capped        bool // counting stopped early: the totals are lower bounds
	Next          int  // offset of the next page; 0 when none
}

// Result returns the page.
func (c *Collector) Result() Result {
	r := Result{Hits: c.page, Total: c.total, TotalSessions: len(c.order), Capped: c.capped}
	if c.relevance() {
		r.Hits = nil
		for i := c.q.Offset; i < len(c.ranked); i++ {
			r.Hits = append(r.Hits, c.ranked[i].f)
		}
		if end := c.q.Offset + len(r.Hits); len(r.Hits) > 0 && (c.total > end || c.capped) {
			r.Next = end
		}
		if c.q.Mode == format.ModeContent || c.q.Mode == "" {
			return r
		}
	}
	if c.q.Mode == format.ModeSessions || c.q.Mode == format.ModeCount {
		if c.relevance() {
			// Sessions with the most matching messages first.
			order := slices.Clone(c.order)
			slices.SortStableFunc(order, func(a, b SessionHits) int { return b.Hits - a.Hits })
			c.order = order
		}
		lo := min(c.q.Offset, len(c.order))
		hi := min(lo+c.q.Limit, len(c.order))
		r.Sessions, r.Hits = c.order[lo:hi], nil
		if hi < len(c.order) || c.capped {
			r.Next = hi
		}
		return r
	}
	if end := c.q.Offset + len(c.page); len(c.page) > 0 && (c.total > end || c.capped) {
		r.Next = end
	}
	return r
}

// Lines returns the matching lines of text (the match locations locs, in
// order), at most MaxLines of them, each with before and after lines of
// context, and the number of matching lines left out.
func Lines(text string, locs [][]int, before, after int) ([]format.Line, int) {
	return MatchLines(text, locs, LineOpts{Before: before, After: after})
}

// LineOpts shape a hit's lines: context lines before and after, only the
// matched text (-o), and matches spanning lines (-U).
type LineOpts struct {
	Before, After int
	Only, Multi   bool
}

// MatchLines returns a hit's lines for the match locations locs (in
// order) and the number of matching lines (with Only, matches) left out.
// A line holds one match's line, or with Multi every line a match spans;
// with Only, a line is a match's text (newlines shown as ⏎) and has no
// context.
func MatchLines(text string, locs [][]int, o LineOpts) ([]format.Line, int) {
	starts := []int{0}
	for i := 0; ; {
		nl := strings.IndexByte(text[i:], '\n')
		if nl < 0 {
			break
		}
		i += nl + 1
		starts = append(starts, i)
	}
	lineOf := func(pos int) int { return sort.SearchInts(starts, pos+1) } // 1-based
	lineText := func(n int) string {
		end := len(text)
		if n < len(starts) {
			end = starts[n] - 1
		}
		return strings.TrimSuffix(text[starts[n-1]:end], "\r")
	}
	more := 0
	if o.Only {
		var out []format.Line
		for _, loc := range locs {
			if loc[1] <= loc[0] {
				continue
			}
			if len(out) >= MaxLines {
				more++
				continue
			}
			m := strings.ReplaceAll(strings.ReplaceAll(text[loc[0]:loc[1]], "\r\n", "\n"), "\n", "⏎")
			out = append(out, format.Line{N: lineOf(loc[0]), Text: format.ClipAround(m, 0, LineWidth), Match: true})
		}
		return out, more
	}
	// Each match covers lines from..to; the first line shows around col.
	type span struct{ from, to, col int }
	var spans []span
	budget := MaxLines
	if o.Multi {
		budget = MaxMultiLines
	}
	used, last := 0, 0
	for _, loc := range locs {
		from := lineOf(loc[0])
		to := from
		if o.Multi {
			to = lineOf(max(loc[1]-1, loc[0]))
		}
		if to <= last {
			continue
		}
		col := loc[0] - starts[from-1]
		if from <= last {
			from, col = last+1, -1
		}
		n := to - from + 1
		if used+n > budget {
			keep := budget - used
			more += n - max(keep, 0)
			if keep <= 0 {
				last = to
				continue
			}
			to = from + keep - 1
			n = keep
		}
		used += n
		last = to
		if k := len(spans); k > 0 && spans[k-1].to+1 == from && o.Multi {
			spans[k-1].to = to
			continue
		}
		spans = append(spans, span{from, to, col})
	}
	if len(spans) == 0 {
		return nil, more
	}
	colOf := map[int]int{}
	isMatch := map[int]bool{}
	for _, sp := range spans {
		for n := sp.from; n <= sp.to; n++ {
			isMatch[n] = true
		}
		if sp.col >= 0 {
			colOf[sp.from] = sp.col
		}
	}
	var out []format.Line
	prev := 0
	for _, sp := range spans {
		from, to := max(1, sp.from-o.Before, prev+1), min(len(starts), sp.to+o.After)
		for n := from; n <= to; n++ {
			out = append(out, format.Line{N: n, Text: format.ClipAround(lineText(n), colOf[n], LineWidth), Match: isMatch[n]})
			prev = n
		}
	}
	return out, more
}
