package digest

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

// The fixtures below are synthetic. Each harness writes a shell call and
// its result in its own shape:
//
//   - Claude: tool Bash, arguments {"command": ...}; the result is the
//     command's output, is_error on a nonzero exit.
//   - Codex (legacy shell): arguments {"cmd": [...], "workdir": ...}; the
//     result is the output, is_error from the exit code.
//   - Codex (unified exec_command): arguments {"cmd": "...", ...}; the
//     result is a header with "Process exited with code N", then the
//     output, never is_error.
//   - Codex (exec script): tool exec runs a script; the commands it ran
//     and their exit codes are in the call's enrichment, and the result
//     holds one JSON chunk per command ({"exit_code":N,"output":"..."}),
//     never is_error.
//   - Devin: tool exec, arguments {"command": ...}; the command, cwd and
//     exit code are in the call's enrichment; the result is "Output from
//     command in shell ID:" and the output, then "Exit code: N".

var t0 = time.Date(2026, 10, 2, 0, 36, 24, 0, time.UTC)

type shape struct {
	name   string
	call   func(ord int64, id, cmd string, exit int) *transcript.Message
	result func(ord int64, id, out string, exit int) *transcript.Message
}

func at(m *transcript.Message, ord int64) *transcript.Message {
	m.TS = t0.Add(time.Duration(ord) * time.Second)
	return m
}

var shapes = []shape{
	{"claude",
		func(ord int64, id, cmd string, exit int) *transcript.Message {
			b, _ := json.Marshal(map[string]string{"command": cmd, "description": "x"})
			return at(msg(ord, transcript.KindToolCall, "Bash", id, string(b)), ord)
		},
		func(ord int64, id, out string, exit int) *transcript.Message {
			m := msg(ord, transcript.KindToolResult, "Bash", id, out)
			if exit != 0 {
				m.Text, m.IsError = fmt.Sprintf("Exit code %d\n%s", exit, out), true
			}
			return at(m, ord)
		}},
	{"codex-exec-command",
		func(ord int64, id, cmd string, exit int) *transcript.Message {
			b, _ := json.Marshal(map[string]any{"cmd": []string{"bash", "-lc", cmd}, "workdir": "/r"})
			return at(msg(ord, transcript.KindToolCall, "exec_command", id, string(b)), ord)
		},
		func(ord int64, id, out string, exit int) *transcript.Message {
			m := msg(ord, transcript.KindToolResult, "exec_command", id, out)
			m.IsError = exit != 0
			return at(m, ord)
		}},
	// Codex's unified exec_command as rollouts record it: cmd a string,
	// the exit code only in the output's header, never is_error.
	{"codex-unified-exec",
		func(ord int64, id, cmd string, exit int) *transcript.Message {
			b, _ := json.Marshal(map[string]any{"cmd": cmd, "workdir": "/r", "yield_time_ms": 10000, "max_output_tokens": 6000})
			return at(msg(ord, transcript.KindToolCall, "exec_command", id, string(b)), ord)
		},
		func(ord int64, id, out string, exit int) *transcript.Message {
			text := fmt.Sprintf("Chunk ID: a1b2c3\nWall time: 0.0412 seconds\nProcess exited with code %d\nOriginal token count: 12\nOutput:\n%s", exit, out)
			return at(msg(ord, transcript.KindToolResult, "exec_command", id, text), ord)
		}},
	{"codex-exec-script",
		func(ord int64, id, cmd string, exit int) *transcript.Message {
			q, _ := json.Marshal(cmd)
			m := msg(ord, transcript.KindToolCall, "exec", id, "text(await tools.exec_command({cmd:"+string(q)+"}));\n")
			m.Enrichment = map[string]any{"commands": []map[string]any{{"cmd": cmd, "cwd": "/r", "exit_code": exit}}}
			m.IsError = exit != 0
			return at(m, ord)
		},
		func(ord int64, id, out string, exit int) *transcript.Message {
			chunk, _ := json.Marshal(map[string]any{"chunk_id": "a1b2c3", "wall_time_seconds": 0.04, "exit_code": exit, "original_token_count": 12, "output": out})
			return at(msg(ord, transcript.KindToolResult, "exec", id, "Script completed\nWall time 0.1 seconds\nOutput:\n"+string(chunk)), ord)
		}},
	{"devin",
		func(ord int64, id, cmd string, exit int) *transcript.Message {
			b, _ := json.Marshal(map[string]string{"command": cmd})
			m := msg(ord, transcript.KindToolCall, "exec", id, string(b))
			m.Enrichment = map[string]any{"commands": []map[string]any{{"cmd": cmd, "cwd": "/r", "exit_code": int64(exit)}}}
			m.IsError = exit != 0
			return at(m, ord)
		},
		func(ord int64, id, out string, exit int) *transcript.Message {
			m := msg(ord, transcript.KindToolResult, "exec", id, fmt.Sprintf("Output from command in shell 2812b5:\n%s\n\nExit code: %d", out, exit))
			m.IsError = exit != 0
			return at(m, ord)
		}},
}

