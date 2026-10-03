package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// errDevinLoggedOut is a `devin plugins` command refused because the user
// is not logged in to Devin: every plugins command needs a login.
var errDevinLoggedOut = errors.New("not logged in to Devin (devin plugins needs a login)")

// devinLoggedOut reports whether a failed `devin plugins` command printed
// Devin's login error: "You must be logged in to manage plugins".
func devinLoggedOut(out, errb []byte) bool {
	const msg = "You must be logged in"
	return bytes.Contains(errb, []byte(msg)) || bytes.Contains(out, []byte(msg))
}

// --- Devin CLI ---
//
// Devin CLI (3000.11.1) has a plugin system, `devin plugins install`, and
// loads a Claude Code plugin as it is: .claude-plugin/plugin.json, the
// hooks in hooks/hooks.json, the MCP servers in .mcp.json and the skills.
// So setup installs the Claude Code plugin directory into Devin with
// Devin's own command, the same way it does for the other harnesses, and
// writes no Devin config file itself.
//
// It installs with --local: on this machine only. Without it Devin also
// records the plugin in the user's personal manifest in Devin Cloud, which
// loads it on every machine and in cloud sessions, where flopwire is not
// installed. `devin plugins` has no JSON output; setup reads the text of
// `devin plugins list` and `devin plugins info`.

const (
	// devinPlugin is the plugin's name in Devin: the "name" of the Claude
	// Code plugin's manifest.
	devinPlugin = "flopwire"
	// devinPluginDir is the plugin inside the repository: Devin loads the
	// Claude Code plugin.
	devinPluginDir = "plugins/claude-code/flopwire"
)

// devinHookEvents are the events the plugin hooks, as `devin plugins info`
// names them, with the Claude-format names the plugin's hooks.json uses.
var devinHookEvents = map[string]string{
	"session_start": "SessionStart",
	"user_prompt":   "UserPromptSubmit",
	"post_tool":     "PostToolUse",
	"stop":          "Stop",
}

// devinPluginInfo is what `devin plugins info NAME` prints, in part.
type devinPluginInfo struct {
	Source      string
	Description string
	Skills      []string
	// Hooks maps each event to the commands it runs.
	Hooks map[string][]string
	// MCP maps each MCP server to its command.
	MCP map[string]string
}

// parseDevinPluginInfo reads the text of `devin plugins info`:
//
//	Plugin: flopwire
//	  source: /path/or/url
//	  description: …
//
//	Skills
//	  /flopwire:messaging - …
//
//	Hooks
//	  • on stop
//	      runs: flopwire hook || true (timeout: 5000ms)
//
//	MCP servers
//	  • flopwire
//	      runs: flopwire mcp
func parseDevinPluginInfo(out []byte) (*devinPluginInfo, bool) {
	info := &devinPluginInfo{Hooks: map[string][]string{}, MCP: map[string]string{}}
	section, item := "", ""
	seen := false
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		t := strings.TrimSpace(line)
		switch {
		case t == "":
			continue
		case strings.HasPrefix(line, "Plugin: "):
			seen, section = true, "plugin"
			continue
		case !strings.HasPrefix(line, " "):
			section, item = t, ""
			continue
		}
		switch section {
		case "plugin":
			if v, ok := strings.CutPrefix(t, "source: "); ok {
				info.Source = strings.TrimSpace(v)
			} else if v, ok := strings.CutPrefix(t, "description: "); ok {
				info.Description = strings.TrimSpace(v)
			}
		case "Skills":
			if strings.HasPrefix(t, "/") {
				name, _, _ := strings.Cut(t, " - ")
				info.Skills = append(info.Skills, name)
			}
		case "Hooks":
			if v, ok := strings.CutPrefix(t, "• on "); ok {
				item = strings.TrimSpace(v)
			} else if v, ok := strings.CutPrefix(t, "runs: "); ok && item != "" {
				info.Hooks[item] = append(info.Hooks[item], devinCommand(v))
			}
		case "MCP servers":
			if v, ok := strings.CutPrefix(t, "• "); ok {
				item = strings.TrimSpace(v)
			} else if v, ok := strings.CutPrefix(t, "runs: "); ok && item != "" {
				info.MCP[item] = devinCommand(v)
			}
		}
	}
	return info, seen
}

// devinCommand drops the " (timeout: 5000ms)" Devin prints after a command.
func devinCommand(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, " (timeout: "); i >= 0 && strings.HasSuffix(s, ")") {
		s = s[:i]
	}
	return s
}

