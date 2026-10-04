package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeCodexState is the fake Codex's plugin state, kept in FAKE_CODEX_STATE.
type fakeCodexState struct {
	// Marketplaces: name → source; SourceType "local" or "git".
	Marketplaces []fakeCodexMarketplace `json:"marketplaces"`
	// Plugins installed: id → enabled.
	Plugins map[string]bool `json:"plugins"`
	// Available is the plugin revision the marketplace offers; plugin add
	// copies it into the cache.
	Available string `json:"available"`
	// Trust is the trust status per hook event (default "untrusted").
	Trust map[string]string `json:"trust,omitempty"`
	// ManualHooks are user or project hooks hooks/list also returns.
	ManualHooks []codexHook `json:"manual_hooks,omitempty"`
	// Layers is config/read's layers.
	Layers json.RawMessage `json:"layers,omitempty"`
	// ExtraConfig starts the config.toml the fake writes.
	ExtraConfig string `json:"extra_config,omitempty"`
	// Fail makes a command ("plugin add", "marketplace add", "app-server",
	// …) exit 1 with this message on stderr.
	Fail map[string]string `json:"fail,omitempty"`
}

type fakeCodexMarketplace struct {
	Name       string `json:"name"`
	SourceType string `json:"source_type"`
	Source     string `json:"source"`
}

var codexPluginEvents = []string{"postToolUse", "sessionStart", "userPromptSubmit", "stop", "sessionEnd"}

// codex0160HookHashes are the hashes codex 0.160.0's hooks/list gave the
// plugin's hooks (currentHash), captured live from a scratch CODEX_HOME
// with plugins/codex/flopwire/hooks/hooks.json as of this commit. Codex
// writes them to config.toml as trusted_hash when you trust a hook.
var codex0160HookHashes = map[string]string{
	"postToolUse":      "sha256:3d4deac7e6068391f18636c547475ec237763fe3f0f602c7ea54b778e094a2d7",
	"sessionStart":     "sha256:e8fe555d794ce3d1b8333f0e12033db0ac10536b59fd0aedead0964d81ca08e3",
	"sessionEnd":       "sha256:abb24f1d1cf8dba2e673141d2bd7672457199b468a753f092c17309acf1bb176",
	"userPromptSubmit": "sha256:77289e39427cb5248871c51f4560862fc5c7dd46fd67b62afd65414c0efa8776",
	"stop":             "sha256:8359d1f55bef687b7626f2f28229462c397ace9b1f35a2595a2d516f4e38191a",
}

