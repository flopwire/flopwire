package agent

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/antigravity"
)

const (
	agySession = "d18f778d-65eb-4daf-a402-244b505fae9a"
	agyInput   = `{"step_index":0,"type":"USER_INPUT","source":"USER_EXPLICIT","status":"DONE","content":"<USER_REQUEST>\nAntigravity canonical test prompt\n</USER_REQUEST>","created_at":"2026-11-24T12:04:06Z"}` + "\n"
)

func TestAntigravityDiscoveryAndCapture(t *testing.T) {
	f := newFixture(t, "-")
	f.cfg.AntigravityRoot = f.path("custom-brain")
	f.restart()
	writeAntigravityWorkspace(t, f.home, f.home)

	logs := filepath.Join(f.cfg.AntigravityRoot, agySession, ".system_generated", "logs")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logs, "transcript.jsonl")
	for _, p := range []string{path, filepath.Join(logs, "full_transcript.jsonl"), filepath.Join(logs, "other.jsonl")} {
		if err := os.WriteFile(p, []byte(agyInput), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(f.cfg.AntigravityRoot, agySession), filepath.Join(f.cfg.AntigravityRoot, "ec102b6f-5bea-419a-9dc4-555ef893421b")); err != nil {
		t.Fatal(err)
	}

	f.once()
	if count := f.count("SELECT count(*) FROM sources WHERE agent = 'antigravity'"); count != 1 {
		t.Fatalf("Antigravity sources = %d, want 1", count)
	}
	if got := f.find("Antigravity canonical test prompt", false); len(got) != 1 {
		t.Fatalf("Antigravity matches = %d, want 1", len(got))
	}
	spec, ok := f.rec.spec(path)
	if !ok || spec.Agent != transcript.AgentAntigravity || spec.Parser != antigravity.ParserName ||
		spec.SessionKey != agySession || spec.Root != f.cfg.AntigravityRoot || spec.StorageKind != transcript.StorageJSONLAppend {
		t.Fatalf("Antigravity sync spec = %+v, present = %v", spec, ok)
	}
}

func TestNewAntigravityConversationListedAfterWatch(t *testing.T) {
	f := newFixture(t, "-")
	f.cfg.AntigravityRoot = f.path("brain")
	if err := os.Mkdir(f.cfg.AntigravityRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	f.restart()
	f.once()

	w := newWatcher(f.cfg.Logger)
	defer w.close()
	f.a.rewatch(w)
	logs := filepath.Join(f.cfg.AntigravityRoot, agySession, ".system_generated", "logs")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logs, "transcript.jsonl")
	testHookAfterRootSweep = func() {
		if err := os.WriteFile(path, []byte(agyInput), 0o600); err != nil {
			t.Error(err)
		}
	}
	defer func() { testHookAfterRootSweep = nil }()
	f.a.scanDir(ctx, f.cfg.AntigravityRoot, w)
	f.a.mu.Lock()
	tracked := f.a.targets[path] != nil
	f.a.mu.Unlock()
	if !tracked {
		t.Fatal("Antigravity transcript created before its watch waits for the next sweep")
	}
}

func TestAntigravityCatalogTightensUnchangedTranscript(t *testing.T) {
	f := newFixture(t, "-")
	f.cfg.AntigravityRoot = f.path("brain")
	blocked := f.path("private")
	f.cfg.UserRuleList = []string{"deny:" + blocked}
	var withheld atomic.Bool
	f.cfg.Withhold = func(_ context.Context, req localindex.Withhold) error {
		if req.Agent == transcript.AgentAntigravity && req.SessionID == agySession && req.Mode == "deny" {
			withheld.Store(true)
		}
		return nil
	}
	for _, dir := range []string{f.path("public"), blocked} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f.restart()

	logs := filepath.Join(f.cfg.AntigravityRoot, agySession, ".system_generated", "logs")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logs, "transcript.jsonl")
	if err := os.WriteFile(path, []byte(agyInput), 0o600); err != nil {
		t.Fatal(err)
	}
	writeAntigravityWorkspace(t, f.home, f.path("public"))
	f.once()
	if got := f.find("Antigravity canonical test prompt", false); len(got) != 1 {
		t.Fatalf("initial Antigravity matches = %d, want 1", len(got))
	}
	spec, ok := f.rec.spec(path)
	if !ok || !f.a.allowUpload(spec) {
		t.Fatal("public Antigravity trajectory was withheld")
	}

	auth, err := f.a.authorizeCapture(ctx, spec)
	if err != nil || auth == nil {
		t.Fatalf("public Antigravity capture authorization = %v, %v", auth, err)
	}
	writeAntigravityWorkspace(t, f.home)
	err = auth.Check(ctx)
	auth.Release()
	if err == nil {
		t.Fatal("stale workspace catalog authorized capture")
	}
	f.once()
	if f.a.allowUpload(spec) {
		t.Fatal("vacated workspace metadata retained the previous capture authorization")
	}

	writeAntigravityWorkspace(t, f.home, f.path("public"), blocked)
	f.once()
	if got := f.find("Antigravity canonical test prompt", false); len(got) != 0 {
		t.Fatalf("Antigravity matches after deny = %d, want 0", len(got))
	}
	if f.a.allowUpload(spec) {
		t.Fatal("a denied secondary workspace failed to withhold the trajectory")
	}
	if !withheld.Load() {
		t.Fatal("catalog tightening did not request deletion of the previously eligible server copy")
	}
}

func writeAntigravityWorkspace(t *testing.T, home string, paths ...string) {
	t.Helper()
	uris := make([]string, 0, len(paths))
	for _, path := range paths {
		uris = append(uris, (&url.URL{Scheme: "file", Path: path}).String())
	}
	cache := map[string]any{"conversations": map[string]any{
		agySession: map[string]any{"is_internal": false, "summary": map[string]any{"WorkspaceURIs": uris}},
	}}
	data, err := json.Marshal(cache)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "cache")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "conversation_metadata.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAntigravityInternalCacheCannotAuthorizeUnplaceableUpload(t *testing.T) {
	f := newFixture(t, "-")
	f.cfg.AntigravityRoot = f.path("brain")
	f.cfg.Unplaceable = "allow"
	f.restart()

	logs := filepath.Join(f.cfg.AntigravityRoot, agySession, ".system_generated", "logs")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logs, "transcript.jsonl")
	if err := os.WriteFile(path, []byte(agyInput), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := f.path("cache")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cache := `{"conversations":{"` + agySession + `":{"is_internal":true,"summary":{"WorkspaceURIs":[]}}}}`
	if err := os.WriteFile(filepath.Join(dir, "conversation_metadata.json"), []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}

	f.once()
	if _, ok := f.rec.spec(path); ok {
		t.Fatal("cache-only internal conversation was handed to sync")
	}
	if got := f.find("Antigravity canonical test prompt", false); len(got) != 1 {
		t.Fatalf("locally searchable Antigravity matches = %d, want 1", len(got))
	}
}
