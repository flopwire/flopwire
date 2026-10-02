package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/client"
)

// flopwire setup installs Flopwire into each coding-agent harness on this
// machine through the harness's own plugin commands (issues #58, #59, #60).
// It never edits a harness's settings files itself; it reads them only to
// warn about older manual entries that would now run twice.

// defaultPluginSource is where a harness fetches the Flopwire plugins: this
// repository, which is its own marketplace.
const defaultPluginSource = "flopwire/flopwire"

// envPluginSource overrides defaultPluginSource: a local checkout or
// directory for development and tests, or another owner/repo or git URL.
const envPluginSource = "FLOPWIRE_PLUGIN_SOURCE"

// harnessCommandTimeout bounds one harness command; adding a marketplace
// from GitHub clones it.
var harnessCommandTimeout = 3 * time.Minute

const setupHelp = `flopwire setup — install Flopwire into the coding-agent harnesses on this machine

  flopwire setup            install or update the plugin in each harness found
  flopwire setup --check    report what is installed; change nothing
  flopwire setup --remove   uninstall the plugin through each harness's own command
  flopwire setup --text     a readable report instead of JSON

For each harness it finds, setup runs that harness's own plugin commands. It
never edits the harness's settings files. Harnesses: Claude Code (claude),
Codex (codex) and Devin CLI (devin). Devin loads the Claude Code plugin;
setup installs it with devin plugins install --local, on this machine only.

Codex runs a plugin's hooks only after you trust them once: start codex and
answer its "Hooks need review" prompt, or use /hooks. setup reports whether
that is still needed and never approves hooks for you.

The plugin runs "flopwire hook" and "flopwire mcp", so flopwire must be on
PATH, and messaging needs the device agent (flopwire agent run). setup
reports both and starts neither.

Flags
  --check            report only
  --remove           uninstall
  --text             readable output (default: JSON)
  --source SRC       plugin marketplace: owner/repo, a git URL or a local
                     directory such as a checkout of this repository
                     (default $FLOPWIRE_PLUGIN_SOURCE, else flopwire/flopwire)
  --scope SCOPE      Claude Code install scope: user (default), project or
                     local; project and local apply to the current directory.
                     Codex and Devin install for the user only

JSON: {"kind":"setup","mode","ok","flopwire":{"path","note"},"agent":{"running",
"socket"},"server":{"configured","url"},"harnesses":[{"harness","detected","command",
"harness_version","plugin","marketplace","installed","enabled","version","scope",
"done":[…],"todo":[…],"warnings":[…],"error","hook_trust":{"hooks","trusted",
"need_review":[…],"disabled":[…]}}],"todo":[…]}. hook_trust is Codex only.
Exit 1 when a harness command failed.
`

// setupReport is what setup prints.
type setupReport struct {
	Kind      string          `json:"kind"`
	Mode      string          `json:"mode"`
	OK        bool            `json:"ok"`
	Flopwire  setupBinary     `json:"flopwire"`
	Agent     setupAgent      `json:"agent"`
	Server    setupServer     `json:"server"`
	Harnesses []harnessReport `json:"harnesses"`
	Todo      []string        `json:"todo"`
}

type setupBinary struct {
	Path string `json:"path,omitempty"`
	Note string `json:"note,omitempty"`
}

type setupAgent struct {
	Running bool   `json:"running"`
	Socket  string `json:"socket"`
}

type setupServer struct {
	Configured bool   `json:"configured"`
	URL        string `json:"url,omitempty"`
}

// harnessReport is one harness's state after setup ran.
type harnessReport struct {
	Harness        string   `json:"harness"`
	Detected       bool     `json:"detected"`
	Command        string   `json:"command,omitempty"`
	HarnessVersion string   `json:"harness_version,omitempty"`
	Plugin         string   `json:"plugin,omitempty"`
	Marketplace    string   `json:"marketplace,omitempty"`
	Installed      bool     `json:"installed"`
	Enabled        bool     `json:"enabled"`
	Version        string   `json:"version,omitempty"`
	Scope          string   `json:"scope,omitempty"`
	Done           []string `json:"done"`
	Todo           []string `json:"todo"`
	Warnings       []string `json:"warnings"`
	Error          string   `json:"error,omitempty"`
	// HookTrust is Codex's trust state for the plugin's hooks.
	HookTrust *hookTrustReport `json:"hook_trust,omitempty"`
}