// writeFakeCodexHome writes what Codex keeps on disk for st: config.toml
// (marketplaces, installed plugins, hooks.state) and, for an installed
// flopwire plugin, its cached hooks file and manifest.
func writeFakeCodexHome(st fakeCodexState) error {
	home, src := os.Getenv("CODEX_HOME"), os.Getenv("FAKE_CODEX_PLUGIN")
	var b strings.Builder
	b.WriteString(st.ExtraConfig + "\n") // top-level keys come before tables
	for _, m := range st.Marketplaces {
		fmt.Fprintf(&b, "[marketplaces.%s]\nsource_type = %q\nsource = %q\n\n", m.Name, m.SourceType, m.Source)
	}
	ids := make([]string, 0, len(st.Plugins))
	for id := range st.Plugins {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		fmt.Fprintf(&b, "[plugins.%q]\nenabled = %v\n\n", id, st.Plugins[id])
	}
	for _, ev := range codexPluginEvents {
		ts := st.Trust[ev]
		if ts == "" || ts == "untrusted" {
			continue
		}
		label := strings.ToLower(regexp.MustCompile(`([A-Z])`).ReplaceAllString(ev, "_$1"))
		fmt.Fprintf(&b, "[hooks.state.\"flopwire@flopwire:hooks/hooks.json:%s:0:0\"]\n", label)
		hash := codex0160HookHashes[ev]
		if ts == "modified" {
			hash = "sha256:0123"
		}
		fmt.Fprintf(&b, "trusted_hash = %q\n", hash)
		if ts == "disabled" {
			b.WriteString("enabled = false\n")
		}
		b.WriteString("\n")
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(b.String()), 0o600); err != nil {
		return err
	}
	if _, ok := st.Plugins[codexPlugin]; !ok || src == "" {
		return nil
	}
	cache := filepath.Join(home, "plugins", "cache", "flopwire", "flopwire", "local")
	for _, f := range []string{"hooks/hooks.json", ".codex-plugin/plugin.json"} {
		raw, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(cache, f)), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(cache, f), raw, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// fakeCodex plays `codex plugin …` and `codex app-server` against the
// state file and appends each call to FAKE_CODEX_LOG.
func fakeCodex(args []string) int {
	statePath := os.Getenv("FAKE_CODEX_STATE")
	logCall := func(s string) {
		if f, err := os.OpenFile(os.Getenv("FAKE_CODEX_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			fmt.Fprintln(f, s)
			f.Close()
		}
	}
	if len(args) == 0 || args[0] != "app-server" {
		logCall(strings.Join(args, " "))
	}
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("codex-cli 0.159.3")
		return 0
	}
	var st fakeCodexState
	raw, _ := os.ReadFile(statePath)
	_ = json.Unmarshal(raw, &st)
	save := func() {
		b, _ := json.Marshal(st)
		_ = os.WriteFile(statePath, b, 0o600)
		_ = writeFakeCodexHome(st)
	}
	if len(args) == 1 && args[0] == "app-server" {
		// Every launch is logged: --check must not start the app server,
		// whose startup refreshes Codex's marketplaces in the background.
		logCall("app-server launched")
		if msg, ok := st.Fail["app-server"]; ok {
			fmt.Fprintln(os.Stderr, msg)
			return 1
		}
		return fakeCodexAppServer(st, logCall)
	}
	if len(args) < 2 || args[0] != "plugin" {
		fmt.Fprintln(os.Stderr, "fake codex: unexpected", args)
		return 2
	}
	verb, rest := args[1], args[2:]
	if verb == "marketplace" {
		verb, rest = "marketplace "+args[2], args[3:]
	}
	var pos []string
	onlyMarketplace := ""
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--json":
		case "--sparse", "--ref":
			i++
		case "--marketplace":
			i++
			onlyMarketplace = rest[i]
		default:
			pos = append(pos, rest[i])
		}
	}
	if msg, ok := st.Fail[verb]; ok {
		fmt.Fprintln(os.Stderr, "Error: "+msg)
		return 1
	}
	out := func(v any) int {
		b, _ := json.MarshalIndent(v, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	cache := filepath.Join(os.Getenv("CODEX_HOME"), "plugins", "cache", "flopwire", "flopwire", "local")
	switch verb {
	case "marketplace list":
		l := []map[string]any{}
		for _, m := range st.Marketplaces {
			l = append(l, map[string]any{"name": m.Name, "root": "/r/" + m.Name, "marketplaceSource": map[string]string{"sourceType": m.SourceType, "source": m.Source}})
		}
		return out(map[string]any{"marketplaces": l})
	case "list":
		inst := []map[string]any{}
		ids := make([]string, 0, len(st.Plugins))
		for id := range st.Plugins {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for _, id := range ids {
			_, mk, _ := strings.Cut(id, "@")
			if !slices.ContainsFunc(st.Marketplaces, func(m fakeCodexMarketplace) bool { return m.Name == mk }) {
				continue // Codex lists plugins by marketplace
			}
			if onlyMarketplace != "" && mk != onlyMarketplace {
				continue
			}
			inst = append(inst, map[string]any{"pluginId": id, "marketplaceName": mk, "version": "local", "installed": true, "enabled": st.Plugins[id]})
		}
		return out(map[string]any{"installed": inst, "available": []any{}})
	case "marketplace add":
		src := pos[0]
		for _, m := range st.Marketplaces {
			if m.Name == "flopwire" {
				if m.Source != src {
					fmt.Fprintln(os.Stderr, "Error: marketplace 'flopwire' is already added from a different source; remove it before adding this source")
					return 1
				}
				return out(map[string]any{"marketplaceName": "flopwire", "installedRoot": "/r", "alreadyAdded": true})
			}
		}
		m := fakeCodexMarketplace{Name: "flopwire", SourceType: "git", Source: "https://github.com/" + src + ".git"}
		if strings.HasPrefix(src, "/") {
			m = fakeCodexMarketplace{Name: "flopwire", SourceType: "local", Source: src}
		}
		st.Marketplaces = append(st.Marketplaces, m)
		save()
		return out(map[string]any{"marketplaceName": "flopwire", "installedRoot": "/r", "alreadyAdded": false})
	case "marketplace upgrade":
		return out(map[string]any{"selectedMarketplaces": []string{pos[0]}, "upgradedRoots": []string{}, "errors": []any{}})
	case "marketplace remove":
		i := slices.IndexFunc(st.Marketplaces, func(m fakeCodexMarketplace) bool { return m.Name == pos[0] })
		if i < 0 {
			fmt.Fprintf(os.Stderr, "Error: marketplace `%s` is not configured or installed\n", pos[0])
			return 1
		}
		st.Marketplaces = slices.Delete(st.Marketplaces, i, i+1)
		save()
		return out(map[string]any{"marketplaceName": pos[0], "installedRoot": nil})
	case "add":
		if !slices.ContainsFunc(st.Marketplaces, func(m fakeCodexMarketplace) bool { return m.Name == "flopwire" }) {
			fmt.Fprintln(os.Stderr, "Error: marketplace `flopwire` not found")
			return 1
		}
		if st.Plugins == nil {
			st.Plugins = map[string]bool{}
		}
		st.Plugins[pos[0]] = true // Codex enables on add
		save()
		_ = os.MkdirAll(cache, 0o755)
		_ = os.WriteFile(filepath.Join(cache, "rev"), []byte(st.Available), 0o644)
		return out(map[string]any{"pluginId": pos[0], "name": "flopwire", "marketplaceName": "flopwire", "version": "local", "installedPath": cache, "authPolicy": "ON_INSTALL"})
	case "remove":
		if _, ok := st.Plugins[pos[0]]; !ok {
			fmt.Fprintf(os.Stderr, "Error: plugin `%s` is not installed\n", pos[0])
			return 1
		}
		delete(st.Plugins, pos[0])
		save()
		return out(map[string]any{"pluginId": pos[0], "name": "flopwire", "marketplaceName": "flopwire"})
	}
	fmt.Fprintln(os.Stderr, "fake codex: unknown command", args)
	return 2
}

// fakeCodexAppServer answers initialize, hooks/list and config/read, one
// JSON-RPC line at a time, and exits at the end of its input.
func fakeCodexAppServer(st fakeCodexState, logCall func(string)) int {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var m struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil || m.ID == nil {
			continue
		}
		logCall("app-server " + m.Method)
		var res any
		switch m.Method {
		case "initialize":
			res = map[string]any{"userAgent": "fake"}
			// A notification first, as the real server sends.
			fmt.Println(`{"method":"remoteControl/status/changed","params":{"status":"disabled"}}`)
		case "hooks/list":
			var p struct {
				Cwds []string `json:"cwds"`
			}
			_ = json.Unmarshal(m.Params, &p)
			hooks := []map[string]any{}
			if st.Plugins[codexPlugin] {
				for _, ev := range codexPluginEvents {
					ts := st.Trust[ev]
					if ts == "" {
						ts = "untrusted"
					}
					hooks = append(hooks, map[string]any{"key": "flopwire@flopwire:hooks/hooks.json:" + ev + ":0:0", "eventName": ev, "command": "flopwire hook || true",
						"source": "plugin", "pluginId": codexPlugin, "enabled": ts != "disabled", "trustStatus": strings.Replace(ts, "disabled", "trusted", 1)})
				}
			}
			for _, h := range st.ManualHooks {
				hooks = append(hooks, map[string]any{"key": h.Key, "eventName": h.EventName, "command": h.Command, "sourcePath": h.SourcePath, "source": h.Source, "enabled": true, "trustStatus": "trusted"})
			}
			res = map[string]any{"data": []any{map[string]any{"cwd": p.Cwds[0], "hooks": hooks, "warnings": []any{}, "errors": []any{}}}}
		case "config/read":
			layers := st.Layers
			if layers == nil {
				// The user layer holds the installed plugins.
				plugins := map[string]any{}
				for id, on := range st.Plugins {
					plugins[id] = map[string]any{"enabled": on}
				}
				layers = mustJSON([]any{map[string]any{"name": map[string]any{"type": "user", "file": "/home/u/.codex/config.toml"}, "config": map[string]any{"plugins": plugins}}})
			}
			res = map[string]any{"config": map[string]any{}, "layers": layers}
		default:
			b, _ := json.Marshal(map[string]any{"id": *m.ID, "error": map[string]any{"code": -32601, "message": "unknown method"}})
			fmt.Println(string(b))
			continue
		}
		b, _ := json.Marshal(map[string]any{"id": *m.ID, "result": res})
		fmt.Println(string(b))
	}
	return 0
}

// codexFixture adds a fake codex to a setup fixture.
type codexFixture struct {
	*setupFixture
	state, log, home string
}

func newCodexFixture(t *testing.T, withClaude bool) *codexFixture {
	t.Helper()
	f := newSetupFixture(t, withClaude)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, filepath.Join(f.dir, "bin", "codex")); err != nil {
		t.Fatal(err)
	}
	c := &codexFixture{setupFixture: f, state: filepath.Join(f.dir, "codex-state.json"), log: filepath.Join(f.dir, "codex.log"), home: filepath.Join(f.dir, "codex-home")}
	t.Setenv("FAKE_CODEX_STATE", c.state)
	t.Setenv("FAKE_CODEX_LOG", c.log)
	t.Setenv("CODEX_HOME", c.home)
	t.Setenv("FAKE_CODEX_PLUGIN", filepath.Join(f.repo, "plugins", "codex", "flopwire"))
	c.setCodex(fakeCodexState{Available: "rev1"})
	return c
}

