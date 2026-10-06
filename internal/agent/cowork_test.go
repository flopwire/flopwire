package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/transcript"
)

const coworkNativeID = "11111111-2222-4333-8444-555555555555"

// All app storage and metadata here are synthetic. No default app roots are read.
func newCoworkFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	store, err := localindex.Open(filepath.Join(home, "index.db"), localindex.Options{DeferCommit: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	f := &fixture{t: t, home: home, store: store, rec: newRecorder()}
	f.cfg = Config{ClaudeProjects: filepath.Join(home, "cli-projects"), CoworkRoot: filepath.Join(home, "cowork"), CodexHome: filepath.Join(home, "codex"), DevinDB: "-", OpencodeDB: "-", OpencodeRegistry: "-", Home: home, Workers: 2, Sync: f.rec, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	f.restart()
	return f
}

func coworkWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func coworkMetadata(t *testing.T, f *fixture, selected, approved, mounts []string) {
	t.Helper()
	md := map[string]any{"sessionId": "app-session", "cliSessionId": coworkNativeID, "cwd": "/sessions/vm-workspace", "userSelectedFolders": selected}
	if selected == nil {
		md["userSelectedFolders"] = []string{}
	}
	if approved != nil {
		md["userApprovedFileAccessPaths"] = approved
	}
	if mounts != nil {
		md["fileDeleteApprovedMounts"] = mounts
	}
	b, err := json.Marshal(md)
	if err != nil {
		t.Fatal(err)
	}
	coworkWrite(t, filepath.Join(f.cfg.CoworkRoot, "account", "workspace", "app-session.json"), string(b))
}

func coworkRecord(session, id, text string) string {
	return fmt.Sprintf(`{"parentUuid":null,"cwd":"/sessions/vm-workspace","sessionId":%q,"type":"user","message":{"role":"user","content":%q},"uuid":%q,"timestamp":"2026-10-06T11:00:00.000Z"}`+"\n", session, text, id)
}

func coworkTranscript(t *testing.T, f *fixture) string {
	t.Helper()
	path := filepath.Join(f.cfg.CoworkRoot, "account", "workspace", "app-session", ".claude", "projects", "-sessions-vm-workspace", coworkNativeID+".jsonl")
	coworkWrite(t, path, coworkRecord(coworkNativeID, "main-message", "cowork main needle"))
	return path
}

func coworkChildren(t *testing.T, path string) (string, string) {
	t.Helper()
	dir := filepath.Join(filepath.Dir(path), coworkNativeID)
	sub := filepath.Join(dir, "subagents", "agent-cafe.jsonl")
	companion := filepath.Join(dir, "tool-results", "result.txt")
	coworkWrite(t, sub, coworkRecord("agent-cafe", "child-message", "cowork child needle"))
	coworkWrite(t, companion, "synthetic tool output")
	return sub, companion
}

func TestCoworkNativeSearchAndProvenance(t *testing.T) {
	f := newCoworkFixture(t)
	coworkMetadata(t, f, []string{filepath.Join(f.home, "host-project")}, nil, nil)
	path := coworkTranscript(t, f)
	f.once()
	if hits := f.find("cowork main needle", false); len(hits) != 1 {
		t.Fatalf("search hits = %d", len(hits))
	}
	if n := f.count(`SELECT count(*) FROM sources s JOIN messages m ON m.source_id=s.id JOIN conversations c ON c.id=m.conversation_id WHERE s.path=? AND s.agent='claude' AND c.session_id=?`, path, coworkNativeID); n != 1 {
		t.Fatalf("native conversation/source provenance rows = %d", n)
	}
	if _, ok := f.rec.spec(path); ok {
		t.Error("Cowork session uploaded before durable server policy support")
	}
	// App metadata contains prompts/config in reality and must never be a companion.
	if n := f.count(`SELECT count(*) FROM sources WHERE path LIKE '%app-session.json'`); n != 0 {
		t.Errorf("metadata indexed as source: %d", n)
	}
	f.restart()
	f.once()
	if hits := f.find("cowork main needle", false); len(hits) != 1 {
		t.Errorf("restart search hits = %d", len(hits))
	}
}

func TestCoworkStrictestHostPathPolicyAndChildren(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		selected, approved, mounts []string
		rules                      []string
		indexed, upload            bool
	}{
		{name: "mapped local until server policy support", selected: []string{"/host/public"}, indexed: true},
		{name: "missing denied host directory", selected: []string{"/host/public", "/host/missing-secret"}, rules: []string{"deny /host/missing-secret"}},
		{name: "approved path local wins", selected: []string{"/host/public"}, approved: []string{"/host/private"}, rules: []string{"local /host/private"}, indexed: true},
		{name: "virtual approved unknown", selected: []string{"/host/public"}, approved: []string{"/sessions/vm-private"}, indexed: true},
		{name: "unresolved deletion mount", selected: []string{"/host/public"}, mounts: []string{"private-mount"}, indexed: true},
		{name: "empty mapping", indexed: true},
		{name: "deny beats unknown", selected: []string{"/host/secret"}, approved: []string{"/sessions/vm-private"}, rules: []string{"deny /host/secret"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCoworkFixture(t)
			f.cfg.UserRuleList = tc.rules
			f.cfg.Unplaceable = "upload" // Unknown app mapping must override this permissive fallback.
			f.restart()
			coworkMetadata(t, f, tc.selected, tc.approved, tc.mounts)
			path := coworkTranscript(t, f)
			sub, companion := coworkChildren(t, path)
			f.once()
			want := 0
			if tc.indexed {
				want = 2
			}
			if n := f.count(`SELECT count(*) FROM messages WHERE superseded=0`); n != want {
				t.Errorf("indexed parent/child messages=%d, want %d", n, want)
			}
			for _, p := range []string{path, sub, companion} {
				if _, ok := f.rec.spec(p); ok != tc.upload {
					t.Errorf("sync eligibility for %s = %v, want %v", filepath.Base(p), ok, tc.upload)
				}
			}
			if !tc.indexed {
				if n := f.count(`SELECT count(*) FROM companions`); n != 0 {
					t.Errorf("denied companion rows=%d", n)
				}
			}
		})
	}
}

func TestCoworkMetadataPolicyChangePurgesAndRestores(t *testing.T) {
	f := newCoworkFixture(t)
	rules := filepath.Join(f.home, "rules")
	writeRules(t, rules, "deny /host/secret")
	f.cfg.UserRules = rules
	f.restart()
	coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
	path := coworkTranscript(t, f)
	coworkChildren(t, path)
	f.once()
	if len(f.find("cowork main needle", false)) != 1 {
		t.Fatal("initial session absent")
	}
	coworkMetadata(t, f, []string{"/host/secret"}, nil, nil)
	f.once()
	if n := f.count(`SELECT count(*) FROM messages`); n != 0 {
		t.Errorf("metadata denial left %d messages", n)
	}
	if n := f.count(`SELECT count(*) FROM companions`); n != 0 {
		t.Errorf("metadata denial left %d companions", n)
	}
	coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
	f.once()
	if n := f.count(`SELECT count(*) FROM messages`); n != 0 {
		t.Errorf("removing historical folder bypassed denial: %d messages", n)
	}
	writeRules(t, rules, "local /host/secret")
	f.once()
	if len(f.find("cowork main needle", false)) != 1 || len(f.find("cowork child needle", false)) != 1 {
		t.Error("loosened rule failed to restore parent/child")
	}
}

func TestCoworkCLIOverlapDeduplicatesAndCannotBypassDeny(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(fmt.Sprint("deny=", denied), func(t *testing.T) {
			f := newCoworkFixture(t)
			host := "/host/public"
			if denied {
				host = "/host/secret"
				f.cfg.UserRuleList = []string{"deny /host/secret"}
			}
			f.restart()
			coworkMetadata(t, f, []string{host}, nil, nil)
			path := coworkTranscript(t, f)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			coworkWrite(t, filepath.Join(f.cfg.ClaudeProjects, "cli-project", coworkNativeID+".jsonl"), string(data))
			f.once()
			want := 1
			if denied {
				want = 0
			}
			if n := f.count(`SELECT count(*) FROM conversations WHERE session_id=?`, coworkNativeID); n != want {
				t.Errorf("conversation count=%d, want %d", n, want)
			}
			if n := f.count(`SELECT count(*) FROM messages WHERE superseded=0`); n != want {
				t.Errorf("live message count=%d, want %d", n, want)
			}
			{
				f.rec.mu.Lock()
				n := len(f.rec.notify)
				f.rec.mu.Unlock()
				if n != 0 {
					t.Errorf("CLI overlap uploaded Cowork-scoped session: %d sources", n)
				}
			}
		})
	}
}