// step is one shell call and its result.
type step struct {
	cmd, out string
	exit     int
}

var repoConv = Conv{Cwd: "/r", RepoRoot: "/r", Branches: []string{"api-cursors"}}

// run folds steps, one call and result each, in one batch per step (the
// result of each in the next batch), in every harness shape.
func runSteps(t *testing.T, sh shape, c Conv, steps ...step) *Digest {
	t.Helper()
	var b []byte
	for i, s := range steps {
		id := fmt.Sprintf("call%d", i)
		ord := int64(10 * (i + 1))
		b = Fold(b, c, []*transcript.Message{sh.call(ord, id, s.cmd, s.exit)})
		b = Fold(b, c, []*transcript.Message{sh.result(ord+1, id, s.out, s.exit)})
	}
	return Parse(b)
}

func subjects(cs []Commit) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Subject)
	}
	return out
}

// Issue #80: git commit -q prints no "[branch sha]" line. The commit is
// still recorded, without a sha, with its subject, branch and time.
func TestQuietCommitIsRecordedWithoutSHA(t *testing.T) {
	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			d := runSteps(t, sh, repoConv, step{cmd: `git add src/api/users.ts && git commit -q -m "Switch GET /users to cursor pagination"`})
			if len(d.Commits) != 0 || len(d.CommitsNoSHA) != 1 {
				t.Fatalf("commits %v, without sha %+v", d.Commits, d.CommitsNoSHA)
			}
			c := d.CommitsNoSHA[0]
			if c.Subject != "Switch GET /users to cursor pagination" || c.Branch != "api-cursors" || c.At == nil || !c.At.Equal(t0.Add(11*time.Second)) {
				t.Fatalf("commit without sha: %+v", c)
			}
			if strings.Contains(string(d.Marshal()), `"sha"`) {
				t.Fatalf("a sha was made up: %s", d.Marshal())
			}
		})
	}
}

// commitCase is one way a commit shows in a transcript, and what the
// digest must take from it.
type commitCase struct {
	name   string
	steps  []step
	commit []string // shas
	noSHA  []string // subjects of commits without a sha
}

const (
	sha1 = "650a939"
	full = "650a93912c4be5f0d61e7a1b9a3c2d4e5f6a7b8c"
	old  = "1f2e3d4"
)

