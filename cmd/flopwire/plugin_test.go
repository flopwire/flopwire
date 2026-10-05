package main

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The Claude Code plugin in plugins/claude-code/flopwire, checked against
// what Claude Code's plugin documentation requires and against #75's hook
// contract.

const claudePluginDir = "../../plugins/claude-code/flopwire"

func readJSONFile(t *testing.T, p string, v any) {
	t.Helper()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(v); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
}

// subcommands are the commands usageText lists.
func subcommands() map[string]bool { return usageCommands(usageText) }

func TestClaudeMarketplaceManifest(t *testing.T) {
	var mkt struct {
		Name  string `json:"name"`
		Owner struct {
			Name string `json:"name"`
		} `json:"owner"`
		Plugins []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"plugins"`
	}
	readJSONFile(t, "../../.claude-plugin/marketplace.json", &mkt)
	if mkt.Name != claudeMarketplace || mkt.Owner.Name == "" || len(mkt.Plugins) != 1 {
		t.Fatalf("marketplace.json: name, owner and one plugin required; got %+v", mkt)
	}
	p := mkt.Plugins[0]
	if p.Name+"@"+mkt.Name != claudePlugin {
		t.Fatalf("plugin id %s@%s, setup installs %s", p.Name, mkt.Name, claudePlugin)
	}
	if !strings.HasPrefix(p.Source, "./") || strings.Contains(p.Source, "..") {
		t.Fatalf("source %q: a relative path from the marketplace root, starting ./ and without ..", p.Source)
	}
	if filepath.Clean(filepath.Join("../..", p.Source)) != filepath.Clean(claudePluginDir) {
		t.Fatalf("source %q is not %s", p.Source, claudePluginDir)
	}
	var man struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Author  struct {
			Name string `json:"name"`
		} `json:"author"`
	}
	readJSONFile(t, filepath.Join(claudePluginDir, ".claude-plugin", "plugin.json"), &man)
	// The entry name and the manifest name must match (marketplace docs).
	if man.Name != p.Name || man.Author.Name == "" {
		t.Fatalf("plugin.json: name %q (entry %q), author %q", man.Name, p.Name, man.Author.Name)
	}
	// No version: Claude Code then versions the plugin by commit, so
	// flopwire setup picks up every change (pre-release).
	if man.Version != "" {
		t.Fatalf("plugin.json pins version %q; see the Decisions in #58", man.Version)
	}
}

func TestClaudePluginHooks(t *testing.T) {
	var hf struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string   `json:"type"`
				Command string   `json:"command"`
				Args    []string `json:"args"`
				Timeout int      `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	readJSONFile(t, filepath.Join(claudePluginDir, "hooks", "hooks.json"), &hf)
	// #75's contract for Claude Code, and SessionEnd (#67, #82); never
	// PreToolUse.
	want := []string{"PostToolUse", "SessionEnd", "SessionStart", "Stop", "UserPromptSubmit"}
	if got := slices.Sorted(maps.Keys(hf.Hooks)); !slices.Equal(got, want) {
		t.Fatalf("hook events %q, want %q", got, want)
	}
	cmds := subcommands()
	for ev, groups := range hf.Hooks {
		if len(groups) != 1 || len(groups[0].Hooks) != 1 {
			t.Fatalf("%s: want one matcher group with one hook; got %+v", ev, groups)
		}
		g := groups[0]
		if ev == "PostToolUse" && g.Matcher != "*" {
			t.Errorf("PostToolUse matcher %q, want *", g.Matcher)
		}
		if ev != "PostToolUse" && g.Matcher != "" {
			t.Errorf("%s matcher %q: the event takes none", ev, g.Matcher)
		}
		h := g.Hooks[0]
		// The shim finds the binary; a missing or failing one is reported,
		// never with exit 2, which blocks (TestClaudePluginHooksThroughShim).
		if h.Type != "command" || h.Command != claudeHookCommand || len(h.Args) != 0 {
			t.Errorf("%s: command %q %q, want the shell command %q", ev, h.Command, h.Args, claudeHookCommand)
			continue
		}
		if !cmds["hook"] {
			t.Errorf("%s runs flopwire hook, which usageText does not list", ev)
		}
		if h.Timeout < 1 || h.Timeout > 10 {
			t.Errorf("%s timeout %ds; the hook takes at most ~300ms, keep 1-10s", ev, h.Timeout)
		}
	}
}

// claudeHookCommand is the Claude Code plugin's hook command: the shim,
// by the plugin root Claude Code (and Devin) set, through /bin/sh so the
// file's mode bits do not matter.
const claudeHookCommand = `/bin/sh "${CLAUDE_PLUGIN_ROOT}/bin/flopwire-hook" hook`

// TestClaudePluginHooksThroughShim runs each hook command as Claude Code
// does (/bin/sh -c, CLAUDE_PLUGIN_ROOT set) against flopwire binaries
// that are missing, current, older and crashing.
func TestClaudePluginHooksThroughShim(t *testing.T) {
	testHooksThroughShim(t, claudePluginDir, "CLAUDE_PLUGIN_ROOT")
}

// testHooksThroughShim: with no flopwire anywhere, every hook exits 1 with
// one line on stderr that names the recorded path, the PATH searched and
// flopwire setup: delivery has stopped and the harness shows it. A
// flopwire that fails (an older binary without the hook command; flopwire
// hook itself always exits 0) exits 1 with a hint. A crash's exit 2 also
// becomes 1: no failure exits 2, which would block a prompt or a stop.
// Nothing reaches stdout, the model's context.
func testHooksThroughShim(t *testing.T, pluginDir, rootVar string) {
	t.Helper()
	if knownFlopwireInstalled() {
		t.Skip("a flopwire in /opt/homebrew/bin or /usr/local/bin would be found")
	}
	var hf struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	readJSONFile(t, filepath.Join(pluginDir, "hooks", "hooks.json"), &hf)
	root, err := filepath.Abs(pluginDir)
	if err != nil {
		t.Fatal(err)
	}
	fake := func(body string) string {
		d := t.TempDir()
		if err := os.WriteFile(filepath.Join(d, "flopwire"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return d
	}
	for _, c := range []struct {
		name, path string
		exit       int
		stderr     []string
	}{
		{"missing", t.TempDir(), 1, []string{"recorded path (none", "flopwire is not on PATH", "fix: run flopwire setup"}},
		{"current", fake("cat >/dev/null; exit 0"), 0, nil},
		{"older", fake("echo 'Usage: flopwire <command>' >&2; exit 1"), 1, []string{"exited 1", "run: flopwire setup --check"}},
		{"crashed", fake("echo 'fatal error: concurrent map writes' >&2; exit 2"), 1, []string{"exited 2", "run: flopwire setup --check"}},
	} {
		for ev, groups := range hf.Hooks {
			for _, g := range groups {
				for _, h := range g.Hooks {
					cmd := exec.Command("/bin/sh", "-c", h.Command)
					cmd.Env = []string{"PATH=" + c.path, "HOME=" + t.TempDir(), "FLOPWIRE_CONFIG=" + filepath.Join(t.TempDir(), "config.json"), rootVar + "=" + root}
					cmd.Stdin = strings.NewReader(`{"hook_event_name":"` + ev + `","session_id":"s"}`)
					var out, errb bytes.Buffer
					cmd.Stdout, cmd.Stderr = &out, &errb
					_ = cmd.Run()
					code := cmd.ProcessState.ExitCode()
					ok := code == c.exit && out.Len() == 0
					for _, want := range c.stderr {
						ok = ok && strings.Contains(errb.String(), want)
					}
					if c.name == "missing" {
						ok = ok && strings.Count(errb.String(), "\n") == 1 && strings.Contains(errb.String(), c.path)
					}
					if !ok {
						t.Errorf("%s binary, %s: exit %d, stdout %q, stderr %q; want exit %d, no stdout, stderr with %q", c.name, ev, code, out.String(), errb.String(), c.exit, c.stderr)
					}
				}
			}
		}
	}
}

// knownFlopwireInstalled: the shim's last resort finds a flopwire in a
// system directory a test cannot hide.
func knownFlopwireInstalled() bool {
	for _, p := range []string{"/opt/homebrew/bin/flopwire", "/usr/local/bin/flopwire"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

func TestClaudePluginMCP(t *testing.T) {
	var m struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	readJSONFile(t, filepath.Join(claudePluginDir, ".mcp.json"), &m)
	s, ok := m.MCPServers["flopwire"]
	if len(m.MCPServers) != 1 || !ok || s.Command != "flopwire" || !slices.Equal(s.Args, []string{"mcp"}) {
		t.Fatalf(".mcp.json: want one server flopwire = flopwire mcp; got %+v", m)
	}
	if !subcommands()["mcp"] {
		t.Fatal("mcp is not a flopwire subcommand")
	}
	// The seven tools the plugin promises.
	tools := slices.Sorted(maps.Keys(mcpVerbs))
	tools = append(tools, slices.Sorted(maps.Keys(busToolNames))...)
	want := []string{"flopwire_grep", "flopwire_read", "flopwire_search", "flopwire_sessions", "flopwire_inbox", "flopwire_peers", "flopwire_send"}
	if !slices.Equal(tools, want) {
		t.Fatalf("MCP tools %q, want %q", tools, want)
	}
}

func TestClaudePluginSkill(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(claudePluginDir, "skills", "messaging", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)\A---\nname: ([a-z0-9-]+)\ndescription: ([^\n]+)\n---\n(.*)\z`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("SKILL.md: want frontmatter with name and description, then the body")
	}
	if string(m[1]) != "messaging" {
		t.Errorf("skill name %q, want its directory name", m[1])
	}
	if len(m[2]) > 1536 {
		t.Errorf("description is %d characters; Claude Code caps it at 1536", len(m[2]))
	}
	// Short: it rides on the standing instruction and tool descriptions.
	if n := strings.Count(string(m[3]), "\n"); n > 60 {
		t.Errorf("skill body is %d lines; keep it short", n)
	}
	for _, tool := range []string{"flopwire_sessions", "flopwire_peers", "flopwire_send"} {
		if !bytes.Contains(m[3], []byte(tool)) {
			t.Errorf("skill does not name %s", tool)
		}
	}
}

// TestClaudePluginValidates runs Claude Code's own validator when the real
// claude is installed (not in CI).
func TestClaudePluginValidates(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude is not installed")
	}
	for _, dir := range []string{"../..", claudePluginDir} {
		out, err := exec.Command(claude, "plugin", "validate", "--json", dir).Output()
		var rep struct {
			Success  bool `json:"success"`
			Manifest struct {
				Errors []any `json:"errors"`
			} `json:"manifest"`
		}
		if jerr := json.Unmarshal(out, &rep); jerr != nil {
			t.Fatalf("claude plugin validate %s: %v %v\n%s", dir, err, jerr, out)
		}
		if !rep.Success || len(rep.Manifest.Errors) > 0 {
			t.Errorf("claude plugin validate %s failed:\n%s", dir, out)
		}
	}
}
