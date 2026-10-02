// Package redact masks secrets in transcript bytes before they leave the
// device, and again on the server when it parses or serves archived bytes
// (notes/redaction.md).
//
// Redaction is length-preserving: a secret of n bytes becomes exactly n
// bytes of marker, so every byte offset (message addresses, read --raw
// ranges, chunk manifests) means the same thing on the device, in the
// archive and in the local index. Markers never contain '"', '\' or
// control bytes, and a match that starts or ends inside a JSON escape is
// widened to cover it, so a redacted JSON line still parses.
//
// Markers, by space available:
//
//	[REDACTED:<rule>:<h8>]***   h8: first 8 hex of SHA-256 over the secret
//	[REDACTED:<rule>]***        (password-class rules never carry h8)
//	[REDACTED]***
//	*****
//
// The hash lets two hits of the same token correlate without storing any
// reversible form; password-class rules omit it because a guessable
// password plus a 32-bit hash confirms a guess. Nothing in this package
// logs or returns the matched text.
package redact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"slices"
	"sync"
	"unicode/utf8"
)

// Match is one masked region of a scanned buffer.
type Match struct {
	Start, End int
	Rule       string
	hashed     bool
	prio       int // rule order: the earlier rule names a merged region
}

// Find returns the regions of b to mask, sorted and non-overlapping. b is
// one segment (a line, or a window of a larger file); matches never span
// segments.
func Find(b []byte) []Match {
	hp := hitPool.Get().(*[]acHit)
	defer hitPool.Put(hp)
	hits := ac.scan(b, (*hp)[:0])
	*hp = hits
	if len(hits) == 0 {
		return nil
	}
	var ms []Match
	// Windows per rule, in hit order; a window that overlaps the previous
	// one of the same rule extends it.
	var win map[int][][2]int
	for _, h := range hits {
		p := &acPatterns[h.pat]
		start := h.end - len(p.text)
		if p.assign {
			if m, ok := assignAt(b, start, h.end); ok {
				ms = append(ms, m)
			}
			continue
		}
		for _, ri := range p.rules {
			r := &rules[ri]
			if !r.ci && string(b[start:h.end]) != string(p.text) {
				continue
			}
			if win == nil {
				win = map[int][][2]int{}
			}
			lo, hi := max(0, start-r.back), min(len(b), start+r.fwd)
			ws := win[ri]
			if n := len(ws); n > 0 && lo <= ws[n-1][1] {
				ws[n-1][1] = max(ws[n-1][1], hi)
			} else {
				win[ri] = append(ws, [2]int{lo, hi})
			}
		}
	}
	for ri := range rules {
		for _, w := range win[ri] {
			ms = matchWindow(b, ri, w[0], w[1], ms)
		}
	}
	return normalize(b, ms)
}

var hitPool = sync.Pool{New: func() any { return new([]acHit) }}

// matchWindow runs rule ri's regex on b[lo:hi], growing hi while a match
// runs into it (a longer token than the window guessed).
func matchWindow(b []byte, ri, lo, hi int, ms []Match) []Match {
	r := &rules[ri]
	for {
		locs := r.re.FindAllSubmatchIndex(b[lo:hi], -1)
		if hi < len(b) && len(locs) > 0 && locs[len(locs)-1][1] == hi-lo {
			hi = min(len(b), lo+2*(hi-lo))
			continue
		}
		for _, loc := range locs {
			s, e := pick(loc, r.group)
			if s < 0 || e <= s {
				continue
			}
			s, e = s+lo, e+lo
			if r.bound && (!boundaryBefore(b, s) || e < len(b) && isAlnum(b[e])) {
				continue
			}
			v := b[s:e]
			if r.validate != nil && !r.validate(v) || allowlisted(v) {
				continue
			}
			if r.skip != nil {
				sub := make([][]byte, len(loc)/2)
				for g := range sub {
					if loc[2*g] >= 0 {
						sub[g] = b[lo+loc[2*g] : lo+loc[2*g+1]]
					}
				}
				if r.skip(sub) {
					continue
				}
			}
			ms = append(ms, Match{Start: s, End: e, Rule: r.id, hashed: r.hashed, prio: ri})
		}
		return ms
	}
}

// pick returns the span of the rule's group: 0 whole match, n group n,
// -1 the first group that matched.
func pick(loc []int, group int) (int, int) {
	if group >= 0 {
		return loc[2*group], loc[2*group+1]
	}
	for g := 1; 2*g+1 < len(loc); g++ {
		if loc[2*g] >= 0 {
			return loc[2*g], loc[2*g+1]
		}
	}
	return -1, -1
}

// normalize widens matches to whole JSON escapes, then sorts and merges
// overlaps (the earlier, higher-priority rule names the merged region).
func normalize(b []byte, ms []Match) []Match {
	if len(ms) == 0 {
		return nil
	}
	for i := range ms {
		ms[i].Start, ms[i].End = widenEscapes(b, ms[i].Start, ms[i].End)
	}
	slices.SortFunc(ms, func(x, y Match) int {
		if x.Start != y.Start {
			return x.Start - y.Start
		}
		if x.prio != y.prio {
			return x.prio - y.prio
		}
		return y.End - x.End
	})
	out := ms[:1]
	for _, m := range ms[1:] {
		last := &out[len(out)-1]
		if m.Start < last.End {
			last.End = max(last.End, m.End)
			continue
		}
		out = append(out, m)
	}
	return out
}

