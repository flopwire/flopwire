package format

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCutFocus(t *testing.T) {
	for _, tc := range []struct {
		name, text, want               string
		offset, line, budget, from, to int
		clipped                        bool
	}{
		{"long preceding line", strings.Repeat("x", 2000) + "\nrationale\nlater", "rationale\nlater", 0, 2, 500, 2, 3, true},
		{"nearest context fits", strings.Repeat("x", 2000) + "\na\nb\nrationale\nlater", "a\nb\nrationale\nlater", 0, 4, 500, 2, 5, true},
		{"reserve complete target", "context\nrationale\nlater", "rationale", 0, 2, 9, 2, 2, true},
		{"exact fit", "ctx\nfocus\nlater", "ctx\nfocus", 0, 2, 9, 1, 2, true},
		{"five preceding lines", "0\n1\n2\n3\n4\n5\nfocus\nlater", "1\n2\n3\n4\n5\nfocus\nlater", 0, 7, 100, 2, 8, true},
		{"oversized target", "ctx\nhéllo world\nlater", "h", 0, 2, 2, 2, 2, true},
		{"rune exceeds budget", "ctx\né\nlater", "", 0, 2, 1, 2, 2, true},
		{"explicit offset before target", strings.Repeat("x", 20) + "\nfocus", "xxxxx", 1, 2, 5, 1, 1, true},
		{"explicit offset after target", "ctx\nfocus\nlater", "later", 3, 2, 100, 3, 3, true},
		{"uncited unchanged", "ctx\nfocus", "ctx", 0, 0, 5, 1, 1, true},
		{"first line citation", "focus\nlater", "focus", 0, 1, 5, 1, 1, true},
		{"unbounded", "ctx\nfocus\nlater\n", "ctx\nfocus\nlater", 0, 2, 0, 1, 3, false},
		{"empty text", "", "", 0, 1, 5, 1, 1, false},
		{"citation past end", "ctx\nlast", "last", 0, 99, 5, 2, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := CutFocus(tc.text, tc.offset, tc.line, tc.budget)
			if e.Text != tc.want || e.From != tc.from || e.To != tc.to || e.Clipped != tc.clipped || !utf8.ValidString(e.Text) {
				t.Fatalf("excerpt %+v, want text=%q lines=%d-%d clipped=%v", e, tc.want, tc.from, tc.to, tc.clipped)
			}
			if tc.budget > 0 && len(e.Text) > tc.budget {
				t.Fatalf("%d bytes exceed budget %d", len(e.Text), tc.budget)
			}
		})
	}
}
