// Package regexq runs regular-expression find over the local index (spec
// §8): a port of google/codesearch's RegexpQuery (plan.go) turns the
// pattern into a boolean trigram query, the query selects candidate rows
// from the FTS5 trigram table, and Go's regexp verifies each candidate's
// stored text. A pattern with no required trigram (".*", "a.b") cannot use
// the index; it falls back to a scan of the newest rows the filters allow,
// bounded by MaxScan, and the result says so.
package regexq

import (
	"context"
	"fmt"
	"regexp"
	"regexp/syntax"
	"strings"
	"unicode/utf8"

	"github.com/flopwire/flopwire/internal/localindex"
)

// Plan is a compiled find pattern.
type Plan struct {
	Pattern string
	Re      *regexp.Regexp
	Query   *Query
}

// Compile compiles pattern with Go's RE2 syntax in multi-line mode (^ and $
// match at line breaks, as in grep), case-insensitive unless caseSensitive,
// and plans its trigram query.
func Compile(pattern string, caseSensitive bool) (*Plan, error) {
	flags := "(?m)"
	if !caseSensitive {
		flags = "(?mi)"
	}
	src := flags + pattern
	re, err := regexp.Compile(src)
	if err != nil {
		return nil, err
	}
	syn, err := syntax.Parse(src, syntax.Perl)
	if err != nil {
		return nil, err
	}
	return &Plan{Pattern: pattern, Re: re, Query: RegexpQuery(syn)}, nil
}

// Indexed reports whether the plan constrains the trigram index.
func (p *Plan) Indexed() bool { return p.Query.Op != QAll }

// TrigramQuery converts q to the index's query type.
func (q *Query) TrigramQuery() *localindex.TrigramQuery {
	out := &localindex.TrigramQuery{Trigrams: append([]string(nil), q.Trigram...)}
	switch q.Op {
	case QAll:
		out.Op = localindex.TrigramAll
	case QNone:
		out.Op = localindex.TrigramNone
	case QAnd:
		out.Op = localindex.TrigramAnd
	case QOr:
		out.Op = localindex.TrigramOr
	}
	for _, s := range q.Sub {
		out.Sub = append(out.Sub, s.TrigramQuery())
	}
	return out
}

// MatchExpr is the fts_tri MATCH expression for the plan: all is true when
// the plan has no constraint, and expr is "" with all false when it matches
// nothing.
func (p *Plan) MatchExpr() (expr string, all bool) { return p.Query.TrigramQuery().MatchExpr() }

// Options tune Find.
type Options struct {
	localindex.Filter
	Limit int // matching messages returned; default 20
	// MaxScan bounds the rows read by the unindexed fallback; default 20000.
	MaxScan int
	// MaxCandidates bounds the candidate rows verified for an indexed
	// plan; default 200000.
	MaxCandidates int
	// Budget bounds the time and the text verified (decision D8).
	localindex.Budget
}

// Match is a message whose text matches, with its first matching line.
type Match struct {
	localindex.Row
	Line      int    // 1-based line of the first match within the text
	LineText  string // that line, clipped to 400 bytes around the match
	LineCount int    // matching lines in the text
	Copies    int    // older matching messages with identical text folded into this one
}

// Stats describe how Find ran.
type Stats struct {
	Unindexed  bool   // no trigram constraint: a bounded scan ran instead
	Truncated  bool   // a bound or the budget stopped the search early
	Reason     string // why, when Truncated (localindex.Reason*)
	Candidates int    // rows read and verified
	Total      int    // candidate rows in all; -1 when unknown
	Expr       string // the fts_tri expression, when indexed
}