// widenEscapes moves s back and e forward so neither falls inside a JSON
// escape sequence (\x or \uXXXX), at any nesting depth: JSON inside a
// JSON string (Devin exports, stringified tool input) escapes its own
// escapes, so an escape is a run of backslashes plus its character.
func widenEscapes(b []byte, s, e int) (int, int) {
	if lo, _ := escAt(b, s); lo >= 0 {
		s = lo
	}
	if e < len(b) {
		if _, hi := escAt(b, e); hi >= 0 {
			e = hi
		}
	}
	return s, e
}

// escAt returns the escape sequence [lo, hi) with lo < i < hi, or -1s:
// the nearest backslash run within six bytes before i, its escaped
// character, and four hex digits after a 'u'.
func escAt(b []byte, i int) (int, int) {
	for j := i - 1; j >= 0 && j >= i-6; j-- {
		if b[j] != '\\' {
			continue
		}
		lo := j
		for lo > 0 && b[lo-1] == '\\' {
			lo--
		}
		hi := j + 1
		if hi < len(b) && b[hi] == 'u' {
			hi = min(len(b), hi+5)
		} else if hi < len(b) {
			hi++
		}
		// A truncated escape (\u before non-hex text) may end inside a
		// multibyte character; stop at the next character boundary so the
		// mask never leaves half a character behind.
		for hi < len(b) && !utf8.RuneStart(b[hi]) {
			hi++
		}
		if lo < i && i < hi {
			return lo, hi
		}
		return -1, -1
	}
	return -1, -1
}

// Apply writes the marker of each match into dst, which holds a copy of
// src (the scanned segment); both have the same length.
func Apply(dst, src []byte, ms []Match) {
	for _, m := range ms {
		marker(dst[m.Start:m.End], src[m.Start:m.End], m.Rule, m.hashed)
	}
}

// Redact returns b with every secret masked, and the matches.
func Redact(b []byte) ([]byte, []Match) {
	ms := Find(b)
	if len(ms) == 0 {
		return b, nil
	}
	out := bytes.Clone(b)
	Apply(out, b, ms)
	return out, ms
}

// marker fills dst (len n) with the best marker that fits.
func marker(dst, secret []byte, rule string, hashed bool) {
	n := len(dst)
	var m string
	if hashed {
		sum := sha256.Sum256(append([]byte("flopwire-redact\x00"), secret...))
		m = "[REDACTED:" + rule + ":" + hex.EncodeToString(sum[:4]) + "]"
	}
	for _, cand := range []string{m, "[REDACTED:" + rule + "]", "[REDACTED]"} {
		if cand != "" && len(cand) <= n {
			k := copy(dst, cand)
			for i := k; i < n; i++ {
				dst[i] = '*'
			}
			return
		}
	}
	for i := range dst {
		dst[i] = '*'
	}
}

func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// boundaryBefore: position i starts a new token: at the buffer start,
// after a non-alphanumeric byte, or after a JSON escape like \n.
func boundaryBefore(b []byte, i int) bool {
	if i == 0 || !isAlnum(b[i-1]) {
		return true
	}
	return i >= 2 && b[i-2] == '\\' && escaping(b, i-2) && (b[i-1] == 'n' || b[i-1] == 'r' || b[i-1] == 't')
}

// isMarker: v is (or starts) a marker this package wrote, or a run of '*'.
func isMarker(v []byte) bool {
	if bytes.HasPrefix(v, []byte("[REDACTED")) || bytes.HasPrefix(v, []byte("REDACTED")) {
		return true
	}
	return len(bytes.Trim(v, "*")) == 0
}

// allowlisted: documented example credentials.
func allowlisted(v []byte) bool {
	return bytes.HasSuffix(v, []byte("EXAMPLE")) || bytes.Contains(v, []byte("EXAMPLEKEY")) || isMarker(v)
}

// isPlaceholder: common stand-ins for a real value.
func isPlaceholder(v []byte) bool {
	l := bytes.ToLower(v)
	for _, p := range [][]byte{[]byte("changeme"), []byte("placeholder"), []byte("example"), []byte("your_"), []byte("your-"),
		[]byte("xxxx"), []byte("dummy"), []byte("redacted"), []byte("<"), []byte("${"), []byte("{{"), []byte("...")} {
		if bytes.Contains(l, p) {
			return true
		}
	}
	return repeated(v)
}

// repeated: one byte makes up at least 3/4 of v ("aaaa", "****", "0000").
func repeated(v []byte) bool {
	var freq [256]int
	for _, c := range v {
		freq[c]++
	}
	return slices.Max(freq[:])*4 >= len(v)*3
}

func hasDigitOrSymbol(v []byte) bool {
	for _, c := range v {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return true
		}
	}
	return false
}

// entropy is the Shannon entropy of v in bits per byte.
func entropy(v []byte) float64 {
	if len(v) == 0 {
		return 0
	}
	var freq [256]int
	for _, c := range v {
		freq[c]++
	}
	n := float64(len(v))
	h := 0.0
	for _, f := range freq {
		if f > 0 {
			p := float64(f) / n
			h -= p * math.Log2(p)
		}
	}
	return h
}

// escaping reports whether the backslash at j starts an escape: an even
// number of backslashes precede it.
func escaping(b []byte, j int) bool {
	n := 0
	for k := j - 1; k >= 0 && b[k] == '\\'; k-- {
		n++
	}
	return n%2 == 0
}
