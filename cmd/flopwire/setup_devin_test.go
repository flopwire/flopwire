package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeDevinState is the fake Devin CLI's plugin state, kept in the file
// FAKE_DEVIN_STATE between calls.
type fakeDevinState struct {
	// Installed is the plugin flopwire, if installed.
	Installed *fakeDevinPlugin `json:"installed,omitempty"`
	// Available is the plugin revision a fetch gets; update moves to it.
	Available string `json:"available"`
	// Fail makes a subcommand ("install", "info", …) fail with this
	// message on stderr and exit 1.
	Fail map[string]string `json:"fail,omitempty"`
	// DropHooks leaves the plugin's hooks out of what Devin loads.
	DropHooks bool `json:"drop_hooks,omitempty"`
}

type fakeDevinPlugin struct {
	Source   string `json:"source"`
	Rev      string `json:"rev"`
	Blocked  bool   `json:"blocked,omitempty"`
	Personal bool   `json:"personal,omitempty"` // installed without --local
}

// fakeDevin plays `devin plugins …` against the state file, printing the
// text Devin 3000.11.1 prints, and appends each call's arguments to
// FAKE_DEVIN_LOG. What the plugin holds comes from the repository's Claude
// Code plugin, as Devin reads it.
func fakeDevin(args []string) int {
	statePath := os.Getenv("FAKE_DEVIN_STATE")
	if f, err := os.OpenFile(os.Getenv("FAKE_DEVIN_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		fmt.Fprintln(f, strings.Join(args, " "))
		f.Close()
	}
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("devin 3000.11.1 (cc4e349ca55e)")
		return 0
	}
	var st fakeDevinState
	raw, _ := os.ReadFile(statePath)
	_ = json.Unmarshal(raw, &st)
	save := func() {
		b, _ := json.Marshal(st)
		_ = os.WriteFile(statePath, b, 0o600)
	}
	if len(args) < 2 || args[0] != "plugins" {
		fmt.Fprintln(os.Stderr, "fake devin: unexpected", args)
		return 2
	}
	verb := args[1]
	if msg, ok := st.Fail[verb]; ok {
		fmt.Fprintln(os.Stderr, "Error: "+msg)
		return 1
	}
	var pos []string
	local, yes := false, false
	for _, a := range args[2:] {
		switch a {
		case "--local":
			local = true
		case "-y", "--yes":
			yes = true
		default:
			pos = append(pos, a)
		}
	}
	notInstalled := func() int {
		fmt.Fprintln(os.Stderr, "Error: plugin 'flopwire' is not installed")
		return 1
	}
	switch verb {
	case "list":
		if st.Installed == nil {
			fmt.Println("No plugins installed.")
			return 0
		}
		line := "  • flopwire unversioned"
		if st.Installed.Blocked {
			line += " (blocked by acme/policy)"
		}
		fmt.Printf("Installed plugins\n\n%s\n\n", line)
		return 0
	case "info":
		if st.Installed == nil || len(pos) != 1 || pos[0] != "flopwire" {
			return notInstalled()
		}
		fmt.Print(fakeDevinInfo(st))
		return 0
	case "install":
		if !yes || len(pos) != 1 {
			fmt.Fprintln(os.Stderr, "fake devin: install needs -y and one source")
			return 2
		}
		if st.Installed != nil {
			fmt.Fprintln(os.Stderr, "Error: a plugin named 'flopwire' is already installed")
			return 1
		}
		src := pos[0]
		if !strings.HasPrefix(src, "/") {
			src = "https://github.com/" + src
		}
		st.Installed = &fakeDevinPlugin{Source: src, Rev: st.Available, Personal: !local}
		save()
		fmt.Println("✓ Installed flopwire.")
		return 0
	case "update":
		if st.Installed == nil {
			return notInstalled()
		}
		st.Installed.Rev = st.Available
		save()
		fmt.Println("✓ Updated: flopwire")
		return 0
	case "remove":
		if st.Installed == nil {
			return notInstalled()
		}
		if local && st.Installed.Personal {
			fmt.Fprintln(os.Stderr, "Error: 'flopwire' is in your personal plugins; remove it without --local")
			return 1
		}
		st.Installed = nil
		save()
		fmt.Println("✓ Removed flopwire.")
		return 0
	}
	fmt.Fprintln(os.Stderr, "fake devin: unknown command", args)
	return 2
}