// devinCLI runs `devin plugins` commands for one setup.
type devinCLI struct {
	env  *setupEnv
	path string
}

// info returns the installed plugin's details, or nil when it is not
// installed.
func (c devinCLI) info(ctx context.Context) (*devinPluginInfo, error) {
	args := []string{"plugins", "info", devinPlugin}
	out, errb, err := c.env.run(ctx, c.path, args...)
	if err != nil {
		if bytes.Contains(errb, []byte("is not installed")) || bytes.Contains(out, []byte("is not installed")) {
			return nil, nil
		}
		if devinLoggedOut(out, errb) {
			return nil, errDevinLoggedOut
		}
		return nil, commandError(c.path, args, out, errb, err)
	}
	info, ok := parseDevinPluginInfo(out)
	if !ok {
		return nil, commandError(c.path, args, out, errb, err)
	}
	return info, nil
}

// listed returns the plugin's line in `devin plugins list` ("• flopwire
// unversioned"), or "" when it is not listed.
func (c devinCLI) listed(ctx context.Context) (string, error) {
	args := []string{"plugins", "list"}
	out, errb, err := c.env.run(ctx, c.path, args...)
	if err != nil {
		return "", commandError(c.path, args, out, errb, err)
	}
	for l := range strings.SplitSeq(string(out), "\n") {
		f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(l), "•"))
		if len(f) > 0 && f[0] == devinPlugin {
			return strings.Join(f, " "), nil
		}
	}
	return "", nil
}

// devinSource is the `devin plugins install` source for the Claude Code
// plugin in the marketplace source: a directory inside a local checkout,
// or REPO#plugins/claude-code/flopwire, where Devin reads # as the path
// inside the repository. Devin cannot pin a branch or tag, so a source
// with #REF is refused.
func devinSource(source string) (string, error) {
	if isLocalSource(source) {
		dir := filepath.Join(source, filepath.FromSlash(devinPluginDir))
		if _, err := os.Stat(filepath.Join(dir, ".claude-plugin", "plugin.json")); err != nil {
			return "", fmt.Errorf("--source %s has no %s/.claude-plugin/plugin.json: point it at a checkout of github.com/flopwire/flopwire", source, devinPluginDir)
		}
		return dir, nil
	}
	if strings.Contains(source, "#") {
		return "", fmt.Errorf("devin plugins install cannot pin a branch or tag, and reads # as a path inside the repository; set up Devin from %s without #REF, or from a local checkout of that ref", source)
	}
	return source + "#" + devinPluginDir, nil
}

// sameDevinSource reports whether an installed plugin's source (as
// `devin plugins info` prints it) is want. Devin prints a GitHub owner/repo
// as https://github.com/owner/repo.
func sameDevinSource(have, want string) bool {
	if isLocalSource(want) {
		return isLocalSource(have) && samePath(have, want)
	}
	norm := func(s string) (string, string) {
		loc, sub, _ := strings.Cut(strings.ToLower(s), "#")
		loc = strings.TrimSuffix(strings.TrimSuffix(loc, "/"), ".git")
		loc = strings.TrimPrefix(strings.TrimPrefix(loc, "https://"), "github.com/")
		return loc, strings.Trim(sub, "/")
	}
	hl, hs := norm(have)
	wl, ws := norm(want)
	return hl == wl && hs == ws
}

