package transcript

import "testing"

func TestReparseKey(t *testing.T) {
	cases := map[string]string{
		"claude@3.7": "claude@3", "claude@3": "claude@3",
		"codex@3.2+codex-events@1.4/extraction@1/abc": "codex@3+codex-events@1/extraction@1/abc",
		"claude@next": "claude@next",
	}
	for input, want := range cases {
		if got := ReparseKey(input); got != want {
			t.Fatalf("%q: %q want %q", input, got, want)
		}
	}
	if Contract("claude@3.1", nil, 100, 200) != Contract("claude@3.2", nil, 100, 200) {
		t.Fatal("minor release changed extraction policy")
	}
	if Contract("claude@3.1", nil, 100, 200) == Contract("claude@4.0", nil, 100, 200) {
		t.Fatal("major release did not change extraction policy")
	}
}
