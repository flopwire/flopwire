package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/devin"
)

const (
	oracleHome  = "../../testdata/oracle/home"
	devinSeed   = "../../testdata/oracle/seeds/devin.sql"
	alphaID     = "0b7e2c1a-0000-4000-8000-000000000001"
	alphaRel    = ".claude/projects/-tmp-oracle-alpha/" + alphaID + ".jsonl"
	orphanID    = "0b7e2c1a-0000-4000-8000-00000000000f"
	orphanTxt   = ".claude/projects/-tmp-oracle-beta/" + orphanID + "/tool-results/orphan01.txt"
	codexActive = ".codex/sessions/2026/08/10/rollout-2026-08-10T14-00-00-019a0000-0000-7000-8000-0000000000a2.jsonl"
)

var ctx = context.Background()

// recorder is a Sync that records what the agent handed over.
type recorder struct {
	mu      sync.Mutex
	notify  map[string]devicesync.SourceSpec
	exports map[string]func(context.Context) ([]byte, error)
	flushed []string
}

func newRecorder() *recorder {
	return &recorder{notify: map[string]devicesync.SourceSpec{}, exports: map[string]func(context.Context) ([]byte, error){}}
}

func (r *recorder) Notify(s devicesync.SourceSpec) {
	r.mu.Lock()
	r.notify[s.Path] = s
	r.mu.Unlock()
}

func (r *recorder) NotifyExportFunc(s devicesync.SourceSpec, fn func(context.Context) ([]byte, error)) {
	r.mu.Lock()
	r.notify[s.Path], r.exports[s.Path] = s, fn
	r.mu.Unlock()
}

func (r *recorder) Flush(s devicesync.SourceSpec) {
	r.mu.Lock()
	r.flushed = append(r.flushed, s.Path)
	r.mu.Unlock()
}

func (r *recorder) spec(path string) (devicesync.SourceSpec, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.notify[path]
	return s, ok
}

type fixture struct {
	t     *testing.T
	home  string
	store *localindex.Store
	rec   *recorder
	a     *Agent
	cfg   Config
}

