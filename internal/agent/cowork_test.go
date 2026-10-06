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
	f.cfg = Config{ClaudeProjects: filepath.Join(home, "cli-projects"), DesktopCodeRoot: "-", CoworkRoot: filepath.Join(home, "cowork"), CodexHome: filepath.Join(home, "codex"), DevinDB: "-", OpencodeDB: "-", OpencodeRegistry: "-", Home: home, Workers: 2, Sync: f.rec, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
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

func TestCoworkHistoricalUnknownUnplaceableFloorAndParentInheritance(t *testing.T) {
	for _, admin := range []bool{false, true} {
		t.Run(fmt.Sprint("admin=", admin), func(t *testing.T) {
			f := newCoworkFixture(t)
			for _, entry := range []struct{ id, how string }{{coworkNativeID, localindex.PlacedByCoworkUnknown}, {"agent-cafe", localindex.PlacedByCowork}} {
				if err := f.a.saveCoworkPlace(context.Background(), placeKey{transcript.AgentClaude, entry.id}, placed{pl: pathpolicy.Placement{Cwd: "/host/public"}, how: entry.how}); err != nil {
					t.Fatal(err)
				}
			}
			childPath := "/synthetic/agent-cafe.jsonl"
			child := &target{path: childPath, src: transcript.Source{Agent: transcript.AgentClaude, SessionKey: "agent-cafe"}, root: coworkNativeID}
			f.a.mu.Lock()
			f.a.targets[childPath] = child
			f.a.mu.Unlock()
			companion := &target{kind: kindCompanion, path: "/synthetic/result.txt", owner: "agent-cafe", parent: childPath}
			main := &target{path: "/synthetic/cli-overlap.jsonl", src: transcript.Source{Agent: transcript.AgentClaude, SessionKey: coworkNativeID}}
			pv := &policyView{pol: pathpolicy.Policy{Unplaceable: pathpolicy.Deny, UnplaceableAdmin: admin}}
			for _, target := range []*target{main, child, companion} {
				d, ok := f.a.coworkMode(pv, target)
				if !ok || d.Mode != pathpolicy.Deny || !d.Unplaceable || d.Admin != admin {
					t.Fatalf("historical group floor: %+v %v", d, ok)
				}
			}
			pv.pol.User, _ = pathpolicy.ParseRules([]string{"deny /host/public"})
			if d, _ := f.a.coworkMode(pv, child); d.Mode != pathpolicy.Deny || d.Unplaceable || d.Rule.Pattern != "/host/public" {
				t.Fatalf("actual deny reason weakened: %+v", d)
			}
		})
	}
}

func TestCoworkHistoricalParentTaintsNewKnownChildren(t *testing.T) {
	f := newCoworkFixture(t)
	coworkMetadata(t, f, nil, nil, nil)
	path := coworkTranscript(t, f)
	f.once()
	coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
	sub, companion := coworkChildren(t, path)
	cli := filepath.Join(f.cfg.ClaudeProjects, "cli-project", coworkNativeID+".jsonl")
	coworkWrite(t, cli, coworkRecord(coworkNativeID, "main-message", "cowork main needle"))
	f.cfg.Unplaceable = "exclude"
	f.restart()
	f.once()
	for _, id := range []string{coworkNativeID, "agent-cafe"} {
		if n := f.count(`SELECT count(*) FROM placements WHERE session_id=? AND how='cowork-unknown'`, id); n != 1 {
			t.Fatalf("group member %s not historically unknown", id)
		}
	}
	if n := f.count(`SELECT count(*) FROM messages`); n != 0 {
		t.Fatalf("historical deny indexed %d messages", n)
	}
	if n := f.count(`SELECT count(*) FROM companions`); n != 0 {
		t.Fatalf("historical deny retained %d companions", n)
	}
	for _, p := range []string{path, sub, companion, cli} {
		if f.a.allowUpload(devicesync.SourceSpec{Path: p, Agent: transcript.AgentClaude, SessionKey: coworkNativeID}) {
			t.Fatalf("group evidence uploaded %s", p)
		}
	}
	f.restart()
	f.once()
	if n := f.count(`SELECT count(*) FROM messages`); n != 0 {
		t.Fatalf("restart indexed denied group: %d", n)
	}
}

func TestCoworkLegacyOrphanChildTaintsKnownParentWithoutBytes(t *testing.T) {
	f := newCoworkFixture(t)
	cliMain := filepath.Join(f.cfg.ClaudeProjects, "cli-project", coworkNativeID+".jsonl")
	cliChild, _ := coworkChildren(t, cliMain)
	// Only a native child transcript existed before app scope was available.
	if err := os.Remove(filepath.Join(filepath.Dir(cliMain), coworkNativeID, "tool-results", "result.txt")); err != nil {
		t.Fatal(err)
	}
	f.once()
	if have, err := f.store.SessionHasEvidence(context.Background(), transcript.AgentClaude, coworkNativeID); err != nil || have {
		t.Fatalf("parent unexpectedly has evidence: %v %v", have, err)
	}
	if have, err := f.store.SessionHasEvidence(context.Background(), transcript.AgentClaude, "agent-cafe"); err != nil || !have {
		t.Fatalf("legacy child evidence missing: %v %v", have, err)
	}
	coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
	appMain := coworkTranscript(t, f)
	coworkChildren(t, appMain)
	if err := os.Remove(appMain); err != nil {
		t.Fatal(err)
	}
	f.cfg.Unplaceable = "exclude"
	f.restart()
	f.once()
	for _, id := range []string{coworkNativeID, "agent-cafe"} {
		if n := f.count(`SELECT count(*) FROM placements WHERE session_id=? AND how='cowork-unknown'`, id); n != 1 {
			t.Fatalf("orphan group member %s not unknown", id)
		}
	}
	if n := f.count(`SELECT count(*) FROM messages`); n != 0 {
		t.Fatalf("legacy child retained %d messages", n)
	}
	coworkTranscript(t, f)
	f.restart()
	f.once()
	if n := f.count(`SELECT count(*) FROM messages`); n != 0 {
		t.Fatalf("new parent bytes granted known scope: %d", n)
	}
	if f.a.allowUpload(devicesync.SourceSpec{Path: cliChild, Agent: transcript.AgentClaude, SessionKey: "agent-cafe"}) {
		t.Fatal("CLI child overlap uploaded")
	}
}

func TestCoworkMetadataOnlyParentFindsHistoricalCLIChild(t *testing.T) {
	for _, removeFiles := range []bool{false, true} {
		t.Run(fmt.Sprint("deleted=", removeFiles), func(t *testing.T) {
			f := newCoworkFixture(t)
			cliMain := filepath.Join(f.cfg.ClaudeProjects, "cli-project", coworkNativeID+".jsonl")
			cliChild, _ := coworkChildren(t, cliMain)
			if err := os.Remove(filepath.Join(filepath.Dir(cliMain), coworkNativeID, "tool-results", "result.txt")); err != nil {
				t.Fatal(err)
			}
			f.once()
			if removeFiles {
				if err := os.RemoveAll(f.cfg.ClaudeProjects); err != nil {
					t.Fatal(err)
				}
			}
			coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
			f.cfg.Unplaceable = "exclude"
			f.restart()
			f.once()
			for _, id := range []string{coworkNativeID, "agent-cafe"} {
				if n := f.count(`SELECT count(*) FROM placements WHERE session_id=? AND how='cowork-unknown'`, id); n != 1 {
					t.Fatalf("metadata-only root lost historical member %s", id)
				}
			}
			if n := f.count(`SELECT count(*) FROM messages`); n != 0 {
				t.Fatalf("retained denied old child %d", n)
			}
			coworkTranscript(t, f)
			f.restart()
			f.once()
			if n := f.count(`SELECT count(*) FROM messages`); n != 0 {
				t.Fatalf("parent escaped orphan child taint %d", n)
			}
			if f.a.allowUpload(devicesync.SourceSpec{Path: cliChild, Agent: transcript.AgentClaude, SessionKey: "agent-cafe"}) {
				t.Fatal("historical CLI child offered to sync")
			}
		})
	}
}

func TestCoworkMetadataOnlyParentFindsPendingDeletedCLIChild(t *testing.T) {
	f := newCoworkFixture(t)
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", f.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	path := filepath.Join(f.cfg.ClaudeProjects, "cli-project", coworkNativeID, "subagents", "agent-cafe.jsonl")
	spec, err := json.Marshal(devicesync.SourceSpec{Path: path, Agent: transcript.AgentClaude, StorageKind: transcript.StorageJSONLAppend, Parser: "claude@1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{`CREATE TABLE devsync_sources(id INTEGER PRIMARY KEY,path TEXT,spec TEXT,generation INTEGER)`, `CREATE TABLE devsync_gens(source_id INTEGER,generation INTEGER,size INTEGER,closed INTEGER,acked INTEGER)`} {
		if _, err = db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec(`INSERT INTO devsync_sources VALUES(1,?,?,3)`, path, string(spec)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO devsync_gens VALUES(1,1,9,1,1)`); err != nil {
		t.Fatal(err)
	}
	coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
	f.cfg.Unplaceable = "exclude"
	f.restart()
	f.once()
	for _, id := range []string{coworkNativeID, "agent-cafe"} {
		if n := f.count(`SELECT count(*) FROM placements WHERE session_id=? AND how='cowork-unknown'`, id); n != 1 {
			t.Fatalf("pending deleted child did not taint %s", id)
		}
	}
	coworkTranscript(t, f)
	f.restart()
	f.once()
	if n := f.count(`SELECT count(*) FROM messages`); n != 0 {
		t.Fatalf("pending child granted new parent scope %d", n)
	}
}

func TestCoworkKnownMetadataOnlyParentRegistersFreshCLIChild(t *testing.T) {
	f := newCoworkFixture(t)
	coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
	f.once()
	cliMain := filepath.Join(f.cfg.ClaudeProjects, "cli-project", coworkNativeID+".jsonl")
	child, _ := coworkChildren(t, cliMain)
	if err := os.Remove(filepath.Join(filepath.Dir(cliMain), coworkNativeID, "tool-results", "result.txt")); err != nil {
		t.Fatal(err)
	}
	f.once()
	if hits := f.find("cowork child needle", false); len(hits) != 1 {
		t.Fatalf("controlled CLI child missing %d", len(hits))
	}
	if p, _ := f.a.storedPlace(placeKey{transcript.AgentClaude, "agent-cafe"}); p.how != localindex.PlacedByCowork {
		t.Fatalf("fresh controlled CLI child incorrectly historical %+v", p)
	}
	if f.a.allowUpload(devicesync.SourceSpec{Path: child, Agent: transcript.AgentClaude, SessionKey: "agent-cafe"}) {
		t.Fatal("controlled child uploaded before server support")
	}
}

func TestCoworkMetadataOnlyParentDoesNotRelinkUnrelatedCLIChildID(t *testing.T) {
	f := newCoworkFixture(t)
	otherParent := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	otherChild := filepath.Join(f.cfg.ClaudeProjects, "cli-project", otherParent, "subagents", "agent-cafe.jsonl")
	coworkWrite(t, otherChild, coworkRecord("agent-cafe", "unrelated-child", "unrelated child needle"))
	f.once()
	coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
	f.once()
	if p, _ := f.a.storedPlace(placeKey{transcript.AgentClaude, coworkNativeID}); p.how != localindex.PlacedByCowork {
		t.Fatalf("global child ID relinked to unrelated parent %+v", p)
	}
	if p, _ := f.a.storedPlace(placeKey{transcript.AgentClaude, "agent-cafe"}); localindex.IsCoworkPlacement(p.how) {
		t.Fatalf("unrelated child inherited app scope %+v", p)
	}
	coworkTranscript(t, f)
	f.once()
	if n := len(f.find("cowork main needle", false)); n != 1 {
		t.Fatalf("unrelated child tainted new parent %d", n)
	}
}

func TestCoworkCLIEnumerationErrorHoldsWithoutHistoricalTaint(t *testing.T) {
	f := newCoworkFixture(t)
	f.cfg.Unplaceable = "exclude"
	f.restart()
	coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
	f.a.refreshCowork(ctx)
	coworkWrite(t, f.cfg.ClaudeProjects, "synthetic file replacing directory")
	f.a.refreshCowork(ctx)
	key := placeKey{transcript.AgentClaude, coworkNativeID}
	if p, _ := f.a.storedPlace(key); p.how != localindex.PlacedByCowork {
		t.Fatal("enumeration error permanently tainted metadata-only scope")
	}
	target := &target{path: "/synthetic/cli.jsonl", src: transcript.Source{Agent: transcript.AgentClaude, SessionKey: coworkNativeID}}
	if _, known := f.a.coworkCaptureScope(target); known {
		t.Fatal("failed enumeration reported ready")
	}
	if d, _ := f.a.coworkMode(f.a.policy(), target); d.Mode != pathpolicy.Local {
		t.Fatalf("current error invented historical exclude %+v", d)
	}
	if err := f.a.taintCoworkEvidence(ctx, target); err == nil {
		t.Fatal("current registration error allowed capture")
	}
	if err := os.Remove(f.cfg.ClaudeProjects); err != nil {
		t.Fatal(err)
	}
	f.a.refreshCowork(ctx)
	if _, known := f.a.coworkCaptureScope(target); !known {
		t.Fatal("recovered current scope stayed held")
	}
}

func TestCoworkPendingHistoricalUpgradeFailureHoldsUnchangedScope(t *testing.T) {
	f := newCoworkFixture(t)
	coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
	f.a.refreshCowork(ctx)
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", f.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{`CREATE TABLE devsync_sources(id INTEGER PRIMARY KEY,path TEXT,spec TEXT,generation INTEGER)`, `CREATE TABLE devsync_gens(source_id INTEGER,generation INTEGER,size INTEGER)`, `CREATE TRIGGER fail_historical_upgrade BEFORE UPDATE OF how ON placements WHEN NEW.how='cowork-unknown' BEGIN SELECT RAISE(FAIL,'synthetic unknown upgrade failure'); END`} {
		if _, err = db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(f.cfg.ClaudeProjects, "project", coworkNativeID, "subagents", "agent-cafe.jsonl")
	spec := `{"Agent":"claude","Parser":"claude@1","StorageKind":"jsonl_append","SessionKey":""}`
	if _, err = db.Exec(`INSERT INTO devsync_sources VALUES(1,?,?,3)`, path, spec); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO devsync_gens VALUES(1,1,9)`); err != nil {
		t.Fatal(err)
	}
	f.a.refreshCowork(ctx)
	if p, _ := f.a.storedPlace(placeKey{transcript.AgentClaude, coworkNativeID}); p.how != localindex.PlacedByCowork {
		t.Fatal("failed unknown save published")
	}
	target := &target{path: "/synthetic/cli.jsonl", src: transcript.Source{Agent: transcript.AgentClaude, SessionKey: coworkNativeID}}
	pv := &policyView{pol: pathpolicy.Policy{Unplaceable: pathpolicy.Deny}}
	if d, _ := f.a.coworkMode(pv, target); d.Mode != pathpolicy.Deny {
		t.Fatal("pending unknown upgrade lost exclude floor")
	}
	f.a.mu.Lock()
	f.a.coworkError = ""
	f.a.mu.Unlock()
	if err = f.a.taintCoworkEvidence(ctx, target); err == nil {
		t.Fatal("unchanged registered paths bypassed pending unknown save")
	}
	if _, err = db.Exec(`DROP TRIGGER fail_historical_upgrade`); err != nil {
		t.Fatal(err)
	}
	f.a.refreshCowork(ctx)
	if p, _ := f.a.storedPlace(placeKey{transcript.AgentClaude, coworkNativeID}); p.how != localindex.PlacedByCoworkUnknown {
		t.Fatal("retry failed historical upgrade")
	}
}

func TestCoworkSharedVerifiedChildGroupUnknownFixedPoint(t *testing.T) {
	f := newCoworkFixture(t)
	coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
	first := coworkTranscript(t, f)
	coworkChildren(t, first)
	secondID := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	base := filepath.Join(f.cfg.CoworkRoot, "account", "workspace")
	md, err := json.Marshal(map[string]any{"sessionId": "second", "cliSessionId": secondID, "userSelectedFolders": []string{"/host/public"}})
	if err != nil {
		t.Fatal(err)
	}
	coworkWrite(t, filepath.Join(base, "second.json"), string(md))
	project := filepath.Join(base, "second", ".claude", "projects", "project")
	coworkWrite(t, filepath.Join(project, secondID+".jsonl"), coworkRecord(secondID, "second", "second parent"))
	coworkWrite(t, filepath.Join(project, secondID, "subagents", "agent-cafe.jsonl"), coworkRecord("agent-cafe", "shared", "shared child"))
	if err = f.a.saveCoworkPlace(ctx, placeKey{transcript.AgentClaude, coworkNativeID}, placed{pl: pathpolicy.Placement{Cwd: "/host/public"}, how: localindex.PlacedByCoworkUnknown}); err != nil {
		t.Fatal(err)
	}
	f.a.refreshCowork(ctx)
	for _, id := range []string{coworkNativeID, secondID, "agent-cafe"} {
		if p, _ := f.a.storedPlace(placeKey{transcript.AgentClaude, id}); p.how != localindex.PlacedByCoworkUnknown {
			t.Fatalf("connected group %s not unknown on first refresh", id)
		}
	}
}

func TestCoworkFirstUnknownNonemptyEvidenceRechecksExclude(t *testing.T) {
	for _, admin := range []bool{false, true} {
		for _, companionOnly := range []bool{false, true} {
			t.Run(fmt.Sprintf("admin=%v/companion=%v", admin, companionOnly), func(t *testing.T) {
				f := newCoworkFixture(t)
				f.cfg.Unplaceable = "exclude"
				if admin {
					f.cfg.Unplaceable = "upload"
					f.cfg.AdminRules = func(context.Context) (AdminPolicy, error) { return AdminPolicy{Unplaceable: "exclude"}, nil }
				}
				f.restart()
				coworkMetadata(t, f, []string{"/host/public"}, []string{"/sessions/unknown"}, nil)
				path := coworkTranscript(t, f)
				if companionOnly {
					coworkWrite(t, path, "")
					coworkWrite(t, filepath.Join(filepath.Dir(path), coworkNativeID, "tool-results", "result.txt"), "first unknown companion bytes")
				}
				f.once()
				if n := f.count(`SELECT count(*) FROM messages`); n != 0 {
					t.Fatalf("stale local decision indexed %d messages", n)
				}
				if n := f.count(`SELECT count(*) FROM companions`); n != 0 {
					t.Fatalf("stale local decision stored %d companions", n)
				}
				if p, _ := f.a.storedPlace(placeKey{transcript.AgentClaude, coworkNativeID}); p.how != localindex.PlacedByCoworkUnknown {
					t.Fatal("first unknown evidence did not establish historical taint")
				}
			})
		}
	}
}

func TestCoworkFailedHistoricalFamilyUpgradeSurvivesPurgeAndRestart(t *testing.T) {
	f := newCoworkFixture(t)
	cliMain := filepath.Join(f.cfg.ClaudeProjects, "project", coworkNativeID+".jsonl")
	childPath := filepath.Join(filepath.Dir(cliMain), coworkNativeID, "subagents", "agent-cafe.jsonl")
	coworkWrite(t, childPath, coworkRecord("agent-cafe", "old-child", "old deleted child bytes"))
	f.once()
	if err := os.RemoveAll(f.cfg.ClaudeProjects); err != nil {
		t.Fatal(err)
	}
	// A prior controlled snapshot knew the parent but had not associated this
	// deleted local-only child's historical capture with its family.
	if err := f.a.saveCoworkPlace(ctx, placeKey{transcript.AgentClaude, coworkNativeID}, placed{pl: pathpolicy.Placement{Cwd: "/host/public"}, how: localindex.PlacedByCowork}); err != nil {
		t.Fatal(err)
	}
	coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", f.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TRIGGER fail_family_upgrade BEFORE INSERT ON placements WHEN NEW.how='cowork-unknown' BEGIN SELECT RAISE(FAIL,'synthetic family upgrade failure'); END`); err != nil {
		t.Fatal(err)
	}
	f.cfg.Unplaceable = "exclude"
	f.restart()
	f.once()
	if n := f.count(`SELECT count(*) FROM messages`); n != 0 {
		t.Fatalf("pending family exclude retained %d messages", n)
	}
	if n := f.count(`SELECT count(*) FROM sources WHERE session_key='agent-cafe' AND (wm_size>0 OR wm_offset>0)`); n != 0 {
		t.Fatalf("expected purge reset child watermark: %d", n)
	}
	if n := f.count(`SELECT count(*) FROM generations g JOIN sources s ON s.id=g.source_id WHERE s.session_key='agent-cafe' AND g.size>0`); n == 0 {
		t.Fatal("purge erased durable capture metadata")
	}
	f.restart()
	f.once()
	target := &target{path: "/synthetic/parent-cli.jsonl", src: transcript.Source{Agent: transcript.AgentClaude, SessionKey: coworkNativeID}}
	if d, _ := f.a.coworkMode(f.a.policy(), target); d.Mode != pathpolicy.Deny {
		t.Fatalf("restart regranted family after proof-row purge %+v", d)
	}
	if err = f.a.taintCoworkEvidence(ctx, target); err == nil {
		t.Fatal("restart accepted failed provenance upgrade")
	}
	if _, err = db.Exec(`DROP TRIGGER fail_family_upgrade`); err != nil {
		t.Fatal(err)
	}
	f.once()
	for _, id := range []string{coworkNativeID, "agent-cafe"} {
		if p, _ := f.a.storedPlace(placeKey{transcript.AgentClaude, id}); p.how != localindex.PlacedByCoworkUnknown {
			t.Fatalf("family retry failed %s", id)
		}
	}
	coworkTranscript(t, f)
	f.restart()
	f.once()
	if n := f.count(`SELECT count(*) FROM messages`); n != 0 {
		t.Fatalf("new parent bytes escaped recovered historical floor %d", n)
	}
}

func TestCoworkCompanionOnlyHistoryFactBarrierSurvivesPurgeAndGC(t *testing.T) {
	f := newCoworkFixture(t)
	childPath := filepath.Join(f.cfg.ClaudeProjects, "project", coworkNativeID, "subagents", "agent-cafe.jsonl")
	src, err := f.store.EnsureSource(ctx, transcript.Source{Agent: transcript.AgentClaude, Path: childPath, SessionKey: "agent-cafe", StorageKind: transcript.StorageJSONLAppend, Parser: "claude@1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.ApplyBatch(ctx, localindex.Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "agent-cafe", ParentSessionID: coworkNativeID}}}); err != nil {
		t.Fatal(err)
	}
	if err = f.store.UpsertCompanion(ctx, localindex.Companion{Agent: transcript.AgentClaude, SessionID: "agent-cafe", SourceID: src.ID, Path: "/synthetic/deleted-result.txt", Size: 9}); err != nil {
		t.Fatal(err)
	}
	if err = f.a.saveCoworkPlace(ctx, placeKey{transcript.AgentClaude, coworkNativeID}, placed{pl: pathpolicy.Placement{Cwd: "/host/public"}, how: localindex.PlacedByCowork}); err != nil {
		t.Fatal(err)
	}
	coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
	if err = f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", f.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{`CREATE TABLE cowork_history(device_id TEXT NOT NULL,session_id TEXT NOT NULL,PRIMARY KEY(device_id,session_id)) WITHOUT ROWID`, `CREATE TRIGGER fail_fact BEFORE INSERT ON cowork_history BEGIN SELECT RAISE(FAIL,'synthetic fact commit failure'); END`, `CREATE TRIGGER fail_unknown BEFORE INSERT ON placements WHEN NEW.how='cowork-unknown' BEGIN SELECT RAISE(FAIL,'synthetic how failure'); END`} {
		if _, err = db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	f.cfg.Unplaceable = "exclude"
	f.restart()
	f.once()
	if n := f.count(`SELECT count(*) FROM companions`); n != 1 {
		t.Fatalf("failed marker destroyed last companion proof %d", n)
	}
	if n := f.count(`SELECT count(*) FROM cowork_history`); n != 0 {
		t.Fatalf("failed marker partially committed %d", n)
	}
	if err = f.a.purgeDenied(ctx); err == nil {
		t.Fatal("destructive purge bypassed fact failure")
	}
	if err = f.a.purgeSource(ctx, src.ID, pathpolicy.Decision{Mode: pathpolicy.Deny}); err == nil {
		t.Fatal("direct source purge bypassed fact failure")
	}
	ordinary := filepath.Join(f.cfg.ClaudeProjects, "-host-ordinary", "ordinary.jsonl")
	coworkWrite(t, ordinary, coworkRecord("ordinary", "ordinary-message", "ordinary CLI remains usable"))
	f.once()
	if n := len(f.find("ordinary CLI remains usable", false)); n != 1 {
		t.Fatalf("unrelated CLI blocked by scoped fact failure %d", n)
	}
	if err = f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`DROP TRIGGER fail_fact`); err != nil {
		t.Fatal(err)
	}
	f.once()
	for _, id := range []string{coworkNativeID, "agent-cafe"} {
		if have, err := f.store.CoworkHistoricalUnknown(ctx, id); err != nil || !have {
			t.Fatalf("family fact missing %s %v %v", id, have, err)
		}
	}
	if n := f.count(`SELECT count(*) FROM companions`); n != 0 {
		t.Fatalf("committed marker did not permit deny purge %d", n)
	}
	if err = f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`DROP TRIGGER fail_unknown`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"messages", "companions", "conversations", "generations", "sources", "placements"} {
		if _, err = db.Exec("DELETE FROM " + table); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.RemoveAll(f.cfg.CoworkRoot); err != nil {
		t.Fatal(err)
	}
	f.cfg.CoworkRoot = "-"
	f.restart()
	for _, id := range []string{coworkNativeID, "agent-cafe"} {
		if p, _ := f.a.storedPlace(placeKey{transcript.AgentClaude, id}); p.how != localindex.PlacedByCoworkUnknown {
			t.Fatalf("GC/restart erased historical origin %s", id)
		}
		if f.a.allowUpload(devicesync.SourceSpec{Agent: transcript.AgentClaude, SessionKey: id, Path: "/synthetic/overlap.jsonl"}) {
			t.Fatalf("fact-backed overlap shared after GC %s", id)
		}
	}
}

func TestCoworkFactReadFailureHoldsClaudeSharingBeforeEmptyPolicyFastPath(t *testing.T) {
	f := newCoworkFixture(t)
	f.cfg.Unplaceable = "upload"
	f.restart()
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", f.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE cowork_history(device_id TEXT)`); err != nil {
		t.Fatal(err)
	}
	f.restart()
	path := filepath.Join(f.cfg.ClaudeProjects, "-host-ordinary", "ordinary.jsonl")
	coworkWrite(t, path, coworkRecord("ordinary", "message", "ordinary local search during history error"))
	f.once()
	if n := len(f.find("ordinary local search during history error", false)); n != 1 {
		t.Fatalf("history read error blocked ordinary local search %d", n)
	}
	if f.a.allowUpload(devicesync.SourceSpec{Agent: transcript.AgentClaude, SessionKey: "ordinary", Path: path}) {
		t.Fatal("empty policy bypassed historical read error")
	}
	if !f.a.allowUpload(devicesync.SourceSpec{Agent: transcript.AgentCodex, SessionKey: "ordinary", Path: "/synthetic/codex"}) {
		t.Fatal("Claude history error blocked unrelated harness")
	}
	if err = f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`DROP TABLE cowork_history`); err != nil {
		t.Fatal(err)
	}
	f.a.refreshCowork(ctx)
	if !f.a.allowUpload(devicesync.SourceSpec{Agent: transcript.AgentClaude, SessionKey: "ordinary", Path: path}) {
		t.Fatal("recovered history read remained held")
	}
}