// Setup modes.
const (
	setupInstall = "install"
	setupCheck   = "check"
	setupRemove  = "remove"
)

// setupEnv is what a harness entry needs: the request and the machine.
type setupEnv struct {
	mode   string
	source string // marketplace source as given to the harness
	scope  string
	cwd    string
	home   string
	// run runs a harness command and returns its stdout, stderr and error.
	run func(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)
	// lookPath finds a harness binary.
	lookPath func(string) (string, error)
}

// setupHarness is one harness setup knows. To add a harness, append an
// entry whose apply detects it, then installs, checks or removes the
// Flopwire plugin with that harness's own commands, according to env.mode.
type setupHarness struct {
	name  string
	apply func(ctx context.Context, env *setupEnv) harnessReport
}

// setupHarnesses is every harness setup handles, in report order.
var setupHarnesses = []setupHarness{
	{name: "claude", apply: setupClaude},
	{name: "codex", apply: setupCodex},
	{name: "devin", apply: setupDevin},
}

func setupMain(ctx context.Context, args []string) error {
	return setupCmd(ctx, args, os.Stdout, os.Stderr)
}

func setupCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	check := fs.Bool("check", false, "")
	remove := fs.Bool("remove", false, "")
	text := fs.Bool("text", false, "")
	source := fs.String("source", "", "")
	scope := fs.String("scope", "user", "")
	help := fs.Bool("help", false, "")
	fs.BoolVar(help, "h", false, "")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("setup: %w (flopwire setup --help)", err)
	}
	if *help {
		_, err := io.WriteString(stdout, setupHelp)
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("setup takes no arguments; got %q (flopwire setup --help)", strings.Join(fs.Args(), " "))
	}
	if *check && *remove {
		return errors.New("setup: --check and --remove do not combine")
	}
	if !slices.Contains([]string{"user", "project", "local"}, *scope) {
		return fmt.Errorf("setup: --scope is user, project or local; got %q", *scope)
	}
	env, err := newSetupEnv(*source, *scope)
	if err != nil {
		return err
	}
	switch {
	case *check:
		env.mode = setupCheck
	case *remove:
		env.mode = setupRemove
	}
	rep := runSetup(ctx, env)
	if *text {
		writeSetupText(stdout, rep)
	} else if err := json.NewEncoder(stdout).Encode(rep); err != nil {
		return err
	}
	if !rep.OK {
		return errReported
	}
	return nil
}

func newSetupEnv(source, scope string) (*setupEnv, error) {
	if source == "" {
		source = os.Getenv(envPluginSource)
	}
	if source == "" {
		source = defaultPluginSource
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(source, "-") {
		// The source is one argument of `claude plugin marketplace add`;
		// a leading dash would turn it into an option.
		return nil, fmt.Errorf("setup: plugin source %q starts with -: give owner/repo, a git URL or a directory", source)
	}
	home, _ := os.UserHomeDir()
	if isLocalSource(source) {
		p := source
		if strings.HasPrefix(p, "~/") && home != "" {
			p = filepath.Join(home, p[2:])
		}
		if p, err = filepath.Abs(p); err != nil {
			return nil, err
		}
		if _, err := os.Stat(filepath.Join(p, ".claude-plugin", "marketplace.json")); err != nil {
			return nil, fmt.Errorf("setup: --source %s has no .claude-plugin/marketplace.json: point it at a checkout of github.com/flopwire/flopwire", p)
		}
		source = p
	}
	return &setupEnv{mode: setupInstall, source: source, scope: scope, cwd: cwd, home: home, run: runHarnessCommand, lookPath: exec.LookPath}, nil
}

// isLocalSource reports whether a marketplace source is a path.
func isLocalSource(s string) bool {
	return strings.HasPrefix(s, "/") || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") || strings.HasPrefix(s, "~/") || s == "." || s == ".."
}

func runHarnessCommand(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, harnessCommandTimeout)
	defer cancel()
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	// A child the harness started (git for a clone) can hold the output
	// open after the harness is killed; stop waiting for it.
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		err = fmt.Errorf("timed out after %s", harnessCommandTimeout)
	}
	return out.Bytes(), errb.Bytes(), err
}