func setupDevin(ctx context.Context, env *setupEnv) harnessReport {
	r := harnessReport{Harness: "devin", Plugin: devinPlugin, Done: []string{}, Todo: []string{}, Warnings: []string{}}
	path, err := env.lookPath("devin")
	if err != nil {
		r.Plugin = ""
		return r
	}
	r.Detected, r.Command = true, path
	if out, _, err := env.run(ctx, path, "--version"); err == nil {
		r.HarnessVersion = strings.TrimSpace(string(out))
	}
	fail := func(err error) harnessReport {
		r.Error = err.Error()
		return r
	}
	mode := env.mode
	if env.scope != "user" && mode != setupCheck {
		// Devin installs plugins for the user only.
		mode = setupCheck
		r.Warnings = append(r.Warnings, "Devin installs plugins for the user only, so --scope "+env.scope+" changed nothing in Devin; run flopwire setup without --scope to set it up")
	}
	src, srcErr := devinSource(env.source)
	if srcErr != nil && mode == setupInstall {
		return fail(srcErr)
	}
	r.Marketplace = src
	c := devinCLI{env: env, path: path}
	installed, err := c.info(ctx)
	if errors.Is(err, errDevinLoggedOut) {
		// Devin is not set up on this machine: skip it without failing
		// the run.
		again := "flopwire setup"
		if env.mode != setupInstall {
			again += " --" + env.mode
		}
		r.Todo = append(r.Todo, "log in to Devin: devin auth login, then run "+again+" again")
		r.Skipped = err.Error()
		return r
	}
	if err != nil {
		return fail(err)
	}
	foreign := installed != nil && (srcErr != nil || !sameDevinSource(installed.Source, src))
	if installed != nil {
		r.Marketplace = installed.Source
	}
	if foreign {
		r.Warnings = append(r.Warnings, fmt.Sprintf("Devin's plugin %s comes from %s, not %s; setup installs, updates and removes nothing it did not install. If you trust it, run flopwire setup --source with that source. To switch, run devin plugins remove %s, then flopwire setup", devinPlugin, installed.Source, src, devinPlugin))
	}

	switch mode {
	case setupInstall:
		switch {
		case foreign:
			r.Error = fmt.Sprintf("did not install or update the Devin plugin %s: it comes from %s, not %s (see warnings)", devinPlugin, installed.Source, src)
		case installed == nil:
			// -y answers Devin's trust prompt, which lists what the plugin
			// adds; running flopwire setup is the user's consent to it.
			args := []string{"plugins", "install", "--local", src, "-y"}
			if out, errb, err := env.run(ctx, path, args...); err != nil {
				return fail(commandError(path, args, out, errb, err))
			}
			r.Done = append(r.Done, "installed the Devin plugin "+devinPlugin+" from "+src+" (this machine only)")
		default:
			args := []string{"plugins", "update", devinPlugin}
			if out, errb, err := env.run(ctx, path, args...); err != nil {
				r.Warnings = append(r.Warnings, "could not update the plugin: "+commandError(path, args, out, errb, err).Error())
			}
		}
	case setupRemove:
		if installed != nil && !foreign {
			args := []string{"plugins", "remove", devinPlugin, "--local", "-y"}
			out, errb, err := env.run(ctx, path, args...)
			switch {
			case err == nil:
				r.Done = append(r.Done, "removed the Devin plugin "+devinPlugin)
			case bytes.Contains(errb, []byte("personal plugins")) || bytes.Contains(out, []byte("personal plugins")):
				// Installed without --local, so not by setup: it is in the
				// user's personal manifest in Devin Cloud.
				r.Warnings = append(r.Warnings, "Devin's plugin "+devinPlugin+" is in your personal plugins in Devin Cloud, which setup does not change; remove it with: devin plugins remove "+devinPlugin)
			default:
				return fail(commandError(path, args, out, errb, err))
			}
		}
	}

	// Verify by asking Devin.
	before := installed
	if mode != setupCheck {
		if installed, err = c.info(ctx); err != nil {
			return fail(err)
		}
	}
	if mode == setupInstall && before != nil && installed != nil && !foreign && !sameDevinInfo(before, installed) {
		r.Done = append(r.Done, "updated the Devin plugin "+devinPlugin)
		// Devin's update takes a new revision's hooks without asking, so
		// name each hook the update enabled or dropped.
		added, removed := devinHookChanges(before, installed)
		if len(added) > 0 {
			r.Warnings = append(r.Warnings, "the plugin update enabled new hooks in Devin: "+strings.Join(added, "; "))
		}
		if len(removed) > 0 {
			r.Done = append(r.Done, "the plugin update removed hooks from Devin: "+strings.Join(removed, "; "))
		}
	}
	if installed != nil {
		r.Installed, r.Enabled, r.Scope = true, true, "user"
		line, err := c.listed(ctx)
		if err != nil {
			return fail(err)
		}
		if f := strings.Fields(line); len(f) > 1 {
			r.Version = f[1]
		}
		if strings.Contains(strings.ToLower(line), "blocked") {
			r.Enabled = false
			r.Warnings = append(r.Warnings, "Devin lists the plugin as blocked by a plugin policy ("+line+"), so its hooks, tools and skill do not load")
		}
		if !foreign {
			r.Warnings = append(r.Warnings, devinPluginProblems(installed)...)
		}
	}
	switch {
	case mode == setupInstall && !r.Installed && r.Error == "":
		return fail(fmt.Errorf("devin plugins info does not show %s after the install", devinPlugin))
	case mode == setupRemove && r.Installed && !foreign && len(r.Done) > 0:
		return fail(fmt.Errorf("devin plugins info still shows %s", devinPlugin))
	case mode == setupCheck && !r.Installed:
		r.Todo = append(r.Todo, "install the plugin: flopwire setup")
	}
	if len(r.Done) > 0 {
		r.Todo = append(r.Todo, "start new Devin sessions to apply the change: a running session keeps the hooks and tools it started with")
	}
	if r.Installed && !foreign {
		r.Warnings = append(r.Warnings, devinManualEntries(env)...)
	}
	return r
}

