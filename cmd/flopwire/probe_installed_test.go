package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

func TestProbeAsInstalledFlag(t *testing.T) {
	o, err := parseProbeFlags([]string{"--as-installed", "--local"}, &strings.Builder{})
	if err != nil || !o.asInstalled {
		t.Fatalf("%+v %v", o, err)
	}
	if o, _ := parseProbeFlags(nil, &strings.Builder{}); o.asInstalled {
		t.Fatal("as-installed by default")
	}
}

// TestProbeInstallPlugins: the scratch marketplace holds both plugins of
// this binary, the shim executable, Codex's marketplace file, and this
// binary's path recorded where the shim looks with the probe's
// FLOPWIRE_CONFIG.
func TestProbeInstallPlugins(t *testing.T) {
	p := &prober{dir: t.TempDir(), o: probeOpts{asInstalled: true}, sock: "/tmp/s.sock"}
	if err := p.installPlugins(); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"claude-code/flopwire", "codex/flopwire"} {
		shim := filepath.Join(p.pluginSrc(), "plugins", d, "bin", "flopwire-hook")
		if st, err := os.Stat(shim); err != nil || st.Mode().Perm()&0o100 == 0 {
			t.Fatalf("%s: %v", shim, err)
		}
		for _, f := range []string{"hooks/hooks.json", ".mcp.json", "skills/messaging/SKILL.md"} {
			if _, err := os.Stat(filepath.Join(p.pluginSrc(), "plugins", d, f)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(p.pluginSrc(), "plugins", "claude-code", "flopwire", ".claude-plugin", "plugin.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.pluginSrc(), codexMarketplaceFile)); err != nil {
		t.Fatal(err)
	}
	self, _ := selfPath()
	if got := readRecordedBinary(filepath.Join(p.dir, "flopwire", binaryPathFile)); got != self {
		t.Fatalf("recorded %q, want %q", got, self)
	}
	// The harness environment points the shim and the hook there.
	var cfg, tap, bin string
	for _, kv := range p.harnessEnv(transcript.AgentOpencode) {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "FLOPWIRE_CONFIG":
			cfg = v
		case envProbeTap:
			tap = v
		case "FLOPWIRE_BIN":
			bin = v
		}
	}
	if cfg != filepath.Join(p.dir, "flopwire", "config.json") || tap != filepath.Join(p.dir, "opencode", "tap.jsonl") || bin != "" {
		t.Fatalf("opencode env: config %q tap %q bin %q", cfg, tap, bin)
	}
}

// TestHookTapsItselfUnderProbe: with FLOPWIRE_PROBE_TAP set, `flopwire
// hook` logs the event, the binary that ran and how the shim found it.
func TestHookTapsItselfUnderProbe(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "tap.jsonl")
	t.Setenv(envProbeTap, log)
	t.Setenv(envHookVia, "recorded")
	t.Setenv("FLOPWIRE_SOCKET", filepath.Join(dir, "none.sock"))
	in := filepath.Join(dir, "in.json")
	if err := os.WriteFile(in, []byte(`{"hook_event_name":"Stop","session_id":"s1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	stdin := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = stdin }()
	if err := hookMain(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	entries, err := readTap(log)
	self, _ := selfPath()
	if err != nil || len(entries) != 1 || entries[0].Event != "Stop" || entries[0].Session != "s1" || entries[0].Binary != self || entries[0].Via != "recorded" {
		t.Fatalf("tap: %v %+v", err, entries)
	}
}

func TestHookBinaryResult(t *testing.T) {
	self, _ := selfPath()
	r := &harnessRun{p: &prober{exe: self}, name: transcript.AgentCodex}
	e := func(ev, bin, via string) tapEntry { return tapEntry{Event: ev, Binary: bin, Via: via} }
	for _, c := range []struct {
		name    string
		entries []tapEntry
		pass    bool
		want    string
	}{
		{"all through the shim", []tapEntry{e("SessionStart", self, "recorded"), e("PreToolUse", "", ""), e("Stop", self, "recorded")}, true, "2 hooks via recorded path"},
		{"none", []tapEntry{e("PreToolUse", "", "")}, false, "no plugin hook reached flopwire hook"},
		{"another binary", []tapEntry{e("SessionStart", "/usr/local/bin/flopwire", "path")}, false, `ran "/usr/local/bin/flopwire" via "path"`},
		{"not through the shim", []tapEntry{e("SessionStart", self, "")}, false, "did not run"},
	} {
		res := r.hookBinaryResult(c.entries)
		if res.Pass != c.pass || !strings.Contains(res.Evidence, c.want) || res.Case != caseHookBinary {
			t.Errorf("%s: %+v", c.name, res)
		}
	}
}

// fakeSession is a probeSession with only an id.
type fakeSession struct{ probeSession }

func (fakeSession) ID() string { return "s1" }

// TestProbeSettleWaitsForAsyncStop: Codex runs the plugin's async Stop
// hook after it reports the turn done; settle waits until every turn of
// the recipient has its Stop, and no longer.
func TestProbeSettleWaitsForAsyncStop(t *testing.T) {
	tap := filepath.Join(t.TempDir(), "tap.jsonl")
	r := &harnessRun{tap: tap, recv: fakeSession{}}
	for _, e := range []tapEntry{
		{At: 1, Event: evUserPromptSubmit, Session: "s1"}, {At: 2, Event: "Stop", Session: "s1"},
		{At: 3, Event: evUserPromptSubmit, Session: "s1"},
		{At: 4, Event: evUserPromptSubmit, Session: "s1", AgentID: "sub"}, // a subagent's prompt has no Stop
		{At: 5, Event: "Stop", Session: "other"},
	} {
		if err := appendTap(tap, e); err != nil {
			t.Fatal(err)
		}
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = appendTap(tap, tapEntry{At: 6, Event: "Stop", Session: "s1"})
	}()
	start := time.Now()
	r.settle(context.Background())
	if d := time.Since(start); d < 250*time.Millisecond || d > 5*time.Second {
		t.Fatalf("settle returned after %s; the Stop came after 300ms", d)
	}
}
