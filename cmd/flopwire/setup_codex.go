package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// --- Codex ---
//
// Codex (0.159.3) installs plugins with `codex plugin marketplace add` and
// `codex plugin add`, and copies the plugin into
// $CODEX_HOME/plugins/cache/<marketplace>/<plugin>/<version>. A plugin's
// hooks run only after the user trusts them once in Codex (the "Hooks need
// review" prompt, or /hooks); setup reports that state through the app
// server's hooks/list and never grants it.

// The Codex marketplace and plugin names, from .agents/plugins/marketplace.json
// and plugins/codex/flopwire/.codex-plugin/plugin.json.
const (
	codexMarketplace = "flopwire"
	codexPlugin      = "flopwire@flopwire"
	// codexMarketplaceFile is where Codex looks for a repository's
	// marketplace before .claude-plugin/marketplace.json.
	codexMarketplaceFile = ".agents/plugins/marketplace.json"
)

// codexSparse are the paths a GitHub clone of the marketplace needs.
var codexSparse = []string{".agents/plugins", "plugins/codex"}

type codexMarketplaceEntry struct {
	Name              string `json:"name"`
	Root              string `json:"root"`
	MarketplaceSource *struct {
		SourceType string `json:"sourceType"`
		Source     string `json:"source"`
	} `json:"marketplaceSource"`
}

func (m codexMarketplaceEntry) location() string {
	if m.MarketplaceSource != nil {
		return m.MarketplaceSource.Source
	}
	return m.Root
}

type codexPluginEntry struct {
	PluginID        string  `json:"pluginId"`
	MarketplaceName string  `json:"marketplaceName"`
	Version         *string `json:"version"`
	Installed       bool    `json:"installed"`
	Enabled         bool    `json:"enabled"`
}

// codexHook is one entry of the app server's hooks/list answer.
type codexHook struct {
	Key         string `json:"key"`
	EventName   string `json:"eventName"`
	Command     string `json:"command"`
	SourcePath  string `json:"sourcePath"`
	Source      string `json:"source"`
	PluginID    string `json:"pluginId"`
	Enabled     bool   `json:"enabled"`
	TrustStatus string `json:"trustStatus"`
}

// hookTrustReport is the trust state of the plugin's hooks, from Codex.
type hookTrustReport struct {
	Hooks      int      `json:"hooks"`
	Trusted    int      `json:"trusted"`
	NeedReview []string `json:"need_review"`
	Disabled   []string `json:"disabled"`
}

// codexCLI runs `codex plugin` commands for one setup.
type codexCLI struct {
	env  *setupEnv
	path string
}

// json runs a codex command that prints one JSON document on success.
func (c codexCLI) json(ctx context.Context, v any, args ...string) error {
	out, errb, err := c.env.run(ctx, c.path, args...)
	if err != nil {
		return commandError(c.path, args, out, errb, err)
	}
	i := bytes.IndexAny(out, "{[")
	if i < 0 || json.Unmarshal(out[i:], v) != nil {
		return commandError(c.path, args, out, errb, err)
	}
	return nil
}

func (c codexCLI) marketplaces(ctx context.Context) ([]codexMarketplaceEntry, error) {
	var l struct {
		Marketplaces []codexMarketplaceEntry `json:"marketplaces"`
	}
	err := c.json(ctx, &l, "plugin", "marketplace", "list", "--json")
	return l.Marketplaces, err
}

func (c codexCLI) installed(ctx context.Context) ([]codexPluginEntry, error) {
	var l struct {
		Installed []codexPluginEntry `json:"installed"`
	}
	err := c.json(ctx, &l, "plugin", "list", "--json")
	return l.Installed, err
}

func findCodexPlugin(l []codexPluginEntry) *codexPluginEntry {
	for i := range l {
		if l[i].PluginID == codexPlugin && l[i].Installed {
			return &l[i]
		}
	}
	return nil
}