func TestCoworkAbsentContainerAppearsWhileRunning(t *testing.T) {
	f := newCoworkFixture(t)
	f.cfg.Sweep = 40 * time.Millisecond
	f.cfg.FastLane = 40 * time.Millisecond
	f.restart()
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- f.a.Run(runCtx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil && err != context.Canceled {
			t.Errorf("Run: %v", err)
		}
	}()
	waitFor(t, func() bool { return f.a.stats.Sweeps.Load() > 0 })
	coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
	path := coworkTranscript(t, f)
	waitFor(t, func() bool {
		hits, err := f.store.Find(ctx, "cowork main needle", localindex.FindOptions{})
		return err == nil && len(hits) == 1
	})
	f.a.WaitIdle()
	if len(f.find("cowork main needle", false)) != 1 {
		t.Fatal("new container not indexed without restart")
	}
	appendFile(t, path, coworkRecord(coworkNativeID, "append-message", "cowork appended needle"))
	waitFor(t, func() bool {
		hits, err := f.store.Find(ctx, "cowork appended needle", localindex.FindOptions{})
		return err == nil && len(hits) == 1
	})
}

// Persisted scopes protect already queued sync jobs before discovery, including
// after the app removes metadata or the owner disables collection.
func TestCoworkPersistedScopeBlocksQueuedUploadWithoutRoot(t *testing.T) {
	f := newCoworkFixture(t)
	coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
	path := coworkTranscript(t, f)
	sub, companion := coworkChildren(t, path)
	f.once()
	if n := f.count(`SELECT count(*) FROM placements WHERE agent='claude' AND session_id=? AND how='cowork'`, coworkNativeID); n != 1 {
		t.Fatalf("persisted Cowork provenance rows=%d", n)
	}
	if err := os.RemoveAll(f.cfg.CoworkRoot); err != nil {
		t.Fatal(err)
	}
	f.cfg.CoworkRoot = "-"
	f.restart()
	for _, spec := range []devicesync.SourceSpec{
		{Path: path, Agent: transcript.AgentClaude, SessionKey: coworkNativeID},
		{Path: sub, Agent: transcript.AgentClaude, SessionKey: "agent-cafe", Parent: path},
		{Path: companion, Agent: transcript.AgentClaude, SessionKey: coworkNativeID, Parent: path, StorageKind: transcript.StorageCompanion},
	} {
		if f.a.allowUpload(spec) {
			t.Errorf("persisted queued source admitted before discovery: %s", spec.Path)
		}
	}
}