var commitCases = []commitCase{
	{"env -u X git commit", []step{{cmd: `env -u TMUX GIT_EDITOR=true git commit -m x`, out: "[api-cursors 650a939] x\n"}}, []string{sha1}, nil},
	{"commit prints its sha", []step{{cmd: `git commit -am "fix upload"`, out: "[api-cursors 650a939] fix upload\n 2 files changed, 3 insertions(+)"}},
		[]string{sha1}, nil},
	{"root commit", []step{{cmd: `git commit -m init`, out: "[main (root-commit) 650a939] init\n 1 file changed"}}, []string{sha1}, nil},
	{"detached HEAD", []step{{cmd: `git commit -m wip`, out: "[detached HEAD 650a939] wip\n 1 file changed"}}, []string{sha1}, nil},
	{"commit -q", []step{{cmd: `git commit -q -m "Switch GET /users to cursor pagination"`}}, nil, []string{"Switch GET /users to cursor pagination"}},
	{"commit --quiet", []step{{cmd: `git commit --quiet --message="Add cursor"`}}, nil, []string{"Add cursor"}},
	{"short option cluster", []step{{cmd: `git commit -qam 'Add cursor'`}}, nil, []string{"Add cursor"}},
	{"output with no sha line (hooks only)", []step{{cmd: `git commit -m "Add cursor"`, out: "trim trailing whitespace.................Passed\n"}},
		nil, []string{"Add cursor"}},
	{"message from a cat here-document", []step{{cmd: "git commit -q -m \"$(cat <<'EOF'\nAdd cursor (users)\n\nThe body says \"why\".\nEOF\n)\""}},
		nil, []string{"Add cursor (users)"}},
	{"message from stdin here-document", []step{{cmd: "git commit -q -F - <<'EOF'\nAdd cursor\n\nbody\nEOF"}}, nil, []string{"Add cursor"}},
	{"message from a file", []step{{cmd: `git commit -q -F msg.txt`}}, nil, []string{""}},
	{"amend -q", []step{{cmd: `git commit -q --amend --no-edit`}}, nil, []string{""}},

	{"-q then git log --oneline -1", []step{{cmd: `git commit -q -m "Add cursor"`}, {cmd: `git log --oneline -1`, out: "650a939 (HEAD -> api-cursors) Add cursor\n"}},
		[]string{sha1}, nil},
	{"-q && git log --oneline -1 in one call", []step{{cmd: `git add src/api/users.ts && git commit -q -m "Add cursor" && git log --oneline -1`,
		out: "650a939 Add cursor\n"}}, []string{sha1}, nil},
	{"-q then git rev-parse HEAD", []step{{cmd: `git commit -q -m x`}, {cmd: `git rev-parse HEAD`, out: full + "\n"}}, []string{full}, nil},
	{"-q then git rev-parse --short HEAD", []step{{cmd: `git commit -q -m x`}, {cmd: `git rev-parse --short HEAD`, out: sha1 + "\n"}}, []string{sha1}, nil},
	{"-q then git log -1", []step{{cmd: `git commit -q -m x`}, {cmd: `git log -1`, out: "commit " + full + "\nAuthor: A <a@example.test>\n\n    x\n"}},
		[]string{full}, nil},
	{"-q then git log -1 --format=%H", []step{{cmd: `git commit -q -m x`}, {cmd: `git log -1 --format=%H`, out: full + "\n"}}, []string{full}, nil},
	{"-q then git show --stat", []step{{cmd: `git commit -q -m x`}, {cmd: `git show --stat`, out: "commit " + full + "\nAuthor: A\n\n    x\n\n a.go | 2 +-\n"}},
		[]string{full}, nil},
	{"-q then git log --oneline -5 alone: the first entry", []step{{cmd: `git commit -q -m x`},
		{cmd: `git log --oneline -5`, out: sha1 + " x\n" + old + " older\n"}}, []string{sha1}, nil},
	{"-q then git push", []step{{cmd: `git commit -q -m x`}, {cmd: `git push`, out: "To github.com:o/r.git\n   1f2e3d4..650a939  api-cursors -> api-cursors\n"}},
		[]string{sha1}, nil},
	{"-q && git push && gh pr create", []step{{cmd: `git commit -q -m x && git push -u origin HEAD && gh pr create --fill`,
		out: "To github.com:o/r.git\n   1f2e3d4..650a939  HEAD -> api-cursors\nhttps://github.com/o/r/pull/43\n"}}, []string{sha1}, nil},
	{"cd into the repo, then -q, then log", []step{{cmd: `cd /r && git commit -q -m x`}, {cmd: `git log --oneline -1`, out: sha1 + " x\n"}},
		[]string{sha1}, nil},
	{"cherry-pick", []step{{cmd: `git cherry-pick 1f2e3d4`, out: "[api-cursors 650a939] x\n Date: ...\n"}}, []string{sha1}, nil},
	{"revert", []step{{cmd: `git revert --no-edit HEAD`, out: "[api-cursors 650a939] Revert \"x\"\n"}}, []string{sha1}, nil},

	// Not detected, by decision.
	{"merge prints no sha", []step{{cmd: `git merge --no-ff feat`, out: "Merge made by the 'ort' strategy.\n a.go | 2 +-\n"}}, nil, nil},
	{"fast-forward merge makes no commit", []step{{cmd: `git merge feat`, out: "Updating 1f2e3d4..650a939\nFast-forward\n"}}, nil, nil},
	{"rebase", []step{{cmd: `git rebase main`, out: "Successfully rebased and updated refs/heads/api-cursors.\n"}}, nil, nil},
	{"am", []step{{cmd: `git am 0001-x.patch`, out: "Applying: x\n"}}, nil, nil},
	{"gh pr merge", []step{{cmd: `gh pr merge 43 --squash`, out: "✓ Squashed and merged pull request o/r#43 (x)\n"}}, nil, nil},
	{"a script that commits", []step{{cmd: `./scripts/release.sh`, out: "released\n"}}, nil, nil},
	{"-q; then log (the commit's success is unknown)", []step{{cmd: `git commit -q -m x; git log --oneline -1`, out: old + " older\n"}}, nil, nil},
	{"-q | tail (the pipe hides its exit)", []step{{cmd: `git commit -q -m x 2>&1 | tail -3`}}, nil, nil},

	// False positives kept out.
	{"failed commit: hook rejected", []step{{cmd: `git commit -q -m x`, out: "lint failed", exit: 1}}, nil, nil},
	{"failed commit: nothing to commit", []step{{cmd: `git commit -m x`, out: "On branch api-cursors\nnothing to commit, working tree clean", exit: 1}}, nil, nil},
	{"failed commit, then log shows the old HEAD", []step{{cmd: `git commit -q -m x`, out: "nothing to commit", exit: 1},
		{cmd: `git log --oneline -1`, out: old + " older\n"}}, nil, nil},
	{"git log of old commits, no commit made", []step{{cmd: `git log --oneline -3`, out: old + " older\n"}}, nil, nil},
	{"printed commit, then log of older commits", []step{{cmd: `git commit -m x`, out: "[api-cursors 650a939] x\n"},
		{cmd: `git log --oneline -3`, out: sha1 + " x\n" + old + " older\n"}}, []string{sha1}, nil},
	{"a cat of an old commit line", []step{{cmd: `cat old.log`, out: "[main 1f2e3d4] old\n"}}, nil, nil},
	{"-q then a log with a path filter", []step{{cmd: `git commit -q -m x`}, {cmd: `git log -1 --oneline -- src`, out: old + " older\n"}},
		nil, []string{"x"}},
	{"-q then a log of another branch", []step{{cmd: `git commit -q -m x`}, {cmd: `git log --oneline -1 main`, out: old + " older\n"}},
		nil, []string{"x"}},
	{"-q then a log whose format shows no sha", []step{{cmd: `git commit -q -m x`}, {cmd: `git log -1 --format=%s`, out: "1f2e3d4 subject\n"}},
		nil, []string{"x"}},
	{"-q then log in a chain that does not end with it", []step{{cmd: `git commit -q -m x`}, {cmd: `git log --oneline -1 && git status`, out: sha1 + " x\nclean\n"}},
		nil, []string{"x"}},
	{"-q then a push of another branch", []step{{cmd: `git commit -q -m x`}, {cmd: `git push origin main`, out: "   0000001..1f2e3d4  main -> main\n"}},
		nil, []string{"x"}},
	{"-q then a push of a new branch", []step{{cmd: `git commit -q -m x`}, {cmd: `git push -u origin api-cursors`, out: " * [new branch]      api-cursors -> api-cursors\n"}},
		nil, []string{"x"}},
	{"-q, checkout, then log: HEAD moved", []step{{cmd: `git commit -q -m x`}, {cmd: `git checkout main`, out: "Switched to branch 'main'\n"},
		{cmd: `git log --oneline -1`, out: old + " older\n"}}, nil, []string{"x"}},
	{"-q, rebase, then log: HEAD moved", []step{{cmd: `git commit -q -m x`}, {cmd: `git pull --rebase`},
		{cmd: `git rev-parse HEAD`, out: full + "\n"}}, nil, []string{"x"}},
	{"-q, a second -q, then log: only the second", []step{{cmd: `git commit -q -m one`}, {cmd: `git commit -q -m two`},
		{cmd: `git log --oneline -1`, out: sha1 + " two\n"}}, []string{sha1}, []string{"one"}},
	{"-q, then log in another directory", []step{{cmd: `git commit -q -m x`}, {cmd: `cd /r/vendor/lib && git log --oneline -1`, out: old + " lib\n"}},
		nil, []string{"x"}},
	// Outside the session's repo and cwd (a sibling worktree, another
	// repo): a sha names its commit wherever it was made, so it counts; a
	// commit without one does not (its branch would be the session's).
	{"commit in another directory prints its sha", []step{{cmd: `cd /elsewhere && git commit -m x`, out: "[main 1f2e3d4] x\n"}}, []string{old}, nil},
	{"git -C another repo commit -q", []step{{cmd: `git -C /elsewhere commit -q -m x`}}, nil, nil},
	{"-q in another directory, then log there", []step{{cmd: `cd /r-wt-x && git commit -q -m x`}, {cmd: `cd /r-wt-x && git log --oneline -1`, out: sha1 + " x\n"}},
		[]string{sha1}, nil},
	{"-q in another directory, then log in the session's", []step{{cmd: `cd /r-wt-x && git commit -q -m x`}, {cmd: `git log --oneline -1`, out: old + " older\n"}},
		nil, nil},
	// Claude's shell keeps the directory a cd leaves it in: the later log
	// runs there (a nested worktree, a submodule), not in the session's.
	{"-q, cd into a nested worktree, then log", []step{{cmd: `git commit -q -m x`}, {cmd: `cd .claude/worktrees/feat`},
		{cmd: `git log --oneline -1`, out: old + " other work\n"}}, nil, []string{"x"}},
	{"-q, cd somewhere && build, then log", []step{{cmd: `git commit -q -m x`}, {cmd: `cd vendor/lib && make`},
		{cmd: `git rev-parse HEAD`, out: full + "\n"}}, nil, []string{"x"}},
	// gh pr merge --delete-branch checks out the default branch and pulls.
	{"-q, gh pr merge -d, then log", []step{{cmd: `git commit -q -m x`}, {cmd: `gh pr merge 43 --squash --delete-branch`,
		out: "✓ Squashed and merged pull request o/r#43 (x)\n✓ Deleted local branch api-cursors and switched to branch main\n"},
		{cmd: `git log --oneline -1`, out: old + " someone's merge\n"}}, nil, []string{"x"}},
	{"dry run", []step{{cmd: `git commit --dry-run -m x`, out: "On branch api-cursors\nChanges to be committed:\n"}}, nil, nil},
}