func (c *codexFixture) setCodex(st fakeCodexState) {
	if err := os.WriteFile(c.state, mustJSON(st), 0o600); err != nil {
		c.t.Fatal(err)
	}
	if err := writeFakeCodexHome(st); err != nil {
		c.t.Fatal(err)
	}
}

func (c *codexFixture) getCodex() fakeCodexState {
	var st fakeCodexState
	raw, err := os.ReadFile(c.state)
	if err != nil {
		c.t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		c.t.Fatal(err)
	}
	return st
}

// codexCalls returns the logged codex calls and clears the log.
func (c *codexFixture) codexCalls() []string {
	raw, _ := os.ReadFile(c.log)
	_ = os.Remove(c.log)
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func (c *codexFixture) codex(rep setupReport) harnessReport {
	c.t.Helper()
	for _, h := range rep.Harnesses {
		if h.Harness == "codex" {
			return h
		}
	}
	c.t.Fatalf("no codex entry in %+v", rep)
	return harnessReport{}
}

func hasString(l []string, sub string) bool {
	return slices.ContainsFunc(l, func(s string) bool { return strings.Contains(s, sub) })
}

func TestSetupCodexMissing(t *testing.T) {
	f := newSetupFixture(t, false)
	t.Setenv("FAKE_CODEX_LOG", filepath.Join(f.dir, "codex.log"))
	rep, _, err := f.run()
	if err != nil {
		t.Fatal(err)
	}
	var h *harnessReport
	for i := range rep.Harnesses {
		if rep.Harnesses[i].Harness == "codex" {
			h = &rep.Harnesses[i]
		}
	}
	if h == nil || h.Detected || h.Installed || len(h.Done) != 0 || h.Error != "" || h.HookTrust != nil || !rep.OK {
		t.Fatalf("codex missing: want not detected and nothing done; got %+v", h)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "codex.log")); err == nil {
		t.Fatal("setup ran codex although it is not installed")
	}
}

