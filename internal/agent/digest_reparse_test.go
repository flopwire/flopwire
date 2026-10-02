package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/digest"
	"github.com/flopwire/flopwire/internal/transcript/claude"
)

// quietCommitSession is a synthetic Claude session that commits with git
// commit -q and then shows the commit with git log --oneline -1.
const quietCommitID = "0b7e2c1a-0000-4000-8000-0000000000c1"

func quietCommitSession() string {
	line := func(n int, typ, content string) string {
		return fmt.Sprintf(`{"parentUuid":null,"isSidechain":false,"userType":"external","cwd":"/tmp/oracle-gamma","sessionId":"%s","version":"2.1.0","gitBranch":"api-cursors","type":"%s","message":%s,"uuid":"e1000000-0000-4000-8000-%012d","timestamp":"2026-10-02T00:36:%02d.000Z"}`+"\n",
			quietCommitID, typ, content, n, n)
	}
	use := func(id, cmd string) string {
		return fmt.Sprintf(`{"id":"msg_%s","type":"message","role":"assistant","model":"claude-opus-4-1","content":[{"type":"tool_use","id":"%s","name":"Bash","input":{"command":%q}}]}`, id, id, cmd)
	}
	res := func(id, out string) string {
		return fmt.Sprintf(`{"role":"user","content":[{"tool_use_id":"%s","type":"tool_result","content":%q,"is_error":false}]}`, id, out)
	}
	return line(1, "user", `{"role":"user","content":"switch GET /users to cursor pagination and commit it"}`) +
		line(2, "assistant", use("toolu_c1", `git add src/api/users.ts && git commit -q -m "Switch GET /users to cursor pagination"`)) +
		line(3, "user", res("toolu_c1", "")) +
		line(4, "assistant", use("toolu_c2", "git log --oneline -1")) +
		line(5, "user", res("toolu_c2", "650a939 Switch GET /users to cursor pagination\n"))
}

// Issue #80: digests stored before the commit fold changed are rebuilt.
// The parser majors were bumped with it, so every source indexed by the
// previous ones is re-parsed in the background, and the re-parse folds
// every row into the digest again.
func TestDigestRebuiltWhenParserMajorChanges(t *testing.T) {
	f := newFixture(t, "-")
	dir := f.path(".claude/projects/-tmp-oracle-gamma")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, quietCommitID+".jsonl")
	if err := os.WriteFile(file, []byte(quietCommitSession()), 0o644); err != nil {
		t.Fatal(err)
	}
	f.once()
	waitRacy()
	f.once()
	stored := func() *digest.Digest {
		t.Helper()
		var b string
		if err := f.store.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		if err := f.store.DB().QueryRow(`SELECT digest FROM conversations WHERE session_id = ?`, quietCommitID).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return digest.Parse([]byte(b))
	}
	if d := stored(); !slices.Contains(d.Commits, "650a939") {
		t.Fatalf("fresh index: commits %v, without sha %+v", d.Commits, d.CommitsNoSHA)
	}
	// As the previous release left it: a digest whose fold saw no commit,
	// stamped with the previous parser major.
	f.exec(`UPDATE conversations SET digest = '{"intent":"switch GET /users to cursor pagination and commit it"}' WHERE session_id = ?`, quietCommitID)
	f.exec(`UPDATE sources SET parser = 'claude@3.0', extraction_report = json_set(extraction_report, '$.contract',
		replace(json_extract(extraction_report, '$.contract'), ?, 'claude@3')) WHERE path = ?`, strings.SplitN(claude.ParserName, ".", 2)[0], file)
	f.restart()
	stop := f.run()
	waitFor(t, func() bool {
		return f.count(`SELECT count(*) FROM sources WHERE path = ? AND parser = ?`, file, claude.ParserName) == 1
	})
	f.a.WaitIdle()
	stop()
	if d := stored(); fmt.Sprint(d.Commits) != "[650a939]" || len(d.CommitsNoSHA) != 0 {
		t.Fatalf("after the re-parse: commits %v, without sha %+v", d.Commits, d.CommitsNoSHA)
	}
}
