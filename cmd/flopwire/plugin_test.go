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
func subcommands() map[string]bool {
	m := map[string]bool{}
	for _, l := range strings.Split(usageText, "\n") {
		if f := strings.Fields(l); len(f) > 1 && strings.HasPrefix(l, "  ") && !strings.HasPrefix(l, "    ") {
			m[f[0]] = true
		}
	}
	return m
}

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
	// #75's contract for Claude Code; never PreToolUse.
	want := []string{"PostToolUse", "SessionStart", "Stop", "UserPromptSubmit"}
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
		f := strings.Fields(h.Command)
		if h.Type != "command" || len(f) != 2 || f[0] != "flopwire" || len(h.Args) != 0 {
			t.Errorf("%s: command %q %q, want the shell command `flopwire hook`", ev, h.Command, h.Args)
			continue
		}
		if f[1] != "hook" || !cmds[f[1]] {
			t.Errorf("%s runs flopwire %s: not the hook subcommand", ev, f[1])
		}
		if h.Timeout < 1 || h.Timeout > 10 {
			t.Errorf("%s timeout %ds; the hook takes at most ~300ms, keep 1-10s", ev, h.Timeout)
		}
	}
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
