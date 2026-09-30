// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in third_party/codesearch/LICENSE.
//
// Ported from google/codesearch index/regexp.go
// (https://github.com/google/codesearch, git 74a12a911a79b901d1158c48d011b2da1b090fc9).
// Changes from upstream, all for Flopwire's SQLite FTS5 trigram table:
//
//   - Trigrams are three Unicode characters, not three bytes: FTS5's trigram
//     tokenizer works on code points. Lengths and prefix/suffix cuts count
//     runes.
//   - The index folds case (trigram case_sensitive 0), so literals and
//     classes are mapped through foldRune before they enter a set: ASCII
//     upper case becomes lower case, every other rune stays as is (FTS5
//     folds the query string the same way it folded the text, so an
//     unfolded non-ASCII rune still meets its folded form). Upstream expands
//     (?i) into every case variant instead.
//   - Trigrams holding NUL or U+FFFD are dropped (never required); a string
//     with no usable trigram makes its alternative unconstrained.
//   - Set and query operations copy instead of reusing their arguments'
//     backing arrays, so a shared node or set is never rewritten in place.
//   - Unused helpers (maybeRewrite, maxLen, clear, contains) are gone.

package regexq

import (
	"regexp/syntax"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A Query is a boolean query over trigrams that every match of a regexp
// satisfies: it matches everything the regexp would match, and probably more.
// Candidates selected by it are verified with the regexp itself.
type Query struct {
	Op      QueryOp
	Trigram []string
	Sub     []*Query
}

// QueryOp is a Query node's operator.
type QueryOp int

const (
	QAll  QueryOp = iota // Everything matches
	QNone                // Nothing matches
	QAnd                 // All in Sub and Trigram must match
	QOr                  // At least one in Sub or Trigram must match
)

var allQuery = &Query{Op: QAll}
var noneQuery = &Query{Op: QNone}

// clone copies q's slices so andOr can rewrite them.
func (q *Query) clone() *Query {
	return &Query{Op: q.Op, Trigram: append([]string(nil), q.Trigram...), Sub: append([]*Query(nil), q.Sub...)}
}

func (q *Query) and(r *Query) *Query { return q.andOr(r, QAnd) }
func (q *Query) or(r *Query) *Query  { return q.andOr(r, QOr) }

// andOr returns the query q AND r or q OR r. It works hard to avoid creating
// unnecessarily complicated structures.
func (q *Query) andOr(r *Query, op QueryOp) *Query {
	if len(q.Trigram) == 0 && len(q.Sub) == 1 {
		q = q.Sub[0]
	}
	if len(r.Trigram) == 0 && len(r.Sub) == 1 {
		r = r.Sub[0]
	}

	// Boolean simplification.
	// If q ⇒ r, q AND r ≡ q.
	// If q ⇒ r, q OR r ≡ r.
	if q.implies(r) {
		if op == QAnd {
			return q
		}
		return r
	}
	if r.implies(q) {
		if op == QAnd {
			return r
		}
		return q
	}
	q, r = q.clone(), r.clone()

	// Both q and r are QAnd or QOr.
	// If they match or can be made to match, merge.
	qAtom := len(q.Trigram) == 1 && len(q.Sub) == 0
	rAtom := len(r.Trigram) == 1 && len(r.Sub) == 0
	if q.Op == op && (r.Op == op || rAtom) {
		q.Trigram = stringSet.union(q.Trigram, r.Trigram, false)
		q.Sub = append(q.Sub, r.Sub...)
		return q
	}
	if r.Op == op && qAtom {
		r.Trigram = stringSet.union(r.Trigram, q.Trigram, false)
		return r
	}
	if qAtom && rAtom {
		q.Op = op
		q.Trigram = stringSet.union(q.Trigram, r.Trigram, false)
		return q
	}

	// If one matches the op, add the other to it.
	if q.Op == op {
		q.Sub = append(q.Sub, r)
		return q
	}
	if r.Op == op {
		r.Sub = append(r.Sub, q)
		return r
	}

	// We are creating an AND of ORs or an OR of ANDs.
	// Factor out common trigrams, if any.
	common := stringSet{}
	i, j := 0, 0
	wi, wj := 0, 0
	for i < len(q.Trigram) && j < len(r.Trigram) {
		qt, rt := q.Trigram[i], r.Trigram[j]
		if qt < rt {
			q.Trigram[wi] = qt
			wi++
			i++
		} else if qt > rt {
			r.Trigram[wj] = rt
			wj++
			j++
		} else {
			common = append(common, qt)
			i++
			j++
		}
	}
	for ; i < len(q.Trigram); i++ {
		q.Trigram[wi] = q.Trigram[i]
		wi++
	}
	for ; j < len(r.Trigram); j++ {
		r.Trigram[wj] = r.Trigram[j]
		wj++
	}
	q.Trigram = q.Trigram[:wi]
	r.Trigram = r.Trigram[:wj]
	if len(common) > 0 {
		// If there were common trigrams, rewrite
		//
		//	(abc|def|ghi|jkl) AND (abc|def|mno|prs) =>
		//		(abc|def) OR ((ghi|jkl) AND (mno|prs))
		//
		//	(abc&def&ghi&jkl) OR (abc&def&mno&prs) =>
		//		(abc&def) AND ((ghi&jkl) OR (mno&prs))
		//
		// Call andOr recursively in case q and r can now be simplified
		// (we removed some trigrams).
		s := q.andOr(r, op)

		// Add in factored trigrams.
		otherOp := QAnd + QOr - op
		t := &Query{Op: otherOp, Trigram: common}
		return t.andOr(s, t.Op)
	}

	// Otherwise just create the op.
	return &Query{Op: op, Sub: []*Query{q, r}}
}

// implies reports whether q implies r. It is okay for it to return false
// negatives.
func (q *Query) implies(r *Query) bool {
	if q.Op == QNone || r.Op == QAll {
		// False implies everything.
		// Everything implies True.
		return true
	}
	if q.Op == QAll || r.Op == QNone {
		// True implies nothing.
		// Nothing implies False.
		return false
	}

	if q.Op == QAnd || (q.Op == QOr && len(q.Trigram) == 1 && len(q.Sub) == 0) {
		return trigramsImply(q.Trigram, r)
	}

	if q.Op == QOr && r.Op == QOr &&
		len(q.Trigram) > 0 && len(q.Sub) == 0 &&
		stringSet.isSubsetOf(q.Trigram, r.Trigram) {
		return true
	}
	return false
}

func trigramsImply(t []string, q *Query) bool {
	switch q.Op {
	case QOr:
		for _, qq := range q.Sub {
			if trigramsImply(t, qq) {
				return true
			}
		}
		for i := range t {
			if stringSet.isSubsetOf(t[i:i+1], q.Trigram) {
				return true
			}
		}
		return false
	case QAnd:
		for _, qq := range q.Sub {
			if !trigramsImply(t, qq) {
				return false
			}
		}
		return stringSet.isSubsetOf(q.Trigram, t)
	}
	return false
}

// andTrigrams returns q AND the OR of the AND of the trigrams present in
// each string.
func (q *Query) andTrigrams(t stringSet) *Query {
	if t.minLen() < 3 {
		// If there is a short string, we can't guarantee
		// that any trigrams must be present, so use ALL.
		// q AND ALL = q.
		return q
	}

	or := noneQuery
	for _, tt := range t {
		trig := trigrams(tt)
		if len(trig) == 0 {
			// Only unusable trigrams: this alternative needs nothing.
			return q
		}
		or = or.or(&Query{Op: QAnd, Trigram: trig})
	}
	return q.and(or)
}

// trigrams returns the sorted distinct trigrams of s, in runes, leaving out
// any holding a rune the index cannot be trusted to hold as written.
func trigrams(s string) stringSet {
	rs := []rune(s)
	var trig stringSet
	for i := 0; i+3 <= len(rs); i++ {
		if usable(rs[i]) && usable(rs[i+1]) && usable(rs[i+2]) {
			trig.add(string(rs[i : i+3]))
		}
	}
	trig.clean(false)
	return trig
}

func usable(r rune) bool { return r != 0 && r != utf8.RuneError }

// foldRune maps r to the form the case-insensitive trigram index is
// queried with. Only ASCII is folded here; FTS5 folds the query string
// itself, so any other rune may be passed through unchanged.
func foldRune(r rune) rune {
	if 'A' <= r && r <= 'Z' {
		return r + 'a' - 'A'
	}
	return r
}

func foldString(rs []rune) string {
	b := make([]rune, len(rs))
	for i, r := range rs {
		b[i] = foldRune(r)
	}
	return string(b)
}

func (q *Query) String() string {
	if q == nil {
		return "?"
	}
	if q.Op == QNone {
		return "-"
	}
	if q.Op == QAll {
		return "+"
	}

	if len(q.Sub) == 0 && len(q.Trigram) == 1 {
		return strconv.Quote(q.Trigram[0])
	}

	var (
		s     string
		sjoin string
		end   string
		tjoin string
	)
	if q.Op == QAnd {
		sjoin = " "
		tjoin = " "
	} else {
		s = "("
		sjoin = ")|("
		end = ")"
		tjoin = "|"
	}
	for i, t := range q.Trigram {
		if i > 0 {
			s += tjoin
		}
		s += strconv.Quote(t)
	}
	if len(q.Sub) > 0 {
		if len(q.Trigram) > 0 {
			s += sjoin
		}
		s += q.Sub[0].String()
		for i := 1; i < len(q.Sub); i++ {
			s += sjoin + q.Sub[i].String()
		}
	}
	s += end
	return s
}

// RegexpQuery returns a Query for the given regexp.
func RegexpQuery(re *syntax.Regexp) *Query {
	info := analyze(re)
	info.simplify(true)
	info.addExact()
	return info.match
}

// A regexpInfo summarizes the results of analyzing a regexp.
type regexpInfo struct {
	// canEmpty records whether the regexp matches the empty string
	canEmpty bool

	// exact is the exact set of strings matching the regexp.
	exact stringSet

	// if exact is nil, prefix is the set of possible match prefixes,
	// and suffix is the set of possible match suffixes.
	prefix stringSet // otherwise: the exact set of matching prefixes ...
	suffix stringSet // ... and suffixes

	// match records a query that must be satisfied by any
	// match for the regexp, in addition to the information
	// recorded above.
	match *Query
}

const (
	// Exact sets are limited to maxExact strings.
	// If they get too big, simplify will rewrite the regexpInfo
	// to use prefix and suffix instead.  It's not worthwhile for
	// this to be bigger than maxSet.
	maxExact = 7

	// Prefix and suffix sets are limited to maxSet strings.
	// If they get too big, simplify will replace groups of strings
	// sharing a common leading prefix (or trailing suffix) with
	// that common prefix (or suffix).
	maxSet = 20
)

// anyMatch returns the regexpInfo describing a regexp that
// matches any string.
func anyMatch() regexpInfo {
	return regexpInfo{
		canEmpty: true,
		prefix:   []string{""},
		suffix:   []string{""},
		match:    allQuery,
	}
}

// anyChar returns the regexpInfo describing a regexp that
// matches any single character.
func anyChar() regexpInfo {
	return regexpInfo{
		prefix: []string{""},
		suffix: []string{""},
		match:  allQuery,
	}
}

// noMatch returns the regexpInfo describing a regexp that
// matches no strings at all.
func noMatch() regexpInfo {
	return regexpInfo{
		match: noneQuery,
	}
}

// emptyString returns the regexpInfo describing a regexp that
// matches only the empty string.
func emptyString() regexpInfo {
	return regexpInfo{
		canEmpty: true,
		exact:    []string{""},
		match:    allQuery,
	}
}

// analyze returns the regexpInfo for the regexp re.
func analyze(re *syntax.Regexp) (ret regexpInfo) {
	var info regexpInfo
	switch re.Op {
	case syntax.OpNoMatch:
		return noMatch()

	case syntax.OpEmptyMatch,
		syntax.OpBeginLine, syntax.OpEndLine,
		syntax.OpBeginText, syntax.OpEndText,
		syntax.OpWordBoundary, syntax.OpNoWordBoundary:
		return emptyString()

	case syntax.OpLiteral:
		if re.Flags&syntax.FoldCase != 0 {
			switch len(re.Rune) {
			case 0:
				return emptyString()
			case 1:
				// Single-letter case-folded string:
				// rewrite into char class and analyze.
				re1 := &syntax.Regexp{
					Op: syntax.OpCharClass,
				}
				r0 := re.Rune[0]
				re1.Rune = append(re1.Rune, r0, r0)
				for r1 := unicode.SimpleFold(r0); r1 != r0; r1 = unicode.SimpleFold(r1) {
					re1.Rune = append(re1.Rune, r1, r1)
				}
				return analyze(re1)
			}
			// Multi-letter case-folded string:
			// treat as concatenation of single-letter case-folded strings.
			info = emptyString()
			for i := range re.Rune {
				re1 := &syntax.Regexp{
					Op:    syntax.OpLiteral,
					Flags: syntax.FoldCase,
					Rune:  re.Rune[i : i+1],
				}
				info = concat(info, analyze(re1))
			}
			return info
		}
		info.exact = stringSet{foldString(re.Rune)}
		info.match = allQuery

	case syntax.OpAnyCharNotNL, syntax.OpAnyChar:
		return anyChar()

	case syntax.OpCapture:
		return analyze(re.Sub[0])

	case syntax.OpConcat:
		return fold(concat, re.Sub, emptyString())

	case syntax.OpAlternate:
		return fold(alternate, re.Sub, noMatch())

	case syntax.OpQuest:
		return alternate(analyze(re.Sub[0]), emptyString())

	case syntax.OpStar:
		// We don't know anything, so assume the worst.
		return anyMatch()

	case syntax.OpRepeat:
		if re.Min == 0 {
			// Like OpStar
			return anyMatch()
		}
		fallthrough
	case syntax.OpPlus:
		// x+
		// Since there has to be at least one x, the prefixes and suffixes
		// stay the same.  If x was exact, it isn't anymore.
		info = analyze(re.Sub[0])
		if info.exact.have() {
			info.prefix = info.exact
			info.suffix = info.exact.copy()
			info.exact = nil
		}

	case syntax.OpCharClass:
		info.match = allQuery

		// Special case.
		if len(re.Rune) == 0 {
			return noMatch()
		}

		// Special case.
		if len(re.Rune) == 2 && re.Rune[0] == re.Rune[1] {
			info.exact = stringSet{string(foldRune(re.Rune[0]))}
			break
		}

		n := 0
		for i := 0; i < len(re.Rune); i += 2 {
			n += int(re.Rune[i+1]-re.Rune[i]) + 1
		}
		// If the class is too large, it's okay to overestimate.
		if n > 100 {
			return anyChar()
		}

		info.exact = []string{}
		for i := 0; i < len(re.Rune); i += 2 {
			lo, hi := re.Rune[i], re.Rune[i+1]
			for rr := lo; rr <= hi; rr++ {
				info.exact.add(string(foldRune(rr)))
			}
		}
	}

	info.simplify(false)
	return info
}

// fold is the usual higher-order function.
func fold(f func(x, y regexpInfo) regexpInfo, sub []*syntax.Regexp, zero regexpInfo) regexpInfo {
	if len(sub) == 0 {
		return zero
	}
	if len(sub) == 1 {
		return analyze(sub[0])
	}
	info := f(analyze(sub[0]), analyze(sub[1]))
	for i := 2; i < len(sub); i++ {
		info = f(info, analyze(sub[i]))
	}
	return info
}

// concat returns the regexp info for xy given x and y.
func concat(x, y regexpInfo) (out regexpInfo) {
	var xy regexpInfo
	xy.match = x.match.and(y.match)
	if x.exact.have() && y.exact.have() {
		xy.exact = x.exact.cross(y.exact, false)
	} else {
		if x.exact.have() {
			xy.prefix = x.exact.cross(y.prefix, false)
		} else {
			xy.prefix = x.prefix
			if x.canEmpty {
				xy.prefix = xy.prefix.union(y.prefix, false)
			}
		}
		if y.exact.have() {
			xy.suffix = x.suffix.cross(y.exact, true)
		} else {
			xy.suffix = y.suffix
			if y.canEmpty {
				xy.suffix = xy.suffix.union(x.suffix, true)
			}
		}
	}

	// If all the possible strings in the cross product of x.suffix
	// and y.prefix are long enough, then the trigram for one
	// of them must be present and would not necessarily be
	// accounted for in xy.prefix or xy.suffix yet.  Cut things off
	// at maxSet just to keep the sets manageable.
	if !x.exact.have() && !y.exact.have() &&
		x.suffix.size() <= maxSet && y.prefix.size() <= maxSet &&
		x.suffix.minLen()+y.prefix.minLen() >= 3 {
		xy.match = xy.match.andTrigrams(x.suffix.cross(y.prefix, false))
	}

	xy.simplify(false)
	return xy
}

// alternate returns the regexpInfo for x|y given x and y.
func alternate(x, y regexpInfo) (out regexpInfo) {
	var xy regexpInfo
	if x.exact.have() && y.exact.have() {
		xy.exact = x.exact.union(y.exact, false)
	} else if x.exact.have() {
		xy.prefix = x.exact.union(y.prefix, false)
		xy.suffix = x.exact.union(y.suffix, true)
		x.addExact()
	} else if y.exact.have() {
		xy.prefix = x.prefix.union(y.exact, false)
		xy.suffix = x.suffix.union(y.exact, true)
		y.addExact()
	} else {
		xy.prefix = x.prefix.union(y.prefix, false)
		xy.suffix = x.suffix.union(y.suffix, true)
	}
	xy.canEmpty = x.canEmpty || y.canEmpty
	xy.match = x.match.or(y.match)

	xy.simplify(false)
	return xy
}

// addExact adds to the match query the trigrams for matching info.exact.
func (info *regexpInfo) addExact() {
	if info.exact.have() {
		info.match = info.match.andTrigrams(info.exact)
	}
}

// simplify simplifies the regexpInfo when the exact set gets too large.
func (info *regexpInfo) simplify(force bool) {
	// If there are now too many exact strings,
	// loop over them, adding trigrams and moving
	// the relevant pieces into prefix and suffix.
	info.exact.clean(false)
	if len(info.exact) > maxExact || (info.exact.minLen() >= 3 && force) || info.exact.minLen() >= 4 {
		info.addExact()
		for _, s := range info.exact {
			if runeLen(s) < 3 {
				info.prefix.add(s)
				info.suffix.add(s)
			} else {
				info.prefix.add(runePrefix(s, 2))
				info.suffix.add(runeSuffix(s, 2))
			}
		}
		info.exact = nil
	}

	if !info.exact.have() {
		info.prefix = info.simplifySet(info.prefix, false)
		info.suffix = info.simplifySet(info.suffix, true)
	}
}

// simplifySet reduces the size of the given set (the prefix set, or the
// suffix set when isSuffix). There is no need to pass around enormous
// prefix or suffix sets, since they will only be used to create trigrams.
// As they get too big, simplifySet moves the information they contain into
// the match query, which is more efficient to pass around.
func (info *regexpInfo) simplifySet(s stringSet, isSuffix bool) stringSet {
	t := s.copy()
	t.clean(isSuffix)

	// Add the OR of the current prefix/suffix set to the query.
	info.match = info.match.andTrigrams(t)

	for n := 3; n == 3 || t.size() > maxSet; n-- {
		// Replace set by strings of length n-1.
		w := 0
		for _, str := range t {
			if runeLen(str) >= n {
				if !isSuffix {
					str = runePrefix(str, n-1)
				} else {
					str = runeSuffix(str, n-1)
				}
			}
			if w == 0 || t[w-1] != str {
				t[w] = str
				w++
			}
		}
		t = t[:w]
		t.clean(isSuffix)
	}

	// Now make sure that the prefix/suffix sets aren't redundant.
	// For example, if we know "ab" is a possible prefix, then it
	// doesn't help at all to know that  "abc" is also a possible
	// prefix, so delete "abc".
	w := 0
	f := strings.HasPrefix
	if isSuffix {
		f = strings.HasSuffix
	}
	for _, str := range t {
		if w == 0 || !f(str, t[w-1]) {
			t[w] = str
			w++
		}
	}
	return t[:w]
}

func (info regexpInfo) String() string {
	s := ""
	if info.canEmpty {
		s += "canempty "
	}
	if info.exact.have() {
		s += "exact:" + strings.Join(info.exact, ",")
	} else {
		s += "prefix:" + strings.Join(info.prefix, ",")
		s += " suffix:" + strings.Join(info.suffix, ",")
	}
	s += " match: " + info.match.String()
	return s
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// runePrefix returns the first n runes of s.
func runePrefix(s string, n int) string {
	for i := range s {
		if n == 0 {
			return s[:i]
		}
		n--
	}
	return s
}

// runeSuffix returns the last n runes of s.
func runeSuffix(s string, n int) string {
	i := len(s)
	for ; n > 0 && i > 0; n-- {
		_, size := utf8.DecodeLastRuneInString(s[:i])
		i -= size
	}
	return s[i:]
}

// A stringSet is a set of strings.
// The nil stringSet indicates not having a set.
// The non-nil but empty stringSet is the empty set.
type stringSet []string

// have reports whether we have a stringSet.
func (s stringSet) have() bool {
	return s != nil
}

type byPrefix []string

func (x *byPrefix) Len() int           { return len(*x) }
func (x *byPrefix) Swap(i, j int)      { (*x)[i], (*x)[j] = (*x)[j], (*x)[i] }
func (x *byPrefix) Less(i, j int) bool { return (*x)[i] < (*x)[j] }

type bySuffix []string

func (x *bySuffix) Len() int      { return len(*x) }
func (x *bySuffix) Swap(i, j int) { (*x)[i], (*x)[j] = (*x)[j], (*x)[i] }
func (x *bySuffix) Less(i, j int) bool {
	s := (*x)[i]
	t := (*x)[j]
	for i := 1; i <= len(s) && i <= len(t); i++ {
		si := s[len(s)-i]
		ti := t[len(t)-i]
		if si < ti {
			return true
		}
		if si > ti {
			return false
		}
	}
	return len(s) < len(t)
}

// add adds str to the set.
func (s *stringSet) add(str string) {
	*s = append(*s, str)
}

// clean sorts the set in place and removes duplicates.
func (s *stringSet) clean(isSuffix bool) {
	t := *s
	if isSuffix {
		sort.Sort((*bySuffix)(s))
	} else {
		sort.Sort((*byPrefix)(s))
	}
	w := 0
	for _, str := range t {
		if w == 0 || t[w-1] != str {
			t[w] = str
			w++
		}
	}
	*s = t[:w]
}

// size returns the number of strings in s.
func (s stringSet) size() int {
	return len(s)
}

// minLen returns the length, in runes, of the shortest string in s.
func (s stringSet) minLen() int {
	if len(s) == 0 {
		return 0
	}
	m := runeLen(s[0])
	for _, str := range s {
		if n := runeLen(str); m > n {
			m = n
		}
	}
	return m
}

// union returns the union of s and t in new storage.
func (s stringSet) union(t stringSet, isSuffix bool) stringSet {
	u := make(stringSet, 0, len(s)+len(t))
	u = append(append(u, s...), t...)
	u.clean(isSuffix)
	return u
}

// cross returns the cross product of s and t.
func (s stringSet) cross(t stringSet, isSuffix bool) stringSet {
	p := stringSet{}
	for _, ss := range s {
		for _, tt := range t {
			p.add(ss + tt)
		}
	}
	p.clean(isSuffix)
	return p
}

// copy returns a copy of the set that does not share storage with the original.
func (s stringSet) copy() stringSet {
	if s == nil {
		return nil
	}
	return append(stringSet{}, s...)
}

// isSubsetOf returns true if all strings in s are also in t.
// It assumes both sets are sorted.
func (s stringSet) isSubsetOf(t stringSet) bool {
	j := 0
	for _, ss := range s {
		for j < len(t) && t[j] < ss {
			j++
		}
		if j >= len(t) || t[j] != ss {
			return false
		}
	}
	return true
}
