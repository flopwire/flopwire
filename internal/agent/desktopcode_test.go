package agent

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
)

const desktopAppID = "local_aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

func newDesktopCodeFixture(t *testing.T) *fixture {
	t.Helper()
	f := newCoworkFixture(t)
	f.cfg.CoworkRoot = "-"
	f.cfg.DesktopCodeRoot = filepath.Join(f.home, "desktop-code")
	f.restart()
	return f
}
func desktopMetadata(t *testing.T, f *fixture, origin string) {
	t.Helper()
	md := map[string]any{"sessionId": desktopAppID, "cliSessionId": coworkNativeID, "cwd": filepath.Join(f.home, "repo"), "originCwd": filepath.Join(f.home, "repo")}
	if origin != "" {
		md["importedFrom"] = origin
	}
	b, e := json.Marshal(md)
	if e != nil {
		t.Fatal(e)
	}
	coworkWrite(t, filepath.Join(f.cfg.DesktopCodeRoot, "account", "org", desktopAppID+".json"), string(b))
}
func desktopRecord(f *fixture, session, id, text string) string {
	return strings.Replace(coworkRecord(session, id, text), "/sessions/vm-workspace", filepath.Join(f.home, "repo"), 1)
}
func desktopTranscript(t *testing.T, f *fixture) string {
	t.Helper()
	p := filepath.Join(f.cfg.DesktopCodeRoot, "account", "org", desktopAppID, ".claude", "projects", "-host-repo", coworkNativeID+".jsonl")
	coworkWrite(t, p, desktopRecord(f, coworkNativeID, "desktop-main", "desktop code main needle"))
	return p
}
func desktopCLI(t *testing.T, f *fixture) string {
	t.Helper()
	p := filepath.Join(f.cfg.ClaudeProjects, "configured-project", coworkNativeID+".jsonl")
	coworkWrite(t, p, desktopRecord(f, coworkNativeID, "cli-main", "configured local code needle"))
	return p
}

func TestDesktopCodeScopedNativeDescendantsAndConfiguredCLI(t *testing.T) {
	f := newDesktopCodeFixture(t)
	desktopMetadata(t, f, "")
	main := desktopTranscript(t, f)
	sub, companion := coworkChildren(t, main)
	coworkWrite(t, sub, desktopRecord(f, "agent-cafe", "desktop-child", "desktop code child needle"))
	cli := desktopCLI(t, f)
	unrelated := "99999999-8888-4777-8666-555555555555"
	coworkWrite(t, filepath.Join(filepath.Dir(main), unrelated+".jsonl"), desktopRecord(f, unrelated, "unrelated", "unrelated scoped text"))
	f.once()
	if len(f.find("desktop code main needle", false)) != 1 || len(f.find("desktop code child needle", false)) != 1 || len(f.find("configured local code needle", false)) != 1 {
		t.Fatal("native/default root collection missing")
	}
	if len(f.find("unrelated scoped text", false)) != 0 {
		t.Fatal("unrelated scoped parent collected")
	}
	for _, p := range []string{main, sub, companion} {
		if _, ok := f.rec.spec(p); ok {
			t.Fatal("scoped Code uploaded before secure sync source hook")
		}
		if f.a.allowUpload(devicesync.SourceSpec{Path: p, Agent: transcript.AgentClaude, SessionKey: coworkNativeID}) {
			t.Fatal("empty policy bypassed scoped Code sync hold")
		}
	}
	if _, ok := f.rec.spec(cli); !ok {
		t.Fatal("configured normal CLI sharing suppressed")
	}
	if n := f.count(`SELECT count(*) FROM sources WHERE path=? AND agent='claude'`, main); n != 1 {
		t.Fatal("native source path changed")
	}
	st := f.a.desktopCodeStatus()
	if st.Sessions != 1 || st.NormalLinks != 1 || st.SharedHold == "" {
		t.Fatalf("status %+v", st)
	}
	f.restart()
	f.once()
	if len(f.find("desktop code main needle", false)) != 1 {
		t.Fatal("restart duplicated or lost scoped Code")
	}
}