// newFixture copies the oracle home into a temp dir and opens an index.
// devinDB is "-" or a store path.
func newFixture(t *testing.T, devinDB string) *fixture {
	t.Helper()
	home := t.TempDir()
	copyTree(t, oracleHome, home)
	store, err := localindex.Open(filepath.Join(t.TempDir(), "index.db"), localindex.Options{DeferCommit: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	f := &fixture{t: t, home: home, store: store, rec: newRecorder()}
	f.cfg = Config{ClaudeProjects: filepath.Join(home, ".claude", "projects"), CodexHome: filepath.Join(home, ".codex"),
		DevinDB: devinDB, Workers: 3, Sync: f.rec, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	f.a = New(store, f.cfg)
	return f
}

func (f *fixture) path(rel string) string { return filepath.Join(f.home, rel) }

func (f *fixture) once() {
	f.t.Helper()
	if err := f.a.Once(ctx); err != nil {
		f.t.Fatal(err)
	}
}

// restart is a new agent on the same index, as after a process restart.
func (f *fixture) restart() {
	f.a = New(f.store, f.cfg)
}

func (f *fixture) find(pattern string, superseded bool) []localindex.FindHit {
	f.t.Helper()
	hits, err := f.store.Find(ctx, pattern, localindex.FindOptions{Filter: localindex.Filter{IncludeSuperseded: superseded}})
	if err != nil {
		f.t.Fatal(err)
	}
	return hits
}

func (f *fixture) count(q string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.store.DB().QueryRow(q, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func claudeUser(uuid, text string) string {
	return fmt.Sprintf(`{"parentUuid":null,"isSidechain":false,"userType":"external","cwd":"/tmp/oracle-alpha","sessionId":"%s","version":"2.1.0","type":"user","message":{"role":"user","content":%q},"uuid":"%s","timestamp":"2026-09-23T11:00:00.000Z"}`+"\n", alphaID, text, uuid)
}

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteString(s); err != nil {
		t.Fatal(err)
	}
	fh.Close()
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// ageAll moves every file's change time out of the racy window by waiting
// it out; tests that assert "nothing parsed" need a non-racy gate.
func waitRacy() { time.Sleep(transcript.RacyWindow + 100*time.Millisecond) }

func TestDiscoveryIndexesEveryHarness(t *testing.T) {
	f := newFixture(t, "-")
	f.once()
	f.a.mu.Lock()
	var transcripts, companions int
	for _, tg := range f.a.targets {
		switch tg.kind {
		case kindTranscript:
			transcripts++
		case kindCompanion:
			companions++
		}
	}
	orphan := f.a.targets[f.path(orphanTxt)]
	meta := f.a.targets[f.path(".claude/projects/-tmp-oracle-alpha/"+alphaID+"/subagents/agent-a1b2c3.meta.json")]
	f.a.mu.Unlock()
	if transcripts < 8 || companions < 4 {
		t.Fatalf("transcripts %d companions %d", transcripts, companions)
	}
	// Claude main + subagents, Codex sessions/ and archived_sessions/.
	for _, agent := range []string{"claude", "codex"} {
		if n := f.count(`SELECT count(*) FROM messages m JOIN sources s ON s.id = m.source_id WHERE s.agent = ?`, agent); n == 0 {
			t.Errorf("no %s rows", agent)
		}
	}
	if n := f.count(`SELECT count(*) FROM sources WHERE path LIKE '%archived_sessions%'`); n != 1 {
		t.Errorf("archived rollout sources %d", n)
	}
	if n := f.count(`SELECT count(*) FROM conversations WHERE session_id = 'agent-a1b2c3' AND parent_conversation_id IS NOT NULL`); n != 1 {
		t.Error("subagent not linked to its parent")
	}
	// Companions: recorded under their conversation, synced with a parent.
	if orphan == nil || orphan.parent != f.path(".claude/projects/-tmp-oracle-beta/"+orphanID+".jsonl") || orphan.owner != orphanID {
		t.Errorf("orphan companion %+v", orphan)
	}
	if meta == nil || meta.owner != "agent-a1b2c3" || !strings.HasSuffix(meta.parent, "agent-a1b2c3.jsonl") {
		t.Errorf("meta companion %+v", meta)
	}
	if n := f.count(`SELECT count(*) FROM companions c JOIN conversations v ON v.id = c.conversation_id WHERE v.session_id = ?`, orphanID); n != 1 {
		t.Error("orphan companion has no stub conversation")
	}
	spec, ok := f.rec.spec(f.path(orphanTxt))
	if !ok || spec.Parent == "" || spec.StorageKind != transcript.StorageCompanion {
		t.Errorf("companion spec %+v", spec)
	}
	if _, ok := f.rec.spec(f.path(alphaRel)); !ok {
		t.Error("transcript not handed to sync")
	}
}

func TestSweepGateSkipsUnchangedAndAppends(t *testing.T) {
	f := newFixture(t, "-")
	f.once()
	waitRacy()
	f.once() // re-verifies the racy entries once
	f.restart()
	f.once()
	if n := f.a.stats.Indexed.Load() + f.a.stats.Unchanged.Load(); n != 0 {
		t.Fatalf("restart with nothing changed opened %d transcripts", n)
	}

	appendFile(t, f.path(alphaRel), claudeUser("c1000000-0000-4000-8000-0000000000aa", "zebra appended line"))
	f.once()
	if got := f.a.stats.Appends.Load(); got != 1 {
		t.Fatalf("appends %d", got)
	}
	if hits := f.find("zebra appended", false); len(hits) != 1 {
		t.Fatalf("appended line: %d hits", len(hits))
	}
	// A partial line is left for the next pass.
	appendFile(t, f.path(alphaRel), `{"type":"user","uuid":"partial`)
	f.once()
	if hits := f.find("partial", false); len(hits) != 0 {
		t.Fatal("indexed an incomplete line")
	}
}

func TestRewriteStartsGenerationAndSupersedes(t *testing.T) {
	f := newFixture(t, "-")
	p := f.path(alphaRel)
	appendFile(t, p, claudeUser("c1000000-0000-4000-8000-0000000000ab", "walrus doomed line"))
	f.once()
	if len(f.find("walrus doomed", false)) != 1 {
		t.Fatal("line not indexed")
	}
	// Rewrite in place (same inode): drop the last line, add another.
	b, _ := os.ReadFile(p)
	lines := strings.SplitAfter(strings.TrimSuffix(string(b), "\n"), "\n")
	kept := strings.Join(lines[:len(lines)-1], "") + "\n" + claudeUser("c1000000-0000-4000-8000-0000000000ac", "narwhal replacement")
	if err := os.WriteFile(p, []byte(kept), 0o644); err != nil {
		t.Fatal(err)
	}
	f.once()
	if f.a.stats.Rewrites.Load() != 1 {
		t.Fatalf("rewrites %d", f.a.stats.Rewrites.Load())
	}
	if len(f.find("walrus doomed", false)) != 0 || len(f.find("walrus doomed", true)) != 1 {
		t.Error("dropped line should be superseded, not deleted")
	}
	if len(f.find("narwhal replacement", false)) != 1 {
		t.Error("new line missing")
	}
	if len(f.find("login test flake", false)) != 1 {
		t.Error("kept line should stay live")
	}
	if n := f.count(`SELECT generation FROM sources WHERE path = ?`, p); n != 2 {
		t.Errorf("generation %d", n)
	}
}

func TestNewInodeAtSamePathRetiresOldSource(t *testing.T) {
	f := newFixture(t, "-")
	p := f.path(alphaRel)
	appendFile(t, p, claudeUser("c1000000-0000-4000-8000-0000000000ad", "okapi vanishing"))
	f.once()
	b, _ := os.ReadFile(p)
	lines := strings.SplitAfter(strings.TrimSuffix(string(b), "\n"), "\n")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines[:len(lines)-1], "")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p); err != nil {
		t.Fatal(err)
	}
	f.once()
	if n := f.count(`SELECT count(*) FROM sources WHERE path = ?`, p); n != 2 {
		t.Fatalf("sources at path %d, want 2 (one per identity)", n)
	}
	if len(f.find("okapi vanishing", false)) != 0 || len(f.find("okapi vanishing", true)) != 1 {
		t.Error("line absent from the new file should be superseded")
	}
	if len(f.find("login test flake", false)) != 1 {
		t.Error("line kept in the new file should stay live")
	}
}

func TestCodexArchiveMoveKeepsSource(t *testing.T) {
	f := newFixture(t, "-")
	f.once()
	src := f.path(codexActive)
	var id int64
	f.store.DB().QueryRow(`SELECT id FROM sources WHERE path = ?`, src).Scan(&id)
	rows := f.count(`SELECT count(*) FROM messages`)
	dst := filepath.Join(f.home, ".codex", "archived_sessions", filepath.Base(src))
	if err := os.Rename(src, dst); err != nil {
		t.Fatal(err)
	}
	f.once()
	if f.a.stats.Renames.Load() != 1 {
		t.Fatalf("renames %d", f.a.stats.Renames.Load())
	}
	var got int64
	f.store.DB().QueryRow(`SELECT id FROM sources WHERE path = ?`, dst).Scan(&got)
	if got != id || f.count(`SELECT count(*) FROM sources WHERE path = ?`, src) != 0 {
		t.Errorf("moved rollout got source %d, want %d", got, id)
	}
	if n := f.count(`SELECT count(*) FROM messages`); n != rows {
		t.Errorf("rows %d after move, want %d", n, rows)
	}
	if n := f.count(`SELECT count(*) FROM messages WHERE superseded = 1`); n != 0 {
		t.Errorf("%d rows superseded by a move", n)
	}
}

func TestVanishedFileKeepsRowsLive(t *testing.T) {
	f := newFixture(t, "-")
	f.once()
	if err := os.Remove(f.path(alphaRel)); err != nil {
		t.Fatal(err)
	}
	f.once()
	if len(f.find("login test flake", false)) != 1 {
		t.Error("rows of a deleted transcript should stay searchable")
	}
	// D1: no flag either, and a restart does not change that.
	f.restart()
	f.once()
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.find("login test flake", false)) != 1 {
		t.Error("rows of a deleted transcript hidden after a restart")
	}
	flagged := `SELECT count(*) FROM conversations WHERE session_id = ? AND deleted_in_generation IS NOT NULL`
	if n := f.count(flagged, alphaID); n != 0 {
		t.Error("conversation of a deleted transcript flagged deleted")
	}
	superseded := `SELECT count(*) FROM messages m JOIN conversations c ON c.id = m.conversation_id WHERE c.session_id = ? AND m.superseded = 1`
	if n := f.count(superseded, alphaID); n != 0 {
		t.Errorf("%d rows of a deleted transcript superseded", n)
	}
}

func buildDevin(t *testing.T) (string, *sql.DB) {
	t.Helper()
	seed, err := os.ReadFile(devinSeed)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "sessions.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(string(seed)); err != nil {
		t.Fatal(err)
	}
	return path, db
}

func TestDevinSessionDeletion(t *testing.T) {
	path, db := buildDevin(t)
	f := newFixture(t, path)
	f.once()
	live := `SELECT count(*) FROM messages m JOIN conversations c ON c.id = m.conversation_id WHERE c.session_id = ? AND m.superseded = 0`
	if n := f.count(live, "devin-oracle-002"); n == 0 {
		t.Fatal("no Devin rows")
	}
	gone := devin.ExportPath(path, "devin-oracle-002")
	if _, ok := f.rec.spec(gone); !ok {
		t.Fatal("session not handed to sync")
	}
	// Unchanged store: the stat gate skips the parse.
	polls := f.a.stats.DevinPolls.Load()
	f.a.pollDevin(ctx, false, false)
	f.a.devin.last = time.Time{}
	f.a.pollDevin(ctx, false, false)
	if f.a.stats.DevinPolls.Load() != polls {
		t.Error("unchanged store re-parsed")
	}

	f.rec = newRecorder()
	f.a.cfg.Sync = f.rec
	for _, q := range []string{`DELETE FROM message_nodes WHERE session_id = 'devin-oracle-002'`, `DELETE FROM sessions WHERE id = 'devin-oracle-002'`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	f.a.devin.last = time.Time{}
	f.a.pollDevin(ctx, false, false)
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.count(live, "devin-oracle-002"); n != 0 {
		t.Errorf("%d rows of the deleted session still live", n)
	}
	if n := f.count(`SELECT count(*) FROM messages m JOIN conversations c ON c.id = m.conversation_id WHERE c.session_id = ?`, "devin-oracle-002"); n == 0 {
		t.Error("deleted session's rows were dropped")
	}
	fn := f.rec.exports[gone]
	if fn == nil {
		t.Fatal("deleted session not handed to sync")
	}
	data, err := fn(ctx)
	if err != nil || !strings.Contains(string(data), `"gone"`) {
		t.Errorf("export of a deleted session: %q %v", data, err)
	}
	if spec := f.rec.notify[gone]; spec.Parser != devin.ExportFormat || !spec.Export {
		t.Errorf("export spec %+v", spec)
	}
	if _, ok := f.rec.spec(devin.ExportPath(path, "devin-oracle-001")); ok {
		t.Error("untouched session re-exported")
	}
}

func TestHookFlushIndexesAtOnce(t *testing.T) {
	f := newFixture(t, "-")
	f.cfg.Sweep, f.cfg.FastLane = time.Hour, time.Hour // only the hook can index
	f.a = New(f.store, f.cfg)
	runCtx, cancel := context.WithCancel(ctx)
	sock := filepath.Join(shortTemp(t), "a.sock")
	done := make(chan error, 2)
	go func() { done <- f.a.Run(runCtx) }()
	go func() { done <- f.a.Serve(runCtx, sock) }()
	defer func() {
		cancel()
		<-done
		<-done
	}()
	waitFor(t, func() bool { _, err := Call(ctx, sock, Request{Op: "ping"}); return err == nil })
	f.a.WaitIdle()

	p := f.path(alphaRel)
	appendFile(t, p, claudeUser("c1000000-0000-4000-8000-0000000000ae", "hook flushed quokka"))
	resp, err := Call(ctx, sock, Request{Op: "flush", Path: p})
	if err != nil || resp.Path != p {
		t.Fatalf("flush: %+v %v", resp, err)
	}
	if len(f.find("hook flushed quokka", false)) != 1 {
		t.Error("line not indexed when flush returned")
	}
	f.rec.mu.Lock()
	flushed := strings.Join(f.rec.flushed, ",")
	f.rec.mu.Unlock()
	if flushed != p {
		t.Errorf("sync flushes %q", flushed)
	}
	// By session id (Codex notify carries the thread id).
	if resp, err := Call(ctx, sock, Request{Op: "flush", Session: "019a0000-0000-7000-8000-0000000000a2"}); err != nil || resp.Path != f.path(codexActive) {
		t.Errorf("flush by session: %+v %v", resp, err)
	}
	if _, err := Call(ctx, sock, Request{Op: "flush", Path: "/nonexistent.jsonl"}); err == nil {
		t.Error("flush of an unknown path should fail")
	}
}

// A hook flush that arrives while the agent starts, before its first
// discovery pass, waits for that pass rather than failing (a Codex notify
// carries only the thread id) or merging a partial pass that makes load
// skip the stored gates.
func TestHookFlushBeforeFirstDiscoveryWaits(t *testing.T) {
	f := newFixture(t, "-")
	f.cfg.Sweep, f.cfg.FastLane = time.Hour, time.Hour
	f.a = New(f.store, f.cfg)
	const codexThread = "019a0000-0000-7000-8000-0000000000a2"

	early, stop := context.WithCancel(ctx)
	stop()
	for _, req := range []Request{{Session: codexThread}, {Path: f.path(alphaRel)}} {
		if _, err := f.a.FlushPath(early, req.Path, req.Session); !errors.Is(err, context.Canceled) {
			t.Errorf("flush %+v before discovery: %v, want it to wait", req, err)
		}
	}
	f.a.mu.Lock()
	n := len(f.a.targets)
	f.a.mu.Unlock()
	if n != 0 {
		t.Fatalf("flush before discovery tracked %d files; load would skip the stored gates", n)
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- f.a.Run(runCtx) }()
	defer func() {
		cancel()
		<-done
	}()
	if p, err := f.a.FlushPath(ctx, "", codexThread); err != nil || p != f.path(codexActive) {
		t.Errorf("flush by session at start: %q %v", p, err)
	}
}

// shortTemp is a temp dir with a short path: unix socket paths are
// limited to about 104 bytes on macOS.
func shortTemp(t *testing.T) string {
	d, err := os.MkdirTemp("/tmp", "tm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// L7: a discovery pass queues files oldest-modified first, so row ids of a
// bulk load follow time rather than path order.
func TestDiscoveryQueuesByModTime(t *testing.T) {
	f := newFixture(t, "-")
	if err := f.a.load(ctx); err != nil {
		t.Fatal(err)
	}
	found, err := f.a.discoverAll()
	if err != nil {
		t.Fatal(err)
	}
	// Reverse path order in time: the last path is the oldest.
	var paths []string
	for _, tg := range found.targets {
		paths = append(paths, tg.path)
	}
	sort.Strings(paths)
	base := time.Now().Add(-time.Hour)
	for i, p := range paths {
		mt := base.Add(time.Duration(len(paths)-i) * time.Second)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	f.a.merge(ctx, found, true)
	f.a.mu.Lock()
	queued := f.a.normal.targets()
	f.a.mu.Unlock()
	if len(queued) < 8 {
		t.Fatalf("queued %d", len(queued))
	}
	var prev time.Time
	for _, q := range queued {
		fi, err := os.Stat(q.path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.ModTime().Before(prev) {
			t.Fatalf("queue not in modification order at %s", q.path)
		}
		prev = fi.ModTime()
	}
}

// L8: an inotify write event for a file the agent does not track must not
// rescan the project (a create event does).
func TestWriteEventForUntrackedFileIsIgnored(t *testing.T) {
	f := newFixture(t, "-")
	f.once()
	proj := filepath.Dir(f.path(alphaRel))
	const sid = "0b7e2c1a-0000-4000-8000-0000000000bb"
	p := filepath.Join(proj, sid+".jsonl")
	if err := os.WriteFile(p, []byte(strings.ReplaceAll(claudeUser("c1000000-0000-4000-8000-0000000000bb", "untracked write"), alphaID, sid)), 0o600); err != nil {
		t.Fatal(err)
	}
	tracked := func() bool {
		f.a.mu.Lock()
		defer f.a.mu.Unlock()
		return f.a.targets[p] != nil
	}
	f.a.dirEvent(ctx, watchEvent{path: filepath.Join(proj, "notes.txt"), modify: true}, nil)
	if tracked() {
		t.Fatal("a write to an untracked file rescanned the project")
	}
	w := newWatcher(f.cfg.Logger)
	defer w.close()
	f.a.dirEvent(ctx, watchEvent{path: p}, w)
	if !tracked() {
		t.Fatal("a create event did not discover the new transcript")
	}
}

// L8: a new Claude project directory triggers a full pass, then gets a
// watch. A transcript created after the pass listed the project but before
// the watch existed must still be found then, not at the next sweep.
func TestNewProjectDirListedAfterWatch(t *testing.T) {
	f := newFixture(t, "-")
	f.once()
	w := newWatcher(f.cfg.Logger)
	defer w.close()
	f.a.rewatch(w)
	proj := filepath.Join(f.cfg.ClaudeProjects, "-tmp-oracle-new")
	if err := os.Mkdir(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	const sid = "0b7e2c1a-0000-4000-8000-0000000000dd"
	p := filepath.Join(proj, sid+".jsonl")
	testHookAfterRootSweep = func() {
		if err := os.WriteFile(p, []byte(strings.ReplaceAll(claudeUser("c1000000-0000-4000-8000-0000000000dd", "late file"), alphaID, sid)), 0o600); err != nil {
			t.Error(err)
		}
	}
	defer func() { testHookAfterRootSweep = nil }()
	f.a.scanDir(ctx, f.cfg.ClaudeProjects, w)
	f.a.mu.Lock()
	tracked := f.a.targets[p] != nil
	f.a.mu.Unlock()
	if !tracked {
		t.Fatal("a transcript created before its new project directory was watched waits for the next sweep")
	}
}

// A pass asked over the control socket while the agent starts (the
// post-bulk-load re-exec serves before Run's load) waits for the first
// discovery pass too: run before load, its full merge would make load skip
// the stored gates, and it would release waiting flushes before load.
func TestPassBeforeFirstDiscoveryWaits(t *testing.T) {
	f := newFixture(t, "-")
	f.once()
	waitRacy()
	f.once() // re-verifies the racy entries once
	f.restart()
	f.cfg.Sweep, f.cfg.FastLane = time.Hour, time.Hour
	f.a = New(f.store, f.cfg)

	early, stop := context.WithCancel(ctx)
	stop()
	done := make(chan error, 1)
	go func() { done <- f.a.Pass(early) }()
	deadline := time.After(2 * time.Second)
wait:
	for {
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("pass before discovery: %v, want it to wait", err)
			}
			break wait
		case <-deadline:
			t.Fatal("pass before discovery neither waited nor returned")
		case <-time.After(10 * time.Millisecond):
			f.a.mu.Lock()
			n := len(f.a.targets)
			f.a.mu.Unlock()
			if n != 0 {
				t.Errorf("pass before discovery tracked %d files; load would skip the stored gates", n)
				break wait
			}
		}
	}

	runCtx, cancel := context.WithCancel(ctx)
	ran := make(chan error, 1)
	go func() { ran <- f.a.Run(runCtx) }()
	defer func() {
		cancel()
		<-ran
	}()
	if err := f.a.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.a.stats.Indexed.Load() + f.a.stats.Unchanged.Load(); n != 0 {
		t.Errorf("start with nothing changed opened %d transcripts", n)
	}
}
