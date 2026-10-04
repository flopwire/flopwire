package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

// #102: what the agent hands sync carries the session's repository as
// placed (main checkout and remote), and a placement whose repository
// changes later (the recovery pass) is handed over again, so the server
// hears it.
func TestSpecCarriesRepository(t *testing.T) {
	f := newFixture(t, "-")
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "init", "-q")
	gitRun(t, repo, "remote", "add", "origin", "git@github.com:acme/web.git")
	const id = "0b7e2c1a-0000-4000-8000-0000000000e1"
	line := fmt.Sprintf(`{"parentUuid":null,"isSidechain":false,"userType":"external","cwd":%q,"sessionId":%q,"version":"2.1.0","type":"user","message":{"role":"user","content":"hello"},"uuid":"e1000000-0000-4000-8000-000000000001","timestamp":"2026-09-23T11:00:00.000Z"}`+"\n", repo, id)
	dir := f.path(".claude/projects/-web")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	f.once()
	sp, ok := f.rec.spec(path)
	if !ok || sp.Checkout != repo || sp.Remote != "github.com/acme/web" {
		t.Fatalf("spec: %+v %v", sp, ok)
	}
	// The recovery pass places it in another checkout: handed over again.
	f.rec.mu.Lock()
	delete(f.rec.notify, path)
	f.rec.mu.Unlock()
	key := placeKey{transcript.AgentClaude, id}
	old, _ := f.a.storedPlace(key)
	next := old
	next.pl.Main = "/elsewhere/web"
	if !f.a.replacePlace(key, old, next) {
		t.Fatal("replacePlace")
	}
	if sp, ok := f.rec.spec(path); !ok || sp.Checkout != "/elsewhere/web" {
		t.Fatalf("after the placement changed: %+v %v", sp, ok)
	}
}