func TestCoworkMetadataOnlyIdentityScopesCLI(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(fmt.Sprintf("deny=%v", denied), func(t *testing.T) {
			f := newCoworkFixture(t)
			host := "/host/public"
			if denied {
				host = "/host/secret"
				f.cfg.UserRuleList = []string{"deny /host/secret"}
			}
			f.restart()
			coworkMetadata(t, f, []string{host}, nil, nil)
			path := filepath.Join(f.cfg.ClaudeProjects, "cli-project", coworkNativeID+".jsonl")
			coworkWrite(t, path, coworkRecord(coworkNativeID, "cli-only-message", "metadata-only CLI needle"))
			f.once()
			want := 1
			if denied {
				want = 0
			}
			if n := len(f.find("metadata-only CLI needle", false)); n != want {
				t.Errorf("CLI indexed count=%d, want %d", n, want)
			}
			if _, ok := f.rec.spec(path); ok {
				t.Error("metadata-only scoped CLI offered to sync")
			}
			if f.a.allowUpload(devicesync.SourceSpec{Path: path, Agent: transcript.AgentClaude, SessionKey: coworkNativeID}) {
				t.Error("metadata-only identity allowed queued CLI upload")
			}
		})
	}
}

func TestCoworkUnknownOriginRemainsAfterKnownMappingAndRestart(t *testing.T) {
	f := newCoworkFixture(t)
	coworkMetadata(t, f, nil, nil, nil)
	path := coworkTranscript(t, f)
	f.once()
	if n := f.count(`SELECT count(*) FROM placements WHERE session_id=? AND how='cowork-unknown'`, coworkNativeID); n != 1 {
		t.Fatalf("unknown origin rows=%d", n)
	}
	coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
	f.once()
	f.restart()
	f.once()
	if n := f.count(`SELECT count(*) FROM placements WHERE session_id=? AND how='cowork-unknown'`, coworkNativeID); n != 1 {
		t.Errorf("known metadata erased historical unknown origin: %d rows", n)
	}
	if f.a.allowUpload(devicesync.SourceSpec{Path: path, Agent: transcript.AgentClaude, SessionKey: coworkNativeID}) {
		t.Error("historical unknown upload admitted after restart")
	}
	if n := len(f.find("cowork main needle", false)); n != 1 {
		t.Errorf("historical unknown local hits=%d", n)
	}
}