// fakeDevinInfo prints `devin plugins info flopwire` for the repository's
// Claude Code plugin.
func fakeDevinInfo(st fakeDevinState) string {
	dir := filepath.Join(os.Getenv("FAKE_DEVIN_REPO"), filepath.FromSlash(devinPluginDir))
	var hooks struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "hooks", "hooks.json"))
	_ = json.Unmarshal(raw, &hooks)
	var mcp struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	raw, _ = os.ReadFile(filepath.Join(dir, ".mcp.json"))
	_ = json.Unmarshal(raw, &mcp)
	names := map[string]string{"SessionStart": "session_start", "UserPromptSubmit": "user_prompt", "PostToolUse": "post_tool", "Stop": "stop"}
	var b strings.Builder
	fmt.Fprintf(&b, "Plugin: flopwire\n  source: %s\n  description: rev %s\n\nSkills\n  /flopwire:messaging - Coordinate with another live coding-agent session.\n\nAgents\n  (none)\n\nHooks\n", st.Installed.Source, st.Installed.Rev)
	if !st.DropHooks {
		evs := make([]string, 0, len(hooks.Hooks))
		for ev := range hooks.Hooks {
			evs = append(evs, ev)
		}
		slices.Sort(evs)
		for _, ev := range evs {
			for _, g := range hooks.Hooks[ev] {
				for _, h := range g.Hooks {
					fmt.Fprintf(&b, "  • on %s\n      runs: %s (timeout: %dms)\n", names[ev], h.Command, h.Timeout*1000)
				}
			}
		}
	}
	b.WriteString("\nRules\n  (none)\n\nMCP servers\n")
	for n, s := range mcp.MCPServers {
		fmt.Fprintf(&b, "  • %s\n      runs: %s\n", n, strings.TrimSpace(s.Command+" "+strings.Join(s.Args, " ")))
	}
	b.WriteString("\nRequired plugins: (none)\n\nOptional plugins: (none)\n\nForbidden plugins: (none)\n")
	return b.String()
}

// devinFixture adds a fake devin to a setup fixture.
type devinFixture struct {
	*setupFixture
	state, log string
}

func newDevinFixture(t *testing.T, withClaude bool) *devinFixture {
	t.Helper()
	f := newSetupFixture(t, withClaude)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, filepath.Join(f.dir, "bin", "devin")); err != nil {
		t.Fatal(err)
	}
	d := &devinFixture{setupFixture: f, state: filepath.Join(f.dir, "devin-state.json"), log: filepath.Join(f.dir, "devin.log")}
	t.Setenv("FAKE_DEVIN_STATE", d.state)
	t.Setenv("FAKE_DEVIN_LOG", d.log)
	t.Setenv("FAKE_DEVIN_REPO", f.repo)
	d.setDevin(fakeDevinState{Available: "rev1"})
	return d
}

func (d *devinFixture) setDevin(st fakeDevinState) {
	if err := os.WriteFile(d.state, mustJSON(st), 0o600); err != nil {
		d.t.Fatal(err)
	}
}

func (d *devinFixture) getDevin() fakeDevinState {
	var st fakeDevinState
	raw, err := os.ReadFile(d.state)
	if err != nil {
		d.t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		d.t.Fatal(err)
	}
	return st
}