func runSetup(ctx context.Context, env *setupEnv) setupReport {
	rep := setupReport{Kind: "setup", Mode: env.mode, OK: true, Todo: []string{}}
	if p, err := env.lookPath("flopwire"); err != nil {
		rep.Flopwire.Note = "flopwire is not on PATH: the plugin runs `flopwire hook` and `flopwire mcp`, so its hooks fail and its tools do not load"
		if env.mode != setupRemove {
			rep.Todo = append(rep.Todo, "put the flopwire binary on PATH (for example in ~/.local/bin), then restart your agent sessions")
		}
	} else {
		rep.Flopwire.Path = p
		if self, err := os.Executable(); err == nil && !samePath(self, p) {
			rep.Flopwire.Note = fmt.Sprintf("the plugin runs %s, not this binary (%s)", p, self)
		}
	}
	rep.Agent.Socket, _ = defaultSocket()
	if rep.Agent.Socket != "" {
		c, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := agent.Call(c, rep.Agent.Socket, agent.Request{Op: "status"})
		cancel()
		rep.Agent.Running = err == nil
	}
	if cfg, err := client.Load(); err == nil {
		rep.Server = setupServer{Configured: true, URL: cfg.Server}
	}
	if env.mode != setupRemove {
		if !rep.Agent.Running {
			rep.Todo = append(rep.Todo, "start the device agent and keep it running: flopwire agent run (messages and capture need it; see docs/agent.md)")
		}
		if !rep.Server.Configured {
			rep.Todo = append(rep.Todo, "optional: no server is configured, so messages go only between this device's sessions; flopwire claim and flopwire enroll join a team")
		}
	}
	for _, h := range setupHarnesses {
		r := h.apply(ctx, env)
		if r.Error != "" {
			rep.OK = false
		}
		rep.Harnesses = append(rep.Harnesses, r)
	}
	return rep
}

func samePath(a, b string) bool {
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		a = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		b = rb
	}
	return a == b
}

// --- Claude Code ---

// The Claude Code marketplace and plugin names, from
// .claude-plugin/marketplace.json and the plugin's manifest.
const (
	claudeMarketplace = "flopwire"
	claudePlugin      = "flopwire@flopwire"
)

// claudeResult is the last stdout line of a `claude plugin … --json` command.
type claudeResult struct {
	Outcome       string `json:"outcome"`
	Message       string `json:"message"`
	FailureCode   string `json:"failureCode"`
	UpdateOutcome string `json:"updateOutcome"`
	OldVersion    string `json:"oldVersion"`
	NewVersion    string `json:"newVersion"`
}

type claudeMarketplaceEntry struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Repo   string `json:"repo"`
	URL    string `json:"url"`
	Path   string `json:"path"`
	Ref    string `json:"ref"`
}

func (m claudeMarketplaceEntry) location() string {
	loc := m.URL
	switch {
	case m.Repo != "":
		loc = m.Repo
	case m.Path != "":
		return m.Path
	}
	if m.Ref != "" {
		loc += "#" + m.Ref
	}
	return loc
}

type claudePluginEntry struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	Scope       string `json:"scope"`
	Enabled     bool   `json:"enabled"`
	ProjectPath string `json:"projectPath"`
	// Errors are load errors and Notes are warnings. Claude Code 2.1.287
	// prints each only when it has some, so an empty list is no field.
	Errors []string `json:"errors,omitempty"`
	Notes  []string `json:"notes,omitempty"`
}

// claudeCLI runs `claude plugin` commands for one setup.
type claudeCLI struct {
	env  *setupEnv
	path string
}

func (c claudeCLI) raw(ctx context.Context, args ...string) ([]byte, []byte, error) {
	return c.env.run(ctx, c.path, args...)
}

// result runs a command that prints one JSON result line. A failed outcome
// is returned as the result, not an error; err is a command that did not
// answer.
func (c claudeCLI) result(ctx context.Context, args ...string) (claudeResult, error) {
	out, errb, err := c.raw(ctx, args...)
	var r claudeResult
	if line := lastJSONLine(out); line == nil || json.Unmarshal(line, &r) != nil || r.Outcome == "" {
		return r, commandError(c.path, args, out, errb, err)
	}
	return r, nil
}