func TestCoworkOriginSurvivesHostGitInitializationAndReresolve(t *testing.T) {
	needGit(t)
	f := newCoworkFixture(t)
	host := filepath.Join(f.home, "host-project")
	if err := os.MkdirAll(host, 0700); err != nil {
		t.Fatal(err)
	}
	coworkMetadata(t, f, []string{host}, nil, nil)
	path := coworkTranscript(t, f)
	cli := filepath.Join(f.cfg.ClaudeProjects, "cli-project", coworkNativeID+".jsonl")
	coworkWrite(t, cli, coworkRecord(coworkNativeID, "main-message", "cowork main needle"))
	f.once()
	gitRun(t, host, "init", "-q")
	if err := os.RemoveAll(f.cfg.CoworkRoot); err != nil {
		t.Fatal(err)
	}
	f.cfg.CoworkRoot = "-"
	f.restart()
	f.once()
	f.a.reresolve()
	if n := f.count(`SELECT count(*) FROM placements WHERE session_id=? AND how='cowork'`, coworkNativeID); n != 1 {
		t.Errorf("reresolve erased app origin: %d rows", n)
	}
	for _, p := range []string{path, cli} {
		if f.a.allowUpload(devicesync.SourceSpec{Path: p, Agent: transcript.AgentClaude, SessionKey: coworkNativeID}) {
			t.Errorf("reresolve admitted scoped source: %s", p)
		}
	}
	if n := len(f.find("cowork main needle", false)); n != 1 {
		t.Errorf("overlap search hits after root removal=%d", n)
	}
}

