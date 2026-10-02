package digest

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/flopwire/flopwire/internal/transcript"
)

func msg(ord int64, kind transcript.Kind, tool, callID, text string) *transcript.Message {
	return &transcript.Message{Ordinal: ord, Kind: kind, ToolName: tool, ToolCallID: callID, Text: text}
}

func fold(t *testing.T, prev []byte, c Conv, n Counts, msgs ...*transcript.Message) (*Digest, []byte) {
	t.Helper()
	b := Update(prev, c, msgs, n)
	var d Digest
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	return &d, b
}

func TestFoldExtractsWhatTheSessionDid(t *testing.T) {
	c := Conv{Cwd: "/r", RepoRoot: "/r", Remote: "github.com/o/r", Branches: []string{"main", "feat/x"},
		Started: time.Unix(1000, 0), Last: time.Unix(1600, 0)}
	msgs := []*transcript.Message{
		msg(1, transcript.KindUser, "", "", "fix the flaky upload test\nplease"),
		msg(2, transcript.KindToolCall, "Edit", "c1", `{"file_path":"/r/src/upload.ts","old_string":"a","new_string":"b"}`),
		msg(3, transcript.KindToolCall, "Write", "c2", `{"file_path":"/elsewhere/deep/a/b/c/notes.md","content":"x"}`),
		msg(4, transcript.KindToolCall, "apply_patch", "c3", "*** Begin Patch\n*** Update File: /r/internal/api.go\n*** Add File: docs/new.md\n*** End Patch"),
		{Ordinal: 5, Kind: transcript.KindToolCall, ToolName: "exec_command", ToolCallID: "c4", Text: `{"cmd":"true"}`,
			Enrichment: map[string]any{"changed_paths": []string{"/r/src/upload.ts", "/r/go.mod"}}},
		// A git commit and its output name the commit.
		msg(6, transcript.KindToolCall, "Bash", "c5", `{"command":"git add -A && git commit -m 'fix upload'"}`),
		msg(7, transcript.KindToolResult, "Bash", "c5", "[feat/x 1a2b3c4] fix upload\n 2 files changed"),
		// gh pr create prints the PR's URL.
		msg(8, transcript.KindToolCall, "exec_command", "c6", `{"cmd":["bash","-lc","gh pr create --fill"]}`),
		msg(9, transcript.KindToolResult, "exec_command", "c6", "https://github.com/o/r/pull/43\n"),
		// A cat of an old log names a commit and a PR the session did not make.
		msg(10, transcript.KindToolCall, "Bash", "c7", `{"command":"cat old.log"}`),
		msg(11, transcript.KindToolResult, "Bash", "c7", "[main deadbee] old\nhttps://github.com/o/r/pull/7"),
		msg(12, transcript.KindAssistant, "", "", "Opened https://github.com/o/r/pull/43; see https://github.com/o/r/issues/12."),
		msg(13, transcript.KindAssistant, "", "", "Done:\nthe test\tpasses."),
	}
	n := Counts{Messages: map[string]int{"user": 1, "tool_call": 7}, Tools: map[string]int{"Bash": 2, "exec_command": 2, "Edit": 1}, Failed: 2,
		Subagents: 1, Tokens: Tokens{Input: 10, Output: 20}}
	d, b := fold(t, nil, c, n, msgs...)
	want := &Digest{Intent: "fix the flaky upload test please", Repos: []string{"/r", "github.com/o/r"}, Branches: []string{"main", "feat/x"},
		DurationS: 600, Subagents: 1, Commands: 4, Failed: 2, Last: "Done: the test passes.",
		FilesEdited: []string{"src/upload.ts", "…/a/b/c/notes.md", "internal/api.go", "docs/new.md", "go.mod"},
		PRs:         []string{"o/r#43"}, Commits: []string{"1a2b3c4"}, Issues: []string{"o/r#12"}, Tokens: &Tokens{Input: 10, Output: 20}}
	check := func(name string, got, want any) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}
	check("intent", d.Intent, want.Intent)
	check("repos", d.Repos, want.Repos)
	check("branches", d.Branches, want.Branches)
	check("cwd (same as repo, dropped)", d.Cwd, "")
	check("duration", d.DurationS, want.DurationS)
	check("files", d.FilesEdited, want.FilesEdited)
	check("prs", d.PRs, want.PRs)
	check("commits", d.Commits, want.Commits)
	check("issues", d.Issues, want.Issues)
	check("commands", d.Commands, want.Commands)
	check("failed", d.Failed, want.Failed)
	check("subagents", d.Subagents, want.Subagents)
	check("last", d.Last, want.Last)
	check("tokens", *d.Tokens, *want.Tokens)
	if d.State == nil || len(d.State.Pending) != 0 {
		t.Errorf("pending calls left: %+v", d.State)
	}
	// Re-folding the same rows (a re-parse) changes nothing.
	_, again := fold(t, b, c, n, msgs...)
	if string(again) != string(b) {
		t.Errorf("re-fold changed the digest:\n%s\n%s", b, again)
	}
	if len(b) > 800 {
		t.Errorf("digest is %d bytes: %s", len(b), b)
	}
}