func TestCoworkFactSurvivesReloadOfStaleOrdinaryPlacement(t *testing.T) {
	f := newCoworkFixture(t)
	f.cfg.Unplaceable = "exclude"
	if err := f.store.SavePlacement(ctx, localindex.Placement{Agent: transcript.AgentClaude, SessionID: coworkNativeID, Placement: pathpolicy.Placement{Cwd: "/host/public"}, How: localindex.PlacedByCwd}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.MarkCoworkHistoricalUnknown(ctx, []string{coworkNativeID}); err != nil {
		t.Fatal(err)
	}
	// A file at the configured root makes discovery fail on every platform.
	coworkWrite(t, f.cfg.CoworkRoot, "unreadable container layout")
	f.restart()
	if err := f.a.load(ctx); err != nil {
		t.Fatal(err)
	}
	p, _ := f.a.storedPlace(placeKey{transcript.AgentClaude, coworkNativeID})
	if p.how != localindex.PlacedByCoworkUnknown || p.pl.Cwd != "/host/public" {
		t.Fatalf("reload lost historical provenance or actual path: %+v", p)
	}
	target := &target{path: "/synthetic/cli-overlap.jsonl", src: transcript.Source{Agent: transcript.AgentClaude, SessionKey: coworkNativeID}}
	if d, ok := f.a.coworkMode(f.a.policy(), target); !ok || d.Mode != pathpolicy.Deny {
		t.Fatalf("reload bypassed historical floor: %+v %v", d, ok)
	}
	if f.a.allowUpload(devicesync.SourceSpec{Agent: transcript.AgentClaude, SessionKey: coworkNativeID, Path: target.path}) {
		t.Fatal("reload shared fact-backed CLI overlap")
	}
}

func TestCoworkFactRestoredBeforeAppRootDiscoveryError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission fixture requires non-root process")
	}
	f := newCoworkFixture(t)
	if err := f.store.MarkCoworkHistoricalUnknown(ctx, []string{coworkNativeID}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.cfg.CoworkRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.cfg.CoworkRoot, 0000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(f.cfg.CoworkRoot, 0700)
	f.restart()
	if p, _ := f.a.storedPlace(placeKey{transcript.AgentClaude, coworkNativeID}); p.how != localindex.PlacedByCoworkUnknown {
		t.Fatal("app discovery error erased fact-backed origin")
	}
	if f.a.allowUpload(devicesync.SourceSpec{Agent: transcript.AgentClaude, SessionKey: coworkNativeID, Path: "/synthetic/cli-overlap.jsonl"}) {
		t.Fatal("discovery error shared fact-backed CLI overlap")
	}
}