func (c claudeCLI) marketplaces(ctx context.Context) ([]claudeMarketplaceEntry, error) {
	args := []string{"plugin", "marketplace", "list", "--json"}
	out, errb, err := c.raw(ctx, args...)
	var l []claudeMarketplaceEntry
	if err != nil || json.Unmarshal(bytes.TrimSpace(out), &l) != nil {
		return nil, commandError(c.path, args, out, errb, err)
	}
	return l, nil
}

// plugin returns Flopwire's install at the setup's scope, if any.
func (c claudeCLI) plugin(ctx context.Context) (*claudePluginEntry, error) {
	args := []string{"plugin", "list", "--json"}
	out, errb, err := c.raw(ctx, args...)
	var l []claudePluginEntry
	if err != nil || json.Unmarshal(bytes.TrimSpace(out), &l) != nil {
		return nil, commandError(c.path, args, out, errb, err)
	}
	for i := range l {
		p := l[i]
		if p.ID != claudePlugin || p.Scope != c.env.scope {
			continue
		}
		if c.env.scope != "user" && p.ProjectPath != "" && !samePath(p.ProjectPath, c.env.cwd) {
			continue
		}
		return &p, nil
	}
	return nil, nil
}

// installsFrom lists the plugins Claude Code has installed from Flopwire's
// marketplace, in any scope or project.
func (c claudeCLI) installsFrom(ctx context.Context) ([]string, error) {
	args := []string{"plugin", "list", "--json"}
	out, errb, err := c.raw(ctx, args...)
	var l []claudePluginEntry
	if err != nil || json.Unmarshal(bytes.TrimSpace(out), &l) != nil {
		return nil, commandError(c.path, args, out, errb, err)
	}
	var ids []string
	for _, p := range l {
		if strings.HasSuffix(p.ID, "@"+claudeMarketplace) {
			id := p.ID + " (" + p.Scope + " scope"
			if p.ProjectPath != "" {
				id += ", " + p.ProjectPath
			}
			ids = append(ids, id+")")
		}
	}
	return ids, nil
}

func commandError(path string, args []string, out, errb []byte, err error) error {
	msg := strings.TrimSpace(string(errb))
	if msg == "" {
		msg = strings.TrimSpace(string(out))
	}
	if len(msg) > 400 {
		msg = msg[:400] + "…"
	}
	cmd := filepath.Base(path) + " " + strings.Join(args, " ")
	switch {
	case err != nil && msg != "":
		return fmt.Errorf("%s: %v: %s", cmd, err, msg)
	case err != nil:
		return fmt.Errorf("%s: %v", cmd, err)
	}
	return fmt.Errorf("%s: unexpected output: %s", cmd, msg)
}

// lastJSONLine is the last stdout line that holds a JSON object: the
// result line of a `claude plugin … --json` command.
func lastJSONLine(out []byte) []byte {
	lines := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		l := bytes.TrimSpace(lines[i])
		if bytes.HasPrefix(l, []byte("{")) {
			return l
		}
	}
	return nil
}

// sameSource reports whether a configured marketplace comes from source.
// A source may pin a branch or tag as owner/repo#ref; Claude Code lists
// the ref apart.
func sameSource(m claudeMarketplaceEntry, source string) bool {
	if isLocalSource(source) {
		return m.Path != "" && samePath(m.Path, source)
	}
	norm := func(s string) string {
		s = strings.TrimSuffix(strings.TrimSuffix(strings.ToLower(s), "/"), ".git")
		return strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "github.com/")
	}
	loc, ref, _ := strings.Cut(source, "#")
	have := m.URL
	if m.Repo != "" {
		have = m.Repo
	}
	return norm(have) == norm(loc) && m.Ref == ref
}

