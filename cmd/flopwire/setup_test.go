package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestMain lets the test binary stand in for the Claude Code CLI: run
// through a symlink named "claude", it is fakeClaude.
func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == "claude" {
		os.Exit(fakeClaude(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeClaudeState is the fake harness's plugin state, kept in the file
// FAKE_CLAUDE_STATE between calls.
type fakeClaudeState struct {
	Marketplaces []claudeMarketplaceEntry `json:"marketplaces"`
	Plugins      []claudePluginEntry      `json:"plugins"`
	// Available is the plugin version the marketplace offers.
	Available string `json:"available"`
	// Fail makes a subcommand ("install", "marketplace add", …) answer a
	// failed outcome with this message; Garbage makes it print non-JSON
	// and exit 1.
	Fail    map[string]string `json:"fail,omitempty"`
	Garbage map[string]bool   `json:"garbage,omitempty"`
}

// fakeClaude plays `claude plugin …` against the state file and appends
// each call's arguments to FAKE_CLAUDE_LOG.
func fakeClaude(args []string) int {
	statePath := os.Getenv("FAKE_CLAUDE_STATE")
	if f, err := os.OpenFile(os.Getenv("FAKE_CLAUDE_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		fmt.Fprintln(f, strings.Join(args, " "))
		f.Close()
	}
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("2.1.287 (Claude Code)")
		return 0
	}
	var st fakeClaudeState
	raw, _ := os.ReadFile(statePath)
	_ = json.Unmarshal(raw, &st)
	if len(args) < 2 || args[0] != "plugin" {
		fmt.Fprintln(os.Stderr, "fake claude: unexpected", args)
		return 2
	}
	verb := args[1]
	rest := args[2:]
	if verb == "marketplace" {
		verb, rest = "marketplace "+args[2], args[3:]
	}
	var pos []string
	scope := "user"
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--json":
		case "--scope", "-s":
			i++
			scope = rest[i]
		case "--sparse":
			for i+1 < len(rest) && !strings.HasPrefix(rest[i+1], "-") {
				i++
			}
		default:
			pos = append(pos, rest[i])
		}
	}
	if st.Garbage[verb] {
		fmt.Println("Error: something broke")
		return 1
	}
	result := func(cmd string, ok bool, msg string, extra map[string]any) int {
		r := map[string]any{"command": cmd, "outcome": "ok", "message": msg}
		if !ok {
			r["outcome"] = "failed"
		}
		for k, v := range extra {
			r[k] = v
		}
		b, _ := json.Marshal(r)
		fmt.Println(string(b))
		if !ok {
			fmt.Fprintln(os.Stderr, "✘ "+msg)
			return 1
		}
		return 0
	}
	if msg, ok := st.Fail[verb]; ok {
		return result(strings.ReplaceAll(verb, " ", "-"), false, msg, map[string]any{"failureCode": "fake"})
	}
	save := func() {
		b, _ := json.Marshal(st)
		_ = os.WriteFile(statePath, b, 0o600)
	}
	find := func(id string) int {
		return slices.IndexFunc(st.Plugins, func(p claudePluginEntry) bool { return p.ID == id && p.Scope == scope })
	}
	switch verb {
	case "marketplace list":
		b, _ := json.Marshal(st.Marketplaces)
		if st.Marketplaces == nil {
			b = []byte("[]")
		}
		fmt.Println(string(b))
		return 0
	case "list":
		b, _ := json.Marshal(st.Plugins)
		if st.Plugins == nil {
			b = []byte("[]")
		}
		fmt.Println(string(b))
		return 0
	case "marketplace add":
		if slices.ContainsFunc(st.Marketplaces, func(m claudeMarketplaceEntry) bool { return m.Name == "flopwire" }) {
			return result("marketplace-add", true, "Marketplace 'flopwire' already on disk", nil)
		}
		m := claudeMarketplaceEntry{Name: "flopwire", Source: "github", Repo: pos[0]}
		if strings.HasPrefix(pos[0], "/") {
			m = claudeMarketplaceEntry{Name: "flopwire", Source: "directory", Path: pos[0]}
		}
		st.Marketplaces = append(st.Marketplaces, m)
		save()
		return result("marketplace-add", true, "Successfully added marketplace: flopwire", nil)
	case "marketplace update":
		return result("marketplace-update", true, "Successfully updated marketplace: flopwire", nil)
	case "marketplace remove":
		i := slices.IndexFunc(st.Marketplaces, func(m claudeMarketplaceEntry) bool { return m.Name == pos[0] })
		if i < 0 {
			return result("marketplace-remove", false, "Marketplace '"+pos[0]+"' not found", map[string]any{"failureCode": "not_configured"})
		}
		st.Marketplaces = slices.Delete(st.Marketplaces, i, i+1)
		st.Plugins = slices.DeleteFunc(st.Plugins, func(p claudePluginEntry) bool { return strings.HasSuffix(p.ID, "@"+pos[0]) })
		save()
		return result("marketplace-remove", true, "Successfully removed marketplace: "+pos[0], nil)
	case "install":
		if !slices.ContainsFunc(st.Marketplaces, func(m claudeMarketplaceEntry) bool { return m.Name == "flopwire" }) {
			return result("install", false, `Plugin "flopwire" not found in marketplace "flopwire"`, map[string]any{"failureCode": "not_found"})
		}
		if find(pos[0]) >= 0 {
			return result("install", true, "already installed", nil)
		}
		st.Plugins = append(st.Plugins, claudePluginEntry{ID: pos[0], Version: st.Available, Scope: scope, Enabled: true})
		save()
		return result("install", true, "Successfully installed plugin: "+pos[0], map[string]any{"pluginId": pos[0], "scope": scope})
	case "update":
		i := find(pos[0])
		if i < 0 {
			return result("update", false, `Plugin "flopwire" not found`, map[string]any{"failureCode": "not_found"})
		}
		old := st.Plugins[i].Version
		if old == st.Available {
			return result("update", true, "flopwire is already at the latest version ("+old+").", map[string]any{"updateOutcome": "up_to_date", "oldVersion": old, "newVersion": old})
		}
		st.Plugins[i].Version = st.Available
		save()
		return result("update", true, "updated", map[string]any{"updateOutcome": "updated", "oldVersion": old, "newVersion": st.Available})
	case "uninstall":
		i := find(pos[0])
		if i < 0 {
			return result("uninstall", false, `Plugin "`+pos[0]+`" not found in installed plugins`, map[string]any{"failureCode": "not_installed"})
		}
		st.Plugins = slices.Delete(st.Plugins, i, i+1)
		save()
		return result("uninstall", true, "Successfully uninstalled plugin: flopwire", nil)
	}
	fmt.Fprintln(os.Stderr, "fake claude: unknown command", args)
	return 2
}

// setupFixture is a machine for setup: a PATH with the fake claude (or
// without it), a home, a config dir with no agent, and the repository as
// the plugin source.
type setupFixture struct {
	t     *testing.T
	dir   string
	home  string
	state string
	log   string
	repo  string
}

func newSetupFixture(t *testing.T, withClaude bool) *setupFixture {
	t.Helper()
	f := &setupFixture{t: t, dir: t.TempDir()}
	bin := filepath.Join(f.dir, "bin")
	f.home = filepath.Join(f.dir, "home")
	for _, d := range []string{bin, f.home, filepath.Join(f.dir, "cwd")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if withClaude {
		if err := os.Symlink(self, filepath.Join(bin, "claude")); err != nil {
			t.Fatal(err)
		}
	}
	// A flopwire on PATH; setup only looks it up.
	if err := os.Symlink(self, filepath.Join(bin, "flopwire")); err != nil {
		t.Fatal(err)
	}
	f.state = filepath.Join(f.dir, "claude-state.json")
	f.log = filepath.Join(f.dir, "claude.log")
	f.repo, err = filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("HOME", f.home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(f.dir, "fw", "config.json"))
	t.Setenv("FLOPWIRE_TOKEN", "")
	t.Setenv(envPluginSource, "")
	t.Setenv("FAKE_CLAUDE_STATE", f.state)
	t.Setenv("FAKE_CLAUDE_LOG", f.log)
	t.Chdir(filepath.Join(f.dir, "cwd"))
	f.setState(fakeClaudeState{Available: "aaaaaaaaaaaa"})
	return f
}

func (f *setupFixture) setState(st fakeClaudeState) {
	b, _ := json.Marshal(st)
	if err := os.WriteFile(f.state, b, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *setupFixture) getState() fakeClaudeState {
	var st fakeClaudeState
	raw, err := os.ReadFile(f.state)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		f.t.Fatal(err)
	}
	return st
}

// calls returns the logged claude calls and clears the log.
func (f *setupFixture) calls() []string {
	raw, _ := os.ReadFile(f.log)
	_ = os.Remove(f.log)
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

// run runs flopwire setup with args, with the repository as the source,
// and decodes the JSON report.
func (f *setupFixture) run(args ...string) (setupReport, string, error) {
	f.t.Helper()
	var out, errb bytes.Buffer
	err := setupCmd(context.Background(), append([]string{"--source", f.repo}, args...), &out, &errb)
	var rep setupReport
	if !slices.Contains(args, "--text") && !slices.Contains(args, "--help") {
		if jerr := json.Unmarshal(out.Bytes(), &rep); jerr != nil {
			f.t.Fatalf("setup %v: not one JSON report: %v\n%s\nstderr: %s", args, jerr, out.String(), errb.String())
		}
	}
	return rep, out.String(), err
}

func (f *setupFixture) claude(rep setupReport) harnessReport {
	f.t.Helper()
	for _, h := range rep.Harnesses {
		if h.Harness == "claude" {
			return h
		}
	}
	f.t.Fatalf("no claude entry in %+v", rep)
	return harnessReport{}
}

func mutating(calls []string) []string {
	var m []string
	for _, c := range calls {
		if c == "" || c == "--version" || strings.HasPrefix(c, "plugin list") || strings.HasPrefix(c, "plugin marketplace list") {
			continue
		}
		m = append(m, c)
	}
	return m
}

func TestSetupHarnessMissing(t *testing.T) {
	f := newSetupFixture(t, false)
	rep, _, err := f.run()
	if err != nil {
		t.Fatal(err)
	}
	h := f.claude(rep)
	if h.Detected || h.Installed || len(h.Done) != 0 || h.Error != "" || !rep.OK {
		t.Fatalf("claude missing: want not detected and nothing done; got %+v", h)
	}
	if _, err := os.Stat(f.log); err == nil {
		t.Fatal("setup ran a harness command although claude is not installed")
	}
}

func TestSetupInstall(t *testing.T) {
	f := newSetupFixture(t, true)
	rep, _, err := f.run()
	if err != nil {
		t.Fatal(err)
	}
	h := f.claude(rep)
	if !h.Detected || !h.Installed || !h.Enabled || h.Version != "aaaaaaaaaaaa" || h.Scope != "user" || h.Error != "" {
		t.Fatalf("install: %+v", h)
	}
	if h.HarnessVersion != "2.1.287 (Claude Code)" || h.Marketplace != f.repo {
		t.Fatalf("install: harness version %q, marketplace %q", h.HarnessVersion, h.Marketplace)
	}
	want := []string{
		"plugin marketplace add " + f.repo + " --json --scope user",
		"plugin install flopwire@flopwire --json --scope user",
	}
	if got := mutating(f.calls()); !slices.Equal(got, want) {
		t.Fatalf("install calls:\n got %q\nwant %q", got, want)
	}
	if len(h.Done) != 2 || !slices.ContainsFunc(h.Todo, func(s string) bool { return strings.Contains(s, "restart") }) {
		t.Fatalf("install: done %q todo %q", h.Done, h.Todo)
	}
	// The agent is not running and no server is configured: both reported.
	if rep.Agent.Running || rep.Server.Configured || !slices.ContainsFunc(rep.Todo, func(s string) bool { return strings.Contains(s, "flopwire agent run") }) {
		t.Fatalf("agent/server: %+v %+v %q", rep.Agent, rep.Server, rep.Todo)
	}
	if rep.Flopwire.Path == "" {
		t.Fatalf("flopwire on PATH not reported: %+v", rep.Flopwire)
	}
}

func TestSetupGitHubSourceIsSparse(t *testing.T) {
	f := newSetupFixture(t, true)
	var out bytes.Buffer
	if err := setupCmd(context.Background(), nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	got := mutating(f.calls())
	if len(got) == 0 || got[0] != "plugin marketplace add flopwire/flopwire --json --scope user --sparse .claude-plugin plugins" {
		t.Fatalf("default source: %q", got)
	}
}

func TestSetupTwiceChangesNothing(t *testing.T) {
	f := newSetupFixture(t, true)
	if _, _, err := f.run(); err != nil {
		t.Fatal(err)
	}
	f.calls()
	before := f.getState()
	rep, _, err := f.run()
	if err != nil {
		t.Fatal(err)
	}
	h := f.claude(rep)
	if len(h.Done) != 0 || !h.Installed || slices.ContainsFunc(h.Todo, func(s string) bool { return strings.Contains(s, "restart") }) {
		t.Fatalf("second run: %+v", h)
	}
	after := f.getState()
	if b1, _ := json.Marshal(before); !bytes.Equal(b1, mustJSON(after)) {
		t.Fatalf("second run changed the harness:\n%s\n%s", b1, mustJSON(after))
	}
	// It refreshes the catalog and asks for an update, nothing else.
	want := []string{"plugin marketplace update flopwire --json", "plugin update flopwire@flopwire --json --scope user"}
	if got := mutating(f.calls()); !slices.Equal(got, want) {
		t.Fatalf("second run calls %q, want %q", got, want)
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func TestSetupUpdate(t *testing.T) {
	f := newSetupFixture(t, true)
	f.setState(fakeClaudeState{
		Available:    "bbbbbbbbbbbb",
		Marketplaces: []claudeMarketplaceEntry{{Name: "flopwire", Source: "directory", Path: f.repo}},
		Plugins:      []claudePluginEntry{{ID: claudePlugin, Version: "aaaaaaaaaaaa", Scope: "user", Enabled: true}},
	})
	rep, _, err := f.run()
	if err != nil {
		t.Fatal(err)
	}
	h := f.claude(rep)
	if h.Version != "bbbbbbbbbbbb" || !slices.Equal(h.Done, []string{"updated flopwire@flopwire from aaaaaaaaaaaa to bbbbbbbbbbbb"}) {
		t.Fatalf("update: %+v", h)
	}
}

func TestSetupRemove(t *testing.T) {
	f := newSetupFixture(t, true)
	if _, _, err := f.run(); err != nil {
		t.Fatal(err)
	}
	f.calls()
	rep, _, err := f.run("--remove")
	if err != nil {
		t.Fatal(err)
	}
	h := f.claude(rep)
	if h.Installed || h.Marketplace != "" || len(h.Done) != 2 || rep.Mode != setupRemove {
		t.Fatalf("remove: %+v", h)
	}
	want := []string{"plugin uninstall flopwire@flopwire --json --scope user", "plugin marketplace remove flopwire --json --scope user"}
	if got := mutating(f.calls()); !slices.Equal(got, want) {
		t.Fatalf("remove calls %q, want %q", got, want)
	}
	if st := f.getState(); len(st.Plugins) != 0 || len(st.Marketplaces) != 0 {
		t.Fatalf("remove left %+v", st)
	}
	// Removing again is a no-op.
	rep, _, err = f.run("--remove")
	if err != nil || len(f.claude(rep).Done) != 0 || len(mutating(f.calls())) != 0 {
		t.Fatalf("second remove: %v %+v", err, f.claude(rep))
	}
	rep, _, err = f.run("--check")
	if err != nil || f.claude(rep).Installed {
		t.Fatalf("check after remove: %v %+v", err, f.claude(rep))
	}
}

func TestSetupCheckChangesNothing(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(fmt.Sprint("installed=", installed), func(t *testing.T) {
			f := newSetupFixture(t, true)
			if installed {
				if _, _, err := f.run(); err != nil {
					t.Fatal(err)
				}
				f.calls()
			}
			before := mustJSON(f.getState())
			rep, _, err := f.run("--check")
			if err != nil {
				t.Fatal(err)
			}
			if got := mutating(f.calls()); len(got) != 0 {
				t.Fatalf("--check ran %q", got)
			}
			if !bytes.Equal(before, mustJSON(f.getState())) {
				t.Fatal("--check changed the harness")
			}
			h := f.claude(rep)
			if h.Installed != installed || len(h.Done) != 0 || rep.Mode != setupCheck {
				t.Fatalf("--check: %+v", h)
			}
			if !installed && !slices.Contains(h.Todo, "install the plugin: flopwire setup") {
				t.Fatalf("--check, not installed: todo %q", h.Todo)
			}
		})
	}
}

func TestSetupHarnessCommandFails(t *testing.T) {
	cases := []struct {
		name string
		st   fakeClaudeState
		want string
	}{
		{"install refused", fakeClaudeState{Available: "a", Fail: map[string]string{"install": "network down"}}, "network down"},
		{"marketplace add refused", fakeClaudeState{Available: "a", Fail: map[string]string{"marketplace add": "clone failed"}}, "clone failed"},
		{"list not JSON", fakeClaudeState{Available: "a", Garbage: map[string]bool{"marketplace list": true}}, "something broke"},
		{"install not JSON", fakeClaudeState{Available: "a", Garbage: map[string]bool{"install": true}}, "something broke"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newSetupFixture(t, true)
			f.setState(c.st)
			rep, _, err := f.run()
			if !errors.Is(err, errReported) {
				t.Fatalf("want errReported (exit 1, report printed); got %v", err)
			}
			h := f.claude(rep)
			if rep.OK || !strings.Contains(h.Error, c.want) || h.Installed {
				t.Fatalf("%s: %+v", c.name, h)
			}
		})
	}
}

func TestSetupText(t *testing.T) {
	f := newSetupFixture(t, true)
	_, out, err := f.run("--text")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"agent: not running\n",
		"server: none",
		"claude: ",
		"(2.1.287 (Claude Code))",
		"  plugin: flopwire@flopwire aaaaaaaaaaaa, user scope, enabled\n",
		"  done: installed flopwire@flopwire\n",
		"  todo: restart running Claude Code sessions",
		"todo: start the device agent",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("--text output lacks %q:\n%s", want, out)
		}
	}
	if json.Valid([]byte(out)) {
		t.Fatal("--text printed JSON")
	}
	_, out, err = f.run("--text")
	if err != nil || !strings.Contains(out, "  done: nothing to change\n") {
		t.Fatalf("second --text run: %v\n%s", err, out)
	}
	// A failure: the report is printed, the exit status is 1.
	st := f.getState()
	st.Fail = map[string]string{"update": "registry unreachable"}
	f.setState(st)
	_, out, err = f.run("--text")
	if !errors.Is(err, errReported) || !strings.Contains(out, "  error: update flopwire@flopwire: registry unreachable\n") {
		t.Fatalf("failed --text run: %v\n%s", err, out)
	}
}

func TestSetupWarnsAboutManualEntries(t *testing.T) {
	f := newSetupFixture(t, true)
	settings := `{"hooks":{
	  "PostToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"flopwire hook","timeout":5}]}],
	  "Stop":[{"hooks":[{"type":"command","command":"/usr/local/bin/flopwire agent flush"}]}],
	  "PreToolUse":[{"hooks":[{"type":"command","command":"echo flopwire-hooked"}]}]
	},"model":"x"}`
	claudeJSON := `{"mcpServers":{"flopwire":{"type":"stdio","command":"flopwire","args":["mcp"]},"other":{"command":"node","args":["s.js"]}}}`
	project := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"flopwire","args":["hook"]}]}]}}`
	files := map[string]string{
		filepath.Join(f.home, ".claude", "settings.json"):       settings,
		filepath.Join(f.home, ".claude.json"):                   claudeJSON,
		filepath.Join(f.dir, "cwd", ".claude", "settings.json"): project,
	}
	for p, s := range files {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Not installed yet: --check reports no duplicates, since the manual
	// entries are then the only ones.
	rep, _, err := f.run("--check")
	if err != nil || len(f.claude(rep).Warnings) != 0 {
		t.Fatalf("check before install: %v %q", err, f.claude(rep).Warnings)
	}
	rep, _, err = f.run()
	if err != nil {
		t.Fatal(err)
	}
	w := strings.Join(f.claude(rep).Warnings, "\n")
	for _, want := range []string{
		`~/.claude/settings.json runs "flopwire hook" on PostToolUse (hooks.PostToolUse[0])`,
		`~/.claude/settings.json runs "/usr/local/bin/flopwire agent flush" on Stop (hooks.Stop[0])`,
		`.claude/settings.json runs "flopwire hook" on SessionStart (hooks.SessionStart[0])`,
		`a user MCP server "flopwire" runs flopwire mcp`,
		"claude mcp remove flopwire --scope user",
	} {
		if !strings.Contains(w, want) {
			t.Errorf("warnings lack %q:\n%s", want, w)
		}
	}
	if strings.Contains(w, "PreToolUse") || strings.Contains(w, `"other"`) {
		t.Errorf("warned about an unrelated entry:\n%s", w)
	}
	for p, s := range files {
		got, _ := os.ReadFile(p)
		if string(got) != s {
			t.Errorf("setup changed %s", p)
		}
	}
}

func TestSetupDisabledAndForeignMarketplace(t *testing.T) {
	f := newSetupFixture(t, true)
	f.setState(fakeClaudeState{
		Available:    "aaaaaaaaaaaa",
		Marketplaces: []claudeMarketplaceEntry{{Name: "flopwire", Source: "github", Repo: "someone/fork"}},
		Plugins:      []claudePluginEntry{{ID: claudePlugin, Version: "aaaaaaaaaaaa", Scope: "user", Enabled: false}},
	})
	rep, _, err := f.run()
	if err != nil {
		t.Fatal(err)
	}
	h := f.claude(rep)
	if h.Enabled || !slices.ContainsFunc(h.Todo, func(s string) bool { return strings.Contains(s, "claude plugin enable flopwire@flopwire --scope user") }) {
		t.Fatalf("disabled plugin: %+v", h)
	}
	if h.Marketplace != "someone/fork" || !slices.ContainsFunc(h.Warnings, func(s string) bool { return strings.Contains(s, "comes from someone/fork") }) {
		t.Fatalf("foreign marketplace: %+v", h)
	}
	for _, c := range f.calls() {
		if strings.Contains(c, "enable") || strings.Contains(c, "marketplace add") || strings.Contains(c, "marketplace remove") {
			t.Fatalf("setup overrode the user's choice: %q", c)
		}
	}
}

func TestSetupScopeAndFlags(t *testing.T) {
	f := newSetupFixture(t, true)
	if _, _, err := f.run("--scope", "local"); err != nil {
		t.Fatal(err)
	}
	for _, c := range mutating(f.calls()) {
		if !strings.HasSuffix(c, "--scope local") {
			t.Fatalf("call without --scope local: %q", c)
		}
	}
	var out bytes.Buffer
	for _, args := range [][]string{{"--check", "--remove"}, {"--scope", "global"}, {"extra"}, {"--source", t.TempDir()}} {
		if err := setupCmd(context.Background(), args, &out, &out); err == nil || errors.Is(err, errReported) {
			t.Errorf("setup %q: want a usage error, got %v", args, err)
		}
	}
	out.Reset()
	if err := setupCmd(context.Background(), []string{"--help"}, &out, &out); err != nil || !strings.Contains(out.String(), "flopwire setup --check") {
		t.Fatalf("--help: %v %s", err, out.String())
	}
	// FLOPWIRE_PLUGIN_SOURCE is the default source.
	t.Setenv(envPluginSource, f.repo)
	env, err := newSetupEnv("", "user")
	if err != nil || env.source != f.repo {
		t.Fatalf("env source: %v %+v", err, env)
	}
}