func TestDesktopCodeFolderRulesApplyToScopedChildren(t *testing.T) {
	f := newDesktopCodeFixture(t)
	f.cfg.UserRuleList = []string{"deny " + filepath.Join(f.home, "repo")}
	f.restart()
	desktopMetadata(t, f, "")
	main := desktopTranscript(t, f)
	coworkChildren(t, main)
	f.once()
	if n := f.count(`SELECT count(*) FROM messages WHERE superseded=0`); n != 0 {
		t.Fatalf("deny leaked %d messages", n)
	}
	if n := f.count(`SELECT count(*) FROM companions`); n != 0 {
		t.Fatal("denied companions indexed")
	}
}

func TestDesktopCodeLiveRootAdditionAndScopedSymlinkSwap(t *testing.T) {
	f := newDesktopCodeFixture(t)
	if f.a.desktopCodeStatus().State != "unavailable" {
		t.Fatal("missing app root falsely supported")
	}
	desktopMetadata(t, f, "")
	main := desktopTranscript(t, f)
	if found, known := f.a.discoverDir(filepath.Dir(main)); !known || found != nil {
		t.Fatal("new scoped directory did not request full refresh")
	}
	f.once()
	if len(f.find("desktop code main needle", false)) != 1 {
		t.Fatal("new Code root not discovered")
	}
	found, e := f.a.discoverAll()
	if e != nil {
		t.Fatal(e)
	}
	var target *target
	for _, entry := range found.targets {
		if entry.path == main {
			target = entry
			break
		}
	}
	if target == nil {
		t.Fatal("no native target")
	}
	outside := filepath.Join(f.home, "outside.jsonl")
	coworkWrite(t, outside, desktopRecord(f, coworkNativeID, "escaped", "outside scoped secret"))
	if e = os.Remove(main); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(outside, main); e != nil {
		t.Fatal(e)
	}
	if _, e = f.a.indexTranscript(ctx, target); e != nil {
		t.Fatal(e)
	}
	if len(f.find("outside scoped secret", false)) != 0 {
		t.Fatal("swapped path escaped secure index open")
	}
	if _, e = f.a.openNativeEvidence(main); e == nil {
		t.Fatal("contained opener followed swapped path")
	}
}

func TestDesktopCodeCoworkAliasBeforeCaptureAndRestart(t *testing.T) {
	f := newDesktopCodeFixture(t)
	desktopMetadata(t, f, "local-1p-cowork")
	main := desktopCLI(t, f)
	sub, companion := coworkChildren(t, main)
	f.once()
	for _, id := range []string{coworkNativeID, "agent-cafe"} {
		p, ok := f.a.storedPlace(placeKey{transcript.AgentClaude, id})
		if !ok || p.how != localindex.PlacedByCoworkUnknown {
			t.Fatalf("unknown capture origin %s: %+v", id, p)
		}
	}
	if len(f.find("configured local code needle", false)) != 1 {
		t.Fatal("alias suppressed local normal CLI search")
	}
	for _, p := range []string{main, sub, companion} {
		if _, ok := f.rec.spec(p); ok {
			t.Fatal("Cowork alias copy escaped sharing hold")
		}
	}
	if st := f.a.desktopCodeStatus(); st.CoworkAliases != 1 || st.OutOfScope != 1 || st.Sessions != 0 {
		t.Fatalf("alias falsely collected as Code %+v", st)
	}
	if e := os.RemoveAll(f.cfg.DesktopCodeRoot); e != nil {
		t.Fatal(e)
	}
	f.restart()
	f.once()
	for _, p := range []string{main, sub, companion} {
		if f.a.allowUpload(devicesync.SourceSpec{Path: p, Agent: transcript.AgentClaude, SessionKey: coworkNativeID}) {
			t.Fatal("restart lost durable Cowork origin")
		}
	}
}

func TestDesktopCodeMetadataOnlyCoworkAliasDoesNotTaint(t *testing.T) {
	f := newDesktopCodeFixture(t)
	desktopMetadata(t, f, "local-1p-cowork")
	f.a.refreshCowork(ctx)
	p, ok := f.a.storedPlace(placeKey{transcript.AgentClaude, coworkNativeID})
	if !ok || p.how != localindex.PlacedByCowork {
		t.Fatalf("metadata-only alias tainted future bytes %+v", p)
	}
	// Genuine complete Cowork metadata can qualify the first controlled content.
	f.cfg.CoworkRoot = filepath.Join(f.home, "cowork")
	coworkMetadata(t, f, []string{filepath.Join(f.home, "repo")}, nil, nil)
	desktopCLI(t, f)
	f.restart()
	f.once()
	p, _ = f.a.storedPlace(placeKey{transcript.AgentClaude, coworkNativeID})
	if p.how != localindex.PlacedByCowork {
		t.Fatalf("Code alias weakened genuine mapping %+v", p)
	}
}

