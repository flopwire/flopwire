package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/flopwire/flopwire/internal/agent"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedFrozenCorpus reuses the private copied corpus without sampling live homes.
// Its manifest excludes all scenario-mutated oracle and growth fixtures.
func (d *device) seedFrozenCorpus(t *testing.T, root string) corpusStats {
	var manifest []struct {
		Path   string `json:"path"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	}
	raw, err := os.ReadFile(filepath.Join(root, "corpus-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	var st corpusStats
	source := os.Getenv("FLOPWIRE_E2E_FROZEN_SOURCE")
	if source == "" {
		t.Fatal("FLOPWIRE_E2E_FROZEN_SOURCE required")
	}
	for _, entry := range manifest {
		if !filepath.IsLocal(entry.Path) {
			t.Fatal("nonlocal corpus path")
		}
		data, err := os.ReadFile(filepath.Join(source, entry.Path))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if int64(len(data)) != entry.Size || hex.EncodeToString(sum[:]) != entry.SHA256 {
			t.Fatal("frozen corpus checksum mismatch")
		}
		target := filepath.Join(d.home, entry.Path)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, data, 0600); err != nil {
			t.Fatal(err)
		}
		st.bytes += entry.Size
		if strings.HasPrefix(entry.Path, ".codex/") {
			st.codex++
		}
		if strings.HasPrefix(entry.Path, ".claude/projects/") && strings.HasSuffix(entry.Path, ".jsonl") && strings.Count(entry.Path, "/") == 3 {
			st.claude++
		}
	}
	st.devin = d.mergeDevinSample(t, filepath.Join(root, "devin-corpus.db"), 25)
	return st
}

// sampleOwnedDiagnostics only touches sockets created by this harness.
func sampleOwnedDiagnostics(h *harness) {
	f, err := os.OpenFile(filepath.Join(h.cfg.root, "agent-diagnostics.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		h.t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				for n := 1; n <= 2; n++ {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					response, err := agent.Call(ctx, filepath.Join(h.sockDir, fmt.Sprintf("d%d.sock", n)), agent.Request{Op: "diagnostics"})
					cancel()
					sample := map[string]any{"time": time.Now().UTC(), "device": n, "response": response}
					if err != nil {
						sample["error"] = err.Error()
					}
					_ = json.NewEncoder(f).Encode(sample)
				}
			}
		}
	}()
	h.t.Cleanup(func() { close(stop); <-done; _ = f.Close() })
}