// sameDevinInfo reports whether two `devin plugins info` reads describe the
// same plugin. The plugin has no version, so an update that changes only
// file contents info does not show (a skill's body) is not reported.
func sameDevinInfo(a, b *devinPluginInfo) bool {
	return mustString(a) == mustString(b)
}

func mustString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// devinPluginProblems checks what Devin loaded from the plugin against what
// the plugin ships: flopwire hook on the four events, the flopwire MCP
// server and the messaging skill.
func devinPluginProblems(info *devinPluginInfo) []string {
	var warn []string
	events := make([]string, 0, len(devinHookEvents))
	for ev := range devinHookEvents {
		events = append(events, ev)
	}
	slices.Sort(events)
	var missing []string
	for _, ev := range events {
		if !slices.ContainsFunc(info.Hooks[ev], isFlopwireHook) {
			missing = append(missing, devinHookEvents[ev])
		}
	}
	if len(missing) > 0 {
		warn = append(warn, "Devin did not load the plugin's flopwire hook on "+strings.Join(missing, ", ")+"; reinstall it: flopwire setup --remove, then flopwire setup")
	}
	if !strings.HasSuffix(info.MCP["flopwire"], "flopwire mcp") {
		warn = append(warn, "Devin did not load the plugin's flopwire MCP server; reinstall it: flopwire setup --remove, then flopwire setup")
	}
	return warn
}

func isFlopwireHook(cmd string) bool { return manualHookRe.MatchString(cmd) }

// devinManualEntries reads, read-only, the files Devin takes hooks and MCP
// servers from besides plugins, and returns a warning for each Flopwire
// hook or MCP server that the plugin now duplicates in Devin sessions.
// Devin reads Claude Code's settings files too (unless the user turned off
// read_config_from.claude), but not Claude Code's plugins.
func devinManualEntries(env *setupEnv) []string {
	userDir := filepath.Join(env.home, ".config", "devin")
	userConfig := filepath.Join(userDir, "config.json")
	claude := devinReadsClaude(userConfig, filepath.Join(env.cwd, ".devin", "config.json"), filepath.Join(env.cwd, ".devin", "config.local.json"))
	type hookFile struct {
		path     string
		topLevel bool // .devin/hooks.v1.json: the events are the whole file
		claude   bool
	}
	files := []hookFile{
		{path: userConfig},
		{path: filepath.Join(env.cwd, ".devin", "hooks.v1.json"), topLevel: true},
		{path: filepath.Join(env.cwd, ".devin", "config.json")},
		{path: filepath.Join(env.cwd, ".devin", "config.local.json")},
	}
	if env.home != "" {
		files = append(files,
			hookFile{path: filepath.Join(env.home, ".claude.json"), claude: true},
			hookFile{path: filepath.Join(env.home, ".claude", "settings.json"), claude: true},
			hookFile{path: filepath.Join(env.home, ".claude", "settings.local.json"), claude: true})
	}
	files = append(files,
		hookFile{path: filepath.Join(env.cwd, ".claude", "settings.json"), claude: true},
		hookFile{path: filepath.Join(env.cwd, ".claude", "settings.local.json"), claude: true})
	var warn []string
	seen := map[string]bool{}
	for _, f := range files {
		if seen[f.path] || (f.claude && !claude) {
			continue
		}
		seen[f.path] = true
		for _, h := range flopwireHooksIn(f.path, f.topLevel) {
			why := "the plugin runs flopwire hook on that event too, so Devin runs it twice (messages and the standing instruction still arrive once)"
			fix := "remove that entry from the file yourself (setup does not edit it)"
			if f.claude {
				fix = "if Claude Code gets Flopwire from its plugin, remove that entry yourself; Devin reads Claude Code's settings hooks but not its plugins (setup does not edit the file)"
			}
			warn = append(warn, fmt.Sprintf("Devin reads %s, which runs %q on %s (%s); %s; %s", tildePath(f.path, env.home), h.command, h.event, h.where, why, fix))
		}
	}
	// MCP servers running flopwire mcp in Devin's own MCP config files.
	for _, m := range []struct{ path, scope string }{
		{filepath.Join(userDir, "mcp_config.json"), "user"},
		{filepath.Join(env.cwd, ".devin", "mcp_config.json"), "project"},
		{filepath.Join(env.cwd, ".devin", "mcp_config.local.json"), "local"},
	} {
		for _, n := range flopwireMCPServersIn(m.path) {
			warn = append(warn, fmt.Sprintf("%s has an MCP server %q that runs flopwire mcp; the plugin provides the same tools, so remove it: devin mcp remove %s --scope %s", tildePath(m.path, env.home), n, n, m.scope))
		}
	}
	// Devin also imports Claude Code's MCP servers.
	if claude {
		var claudeMCP []string
		if env.home != "" {
			claudeMCP = append(claudeMCP, filepath.Join(env.home, ".claude.json"))
		}
		claudeMCP = append(claudeMCP, filepath.Join(env.cwd, ".mcp.json"))
		for _, p := range claudeMCP {
			for _, n := range flopwireMCPServersIn(p) {
				warn = append(warn, fmt.Sprintf("Devin imports the MCP server %q from %s, which runs flopwire mcp; the plugin provides the same tools in Devin. If Claude Code gets Flopwire from its plugin, remove that server; otherwise keep it (setup does not edit the file)", n, tildePath(p, env.home)))
			}
		}
	}
	return warn
}

