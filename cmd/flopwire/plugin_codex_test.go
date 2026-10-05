package main

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The Codex plugin in plugins/codex/flopwire, checked against what Codex
// 0.159.3 requires (codex-rs/core-plugins: manifest.rs, marketplace.rs) and
// against #75's hook contract.

const codexPluginDir = "../../plugins/codex/flopwire"

func TestCodexMarketplaceManifest(t *testing.T) {
	var mkt struct {
		Name    string `json:"name"`
		Plugins []struct {
			Name   string `json:"name"`
			Source struct {
				Source string `json:"source"`
				Path   string `json:"path"`
			} `json:"source"`
			Policy struct {
				Installation   string `json:"installation"`
				Authentication string `json:"authentication"`
			} `json:"policy"`
		} `json:"plugins"`
	}
	readJSONFile(t, filepath.Join("../..", codexMarketplaceFile), &mkt)
	if mkt.Name != codexMarketplace || len(mkt.Plugins) != 1 {
		t.Fatalf("marketplace.json: name %q and one plugin required; got %+v", mkt.Name, mkt)
	}
	p := mkt.Plugins[0]
	if p.Name+"@"+mkt.Name != codexPlugin {
		t.Fatalf("plugin id %s@%s, setup installs %s", p.Name, mkt.Name, codexPlugin)
	}
	// Codex: a local source must start with ./ and stay inside the root.
	if p.Source.Source != "local" || !strings.HasPrefix(p.Source.Path, "./") || strings.Contains(p.Source.Path, "..") {
		t.Fatalf("source %+v: want {source: local, path: ./…} without ..", p.Source)
	}
	if filepath.Clean(filepath.Join("../..", p.Source.Path)) != filepath.Clean(codexPluginDir) {
		t.Fatalf("source %q is not %s", p.Source.Path, codexPluginDir)
	}
	if p.Policy.Installation != "AVAILABLE" || !slices.Contains([]string{"ON_INSTALL", "ON_USE"}, p.Policy.Authentication) {
		t.Fatalf("policy %+v", p.Policy)
	}
	// Each sparse path setup clones must exist.
	for _, s := range codexSparse {
		if _, err := os.Stat(filepath.Join("../..", s)); err != nil {
			t.Errorf("sparse path %s: %v", s, err)
		}
	}
}

func TestCodexPluginManifest(t *testing.T) {
	var man struct {
		Name       string `json:"name"`
		Version    string `json:"version"`
		Skills     string `json:"skills"`
		MCPServers string `json:"mcpServers"`
		Hooks      string `json:"hooks"`
		Interface  struct {
			DisplayName string `json:"displayName"`
		} `json:"interface"`
	}
	readJSONFile(t, filepath.Join(codexPluginDir, ".codex-plugin", "plugin.json"), &man)
	if man.Name != "flopwire" || man.Interface.DisplayName == "" {
		t.Fatalf("plugin.json: %+v", man)
	}
	// No version: Codex then caches the plugin as "local" and setup
	// reinstalls it on every run (pre-release).
	if man.Version != "" {
		t.Fatalf("plugin.json pins version %q", man.Version)
	}
	// Codex ignores a component path that does not start with ./ .
	for field, p := range map[string]string{"skills": man.Skills, "mcpServers": man.MCPServers, "hooks": man.Hooks} {
		if !strings.HasPrefix(p, "./") || strings.Contains(p, "..") {
			t.Errorf("%s %q: want ./ relative to the plugin root", field, p)
			continue
		}
		if _, err := os.Stat(filepath.Join(codexPluginDir, p)); err != nil {
			t.Errorf("%s: %v", field, err)
		}
	}
}

// codexHookContract is the plugin's hook definition, exactly. Codex's
// trust hash covers event, matcher, command and timeout: change any of
// them and every user must approve the hooks again. SessionEnd is 3:
// Codex clamps a SessionEnd timeout above 3 s and prints "clamping
// SessionEnd hook timeout to 3s" at the start of every session
// (codex-rs/hooks discovery.rs, 0.160.0).
var codexHookContract = map[string]struct {
	matcher, command string
	timeout          int
	async            bool
}{
	"SessionStart":     {"", codexHookCommand, 5, false},
	"UserPromptSubmit": {"", codexHookCommand, 5, false},
	"PostToolUse":      {"*", codexHookCommand, 5, false},
	// Async: see TestCodexStopHookSurvivesShellNoise.
	"Stop":       {"", codexHookCommand, 5, true},
	"SessionEnd": {"", codexHookCommand, 3, false},
}

// codexHookCommand runs the shim by Codex's plugin root. Codex hashes it
// as written, so the trust survives a reinstall to another path.
const codexHookCommand = `/bin/sh "${PLUGIN_ROOT}/bin/flopwire-hook" hook`

