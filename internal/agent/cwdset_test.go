package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/transcript"
)

// claudeUserAt is a Claude user line recording cwd.
func claudeUserAt(cwd, uuid, text string) string {
	return fmt.Sprintf(`{"parentUuid":null,"isSidechain":false,"userType":"external","cwd":%q,"sessionId":"%s","version":"2.1.0","type":"user","message":{"role":"user","content":%q},"uuid":"%s","timestamp":"2026-09-23T12:00:00.000Z"}`+"\n", cwd, alphaID, text, uuid)
}

func (f *fixture) clearNotified() {
	f.rec.mu.Lock()
	clear(f.rec.notify)
	f.rec.flushed = nil
	f.rec.mu.Unlock()
}

func (f *fixture) alphaSpec() devicesync.SourceSpec {
	return devicesync.SourceSpec{Path: f.path(alphaRel), Agent: transcript.AgentClaude, SessionKey: alphaID}
}

// uploadableAlpha is what of the alpha session was handed to sync and
// that the scheduler's filter would still upload. A companion unchanged
// since the last pass can be handed over before its session's new lines
// are parsed; the filter stops it at upload.
func (f *fixture) uploadableAlpha() []string {
	var out []string
	f.rec.mu.Lock()
	specs := map[string]devicesync.SourceSpec{}
	for p, s := range f.rec.notify {
		specs[p] = s
	}
	for _, p := range f.rec.flushed {
		if _, ok := specs[p]; !ok {
			specs[p] = devicesync.SourceSpec{Path: p}
		}
	}
	f.rec.mu.Unlock()
	for p, s := range specs {
		if underAlpha(s) && f.a.allowUpload(s) {
			out = append(out, p)
		}
	}
	return out
}

