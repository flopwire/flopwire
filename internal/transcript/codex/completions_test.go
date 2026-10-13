package codex

import (
	"os"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

func TestCommandCompletionsPreserveExactEvidence(t *testing.T) {
	path := "testdata/command-completions.jsonl"
	c := parseFile(t, path, nil)
	msgs := upsert(c.Messages)
	byID := map[string]*transcript.Message{}
	count := 0
	for _, m := range msgs {
		byID[m.NativeID] = m
		if m.Enrichment["command_completion"] == true {
			count++
		}
	}
	if count != 3 {
		t.Fatalf("completion evidence count %d, want 3", count)
	}
	first := byID["completion-a"]
	if first == nil || first.Kind != transcript.KindToolResult || first.ToolCallID != "call-a" || first.Role != "tool" || first.ToolName != "shell" || first.Text != "[topic abc1234] first" || first.LineNo != 5 || first.IsError || first.Enrichment["completion_legacy"] != nil {
		t.Fatalf("first completion %+v", first)
	}
	cmds, _ := first.Enrichment["commands"].([]Command)
	if len(cmds) != 1 || cmds[0].Cmd != "git commit -m first" || cmds[0].Cwd != "/tmp/synthetic" || cmds[0].ExitCode == nil || *cmds[0].ExitCode != 0 {
		t.Fatalf("commands %+v", cmds)
	}
	delayed := byID["completion-delayed"]
	if delayed == nil || delayed.ToolCallID != "" || delayed.LineNo != 8 || !delayed.IsError || delayed.Text != "[topic def5678] delayed\nsubsequent command failed" || delayed.Enrichment["completion_legacy"] != true {
		t.Fatalf("delayed completion %+v", delayed)
	}
	empty := byID["completion-empty"]
	if empty == nil || empty.Text != "" || empty.Enrichment["command_completion"] != true || empty.Enrichment["completion_legacy"] != true {
		t.Fatalf("empty completion %+v", empty)
	}
	noOutput := byID["completion-no-output"]
	if noOutput == nil || noOutput.Text != "git status" || noOutput.Enrichment["command_completion"] != nil {
		t.Fatalf("legacy no-output %+v", noOutput)
	}
	for _, m := range msgs {
		if m.NativeID == "completion-a" || m.NativeID == "completion-delayed" || m.NativeID == "completion-empty" {
			continue
		}
		if m.Enrichment["command_completion"] != nil {
			t.Fatalf("inferred provenance on %+v", m)
		}
	}
}

func TestCommandCompletionResumeMatchesFullParse(t *testing.T) {
	path := "testdata/command-completions.jsonl"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	src := &transcript.Source{Agent: transcript.AgentCodex, Path: path}
	for i, b := range data {
		if b == '\n' {
			assertResumeMatches(t, src, data, []int64{int64(i + 1)})
		}
	}
}