func TestCommitCatalogue(t *testing.T) {
	for _, tc := range commitCases {
		for _, sh := range shapes {
			t.Run(tc.name+"/"+sh.name, func(t *testing.T) {
				d := runSteps(t, sh, repoConv, tc.steps...)
				if fmt.Sprint(d.Commits) != fmt.Sprint(tc.commit) {
					t.Errorf("commits %v, want %v", d.Commits, tc.commit)
				}
				if got := subjects(d.CommitsNoSHA); fmt.Sprint(got) != fmt.Sprint(tc.noSHA) {
					t.Errorf("commits without sha %q, want %q", got, tc.noSHA)
				}
			})
		}
	}
}

// A sha in a prompt or a reply is no evidence of a commit.
func TestCommitMentionedInProseDoesNotCount(t *testing.T) {
	d, _ := fold(t, nil, repoConv, Counts{},
		msg(1, transcript.KindUser, "", "", "who made [api-cursors 650a939] Switch GET /users? see 1f2e3d4"),
		msg(2, transcript.KindAssistant, "", "", "I committed 650a939 ([api-cursors 650a939] Add cursor) and pushed 1f2e3d4..650a939."),
		msg(3, transcript.KindToolCall, "Bash", "c1", `{"command":"git log --oneline -1"}`),
		msg(4, transcript.KindToolResult, "Bash", "c1", "650a939 Add cursor"))
	if len(d.Commits) != 0 || len(d.CommitsNoSHA) != 0 {
		t.Fatalf("commits %v, without sha %+v", d.Commits, d.CommitsNoSHA)
	}
}

