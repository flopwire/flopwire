package retrieval

import (
	"context"
	"fmt"
	"math/rand/v2"
	"reflect"
	"regexp/syntax"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/grep"
	"github.com/jackc/pgx/v5"
)

var requirementPatterns = []string{
	`foobar.*quux`, `(?i:ask).*(?-i:SKY)`, `ask.*sky`, `foo(?:bar)?baz`,
	`(foobar|quux).*baz`, `(foo|a).*bar`, `(foo|).*bar`, `foo[0-9]*bar`,
	`(foo|bar){2,4}.*baz`, `(?:foo.*bar)+`, `^foobar$`, `\bfoobar\b`,
	`a(bc)d.*xyz`, `ssssss.*quux`, `(?i:a\x{17f}k).*foo`,
	`example\.test.*(saml|sso|okta|entra|google workspace|idp)|(saml|sso|okta|entra|idp).*example\.test`,
	`gh issue create.*(1234|Shared controls)|issues/1234|Shared controls`,
	`demo-mockup[a-z0-9.-]*\.workers\.dev/[^ )"]*author[^ )"]*`,
	`read:project|gh auth refresh|project.*token|token.*project`,
}

func TestRequiredRegexpConditions(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		want    []string
	}{
		{`foobar.*quux`, []string{"%foobar%", "%quux%"}},
		{`a(bc)d.*xyz`, []string{"%abcd%", "%xyz%"}},
		{`(foo|a).*bar`, []string{"%bar%"}},
		{`foo(?:bar)?baz`, []string{"%foo%", "%baz%"}},
		{`(foo|bar){2,4}.*baz`, []string{"%foo%", "%bar%", "%baz%"}},
	} {
		plan, err := grep.Compile(format.GrepQuery{Pattern: tc.pattern, CaseSensitive: true})
		if err != nil {
			t.Fatal(err)
		}
		q := &query{}
		got := requiredRegexpCond(q, plan.Re)
		var args []string
		for _, a := range q.args {
			args = append(args, a.(string))
		}
		if got == "" || !reflect.DeepEqual(args, tc.want) {
			t.Fatalf("%s: %s %v", tc.pattern, got, args)
		}
	}
	for _, pattern := range []string{`foo|a`, `(?:foo)?`, `foo.*|a.*`, `café.*quux`, strings.Repeat("z", 1025), strings.Repeat("ask.*", 33)} {
		plan, err := grep.Compile(format.GrepQuery{Pattern: pattern})
		if err != nil {
			t.Fatal(err)
		}
		q := &query{}
		q.arg("existing")
		if got := requiredRegexpCond(q, plan.Re); got != "" || !reflect.DeepEqual(q.args, []any{"existing"}) {
			t.Fatalf("fallback mutated SQL for %s: %s %v", pattern, got, q.args)
		}
	}
}

func requirementMatches(n *literalRequirement, body string) bool {
	if n == nil {
		return true
	}
	if len(n.literals) > 0 {
		for _, s := range n.literals {
			if strings.Contains(asciiLower(body), asciiLower(s)) {
				return true
			}
		}
		return false
	}
	if n.op == " OR " {
		for _, child := range n.sub {
			if requirementMatches(child, body) {
				return true
			}
		}
		return false
	}
	for _, child := range n.sub {
		if !requirementMatches(child, body) {
			return false
		}
	}
	return true
}

// The conservative ILIKE model folds ASCII only. Every Go match must remain
// a candidate even when PostgreSQL does not fold long-s or Kelvin to ASCII.
func TestRequiredRegexpCandidateProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(87, 11))
	patterns := append([]string(nil), requirementPatterns...)
	literals := []string{"foo", "bar", "baz", "ask", "sky", "a", "ab", "quux"}
	for i := 0; i < 500; i++ {
		a, b, c := literals[r.IntN(len(literals))], literals[r.IntN(len(literals))], literals[r.IntN(len(literals))]
		patterns = append(patterns, fmt.Sprintf([]string{`%s.*%s.*%s`, `(%s|%s).*%s`, `%s(%s)?%s`, `(%s|%s)+%s`, `(?i:%s).*(?-i:%s)%s`}[r.IntN(5)], a, b, c))
	}
	texts := append([]string(nil), literalEquivalenceTexts...)
	texts = append(texts, "aſK gap ſKy", "ASK gap SKY", "foobarbaz", "foobar\nquux", "abcd gap xyz")
	for i := 0; i < 250; i++ {
		var b strings.Builder
		for j := 0; j < 5; j++ {
			b.WriteString(literals[r.IntN(len(literals))])
			if r.IntN(3) == 0 {
				b.WriteByte(' ')
			}
		}
		texts = append(texts, b.String())
	}
	matches := 0
	for _, pattern := range patterns {
		for _, sensitive := range []bool{false, true} {
			plan, err := grep.Compile(format.GrepQuery{Pattern: pattern, CaseSensitive: sensitive})
			if err != nil {
				t.Fatal(err)
			}
			ast, err := syntax.Parse(plan.Re.String(), syntax.Perl)
			if err != nil {
				t.Fatal(err)
			}
			remaining := 128
			n, ok := literalRequirements(ast, &remaining)
			if !ok {
				continue
			}
			for _, text := range texts {
				if plan.Re.MatchString(text) {
					matches++
					if !requirementMatches(n, text) {
						t.Fatalf("candidate excluded matching text: pattern=%s sensitive=%v body=%q", pattern, sensitive, text)
					}
				}
			}
		}
	}
	if matches < 1000 {
		t.Fatalf("vacuous property corpus: only %d matches", matches)
	}
	t.Logf("checked %d regex/text pairs and %d actual matches", len(patterns)*2*len(texts), matches)
}

func TestRequiredRegexpPostgresEquivalence(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, pgtest.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	texts := append([]string(nil), literalEquivalenceTexts...)
	texts = append(texts, "aſK gap ſKy", "ASK gap SKY", "foobarbaz", "foo123bar", "foofoo gap baz", "barbar gap baz", "abcd gap xyz", "ſſſſſſ gap quux", "example.test SAML", "SAML example.test", "gh issue create Shared controls", "issues/1234", "demo-mockup.workers.dev/authorize-flow", "project has token")
	for _, pattern := range requirementPatterns {
		for _, sensitive := range []bool{false, true} {
			plan, err := grep.Compile(format.GrepQuery{Pattern: pattern, CaseSensitive: sensitive})
			if err != nil {
				t.Fatal(err)
			}
			q := &query{}
			cond := requiredRegexpCond(q, plan.Re)
			if cond == "" {
				cond = trigramCond(q, plan.Query)
			}
			if cond == "" {
				continue
			}
			var values []string
			var want []int
			for i, text := range texts {
				values = append(values, fmt.Sprintf("(%d,%s::text)", i, q.arg(text)))
				if plan.Re.MatchString(text) {
					want = append(want, i)
				}
			}
			rows, err := conn.Query(ctx, `SELECT id,text FROM (VALUES `+strings.Join(values, ",")+`) m(id,text) WHERE `+cond+` ORDER BY id`, q.args...)
			if err != nil {
				t.Fatal(err)
			}
			var got []int
			for rows.Next() {
				var i int
				var text string
				if err := rows.Scan(&i, &text); err != nil {
					t.Fatal(err)
				}
				if plan.Re.MatchString(text) {
					got = append(got, i)
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s sensitive=%v: verified=%v oracle=%v", pattern, sensitive, got, want)
			}
		}
	}
}