func setupClaude(ctx context.Context, env *setupEnv) harnessReport {
	r := harnessReport{Harness: "claude", Plugin: claudePlugin, Scope: env.scope, Done: []string{}, Todo: []string{}, Warnings: []string{}}
	path, err := env.lookPath("claude")
	if err != nil {
		r.Plugin, r.Scope = "", ""
		return r
	}
	r.Detected, r.Command = true, path
	if out, _, err := env.run(ctx, path, "--version"); err == nil {
		r.HarnessVersion = strings.TrimSpace(string(out))
	}
	c := claudeCLI{env: env, path: path}
	fail := func(err error) harnessReport {
		r.Error = err.Error()
		return r
	}
	mkts, err := c.marketplaces(ctx)
	if err != nil {
		return fail(err)
	}
	var mkt *claudeMarketplaceEntry
	for i := range mkts {
		if mkts[i].Name == claudeMarketplace {
			mkt = &mkts[i]
		}
	}
	if mkt != nil {
		r.Marketplace = mkt.location()
	}
	// A marketplace named flopwire from another source is a fork or a
	// name squatter: setup installs, updates and removes nothing through it.
	foreign := mkt != nil && !sameSource(*mkt, env.source)
	if foreign {
		r.Warnings = append(r.Warnings, fmt.Sprintf("the marketplace %s comes from %s, not %s; setup installs, updates and removes nothing through a marketplace it did not add. If you trust it, run flopwire setup --source %s. To switch, run claude plugin marketplace remove %s (it uninstalls every plugin from it), then flopwire setup", claudeMarketplace, mkt.location(), env.source, mkt.location(), claudeMarketplace))
	}
	installed, err := c.plugin(ctx)
	if err != nil {
		return fail(err)
	}
	scopeArgs := []string{"--scope", env.scope}

	switch env.mode {
	case setupInstall:
		switch {
		case mkt == nil:
			args := append([]string{"plugin", "marketplace", "add", env.source, "--json"}, scopeArgs...)
			if !isLocalSource(env.source) {
				args = append(args, "--sparse", ".claude-plugin", "plugins")
			}
			res, err := c.result(ctx, args...)
			if err != nil {
				return fail(err)
			}
			if res.Outcome != "ok" {
				return fail(fmt.Errorf("add the marketplace %s: %s", env.source, res.Message))
			}
			r.Marketplace = env.source
			r.Done = append(r.Done, "added the marketplace "+claudeMarketplace+" from "+env.source)
		case foreign:
			r.Error = fmt.Sprintf("did not install or update %s: the marketplace %s comes from %s, not %s (see warnings)", claudePlugin, claudeMarketplace, mkt.location(), env.source)
		default:
			// Refresh the catalog, so an update below sees new versions.
			if res, err := c.result(ctx, "plugin", "marketplace", "update", claudeMarketplace, "--json"); err != nil {
				r.Warnings = append(r.Warnings, "could not refresh the marketplace: "+err.Error())
			} else if res.Outcome != "ok" {
				r.Warnings = append(r.Warnings, "could not refresh the marketplace: "+res.Message)
			}
		}
		if r.Error != "" {
			// Nothing installed or updated.
		} else if installed == nil {
			res, err := c.result(ctx, append([]string{"plugin", "install", claudePlugin, "--json"}, scopeArgs...)...)
			if err != nil {
				return fail(err)
			}
			if res.Outcome != "ok" {
				return fail(fmt.Errorf("install %s: %s", claudePlugin, res.Message))
			}
			r.Done = append(r.Done, "installed "+claudePlugin)
		} else {
			res, err := c.result(ctx, append([]string{"plugin", "update", claudePlugin, "--json"}, scopeArgs...)...)
			if err != nil {
				return fail(err)
			}
			if res.Outcome != "ok" {
				return fail(fmt.Errorf("update %s: %s", claudePlugin, res.Message))
			}
			if res.UpdateOutcome != "up_to_date" && res.OldVersion != res.NewVersion {
				r.Done = append(r.Done, fmt.Sprintf("updated %s from %s to %s", claudePlugin, res.OldVersion, res.NewVersion))
			}
		}
	case setupRemove:
		if installed != nil {
			res, err := c.result(ctx, append([]string{"plugin", "uninstall", claudePlugin, "--json"}, scopeArgs...)...)
			if err != nil {
				return fail(err)
			}
			if res.Outcome != "ok" && res.FailureCode != "not_installed" {
				return fail(fmt.Errorf("uninstall %s: %s", claudePlugin, res.Message))
			}
			r.Done = append(r.Done, "uninstalled "+claudePlugin)
		}
		if mkt != nil && !foreign {
			// Removing a marketplace uninstalls every plugin installed from
			// it, in every scope: keep it while anything else uses it.
			others, err := c.installsFrom(ctx)
			if err != nil {
				return fail(err)
			}
			if len(others) > 0 {
				r.Warnings = append(r.Warnings, fmt.Sprintf("kept the marketplace %s: Claude Code still has %s installed from it, and removing the marketplace would uninstall them", claudeMarketplace, strings.Join(others, ", ")))
				break
			}
			res, err := c.result(ctx, append([]string{"plugin", "marketplace", "remove", claudeMarketplace, "--json"}, scopeArgs...)...)
			switch {
			case err != nil:
				return fail(err)
			case res.Outcome == "ok":
				r.Done = append(r.Done, "removed the marketplace "+claudeMarketplace)
				r.Marketplace = ""
			case res.FailureCode != "not_configured":
				r.Warnings = append(r.Warnings, fmt.Sprintf("the marketplace %s stays: %s", claudeMarketplace, res.Message))
			}
		}
	}

	// Verify by asking Claude Code.
	if env.mode != setupCheck {
		if installed, err = c.plugin(ctx); err != nil {
			return fail(err)
		}
	}
	if installed != nil {
		r.Installed, r.Enabled, r.Version = true, installed.Enabled, installed.Version
		for _, e := range installed.Errors {
			r.Warnings = append(r.Warnings, "Claude Code reports a load error: "+e)
		}
		for _, n := range installed.Notes {
			r.Warnings = append(r.Warnings, "Claude Code notes: "+n)
		}
	}
	switch {
	case env.mode == setupInstall && !r.Installed && r.Error == "":
		return fail(fmt.Errorf("claude plugin list does not show %s at %s scope after the install", claudePlugin, env.scope))
	case env.mode == setupRemove && r.Installed:
		return fail(fmt.Errorf("claude plugin list still shows %s at %s scope", claudePlugin, env.scope))
	case env.mode == setupCheck && !r.Installed:
		r.Todo = append(r.Todo, "install the plugin: flopwire setup")
	}
	if r.Installed && !r.Enabled {
		r.Todo = append(r.Todo, fmt.Sprintf("the plugin is disabled; enable it with: claude plugin enable %s --scope %s", claudePlugin, env.scope))
	}
	if len(r.Done) > 0 {
		r.Todo = append(r.Todo, "restart running Claude Code sessions, or run /reload-plugins in each, to apply the change")
	}
	if r.Installed {
		r.Warnings = append(r.Warnings, claudeManualEntries(env)...)
	}
	return r
}