// A Codex exec script runs the commit and the log as two commands, each
// with its exit code; the log shows the commit's sha.
func TestCodexScriptCommitThenLog(t *testing.T) {
	call := msg(10, transcript.KindToolCall, "exec", "s1", "await tools.exec_command({cmd:\"git commit -q -m x\"});\ntext(await tools.exec_command({cmd:\"git log --oneline -1\"}));\n")
	call.Enrichment = map[string]any{"commands": []map[string]any{{"cmd": "git commit -q -m x", "cwd": "/r", "exit_code": 0},
		{"cmd": "git log --oneline -1", "cwd": "/r", "exit_code": 0}}}
	res := msg(11, transcript.KindToolResult, "exec", "s1", "Script completed\nOutput:\n"+`{"chunk_id":"a","exit_code":0,"output":"650a939 x\n"}`)
	// The parser emits the call before its events arrive, then again enriched.
	bare := *call
	bare.Enrichment = nil
	d := Parse(Fold(nil, repoConv, []*transcript.Message{&bare, call, res}))
	if fmt.Sprint(d.Commits) != "[650a939]" || len(d.CommitsNoSHA) != 0 {
		t.Fatalf("commits %v, without sha %+v", d.Commits, d.CommitsNoSHA)
	}
	// A failed commit command fails the reveal too, though the script's
	// result is no error.
	call.Enrichment = map[string]any{"commands": []map[string]any{{"cmd": "git commit -q -m x", "cwd": "/r", "exit_code": 1},
		{"cmd": "git log --oneline -1", "cwd": "/r", "exit_code": 0}}}
	res.Text = "Script completed\nOutput:\n" + `{"chunk_id":"a","exit_code":1,"output":"nothing to commit\n"}` + "\n" + `{"chunk_id":"b","exit_code":0,"output":"1f2e3d4 old\n"}`
	if d := Parse(Fold(nil, repoConv, []*transcript.Message{call, res})); len(d.Commits) != 0 || len(d.CommitsNoSHA) != 0 {
		t.Fatalf("failed commit: commits %v, without sha %+v", d.Commits, d.CommitsNoSHA)
	}
}

