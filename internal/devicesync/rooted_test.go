//go:build darwin || linux

package devicesync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

func TestRootedCaptureRejectsReplacedFile(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	spec := e.spec("transcript.jsonl", transcript.StorageJSONLAppend)
	spec.Root = filepath.Dir(spec.Path)
	spec.Agent = transcript.AgentAntigravity
	if err := os.WriteFile(spec.Path, []byte("synthetic transcript\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.sync(spec)
	if err := os.Remove(spec.Path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	if err := os.WriteFile(outside, []byte("synthetic outside content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, spec.Path); err != nil {
		t.Fatal(err)
	}
	if err := e.sy.Sync(context.Background(), spec); err == nil {
		t.Fatal("captured an Antigravity transcript through a symlink")
	}
}