// manualHookRe matches a hook that runs flopwire's hook or flush command.
var manualHookRe = regexp.MustCompile(`(^|[\s/"'])flopwire["']?\s+(hook|agent\s+flush)\b`)

// claudeManualEntries reads Claude Code's settings files, read-only, and
// returns a warning for each manual Flopwire hook or MCP server that the
// plugin now duplicates.
func claudeManualEntries(env *setupEnv) []string {
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if configDir == "" && env.home != "" {
		configDir = filepath.Join(env.home, ".claude")
	}
	var warn []string
	var files []string
	if configDir != "" {
		files = append(files, filepath.Join(configDir, "settings.json"))
	}
	files = append(files, filepath.Join(env.cwd, ".claude", "settings.json"), filepath.Join(env.cwd, ".claude", "settings.local.json"))
	seen := map[string]bool{}
	for _, f := range files {
		if seen[f] {
			continue
		}
		seen[f] = true
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var s struct {
			Hooks map[string][]struct {
				Hooks []struct {
					Command string   `json:"command"`
					Args    []string `json:"args"`
				} `json:"hooks"`
			} `json:"hooks"`
		}
		if json.Unmarshal(raw, &s) != nil {
			continue
		}
		events := make([]string, 0, len(s.Hooks))
		for ev := range s.Hooks {
			events = append(events, ev)
		}
		slices.Sort(events)
		for _, ev := range events {
			for i, g := range s.Hooks[ev] {
				for _, h := range g.Hooks {
					line := strings.TrimSpace(h.Command + " " + strings.Join(h.Args, " "))
					if manualHookRe.MatchString(line) {
						warn = append(warn, fmt.Sprintf("%s runs %q on %s (hooks.%s[%d]); the plugin runs flopwire hook on that event too, so remove that entry from the file yourself (setup does not edit it)", tildePath(f, env.home), line, ev, ev, i))
					}
				}
			}
		}
	}
	// A user-level MCP server added with `claude mcp add`.
	claudeJSON := filepath.Join(env.home, ".claude.json")
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		claudeJSON = filepath.Join(d, ".claude.json")
	}
	if env.home != "" || os.Getenv("CLAUDE_CONFIG_DIR") != "" {
		if raw, err := os.ReadFile(claudeJSON); err == nil {
			var cj struct {
				MCPServers map[string]struct {
					Command string   `json:"command"`
					Args    []string `json:"args"`
				} `json:"mcpServers"`
			}
			if json.Unmarshal(raw, &cj) == nil {
				names := make([]string, 0, len(cj.MCPServers))
				for n, s := range cj.MCPServers {
					if filepath.Base(s.Command) == "flopwire" && len(s.Args) > 0 && s.Args[0] == "mcp" {
						names = append(names, n)
					}
				}
				slices.Sort(names)
				for _, n := range names {
					warn = append(warn, fmt.Sprintf("a user MCP server %q runs flopwire mcp; the plugin provides the same tools, so remove it: claude mcp remove %s --scope user", n, n))
				}
			}
		}
	}
	return warn
}