// Re-folding every row (a re-parse) changes nothing: a commit a later log
// resolved is not recorded again without its sha.
func TestCommitRefoldIsStable(t *testing.T) {
	sh := shapes[0]
	var msgs []*transcript.Message
	for i, s := range []step{{cmd: `git commit -q -m one`}, {cmd: `git log --oneline -1`, out: sha1 + " one\n"}, {cmd: `git commit -q -m two`}} {
		id := fmt.Sprint("c", i)
		msgs = append(msgs, sh.call(int64(10*(i+1)), id, s.cmd, 0), sh.result(int64(10*(i+1)+1), id, s.out, 0))
	}
	b := Fold(nil, repoConv, msgs)
	again := Fold(b, repoConv, msgs)
	if string(again) != string(b) {
		t.Fatalf("re-fold changed the digest:\n%s\n%s", b, again)
	}
	d := Parse(again)
	if fmt.Sprint(d.Commits) != "[650a939]" || fmt.Sprint(subjects(d.CommitsNoSHA)) != "[two]" {
		t.Fatalf("commits %v, without sha %+v", d.Commits, d.CommitsNoSHA)
	}
}

// A digest an older fold wrote (no fold version, HEAD bookkeeping from the
// end of the session) is rebuilt by a re-parse that folds every row again.
func TestOlderFoldIsRebuiltByRefold(t *testing.T) {
	sh := shapes[0]
	msgs := []*transcript.Message{sh.call(10, "c1", `git commit -q -m x`, 0), sh.result(11, "c1", "", 0),
		sh.call(20, "c2", `git log --oneline -1`, 0), sh.result(21, "c2", sha1+" x\n", 0)}
	stale := []byte(`{"files_edited":["a.go"],"state":{"h":900}}`)
	d := Parse(Fold(stale, repoConv, msgs))
	if fmt.Sprint(d.Commits) != "[650a939]" || len(d.CommitsNoSHA) != 0 || fmt.Sprint(d.FilesEdited) != "[a.go]" {
		t.Fatalf("commits %v, without sha %+v, files %v", d.Commits, d.CommitsNoSHA, d.FilesEdited)
	}
}

// At most maxNoSHA commits without a sha are kept, and more says so.
func TestCommitsWithoutSHAAreBounded(t *testing.T) {
	var steps []step
	for i := range maxNoSHA + 3 {
		steps = append(steps, step{cmd: fmt.Sprintf(`git commit -q -m "commit %d %s"`, i, strings.Repeat("x", 200))})
	}
	d := runSteps(t, shapes[0], repoConv, steps...)
	if len(d.CommitsNoSHA) != maxNoSHA || !d.Truncated("commits_no_sha") || len(d.CommitsNoSHA[0].Subject) > noSHASubject+len("…") {
		t.Fatalf("%d commits without sha, more %v, subject %d bytes", len(d.CommitsNoSHA), d.More, len(d.CommitsNoSHA[0].Subject))
	}
}

// A redaction hides a subject.
func TestMaskHidesCommitSubject(t *testing.T) {
	d := runSteps(t, shapes[0], repoConv, step{cmd: `git commit -q -m "rotate key sk-ABCDEF0123456789"`})
	out := string(Mask(d.Marshal(), []string{`git commit -q -m "rotate key sk-ABCDEF0123456789"`, "rotate key sk-ABCDEF0123456789"},
		func(s string) string { return strings.Repeat("█", len(s)) }))
	if strings.Contains(out, "sk-ABC") {
		t.Fatalf("subject kept: %s", out)
	}
}

// A Codex exec script that commits and then runs a command that fails
// (rg with no match): the printed sha counts though the script's output
// shows a failure.
func TestCodexScriptPrintedCommitSurvivesALaterFailure(t *testing.T) {
	call := msg(10, transcript.KindToolCall, "exec", "s1", "script")
	call.Enrichment = map[string]any{"commands": []map[string]any{{"cmd": "git commit -m x", "cwd": "/r", "exit_code": 0},
		{"cmd": "rg -n TODO src", "cwd": "/r", "exit_code": 1}}}
	call.IsError = true
	res := msg(11, transcript.KindToolResult, "exec", "s1", "Script completed\nOutput:\n"+`{"chunk_id":"a","exit_code":0,"output":"[main 650a939] x\n 1 file changed\n"}`+
		"\n"+`{"chunk_id":"b","exit_code":1,"output":""}`)
	if d := Parse(Fold(nil, repoConv, []*transcript.Message{call, res})); fmt.Sprint(d.Commits) != "[650a939]" {
		t.Fatalf("commits %v", d.Commits)
	}
}