// devinReadsClaude reports whether Devin imports Claude Code's config:
// read_config_from.claude is true unless a config file sets it false (the
// project's files override the user's).
func devinReadsClaude(files ...string) bool {
	on := true
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var c struct {
			ReadConfigFrom struct {
				Claude *bool `json:"claude"`
			} `json:"read_config_from"`
		}
		if json.Unmarshal(raw, &c) == nil && c.ReadConfigFrom.Claude != nil {
			on = *c.ReadConfigFrom.Claude
		}
	}
	return on
}

type hookHit struct{ event, where, command string }

// flopwireHooksIn returns the hooks in a settings file that run flopwire's
// hook or flush command. A hooks.v1.json file holds the events at the top
// level; other files under "hooks". The file is only read; a file that
// does not parse is skipped.
func flopwireHooksIn(path string, topLevel bool) []hookHit {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	type group struct {
		Hooks []struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"hooks"`
	}
	var events map[string][]group
	prefix := "hooks."
	if topLevel {
		prefix = ""
		if json.Unmarshal(raw, &events) != nil {
			return nil
		}
	} else {
		var s struct {
			Hooks map[string][]group `json:"hooks"`
		}
		if json.Unmarshal(raw, &s) != nil {
			return nil
		}
		events = s.Hooks
	}
	names := make([]string, 0, len(events))
	for ev := range events {
		names = append(names, ev)
	}
	slices.Sort(names)
	var out []hookHit
	for _, ev := range names {
		for i, g := range events[ev] {
			for _, h := range g.Hooks {
				line := strings.TrimSpace(h.Command + " " + strings.Join(h.Args, " "))
				if manualHookRe.MatchString(line) {
					out = append(out, hookHit{event: ev, where: fmt.Sprintf("%s%s[%d]", prefix, ev, i), command: line})
				}
			}
		}
	}
	return out
}

// flopwireMCPServersIn returns the names of the MCP servers in an
// mcpServers file that run flopwire mcp.
func flopwireMCPServersIn(path string) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var f struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if json.Unmarshal(raw, &f) != nil {
		return nil
	}
	var names []string
	for n, s := range f.MCPServers {
		if filepath.Base(s.Command) == "flopwire" && len(s.Args) > 0 && s.Args[0] == "mcp" {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	return names
}

// devinHookChanges returns the hooks in b that are not in a, and those in
// a that are not in b, as "on EVENT runs "COMMAND"", sorted. A command
// that runs once more on an event counts as added.
func devinHookChanges(a, b *devinPluginInfo) (added, removed []string) {
	type hook struct{ event, command string }
	count := func(info *devinPluginInfo) map[hook]int {
		m := map[hook]int{}
		for ev, cmds := range info.Hooks {
			for _, c := range cmds {
				m[hook{ev, c}]++
			}
		}
		return m
	}
	ca, cb := count(a), count(b)
	diff := func(x, y map[hook]int) []string {
		var out []string
		for h, n := range x {
			for range n - y[h] {
				out = append(out, fmt.Sprintf("on %s runs %q", h.event, h.command))
			}
		}
		slices.Sort(out)
		return out
	}
	return diff(cb, ca), diff(ca, cb)
}
