package retrieval

import (
	"regexp"
	"regexp/syntax"
	"strings"
	"unicode"
)

const maxRegexpRequirementParams = 128

// requiredRegexpCond retains whole literal spans around variable regex
// syntax instead of testing every required trigram independently. It is a
// necessary condition only: Go's regexp still verifies each candidate.
// Build first so a bounded fallback never leaves SQL parameters behind.
func requiredRegexpCond(q *query, re *regexp.Regexp) string {
	if len(re.String()) > maxRegexpLiteralSourceBytes {
		return ""
	}
	for _, r := range re.String() {
		if r > unicode.MaxASCII {
			return ""
		}
	}
	ast, err := syntax.Parse(re.String(), syntax.Perl)
	if err != nil {
		return ""
	}
	remaining := maxRegexpRequirementParams
	n, ok := literalRequirements(ast, &remaining)
	if !ok || n == nil {
		return ""
	}
	return n.sql(q)
}

type literalRequirement struct {
	literals []string
	op       string
	sub      []*literalRequirement
}

func literalRequirements(re *syntax.Regexp, remaining *int) (*literalRequirement, bool) {
	if literals, ok := regexpLiterals(re); ok {
		return literalLeaf(literals, remaining)
	}
	switch re.Op {
	case syntax.OpCapture:
		return literalRequirements(re.Sub[0], remaining)
	case syntax.OpPlus:
		return literalRequirements(re.Sub[0], remaining)
	case syntax.OpRepeat:
		if re.Min > 0 {
			return literalRequirements(re.Sub[0], remaining)
		}
	case syntax.OpConcat:
		// Adjacent finite AST children are one contiguous span, even
		// across captures. Never infer adjacency from unordered trigrams.
		n := &literalRequirement{op: " AND "}
		pending := []string{""}
		flush := func() bool {
			child, ok := literalLeaf(pending, remaining)
			if child != nil {
				n.sub = append(n.sub, child)
			}
			pending = []string{""}
			return ok
		}
		for _, sub := range re.Sub {
			if values, ok := regexpLiterals(sub); ok {
				if len(pending)*len(values) > maxRegexpLiterals {
					return nil, false
				}
				var joined []string
				for _, a := range pending {
					for _, b := range values {
						joined = append(joined, a+b)
					}
				}
				pending = joined
				continue
			}
			if !flush() {
				return nil, false
			}
			child, ok := literalRequirements(sub, remaining)
			if !ok {
				return nil, false
			}
			if child != nil {
				n.sub = append(n.sub, child)
			}
		}
		if !flush() {
			return nil, false
		}
		if len(n.sub) > 0 {
			return n, true
		}
	case syntax.OpAlternate:
		n := &literalRequirement{op: " AND "}
		if re.Op == syntax.OpAlternate {
			n.op = " OR "
		}
		for _, sub := range re.Sub {
			child, ok := literalRequirements(sub, remaining)
			if !ok {
				return nil, false
			}
			if child == nil {
				// An unconstrained alternate may match without any other
				// branch's literals. Concatenation may retain its siblings.
				if re.Op == syntax.OpAlternate {
					return nil, true
				}
				continue
			}
			n.sub = append(n.sub, child)
		}
		if len(n.sub) > 0 {
			return n, true
		}
	}
	// Optional, zero-width, Unicode and unsupported nodes impose no safe
	// whole-literal requirement; the original trigram path remains available.
	return nil, true
}

func literalLeaf(literals []string, remaining *int) (*literalRequirement, bool) {
	for _, s := range literals {
		if s == "" || strings.ContainsRune(s, 0) || !likeIndexable(s) {
			return nil, true
		}
	}
	*remaining -= len(literals)
	if *remaining < 0 {
		return nil, false
	}
	return &literalRequirement{literals: literals}, true
}

func (n *literalRequirement) sql(q *query) string {
	var parts []string
	op := n.op
	if len(n.literals) > 0 {
		op = " OR "
		for _, s := range n.literals {
			parts = append(parts, "m.text ILIKE "+q.arg("%"+likeEscape(s)+"%"))
		}
	} else {
		for _, child := range n.sub {
			parts = append(parts, child.sql(q))
		}
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return "(" + strings.Join(parts, op) + ")"
}