// A Codex chunk the text cap cut does not decode as JSON; its printed
// commit line still counts.
func TestCodexCutChunkKeepsItsCommitLine(t *testing.T) {
	call := msg(10, transcript.KindToolCall, "exec", "s1", "script")
	call.Enrichment = map[string]any{"commands": []map[string]any{{"cmd": "git commit -m x", "cwd": "/r", "exit_code": 0},
		{"cmd": "git status", "cwd": "/r", "exit_code": 0}}}
	res := msg(11, transcript.KindToolResult, "exec", "s1", "Script completed\nOutput:\n"+`{"chunk_id":"a","exit_code":0,"output":"[main 650a939] x\n 1 file changed\n`+
		"\n"+`{"chunk_id":"b","exit_code":0,"output":"clean\n"}`)
	if d := Parse(Fold(nil, repoConv, []*transcript.Message{call, res})); fmt.Sprint(d.Commits) != "[650a939]" {
		t.Fatalf("commits %v", d.Commits)
	}
}

// A Codex exec script whose command events are missing: the commands are
// read from the script's exec_command calls.
func TestCodexScriptWithoutEvents(t *testing.T) {
	call := msg(10, transcript.KindToolCall, "exec", "s1", `text(await tools.exec_command({cmd:"git add a.go && git commit -q -m \"Add cursor\" && git log --oneline -1","max_output_tokens":6000}));`+"\n")
	res := msg(11, transcript.KindToolResult, "exec", "s1", "Script completed\nOutput:\n"+`{"chunk_id":"a","exit_code":0,"output":"650a939 Add cursor\n"}`)
	if d := Parse(Fold(nil, repoConv, []*transcript.Message{call, res})); fmt.Sprint(d.Commits) != "[650a939]" {
		t.Fatalf("commits %v, without sha %+v", d.Commits, d.CommitsNoSHA)
	}
}

// Calls whose results never come do not block later ones: past
// maxPending, the oldest pending call gives way.
func TestPendingCallsDoNotBlockLaterOnes(t *testing.T) {
	var msgs []*transcript.Message
	// The calls left waiting have later ordinals than the one that gets
	// its result (Codex re-emits a call at its own, earlier ordinal).
	msgs = append(msgs, msg(1, transcript.KindToolCall, "Bash", "c", `{"command":"git commit -m y"}`))
	for i := range maxPending + 4 {
		msgs = append(msgs, msg(int64(100+i), transcript.KindToolCall, "Bash", fmt.Sprint("lost", i), `{"command":"git commit -m x"}`))
		if i == 2 {
			msgs = append(msgs, msg(2, transcript.KindToolResult, "Bash", "c", "[main 650a939] y\n"))
		}
	}
	d := Parse(Fold(nil, repoConv, msgs))
	if fmt.Sprint(d.Commits) != "[650a939]" || len(d.State.Pending) != maxPending {
		t.Fatalf("commits %v, %d pending", d.Commits, len(d.State.Pending))
	}
}

// A multi-entry git log after other commands of a Codex script: its first
// entry may not start the call's output, so it shows nothing.
func TestCodexScriptLogAfterOtherCommands(t *testing.T) {
	commit := msg(10, transcript.KindToolCall, "exec", "c1", "script")
	commit.Enrichment = map[string]any{"commands": []map[string]any{{"cmd": "git commit -q -m x", "cwd": "/r", "exit_code": 0}}}
	cres := msg(11, transcript.KindToolResult, "exec", "c1", "Script completed\nOutput:\n"+`{"chunk_id":"a","exit_code":0,"output":""}`)
	log := msg(20, transcript.KindToolCall, "exec", "c2", "script")
	log.Enrichment = map[string]any{"commands": []map[string]any{{"cmd": "git rev-parse HEAD~3", "cwd": "/r", "exit_code": 0},
		{"cmd": "git log --oneline -5", "cwd": "/r", "exit_code": 0}}}
	lres := msg(21, transcript.KindToolResult, "exec", "c2", "Script completed\nOutput:\n"+`{"chunk_id":"b","exit_code":0,"output":"1f2e3d4 old\n"}`+"\n"+
		`{"chunk_id":"c","exit_code":0,"output":"650a939 x\n1f2e3d4 old\n"}`)
	d := Parse(Fold(nil, repoConv, []*transcript.Message{commit, cres, log, lres}))
	if len(d.Commits) != 0 || fmt.Sprint(subjects(d.CommitsNoSHA)) != "[x]" {
		t.Fatalf("commits %v, without sha %+v", d.Commits, d.CommitsNoSHA)
	}
}