func TestCoworkOrphanCompanionRestoresAfterDenyLoosened(t *testing.T) {
	f := newCoworkFixture(t)
	rules := filepath.Join(f.home, "rules")
	writeRules(t, rules, "local /host/project")
	f.cfg.UserRules = rules
	f.restart()
	coworkMetadata(t, f, []string{"/host/project"}, nil, nil)
	path := coworkTranscript(t, f)
	_, companion := coworkChildren(t, path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	f.once()
	if n := f.count(`SELECT count(*) FROM companions p JOIN conversations c ON c.id=p.conversation_id WHERE p.path=? AND c.session_id=?`, companion, coworkNativeID); n != 1 {
		t.Fatalf("initial orphan companion linkage=%d", n)
	}
	// Exclude file-timestamp races as a reason the companion is rescanned.
	waitRacy()
	f.once()
	writeRules(t, rules, "deny /host/project")
	f.once()
	if n := f.count(`SELECT count(*) FROM companions`); n != 0 {
		t.Fatalf("denied orphan companions=%d", n)
	}
	writeRules(t, rules, "local /host/project")
	f.once()
	if n := f.count(`SELECT count(*) FROM companions p JOIN conversations c ON c.id=p.conversation_id WHERE p.path=? AND c.session_id=?`, companion, coworkNativeID); n != 1 {
		t.Errorf("restored orphan companion linkage=%d", n)
	}
	if n := f.count(`SELECT count(*) FROM companions p LEFT JOIN conversations c ON c.id=p.conversation_id WHERE p.conversation_id IS NULL OR c.id IS NULL`); n != 0 {
		t.Errorf("restored dangling companions=%d", n)
	}
}

func TestCoworkNilSyncRestoresUnchangedFilesAfterMetadataDeny(t *testing.T) {
	f := newCoworkFixture(t)
	rules := filepath.Join(f.home, "rules")
	writeRules(t, rules, "deny /host/secret")
	f.cfg.UserRules = rules
	f.cfg.Sync = nil
	f.restart()
	coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
	path := coworkTranscript(t, f)
	coworkChildren(t, path)
	f.once()
	// Once beyond the racy window, settle a real unchanged gate first.
	waitRacy()
	f.once()
	coworkMetadata(t, f, []string{"/host/secret"}, nil, nil)
	f.once()
	if n := f.count(`SELECT count(*) FROM messages`); n != 0 {
		t.Fatalf("metadata denial with nil Sync left %d messages", n)
	}
	writeRules(t, rules, "local /host/secret")
	f.once()
	if len(f.find("cowork main needle", false)) != 1 || len(f.find("cowork child needle", false)) != 1 {
		t.Error("nil Sync failed to reindex unchanged denied files after rule loosened")
	}
}

func TestCoworkStalePlacementCannotEraseDurableScope(t *testing.T) {
	f := newCoworkFixture(t)
	f.cfg.UserRuleList = []string{"deny /host/secret"}
	f.restart()
	coworkMetadata(t, f, []string{"/host/secret"}, nil, nil)
	path := coworkTranscript(t, f)
	f.once()
	key := placeKey{transcript.AgentClaude, coworkNativeID}
	// Model a CLI worker that sampled VM placement before Cowork registration,
	// then completes its save after host scope registration.
	stale := placed{pl: pathpolicy.Placement{Cwd: "/sessions/stale-vm"}, how: localindex.PlacedByCwd}
	f.a.savePlace(key, stale)
	p, ok := f.a.storedPlace(key)
	if !ok || !localindex.IsCoworkPlacement(p.how) || f.a.decide(f.a.policy().pol, p).Mode != pathpolicy.Deny {
		t.Fatal("stale save erased Cowork host denial")
	}
	if err := os.RemoveAll(f.cfg.CoworkRoot); err != nil {
		t.Fatal(err)
	}
	f.cfg.CoworkRoot = "-"
	f.restart()
	p, ok = f.a.storedPlace(key)
	if !ok || !localindex.IsCoworkPlacement(p.how) || f.a.decide(f.a.policy().pol, p).Mode != pathpolicy.Deny {
		t.Fatal("durable scope lost on restart")
	}
	if f.a.allowUpload(devicesync.SourceSpec{Path: path, Agent: transcript.AgentClaude, SessionKey: coworkNativeID}) {
		t.Fatal("stale save released upload")
	}
}

func TestCoworkStatusReportsMissingIdentityMapping(t *testing.T) {
	f := newCoworkFixture(t)
	coworkTranscript(t, f)
	f.once()
	st := f.a.coworkStatus()
	if st.Sessions != 1 || st.Unknown != 1 || st.HistoricalUnknown != 1 || st.Held != 1 || len(st.MappingReasons) != 1 {
		t.Fatalf("missing metadata status: %+v", st)
	}
	if st.SharedHold == "" || st.WatchCandidates == 0 {
		t.Fatalf("coverage/readiness missing: %+v", st)
	}
}

func TestCoworkMetadataOnlyUnknownDoesNotTaintFirstKnownContent(t *testing.T) {
	for _, emptyTranscript := range []bool{false, true} {
		t.Run(fmt.Sprint("zero_byte=", emptyTranscript), func(t *testing.T) {
			f := newCoworkFixture(t)
			coworkMetadata(t, f, nil, nil, nil)
			if emptyTranscript {
				path := coworkTranscript(t, f)
				coworkWrite(t, path, "")
			}
			f.once()
			if st := f.a.coworkStatus(); st.HistoricalUnknown != 0 || st.Unknown != 1 {
				t.Fatalf("metadata-only scope tainted: %+v", st)
			}
			coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
			coworkTranscript(t, f)
			f.once()
			if st := f.a.coworkStatus(); st.HistoricalUnknown != 0 {
				t.Fatalf("first mapped content tainted: %+v", st)
			}
			if len(f.find("cowork main needle", false)) != 1 {
				t.Fatal("first content missing")
			}
		})
	}
}

func TestCoworkLegacyCLIBytesRemainUnknownAfterKnownMetadata(t *testing.T) {
	f := newCoworkFixture(t)
	path := filepath.Join(f.cfg.ClaudeProjects, "cli-project", coworkNativeID+".jsonl")
	coworkWrite(t, path, coworkRecord(coworkNativeID, "legacy-message", "legacy CLI needle"))
	f.once()
	coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
	f.once()
	if st := f.a.coworkStatus(); st.HistoricalUnknown != 1 || st.Unknown != 0 {
		t.Fatalf("legacy evidence granted proof by current metadata: %+v", st)
	}
}

func TestCoworkTemporaryMissingMetadataTaintsOnlyNewBytes(t *testing.T) {
	f := newCoworkFixture(t)
	coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
	path := coworkTranscript(t, f)
	f.once()
	waitRacy()
	f.once()
	if err := os.Remove(filepath.Join(f.cfg.CoworkRoot, "account", "workspace", "app-session.json")); err != nil {
		t.Fatal(err)
	}
	f.once()
	if st := f.a.coworkStatus(); st.HistoricalUnknown != 0 || st.Unknown != 1 {
		t.Fatalf("unchanged mapped bytes tainted: %+v", st)
	}
	appendFile(t, path, coworkRecord(coworkNativeID, "unknown-append", "unknown appended needle"))
	f.once()
	if st := f.a.coworkStatus(); st.HistoricalUnknown != 1 {
		t.Fatalf("unknown append not tainted: %+v", st)
	}
}

func TestCoworkTaintPersistenceFailureRetriesBeforeIndexing(t *testing.T) {
	f := newCoworkFixture(t)
	coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
	path := coworkTranscript(t, f)
	f.once()
	if err := os.Remove(filepath.Join(f.cfg.CoworkRoot, "account", "workspace", "app-session.json")); err != nil {
		t.Fatal(err)
	}
	f.a.refreshCowork(ctx)
	appendFile(t, path, coworkRecord(coworkNativeID, "new-unknown", "retry unknown needle"))
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", f.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_cowork_taint BEFORE UPDATE OF how ON placements WHEN NEW.how='cowork-unknown' BEGIN SELECT RAISE(FAIL,'synthetic taint failure'); END`); err != nil {
		t.Fatal(err)
	}
	f.a.mu.Lock()
	target := f.a.targets[path]
	f.a.mu.Unlock()
	if _, err := f.a.indexTranscript(ctx, target); err == nil {
		t.Fatal("unknown bytes indexed despite persistence rejection")
	}
	p, _ := f.a.storedPlace(placeKey{transcript.AgentClaude, coworkNativeID})
	if p.how != localindex.PlacedByCowork {
		t.Fatal("failed durable taint published to memory and would skip retry")
	}
	if len(f.find("retry unknown needle", false)) != 0 {
		t.Fatal("unknown appended content indexed before marker persisted")
	}
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_cowork_taint`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.indexTranscript(ctx, target); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	f.restart()
	p, _ = f.a.storedPlace(placeKey{transcript.AgentClaude, coworkNativeID})
	if p.how != localindex.PlacedByCoworkUnknown || len(f.find("retry unknown needle", false)) != 1 {
		t.Fatal("retry did not persist taint and content across restart")
	}
}

type coworkBarrierParser struct {
	transcript.Parser
	entered chan struct{}
	release chan struct{}
}

func (p *coworkBarrierParser) Parse(c context.Context, in transcript.Input, cur transcript.Cursor, sink transcript.Sink) (transcript.Cursor, error) {
	close(p.entered)
	select {
	case <-p.release:
	case <-c.Done():
		return transcript.Cursor{}, c.Err()
	}
	return p.Parser.Parse(c, in, cur, sink)
}

func TestCoworkRegistrationWaitsForPreproofCLIIndex(t *testing.T) {
	f := newCoworkFixture(t)
	path := filepath.Join(f.cfg.ClaudeProjects, "cli-project", coworkNativeID+".jsonl")
	coworkWrite(t, path, coworkRecord(coworkNativeID, "inflight-message", "inflight CLI needle"))
	barrier := &coworkBarrierParser{Parser: f.a.claude, entered: make(chan struct{}), release: make(chan struct{})}
	f.a.claude = barrier
	indexed := make(chan error, 1)
	go func() { indexed <- f.a.Once(ctx) }()
	select {
	case <-barrier.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("CLI index never reached capture barrier")
	}
	coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
	registered := make(chan struct{})
	go func() { f.a.refreshCowork(ctx); close(registered) }()
	select {
	case <-registered:
		t.Fatal("scope registration overtook in-flight preproof index")
	case <-time.After(50 * time.Millisecond):
	}
	close(barrier.release)
	select {
	case err := <-indexed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked CLI index")
	}
	select {
	case <-registered:
	case <-time.After(5 * time.Second):
		t.Fatal("blocked scope registration")
	}
	if st := f.a.coworkStatus(); st.HistoricalUnknown != 1 {
		t.Fatalf("in-flight preproof bytes granted mapping: %+v", st)
	}
}

func TestCoworkRegistrationFailureHoldsAndRetries(t *testing.T) {
	f := newCoworkFixture(t)
	coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
	coworkTranscript(t, f)
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", f.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_cowork_registration BEFORE INSERT ON placements WHEN NEW.how LIKE 'cowork%' BEGIN SELECT RAISE(FAIL,'synthetic registration failure'); END`); err != nil {
		t.Fatal(err)
	}
	f.once()
	if _, ok := f.a.storedPlace(placeKey{transcript.AgentClaude, coworkNativeID}); ok {
		t.Fatal("uncommitted registration published")
	}
	if len(f.find("cowork main needle", false)) != 0 {
		t.Fatal("evidence captured before policy registration")
	}
	if _, err := db.Exec(`DROP TRIGGER fail_cowork_registration`); err != nil {
		t.Fatal(err)
	}
	f.once()
	f.restart()
	p, ok := f.a.storedPlace(placeKey{transcript.AgentClaude, coworkNativeID})
	if !ok || p.how != localindex.PlacedByCowork || len(f.find("cowork main needle", false)) != 1 {
		t.Fatal("registration retry lost proof or content")
	}
}

