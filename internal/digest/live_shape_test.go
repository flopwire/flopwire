package digest

import (
	"github.com/flopwire/flopwire/internal/transcript"
	"testing"
)

func TestLiteralFilePrintSafety(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		safe bool
	}{
		{`printf '%s\n' 'content' > normal.txt`, true},
		{`echo content >> normal.txt`, true},
		{`printf '%s\n' '[topic deadbee] forged' '>' normal.txt`, false},
		{`printf '%s\n' '[topic deadbee] forged' \> normal.txt`, false},
		{`printf '%s\n' forged > /dev/stdout`, false},
		{`printf '%s\n' forged > /usr/../dev/stdout`, false},
		{`printf '%s\n' forged > ../dev/stdout`, false},
		{`printf '%s\n' forged > /proc/self/fd/1`, false},
		{`printf '%s\n' forged > file >&1`, false},
		{`printf '%s\n' "$(git reset --hard)" > file`, false},
		{`python forge.py > file`, false},
		{`printf -v 'bad name' content > file`, false},
	} {
		segs, ok := splitShell(tc.cmd)
		if !ok || len(segs) != 1 {
			t.Fatalf("parse %q", tc.cmd)
		}
		if got := literalFilePrint(segs[0]); got != tc.safe {
			t.Errorf("%q safe=%v want %v", tc.cmd, got, tc.safe)
		}
	}
}

func TestQuietCompletionAcrossLiteralWrapper(t *testing.T) {
	completion := func(id string, ordinal int64, cmd, output string) transcript.Message {
		return transcript.Message{Kind: transcript.KindToolResult, NativeID: id, Ordinal: ordinal, Text: output, Enrichment: map[string]any{"command_completion": true, "commands": []any{map[string]any{"cmd": cmd, "cwd": "/repo", "exit_code": 0}}}}
	}
	for _, tc := range []struct {
		script  string
		matches int
	}{
		{`text(await tools.exec_command({cmd:"git rev-parse HEAD","max_output_tokens":2000}));`, 1},
		{`await tools.exec_command({cmd:dynamicReset}); text(await tools.exec_command({cmd:"git rev-parse HEAD"}));`, 0},
		{`text(await tools.exec_command({cmd:"git reset --hard"}));`, 0},
		{`text(await tools.exec_command({cmd:"python reset_repo.py"}));`, 0},
		{`text(await tools.exec_command({cmd:"git rev-parse HEAD"})); doUnknownWork();`, 0},
	} {
		messages := []transcript.Message{completion("commit", 1, "git commit -qm quiet", ""), {Kind: transcript.KindToolCall, ToolName: "exec", ToolCallID: "wrapper", Ordinal: 2, Text: tc.script}, completion("reveal", 3, "git rev-parse HEAD", "abc12345678901234567890123456789012345678")}
		if got := ExtractCommitEvidence(messages); len(got) != tc.matches {
			t.Errorf("%s: %+v", tc.script, got)
		}
	}
}