// Codex exec_command returns while a slow command (a commit whose hooks
// run long) is still running: its success is unknown, so a later log
// names nothing.
func TestCodexExecStillRunningIsNoCommit(t *testing.T) {
	sh := shapes[1]
	for _, s := range shapes {
		if s.name == "codex-unified-exec" {
			sh = s
		}
	}
	call := sh.call(10, "c1", `git commit -q -m x`, 0)
	res := at(msg(11, transcript.KindToolResult, "exec_command", "c1", "Chunk ID: a1b2c3\nWall time: 10.0 seconds\nProcess running with session ID 4242\nOriginal token count: 3\nOutput:\nlint…\n"), 11)
	log, lres := sh.call(20, "c2", `git log --oneline -1`, 0), sh.result(21, "c2", old+" older\n", 0)
	d := Parse(Fold(nil, repoConv, []*transcript.Message{call, res, log, lres}))
	if len(d.Commits) != 0 || len(d.CommitsNoSHA) != 0 {
		t.Fatalf("commits %v, without sha %+v", d.Commits, d.CommitsNoSHA)
	}
}

// With maxNoSHA commits still without a sha, a further commit's sha
// still resolves from a later log.
func TestCommitResolvesPastTheNoSHACap(t *testing.T) {
	var steps []step
	for i := range maxNoSHA {
		steps = append(steps, step{cmd: fmt.Sprintf(`git commit -q -m "c%d"`, i)}, step{cmd: `git checkout -`})
	}
	steps = append(steps, step{cmd: `git commit -q -m last`}, step{cmd: `git log --oneline -1`, out: sha1 + " last\n"})
	for _, sh := range shapes {
		if d := runSteps(t, sh, repoConv, steps...); fmt.Sprint(d.Commits) != "[650a939]" {
			t.Errorf("%s: commits %v, without sha %d", sh.name, d.Commits, len(d.CommitsNoSHA))
		}
	}
}

// Conv.Branches lists the branches first seen first, and holds the ones
// the parser has read so far: its last entry is not the branch a commit
// was made on (the session went back to main, or moved on to another
// branch later in the transcript). A commit without a sha names a branch
// only when the session had one, so the digest does not depend on where
// a batch ends.
func TestCommitWithoutSHANamesNoGuessedBranch(t *testing.T) {
	steps := []step{{cmd: `git commit -q -m x`}}
	early := runSteps(t, shapes[0], Conv{Cwd: "/r", RepoRoot: "/r", Branches: []string{"main", "api-cursors"}}, steps...)
	late := runSteps(t, shapes[0], Conv{Cwd: "/r", RepoRoot: "/r", Branches: []string{"main", "api-cursors", "stacked-b"}}, steps...)
	if len(early.CommitsNoSHA) != 1 || len(late.CommitsNoSHA) != 1 || early.CommitsNoSHA[0].Branch != late.CommitsNoSHA[0].Branch ||
		late.CommitsNoSHA[0].Branch == "stacked-b" {
		t.Fatalf("branch %+v, then %+v", early.CommitsNoSHA, late.CommitsNoSHA)
	}
	if one := runSteps(t, shapes[0], repoConv, steps...); one.CommitsNoSHA[0].Branch != "api-cursors" {
		t.Fatalf("one branch: %+v", one.CommitsNoSHA)
	}
}

// A re-parse after the fold changed re-folds every row onto the stored
// digest: commits only the older fold took (a cat of another transcript's
// "[main abc1234]" line) must not survive it.
func TestOlderFoldDropsItsCommits(t *testing.T) {
	sh := shapes[0]
	msgs := []*transcript.Message{sh.call(10, "c1", `git commit -q -m x`, 0), sh.result(11, "c1", "", 0),
		sh.call(20, "c2", `git log --oneline -1`, 0), sh.result(21, "c2", sha1+" x\n", 0)}
	stale := []byte(`{"commits":["deadbee"],"files_edited":["a.go"],"state":{"h":900}}`)
	d := Parse(Fold(stale, repoConv, msgs))
	if fmt.Sprint(d.Commits) != "[650a939]" || fmt.Sprint(d.FilesEdited) != "[a.go]" {
		t.Fatalf("commits %v, files %v", d.Commits, d.FilesEdited)
	}
}
