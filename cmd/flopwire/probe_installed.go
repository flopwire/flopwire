package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/plugins"
)

// probe --as-installed: each harness runs the Flopwire plugin's own hook
// commands, as a user's install does: the shim by the harness's plugin
// root variable, through the harness's shell, finding the binary the way
// it would on the user's machine (here: the path recorded in the scratch
// config directory, as flopwire setup records it). The default probe
// calls the binary by absolute path from project hooks, which hides how
// the plugin finds the binary. The plugins come from this binary
// (plugins.FS), so the run tests the plugin of the same commit:
//
//   - Claude Code: `claude --plugin-dir`, for the probe's sessions only.
//   - Codex: a marketplace in the scratch directory, installed with codex
//     plugin marketplace add and codex plugin add in the scratch
//     CODEX_HOME; the probe trusts the plugin's hooks there.
//   - Devin: devin plugins install --local in the scratch HOME.
//   - opencode: the plugin file without FLOPWIRE_BIN, so it searches.
//
// `flopwire hook` taps itself into the harness's hook log when
// FLOPWIRE_PROBE_TAP is set (hookMain), recording which binary ran and
// how the shim found it; the hook-binary case checks those.

// caseHookBinary is the case --as-installed adds per harness.
const caseHookBinary = "hook-binary"

// pluginSrc is the scratch marketplace root holding the plugins.
func (p *prober) pluginSrc() string { return filepath.Join(p.dir, "plugin-src") }

// installPlugins writes the plugins into the scratch marketplace and
// records this binary's path in the scratch config directory.
func (p *prober) installPlugins() error {
	root := p.pluginSrc()
	err := fs.WalkDir(plugins.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dst := filepath.Join(root, "plugins", filepath.FromSlash(path))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := plugins.FS.ReadFile(path)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(path, "/"+plugins.ShimPath) {
			mode = 0o755
		}
		return os.WriteFile(dst, b, mode)
	})
	if err != nil {
		return fmt.Errorf("probe: write the plugins: %w", err)
	}
	mkt := map[string]any{"name": codexMarketplace, "interface": map[string]any{"displayName": "Flopwire"},
		"plugins": []any{map[string]any{"name": "flopwire", "source": map[string]any{"source": "local", "path": "./plugins/" + plugins.CodexDir},
			"policy": map[string]any{"installation": "AVAILABLE", "authentication": "ON_INSTALL"}, "category": "Productivity"}}}
	b, _ := json.MarshalIndent(mkt, "", "  ")
	if err := os.MkdirAll(filepath.Join(root, filepath.Dir(codexMarketplaceFile)), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, codexMarketplaceFile), b, 0o644); err != nil {
		return err
	}
	exe, err := selfPath()
	if err != nil {
		return err
	}
	_, err = recordBinary(filepath.Join(p.dir, "flopwire", binaryPathFile), exe)
	return err
}

// installPlugin installs the plugin into the harness's scratch home with
// the harness's own command, as flopwire setup does.
func (r *harnessRun) installPlugin(ctx context.Context) error {
	run := func(name string, args ...string) error {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Env, cmd.Dir = r.env, r.proj
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	switch r.name {
	case transcript.AgentCodex:
		if err := run("codex", "plugin", "marketplace", "add", r.p.pluginSrc(), "--json"); err != nil {
			return err
		}
		return run("codex", "plugin", "add", codexPlugin, "--json")
	case transcript.AgentDevin:
		return run("devin", "plugins", "install", "--local", filepath.Join(r.p.pluginSrc(), "plugins", filepath.FromSlash(plugins.ClaudeCodeDir)), "-y")
	}
	return nil
}

// claudePluginArgs are the extra claude arguments for the harness run.
func (r *harnessRun) claudePluginArgs() []string {
	if !r.p.o.asInstalled {
		return nil
	}
	return []string{"--plugin-dir", filepath.Join(r.p.pluginSrc(), "plugins", filepath.FromSlash(plugins.ClaudeCodeDir))}
}

// hookBinaryResult judges the hook-binary case from the hook log: every
// plugin hook ran this binary, found through the shim (or the opencode
// plugin's own search), which said how.
func (r *harnessRun) hookBinaryResult(entries []tapEntry) probeResult {
	res := probeResult{Harness: string(r.name), Case: caseHookBinary}
	n := 0
	vias := map[string]int{}
	var bad []string
	for _, e := range entries {
		if !slices.Contains(probeHookEvents, e.Event) {
			continue
		}
		n++
		vias[e.Via]++
		if e.Via == "" || e.Binary == "" || !samePath(e.Binary, r.p.exe) {
			bad = append(bad, fmt.Sprintf("%s ran %q via %q", e.Event, e.Binary, e.Via))
		}
	}
	switch {
	case n == 0:
		res.Evidence = "no plugin hook reached flopwire hook: the plugin's hook command found no binary (see the harness's stderr log)"
	case len(bad) > 0:
		res.Evidence = fmt.Sprintf("%d of %d plugin hooks did not run %s through the shim: %s", len(bad), n, r.p.exe, strings.Join(bad[:min(len(bad), 3)], "; "))
	default:
		var vs []string
		for _, v := range slices.Sorted(maps.Keys(vias)) {
			vs = append(vs, fmt.Sprintf("%d %s via %s", vias[v], plural(vias[v], "hook", "hooks"), hookVia[v]))
		}
		res.Pass = true
		res.Evidence = "the plugin's hook command ran this probe's flopwire: " + strings.Join(vs, ", ")
	}
	return res
}