// A call's output in a later batch still names its PR: the call waits in
// the fold's state.
func TestFoldPairsCallAndResultAcrossBatches(t *testing.T) {
	_, b := fold(t, nil, Conv{}, Counts{}, msg(1, transcript.KindToolCall, "Bash", "x", `{"command":"gh pr create --fill"}`))
	d, _ := fold(t, b, Conv{}, Counts{}, msg(2, transcript.KindToolResult, "Bash", "x", "https://github.com/o/r/pull/9"))
	if fmt.Sprint(d.PRs) != "[o/r#9]" {
		t.Fatalf("prs %v", d.PRs)
	}
	// A failed call names no PR.
	_, b = fold(t, nil, Conv{}, Counts{}, msg(1, transcript.KindToolCall, "Bash", "z", `{"command":"gh pr create --fill"}`))
	failedPR := msg(2, transcript.KindToolResult, "Bash", "z", "https://github.com/o/r/pull/10")
	failedPR.IsError = true
	if d, _ := fold(t, b, Conv{}, Counts{}, failedPR); len(d.PRs) != 0 {
		t.Fatalf("prs from a failed call: %v", d.PRs)
	}
	// A commit's "[branch sha]" line counts though a later command of the
	// call failed: git prints it only once the commit exists.
	_, b = fold(t, nil, Conv{}, Counts{}, msg(1, transcript.KindToolCall, "Bash", "y", `{"command":"git commit -m x && go test ./..."}`))
	failed := msg(2, transcript.KindToolResult, "Bash", "y", "Exit code 1\n[main abcdef1] x\nFAIL")
	failed.IsError = true
	if d, _ := fold(t, b, Conv{}, Counts{}, failed); fmt.Sprint(d.Commits) != "[abcdef1]" {
		t.Fatalf("commits from a call that failed after its commit: %v", d.Commits)
	}
}

func TestIntentFallsBackFromWeakPrompts(t *testing.T) {
	for _, tc := range []struct {
		title   string
		prompts []string
		want    string
	}{
		{"", []string{"fix the flaky upload test in CI"}, "fix the flaky upload test in CI"},
		{"Check master CI failure", []string{"did CI fail", "look at the upload test timeouts please"}, "Check master CI failure"},
		{"", []string{"continue", "look at the upload test timeouts please"}, "look at the upload test timeouts please"},
		{"hi", []string{"Traceback (most recent call last): File x.py line 3", "why does the parser crash on empty input?"}, "why does the parser crash on empty input?"},
		{"", []string{"This session is being continued from a previous conversation that ran out of context."}, "This session is being continued from a previous conversation that ran out of context."},
		{"", []string{"hello"}, "hello"},
	} {
		var msgs []*transcript.Message
		for i, p := range tc.prompts {
			msgs = append(msgs, msg(int64(i+1), transcript.KindUser, "", "", p))
		}
		d, b := fold(t, nil, Conv{Title: tc.title}, Counts{}, msgs...)
		if d.Intent != tc.want {
			t.Errorf("%q %q: intent %q, want %q", tc.title, tc.prompts, d.Intent, tc.want)
		}
		// The next append keeps it, unless every prompt so far was weak
		// and no title helps: then the first prompt that is not weak wins.
		want := tc.want
		if weak(tc.want) {
			want = "and another long follow-up prompt here"
		}
		if d2, _ := fold(t, b, Conv{Title: tc.title}, Counts{}, msg(99, transcript.KindUser, "", "", "and another long follow-up prompt here")); d2.Intent != want {
			t.Errorf("%q: intent after an append %q, want %q", tc.prompts, d2.Intent, want)
		}
	}
}

