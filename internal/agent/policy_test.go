package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/devin"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// rulesFixture is a fixture whose user rules live in a file.
func rulesFixture(t *testing.T, devinDB string, rules ...string) (*fixture, string, *lockedBuf) {
	t.Helper()
	f := newFixture(t, devinDB)
	file := filepath.Join(t.TempDir(), "path-rules")
	writeRules(t, file, rules...)
	logs := &lockedBuf{}
	f.cfg.UserRules = file
	f.cfg.Logger = slog.New(slog.NewTextHandler(logs, nil))
	f.a = New(f.store, f.cfg)
	return f, file, logs
}

func writeRules(t *testing.T, file string, rules ...string) {
	t.Helper()
	if err := os.WriteFile(file, []byte(strings.Join(rules, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

const (
	liveCwd  = `SELECT count(*) FROM messages m JOIN conversations c ON c.id = m.conversation_id WHERE c.cwd = ?`
	convsCwd = `SELECT count(*) FROM conversations WHERE cwd = ?`
)

func (f *fixture) notified(pred func(devicesync.SourceSpec) bool) []string {
	f.rec.mu.Lock()
	defer f.rec.mu.Unlock()
	var out []string
	for p, s := range f.rec.notify {
		if pred(s) {
			out = append(out, p)
		}
	}
	for _, p := range f.rec.flushed {
		if pred(devicesync.SourceSpec{Path: p}) {
			out = append(out, p)
		}
	}
	return out
}

func underAlpha(s devicesync.SourceSpec) bool {
	return strings.Contains(s.Path, "oracle-alpha") || strings.Contains(s.Path, alphaID) ||
		s.SessionKey == "devin-oracle-001" || strings.Contains(s.Parent, alphaID)
}

// D18: a deny rule keeps a session out of the index and away from sync,
// for Claude (with its subagents and companions), Codex and Devin.
func TestDenyRuleKeepsSessionsOutOfIndexAndSync(t *testing.T) {
	path, _ := buildDevin(t)
	f, _, _ := rulesFixture(t, path, "# personal work", "deny /TMP/oracle-alpha", "oracle-gamma")
	f.once()
	if n := f.count(convsCwd, "/tmp/oracle-gamma"); n != 0 {
		t.Errorf("%d conversations under a directory denied at any depth", n)
	}
	if n := f.count(`SELECT count(*) FROM conversations WHERE agent = 'codex'`); n == 0 {
		t.Error("no Codex conversations indexed")
	}
	if n := f.count(convsCwd, "/tmp/oracle-alpha"); n != 0 {
		t.Errorf("%d conversations under a denied directory indexed", n)
	}
	if n := f.count(`SELECT count(*) FROM conversations WHERE session_id IN (?, 'agent-a1b2c3', 'devin-oracle-001')`, alphaID); n != 0 {
		t.Errorf("%d denied sessions indexed", n)
	}
	if n := f.count(`SELECT count(*) FROM sources WHERE path LIKE ?`, "%"+alphaID+"%"); n != 0 {
		t.Errorf("%d sources recorded for a denied session", n)
	}
	if n := f.count(liveCwd, "/tmp/oracle-beta"); n == 0 {
		t.Error("an allowed directory was not indexed")
	}
	if got := f.notified(underAlpha); len(got) != 0 {
		t.Errorf("denied sources handed to sync: %v", got)
	}
	if _, ok := f.rec.spec(devin.ExportPath(path, "devin-oracle-002")); !ok {
		t.Error("an allowed Devin session was not handed to sync")
	}
}

// D18: a local rule indexes the session but never uploads it, hook
// flushes included.
func TestLocalRuleIndexesButNeverUploads(t *testing.T) {
	path, _ := buildDevin(t)
	f, _, _ := rulesFixture(t, path, "local:/tmp/oracle-alpha")
	f.once()
	if len(f.find("login test flake", false)) != 1 {
		t.Error("a local session was not indexed")
	}
	if n := f.count(`SELECT count(*) FROM conversations WHERE session_id = 'devin-oracle-001'`); n != 1 {
		t.Error("a local Devin session was not indexed")
	}
	if _, err := f.a.FlushPath(ctx, f.path(alphaRel), ""); err != nil {
		t.Fatal(err)
	}
	if got := f.notified(underAlpha); len(got) != 0 {
		t.Errorf("local sources handed to sync: %v", got)
	}
	if f.a.allowUpload(devicesync.SourceSpec{Path: f.path(alphaRel), Agent: transcript.AgentClaude}) {
		t.Error("the sync filter lets a local source upload")
	}
	if f.a.allowUpload(devicesync.SourceSpec{Path: devin.ExportPath(path, "devin-oracle-001"), Agent: transcript.AgentDevin, SessionKey: "devin-oracle-001", Export: true}) {
		t.Error("the sync filter lets a local Devin session upload")
	}
	if !f.a.allowUpload(devicesync.SourceSpec{Path: f.path(codexActive), Agent: transcript.AgentCodex}) {
		t.Error("the sync filter blocks an allowed source")
	}
}

// A Devin session a local rule kept from sync is handed over once the
// rule is removed, though the store has not changed since.
func TestLoosenedLocalRuleHandsDevinSessionToSync(t *testing.T) {
	path, _ := buildDevin(t)
	f, file, _ := rulesFixture(t, path, "local:/tmp/oracle-alpha")
	f.once()
	f.a.pollDevin(ctx, true, true)
	spec := devin.ExportPath(path, "devin-oracle-001")
	if _, ok := f.rec.spec(spec); ok {
		t.Fatal("a local Devin session was handed to sync")
	}
	writeRules(t, file, "# nothing local")
	f.once()
	f.a.pollDevin(ctx, true, true)
	if _, ok := f.rec.spec(spec); !ok {
		t.Fatal("the Devin session was never handed to sync after its local rule was removed")
	}
}

// D18: a deny rule added later purges the session's rows from the local
// index and says that server copies stay; removing the rule indexes the
// session again.
func TestNewDenyPurgesAndLoosenedRuleReindexes(t *testing.T) {
	path, _ := buildDevin(t)
	f, file, logs := rulesFixture(t, path)
	f.once()
	if f.count(liveCwd, "/tmp/oracle-alpha") == 0 || f.count(`SELECT count(*) FROM conversations WHERE session_id = 'devin-oracle-001'`) == 0 {
		t.Fatal("fixture not indexed")
	}
	writeRules(t, file, "deny /tmp/oracle-alpha")
	f.once()
	if n := f.count(convsCwd, "/tmp/oracle-alpha"); n != 0 {
		t.Errorf("%d conversations left under a newly denied directory", n)
	}
	if n := f.count(`SELECT count(*) FROM conversations WHERE session_id IN (?, 'agent-a1b2c3')`, alphaID); n != 0 {
		t.Errorf("%d denied Claude conversations left (with subagents)", n)
	}
	if len(f.find("login test flake", true)) != 0 {
		t.Error("purged rows still searchable")
	}
	if !strings.Contains(logs.String(), "stay on the server") {
		t.Errorf("no prompt about server copies in:\n%s", logs)
	}
	if c := f.a.serverCopiesNotice(); c == nil || c.Count == 0 || len(c.Sessions) == 0 || !strings.Contains(strings.Join(c.Sessions, " "), alphaID) {
		t.Errorf("status carries no server-copies notice: %+v", c)
	}
	if n := f.count(liveCwd, "/tmp/oracle-beta"); n == 0 {
		t.Error("an allowed directory was purged")
	}

	// The same rules after a restart change nothing.
	f.restart()
	f.once()
	if n := f.count(convsCwd, "/tmp/oracle-alpha"); n != 0 {
		t.Errorf("denied sessions back after a restart: %d", n)
	}

	writeRules(t, file, "# nothing denied")
	f.once()
	f.a.pollDevin(ctx, true, true)
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.find("login test flake", false)) != 1 {
		t.Error("a session was not indexed again after its deny rule was removed")
	}
	if n := f.count(`SELECT count(*) FROM conversations WHERE session_id = 'devin-oracle-001'`); n != 1 {
		t.Error("a Devin session was not indexed again after its deny rule was removed")
	}
}

// D18: admin rules are a floor a user rule cannot loosen but can tighten;
// the last fetched admin rules apply when the server is unreachable.
func TestAdminRulesAreAFloor(t *testing.T) {
	f, file, _ := rulesFixture(t, "-", "allow /tmp/**")
	cache := filepath.Join(t.TempDir(), "admin.json")
	f.cfg.AdminRulesCache = cache
	f.cfg.AdminRules = func(context.Context) (AdminPolicy, error) {
		return AdminPolicy{Rules: []string{"local:/tmp/oracle-alpha"}}, nil
	}
	f.a = New(f.store, f.cfg)
	f.once()
	if len(f.find("login test flake", false)) != 1 {
		t.Fatal("admin local rule should still index")
	}
	if got := f.notified(underAlpha); len(got) != 0 {
		t.Errorf("a user allow rule loosened the admin floor: %v", got)
	}

	// Offline: the cached rules apply.
	f.rec = newRecorder()
	f.cfg.Sync = f.rec
	f.cfg.AdminRules = func(context.Context) (AdminPolicy, error) { return AdminPolicy{}, errors.New("server down") }
	f.a = New(f.store, f.cfg)
	f.once()
	if got := f.notified(underAlpha); len(got) != 0 {
		t.Errorf("cached admin rules not applied offline: %v", got)
	}
	if _, ok := f.rec.spec(f.path(codexActive)); !ok {
		t.Error("an allowed source was not handed to sync")
	}

	// The user tightens it.
	writeRules(t, file, "deny /tmp/oracle-alpha")
	f.once()
	if n := f.count(convsCwd, "/tmp/oracle-alpha"); n != 0 {
		t.Errorf("user deny over an admin local rule left %d conversations", n)
	}
}

func TestFetchAdminRules(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/policy" || r.Header.Get("Authorization") != "Bearer dev-token" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"max_storage_bytes":1,"path_rules":["/personal/","local:/acme"],"unplaceable":"exclude"}`))
	}))
	defer srv.Close()
	got, err := FetchAdminRules(srv.URL+"/", "dev-token", srv.Client())(ctx)
	if err != nil || strings.Join(got.Rules, ",") != "/personal/,local:/acme" || got.Unplaceable != "exclude" {
		t.Fatalf("policy %+v, %v", got, err)
	}
	if _, err := FetchAdminRules(srv.URL, "wrong", srv.Client())(ctx); err == nil {
		t.Fatal("a rejected fetch should fail")
	}
}