// Note is a one-line explanation for callers when the result may be
// incomplete; "" otherwise.
func (s Stats) Note(o Options) string {
	o = o.withDefaults()
	switch {
	case s.Reason == localindex.ReasonTimeout || s.Reason == localindex.ReasonVerifyBudget:
		what := "timed out"
		if s.Reason == localindex.ReasonVerifyBudget {
			what = "stopped at the verification budget"
		}
		total := "?"
		if s.Total >= 0 {
			total = fmt.Sprint(s.Total)
		}
		return fmt.Sprintf("%s: checked %d of %s candidates, newest first; narrow with agent, repo, kind or since", what, s.Candidates, total)
	case s.Unindexed && s.Truncated:
		return fmt.Sprintf("unindexed: the pattern has no required trigram, so only the newest %d rows matching the filters were scanned; narrow with agent, repo, kind or since", o.MaxScan)
	case s.Unindexed:
		return fmt.Sprintf("unindexed: the pattern has no required trigram, so rows matching the filters were scanned newest first (at most %d)", o.MaxScan)
	case s.Truncated:
		return fmt.Sprintf("stopped after verifying %d candidate rows; narrow the pattern or the filters", o.MaxCandidates)
	}
	return ""
}

func (o Options) withDefaults() Options {
	if o.Limit <= 0 {
		o.Limit = 20
	}
	if o.MaxScan <= 0 {
		o.MaxScan = 20000
	}
	if o.MaxCandidates <= 0 {
		o.MaxCandidates = 200000
	}
	return o
}

// Find returns up to Limit messages whose text matches the plan, newest
// rows first.
func Find(ctx context.Context, s *localindex.Store, p *Plan, o Options) ([]Match, Stats, error) {
	o = o.withDefaults()
	var st Stats
	st.Unindexed = !p.Indexed()
	if !st.Unindexed {
		st.Expr, _ = p.MatchExpr()
	}
	var out []Match
	var seen localindex.Collapser
	n := 0
	visit := func(r *localindex.Row) bool {
		n++
		if m, ok := match(p.Re, r); ok {
			if i, dup := seen.Add(r); dup {
				out[i].Copies++
			} else if out = append(out, m); len(out) >= o.Limit {
				return false
			}
		}
		if !st.Unindexed && n >= o.MaxCandidates {
			st.Truncated, st.Reason = true, localindex.ReasonCandidateLimit
			return false
		}
		return true
	}
	part, err := s.Scan(ctx, p.Query.TrigramQuery(), localindex.ScanOptions{Filter: o.Filter, MaxScan: o.MaxScan, Budget: o.Budget}, visit)
	st.Candidates, st.Total = part.Checked, part.Total
	if part.Truncated {
		st.Truncated, st.Reason = true, part.Reason
	}
	return out, st, err
}

// maxLineCount bounds the matches counted per message.
const maxLineCount = 1000

func match(re *regexp.Regexp, r *localindex.Row) (Match, bool) {
	locs := re.FindAllStringIndex(r.Text, maxLineCount)
	if len(locs) == 0 {
		return Match{}, false
	}
	m := Match{Row: *r}
	m.Line, m.LineText = lineAt(r.Text, locs[0][0])
	last := -1
	for _, loc := range locs {
		n := strings.Count(r.Text[:loc[0]], "\n")
		if n != last {
			m.LineCount++
			last = n
		}
	}
	return m, true
}

// lineAt returns the 1-based line number holding byte off and the line,
// clipped to 400 bytes around off.
func lineAt(text string, off int) (int, string) {
	ls := strings.LastIndexByte(text[:off], '\n') + 1
	le := strings.IndexByte(text[off:], '\n')
	if le < 0 {
		le = len(text)
	} else {
		le += off
	}
	line, col := text[ls:le], off-ls
	if len(line) > 400 {
		from := max(0, col-150)
		for from > 0 && !utf8.RuneStart(line[from]) {
			from--
		}
		to := min(len(line), from+400)
		for to < len(line) && !utf8.RuneStart(line[to]) {
			to++
		}
		line = line[from:to]
	}
	return strings.Count(text[:ls], "\n") + 1, strings.TrimSuffix(line, "\r")
}