// sameCodexSource reports whether a configured Codex marketplace comes from
// source. Codex lists a GitHub source as its clone URL and does not list
// the ref, so owner/repo#ref matches owner/repo at any ref.
func sameCodexSource(m codexMarketplaceEntry, source string) bool {
	if m.MarketplaceSource == nil {
		return false
	}
	if isLocalSource(source) {
		return m.MarketplaceSource.SourceType == "local" && samePath(m.MarketplaceSource.Source, source)
	}
	if m.MarketplaceSource.SourceType != "git" {
		return false
	}
	loc, _, _ := strings.Cut(source, "#")
	return normGitSource(m.MarketplaceSource.Source) == normGitSource(loc)
}

func normGitSource(s string) string {
	s = strings.TrimSuffix(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), "/"), ".git")
	return strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "github.com/")
}

// codexHome is Codex's home directory: $CODEX_HOME, else ~/.codex.
func codexHome(env *setupEnv) string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	if env.home == "" {
		return ""
	}
	return filepath.Join(env.home, ".codex")
}

// codexCacheDigest hashes Codex's installed copy of the plugin (all
// versions), read-only, so setup can tell whether a reinstall changed it.
// "" when it cannot be read.
func codexCacheDigest(env *setupEnv) string {
	home := codexHome(env)
	if home == "" {
		return ""
	}
	root := filepath.Join(home, "plugins", "cache", codexMarketplace, "flopwire")
	h := sha256.New()
	n := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		fmt.Fprintf(h, "%s\x00%v\x00", rel, d.Type())
		if d.Type().IsRegular() {
			n++
			if n > 1000 {
				return errors.New("too many files")
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(h, io.LimitReader(f, 4<<20))
			f.Close()
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// codexAsk asks a short-lived `codex app-server` for the hooks Codex would
// run in cwd (hooks/list) and the effective config with its layers
// (config/read). Both are reads.
func codexAsk(ctx context.Context, path, cwd string) ([]codexHook, json.RawMessage, error) {
	res, err := codexRPC(ctx, path, []rpcCall{
		{method: "hooks/list", params: map[string]any{"cwds": []string{cwd}}},
		{method: "config/read", params: map[string]any{"includeLayers": true, "cwd": cwd}},
	})
	if err != nil {
		return nil, nil, err
	}
	var hl struct {
		Data []struct {
			Cwd   string      `json:"cwd"`
			Hooks []codexHook `json:"hooks"`
		} `json:"data"`
	}
	if err := json.Unmarshal(res[0], &hl); err != nil {
		return nil, nil, fmt.Errorf("hooks/list: %w", err)
	}
	var hooks []codexHook
	for _, d := range hl.Data {
		hooks = append(hooks, d.Hooks...)
	}
	return hooks, res[1], nil
}

type rpcCall struct {
	method string
	params any
}

// codexRPC runs `codex app-server` (JSON-RPC lines on stdio), initializes
// it, sends calls one at a time and returns each result. The app server
// exits at the end of its input before answering, so input stays open
// until the last answer arrives.
func codexRPC(ctx context.Context, path string, calls []rpcCall) ([]json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, harnessCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "app-server")
	cmd.WaitDelay = 2 * time.Second
	// The process writes stderr while wait reads it after an early EOF.
	var errb lockedBuffer
	cmd.Stderr = &errb
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() {
		stdin.Close()
		cancel()
		_ = cmd.Wait()
	}()
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	send := func(m map[string]any) error {
		m["jsonrpc"] = "2.0"
		b, _ := json.Marshal(m)
		_, err := stdin.Write(append(b, '\n'))
		return err
	}
	wait := func(id int) (json.RawMessage, error) {
		for sc.Scan() {
			var m struct {
				ID     *int            `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(sc.Bytes(), &m) != nil || m.ID == nil || *m.ID != id {
				continue // a notification or a request from the server
			}
			if m.Error != nil {
				return nil, errors.New(m.Error.Message)
			}
			return m.Result, nil
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("timed out after %s", harnessCommandTimeout)
		}
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 400 {
			msg = msg[:400] + "…"
		}
		return nil, fmt.Errorf("the app server stopped before answering: %s", msg)
	}
	if err := send(map[string]any{"id": 0, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "flopwire-setup", "version": "1"}}}); err != nil {
		return nil, err
	}
	if _, err := wait(0); err != nil {
		return nil, fmt.Errorf("codex app-server initialize: %w", err)
	}
	if err := send(map[string]any{"method": "initialized"}); err != nil {
		return nil, err
	}
	var out []json.RawMessage
	for i, c := range calls {
		if err := send(map[string]any{"id": i + 1, "method": c.method, "params": c.params}); err != nil {
			return nil, err
		}
		r, err := wait(i + 1)
		if err != nil {
			return nil, fmt.Errorf("codex app-server %s: %w", c.method, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// lockedBuffer is a bytes.Buffer safe for one writer and one reader.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// codexEventName maps hooks/list's event names to the hooks.json spelling.
func codexEventName(e string) string {
	if e == "" {
		return e
	}
	return strings.ToUpper(e[:1]) + e[1:]
}

func setupCodex(ctx context.Context, env *setupEnv) harnessReport {
	r := harnessReport{Harness: "codex", Plugin: codexPlugin, Done: []string{}, Todo: []string{}, Warnings: []string{}}
	path, err := env.lookPath("codex")
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
		// Codex has no project or local install: do not widen the request
		// to the whole user.
		mode = setupCheck
		r.Warnings = append(r.Warnings, "Codex installs plugins for the user only, so --scope "+env.scope+" changed nothing in Codex; run flopwire setup without --scope to set it up")
	}
	if isLocalSource(env.source) {
		if _, err := os.Stat(filepath.Join(env.source, codexMarketplaceFile)); err != nil {
			return fail(fmt.Errorf("--source %s has no %s: point it at a checkout of github.com/flopwire/flopwire", env.source, codexMarketplaceFile))
		}
	}
	c := codexCLI{env: env, path: path}
	mkts, err := c.marketplaces(ctx)
	if err != nil {
		return fail(err)
	}
	var mkt *codexMarketplaceEntry
	for i := range mkts {
		if mkts[i].Name == codexMarketplace {
			mkt = &mkts[i]
		}
	}
	if mkt != nil {
		r.Marketplace = mkt.location()
	}
	foreign := mkt != nil && !sameCodexSource(*mkt, env.source)
	if foreign {
		r.Warnings = append(r.Warnings, fmt.Sprintf("the Codex marketplace %s comes from %s, not %s; setup installs, updates and removes nothing through a marketplace it did not add. If you trust it, run flopwire setup --source %s. To switch, run codex plugin marketplace remove %s, then flopwire setup", codexMarketplace, mkt.location(), env.source, mkt.location(), codexMarketplace))
	}
	list, err := c.installed(ctx)
	if err != nil {
		return fail(err)
	}
	installed := findCodexPlugin(list)

	switch mode {
	case setupInstall:
		switch {
		case mkt == nil:
			args := []string{"plugin", "marketplace", "add", env.source, "--json"}
			if !isLocalSource(env.source) {
				for _, s := range codexSparse {
					args = append(args, "--sparse", s)
				}
			}
			var res struct {
				MarketplaceName string `json:"marketplaceName"`
			}
			if err := c.json(ctx, &res, args...); err != nil {
				return fail(err)
			}
			if res.MarketplaceName != codexMarketplace {
				return fail(fmt.Errorf("%s holds the marketplace %q, not %q", env.source, res.MarketplaceName, codexMarketplace))
			}
			r.Marketplace = env.source
			r.Done = append(r.Done, "added the marketplace "+codexMarketplace+" from "+env.source)
		case foreign:
			r.Error = fmt.Sprintf("did not install or update %s: the Codex marketplace %s comes from %s, not %s (see warnings)", codexPlugin, codexMarketplace, mkt.location(), env.source)
		case mkt.MarketplaceSource != nil && mkt.MarketplaceSource.SourceType == "git":
			// Refresh the clone, so the reinstall below sees new files.
			var res json.RawMessage
			if err := c.json(ctx, &res, "plugin", "marketplace", "upgrade", codexMarketplace, "--json"); err != nil {
				r.Warnings = append(r.Warnings, "could not refresh the marketplace: "+err.Error())
			}
		}
		switch {
		case r.Error != "":
			// Nothing installed or updated.
		case installed != nil && !installed.Enabled:
			// `codex plugin add` would enable it again: leave the user's
			// choice alone (todo below).
		default:
			before := codexCacheDigest(env)
			var res struct {
				PluginID string `json:"pluginId"`
			}
			if err := c.json(ctx, &res, "plugin", "add", codexPlugin, "--json"); err != nil {
				return fail(err)
			}
			switch after := codexCacheDigest(env); {
			case installed == nil:
				r.Done = append(r.Done, "installed "+codexPlugin)
			case before == "" || after == "":
				r.Done = append(r.Done, "reinstalled "+codexPlugin+" from the marketplace")
			case before != after:
				r.Done = append(r.Done, "updated "+codexPlugin+" (its files changed)")
			}
		}
	case setupRemove:
		if installed != nil {
			var res json.RawMessage
			if err := c.json(ctx, &res, "plugin", "remove", codexPlugin, "--json"); err != nil {
				return fail(err)
			}
			r.Done = append(r.Done, "uninstalled "+codexPlugin)
		}
		if mkt != nil && !foreign {
			// Keep the marketplace while any other plugin from it is
			// installed.
			after, err := c.installed(ctx)
			if err != nil {
				return fail(err)
			}
			var others []string
			for _, p := range after {
				if p.Installed && p.MarketplaceName == codexMarketplace && p.PluginID != codexPlugin {
					others = append(others, p.PluginID)
				}
			}
			if len(others) > 0 {
				r.Warnings = append(r.Warnings, fmt.Sprintf("kept the Codex marketplace %s: %s is still installed from it", codexMarketplace, strings.Join(others, ", ")))
				break
			}
			var res json.RawMessage
			if err := c.json(ctx, &res, "plugin", "marketplace", "remove", codexMarketplace, "--json"); err != nil {
				return fail(err)
			}
			r.Done = append(r.Done, "removed the marketplace "+codexMarketplace)
			r.Marketplace = ""
		}
	}

	// Verify by asking Codex.
	if mode != setupCheck {
		if list, err = c.installed(ctx); err != nil {
			return fail(err)
		}
		installed = findCodexPlugin(list)
	}
	if installed != nil {
		r.Installed, r.Enabled, r.Scope = true, installed.Enabled, "user"
		if installed.Version != nil {
			r.Version = *installed.Version
		}
	}
	switch {
	case mode == setupInstall && !r.Installed && r.Error == "":
		return fail(fmt.Errorf("codex plugin list does not show %s after the install", codexPlugin))
	case mode == setupRemove && r.Installed:
		return fail(fmt.Errorf("codex plugin list still shows %s", codexPlugin))
	case mode == setupCheck && !r.Installed:
		r.Todo = append(r.Todo, "install the plugin: flopwire setup")
	}
	if r.Installed && !r.Enabled {
		r.Todo = append(r.Todo, "the Codex plugin is disabled, so setup did not update it; enable it in Codex (/plugins), then run flopwire setup again")
	}
	if len(r.Done) > 0 {
		r.Todo = append(r.Todo, "restart running Codex sessions to apply the change")
	}
	if r.Installed {
		hooks, cfg, err := codexAsk(ctx, path, env.cwd)
		if err != nil {
			r.Warnings = append(r.Warnings, "could not ask Codex whether the plugin hooks are trusted: "+err.Error())
		} else {
			codexTrust(&r, hooks)
			r.Warnings = append(r.Warnings, codexManualEntries(env, hooks, cfg)...)
		}
	}
	return r
}

// codexTrust fills the plugin hooks' trust state and, when an approval is
// still needed, says how to give it.
func codexTrust(r *harnessReport, hooks []codexHook) {
	t := &hookTrustReport{NeedReview: []string{}, Disabled: []string{}}
	for _, h := range hooks {
		if h.PluginID != codexPlugin {
			continue
		}
		t.Hooks++
		ev := codexEventName(h.EventName)
		switch {
		case !h.Enabled:
			t.Disabled = append(t.Disabled, ev)
		case h.TrustStatus == "trusted" || h.TrustStatus == "managed":
			t.Trusted++
		default: // untrusted, modified
			t.NeedReview = append(t.NeedReview, ev)
		}
	}
	r.HookTrust = t
	if !r.Enabled {
		return
	}
	if t.Hooks == 0 {
		r.Warnings = append(r.Warnings, "Codex lists no hooks from "+codexPlugin+"; messages will not arrive in Codex sessions")
		return
	}
	if len(t.NeedReview) > 0 {
		r.Todo = append(r.Todo, fmt.Sprintf("approve the plugin's hooks once in Codex: start codex; at \"Hooks need review\" choose Review hooks and trust the %d Flopwire hooks (%s; each runs `flopwire hook || true`), or run /hooks later. Until you do, Codex skips them: no messages arrive and no standing instruction. setup does not approve hooks for you", len(t.NeedReview), strings.Join(t.NeedReview, ", ")))
	}
	if len(t.Disabled) > 0 {
		r.Warnings = append(r.Warnings, fmt.Sprintf("you disabled the Flopwire hooks for %s in Codex (/hooks); messages do not arrive through those events", strings.Join(t.Disabled, ", ")))
	}
}

// codexManualEntries returns a warning for each older manual Flopwire entry
// in Codex's config that the plugin now duplicates: a hook running flopwire
// hook or agent flush, a notify running flopwire agent flush, an MCP server
// running flopwire mcp. All come from Codex's own answers; setup edits
// nothing.
func codexManualEntries(env *setupEnv, hooks []codexHook, cfg json.RawMessage) []string {
	var warn []string
	for _, h := range hooks {
		if h.Source == "plugin" || !manualHookRe.MatchString(h.Command) {
			continue
		}
		warn = append(warn, fmt.Sprintf("%s runs %q on %s; the plugin runs flopwire hook on that event too, so remove that entry yourself (setup does not edit it)", tildePath(h.SourcePath, env.home), h.Command, codexEventName(h.EventName)))
	}
	var cr struct {
		Layers []struct {
			Name struct {
				Type           string `json:"type"`
				File           string `json:"file"`
				DotCodexFolder string `json:"dotCodexFolder"`
			} `json:"name"`
			Config struct {
				Notify     []string `json:"notify"`
				MCPServers map[string]struct {
					Command string   `json:"command"`
					Args    []string `json:"args"`
				} `json:"mcp_servers"`
			} `json:"config"`
		} `json:"layers"`
	}
	if json.Unmarshal(cfg, &cr) != nil {
		return warn
	}
	for _, l := range cr.Layers {
		file := l.Name.File
		if file == "" && l.Name.DotCodexFolder != "" {
			file = filepath.Join(l.Name.DotCodexFolder, "config.toml")
		}
		if file == "" {
			file = l.Name.Type + " config"
		}
		file = tildePath(file, env.home)
		if n := l.Config.Notify; len(n) >= 3 && filepath.Base(n[0]) == "flopwire" && n[1] == "agent" && n[2] == "flush" {
			warn = append(warn, fmt.Sprintf("%s has notify = %q; the plugin's Stop hook now asks the agent to index each finished turn, so remove that line yourself (setup does not edit it)", file, n))
		}
		names := make([]string, 0, len(l.Config.MCPServers))
		for n, s := range l.Config.MCPServers {
			if filepath.Base(s.Command) == "flopwire" && len(s.Args) > 0 && s.Args[0] == "mcp" {
				names = append(names, n)
			}
		}
		slices.Sort(names)
		for _, n := range names {
			how := "remove it from that file yourself"
			if l.Name.Type == "user" {
				how = "remove it: codex mcp remove " + n
			}
			warn = append(warn, fmt.Sprintf("%s has an MCP server %q that runs flopwire mcp; the plugin provides the same tools, so %s", file, n, how))
		}
	}
	return warn
}
