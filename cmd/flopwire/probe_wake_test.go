package main

import (
	"encoding/json"
	"github.com/flopwire/flopwire/internal/transcript"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeWakeProbeRequiresExplicitSelectionAndLocalAgent(t *testing.T) {
	var log strings.Builder
	defaults, err := parseProbeFlags(nil, &log)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range defaults.cases {
		if c == caseNativeWake {
			t.Fatal("default probe enables paid native wakes")
		}
	}
	if _, err := parseProbeFlags([]string{"--harness", "claude", "--case", "native-wake"}, &log); err == nil {
		t.Fatal("native wake allowed on user's existing agent")
	}
	opts, err := parseProbeFlags([]string{"--harness", "claude,codex", "--case", "native-wake", "--local"}, &log)
	if err != nil || len(opts.cases) != 1 || opts.cases[0] != caseNativeWake {
		t.Fatalf("native wake flags: %+v %v", opts, err)
	}
	for _, h := range []transcript.Agent{transcript.AgentClaude, transcript.AgentCodex} {
		if len(casesFor(h, opts.cases)) != 1 {
			t.Fatalf("supported harness omitted: %s", h)
		}
	}
	for _, h := range []transcript.Agent{transcript.AgentDevin, transcript.AgentOpencode} {
		if len(casesFor(h, opts.cases)) != 0 {
			t.Fatalf("unsupported harness enabled: %s", h)
		}
	}
}

func TestNativeWakeUpdaterCleanupRefusesUnrelatedProcess(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "app-server-daemon")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "processStartTime": "not this process"})
	if err := os.WriteFile(filepath.Join(dir, "daemon-updater.pid"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := stopProbeDaemonUpdater(t.Context(), home); err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("unrelated process must be left untouched: %v", err)
	}
}
