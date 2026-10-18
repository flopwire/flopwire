package format

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// SearchProbeCap bounds the index rows counted for each rarity probe. If
// every matching term reaches the cap, no rarest anchor is established.
const SearchProbeCap = 4096

func SearchMatching(s string) (string, error) {
	if s == "" {
		return "auto", nil
	}
	switch s {
	case "auto", "all", "any":
		return s, nil
	}
	return "", fmt.Errorf("matching: want auto, all or any, got %q", s)
}

// SearchMatch is a lexical strategy, chosen independently of page offset.
// Raw preserves explicit websearch operators on the server. Phrases remain
// mandatory even when Any ORs the unquoted words.
type SearchMatch struct {
	Raw            string
	Words, Phrases []string
	Any            bool
	Note           string
}

func (m SearchMatch) Text() string {
	if m.Any && len(m.Words) > 0 {
		return strings.Join(m.Words, " ")
	}
	return strings.Join(append(slices.Clone(m.Words), m.Phrases...), " ")
}

// WebQuery quotes data rather than allowing punctuation to become operators.
// Phrases are enforced separately by the server for an any-term strategy.
func (m SearchMatch) WebQuery() string {
	if m.Raw != "" {
		return m.Raw
	}
	var parts []string
	for _, w := range m.Words {
		parts = append(parts, `"`+strings.ReplaceAll(w, `"`, " ")+`"`)
	}
	sep := " "
	if m.Any {
		sep = " or "
	}
	text := strings.Join(parts, sep)
	if !m.Any || text == "" {
		for _, p := range m.Phrases {
			text += ` "` + p + `"`
		}
	}
	return strings.TrimSpace(text)
}

// PlanSearch uses index-backed probes: count up to limit, under the caller's
// single request deadline. Errors (including incomplete probes) stop planning.
func PlanSearch(ctx context.Context, text, matching string, probe func(context.Context, SearchMatch, int) (int, error)) (SearchMatch, error) {
	rawProbe := probe
	probe = func(ctx context.Context, m SearchMatch, cap int) (int, error) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		n, err := rawProbe(ctx, m, cap)
		if err == nil {
			err = ctx.Err()
		}
		return n, err
	}
	mode, err := SearchMatching(matching)
	if err != nil {
		return SearchMatch{}, err
	}
	words, phrases := SplitQuery(text)
	m := SearchMatch{Words: words, Phrases: phrases}
	// Operators belong to the backend parser, never the prose relaxation.
	for _, w := range words {
		if strings.EqualFold(w, "or") || strings.HasPrefix(w, "-") {
			m.Raw = text
			return m, nil
		}
	}
	if mode == "any" {
		m.Any = true
		return m, nil
	}
	// Preserve the original strict query, including backend tokenization.
	m.Raw = text
	if mode == "all" {
		return m, nil
	}
	n, err := probe(ctx, m, 1)
	if err != nil || n > 0 {
		return m, err
	}
	var kept []string
	for _, w := range words {
		w = strings.TrimFunc(strings.ToLower(w), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '*' })
		if w != "" && !stopwords[w] && !slices.Contains(kept, w) {
			kept = append(kept, w)
		}
	}
	if len(kept) == 0 {
		return m, nil
	}
	candidate := SearchMatch{Words: kept, Phrases: phrases}
	finish := func(c SearchMatch, anchor string) SearchMatch {
		required := slices.Clone(c.Words)
		for _, p := range c.Phrases {
			required = append(required, `"`+p+`"`)
		}
		c.Note = "no message has every query term; lexical fallback requires: " + strings.Join(required, " ")
		if anchor != "" {
			c.Note += " (rarest matching term: " + anchor + ")"
		}
		return c
	}
	n, err = probe(ctx, candidate, 1)
	if err != nil {
		return m, err
	}
	if n > 0 {
		return finish(candidate, ""), nil
	}
	counts := make(map[string]int, len(kept))
	anchor, rare := "", SearchProbeCap
	for _, w := range kept {
		n, err = probe(ctx, SearchMatch{Words: []string{w}}, rare)
		if err != nil {
			return m, err
		}
		counts[w] = n
		if n == rare {
			counts[w] = SearchProbeCap
		}
		if n > 0 && n < rare {
			anchor, rare = w, n
		}
	}
	if anchor == "" {
		if slices.ContainsFunc(kept, func(w string) bool { return counts[w] > 0 }) {
			m.Note = "no message has every query term; matching terms exceed the rarity probe limit; quote required topic terms or use matching=any for broad matches"
		}
		return m, nil
	}
	// Missing words go first; then drop the most common matching words.
	// Stable ties retain query order and never drop the anchor or phrases.
	slices.SortStableFunc(kept, func(a, b string) int {
		if a == b {
			return 0
		}
		if a == anchor {
			return 1
		}
		if b == anchor {
			return -1
		}
		if counts[a] == 0 && counts[b] != 0 {
			return -1
		}
		if counts[b] == 0 && counts[a] != 0 {
			return 1
		}
		return counts[b] - counts[a]
	})
	for len(kept) > 1 {
		kept = kept[1:]
		candidate.Words = slices.Clone(kept)
		n, err = probe(ctx, candidate, 1)
		if err != nil {
			return m, err
		}
		if n > 0 {
			return finish(candidate, anchor), nil
		}
	}
	return m, nil
}