func TestCoworkCurrentUnknownWithoutHistoricalBytesDoesNotUseHistoricalFloor(t *testing.T) {
	f := newCoworkFixture(t)
	f.cfg.Unplaceable = "exclude"
	f.restart()
	coworkMetadata(t, f, []string{"/host/public"}, []string{"/sessions/unresolved"}, nil)
	f.a.refreshCowork(context.Background())
	target := &target{path: "/synthetic/metadata-only-cli.jsonl", src: transcript.Source{Agent: transcript.AgentClaude, SessionKey: coworkNativeID}}
	d, ok := f.a.coworkMode(f.a.policy(), target)
	if !ok || d.Mode != pathpolicy.Local {
		t.Fatalf("current unknown mistaken for historical unknown: %+v %v", d, ok)
	}
	if p, _ := f.a.storedPlace(placeKey{transcript.AgentClaude, coworkNativeID}); p.how != localindex.PlacedByCowork {
		t.Fatalf("metadata-only taint: %+v", p)
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
			if facts, err := f.store.CoworkHistoricalUnknownFacts(ctx); err != nil || len(facts) != 0 {
				t.Fatalf("metadata-only/zero bytes created historical fact %v %v", facts, err)
			}
			coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
			coworkTranscript(t, f)
			f.once()
			if st := f.a.coworkStatus(); st.HistoricalUnknown != 0 {
				t.Fatalf("first mapped content tainted: %+v", st)
			}
			if facts, err := f.store.CoworkHistoricalUnknownFacts(ctx); err != nil || len(facts) != 0 {
				t.Fatalf("first controlled bytes created historical fact %v %v", facts, err)
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
	if p.how != localindex.PlacedByCoworkUnknown {
		t.Fatal("committed historical fact was weakened by compatibility failure")
	}
	if have, err := f.store.CoworkHistoricalUnknown(ctx, coworkNativeID); err != nil || !have {
		t.Fatalf("historical fact was not committed before compatibility failure: %v %v", have, err)
	}
	if len(f.find("retry unknown needle", false)) != 0 {
		t.Fatal("unknown appended content indexed despite failed compatibility write")
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

func TestCoworkVerifiedHistoricalEvidenceMarksKnownFamilyBeforePurge(t *testing.T) {
	f := newCoworkFixture(t)
	f.cfg.Unplaceable = "exclude"
	f.restart()
	coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
	path := coworkTranscript(t, f)
	child, _ := coworkChildren(t, path)
	sibling := filepath.Join(filepath.Dir(child), "agent-beef.jsonl")
	coworkWrite(t, sibling, coworkRecord("agent-beef", "sibling-message", "known sibling history"))
	f.once()
	ids := []string{coworkNativeID, "agent-cafe", "agent-beef"}
	for _, id := range ids {
		if p, _ := f.a.storedPlace(placeKey{transcript.AgentClaude, id}); p.how != localindex.PlacedByCowork {
			t.Fatalf("current family was not known %s: %+v", id, p)
		}
	}
	if n := f.count(`SELECT count(*) FROM companions`); n == 0 {
		t.Fatal("missing companion proof fixture")
	}
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", f.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{`CREATE TABLE cowork_history(device_id TEXT NOT NULL,session_id TEXT NOT NULL,PRIMARY KEY(device_id,session_id)) WITHOUT ROWID`, `CREATE TRIGGER fail_fact BEFORE INSERT ON cowork_history BEGIN SELECT RAISE(FAIL,'synthetic fact failure'); END`} {
		if _, err = db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	mark := func() error {
		f.a.captureScopeMu.Lock()
		defer f.a.captureScopeMu.Unlock()
		// This is verified historical evidence, independent of current readiness.
		return f.a.markCoworkHistoricalUnknown(ctx, []placeKey{{transcript.AgentClaude, "agent-cafe"}})
	}
	if err = mark(); err == nil {
		t.Fatal("fact failure ignored")
	}
	for _, id := range ids {
		target := &target{path: path, src: transcript.Source{Agent: transcript.AgentClaude, SessionKey: id}}
		if d, _ := f.a.coworkMode(f.a.policy(), target); d.Mode != pathpolicy.Deny {
			t.Fatalf("failed verified history did not restrict family %s: %+v", id, d)
		}
	}
	if err = f.a.purgeDenied(ctx); err == nil {
		t.Fatal("failed fact commit allowed last proof purge")
	}
	if n := f.count(`SELECT count(*) FROM companions`); n == 0 {
		t.Fatal("failed fact commit erased companion proof")
	}
	if _, err = db.Exec(`DROP TRIGGER fail_fact`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TRIGGER fail_how BEFORE INSERT ON placements WHEN NEW.how='cowork-unknown' BEGIN SELECT RAISE(FAIL,'synthetic compatibility failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err = mark(); err == nil {
		t.Fatal("compatibility failure fixture did not fire")
	}
	for _, id := range ids {
		if have, err := f.store.CoworkHistoricalUnknown(ctx, id); err != nil || !have {
			t.Fatalf("verified family fact missing %s: %v %v", id, have, err)
		}
		if p, _ := f.a.storedPlace(placeKey{transcript.AgentClaude, id}); p.how != localindex.PlacedByCoworkUnknown || p.pl.Cwd != "/host/public" {
			t.Fatalf("fact authority lost after compatibility failure %s: %+v", id, p)
		}
	}
	if err = f.a.purgeDenied(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT count(*) FROM companions`); n != 0 {
		t.Fatalf("durable facts did not release proof purge %d", n)
	}
	if err = f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`DROP TRIGGER fail_how`); err != nil {
		t.Fatal(err)
	}
	if err = mark(); err != nil {
		t.Fatal(err)
	}
	f.restart()
	for _, id := range ids {
		if p, _ := f.a.storedPlace(placeKey{transcript.AgentClaude, id}); p.how != localindex.PlacedByCoworkUnknown {
			t.Fatalf("restart lost verified historical origin %s", id)
		}
	}
}
