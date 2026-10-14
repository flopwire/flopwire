package busrender

import (
	"strings"
	"testing"
)

func TestReferenceTextBoundsSourceBeforeNormalization(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"one\n two", "user: one two"},
		{strings.Repeat(" ", 640) + "outside window", ""},
		{"\x1b[31mred\x1b[0m", "user: ␛[31mred␛[0m"},
		{"", ""},
	} {
		if got := ReferenceText(tc.text, "user"); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
	if got := ReferenceText(strings.Repeat("界", 400), "user"); len(got) > 4*MaxExcerptBytes {
		t.Fatalf("unbounded UTF-8 excerpt: %d", len(got))
	}
}