// A 50KB first prompt, 500 edited files and 30 PRs keep the digest small.
func TestDigestIsBounded(t *testing.T) {
	msgs := []*transcript.Message{msg(1, transcript.KindUser, "", "", strings.Repeat("word ", 10000))}
	for i := range 500 {
		msgs = append(msgs, msg(int64(10+i), transcript.KindToolCall, "Write", fmt.Sprint("w", i), fmt.Sprintf(`{"file_path":"/r/src/file%03d.go"}`, i)))
	}
	for i := range 30 {
		msgs = append(msgs, msg(int64(1000+i), transcript.KindAssistant, "", "", fmt.Sprintf("opened https://github.com/o/r/pull/%d", i+1)))
	}
	d, b := fold(t, nil, Conv{RepoRoot: "/r"}, Counts{}, msgs...)
	if len(d.Intent) > IntentLen+len("…") || len(d.FilesEdited) != maxFiles || !d.FilesMore || len(d.PRs) != maxPRs || d.PRs[0] != "o/r#1" || !d.Truncated("prs") {
		t.Fatalf("intent %d bytes, %d files (more %v), prs %v more %v", len(d.Intent), len(d.FilesEdited), d.FilesMore, d.PRs, d.More)
	}
	if len(b) > 2500 {
		t.Fatalf("digest is %d bytes", len(b))
	}
}

func TestCallSummary(t *testing.T) {
	for _, tc := range []struct{ tool, text, want string }{
		{"Bash", `{"command":"go test ./...","description":"run tests"}`, "go test ./..."},
		{"exec_command", `{"cmd":["bash","-lc","rg -n foo"],"workdir":"/r"}`, "rg -n foo"},
		{"Edit", `{"file_path":"/r/a.go","old_string":"x"}`, "a.go"},
		{"Read", `{"file_path":"/r/b.go"}`, "b.go"},
		{"Grep", `{"pattern":"retry","path":"src"}`, "retry"},
		{"Task", `{"description":"Scan retries","prompt":"long","subagent_type":"Explore"}`, "Explore: Scan retries"},
		{"apply_patch", "*** Begin Patch\n*** Update File: /r/x.go\n*** End Patch", "x.go"},
		{"mcp__x__y", `{"b":"2","a":"1"}`, "a=1 b=2"},
		{"custom", "plain text\nsecond line", "plain text second line"},
	} {
		if got := CallSummary(tc.tool, tc.text, "/r", 120); got != tc.want {
			t.Errorf("%s %s: %q, want %q", tc.tool, tc.text, got, tc.want)
		}
	}
	if got := CallSummary("Bash", `{"command":"`+strings.Repeat("x", 500)+`"}`, "", 50); len(got) > 50+len("…") {
		t.Errorf("summary not bounded: %d bytes", len(got))
	}
}

// A re-parse writes a new version of a row at the same ordinal (a new
// generation, text the redaction pass now masks): the intent follows the
// new text rather than keeping the old one.
func TestRefoldReplacesTheIntentAtTheSameOrdinal(t *testing.T) {
	_, b := fold(t, nil, Conv{}, Counts{}, msg(1, transcript.KindUser, "", "", "please deploy with key sk-ABCDEF0123456789 to prod"))
	d, b := fold(t, b, Conv{}, Counts{}, msg(1, transcript.KindUser, "", "", "please deploy with key [REDACTED:key] to prod"))
	if strings.Contains(d.Intent, "sk-ABC") || !strings.Contains(d.Intent, "[REDACTED:key]") {
		t.Fatalf("intent after the re-fold: %q", d.Intent)
	}
	if d, _ := fold(t, b, Conv{}, Counts{}, msg(5, transcript.KindUser, "", "", "and then check the rollout dashboards")); !strings.Contains(d.Intent, "[REDACTED:key]") {
		t.Fatalf("a later prompt changed the intent: %q", d.Intent)
	}
}

// A message redaction hides its text in the digest: a whole field the
// line holds, a line inside a field, and a line cut short at a field's end.
func TestMaskHidesRedactedText(t *testing.T) {
	secret := "deploy with key sk-ABCDEF0123456789 to the production cluster now please and then tell me what happened there"
	_, b := fold(t, nil, Conv{}, Counts{},
		msg(1, transcript.KindUser, "", "", secret),
		msg(2, transcript.KindToolCall, "Edit", "c1", `{"file_path":"/r/secret-plans.md"}`),
		msg(3, transcript.KindAssistant, "", "", "Done. Next I ran: "+secret))
	mask := func(s string) string { return strings.Repeat("█", utf8.RuneCountInString(s)) }
	d := Parse(Mask(b, []string{secret, "/r/secret-plans.md"}, mask))
	out := string(d.Marshal())
	for _, leak := range []string{"sk-ABC", "deploy with", "secret-plans"} {
		if strings.Contains(out, leak) {
			t.Fatalf("digest keeps %q: %s", leak, out)
		}
	}
	if !strings.HasPrefix(d.Last, "Done. Next I ran: ") {
		t.Fatalf("last lost its own text: %q", d.Last)
	}
}