func TestCoworkCompanionProofSurvivesGateReset(t *testing.T) {
	f := newCoworkFixture(t)
	coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
	path := coworkTranscript(t, f)
	_, companion := coworkChildren(t, path)
	f.once()
	if err := os.Remove(filepath.Join(f.cfg.CoworkRoot, "account", "workspace", "app-session.json")); err != nil {
		t.Fatal(err)
	}
	f.a.refreshCowork(ctx)
	f.a.enforce(ctx, f.a.policy().pol)
	f.once()
	if st := f.a.coworkStatus(); st.HistoricalUnknown != 0 {
		t.Fatalf("unchanged companion lost durable proof: %+v", st)
	}
	data, err := os.ReadFile(companion)
	if err != nil {
		t.Fatal(err)
	}
	for i := range data {
		data[i] = 'x'
	}
	if err := os.WriteFile(companion, data, 0600); err != nil {
		t.Fatal(err)
	}
	f.a.enforce(ctx, f.a.policy().pol)
	f.once()
	if st := f.a.coworkStatus(); st.HistoricalUnknown == 0 {
		t.Fatalf("same-size unknown rewrite escaped taint: %+v", st)
	}
}

func TestCoworkFailedScopeExpansionCannotCaptureNewBytes(t *testing.T) {
	for _, mode := range []string{"local", "deny", "deny-metadata-only"} {
		t.Run(mode, func(t *testing.T) {
			f := newCoworkFixture(t)
			rules := filepath.Join(f.home, "rules")
			ruleMode := mode
			if mode == "deny-metadata-only" {
				ruleMode = "deny"
			}
			writeRules(t, rules, ruleMode+" /host/secret")
			f.cfg.UserRules = rules
			f.restart()
			coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
			path := coworkTranscript(t, f)
			f.once()
			coworkMetadata(t, f, []string{"/host/public", "/host/secret"}, nil, nil)
			appendFile(t, path, coworkRecord(coworkNativeID, "new-scope", "new scope needle"))
			if mode == "deny-metadata-only" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.store.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", f.store.Path())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`CREATE TRIGGER fail_scope_expansion BEFORE INSERT ON placements WHEN NEW.how LIKE 'cowork%' BEGIN SELECT RAISE(FAIL,'synthetic scope expansion failure'); END`); err != nil {
				t.Fatal(err)
			}
			f.once()
			if len(f.find("new scope needle", false)) != 0 {
				t.Fatal("new bytes captured with incomplete durable folder union")
			}
			if ruleMode == "deny" && len(f.find("cowork main needle", false)) != 0 {
				t.Fatal("current denied folder failed to purge earlier session evidence")
			}
			if err := f.store.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`DROP TRIGGER fail_scope_expansion`); err != nil {
				t.Fatal(err)
			}
			f.once()
			f.restart()
			p, _ := f.a.storedPlace(placeKey{transcript.AgentClaude, coworkNativeID})
			found := p.pl.Cwd == "/host/secret"
			for _, other := range decodeOthers(p.others) {
				found = found || other.Cwd == "/host/secret"
			}
			if !found {
				t.Fatal("successful retry did not persist added scope")
			}
			if mode == "local" && len(f.find("new scope needle", false)) != 1 {
				t.Fatal("known local bytes failed after registration retry")
			}
		})
	}
}