func TestSetupCodexInstall(t *testing.T) {
	c := newCodexFixture(t, false)
	rep, _, err := c.run()
	if err != nil {
		t.Fatal(err)
	}
	h := c.codex(rep)
	if !h.Detected || !h.Installed || !h.Enabled || h.Version != "local" || h.HarnessVersion != "codex-cli 0.159.3" || h.Marketplace != c.repo || h.Error != "" {
		t.Fatalf("install: %+v", h)
	}
	want := []string{
		"plugin marketplace add " + c.repo + " --json",
		"plugin add flopwire@flopwire --json",
	}
	calls := c.codexCalls()
	if got := mutating(calls); !slices.Equal(got, want) {
		t.Fatalf("install calls:\n got %q\nwant %q", got, want)
	}
	// Verified by asking Codex, then the hooks' trust state.
	if !slices.Contains(calls, "app-server hooks/list") || !slices.Contains(calls, "app-server config/read") {
		t.Fatalf("setup did not ask Codex for the hook state: %q", calls)
	}
	if !slices.Equal(h.Done, []string{"added the marketplace flopwire from " + c.repo, "installed flopwire@flopwire"}) || !hasString(h.Todo, "restart running Codex sessions") {
		t.Fatalf("install: done %q todo %q", h.Done, h.Todo)
	}
	// Trust is pending, reported plainly, and setup does not grant it.
	if h.HookTrust == nil || h.HookTrust.Hooks != 5 || h.HookTrust.Trusted != 0 || len(h.HookTrust.NeedReview) != 5 {
		t.Fatalf("hook trust: %+v", h.HookTrust)
	}
	if !hasString(h.Todo, `"Hooks need review"`) || !hasString(h.Todo, "/hooks") || !hasString(h.Todo, "setup does not approve hooks for you") {
		t.Fatalf("no approval todo: %q", h.Todo)
	}
	for _, call := range calls {
		if strings.Contains(call, "config/batchWrite") || strings.Contains(call, "config/value/write") {
			t.Fatalf("setup wrote Codex config: %q", call)
		}
	}
}