// devinCalls returns the mutating devin calls logged since the last call,
// and clears the log.
func (d *devinFixture) devinCalls() []string {
	raw, _ := os.ReadFile(d.log)
	_ = os.Remove(d.log)
	var m []string
	for _, c := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if c == "" || c == "--version" || c == "plugins list" || strings.HasPrefix(c, "plugins info ") {
			continue
		}
		m = append(m, c)
	}
	return m
}

func (d *devinFixture) devin(rep setupReport) harnessReport {
	d.t.Helper()
	for _, h := range rep.Harnesses {
		if h.Harness == "devin" {
			return h
		}
	}
	d.t.Fatalf("no devin entry in %+v", rep)
	return harnessReport{}
}

func (d *devinFixture) pluginDir() string {
	return filepath.Join(d.repo, filepath.FromSlash(devinPluginDir))
}

// tree lists every file under dir with its contents, for before/after
// comparisons.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		out[p] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSetupDevinMissing(t *testing.T) {
	f := newSetupFixture(t, false)
	log := filepath.Join(f.dir, "devin.log")
	t.Setenv("FAKE_DEVIN_LOG", log)
	rep, _, err := f.run()
	if err != nil {
		t.Fatal(err)
	}
	var h *harnessReport
	for i := range rep.Harnesses {
		if rep.Harnesses[i].Harness == "devin" {
			h = &rep.Harnesses[i]
		}
	}
	if h == nil || h.Detected || h.Installed || len(h.Done) != 0 || h.Error != "" || h.Plugin != "" || !rep.OK {
		t.Fatalf("devin missing: want not detected and nothing done; got %+v", h)
	}
	if _, err := os.Stat(log); err == nil {
		t.Fatal("setup ran devin although it is not installed")
	}
	// No Devin config appears for a harness that is not installed.
	if _, err := os.Stat(filepath.Join(f.home, ".config", "devin")); err == nil {
		t.Fatal("setup created Devin config although Devin is not installed")
	}
}

func TestSetupDevinInstall(t *testing.T) {
	d := newDevinFixture(t, false)
	before := tree(t, d.home)
	rep, _, err := d.run()
	if err != nil {
		t.Fatal(err)
	}
	h := d.devin(rep)
	if !h.Detected || !h.Installed || !h.Enabled || h.Version != "unversioned" || h.Error != "" || h.HarnessVersion != "devin 3000.11.1 (cc4e349ca55e)" {
		t.Fatalf("install: %+v", h)
	}
	want := []string{"plugins install --local " + d.pluginDir() + " -y"}
	if got := d.devinCalls(); !slices.Equal(got, want) {
		t.Fatalf("install calls:\n got %q\nwant %q", got, want)
	}
	if !slices.Equal(h.Done, []string{"installed the Devin plugin flopwire from " + d.pluginDir() + " (this machine only)"}) || !hasString(h.Todo, "start new Devin sessions") {
		t.Fatalf("install: done %q todo %q", h.Done, h.Todo)
	}
	// What Devin loaded is checked against the plugin: no warning when the
	// four hooks and the MCP server are there.
	if len(h.Warnings) != 0 {
		t.Fatalf("install warnings: %q", h.Warnings)
	}
	if st := d.getDevin(); st.Installed == nil || st.Installed.Personal {
		t.Fatalf("not installed with --local: %+v", st.Installed)
	}
	// setup writes no file of its own in the home directory.
	if after := tree(t, d.home); !maps.Equal(before, after) {
		t.Fatalf("setup wrote in HOME:\n%v\n%v", before, after)
	}
}