func TestDesktopCodeAliasRegistrationWaitsForPreproofCapture(t *testing.T) {
	f := newDesktopCodeFixture(t)
	desktopCLI(t, f)
	barrier := &coworkBarrierParser{Parser: f.a.claude, entered: make(chan struct{}), release: make(chan struct{})}
	f.a.claude = barrier
	indexed := make(chan error, 1)
	go func() { indexed <- f.a.Once(ctx) }()
	select {
	case <-barrier.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no capture barrier")
	}
	desktopMetadata(t, f, "local-1p-cowork")
	registered := make(chan struct{})
	go func() { f.a.refreshCowork(ctx); close(registered) }()
	select {
	case <-registered:
		t.Fatal("Code registration overtook in-flight old content")
	case <-time.After(30 * time.Millisecond):
	}
	close(barrier.release)
	select {
	case e := <-indexed:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("capture blocked")
	}
	select {
	case <-registered:
	case <-time.After(5 * time.Second):
		t.Fatal("registration blocked")
	}
	p, _ := f.a.storedPlace(placeKey{transcript.AgentClaude, coworkNativeID})
	if p.how != localindex.PlacedByCoworkUnknown {
		t.Fatal("preproof content falsely gained known Cowork scope")
	}
}

func TestDesktopCodeParserSideReadAndCompanionSwapStayContained(t *testing.T) {
	f := newDesktopCodeFixture(t)
	desktopMetadata(t, f, "")
	main := desktopTranscript(t, f)
	tool := filepath.Join(filepath.Dir(main), coworkNativeID, "tool-results", "result.txt")
	outside := filepath.Join(f.home, "outside-persisted.txt")
	coworkWrite(t, outside, "outside persisted secret needle")
	if e := os.MkdirAll(filepath.Dir(tool), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(outside, tool); e != nil {
		t.Fatal(e)
	}
	line := map[string]any{"cwd": filepath.Join(f.home, "repo"), "sessionId": coworkNativeID, "type": "user", "uuid": "persisted-result", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "synthetic-tool", "content": "<persisted-output>\nFull output saved to: /fake/tool-results/result.txt\nPreview: safe preview text\n</persisted-output>"}}}}
	b, e := json.Marshal(line)
	if e != nil {
		t.Fatal(e)
	}
	coworkWrite(t, main, string(b)+"\n")
	f.once()
	if len(f.find("outside persisted secret needle", false)) != 0 {
		t.Fatal("parser persisted-output read followed external symlink")
	}
	if len(f.find("safe preview text", false)) != 1 {
		t.Fatal("missing side read did not preserve preview")
	}
	if n := f.count(`SELECT count(*) FROM companions WHERE path=?`, tool); n != 0 {
		t.Fatal("unsafe companion indexed")
	}
	// A companion discovered while regular may become a symlink before its own
	// capture. The actual opener and preflight must both reject that substitution.
	if e = os.Remove(tool); e != nil {
		t.Fatal(e)
	}
	coworkWrite(t, tool, "safe companion")
	f.a.refreshCowork(ctx)
	found, e := f.a.discoverAll()
	if e != nil {
		t.Fatal(e)
	}
	var companion *target
	for _, entry := range found.targets {
		if entry.path == tool {
			companion = entry
			break
		}
	}
	if companion == nil {
		t.Fatal("regular companion absent")
	}
	if e = os.Remove(tool); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(outside, tool); e != nil {
		t.Fatal(e)
	}
	if e = f.a.indexCompanion(ctx, companion); e != nil {
		t.Fatal(e)
	}
	if n := f.count(`SELECT count(*) FROM companions WHERE path=?`, tool); n != 0 {
		t.Fatal("companion swapped after discovery gained digest/index")
	}
}