func TestSetupCodexGitHubSourceIsSparse(t *testing.T) {
	c := newCodexFixture(t, false)
	var out bytes.Buffer
	if err := setupCmd(t.Context(), nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	got := mutating(c.codexCalls())
	if len(got) == 0 || got[0] != "plugin marketplace add flopwire/flopwire --json --sparse .agents/plugins --sparse plugins/codex" {
		t.Fatalf("default source: %q", got)
	}
	// A second run refreshes the clone, then reinstalls; nothing changed.
	var rep setupReport
	out.Reset()
	if err := setupCmd(t.Context(), nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(out.Bytes(), &rep)
	want := []string{"plugin marketplace upgrade flopwire --json", "plugin add flopwire@flopwire --json"}
	if got := mutating(c.codexCalls()); !slices.Equal(got, want) || len(c.codex(rep).Done) != 0 {
		t.Fatalf("second run: calls %q done %q", got, c.codex(rep).Done)
	}
}

func TestSetupCodexAlreadyInstalledAndUpdate(t *testing.T) {
	c := newCodexFixture(t, false)
	if _, _, err := c.run(); err != nil {
		t.Fatal(err)
	}
	c.codexCalls()
	// Already installed, same files: nothing to report, nothing to restart.
	rep, _, err := c.run()
	if err != nil {
		t.Fatal(err)
	}
	h := c.codex(rep)
	if len(h.Done) != 0 || !h.Installed || hasString(h.Todo, "restart") {
		t.Fatalf("second run: %+v", h)
	}
	if got := mutating(c.codexCalls()); !slices.Equal(got, []string{"plugin add flopwire@flopwire --json"}) {
		t.Fatalf("second run calls %q", got)
	}
	// The marketplace offers new files: the reinstall picks them up.
	st := c.getCodex()
	st.Available = "rev2"
	c.setCodex(st)
	rep, _, err = c.run()
	if err != nil {
		t.Fatal(err)
	}
	h = c.codex(rep)
	if !slices.Equal(h.Done, []string{"updated flopwire@flopwire (its files changed)"}) || !hasString(h.Todo, "restart running Codex sessions") {
		t.Fatalf("update: %+v", h)
	}
}

func TestSetupCodexRemove(t *testing.T) {
	c := newCodexFixture(t, false)
	if _, _, err := c.run(); err != nil {
		t.Fatal(err)
	}
	c.codexCalls()
	rep, _, err := c.run("--remove")
	if err != nil {
		t.Fatal(err)
	}
	h := c.codex(rep)
	if h.Installed || h.Marketplace != "" || len(h.Done) != 2 || h.HookTrust != nil {
		t.Fatalf("remove: %+v", h)
	}
	want := []string{"plugin remove flopwire@flopwire --json", "plugin marketplace remove flopwire --json"}
	if got := mutating(c.codexCalls()); !slices.Equal(got, want) {
		t.Fatalf("remove calls %q, want %q", got, want)
	}
	if st := c.getCodex(); len(st.Plugins) != 0 || len(st.Marketplaces) != 0 {
		t.Fatalf("remove left %+v", st)
	}
	rep, _, err = c.run("--remove")
	if err != nil || len(c.codex(rep).Done) != 0 || len(mutating(c.codexCalls())) != 0 {
		t.Fatalf("second remove: %v %+v", err, c.codex(rep))
	}
	rep, _, err = c.run("--check")
	if err != nil || c.codex(rep).Installed || !hasString(c.codex(rep).Todo, "install the plugin") {
		t.Fatalf("check after remove: %v %+v", err, c.codex(rep))
	}
}

// TestSetupCodexRemoveKeepsAMarketplaceInUse: another plugin installed from
// the flopwire marketplace keeps it.
func TestSetupCodexRemoveKeepsAMarketplaceInUse(t *testing.T) {
	c := newCodexFixture(t, false)
	c.setCodex(fakeCodexState{Available: "rev1",
		Marketplaces: []fakeCodexMarketplace{{Name: "flopwire", SourceType: "local", Source: c.repo}},
		Plugins:      map[string]bool{codexPlugin: true, "extra@flopwire": true, "other@elsewhere": true}})
	rep, _, err := c.run("--remove")
	if err != nil {
		t.Fatal(err)
	}
	h := c.codex(rep)
	if got := mutating(c.codexCalls()); !slices.Equal(got, []string{"plugin remove flopwire@flopwire --json"}) {
		t.Fatalf("remove calls %q", got)
	}
	if h.Installed || !hasString(h.Warnings, "extra@flopwire is still installed") {
		t.Fatalf("remove: %+v", h)
	}
	if st := c.getCodex(); len(st.Marketplaces) != 1 || !st.Plugins["extra@flopwire"] || !st.Plugins["other@elsewhere"] {
		t.Fatalf("remove took another install with it: %+v", st)
	}
}

func TestSetupCodexForeignMarketplaceNotTrusted(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(fmt.Sprint("installed=", installed), func(t *testing.T) {
			c := newCodexFixture(t, false)
			st := fakeCodexState{Available: "rev1", Marketplaces: []fakeCodexMarketplace{{Name: "flopwire", SourceType: "git", Source: "https://github.com/someone/fork.git"}}}
			if installed {
				st.Plugins = map[string]bool{codexPlugin: true}
			}
			c.setCodex(st)
			before := mustJSON(c.getCodex())
			rep, _, err := c.run()
			if !errors.Is(err, errReported) || rep.OK {
				t.Fatalf("install through a foreign marketplace: want a failed report, got %v ok=%v", err, rep.OK)
			}
			h := c.codex(rep)
			if !strings.Contains(h.Error, "someone/fork") || h.Installed != installed || len(h.Done) != 0 || !hasString(h.Warnings, "comes from https://github.com/someone/fork.git") {
				t.Fatalf("foreign marketplace: %+v", h)
			}
			if got := mutating(c.codexCalls()); len(got) != 0 {
				t.Fatalf("setup ran %q through a marketplace it did not add", got)
			}
			if !bytes.Equal(before, mustJSON(c.getCodex())) {
				t.Fatal("setup changed Codex")
			}
			rep, _, err = c.run("--check")
			if err != nil || !hasString(c.codex(rep).Warnings, "someone/fork") || len(mutating(c.codexCalls())) != 0 {
				t.Fatalf("--check: %v %+v", err, c.codex(rep))
			}
			if _, _, err = c.run("--remove"); err != nil {
				t.Fatal(err)
			}
			for _, call := range c.codexCalls() {
				if strings.HasPrefix(call, "plugin marketplace remove") {
					t.Fatalf("--remove removed a marketplace setup did not add: %q", call)
				}
			}
			if st := c.getCodex(); len(st.Marketplaces) != 1 {
				t.Fatalf("--remove left %+v", st)
			}
		})
	}
}

func TestSetupCodexSameSource(t *testing.T) {
	m := func(typ, src string) codexMarketplaceEntry {
		e := codexMarketplaceEntry{Name: "flopwire"}
		e.MarketplaceSource = &struct {
			SourceType string `json:"sourceType"`
			Source     string `json:"source"`
		}{typ, src}
		return e
	}
	cases := []struct {
		m    codexMarketplaceEntry
		src  string
		want bool
	}{
		{m("git", "https://github.com/flopwire/flopwire.git"), "flopwire/flopwire", true},
		{m("git", "https://github.com/Flopwire/Flopwire.git"), "https://github.com/flopwire/flopwire", true},
		{m("git", "https://github.com/flopwire/flopwire.git"), "flopwire/flopwire#feat/x", true},
		{m("git", "https://github.com/someone/fork.git"), "flopwire/flopwire", false},
		{m("local", "/a/b"), "/a/b", true},
		{m("local", "/a/b"), "/a/c", false},
		{m("git", "https://github.com/a/b.git"), "/a/b", false},
		{m("local", "a/b"), "a/b", false},
		{codexMarketplaceEntry{Name: "flopwire"}, "flopwire/flopwire", false},
	}
	for _, c := range cases {
		if got := sameCodexSource(c.m, c.src); got != c.want {
			t.Errorf("sameCodexSource(%+v, %q) = %v, want %v", *c.m.MarketplaceSource, c.src, got, c.want)
		}
	}
}

func TestSetupCodexCheckChangesNothing(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(fmt.Sprint("installed=", installed), func(t *testing.T) {
			c := newCodexFixture(t, false)
			if installed {
				if _, _, err := c.run(); err != nil {
					t.Fatal(err)
				}
				c.codexCalls()
			}
			before := mustJSON(c.getCodex())
			rep, _, err := c.run("--check")
			if err != nil {
				t.Fatal(err)
			}
			calls := c.codexCalls()
			if got := mutating(calls); len(got) != 0 {
				t.Fatalf("--check ran %q", got)
			}
			// codex app-server refreshes the configured marketplaces and
			// the plugin catalog in the background as it starts, and a
			// plugin list without --marketplace fetches the remote catalog.
			for _, call := range calls {
				if strings.HasPrefix(call, "app-server") || (strings.HasPrefix(call, "plugin list") && !strings.Contains(call, "--marketplace flopwire")) {
					t.Fatalf("--check ran codex %q, which has side effects", call)
				}
			}
			if !bytes.Equal(before, mustJSON(c.getCodex())) {
				t.Fatal("--check changed Codex")
			}
			h := c.codex(rep)
			if h.Installed != installed || len(h.Done) != 0 || rep.Mode != setupCheck {
				t.Fatalf("--check: %+v", h)
			}
			if installed && (h.HookTrust == nil || len(h.HookTrust.NeedReview) != 5 || !hasString(h.Todo, "Hooks need review")) {
				t.Fatalf("--check does not report pending trust: %+v", h)
			}
		})
	}
}

