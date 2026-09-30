package regexq

import (
	"regexp/syntax"
	"testing"
)

// Upstream's table (codesearch index/regexp_test.go), with expectations
// changed where this port differs on purpose: ASCII is folded to lower case
// (the index is case-insensitive), so upper-case input and (?i) plan as
// lower-case trigrams instead of every case variant.
var queryTests = []struct {
	re string
	q  string
}{
	{`Abcdef`, `"abc" "bcd" "cde" "def"`},
	{`(abc)(def)`, `"abc" "bcd" "cde" "def"`},
	{`abc.*(def|ghi)`, `"abc" ("def"|"ghi")`},
	{`abc(def|ghi)`, `"abc" ("bcd" "cde" "def")|("bcg" "cgh" "ghi")`},
	{`a+hello`, `"ahe" "ell" "hel" "llo"`},
	{`(a+hello|b+world)`, `("ahe" "ell" "hel" "llo")|("bwo" "orl" "rld" "wor")`},
	{`a*bbb`, `"bbb"`},
	{`a?bbb`, `"bbb"`},
	{`(bbb)a?`, `"bbb"`},
	{`(bbb)a*`, `"bbb"`},
	{`^abc`, `"abc"`},
	{`abc$`, `"abc"`},
	{`ab[cde]f`, `("abc" "bcf")|("abd" "bdf")|("abe" "bef")`},
	{`(abc|bac)de`, `"cde" ("abc" "bcd")|("acd" "bac")`},

	// These don't have enough letters for a trigram, so they return the
	// always matching query "+".
	{`ab[^cde]f`, `+`},
	{`ab.f`, `+`},
	{`.`, `+`},
	{`()`, `+`},

	// No matches.
	{`[^\s\S]`, `-`},

	// Factoring works.
	{`(abc|abc)`, `"abc"`},
	{`(ab|ab)c`, `"abc"`},
	{`ab(cab|cat)`, `"abc" "bca" ("cab"|"cat")`},
	{`(z*(abc|def)z*)(z*(abc|def)z*)`, `("abc"|"def")`},
	{`(z*abcz*defz*)|(z*abcz*defz*)`, `"abc" "def"`},
	{`(z*abcz*defz*(ghi|jkl)z*)|(z*abcz*defz*(mno|prs)z*)`,
		`"abc" "def" ("ghi"|"jkl"|"mno"|"prs")`},
	{`(z*(abcz*def)|(ghiz*jkl)z*)|(z*(mnoz*prs)|(tuvz*wxy)z*)`,
		`("abc" "def")|("ghi" "jkl")|("mno" "prs")|("tuv" "wxy")`},
	{`(z*abcz*defz*)(z*(ghi|jkl)z*)`, `"abc" "def" ("ghi"|"jkl")`},
	{`(z*abcz*defz*)|(z*(ghi|jkl)z*)`, `("ghi"|"jkl")|("abc" "def")`},

	// analyze keeps track of multiple possible prefix/suffixes.
	{`[ab][cd][ef]`, `("ace"|"acf"|"ade"|"adf"|"bce"|"bcf"|"bde"|"bdf")`},
	{`ab[cd]e`, `("abc" "bce")|("abd" "bde")`},

	// Different sized suffixes.
	{`(a|ab)cde`, `"cde" ("abc" "bcd")|("acd")`},
	{`(a|b|c|d)(ef|g|hi|j)`, `+`},

	{`(?s).`, `+`},

	// Case: folded to the index's lower case.
	{`(?i)a~~`, `"a~~"`},
	{`(?i)ab~`, `"ab~"`},
	{`(?i)abc`, `"abc"`},
	{`(?i)abc|def`, `("abc"|"def")`},
	{`(?i)abcd`, `"abc" "bcd"`},
	{`(?i)abc|abc`, `"abc"`},
	{`ABC|abc`, `"abc"`},
	// Go's (?i)k also matches U+212A KELVIN SIGN and (?i)s U+017F LONG S;
	// they stay alternatives, since FTS5 folds them itself.
	{`(?i)kay`, `("kay"|"Kay")`},

	// Word boundary.
	{`\b`, `+`},
	{`\B`, `+`},
	{`\babc`, `"abc"`},
	{`\Babc`, `"abc"`},
	{`abc\b`, `"abc"`},
	{`abc\B`, `"abc"`},
	{`ab\bc`, `"abc"`},
	{`ab\Bc`, `"abc"`},

	// Trigrams are counted in characters, not bytes.
	{`日本語`, `"日本語"`},
	{`日本`, `+`},
	{`é日本語x`, `"é日本" "日本語" "本語x"`},
	// NUL never becomes a required trigram.
	{"ab\x00cd", `+`},
}

func TestQuery(t *testing.T) {
	for _, tt := range queryTests {
		re, err := syntax.Parse(tt.re, syntax.Perl)
		if err != nil {
			t.Fatal(err)
		}
		q := RegexpQuery(re).String()
		if q != tt.q {
			t.Errorf("RegexpQuery(%#q) = %#q, want %#q", tt.re, q, tt.q)
		}
	}
}

func TestRuneCuts(t *testing.T) {
	if got := runePrefix("日本語", 2); got != "日本" {
		t.Fatal(got)
	}
	if got := runeSuffix("日本語", 2); got != "本語" {
		t.Fatal(got)
	}
	if got := runeSuffix("ab", 5); got != "ab" {
		t.Fatal(got)
	}
}