func TestDesktopCodePlacementHintReadUsesContainedOpener(t *testing.T) {
	f := newDesktopCodeFixture(t)
	desktopMetadata(t, f, "")
	main := desktopTranscript(t, f)
	outside := filepath.Join(f.home, "hint-outside.jsonl")
	coworkWrite(t, outside, strings.Replace(desktopRecord(f, coworkNativeID, "private-hint", "outside hint"), filepath.Join(f.home, "repo"), "/outside/private-repo", 1))
	if e := os.Remove(main); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(outside, main); e != nil {
		t.Fatal(e)
	}
	hints := scanHintsWithOpen(main, f.a.openNativeEvidence)
	if hints.cwd != "" {
		t.Fatalf("unsafe hint read admitted external cwd %q", hints.cwd)
	}
}

// Opt-in qualification reads only the owner-approved synthetic Local Code
// session. It uses an isolated index and metadata mirror, never the production
// index or sync service and never another real transcript or metadata record.
func TestDesktopCodeApprovedLiveProbe(t *testing.T) {
	metadataPath := os.Getenv("FLOPWIRE_DESKTOP_CODE_PROBE_METADATA")
	transcriptPath := os.Getenv("FLOPWIRE_DESKTOP_CODE_PROBE_TRANSCRIPT")
	if metadataPath == "" || transcriptPath == "" {
		t.Skip("explicit approved synthetic probe paths required")
	}
	const appID = "local_eaef470e-e9ea-4b86-8284-0a9231ec96e2"
	const nativeID = "1f1a67fd-2ed6-4457-912a-7c2328ef166b"
	if filepath.Base(metadataPath) != appID+".json" || filepath.Base(transcriptPath) != nativeID+".jsonl" {
		t.Fatal("paths do not identify the approved synthetic probe")
	}
	b, e := os.ReadFile(metadataPath)
	if e != nil {
		t.Fatal(e)
	}
	var md struct {
		SessionID    string `json:"sessionId"`
		CLISessionID string `json:"cliSessionId"`
		Cwd          string `json:"cwd"`
		OriginCwd    string `json:"originCwd"`
	}
	if e = json.Unmarshal(b, &md); e != nil {
		t.Fatal(e)
	}
	if md.SessionID != appID || md.CLISessionID != nativeID || md.Cwd != "/private/tmp/flopwire-code-tab-probe.56xDBo" {
		t.Fatal("metadata does not match approved probe")
	}
	f := newDesktopCodeFixture(t)
	f.cfg.Sync = nil
	mirror, e := json.Marshal(md)
	if e != nil {
		t.Fatal(e)
	}
	coworkWrite(t, filepath.Join(f.cfg.DesktopCodeRoot, "account", "org", appID+".json"), string(mirror))
	f.restart()
	f.a.mu.Lock()
	links := f.a.desktopCodeResult.IdentityLinks
	f.a.mu.Unlock()
	if len(links) != 1 || len(links[0].CLISessionIDs) != 1 || links[0].CLISessionIDs[0] != nativeID {
		t.Fatal("integrated metadata linkage missing")
	}
	target := &target{path: transcriptPath, kind: kindTranscript, src: transcript.Source{Path: transcriptPath, Agent: transcript.AgentClaude, SessionKey: nativeID, Parser: claude.ParserName, StorageKind: transcript.StorageJSONLAppend}, parser: f.a.claude}
	if _, e = f.a.indexTranscript(ctx, target); e != nil {
		t.Fatal(e)
	}
	if e = f.store.Sync(ctx); e != nil {
		t.Fatal(e)
	}
	const prompt = "Reply with exactly FLOPWIRE_CODE_TAB_PROBE. Do not read files, run commands, or modify anything."
	for _, text := range []string{prompt, "FLOPWIRE_CODE_TAB_PROBE"} {
		matched := 0
		for _, row := range f.find(text, false) {
			if row.Text == text && row.SourcePath == transcriptPath && row.SessionID == nativeID {
				matched++
			}
		}
		if matched != 1 {
			t.Fatalf("approved probe prompt/reply native provenance rows=%d", matched)
		}
	}
	if n := f.count(`SELECT count(*) FROM messages WHERE kind IN ('tool_call','tool_result')`); n != 0 {
		t.Fatalf("unexpected tool rows %d", n)
	}
	if n := f.count(`SELECT count(DISTINCT path) FROM sources`); n != 1 {
		t.Fatal("qualification read other source paths")
	}
}