func TestCodexPluginHooks(t *testing.T) {
	var hf struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
				Async   bool   `json:"async"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	readJSONFile(t, filepath.Join(codexPluginDir, "hooks", "hooks.json"), &hf)
	if got, want := slices.Sorted(maps.Keys(hf.Hooks)), slices.Sorted(maps.Keys(codexHookContract)); !slices.Equal(got, want) {
		t.Fatalf("hook events %q, want %q (never PreToolUse)", got, want)
	}
	cmds := subcommands()
	for ev, groups := range hf.Hooks {
		if len(groups) != 1 || len(groups[0].Hooks) != 1 {
			t.Fatalf("%s: want one matcher group with one hook; got %+v", ev, groups)
		}
		g, h, want := groups[0], groups[0].Hooks[0], codexHookContract[ev]
		if g.Matcher != want.matcher || h.Type != "command" || h.Command != want.command || h.Timeout != want.timeout || h.Async != want.async {
			t.Errorf("%s: matcher %q command %q timeout %d async %v; the contract is %+v", ev, g.Matcher, h.Command, h.Timeout, h.Async, want)
		}
		if m := pluginCommandRe.FindStringSubmatch(h.Command); m == nil || m[1] != "hook" || !cmds[m[1]] {
			t.Errorf("%s runs %q: not flopwire hook", ev, h.Command)
		}
	}
}

// TestCodexStopHookSurvivesShellNoise: Codex runs a hook command through
// the user's shell (`zsh -c`, which reads ~/.zshenv; `$SHELL -lc` when
// it knows no shell), so whatever the shell's startup files print lands
// on the hook's stdout ahead of flopwire hook. No hook command can take
// it back. A synchronous Stop hook fails on any stdout that is not one
// JSON object ("hook returned invalid stop hook JSON output",
// codex-rs/hooks/src/events/stop.rs); an async one is checked only when
// its stdout starts like JSON. So the plugin's Stop hook is async.
func TestCodexStopHookSurvivesShellNoise(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	var hf struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
				Async   bool   `json:"async"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	readJSONFile(t, filepath.Join(codexPluginDir, "hooks", "hooks.json"), &hf)
	dir := t.TempDir()
	// A shell whose startup file prints, as Codex would run it: SHELL -c CMD.
	noisy := filepath.Join(dir, "noisy-sh")
	if err := os.WriteFile(noisy, []byte("#!"+sh+"\necho 'Welcome back!'\nexec "+sh+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := hf.Hooks["Stop"][0].Hooks[0]
	// A flopwire hook that prints nothing: the noise is all there is.
	quiet := t.TempDir()
	if err := os.WriteFile(filepath.Join(quiet, "flopwire"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs(codexPluginDir)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(noisy, "-c", h.Command)
	cmd.Env = []string{"PATH=" + quiet, "HOME=" + t.TempDir(), "FLOPWIRE_CONFIG=" + filepath.Join(t.TempDir(), "config.json"), "PLUGIN_ROOT=" + root}
	cmd.Stdin = strings.NewReader(`{"hook_event_name":"Stop","session_id":"s"}`)
	out, err := cmd.Output()
	if err != nil || len(bytes.TrimSpace(out)) == 0 {
		t.Fatalf("the noisy shell: %v, stdout %q; want exit 0 and the noise", err, out)
	}
	if reason := codexStopFailure(h.Async, out); reason != "" {
		t.Fatalf("Codex fails the Stop hook (async=%v) behind a shell that prints %q: %s", h.Async, out, reason)
	}
}

// codexStopFailure is why Codex 0.160 fails a Stop hook that exited 0
// with this stdout, or "" when it does not.
func codexStopFailure(async bool, stdout []byte) string {
	s := bytes.TrimSpace(stdout)
	if len(s) == 0 {
		return ""
	}
	var obj map[string]any
	if json.Unmarshal(s, &obj) == nil {
		return ""
	}
	if !async || s[0] == '{' || s[0] == '[' {
		return "hook returned invalid stop hook JSON output"
	}
	return ""
}

func TestCodexPluginHooksThroughShim(t *testing.T) {
	testHooksThroughShim(t, codexPluginDir, "PLUGIN_ROOT")
}

// codexMCPEnvVars are the variables that choose where flopwire finds its
// config (and so the agent's socket) and its index. Codex starts an MCP
// server with only HOME, PATH, USER and a few more, so without env_vars a
// user who sets one of these gets an MCP server that looks in another
// place than the agent and the hooks: agent_not_running and "no index
// yet" for good.
var codexMCPEnvVars = []string{"FLOPWIRE_CONFIG", "FLOPWIRE_INDEX", "XDG_CACHE_HOME", "XDG_CONFIG_HOME"}

func TestCodexPluginMCP(t *testing.T) {
	var m struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
			EnvVars []string `json:"env_vars"`
		} `json:"mcpServers"`
	}
	readJSONFile(t, filepath.Join(codexPluginDir, ".mcp.json"), &m)
	s, ok := m.MCPServers["flopwire"]
	if len(m.MCPServers) != 1 || !ok || s.Command != "flopwire" || !slices.Equal(s.Args, []string{"mcp"}) {
		t.Fatalf(".mcp.json: want one server flopwire = flopwire mcp; got %+v", m)
	}
	if got := slices.Sorted(slices.Values(s.EnvVars)); !slices.Equal(got, codexMCPEnvVars) {
		t.Errorf(".mcp.json env_vars %q, want %q: Codex passes an MCP server no other variables", got, codexMCPEnvVars)
	}
}

// TestCodexSkillMatchesClaude: one skill text for both plugins. Each
// harness installs a copy of its plugin directory, so the file cannot be a
// link; edit plugins/claude-code/flopwire/skills/messaging/SKILL.md and
// copy it over.
func TestCodexSkillMatchesClaude(t *testing.T) {
	a, err := os.ReadFile(filepath.Join(claudePluginDir, "skills", "messaging", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(codexPluginDir, "skills", "messaging", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("the Codex skill differs from the Claude Code skill; copy %s/skills/messaging/SKILL.md to %s/skills/messaging/SKILL.md", claudePluginDir, codexPluginDir)
	}
}

// TestCodexPluginLoads installs the plugin into a throwaway CODEX_HOME with
// the real codex, when it is installed (not in CI), and asks Codex what it
// loaded: the plugin, its MCP server and its four hooks, untrusted.
func TestCodexPluginLoads(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	codex, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex is not installed")
	}
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", t.TempDir())
	cwd := t.TempDir()
	run := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command(codex, args...)
		cmd.Dir = cwd
		out, err := cmd.Output()
		if err != nil {
			var stderr []byte
			if ee, ok := err.(*exec.ExitError); ok {
				stderr = ee.Stderr
			}
			t.Fatalf("codex %s: %v\n%s\n%s", strings.Join(args, " "), err, out, stderr)
		}
		return out
	}
	run("plugin", "marketplace", "add", repo, "--json")
	run("plugin", "add", codexPlugin, "--json")
	var list struct {
		Installed []codexPluginEntry `json:"installed"`
	}
	if err := json.Unmarshal(run("plugin", "list", "--json"), &list); err != nil || findCodexPlugin(list.Installed) == nil {
		t.Fatalf("codex plugin list: %v %+v", err, list)
	}
	var servers []struct {
		Name      string `json:"name"`
		Transport struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"transport"`
	}
	if err := json.Unmarshal(run("mcp", "list", "--json"), &servers); err != nil || !slices.ContainsFunc(servers, func(s struct {
		Name      string `json:"name"`
		Transport struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"transport"`
	}) bool {
		return s.Name == "flopwire" && s.Transport.Command == "flopwire" && slices.Equal(s.Transport.Args, []string{"mcp"})
	}) {
		t.Fatalf("codex mcp list has no flopwire server: %v %+v", err, servers)
	}
	res, err := codexRPC(t.Context(), codex, []rpcCall{{method: "hooks/list", params: map[string]any{"cwds": []string{cwd}}}})
	if err != nil {
		t.Fatal(err)
	}
	var hl struct {
		Data []struct {
			Hooks    []codexHook `json:"hooks"`
			Warnings []string    `json:"warnings"`
			Errors   []any       `json:"errors"`
		} `json:"data"`
	}
	if err := json.Unmarshal(res[0], &hl); err != nil || len(hl.Data) != 1 {
		t.Fatalf("hooks/list: %v %s", err, res[0])
	}
	d := hl.Data[0]
	if len(d.Warnings) != 0 || len(d.Errors) != 0 {
		t.Fatalf("Codex reports hook problems: %q %v", d.Warnings, d.Errors)
	}
	var events []string
	for _, h := range d.Hooks {
		if h.PluginID != codexPlugin {
			continue
		}
		events = append(events, codexEventName(h.EventName))
		// hooks/list shows the command with ${PLUGIN_ROOT} expanded to
		// the cached copy; the hash covers it as written.
		want := `/bin/sh "` + filepath.Join(os.Getenv("CODEX_HOME"), "plugins", "cache", "flopwire", "flopwire", "local", "bin", "flopwire-hook") + `" hook`
		if !samePath(strings.TrimSuffix(strings.TrimPrefix(h.Command, `/bin/sh "`), `" hook`), strings.TrimSuffix(strings.TrimPrefix(want, `/bin/sh "`), `" hook`)) || h.TrustStatus != "untrusted" || !h.Enabled {
			t.Errorf("hook %+v, want command %s", h, want)
		}
		// The trust key is the plugin id and the hooks file's relative
		// path, not the install path, so a reinstall keeps the trust.
		if !strings.HasPrefix(h.Key, "flopwire@flopwire:hooks/hooks.json:") {
			t.Errorf("hook key %q", h.Key)
		}
	}
	slices.Sort(events)
	if want := slices.Sorted(maps.Keys(codexHookContract)); !slices.Equal(events, want) {
		t.Fatalf("Codex loaded hooks %q, want %q", events, want)
	}
}