func TestSetupCodexTrustReporting(t *testing.T) {
	c := newCodexFixture(t, false)
	if _, _, err := c.run(); err != nil {
		t.Fatal(err)
	}
	st := c.getCodex()
	st.Trust = map[string]string{"postToolUse": "trusted", "sessionStart": "trusted", "userPromptSubmit": "modified", "stop": "trusted", "sessionEnd": "trusted"}
	c.setCodex(st)
	rep, out, err := c.run("--check")
	if err != nil {
		t.Fatal(err)
	}
	h := c.codex(rep)
	if h.HookTrust.Trusted != 4 || !slices.Equal(h.HookTrust.NeedReview, []string{"UserPromptSubmit"}) || !hasString(h.Todo, "trust the 1 Flopwire hooks (UserPromptSubmit") {
		t.Fatalf("one modified hook: %+v %q", h.HookTrust, h.Todo)
	}
	_ = out
	// All trusted: no todo.
	st.Trust["userPromptSubmit"] = "trusted"
	c.setCodex(st)
	rep, _, err = c.run("--check")
	if err != nil {
		t.Fatal(err)
	}
	h = c.codex(rep)
	if h.HookTrust.Trusted != 5 || len(h.HookTrust.NeedReview) != 0 || hasString(h.Todo, "approve") {
		t.Fatalf("all trusted: %+v %q", h.HookTrust, h.Todo)
	}
	// --text says it too.
	_, txt, err := c.run("--check", "--text")
	if err != nil || !strings.Contains(txt, "  hooks: 5 of 5 trusted\n") {
		t.Fatalf("--text: %v\n%s", err, txt)
	}
	// The user disabled one: a warning, not a todo.
	st.Trust["stop"] = "disabled"
	c.setCodex(st)
	rep, _, _ = c.run("--check")
	if h = c.codex(rep); !hasString(h.Warnings, "disabled the Flopwire hooks for Stop") {
		t.Fatalf("disabled hook: %+v", h)
	}
	// Codex cannot answer: a warning; the install itself stands.
	st.Fail = map[string]string{"app-server": "boom"}
	c.setCodex(st)
	rep, _, err = c.run()
	if err != nil || !hasString(c.codex(rep).Warnings, "could not ask Codex whether the plugin hooks are trusted") {
		t.Fatalf("app-server failure: %v %+v", err, c.codex(rep))
	}
	// --check cannot read Codex's config: a warning too.
	st.Fail = nil
	st.ExtraConfig = "this is not toml\n"
	c.setCodex(st)
	rep, _, err = c.run("--check")
	if err != nil || !hasString(c.codex(rep).Warnings, "could not read Codex's files to tell whether the plugin hooks are trusted") {
		t.Fatalf("broken config.toml: %v %+v", err, c.codex(rep))
	}
}

// TestCodexHookHashMatchesCodex: --check tells a trusted hook from a
// changed one by computing the hash Codex stores as trusted_hash. The
// plugin's hooks must hash to what codex 0.160.0 reported for them.
func TestCodexHookHashMatchesCodex(t *testing.T) {
	events, err := readCodexHooksFile("../../plugins/codex/flopwire/hooks/hooks.json")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for event, groups := range events {
		camel := strings.ToLower(event[:1]) + event[1:]
		want, ok := codex0160HookHashes[camel]
		if !ok {
			t.Fatalf("no captured hash for %s: capture it from codex app-server hooks/list", event)
		}
		if got := codexHookHash(event, groups[0].Matcher, groups[0].Hooks[0]); got != want {
			t.Errorf("%s: hash %s, codex says %s", event, got, want)
		}
		n++
	}
	if n != len(codex0160HookHashes) {
		t.Fatalf("hashed %d hooks, captured %d", n, len(codex0160HookHashes))
	}
	// Codex's normalization: defaults and limits do not change the hash,
	// a matcher on an event without one is dropped.
	five, sixHundred, two := int64(5), int64(600), int64(2500)
	star := "*"
	base := codexHookHandler{Type: "command", Command: "x"}
	withDefault := base
	withDefault.Timeout = &sixHundred
	if codexHookHash("PostToolUse", nil, base) != codexHookHash("PostToolUse", nil, withDefault) {
		t.Error("the default timeout changed the hash")
	}
	withLimit := base
	withLimit.AdditionalContextLimit = &two
	if codexHookHash("PostToolUse", nil, base) != codexHookHash("PostToolUse", nil, withLimit) {
		t.Error("the default additionalContextLimit changed the hash")
	}
	if codexHookHash("Stop", &star, base) != codexHookHash("Stop", nil, base) {
		t.Error("a matcher on Stop changed the hash")
	}
	if codexHookHash("PostToolUse", &star, base) == codexHookHash("PostToolUse", nil, base) {
		t.Error("a matcher on PostToolUse did not change the hash")
	}
	long := base
	long.Timeout = &five
	if codexHookHash("SessionEnd", nil, long) == codexHookHash("SessionEnd", nil, base) {
		t.Error("SessionEnd: a 5 s timeout (clamped to 3) hashed like the 1 s default")
	}
	if codexHookHash("Nope", nil, base) != "" || codexHookHash("Stop", nil, codexHookHandler{Type: "prompt"}) != "" {
		t.Error("hashed a hook Codex does not run")
	}
}