func TestCoworkSelectedSubtreeStrictestHostPolicy(t *testing.T) {
	for _, rule := range []string{"deny /host/Code/private", "deny /host/Code/p*/secrets", "local /host/Code/private", "deny repo:example.com/private/*"} {
		t.Run(rule, func(t *testing.T) {
			f := newCoworkFixture(t)
			rules := filepath.Join(f.home, "rules")
			writeRules(t, rules, rule)
			f.cfg.UserRules = rules
			f.restart()
			coworkMetadata(t, f, []string{"/host/Code"}, []string{"/sessions/unresolved"}, nil)
			path := coworkTranscript(t, f)
			coworkChildren(t, path)
			f.once()
			expectDeny := rule[:4] == "deny" && rule != "deny repo:example.com/private/*"
			if (len(f.find("cowork main needle", false)) == 0) != expectDeny {
				t.Fatalf("selected subtree ignored rule %q", rule)
			}
			if expectDeny && len(f.find("cowork child needle", false)) != 0 {
				t.Fatal("child escaped selected subtree deny")
			}
			f.rec.mu.Lock()
			notifications := len(f.rec.notify)
			f.rec.mu.Unlock()
			if notifications != 0 {
				t.Fatal("Cowork subtree restrictions admitted shared evidence")
			}
			if rule == "deny repo:example.com/private/*" && f.a.coworkStatus().RepositoryScopeUnknown == 0 {
				t.Fatal("unverified nested repos falsely reported known")
			}
		})
	}
}