func TestSetupDevinGitHubSource(t *testing.T) {
	d := newDevinFixture(t, false)
	var out bytes.Buffer
	if err := setupCmd(t.Context(), nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	want := []string{"plugins install --local flopwire/flopwire#plugins/claude-code/flopwire -y"}
	if got := d.devinCalls(); !slices.Equal(got, want) {
		t.Fatalf("default source: %q, want %q", got, want)
	}
	// Devin prints the GitHub source as a URL; it is still ours.
	out.Reset()
	if err := setupCmd(t.Context(), nil, &out, &out); err != nil {
		t.Fatalf("second run: %v\n%s", err, out.String())
	}
	if got := d.devinCalls(); !slices.Equal(got, []string{"plugins update flopwire"}) {
		t.Fatalf("second run with the GitHub source: %q", got)
	}
	// A ref cannot be expressed to Devin: refused, nothing run.
	out.Reset()
	d.setDevin(fakeDevinState{Available: "rev1"})
	err := setupCmd(t.Context(), []string{"--source", "flopwire/flopwire#feat/x"}, &out, &out)
	var rep setupReport
	_ = json.Unmarshal(out.Bytes(), &rep)
	if !errors.Is(err, errReported) || !strings.Contains(d.devin(rep).Error, "cannot pin a branch or tag") {
		t.Fatalf("source with a ref: %v %+v", err, d.devin(rep))
	}
	if got := d.devinCalls(); len(got) != 0 {
		t.Fatalf("source with a ref ran %q", got)
	}
}

func TestSetupDevinAlreadyInstalledAndUpdate(t *testing.T) {
	d := newDevinFixture(t, false)
	if _, _, err := d.run(); err != nil {
		t.Fatal(err)
	}
	d.devinCalls()
	rep, _, err := d.run()
	if err != nil {
		t.Fatal(err)
	}
	h := d.devin(rep)
	if len(h.Done) != 0 || !h.Installed || hasString(h.Todo, "start new Devin sessions") {
		t.Fatalf("second run: %+v", h)
	}
	if got := d.devinCalls(); !slices.Equal(got, []string{"plugins update flopwire"}) {
		t.Fatalf("second run calls %q", got)
	}
	// A new revision: the update is reported.
	st := d.getDevin()
	st.Available = "rev2"
	d.setDevin(st)
	rep, _, err = d.run()
	if err != nil {
		t.Fatal(err)
	}
	if h := d.devin(rep); !slices.Equal(h.Done, []string{"updated the Devin plugin flopwire"}) || !hasString(h.Todo, "start new Devin sessions") {
		t.Fatalf("update: %+v", h)
	}
}

func TestSetupDevinRemove(t *testing.T) {
	d := newDevinFixture(t, false)
	if _, _, err := d.run(); err != nil {
		t.Fatal(err)
	}
	d.devinCalls()
	rep, _, err := d.run("--remove")
	if err != nil {
		t.Fatal(err)
	}
	h := d.devin(rep)
	if h.Installed || !slices.Equal(h.Done, []string{"removed the Devin plugin flopwire"}) {
		t.Fatalf("remove: %+v", h)
	}
	if got := d.devinCalls(); !slices.Equal(got, []string{"plugins remove flopwire --local -y"}) {
		t.Fatalf("remove calls %q", got)
	}
	if st := d.getDevin(); st.Installed != nil {
		t.Fatalf("remove left %+v", st.Installed)
	}
	// Again: nothing to do.
	rep, _, err = d.run("--remove")
	if err != nil || len(d.devin(rep).Done) != 0 || len(d.devinCalls()) != 0 {
		t.Fatalf("second remove: %v %+v", err, d.devin(rep))
	}
	rep, _, err = d.run("--check")
	if h := d.devin(rep); err != nil || h.Installed || !slices.Contains(h.Todo, "install the plugin: flopwire setup") {
		t.Fatalf("check after remove: %v %+v", err, h)
	}
}

// TestSetupDevinRemoveKeepsAPersonalPlugin: a plugin the user installed
// without --local is in their personal manifest in Devin Cloud. setup did
// not install it that way and does not remove it.
func TestSetupDevinRemoveKeepsAPersonalPlugin(t *testing.T) {
	d := newDevinFixture(t, false)
	d.setDevin(fakeDevinState{Available: "rev1", Installed: &fakeDevinPlugin{Source: d.pluginDir(), Rev: "rev1", Personal: true}})
	rep, _, err := d.run("--remove")
	if err != nil {
		t.Fatal(err)
	}
	h := d.devin(rep)
	if !h.Installed || len(h.Done) != 0 || !hasString(h.Warnings, "devin plugins remove flopwire") {
		t.Fatalf("personal plugin: %+v", h)
	}
	if d.getDevin().Installed == nil {
		t.Fatal("removed a personal plugin")
	}
}

func TestSetupDevinForeignPluginNotTouched(t *testing.T) {
	for _, mode := range []string{"", "--check", "--remove"} {
		t.Run("mode="+mode, func(t *testing.T) {
			d := newDevinFixture(t, false)
			d.setDevin(fakeDevinState{Available: "rev1", Installed: &fakeDevinPlugin{Source: "https://github.com/someone/fork#plugins/claude-code/flopwire", Rev: "rev0"}})
			before := d.getDevin()
			var args []string
			if mode != "" {
				args = []string{mode}
			}
			rep, _, err := d.run(args...)
			h := d.devin(rep)
			if mode == "" {
				if !errors.Is(err, errReported) || !strings.Contains(h.Error, "someone/fork") {
					t.Fatalf("install over a foreign plugin: %v %+v", err, h)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !hasString(h.Warnings, "comes from https://github.com/someone/fork") || !h.Installed {
				t.Fatalf("foreign plugin: %+v", h)
			}
			if got := d.devinCalls(); len(got) != 0 {
				t.Fatalf("setup ran %q on a plugin it did not install", got)
			}
			if !bytes.Equal(mustJSON(before), mustJSON(d.getDevin())) {
				t.Fatal("setup changed a plugin it did not install")
			}
		})
	}
}

func TestSetupDevinCheckChangesNothing(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(fmt.Sprint("installed=", installed), func(t *testing.T) {
			d := newDevinFixture(t, false)
			if installed {
				if _, _, err := d.run(); err != nil {
					t.Fatal(err)
				}
				d.devinCalls()
			}
			before := mustJSON(d.getDevin())
			home := tree(t, d.home)
			rep, _, err := d.run("--check")
			if err != nil {
				t.Fatal(err)
			}
			if got := d.devinCalls(); len(got) != 0 {
				t.Fatalf("--check ran %q", got)
			}
			if !bytes.Equal(before, mustJSON(d.getDevin())) || !maps.Equal(home, tree(t, d.home)) {
				t.Fatal("--check changed something")
			}
			if h := d.devin(rep); h.Installed != installed || len(h.Done) != 0 {
				t.Fatalf("--check: %+v", h)
			}
		})
	}
}

func TestSetupDevinReportsWhatDevinLoaded(t *testing.T) {
	d := newDevinFixture(t, false)
	d.setDevin(fakeDevinState{Available: "rev1", DropHooks: true, Installed: &fakeDevinPlugin{Source: d.pluginDir(), Rev: "rev1", Blocked: true}})
	rep, _, err := d.run("--check")
	if err != nil {
		t.Fatal(err)
	}
	h := d.devin(rep)
	if h.Enabled || !hasString(h.Warnings, "blocked by a plugin policy") || !hasString(h.Warnings, "did not load the plugin's flopwire hook on PostToolUse, SessionStart, Stop, UserPromptSubmit") {
		t.Fatalf("loaded state: %+v", h)
	}
}

func TestSetupDevinHarnessFails(t *testing.T) {
	cases := []struct {
		name string
		fail map[string]string
		want string
	}{
		{"install refused", map[string]string{"install": "network down"}, "network down"},
		{"info fails", map[string]string{"info": "store locked"}, "store locked"},
		{"list fails", map[string]string{"list": "store locked"}, "store locked"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newDevinFixture(t, false)
			d.setDevin(fakeDevinState{Available: "rev1", Fail: c.fail})
			rep, _, err := d.run()
			if !errors.Is(err, errReported) {
				t.Fatalf("want errReported; got %v", err)
			}
			if h := d.devin(rep); rep.OK || !strings.Contains(h.Error, c.want) {
				t.Fatalf("%s: %+v", c.name, h)
			}
		})
	}
}

func TestSetupDevinScope(t *testing.T) {
	d := newDevinFixture(t, false)
	rep, _, err := d.run("--scope", "project")
	if err != nil {
		t.Fatal(err)
	}
	if h := d.devin(rep); h.Installed || !hasString(h.Warnings, "Devin installs plugins for the user only") {
		t.Fatalf("--scope project: %+v", h)
	}
	if got := d.devinCalls(); len(got) != 0 {
		t.Fatalf("--scope project ran %q", got)
	}
}

// TestSetupDevinWarnsAboutManualEntries: Devin runs hooks from its own
// config files and from Claude Code's settings files (not from Claude Code
// plugins). A Flopwire hook or MCP server there duplicates the plugin's in
// Devin; setup says so and edits nothing.
func TestSetupDevinWarnsAboutManualEntries(t *testing.T) {
	d := newDevinFixture(t, false)
	cwd := filepath.Join(d.dir, "cwd")
	files := map[string]string{
		filepath.Join(d.home, ".claude", "settings.json"): `{"hooks":{"PostToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"flopwire hook","timeout":5}]}],
			"PreToolUse":[{"hooks":[{"type":"command","command":"echo flopwire-hooked"}]}]},"model":"x"}`,
		filepath.Join(d.home, ".config", "devin", "config.json"):     `{"version":1,"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/usr/local/bin/flopwire agent flush"}]}]}}`,
		filepath.Join(d.home, ".config", "devin", "mcp_config.json"): `{"mcpServers":{"fw":{"command":"flopwire","args":["mcp"]},"other":{"command":"node","args":["s.js"]}}}`,
		filepath.Join(cwd, ".devin", "hooks.v1.json"):                `{"SessionStart":[{"hooks":[{"type":"command","command":"flopwire hook || true"}]}]}`,
	}
	for p, s := range files {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Not installed: the manual entries are the user's working setup.
	rep, _, err := d.run("--check")
	if err != nil || len(d.devin(rep).Warnings) != 0 {
		t.Fatalf("check before install: %v %q", err, d.devin(rep).Warnings)
	}
	rep, _, err = d.run()
	if err != nil {
		t.Fatal(err)
	}
	w := strings.Join(d.devin(rep).Warnings, "\n")
	for _, want := range []string{
		`Devin reads ~/.claude/settings.json, which runs "flopwire hook" on PostToolUse (hooks.PostToolUse[0])`,
		"Devin reads Claude Code's settings hooks but not its plugins",
		`Devin reads ~/.config/devin/config.json, which runs "/usr/local/bin/flopwire agent flush" on Stop (hooks.Stop[0])`,
		`/.devin/hooks.v1.json, which runs "flopwire hook || true" on SessionStart (SessionStart[0])`,
		`~/.config/devin/mcp_config.json has an MCP server "fw" that runs flopwire mcp`,
		"devin mcp remove fw --scope user",
	} {
		if !strings.Contains(w, want) {
			t.Errorf("warnings lack %q:\n%s", want, w)
		}
	}
	if strings.Contains(w, "PreToolUse") || strings.Contains(w, `"other"`) {
		t.Errorf("warned about an unrelated entry:\n%s", w)
	}
	for p, s := range files {
		if got, _ := os.ReadFile(p); string(got) != s {
			t.Errorf("setup changed %s", p)
		}
	}
	// With Devin's Claude import off, the Claude settings hook does not run
	// in Devin, so it is not a duplicate there.
	p := filepath.Join(d.home, ".config", "devin", "config.json")
	if err := os.WriteFile(p, []byte(`{"read_config_from":{"claude":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, _, err = d.run("--check")
	if err != nil || hasString(d.devin(rep).Warnings, ".claude/settings.json") {
		t.Fatalf("claude import off: %v %q", err, d.devin(rep).Warnings)
	}
}

func TestSetupAllThreeHarnesses(t *testing.T) {
	c := newCodexFixture(t, true)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, filepath.Join(c.dir, "bin", "devin")); err != nil {
		t.Fatal(err)
	}
	d := &devinFixture{setupFixture: c.setupFixture, state: filepath.Join(c.dir, "devin-state.json"), log: filepath.Join(c.dir, "devin.log")}
	t.Setenv("FAKE_DEVIN_STATE", d.state)
	t.Setenv("FAKE_DEVIN_LOG", d.log)
	t.Setenv("FAKE_DEVIN_REPO", c.repo)
	d.setDevin(fakeDevinState{Available: "rev1"})

	rep, out, err := c.run()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := len(rep.Harnesses); got != 3 {
		t.Fatalf("harnesses: %d", got)
	}
	for _, h := range rep.Harnesses {
		if !h.Detected || !h.Installed || h.Error != "" {
			t.Errorf("%s: %+v", h.Harness, h)
		}
	}
	_, text, err := c.run("--check", "--text")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"claude: ", "codex: ", "devin: ", "  plugin: flopwire unversioned, user scope, enabled\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("--text lacks %q:\n%s", want, text)
		}
	}
	rep, _, err = c.run("--remove")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range rep.Harnesses {
		if h.Installed {
			t.Errorf("%s still installed: %+v", h.Harness, h)
		}
	}
}

// TestParseDevinPluginInfo reads the real output of `devin plugins info
// flopwire` (3000.11.1, the Claude Code plugin installed with --local).
func TestParseDevinPluginInfo(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "devin", "plugins-info.txt"))
	if err != nil {
		t.Fatal(err)
	}
	info, ok := parseDevinPluginInfo(raw)
	if !ok {
		t.Fatal("not recognised")
	}
	if info.Source != "/src/flopwire/plugins/claude-code/flopwire" || !slices.Equal(info.Skills, []string{"/flopwire:messaging"}) || info.MCP["flopwire"] != "flopwire mcp" {
		t.Fatalf("info: %+v", info)
	}
	for ev := range devinHookEvents {
		if !slices.Equal(info.Hooks[ev], []string{"flopwire hook || true"}) {
			t.Errorf("hook %s: %q", ev, info.Hooks[ev])
		}
	}
	if p := devinPluginProblems(info); len(p) != 0 {
		t.Fatalf("problems: %q", p)
	}
	list, err := os.ReadFile(filepath.Join("testdata", "devin", "plugins-list.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(list), "• flopwire unversioned") {
		t.Fatalf("list fixture: %q", list)
	}
}

func TestSameDevinSource(t *testing.T) {
	cases := []struct {
		have, want string
		same       bool
	}{
		{"https://github.com/flopwire/flopwire#plugins/claude-code/flopwire", "flopwire/flopwire#plugins/claude-code/flopwire", true},
		{"https://github.com/Flopwire/flopwire.git#plugins/claude-code/flopwire/", "https://github.com/flopwire/flopwire#plugins/claude-code/flopwire", true},
		{"https://github.com/someone/fork#plugins/claude-code/flopwire", "flopwire/flopwire#plugins/claude-code/flopwire", false},
		{"https://github.com/flopwire/flopwire#plugins/other", "flopwire/flopwire#plugins/claude-code/flopwire", false},
		{"/a/plugins/claude-code/flopwire", "/a/plugins/claude-code/flopwire", true},
		{"/b/plugins/claude-code/flopwire", "/a/plugins/claude-code/flopwire", false},
		{"https://github.com/a/b", "/a/b", false},
	}
	for _, c := range cases {
		if got := sameDevinSource(c.have, c.want); got != c.same {
			t.Errorf("sameDevinSource(%q, %q) = %v", c.have, c.want, got)
		}
	}
}
