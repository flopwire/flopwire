package digest

import (
	"github.com/flopwire/flopwire/internal/transcript"
	"strings"
	"testing"
)

func TestMayContainCommitEvidence(t *testing.T) {
	for _, tt := range []struct {
		text string
		want bool
	}{
		{"ordinary output", false}, {"abcdef", false}, {"[main abcdef0] fix", true},
		{"xABCDEF0x", true}, {`{"exit_code":0,"output":"\u0061\u0062\u0063\u0064\u0065\u0066\u0030"}`, true},
		{`{"exit_code":0,"output":""}`, true}, {"123 456 789", false},
	} {
		if got := MayContainCommitEvidence(tt.text); got != tt.want {
			t.Errorf("%q: %v, want %v", tt.text, got, tt.want)
		}
	}
}

func TestCommitEvidenceImpliesEligibleResult(t *testing.T) {
	completion := evidenceResult("completion", "[main abcdef0] fix", 1)
	completion.Enrichment = map[string]any{"command_completion": true, "commands": []any{map[string]any{"cmd": "git commit -m fix", "exit_code": float64(0)}}}
	poll := evidenceCall("poll", "", 3)
	poll.ToolName = "write_stdin"
	poll.Text = `{"session_id":42}`
	cases := map[string][]transcript.Message{
		"printed":    {evidenceCall("a", "git commit -m fix", 0), evidenceResult("a", "[main abcdef0] fix", 1)},
		"decoded":    {evidenceCall("a", "git commit -m fix", 0), evidenceResult("a", `{"exit_code":0,"output":"[main \u0061\u0062\u0063\u0064\u0065\u0066\u0030] fix"}`, 1)},
		"quiet":      {evidenceCall("a", "git commit -qm fix", 0), evidenceResult("a", "", 1), evidenceCall("b", "git rev-parse HEAD", 2), evidenceResult("b", strings.Repeat("a", 40), 3)},
		"poll":       {evidenceCall("a", "git commit -m fix", 0), evidenceResult("a", "Chunk ID: synthetic\nProcess running with session ID 42\nOutput:\n", 1), poll, evidenceResult("poll", "[main abcdef0] fix", 4)},
		"completion": {completion},
	}
	for name, rows := range cases {
		t.Run(name, func(t *testing.T) {
			if got := ExtractCommitEvidence(rows); len(got) == 0 {
				t.Fatal("positive evidence control missing")
			}
			eligible := false
			for _, m := range rows {
				eligible = eligible || m.Kind == transcript.KindToolResult && MayContainCommitEvidence(m.Text)
			}
			if !eligible {
				t.Fatal("extractable evidence had no eligible retained result")
			}
		})
	}
}