func tildePath(p, home string) string {
	if home != "" && strings.HasPrefix(p, home+string(filepath.Separator)) {
		return "~" + p[len(home):]
	}
	return p
}

// writeSetupText prints rep for a person.
func writeSetupText(w io.Writer, rep setupReport) {
	var b strings.Builder
	if rep.Flopwire.Path != "" {
		fmt.Fprintf(&b, "flopwire: %s\n", rep.Flopwire.Path)
	} else {
		b.WriteString("flopwire: not on PATH\n")
	}
	if rep.Flopwire.Note != "" {
		fmt.Fprintf(&b, "  note: %s\n", rep.Flopwire.Note)
	}
	if rep.Agent.Running {
		b.WriteString("agent: running\n")
	} else {
		b.WriteString("agent: not running\n")
	}
	if rep.Server.Configured {
		fmt.Fprintf(&b, "server: %s\n", rep.Server.URL)
	} else {
		b.WriteString("server: none (messages stay on this device)\n")
	}
	for _, h := range rep.Harnesses {
		if !h.Detected {
			fmt.Fprintf(&b, "%s: not found\n", h.Harness)
			continue
		}
		fmt.Fprintf(&b, "%s: %s", h.Harness, h.Command)
		if h.HarnessVersion != "" {
			fmt.Fprintf(&b, " (%s)", h.HarnessVersion)
		}
		b.WriteString("\n")
		switch {
		case h.Installed:
			state := "enabled"
			if !h.Enabled {
				state = "disabled"
			}
			fmt.Fprintf(&b, "  plugin: %s %s, %s scope, %s\n", h.Plugin, h.Version, h.Scope, state)
		case h.Plugin != "":
			fmt.Fprintf(&b, "  plugin: %s not installed\n", h.Plugin)
		}
		if h.Marketplace != "" {
			fmt.Fprintf(&b, "  marketplace: %s\n", h.Marketplace)
		}
		if t := h.HookTrust; t != nil {
			fmt.Fprintf(&b, "  hooks: %d of %d trusted", t.Trusted, t.Hooks)
			if len(t.NeedReview) > 0 {
				fmt.Fprintf(&b, "; need your approval: %s", strings.Join(t.NeedReview, ", "))
			}
			if len(t.Disabled) > 0 {
				fmt.Fprintf(&b, "; disabled: %s", strings.Join(t.Disabled, ", "))
			}
			b.WriteString("\n")
		}
		for _, d := range h.Done {
			fmt.Fprintf(&b, "  done: %s\n", d)
		}
		if len(h.Done) == 0 && h.Error == "" && rep.Mode != setupCheck {
			b.WriteString("  done: nothing to change\n")
		}
		for _, x := range h.Warnings {
			fmt.Fprintf(&b, "  warning: %s\n", x)
		}
		if h.Error != "" {
			fmt.Fprintf(&b, "  error: %s\n", h.Error)
		}
		for _, t := range h.Todo {
			fmt.Fprintf(&b, "  todo: %s\n", t)
		}
	}
	for _, t := range rep.Todo {
		fmt.Fprintf(&b, "todo: %s\n", t)
	}
	io.WriteString(w, b.String())
}