func TestCoworkPhysicalSelectedSubtreePolicyPersists(t *testing.T) {
	f := newCoworkFixture(t)
	physical := filepath.Join(f.home, "physical")
	logical := filepath.Join(f.home, "alias")
	if err := os.MkdirAll(physical, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(physical, logical); err != nil {
		t.Fatal(err)
	}
	rules := filepath.Join(f.home, "rules")
	writeRules(t, rules, "deny "+filepath.Join(physical, "private"))
	f.cfg.UserRules = rules
	f.restart()
	coworkMetadata(t, f, []string{logical}, nil, nil)
	coworkTranscript(t, f)
	f.once()
	if len(f.find("cowork main needle", false)) != 0 {
		t.Fatal("logical selected root escaped physical nested deny")
	}
	if err := os.Remove(logical); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.cfg.CoworkRoot, "account", "workspace", "app-session.json")); err != nil {
		t.Fatal(err)
	}
	f.restart()
	p, _ := f.a.storedPlace(placeKey{transcript.AgentClaude, coworkNativeID})
	canonicalPhysical := physicalPath(physical)
	found := p.pl.Cwd == canonicalPhysical
	for _, other := range decodeOthers(p.others) {
		found = found || other.Cwd == canonicalPhysical
	}
	if !found {
		t.Fatal("physical host scope not durable after alias/metadata removal")
	}
	f.once()
	if len(f.find("cowork main needle", false)) != 0 {
		t.Fatal("historical physical subtree deny lost on restart")
	}
}
