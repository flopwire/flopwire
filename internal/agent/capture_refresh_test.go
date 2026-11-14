package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/transcript"
)

// Ordinary captures must not reread unrelated desktop metadata or scan Claude
// history. Count work rather than imposing a machine-dependent timing limit.
func TestCaptureAuthorizationSkipsUnrelatedDesktopDiscovery(t *testing.T) {
	a, path := captureRefreshFixture(t, 16)
	for _, collector := range []transcript.Agent{transcript.AgentCodex, transcript.AgentDevin, transcript.AgentOpencode} {
		t.Run(string(collector), func(t *testing.T) {
			before := captureRefreshCalls(a)
			for range 8 {
				auth, err := a.authorizeCapture(ctx, devicesync.SourceSpec{Path: path, Agent: collector, SessionKey: "ordinary"})
				if err != nil || auth != nil {
					t.Fatalf("ordinary authorization: auth=%+v err=%v", auth, err)
				}
			}
			if calls := captureRefreshCalls(a) - before; calls != 0 {
				t.Fatalf("8 unrelated captures triggered %d global desktop refreshes, want 0", calls)
			}
		})
	}
}

func captureRefreshCalls(a *Agent) uint64 {
	for _, op := range a.Diagnostics().Operations {
		if op.Name == "refresh_cowork" {
			return op.Calls
		}
	}
	panic("missing desktop refresh diagnostic")
}

func TestCaptureAuthorizationStillRefreshesClaudeAndAppContainers(t *testing.T) {
	a, ordinary := captureRefreshFixture(t, 1)
	for _, sp := range []devicesync.SourceSpec{
		{Path: ordinary, Agent: transcript.AgentClaude},
		{Path: filepath.Join(a.cfg.CoworkRoot, "mismatched.jsonl"), Agent: transcript.AgentCodex},
		{Path: filepath.Join(a.cfg.DesktopCodeRoot, "mismatched.jsonl"), Agent: transcript.AgentCodex},
	} {
		before := captureRefreshCalls(a)
		auth, err := a.authorizeCapture(ctx, sp)
		if auth != nil && auth.Release != nil {
			auth.Release()
		}
		if a.nativeEvidenceRoot(sp.Path) != "" && (auth != nil || err == nil) {
			t.Fatalf("mismatched app-contained source authorized: auth=%+v err=%v", auth, err)
		}
		if calls := captureRefreshCalls(a) - before; calls != 1 {
			t.Fatalf("agent=%s app-contained=%v: refreshes=%d, want 1", sp.Agent, a.nativeEvidenceRoot(sp.Path) != "", calls)
		}
	}
}

func TestCaptureAuthorizationUnrelatedCollectorStillRefreshesPathPolicy(t *testing.T) {
	f, rules, _ := rulesFixture(t, "-")
	f.once()
	sp, ok := f.rec.spec(f.path(codexActive))
	if !ok {
		t.Fatal("Codex fixture was not offered for upload")
	}
	before := captureRefreshCalls(f.a)
	if auth, err := f.a.authorizeCapture(ctx, sp); err != nil || auth != nil {
		t.Fatalf("initial authorization: auth=%+v err=%v", auth, err)
	}
	writeRules(t, rules, "deny /tmp/oracle-gamma")
	if auth, err := f.a.authorizeCapture(ctx, sp); err == nil || auth != nil {
		t.Fatalf("changed path policy authorized Codex capture: auth=%+v err=%v", auth, err)
	}
	if calls := captureRefreshCalls(f.a) - before; calls != 0 {
		t.Fatalf("path-policy checks triggered %d unrelated desktop refreshes", calls)
	}
}

// Both sides use fresh, synthetic app metadata and the same ordinary upload.
// Setup and discovery are excluded; one operation is one capture authorization.
func BenchmarkCaptureAuthorizationUnrelatedDesktopHistory(b *testing.B) {
	for _, sessions := range []int{0, 64} {
		b.Run(fmt.Sprintf("desktop_sessions=%d", sessions), func(b *testing.B) {
			a, path := captureRefreshFixture(b, sessions)
			sp := devicesync.SourceSpec{Path: path, Agent: transcript.AgentCodex, SessionKey: "ordinary"}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				auth, err := a.authorizeCapture(context.Background(), sp)
				if err != nil || auth != nil {
					b.Fatalf("ordinary authorization: auth=%+v err=%v", auth, err)
				}
			}
		})
	}
}

func captureRefreshFixture(t testing.TB, sessions int) (*Agent, string) {
	t.Helper()
	home := t.TempDir()
	store, err := localindex.Open(filepath.Join(home, "index.db"), localindex.Options{DeferCommit: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	cfg := Config{Home: home, ClaudeProjects: filepath.Join(home, "cli-projects"),
		CoworkRoot: filepath.Join(home, "cowork"), DesktopCodeRoot: filepath.Join(home, "desktop-code"), CodexHome: filepath.Join(home, "codex"),
		DevinDB: "-", OpencodeDB: "-", OpencodeRegistry: "-", Unplaceable: "upload",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for i := range sessions {
		path := filepath.Join(cfg.CoworkRoot, "account", "workspace", fmt.Sprintf("session-%d.json", i))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		metadata := fmt.Sprintf(`{"sessionId":"session-%d","cliSessionId":"11111111-2222-4333-8444-%012d","userSelectedFolders":["/synthetic/selected"]}`, i, i)
		if err := os.WriteFile(path, []byte(metadata), 0600); err != nil {
			t.Fatal(err)
		}
	}
	a := New(store, cfg)
	if err := a.Once(ctx); err != nil {
		t.Fatal(err)
	}
	return a, filepath.Join(home, "ordinary.jsonl")
}
