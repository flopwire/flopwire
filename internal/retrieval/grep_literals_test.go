package retrieval

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/grep"
	"github.com/jackc/pgx/v5"
)

func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
}

func TestLiteralRegexpCondition(t *testing.T) {
	for _, tt := range []struct {
		pattern   string
		sensitive bool
		want      []string
	}{
		{"1f877ad98|repo/additive-migrations-2049", false, []string{"%1f877ad98%", "%repo/additive-migrations-2049%", "%repo/additive-migrationſ-2049%"}},
		{"1f877ad98|repo/additive-migrations-2049", true, []string{"%1f877ad98%", "%repo/additive-migrations-2049%"}},
		{"(foobar)|(foobaz)|quux", true, []string{"%foobar%", "%foobaz%", "%quux%"}},
		{"foo|foobar", true, []string{"%foo%", "%foobar%"}},
		{"(?i:ask)|(?-i:SKY)", true, []string{"%ask%", "%asK%", "%aſk%", "%aſK%", "%sky%"}},
		{`foo\%\_\\bar\.baz`, true, []string{`%foo\%\_\\bar.baz%`}},
	} {
		t.Run(fmt.Sprintf("%s/sensitive=%v", tt.pattern, tt.sensitive), func(t *testing.T) {
			plan, err := grep.Compile(format.GrepQuery{Pattern: tt.pattern, CaseSensitive: tt.sensitive})
			if err != nil {
				t.Fatal(err)
			}
			q := &query{}
			got := literalRegexpCond(q, plan.Re)
			if got == "" || strings.Contains(got, " AND ") || strings.Count(got, "m.text ILIKE") != len(tt.want) {
				t.Fatalf("condition = %q, args = %v", got, q.args)
			}
			var args []string
			for _, a := range q.args {
				args = append(args, asciiLower(a.(string)))
			}
			if !reflect.DeepEqual(args, tt.want) {
				t.Fatalf("args = %q, want %q", args, tt.want)
			}
		})
	}
}

