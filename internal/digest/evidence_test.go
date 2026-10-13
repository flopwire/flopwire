package digest

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

func evidenceCall(id, cmd string, ord int64) transcript.Message {
	b, _ := json.Marshal(map[string]any{"cmd": cmd, "workdir": "/repo"})
	return transcript.Message{NativeID: "call-" + id, ToolCallID: id, Kind: transcript.KindToolCall, ToolName: "exec_command", Text: string(b), Ordinal: ord, ByteOffset: ord * 100, Part: 1}
}
func evidenceResult(id, text string, ord int64) transcript.Message {
	return transcript.Message{NativeID: "result-" + id, ToolCallID: id, Kind: transcript.KindToolResult, Text: text, Ordinal: ord, ByteOffset: ord * 100, Part: 2}
}

func TestCommitEvidenceRepeatedBannerIsOneAddressedObservation(t *testing.T) {
	out := "[main abcdef0] fix\n[main abcdef0] fix\n[main 1234567] next"
	result := evidenceResult("a", out, 1)
	completion := result
	completion.Enrichment = map[string]any{"command_completion": true, "commands": []any{map[string]any{"cmd": "git commit -m fix", "cwd": "/repo", "exit_code": float64(0)}}}
	for _, rows := range [][]transcript.Message{{evidenceCall("a", "git commit -m fix", 0), result}, {completion}} {
		got := ExtractCommitEvidence(rows)
		if len(got) != 2 || got[0].Hash != "abcdef0" || got[1].Hash != "1234567" {
			t.Fatalf("repeated output lost distinct observations: %+v", got)
		}
	}
	rows := []transcript.Message{evidenceCall("a", "git commit -m fix", 0), evidenceResult("a", "[main abcdef0] fix", 1), evidenceCall("b", "git commit -m fix", 2), evidenceResult("b", "[main abcdef0] fix", 3)}
	if got := ExtractCommitEvidence(rows); len(got) != 2 {
		t.Fatalf("separate executions collapsed: %+v", got)
	}
}
func TestCommitEvidenceExactPairAndUncapped(t *testing.T) {
	var rows []transcript.Message
	for i := int64(0); i < 25; i++ {
		id := strings.Repeat("a", int(i)+1)
		rows = append(rows, evidenceCall(id, "git commit -m x", i*2), evidenceResult(id, "[main abcdef0] x", i*2+1))
	}
	got := ExtractCommitEvidence(rows)
	if len(got) != 25 {
		t.Fatalf("got %d", len(got))
	}
	if got[0].Command.NativeID != "call-a" || got[0].Result.Part != 2 || got[0].Operation != "commit" || !got[0].DirectoryKnown {
		t.Fatalf("%+v", got[0])
	}
	rows = []transcript.Message{evidenceCall("a", "git commit -m x", 0), evidenceResult("unrelated", "call-a [main abcdef0] x", 1)}
	if len(ExtractCommitEvidence(rows)) != 0 {
		t.Fatal("paired by text")
	}
}
func TestCommitEvidenceQuietAndRaces(t *testing.T) {
	tests := []struct {
		name string
		rows []transcript.Message
		want int
	}{
		{"quiet", []transcript.Message{evidenceCall("a", "git commit -qm x", 0), evidenceResult("a", "", 1), evidenceCall("b", "git rev-parse HEAD", 2), evidenceResult("b", "abcdef01234567890123456789012345678901234", 3)}, 1},
		{"result-after-move", []transcript.Message{evidenceCall("a", "git commit -qm x", 0), evidenceCall("m", "git reset --hard HEAD~1", 1), evidenceResult("a", "", 2), evidenceCall("b", "git rev-parse HEAD", 3), evidenceResult("b", "abcdef01234567890123456789012345678901234", 4)}, 0},
		{"reveal-before-quiet-result", []transcript.Message{evidenceCall("a", "git commit -qm x", 0), evidenceCall("b", "git rev-parse HEAD", 1), evidenceResult("a", "", 2), evidenceResult("b", "abcdef01234567890123456789012345678901234", 3)}, 0},
		{"move-after-reveal-call", []transcript.Message{evidenceCall("a", "git commit -qm x", 0), evidenceResult("a", "", 1), evidenceCall("b", "git rev-parse HEAD", 2), evidenceCall("m", "git reset --hard HEAD~1", 3), evidenceResult("b", "abcdef01234567890123456789012345678901234", 4)}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractCommitEvidence(tt.rows)
			if len(got) != tt.want {
				t.Fatalf("%+v", got)
			}
			if tt.want == 1 && got[0].RevealResult == nil {
				t.Fatal("missing reveal")
			}
		})
	}
}
func TestCommitEvidenceDelayedPoll(t *testing.T) {
	poll := evidenceCall("p", "", 2)
	poll.ToolName = "write_stdin"
	poll.Text = `{"session_id":73}`
	rows := []transcript.Message{evidenceCall("a", "git commit -m x", 0), evidenceResult("a", "Chunk ID: abc\nProcess running with session ID 73\nOutput:\n", 1), poll, evidenceResult("p", "Chunk ID: xyz\nProcess exited with code 0\nOutput:\n[main abcdef0] x", 3)}
	got := ExtractCommitEvidence(rows)
	if len(got) != 1 || got[0].Command.NativeID != "call-a" || got[0].Result.NativeID != "result-a" || got[0].RevealCommand == nil || got[0].RevealCommand.NativeID != "call-p" {
		t.Fatalf("%+v", got)
	}
	poll.Text = `{"session_id":74}`
	rows[2] = poll
	if len(ExtractCommitEvidence(rows)) != 0 {
		t.Fatal("wrong execution handle matched")
	}
}
func TestCommitEvidenceCompoundFailureAndSpoof(t *testing.T) {
	call := evidenceCall("a", "git commit -m x && git push", 0)
	r := evidenceResult("a", "[main abcdef0] x", 1)
	r.IsError = true
	got := ExtractCommitEvidence([]transcript.Message{call, r})
	if len(got) != 1 || got[0].Outcome != "failed" {
		t.Fatalf("%+v", got)
	}
	call = evidenceCall("a", "git commit -m x; echo '[main abcdef0] x'", 0)
	if len(ExtractCommitEvidence([]transcript.Message{call, r})) != 0 {
		t.Fatal("accepted extra program output")
	}
}
func TestCommitEvidenceCompletion(t *testing.T) {
	r := evidenceResult("", "[main "+strings.Repeat("a", 64)+"] x", 1)
	r.Enrichment = map[string]any{"command_completion": true, "commands": []any{map[string]any{"cmd": "git cherry-pick foo", "cwd": "/other", "exit_code": float64(0)}}}
	got := ExtractCommitEvidence([]transcript.Message{r})
	if len(got) != 1 || got[0].Operation != "cherry_pick" || len(got[0].Hash) != 64 || got[0].Command != got[0].Result {
		t.Fatalf("%+v", got)
	}
}