func (f *fixture) otherCwds() string {
	f.t.Helper()
	var s string
	if err := f.store.DB().QueryRow(`SELECT ifnull(other_cwds, '') FROM placements WHERE agent = 'claude' AND session_id = ?`, alphaID).Scan(&s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

// A session that moves into a directory a deny rule covers after it
// started: its rows are purged, nothing more is handed to sync, and the
// verdict survives a restart (the directory is stored in its placement).
func TestLaterCwdTightensToDeny(t *testing.T) {
	f, _, logs := rulesFixture(t, "-", "deny /tmp/oracle-secret")
	f.once()
	if f.count(liveCwd, "/tmp/oracle-alpha") == 0 {
		t.Fatal("fixture not indexed")
	}
	if _, ok := f.rec.spec(f.path(alphaRel)); !ok {
		t.Fatal("the allowed session was not handed to sync")
	}
	fi, _ := os.Stat(f.path(alphaRel))
	if n, ok := f.a.uploadBound(f.alphaSpec()); !ok || n != fi.Size() {
		t.Fatalf("upload bound of a parsed transcript: %d %v, want %d", n, ok, fi.Size())
	}
	f.clearNotified()

	appendFile(t, f.path(alphaRel), claudeUserAt("/tmp/oracle-secret/sub", "c9000000-0000-4000-8000-000000000001", "now in the secret checkout"))
	f.once()
	if n := f.count(`SELECT count(*) FROM conversations WHERE session_id = ?`, alphaID); n != 0 {
		t.Errorf("%d conversations left of a session that moved into a denied directory", n)
	}
	if len(f.find("login test flake", true)) != 0 || len(f.find("secret checkout", true)) != 0 {
		t.Error("rows of the denied session still searchable")
	}
	if got := f.uploadableAlpha(); len(got) != 0 {
		t.Errorf("uploadable after the verdict tightened: %v", got)
	}
	if f.a.allowUpload(f.alphaSpec()) {
		t.Error("the sync filter lets the session upload")
	}
	if n, ok := f.a.uploadBound(f.alphaSpec()); !ok || n >= 0 {
		t.Errorf("upload bound of a denied session: %d %v, want a refusal", n, ok)
	}
	if !strings.Contains(f.otherCwds(), "/tmp/oracle-secret/sub") {
		t.Errorf("placement set: %q", f.otherCwds())
	}
	if !strings.Contains(logs.String(), "tightens its verdict") {
		t.Errorf("no log of the tightened verdict:\n%s", logs)
	}

	f.restart()
	f.once()
	if n := f.count(`SELECT count(*) FROM conversations WHERE session_id = ?`, alphaID); n != 0 {
		t.Errorf("a restart indexed the denied session again (%d conversations)", n)
	}
	if f.a.allowUpload(f.alphaSpec()) {
		t.Error("after a restart the sync filter lets the session upload")
	}
}

// A later directory under a local rule keeps the rows but stops uploads,
// and says that what was uploaded before stays on the server.
func TestLaterCwdTightensToLocal(t *testing.T) {
	f, _, logs := rulesFixture(t, "-", "local:/tmp/oracle-secret")
	f.once()
	f.clearNotified()
	appendFile(t, f.path(alphaRel), claudeUserAt("/tmp/oracle-secret", "c9000000-0000-4000-8000-000000000002", "now in the secret checkout"))
	f.once()
	if len(f.find("secret checkout", false)) != 1 {
		t.Error("a local session's new line was not indexed")
	}
	if got := f.uploadableAlpha(); len(got) != 0 {
		t.Errorf("uploadable after the verdict tightened to local: %v", got)
	}
	if _, err := f.a.FlushPath(ctx, f.path(alphaRel), ""); err != nil {
		t.Fatal(err)
	}
	if got := f.uploadableAlpha(); len(got) != 0 {
		t.Errorf("uploadable after a hook flush: %v", got)
	}
	if _, ok := f.rec.spec(f.path(alphaRel)); ok {
		t.Error("the transcript was handed to sync after its verdict tightened to local")
	}
	for _, p := range f.rec.flushed {
		if p == f.path(alphaRel) {
			t.Error("a hook flushed the transcript after its verdict tightened to local")
		}
	}
	if c := f.a.serverCopiesNotice(); c == nil || !strings.Contains(strings.Join(c.Sessions, " "), alphaID) {
		t.Errorf("no server-copies notice: %+v", c)
	}
	if !strings.Contains(logs.String(), "stays on the server") {
		t.Errorf("no server-copies log:\n%s", logs)
	}
}

// Directories that only resemble a denied one do not tighten the verdict.
func TestNearMissLaterCwdDoesNotTighten(t *testing.T) {
	f, _, _ := rulesFixture(t, "-", "deny /tmp/oracle-secret")
	f.once()
	f.clearNotified()
	appendFile(t, f.path(alphaRel),
		claudeUserAt("/tmp/oracle-secretive", "c9000000-0000-4000-8000-000000000003", "near miss one")+
			claudeUserAt("/tmp/oracle-secret-2/x", "c9000000-0000-4000-8000-000000000004", "near miss two")+
			claudeUserAt("tmp/oracle-secret", "c9000000-0000-4000-8000-000000000005", "relative"))
	f.once()
	if len(f.find("near miss two", false)) != 1 {
		t.Error("the session's new lines were not indexed")
	}
	if _, ok := f.rec.spec(f.path(alphaRel)); !ok {
		t.Error("the session was not handed to sync")
	}
	if !f.a.allowUpload(f.alphaSpec()) {
		t.Error("the sync filter blocks a session whose directories only resemble a denied one")
	}
	got := f.otherCwds()
	if !strings.Contains(got, "/tmp/oracle-secretive") || !strings.Contains(got, "/tmp/oracle-secret-2/x") || strings.Contains(got, "\ntmp/") {
		t.Errorf("placement set: %q", got)
	}
}

// Sync reads a transcript only as far as the agent parsed it while rules
// are in force: bytes appended since (which may name a denied directory)
// wait for the parse.
func TestUploadBoundStopsAtParsedOffset(t *testing.T) {
	f, _, _ := rulesFixture(t, "-", "deny /tmp/oracle-secret")
	f.once()
	fi, _ := os.Stat(f.path(alphaRel))
	appendFile(t, f.path(alphaRel), claudeUserAt("/tmp/oracle-secret", "c9000000-0000-4000-8000-000000000006", "unparsed"))
	if n, ok := f.a.uploadBound(f.alphaSpec()); !ok || n != fi.Size() {
		t.Errorf("bound %d %v, want the parsed size %d", n, ok, fi.Size())
	}
	// Nothing can keep a session local: no bound.
	g := newFixture(t, "-")
	g.cfg.Unplaceable = "upload"
	g.a = New(g.store, g.cfg)
	g.once()
	if _, ok := g.a.uploadBound(g.alphaSpec()); ok {
		t.Error("a bound without rules")
	}
}

// olderParser stands in for a parser version that did not list a
// session's other directories.
type olderParser struct{ transcript.Parser }

func (p olderParser) Name() string { return "claude@old" }

func (p olderParser) Parse(ctx context.Context, in transcript.Input, cur transcript.Cursor, sink transcript.Sink) (transcript.Cursor, error) {
	return p.Parser.Parse(ctx, in, cur, stripCwds{sink})
}

type stripCwds struct{ transcript.Sink }

func (s stripCwds) Conversation(c *transcript.Conversation) error {
	c.OtherCwds = nil
	return s.Sink.Conversation(c)
}

// Old sessions: a transcript indexed before the parsers listed every
// directory is re-parsed in the background (D16), and a directory it
// named later then applies (here, a deny purges it).
func TestOldSessionGetsItsDirectoriesOnReparse(t *testing.T) {
	f, _, _ := rulesFixture(t, "-", "deny /tmp/oracle-secret")
	appendFile(t, f.path(alphaRel), claudeUserAt("/tmp/oracle-secret", "c9000000-0000-4000-8000-000000000007", "old secret line"))
	f.a.claude = olderParser{f.a.claude}
	f.once()
	if f.count(`SELECT count(*) FROM conversations WHERE session_id = ?`, alphaID) == 0 {
		t.Fatal("the older parser did not index the session")
	}
	waitRacy()
	f.restart()
	stop := f.run()
	waitFor(t, func() bool {
		return f.count(`SELECT count(*) FROM conversations WHERE session_id = ?`, alphaID) == 0
	})
	stop()
	if !strings.Contains(f.otherCwds(), "/tmp/oracle-secret") {
		t.Errorf("placement set after the re-parse: %q", f.otherCwds())
	}
	if f.a.allowUpload(f.alphaSpec()) {
		t.Error("the sync filter lets the old session upload")
	}
}