func TestDesktopCodeCoworkHistoricalUnknownHonorsExcludeAfterMapping(t *testing.T) {
	f := newDesktopCodeFixture(t)
	main := desktopCLI(t, f)
	sub, companion := coworkChildren(t, main)
	f.once()
	if len(f.find("configured local code needle", false)) != 1 || len(f.find("cowork child needle", false)) != 1 {
		t.Fatal("pre-origin local evidence missing")
	}
	// A current genuine mapping cannot authorize historical bytes accepted
	// before the imported Cowork identity metadata was available.
	desktopMetadata(t, f, "local-1p-cowork")
	f.cfg.CoworkRoot = filepath.Join(f.home, "cowork")
	coworkMetadata(t, f, []string{filepath.Join(f.home, "repo")}, nil, nil)
	f.cfg.Unplaceable = "exclude"
	f.restart()
	f.once()
	if len(f.find("configured local code needle", false)) != 0 || len(f.find("cowork child needle", false)) != 0 {
		t.Fatal("historical unknown Cowork copy bypassed exclude after current mapping")
	}
	for _, item := range []struct{ path, id string }{{main, coworkNativeID}, {sub, "agent-cafe"}} {
		p, ok := f.a.storedPlace(placeKey{transcript.AgentClaude, item.id})
		if !ok || p.how != localindex.PlacedByCoworkUnknown {
			t.Fatal("historical uncertainty was cleared")
		}
		target := &target{path: item.path, kind: kindTranscript, src: transcript.Source{Agent: transcript.AgentClaude, SessionKey: item.id}}
		if mode, known := f.a.modeOf(target); !known || mode != pathpolicy.Deny {
			t.Fatal("imported historical unknown did not inherit exclude")
		}
	}
	for _, path := range []string{main, sub, companion} {
		if f.a.allowUpload(devicesync.SourceSpec{Path: path, Agent: transcript.AgentClaude, SessionKey: coworkNativeID}) {
			t.Fatal("excluded imported Cowork copy shared")
		}
	}
	if e := os.RemoveAll(f.cfg.DesktopCodeRoot); e != nil {
		t.Fatal(e)
	}
	f.restart()
	f.once()
	if len(f.find("configured local code needle", false)) != 0 || len(f.find("cowork child needle", false)) != 0 {
		t.Fatal("restart lost imported historical exclusion")
	}
}

func TestDesktopCodeCoworkAliasOrphanChildTaintsParentGroup(t *testing.T) {
	f := newDesktopCodeFixture(t)
	if e := f.store.Sync(ctx); e != nil {
		t.Fatal(e)
	}
	db, e := sql.Open("sqlite", f.store.Path())
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	sub := filepath.Join(f.cfg.ClaudeProjects, "configured-project", coworkNativeID, "subagents", "agent-cafe.jsonl")
	// A closed, acknowledged generation can survive after every parent/child
	// file and all local-index rows are gone. Its spec may predate SessionKey.
	spec, e := json.Marshal(devicesync.SourceSpec{Path: sub, Agent: transcript.AgentClaude, StorageKind: transcript.StorageJSONLAppend, Parser: claude.ParserName})
	if e != nil {
		t.Fatal(e)
	}
	for _, stmt := range []string{`CREATE TABLE devsync_sources(id INTEGER PRIMARY KEY,path TEXT,spec TEXT,generation INTEGER)`, `CREATE TABLE devsync_gens(source_id INTEGER,generation INTEGER,size INTEGER,closed INTEGER,acked INTEGER)`} {
		if _, e = db.Exec(stmt); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = db.Exec(`INSERT INTO devsync_sources VALUES(1,?,?,3)`, sub, string(spec)); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(`INSERT INTO devsync_gens VALUES(1,1,9,1,1)`); e != nil {
		t.Fatal(e)
	}
	desktopMetadata(t, f, "local-1p-cowork")
	f.restart()
	for _, id := range []string{coworkNativeID, "agent-cafe"} {
		p, ok := f.a.storedPlace(placeKey{transcript.AgentClaude, id})
		if !ok || p.how != localindex.PlacedByCoworkUnknown {
			t.Fatalf("orphan child evidence did not taint group member %s", id)
		}
	}
	if f.a.allowUpload(devicesync.SourceSpec{Path: sub, Agent: transcript.AgentClaude, SessionKey: "agent-cafe"}) {
		t.Fatal("orphan imported Cowork child shared")
	}
}