func TestCommitEvidenceRejectGitFormattedOutput(t *testing.T) {
	for _, cmd := range []string{"git commit -m x && git log -1 --format='[main deadbeef] x'", "git commit -m x && git show HEAD:file"} {
		call := evidenceCall("a", cmd, 0)
		result := evidenceResult("a", "[main deadbeef] x", 1)
		if got := ExtractCommitEvidence([]transcript.Message{call, result}); len(got) != 0 {
			t.Fatalf("%s: %+v", cmd, got)
		}
		result.Enrichment = map[string]any{"command_completion": true, "commands": []any{map[string]any{"cmd": cmd, "exit_code": float64(0)}}}
		if got := ExtractCommitEvidence([]transcript.Message{result}); len(got) != 0 {
			t.Fatalf("completion %s: %+v", cmd, got)
		}
	}
}

func TestCommitEvidenceQuietCompletion(t *testing.T) {
	r := evidenceResult("", "", 0)
	r.Enrichment = map[string]any{"command_completion": true, "commands": []any{map[string]any{"cmd": "git commit -qm x", "cwd": "/repo", "exit_code": float64(0)}}}
	rows := []transcript.Message{r, evidenceCall("rev", "git rev-parse HEAD", 1), evidenceResult("rev", "abcdef01234567890123456789012345678901234", 2)}
	got := ExtractCommitEvidence(rows)
	if len(got) != 1 || got[0].RevealResult == nil {
		t.Fatalf("%+v", got)
	}
}
func BenchmarkExtractCommitEvidence10K(b *testing.B) {
	rows := make([]transcript.Message, 0, 10000)
	for i := int64(0); i < 5000; i++ {
		id := fmt.Sprint(i)
		cmd := "git status --short"
		out := ""
		if i%20 == 0 {
			cmd = "git commit -m x"
			out = "[main abcdef0] x"
		}
		rows = append(rows, evidenceCall(id, cmd, i*2), evidenceResult(id, out, i*2+1))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := ExtractCommitEvidence(rows); len(got) != 250 {
			b.Fatal(len(got))
		}
	}
}