// scanHints: the first cwd in the first complete lines; pending while the
// only line naming it is incomplete; the remote a Codex rollout recorded.
func TestScanHints(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		body  string
		cwd   string
		known bool
	}{
		{`{"type":"summary"}` + "\n" + `{"type":"user","cwd":"/a/b"}` + "\n", "/a/b", true},
		{`{"type":"session_meta","payload":{"cwd":"/c/d"}}` + "\n", "/c/d", true},
		{`{"type":"summary"}` + "\n" + `{"type":"user","cwd":"/a`, "", false},
		{`{"type":"summary"}` + "\n", "", false},
		{strings.Repeat(`{"type":"summary"}`+"\n", cwdScanLines), "", true},
		{`{"id":"x","git":{}}` + "\n" + `{"type":"message","content":[{"text":"<environment_context>\n  <cwd>/old/codex</cwd>\n</environment_context>"}]}` + "\n", "/old/codex", true},
		{`{"type":"message","content":[{"text":"\u003ccwd\u003e/esc/aped\u003c/cwd\u003e"}]}` + "\n", "/esc/aped", true},
		{"", "", false},
		{`{"id":"x","git":{"repository_url":"git@github.com:Acme/Web.git"}}` + "\n", "", false},
	}
	for i, c := range cases {
		p := filepath.Join(dir, "t.jsonl")
		if err := os.WriteFile(p, []byte(c.body), 0o600); err != nil {
			t.Fatal(err)
		}
		h := scanHints(p)
		if h.cwd != c.cwd || (h.state != hintsPending) != c.known {
			t.Errorf("case %d: %+v, want %q %v", i, h, c.cwd, c.known)
		}
	}
	if h := scanHints(filepath.Join(dir, "missing")); h.state != hintsPending {
		t.Error("a missing file is known")
	}
	p := filepath.Join(dir, "meta.jsonl")
	os.WriteFile(p, []byte(`{"type":"session_meta","payload":{"id":"x","git":{"repository_url":"https://github.com/acme/web.git"}}}`+"\n"), 0o600)
	if h := scanHints(p); h.remote != "github.com/acme/web" {
		t.Errorf("session_meta remote %+v", h)
	}
}