// TestSetupCodexCheckReadsManualEntriesFromDisk: --check finds older
// manual entries in the user's config.toml and hooks.json without asking
// codex app-server.
func TestSetupCodexCheckReadsManualEntriesFromDisk(t *testing.T) {
	c := newCodexFixture(t, false)
	if _, _, err := c.run(); err != nil {
		t.Fatal(err)
	}
	st := c.getCodex()
	st.ExtraConfig = `notify = ["flopwire", "agent", "flush"]

[mcp_servers.flopwire]
command = "flopwire"
args = ["mcp"]

[mcp_servers.other]
command = "node"
args = ["s.js"]

[[hooks.Stop]]
[[hooks.Stop.hooks]]
type = "command"
command = "flopwire agent flush"
`
	c.setCodex(st)
	if err := os.WriteFile(filepath.Join(c.home, "hooks.json"), []byte(`{"hooks":{"PostToolUse":[{"hooks":[{"type":"command","command":"flopwire hook"},{"type":"command","command":"echo done"}]}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c.codexCalls()
	_, txt, err := c.run("--check", "--text")
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range c.codexCalls() {
		if strings.HasPrefix(call, "app-server") {
			t.Fatalf("--check started codex app-server")
		}
	}
	for _, want := range []string{
		`hooks.json runs "flopwire hook" on PostToolUse`,
		`config.toml runs "flopwire agent flush" on Stop`,
		`config.toml has notify = ["flopwire" "agent" "flush"]`,
		`config.toml has an MCP server "flopwire" that runs flopwire mcp`,
		"note: --check read Codex's files",
		"did not start codex app-server",
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("--check --text lacks %q:\n%s", want, txt)
		}
	}
	if strings.Contains(txt, "echo done") || strings.Contains(txt, `"other"`) {
		t.Errorf("warned about an unrelated entry:\n%s", txt)
	}
}

func TestSetupCodexDisabledPluginStaysDisabled(t *testing.T) {
	c := newCodexFixture(t, false)
	c.setCodex(fakeCodexState{Available: "rev1",
		Marketplaces: []fakeCodexMarketplace{{Name: "flopwire", SourceType: "local", Source: c.repo}},
		Plugins:      map[string]bool{codexPlugin: false}})
	rep, _, err := c.run()
	if err != nil {
		t.Fatal(err)
	}
	h := c.codex(rep)
	if h.Enabled || !hasString(h.Todo, "the Codex plugin is disabled") {
		t.Fatalf("disabled plugin: %+v", h)
	}
	if got := mutating(c.codexCalls()); len(got) != 0 {
		t.Fatalf("setup changed a disabled plugin: %q", got)
	}
	if c.getCodex().Plugins[codexPlugin] {
		t.Fatal("setup enabled the plugin")
	}
}

func TestSetupCodexHarnessFails(t *testing.T) {
	cases := []struct {
		name string
		fail map[string]string
		want string
	}{
		{"marketplace add refused", map[string]string{"marketplace add": "clone failed"}, "clone failed"},
		{"plugin add refused", map[string]string{"add": "copy failed"}, "copy failed"},
		{"list fails", map[string]string{"list": "config broken"}, "config broken"},
		{"marketplace list fails", map[string]string{"marketplace list": "failed to load marketplace(s)"}, "failed to load"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCodexFixture(t, false)
			c.setCodex(fakeCodexState{Available: "rev1", Fail: tc.fail})
			rep, _, err := c.run()
			if !errors.Is(err, errReported) {
				t.Fatalf("want errReported; got %v", err)
			}
			h := c.codex(rep)
			if rep.OK || !strings.Contains(h.Error, tc.want) || h.Installed {
				t.Fatalf("%s: %+v", tc.name, h)
			}
		})
	}
	// Midway: the marketplace was added, the plugin add failed. A rerun
	// finishes the job.
	c := newCodexFixture(t, false)
	c.setCodex(fakeCodexState{Available: "rev1", Fail: map[string]string{"add": "disk full"}})
	if _, _, err := c.run(); !errors.Is(err, errReported) {
		t.Fatalf("midway: %v", err)
	}
	st := c.getCodex()
	if len(st.Marketplaces) != 1 || len(st.Plugins) != 0 {
		t.Fatalf("midway state: %+v", st)
	}
	st.Fail = nil
	c.setCodex(st)
	c.codexCalls()
	rep, _, err := c.run()
	if err != nil || !c.codex(rep).Installed || !slices.Equal(mutating(c.codexCalls()), []string{"plugin add flopwire@flopwire --json"}) {
		t.Fatalf("rerun after a midway failure: %v %+v", err, c.codex(rep))
	}
}

func TestSetupCodexWarnsAboutManualEntries(t *testing.T) {
	c := newCodexFixture(t, false)
	layers := `[
	 {"name":{"type":"project","dotCodexFolder":"/p/.codex"},"config":{"mcp_servers":{"fwp":{"command":"/usr/local/bin/flopwire","args":["mcp"]}}}},
	 {"name":{"type":"user","file":"` + filepath.Join(c.home, "config.toml") + `"},"config":{"notify":["flopwire","agent","flush"],
	   "mcp_servers":{"flopwire":{"command":"flopwire","args":["mcp"]},"other":{"command":"node","args":["s.js"]}}}}]`
	manual := []codexHook{
		{Key: "u:post_tool_use:0:0", EventName: "postToolUse", Command: "flopwire hook", SourcePath: filepath.Join(c.home, "hooks.json"), Source: "user"},
		{Key: "u:stop:0:0", EventName: "stop", Command: "echo done", SourcePath: filepath.Join(c.home, "hooks.json"), Source: "user"},
	}
	c.setCodex(fakeCodexState{Available: "rev1", Layers: json.RawMessage(layers), ManualHooks: manual})
	rep, _, err := c.run()
	if err != nil {
		t.Fatal(err)
	}
	w := strings.Join(c.codex(rep).Warnings, "\n")
	for _, want := range []string{
		`hooks.json runs "flopwire hook" on PostToolUse`,
		`config.toml has notify = ["flopwire" "agent" "flush"]`,
		`config.toml has an MCP server "flopwire" that runs flopwire mcp`,
		"codex mcp remove flopwire",
		`/p/.codex/config.toml has an MCP server "fwp"`,
		"remove it from that file yourself",
	} {
		if !strings.Contains(w, want) {
			t.Errorf("warnings lack %q:\n%s", want, w)
		}
	}
	if strings.Contains(w, "echo done") || strings.Contains(w, `"other"`) || strings.Contains(w, "flopwire hook || true") {
		t.Errorf("warned about an unrelated entry:\n%s", w)
	}
}

func TestSetupCodexNoDuplicateWarningsBeforeInstall(t *testing.T) {
	c := newCodexFixture(t, false)
	layers := `[{"name":{"type":"user","file":"/h/config.toml"},"config":{"notify":["flopwire","agent","flush"],"mcp_servers":{"flopwire":{"command":"flopwire","args":["mcp"]}}}}]`
	manual := []codexHook{{Key: "u:post_tool_use:0:0", EventName: "postToolUse", Command: "flopwire hook", SourcePath: "/h/hooks.json", Source: "user"}}
	c.setCodex(fakeCodexState{Available: "rev1", Layers: json.RawMessage(layers), ManualHooks: manual})
	rep, _, err := c.run("--check")
	if err != nil || len(c.codex(rep).Warnings) != 0 {
		t.Fatalf("check before install: %v %q", err, c.codex(rep).Warnings)
	}
}

func TestSetupCodexScope(t *testing.T) {
	c := newCodexFixture(t, false)
	rep, _, err := c.run("--scope", "project")
	if err != nil {
		t.Fatal(err)
	}
	h := c.codex(rep)
	if h.Installed || len(mutating(c.codexCalls())) != 0 || !hasString(h.Warnings, "Codex installs plugins for the user only") {
		t.Fatalf("--scope project: %+v", h)
	}
}

// TestSetupCodexSourceWithoutCodexMarketplace: a local source must hold
// Codex's marketplace file.
func TestSetupCodexSourceWithoutCodexMarketplace(t *testing.T) {
	c := newCodexFixture(t, false)
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, ".claude-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".claude-plugin", "marketplace.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := setupCmd(t.Context(), []string{"--source", src}, &out, &out)
	var rep setupReport
	_ = json.Unmarshal(out.Bytes(), &rep)
	if !errors.Is(err, errReported) || !strings.Contains(c.codex(rep).Error, ".agents/plugins/marketplace.json") || len(mutating(c.codexCalls())) != 0 {
		t.Fatalf("source without the Codex marketplace: %v %+v", err, c.codex(rep))
	}
}

// TestCodexRPCTimeoutHolds: an app server that never answers (and leaves a
// child holding its output) still ends at the command timeout.
func TestCodexRPCTimeoutHolds(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "codex")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 20 &\nsleep 20\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	defer func(d time.Duration) { harnessCommandTimeout = d }(harnessCommandTimeout)
	harnessCommandTimeout = 200 * time.Millisecond
	start := time.Now()
	_, _, err := codexAsk(t.Context(), script, dir)
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("codexAsk took %s with a %s timeout", took, harnessCommandTimeout)
	}
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want a timeout; got %v", err)
	}
}

// TestSetupCodexOrphanedPlugin: after the flopwire marketplace is removed
// (codex plugin marketplace remove, as setup's own foreign-marketplace
// warning suggests), Codex keeps loading the plugin from its config and
// cache, and its hooks still run, but codex plugin list no longer shows it.
// --check must not call that "not installed" without a word, and --remove
// must uninstall it.
func TestSetupCodexOrphanedPlugin(t *testing.T) {
	c := newCodexFixture(t, false)
	c.setCodex(fakeCodexState{Available: "rev1", Plugins: map[string]bool{codexPlugin: true}})
	rep, _, err := c.run("--check")
	if err != nil {
		t.Fatal(err)
	}
	if h := c.codex(rep); !hasString(h.Warnings, "Codex still loads flopwire@flopwire") || len(mutating(c.codexCalls())) != 0 {
		t.Fatalf("--check with an orphaned plugin: %+v", h)
	}
	rep, _, err = c.run("--remove")
	if err != nil {
		t.Fatal(err)
	}
	h := c.codex(rep)
	if got := mutating(c.codexCalls()); !slices.Equal(got, []string{"plugin remove flopwire@flopwire --json"}) || !slices.Equal(h.Done, []string{"uninstalled flopwire@flopwire"}) {
		t.Fatalf("--remove with an orphaned plugin: calls %q, %+v", got, h)
	}
	if st := c.getCodex(); len(st.Plugins) != 0 {
		t.Fatalf("--remove left %+v", st.Plugins)
	}
}