func TestCommitEvidenceCompletionSelfReveal(t *testing.T) {
	for _, tt := range []struct {
		cmd  string
		exit int
		want int
	}{
		{"git commit -qm x && git rev-parse HEAD", 0, 1},
		{"git commit -qm x; git rev-parse HEAD", 0, 0},
		{"git commit -qm x || git rev-parse HEAD", 0, 0},
		{"git commit -qm x && git rev-parse HEAD", 1, 0},
	} {
		r := evidenceResult("", "abcdef01234567890123456789012345678901234", 0)
		r.Enrichment = map[string]any{"command_completion": true, "commands": []any{map[string]any{"cmd": tt.cmd, "cwd": "/repo", "exit_code": float64(tt.exit)}}}
		got := ExtractCommitEvidence([]transcript.Message{r})
		if len(got) != tt.want {
			t.Fatalf("%s exit%d: %+v", tt.cmd, tt.exit, got)
		}
	}
}

func TestCommitEvidenceCompletionLaterReveal(t *testing.T) {
	completion := func(cmd, out string, ord int64) transcript.Message {
		r := evidenceResult("", out, ord)
		r.NativeID = fmt.Sprint(ord)
		r.Enrichment = map[string]any{"command_completion": true, "commands": []any{map[string]any{"cmd": cmd, "cwd": "/repo", "exit_code": float64(0)}}}
		return r
	}
	quiet := completion("git commit -qm x", "", 0)
	rev := completion("git rev-parse HEAD", "abcdef01234567890123456789012345678901234", 2)
	got := ExtractCommitEvidence([]transcript.Message{quiet, rev})
	if len(got) != 1 || got[0].RevealResult == nil || got[0].RevealResult.NativeID != "2" {
		t.Fatalf("%+v", got)
	}
	move := completion("git reset --hard HEAD~1", "", 1)
	if got := ExtractCommitEvidence([]transcript.Message{quiet, move, rev}); len(got) != 0 {
		t.Fatalf("HEAD moved: %+v", got)
	}
}

func TestCommitEvidencePlacementOverrides(t *testing.T) {
	for _, cmd := range []string{"env -C /other git commit -qm x", "git --git-dir=/other/.git commit -qm x", "GIT_DIR=/other/.git git commit -qm x", "git --work-tree /other commit -qm x"} {
		rows := []transcript.Message{evidenceCall("a", cmd, 0), evidenceResult("a", "", 1), evidenceCall("r", "git rev-parse HEAD", 2), evidenceResult("r", "abcdef01234567890123456789012345678901234", 3)}
		if got := ExtractCommitEvidence(rows); len(got) != 0 {
			t.Fatalf("%s quiet: %+v", cmd, got)
		}
		rows[1].Text = "[main abcdef0] x"
		got := ExtractCommitEvidence(rows[:2])
		if len(got) != 1 || got[0].DirectoryKnown || got[0].CommandDirectory != "" {
			t.Fatalf("%s printed: %+v", cmd, got)
		}
	}
}

func TestCompletionPreservesDigestSummary(t *testing.T) {
	call := evidenceCall("a", "git commit -m x", 0)
	completion := evidenceResult("a", "[main deadbee] completion", 1)
	completion.IsError = true
	completion.Enrichment = map[string]any{"command_completion": true, "commands": []any{map[string]any{"cmd": "git commit -m x", "exit_code": float64(1)}}}
	canonical := evidenceResult("a", "[main abcdef0] canonical", 2)
	c := Conv{Cwd: "/repo"}
	baseline := Append(nil, c, []*transcript.Message{&call, &canonical}, 0, 0)
	withCompletion := Append(nil, c, []*transcript.Message{&call, &completion, &canonical}, 0, 0)
	if string(baseline) != string(withCompletion) {
		t.Fatalf("summary changed: %s / %s", baseline, withCompletion)
	}
	var d Digest
	if err := json.Unmarshal(withCompletion, &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Commits) != 1 || d.Commits[0] != "abcdef0" || d.Messages["tool_result"] != 1 {
		t.Fatalf("%+v", d)
	}
}

func TestLegacyCompletionCountsRetained(t *testing.T) {
	r := evidenceResult("", "", 0)
	r.Enrichment = map[string]any{"command_completion": true, "completion_legacy": true}
	if IsEvidenceOnly(&r) {
		t.Fatal("legacy captured row excluded")
	}
	b := Append(nil, Conv{Cwd: "/repo"}, []*transcript.Message{&r}, 0, 0)
	var d Digest
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	if d.Messages["tool_result"] != 1 {
		t.Fatalf("legacy count: %+v", d.Messages)
	}
}