// D18: a denied session's companions stay away from sync after Claude's
// cleanup deletes its transcript (an orphaned session): the index has no
// directory for a session it never indexed or purged.
func TestDeniedOrphanCompanionNeverUploads(t *testing.T) {
	for _, purged := range []bool{false, true} {
		t.Run(fmt.Sprintf("purged=%v", purged), func(t *testing.T) {
			var f *fixture
			var file string
			if purged {
				f, file, _ = rulesFixture(t, "-")
			} else {
				f, _, _ = rulesFixture(t, "-", "deny /tmp/oracle-alpha")
			}
			secret := filepath.Join(strings.TrimSuffix(f.path(alphaRel), ".jsonl"), "tool-results", "secret01.txt")
			if err := os.MkdirAll(filepath.Dir(secret), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(secret, []byte("secret tool output\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			f.once()
			if purged {
				writeRules(t, file, "deny /tmp/oracle-alpha")
				f.once()
			} else if _, ok := f.rec.spec(secret); ok {
				t.Fatal("a denied session's companion was handed to sync")
			}
			if err := os.Remove(f.path(alphaRel)); err != nil {
				t.Fatal(err)
			}
			f.rec = newRecorder()
			f.cfg.Sync = f.rec
			f.restart()
			f.once()
			if _, ok := f.rec.spec(secret); ok {
				t.Error("an orphaned denied session's companion was handed to sync")
			}
			if f.a.allowUpload(devicesync.SourceSpec{Path: secret, Agent: transcript.AgentClaude, StorageKind: transcript.StorageCompanion,
				SessionKey: alphaID, Parent: f.path(alphaRel)}) {
				t.Error("the sync filter lets an orphaned denied session's companion upload")
			}
		})
	}
}

// filterRecorder is a recorder that, like devicesync.Scheduler, accepts a
// filter.
type filterRecorder struct {
	*recorder
	mu     sync.Mutex
	filter func(devicesync.SourceSpec) bool
}

func (r *filterRecorder) SetFilter(fn func(devicesync.SourceSpec) bool) {
	r.mu.Lock()
	r.filter = fn
	r.mu.Unlock()
}

// D18: the sync scheduler starts uploading what an earlier run queued as
// soon as it runs, so the agent installs its filter, with the rules on
// disk, when it is created: before the startup pass, not after it.
func TestSyncFilterInstalledWithRulesAtNew(t *testing.T) {
	f, _, _ := rulesFixture(t, "-", "deny /tmp/oracle-alpha")
	fr := &filterRecorder{recorder: f.rec}
	f.cfg.Sync = fr
	f.restart()
	fr.mu.Lock()
	filter := fr.filter
	fr.mu.Unlock()
	if filter == nil {
		t.Fatal("no sync filter installed before the startup pass")
	}
	if filter(devicesync.SourceSpec{Path: f.path(alphaRel), Agent: transcript.AgentClaude}) {
		t.Error("the sync filter lets a denied source upload before the startup pass")
	}
	// A session in a directory that exists (the fixture's are gone, and
	// wait for the recovery pass).
	allowed := f.claudeSessionAt("0b7e2c1a-0000-4000-8000-0000000000c1", realDir(t), "allowed")
	if !filter(devicesync.SourceSpec{Path: allowed, Agent: transcript.AgentClaude, SessionKey: "0b7e2c1a-0000-4000-8000-0000000000c1"}) {
		t.Error("the sync filter blocks an allowed source")
	}
}

// D18 with L8: a transcript is often seen before its first line names the
// directory (the watcher reports it on create). That early look must not
// settle the verdict: once the denied cwd is written, the session stays out
// of the index and away from sync.
func TestDenyRuleAppliesToTranscriptWrittenAfterCreate(t *testing.T) {
	for name, early := range map[string]string{"empty": "", "metadata first": `{"type":"summary","summary":"x"}` + "\n"} {
		t.Run(name, func(t *testing.T) {
			f, _, _ := rulesFixture(t, "-", "deny /tmp/oracle-alpha")
			f.once()
			const sid = "0b7e2c1a-0000-4000-8000-0000000000cc"
			p := filepath.Join(filepath.Dir(f.path(alphaRel)), sid+".jsonl")
			if err := os.WriteFile(p, []byte(early), 0o600); err != nil {
				t.Fatal(err)
			}
			f.once()
			appendFile(t, p, strings.ReplaceAll(claudeUser("c1000000-0000-4000-8000-0000000000cc", "written after create"), alphaID, sid))
			f.once()
			if n := f.count(`SELECT count(*) FROM conversations WHERE session_id = ?`, sid); n != 0 {
				t.Errorf("%d conversations indexed for a denied session", n)
			}
			if got := f.notified(func(s devicesync.SourceSpec) bool { return s.Path == p }); len(got) != 0 {
				t.Errorf("denied transcript handed to sync: %v", got)
			}
			if f.a.allowUpload(devicesync.SourceSpec{Path: p}) {
				t.Error("sync filter allows the denied transcript")
			}
		})
	}
}