func TestLiteralRegexpFallback(t *testing.T) {
	for _, pattern := range []string{
		"abc.*bcd", "abc.+bcd", "abc(def)?", "abc{2}", "^foobar$", `\bfoobar\b`,
		"foo|", "foo|x", ".*", "a.b", "", "abc\\x00def", "café", `caf\x{e9}`,
		strings.Repeat("z", maxRegexpLiteralSourceBytes+1), "ssssss", "kkkkkk", "foo[0-9][0-9]", "[a-z][a-z]foobar",
	} {
		t.Run(pattern, func(t *testing.T) {
			plan, err := grep.Compile(format.GrepQuery{Pattern: pattern})
			if err != nil {
				t.Fatal(err)
			}
			q := &query{}
			q.arg("existing")
			if got := literalRegexpCond(q, plan.Re); got != "" {
				t.Fatalf("optimized unsafe pattern: %q %v", got, q.args)
			}
			if !reflect.DeepEqual(q.args, []any{"existing"}) {
				t.Fatalf("fallback mutated args: %v", q.args)
			}
		})
	}
	for _, pattern := range []string{"[", "(?=abc)", `abc\`} {
		if _, err := grep.Compile(format.GrepQuery{Pattern: pattern}); err == nil {
			t.Fatalf("bad pattern %q accepted", pattern)
		}
	}
}

func TestLiteralRegexpExistingBadRequests(t *testing.T) {
	for _, pattern := range []string{"", "[", "foo|x", "go to", ".*"} {
		_, err := (&Store{}).Grep(context.Background(), format.GrepQuery{Pattern: pattern}, format.Filters{})
		if !errors.Is(err, ErrBadRequest) {
			t.Fatalf("pattern %q: want bad request, got %v", pattern, err)
		}
	}
}

func TestLiteralRegexpFoldBound(t *testing.T) {
	plan, err := grep.Compile(format.GrepQuery{Pattern: "sssss"})
	if err != nil {
		t.Fatal(err)
	}
	q := &query{}
	if literalRegexpCond(q, plan.Re) == "" || len(q.args) != maxRegexpLiterals {
		t.Fatalf("five long-s positions must yield 32 variants: %v", q.args)
	}
}

var literalEquivalenceQueries = []format.GrepQuery{
	{Pattern: "1f877ad98|repo/additive-migrations-2049"},
	{Pattern: "1f877ad98|repo/additive-migrations-2049", CaseSensitive: true},
	{Pattern: "(foobar)|(foobaz)|quux"},
	{Pattern: "(foo|bar)baz"},
	{Pattern: "foo(|bar)"},
	{Pattern: `\Qfoo.bar\E`},
	{Pattern: `a(?i:\x{17f})k`},
	{Pattern: `a(?i:[k])s`},
	{Pattern: "(?i:a(?-i:S)b)", CaseSensitive: true},
	{Pattern: "ask|sky"},
	{Pattern: "ask|sky", CaseSensitive: true},
	{Pattern: "(?i:ask)|(?-i:SKY)", CaseSensitive: true},
	{Pattern: `foo\%\_\\bar\.baz`, CaseSensitive: true},
}

var literalEquivalenceTexts = []string{
	"prefix 1f877ad98 suffix", "1F877AD98", "repo/additive-migrations-2049", "REPO/ADDITIVE-MIGRATIONS-2049",
	"repo/additive-migrationſ-2049", "repo additive migrations 2049", "1f8---877---ad9---98",
	"foobaz", "barbaz", "foo", "foo.bar", "aSb", "ASB", "asb", "AſB", "foobar\nfoobaz", "FOOBAZ", "quux", "ASK", "aſk", "asK", "aſK", "ſKy", "SKY", "sky", "sKy",
	`foo%_\bar.baz`, `fooZZZbarXbaz`, "unrelated",
}

// Model ILIKE with ASCII folding only: no PostgreSQL Unicode fold behavior
// can hide a missing long-s/Kelvin candidate. Verification must recover
// exactly the same matching messages as scanning the unfiltered corpus.
func TestLiteralRegexpHitEquivalence(t *testing.T) {
	for _, gq := range literalEquivalenceQueries {
		plan, err := grep.Compile(gq)
		if err != nil {
			t.Fatal(err)
		}
		q := &query{}
		if literalRegexpCond(q, plan.Re) == "" {
			t.Fatalf("unexpected fallback: %+v", gq)
		}
		for _, body := range literalEquivalenceTexts {
			candidate := false
			for _, a := range q.args {
				// Remove surrounding wildcards and undo LIKE escaping.
				s := a.(string)
				s = strings.NewReplacer(`\\`, `\`, `\%`, `%`, `\_`, `_`).Replace(s[1 : len(s)-1])
				candidate = candidate || strings.Contains(asciiLower(body), asciiLower(s))
			}
			want := plan.Re.MatchString(body)
			if got := candidate && plan.Re.MatchString(body); got != want {
				t.Fatalf("%+v missed %q; args %v", gq, body, q.args)
			}
		}
	}
}

// This gate uses actual PostgreSQL ILIKE/escaping. pgtest reports SKIP
// when FLOPWIRE_TEST_DATABASE_URL is unset; it never reports a fake pass.
func TestLiteralRegexpPostgresHitEquivalence(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, pgtest.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	queries := append([]format.GrepQuery(nil), literalEquivalenceQueries...)
	queries = append(queries, format.GrepQuery{Pattern: "abc.*bcd", CaseSensitive: true})
	texts := append(append([]string(nil), literalEquivalenceTexts...), "abc---bcd", "abcd")
	for _, gq := range queries {
		t.Run(fmt.Sprintf("%s/sensitive=%v", gq.Pattern, gq.CaseSensitive), func(t *testing.T) {
			plan, err := grep.Compile(gq)
			if err != nil {
				t.Fatal(err)
			}
			q := &query{}
			condition := literalRegexpCond(q, plan.Re)
			if condition == "" {
				condition = trigramCond(q, plan.Query)
			}
			var values []string
			var want []int
			for i, body := range texts {
				values = append(values, fmt.Sprintf("(%d,%s::text)", i, q.arg(body)))
				if plan.Re.MatchString(body) {
					want = append(want, i)
				}
			}
			rows, err := conn.Query(ctx, "SELECT id,text FROM (VALUES "+strings.Join(values, ",")+") AS m(id,text) WHERE "+condition+" ORDER BY id", q.args...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var got []int
			for rows.Next() {
				var id int
				var body string
				if err := rows.Scan(&id, &body); err != nil {
					t.Fatal(err)
				}
				if plan.Re.MatchString(body) {
					got = append(got, id)
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("verified hits = %v, full scan = %v", got, want)
			}
		})
	}
}
