package main

// `flopwire probe` re-runs the message-bus delivery marker tests
// (notes/message-bus/probes-2026-10-01.md, docs/probe.md) against the
// harness versions installed on this machine, and fails loudly when a hook
// stops reaching the model or the wrapper's framing changes.
//
// For each harness it creates a scratch project whose project-scope hooks
// run `flopwire probe tap` (probe_tap.go): the real `flopwire hook`, plus a
// log of what each hook printed. It starts two headless sessions there (a
// sender and a recipient, probe_drivers.go), sends real messages from one
// to the other through the device agent's bus, and judges each case
// (probe_verdict.go) by the hook log and by the model quoting the
// message's marker. It never writes the user's harness configuration:
// Claude Code runs with --setting-sources project; Codex and Devin run in
// scratch homes holding a copy of their login file.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/devin"
	"github.com/flopwire/flopwire/internal/transcript/opencode"
	opencodeplugin "github.com/flopwire/flopwire/plugins/opencode"
)

// probeHarnesses are the harnesses the probe drives, in run order.
var probeHarnesses = []transcript.Agent{transcript.AgentClaude, transcript.AgentCodex, transcript.AgentDevin, transcript.AgentOpencode}

// probeDefaultModel is each harness's cheapest model that follows the
// probe's prompts (2026-10-03). --model overrides it.
var probeDefaultModel = map[transcript.Agent]string{
	transcript.AgentClaude: "haiku",
	transcript.AgentCodex:  "gpt-5.6-luna",
	transcript.AgentDevin:  "swe-2-medium",
	// opencode's free model; it needs no login.
	transcript.AgentOpencode: "opencode/big-pickle",
}

// probeBinary is the command each harness runs as.
var probeBinary = map[transcript.Agent]string{
	transcript.AgentClaude:   "claude",
	transcript.AgentCodex:    "codex",
	transcript.AgentDevin:    "devin",
	transcript.AgentOpencode: "opencode",
}

const probeUsage = `Usage: flopwire probe [--harness claude,codex,devin,opencode] [--case CASES] [--model M] [--local] [--json] [--notes]

Re-runs the message-bus delivery tests against the installed harnesses, in
a scratch project with project-scope hooks only. Each case prints PASS or
FAIL with its evidence; the command exits non-zero on any FAIL.

Cases: idle, prompt-submit, framing, mid-turn, subagent, guardian (Codex
only). Codex, Devin and opencode run in scratch homes the running agent
does not watch, so they need --local. See docs/probe.md.`

type probeOpts struct {
	harnesses []transcript.Agent
	cases     []string
	models    map[transcript.Agent]string
	asJSON    bool
	notes     bool
	notesFile string
	local     bool
	dir       string
	socket    string
	idleWait  time.Duration
	turnWait  time.Duration
}

// probeReport is what one run found.
type probeReport struct {
	Date     time.Time      `json:"date"`
	Flopwire string         `json:"flopwire"`
	Mode     string         `json:"mode"` // local or agent
	Dir      string         `json:"dir"`
	Harness  []probeVersion `json:"harnesses"`
	Results  []probeResult  `json:"results"`
	// Skipped names a harness the probe did not run, and why.
	Skipped []string `json:"skipped,omitempty"`

	home string // the user's home directory, kept out of the notes
}

type probeVersion struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Model   string `json:"model"`
}

func probeMain(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "tap" {
		return probeTap(ctx, args[1:], os.Stdin, os.Stdout, os.Stderr)
	}
	o, err := parseProbeFlags(args, os.Stderr)
	if err != nil {
		return err
	}
	rep, err := runProbe(ctx, o, os.Stderr)
	if err != nil {
		return err
	}
	if o.asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else if err := writeProbeTable(os.Stdout, rep); err != nil {
		return err
	}
	if o.notes {
		if err := appendProbeNotes(o.notesFile, rep); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "probe: appended the run to %s\n", o.notesFile)
	}
	if n := rep.failed(); n > 0 {
		return fmt.Errorf("probe: %d of %d cases failed", n, len(rep.Results))
	}
	return nil
}

func (r probeReport) failed() int {
	n := 0
	for _, x := range r.Results {
		if !x.Pass {
			n++
		}
	}
	return n
}

func parseProbeFlags(args []string, stderr io.Writer) (probeOpts, error) {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, probeUsage); fs.PrintDefaults() }
	harness := fs.String("harness", "", "comma-separated harnesses (claude, codex, devin, opencode); default every one installed")
	cases := fs.String("case", "", "comma-separated cases; default all: "+strings.Join(probeCases, ","))
	model := fs.String("model", "", "model: NAME for every chosen harness, or HARNESS=NAME,... (defaults: claude=haiku, codex=gpt-5.6-luna, devin=swe-2-medium, opencode=opencode/big-pickle)")
	o := probeOpts{models: map[transcript.Agent]string{}}
	fs.BoolVar(&o.asJSON, "json", false, "print the report as JSON")
	fs.BoolVar(&o.notes, "notes", false, "append the run to --notes-file")
	fs.StringVar(&o.notesFile, "notes-file", filepath.Join("notes", "message-bus", "probe-runs.md"), "the run log --notes appends to")
	fs.BoolVar(&o.local, "local", false, "start a local-only device agent in the scratch directory instead of using the running one")
	fs.StringVar(&o.dir, "dir", "", "scratch directory (default: a new one under the OS temp directory)")
	fs.StringVar(&o.socket, "socket", "", "the running agent's control socket (default <config dir>/agent.sock; not with --local)")
	fs.DurationVar(&o.idleWait, "idle-wait", 60*time.Second, "idle case: how long a message to an idle session must stay queued")
	fs.DurationVar(&o.turnWait, "turn-timeout", 5*time.Minute, "longest a harness turn may take")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("probe: unexpected argument %q", fs.Arg(0))
	}
	for _, h := range splitList(*harness) {
		a := transcript.Agent(h)
		if !slices.Contains(probeHarnesses, a) {
			return o, fmt.Errorf("probe: unknown harness %q (claude, codex, devin or opencode)", h)
		}
		o.harnesses = append(o.harnesses, a)
	}
	for _, c := range splitList(*cases) {
		if !slices.Contains(probeCases, c) {
			return o, fmt.Errorf("probe: unknown case %q (%s)", c, strings.Join(probeCases, ", "))
		}
		o.cases = append(o.cases, c)
	}
	if len(o.cases) == 0 {
		o.cases = probeCases
	}
	for _, m := range splitList(*model) {
		h, name, ok := strings.Cut(m, "=")
		if !ok {
			if len(o.harnesses) != 1 {
				return o, errors.New("probe: a bare --model needs exactly one --harness; use HARNESS=MODEL pairs")
			}
			h, name = string(o.harnesses[0]), m
		}
		if !slices.Contains(probeHarnesses, transcript.Agent(h)) {
			return o, fmt.Errorf("probe: --model: unknown harness %q", h)
		}
		o.models[transcript.Agent(h)] = name
	}
	if o.local && o.socket != "" {
		return o, errors.New("probe: --socket names a running agent; --local starts its own")
	}
	return o, nil
}

// prober is one probe run.
type prober struct {
	o         probeOpts
	dir       string // scratch root (symlinks resolved)
	exe       string // this flopwire binary
	sock      string
	claudeDir string // Claude Code's config directory (read only)
	home      string
	log       io.Writer
	mirror    *claudeMirror
}

func runProbe(ctx context.Context, o probeOpts, log io.Writer) (probeReport, error) {
	rep := probeReport{Date: time.Now().UTC(), Flopwire: version, Mode: "agent"}
	if o.local {
		rep.Mode = "local"
	}
	harnesses := o.harnesses
	if len(harnesses) == 0 {
		for _, h := range probeHarnesses {
			if _, err := exec.LookPath(probeBinary[h]); err == nil {
				harnesses = append(harnesses, h)
			} else {
				fmt.Fprintf(log, "probe: %s is not installed; skipped\n", probeBinary[h])
			}
		}
		if len(harnesses) == 0 {
			return rep, errors.New("probe: none of claude, codex, devin, opencode is installed")
		}
	}
	noCase := func() error {
		if slices.ContainsFunc(harnesses, func(h transcript.Agent) bool { return len(casesFor(h, o.cases)) > 0 }) {
			return nil
		}
		return fmt.Errorf("probe: no case of %s applies to %s", strings.Join(o.cases, ", "), joinAgents(harnesses))
	}
	if err := noCase(); err != nil {
		return rep, err
	}
	for _, h := range harnesses {
		if _, err := exec.LookPath(probeBinary[h]); err != nil {
			return rep, fmt.Errorf("probe: %s is not installed", probeBinary[h])
		}
		if !o.local && h != transcript.AgentClaude {
			return rep, fmt.Errorf("probe: %s runs in a scratch home the running agent does not watch; pass --local", h)
		}
	}
	p := &prober{o: o, log: &lockedWriter{w: log}}
	var err error
	if p.exe, err = os.Executable(); err != nil {
		return rep, err
	}
	if p.home, err = os.UserHomeDir(); err != nil {
		return rep, err
	}
	if slices.Contains(harnesses, transcript.AgentCodex) {
		if b, err := os.ReadFile(codexAuthPath(p.home)); err == nil {
			if due, why := codexRefreshDue(b, time.Now(), codexRefreshWindow); due {
				msg := "codex: Codex token refresh due (" + why + "); run `codex` once to refresh, then rerun the probe"
				fmt.Fprintf(log, "probe: SKIP %s\n", msg)
				rep.Skipped = append(rep.Skipped, msg)
				harnesses = slices.DeleteFunc(harnesses, func(h transcript.Agent) bool { return h == transcript.AgentCodex })
				if len(harnesses) == 0 {
					return rep, errors.New("probe: " + msg)
				}
				// What is left must still run a case, or the run proves nothing.
				if err := noCase(); err != nil {
					return rep, fmt.Errorf("%w (%s)", err, msg)
				}
			}
		}
	}
	p.claudeDir = os.Getenv("CLAUDE_CONFIG_DIR")
	if p.claudeDir == "" {
		p.claudeDir = filepath.Join(p.home, ".claude")
	}
	rep.home = p.home
	if err := p.makeDir(); err != nil {
		return rep, err
	}
	rep.Dir = p.dir
	fmt.Fprintf(log, "probe: scratch directory %s\n", p.dir)
	stopAgent, err := p.startAgent(ctx)
	if err != nil {
		return rep, err
	}
	defer stopAgent()
	// The harnesses run at once, each in its own project with its own
	// sessions and hook log: a full run then takes as long as the slowest.
	results := make([][]probeResult, len(harnesses))
	var wg sync.WaitGroup
	for i, h := range harnesses {
		model := o.models[h]
		if model == "" {
			model = probeDefaultModel[h]
		}
		rep.Harness = append(rep.Harness, probeVersion{Name: string(h), Version: p.harnessVersion(ctx, h), Model: model})
		wg.Go(func() { results[i] = p.runHarness(ctx, h, model) })
	}
	wg.Wait()
	for _, rs := range results {
		rep.Results = append(rep.Results, rs...)
	}
	if ctx.Err() != nil {
		return rep, ctx.Err()
	}
	return rep, nil
}

func (p *prober) makeDir() error {
	dir := p.o.dir
	if dir == "" {
		d, err := os.MkdirTemp("", "flopwire-probe-")
		if err != nil {
			return err
		}
		dir = d
	} else if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Harnesses name the project by its real path (/var is /private/var
	// on macOS); the hooks, the trust entry and the verdicts must agree.
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	p.dir = real
	return nil
}

// lockedWriter serializes progress lines from the harness runs.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

// casesFor is the cases that apply to harness h.
func casesFor(h transcript.Agent, cases []string) []string {
	var out []string
	for _, c := range cases {
		if c == caseGuardian && h != transcript.AgentCodex {
			continue
		}
		out = append(out, c)
	}
	return out
}

func joinAgents(as []transcript.Agent) string {
	s := make([]string, len(as))
	for i, a := range as {
		s[i] = string(a)
	}
	return strings.Join(s, ", ")
}

// scratchHome is the home a harness runs in: CODEX_HOME for Codex, HOME
// for Devin, the root of the XDG directories for opencode; empty for
// Claude Code.
func (p *prober) scratchHome(h transcript.Agent) string {
	switch h {
	case transcript.AgentCodex:
		return filepath.Join(p.dir, "codex", "home")
	case transcript.AgentDevin:
		return filepath.Join(p.dir, "devin", "home")
	case transcript.AgentOpencode:
		return filepath.Join(p.dir, "opencode", "home")
	}
	return ""
}

// opencodeDB is the store of the probe's opencode sessions.
func (p *prober) opencodeDB() string {
	return filepath.Join(p.scratchHome(transcript.AgentOpencode), "data", "opencode", "opencode.db")
}

// harnessEnv is the environment harness h runs with, in its scratch home.
func (p *prober) harnessEnv(h transcript.Agent) []string {
	env := probeEnv(os.Environ(), h == transcript.AgentDevin)
	switch h {
	case transcript.AgentCodex:
		env = append(env, "CODEX_HOME="+p.scratchHome(h))
	case transcript.AgentDevin:
		env = append(env, "HOME="+p.scratchHome(h))
	case transcript.AgentOpencode:
		// opencode reads its config (and the plugin), and writes its store,
		// under the XDG directories. The plugin runs the tap as its hook and
		// talks to the probe's agent.
		home := p.scratchHome(h)
		hook, _ := json.Marshal([]string{"probe", "tap", "--log", filepath.Join(p.dir, string(h), "tap.jsonl"), "--socket", p.sock})
		env = append(env, "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "XDG_DATA_HOME="+filepath.Join(home, "data"),
			"XDG_STATE_HOME="+filepath.Join(home, "state"), "XDG_CACHE_HOME="+filepath.Join(home, "cache"),
			"FLOPWIRE_BIN="+p.exe, "FLOPWIRE_HOOK_ARGS="+string(hook), "FLOPWIRE_SOCKET="+p.sock,
			"FLOPWIRE_CONFIG="+filepath.Join(p.dir, "flopwire", "config.json"))
	}
	return env
}

// harnessVersion is `<binary> --version`, first line. It runs in the
// scratch home: even --version makes Codex write its home (tmp/arg0).
func (p *prober) harnessVersion(ctx context.Context, h transcript.Agent) string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if home := p.scratchHome(h); home != "" {
		if err := os.MkdirAll(home, 0o700); err != nil {
			return "unknown (" + err.Error() + ")"
		}
	}
	cmd := exec.CommandContext(ctx, probeBinary[h], "--version")
	cmd.Env, cmd.Dir = p.harnessEnv(h), p.dir
	out, err := cmd.Output()
	if err != nil {
		return "unknown (" + err.Error() + ")"
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return line
}

// --- device agent ---

// startAgent starts a local-only agent watching the scratch homes
// (--local), or checks that the user's agent answers.
func (p *prober) startAgent(ctx context.Context) (stop func(), err error) {
	if !p.o.local {
		p.sock = p.o.socket
		if p.sock == "" {
			if p.sock, err = defaultSocket(); err != nil {
				return nil, err
			}
		}
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if _, err := agent.Call(pctx, p.sock, agent.Request{Op: "ping"}); err != nil {
			return nil, fmt.Errorf("probe: no device agent at %s (start it with flopwire agent run, or pass --local): %w", p.sock, err)
		}
		return func() {}, nil
	}
	p.sock = filepath.Join(p.dir, "agent.sock")
	sockDir := ""          // a short directory for the socket, removed at the end
	if len(p.sock) > 100 { // sun_path is 104 bytes on macOS
		d, err := os.MkdirTemp("/tmp", "fwp-")
		if err != nil {
			return nil, err
		}
		sockDir, p.sock = d, filepath.Join(d, "a.sock")
	}
	projects := filepath.Join(p.dir, "claude", "projects")
	if err := os.MkdirAll(projects, 0o700); err != nil {
		return nil, err
	}
	// The agent reads Claude Code's live-session registry (read only) for
	// presence; the probe's transcripts are mirrored next to it, so the
	// agent does not index every other transcript of the user.
	if err := os.Symlink(filepath.Join(p.claudeDir, "sessions"), filepath.Join(p.dir, "claude", "sessions")); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	p.mirror = &claudeMirror{dst: projects, srcs: make(chan string, 8)}
	mctx, mcancel := context.WithCancel(ctx)
	go p.mirror.run(mctx)
	logf, err := os.OpenFile(filepath.Join(p.dir, "agent.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		mcancel()
		return nil, err
	}
	cmd := exec.Command(p.exe, "agent", "run", "--no-sync", "--socket", p.sock, "--claude-projects", projects,
		"--codex-home", filepath.Join(p.dir, "codex", "home"),
		"--devin-db", filepath.Join(p.dir, "devin", "home", ".local", "share", "devin", "cli", "sessions.db"),
		"--opencode-db", p.opencodeDB(),
		"--sweep", "5s")
	cmd.Env = append(probeEnv(os.Environ(), false),
		"FLOPWIRE_CONFIG="+filepath.Join(p.dir, "flopwire", "config.json"),
		"FLOPWIRE_INDEX="+filepath.Join(p.dir, "flopwire", "index.db"),
		client.EnvToken+"=", client.EnvServer+"=")
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		mcancel()
		logf.Close()
		return nil, err
	}
	stop = func() {
		mcancel()
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		logf.Close()
		if sockDir != "" {
			_ = os.Remove(p.sock)
			_ = os.Remove(sockDir)
		}
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		pctx, cancel := context.WithTimeout(ctx, time.Second)
		_, err := agent.Call(pctx, p.sock, agent.Request{Op: "ping"})
		cancel()
		if err == nil {
			fmt.Fprintf(p.log, "probe: local agent at %s (log %s)\n", p.sock, logf.Name())
			return stop, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			stop()
			return nil, fmt.Errorf("probe: the local agent did not answer: %v (see %s)", err, logf.Name())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// probeEnv is the environment a harness or the agent runs with: the
// user's, without the variables a surrounding agent session sets (the
// probe may run inside one) and without Flopwire's own. XDG directories
// are dropped too when the harness gets a scratch HOME (Devin).
func probeEnv(env []string, scratchHome bool) []string {
	keep := map[string]bool{"CLAUDE_CODE_OAUTH_TOKEN": true, "CLAUDE_CODE_USE_BEDROCK": true, "CLAUDE_CODE_USE_VERTEX": true}
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case keep[k]:
		case k == "CLAUDECODE", k == "CLAUDE_PID", k == "CLAUDE_EFFORT",
			strings.HasPrefix(k, "CLAUDE_CODE_"), strings.HasPrefix(k, "CLAUDE_PLUGIN_"),
			strings.HasPrefix(k, "CODEX_"), strings.HasPrefix(k, "DEVIN_"), strings.HasPrefix(k, "CHISEL_"), strings.HasPrefix(k, "OPENCODE"),
			strings.HasPrefix(k, "FLOPWIRE_"):
			continue
		case scratchHome && (k == "HOME" || strings.HasPrefix(k, "XDG_")):
			continue
		}
		out = append(out, kv)
	}
	return out
}

// claudeMirror copies the probe's Claude Code project directory (the
// harness writes it under the user's ~/.claude/projects) into the local
// agent's projects root, appending what each file gained.
type claudeMirror struct {
	dst  string
	srcs chan string
}

func (m *claudeMirror) add(src string) { m.srcs <- src }

func (m *claudeMirror) run(ctx context.Context) {
	var srcs []string
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case s := <-m.srcs:
			if !slices.Contains(srcs, s) {
				srcs = append(srcs, s)
			}
		case <-tick.C:
		}
		for _, s := range srcs {
			mirrorDir(s, filepath.Join(m.dst, filepath.Base(s)))
		}
	}
}

// mirrorDir appends to each file under dst what its source under src
// gained since the last copy; a source that shrank is copied whole.
func mirrorDir(src, dst string) {
	_ = filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return nil
		}
		to := filepath.Join(dst, rel)
		si, err := d.Info()
		if err != nil {
			return nil
		}
		var off int64
		if di, err := os.Stat(to); err == nil {
			off = di.Size()
		}
		if off == si.Size() {
			return nil
		}
		if off > si.Size() {
			off = 0
		}
		in, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer in.Close()
		if _, err := in.Seek(off, io.SeekStart); err != nil {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
			return nil
		}
		flags := os.O_WRONLY | os.O_CREATE | os.O_APPEND
		if off == 0 {
			flags |= os.O_TRUNC
		}
		out, err := os.OpenFile(to, flags, 0o600)
		if err != nil {
			return nil
		}
		_, _ = io.CopyN(out, in, si.Size()-off)
		_ = out.Close()
		return nil
	})
}

// --- per harness ---

// harnessRun is one harness's probe: its scratch project, hook log and
// sessions.
type harnessRun struct {
	p      *prober
	name   transcript.Agent
	model  string
	root   string // <scratch>/<harness>
	proj   string
	tap    string
	home   string // Codex: CODEX_HOME; Devin: HOME
	login  *loginCopy
	env    []string
	sender probeSession
	recv   probeSession
}

func (p *prober) runHarness(ctx context.Context, h transcript.Agent, model string) (results []probeResult) {
	r := &harnessRun{p: p, name: h, model: model, root: filepath.Join(p.dir, string(h))}
	r.proj, r.tap = filepath.Join(r.root, "project"), filepath.Join(r.root, "tap.jsonl")
	fail := func(c, why string) []probeResult {
		var out []probeResult
		for _, x := range casesFor(h, p.o.cases) {
			if c == "" || c == x {
				out = append(out, probeResult{Harness: string(h), Case: x, Evidence: "setup failed: " + why})
			}
		}
		return out
	}
	// The harnesses run in goroutines: a panic here would end the process
	// before the other harnesses delete their login copies. It fails this
	// harness's cases instead (the deferred removeLogin below runs first).
	defer func() {
		if v := recover(); v != nil {
			results = fail("", fmt.Sprintf("panic: %v", v))
		}
	}()
	// Registered before setup, which may fail after copying the login.
	defer r.removeLogin()
	fmt.Fprintf(p.log, "probe: %s (%s)\n", h, model)
	if err := r.setup(); err != nil {
		return fail("", err.Error())
	}
	var err error
	if r.sender, err = r.session(ctx, "sender", nil); err != nil {
		return fail("", err.Error())
	}
	defer r.sender.Close()
	if r.recv, err = r.session(ctx, "recipient", nil); err != nil {
		return fail("", err.Error())
	}
	defer r.recv.Close()
	fmt.Fprintf(p.log, "probe: %s sender %s, recipient %s\n", h, r.sender.ID(), r.recv.ID())
	var out []probeResult
	for _, c := range casesFor(h, p.o.cases) {
		if ctx.Err() != nil {
			break
		}
		fmt.Fprintf(p.log, "probe: %s %s\n", h, c)
		res := r.runCase(ctx, c)
		fmt.Fprintf(p.log, "probe: %s %s %s: %s\n", h, c, res.verdict(), res.Evidence)
		out = append(out, res)
	}
	all, _ := readTap(r.tap)
	checked := checkDeliveredOnce(out, all)
	for i := range checked {
		if checked[i].Pass != out[i].Pass {
			fmt.Fprintf(p.log, "probe: %s %s %s: %s\n", h, checked[i].Case, checked[i].verdict(), checked[i].Evidence)
		}
	}
	return checked
}

// setup writes the scratch project's hooks and, for Codex and Devin, the
// scratch home with a copy of the login file.
func (r *harnessRun) setup() error {
	if err := os.MkdirAll(r.proj, 0o700); err != nil {
		return err
	}
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	cmd := q(r.p.exe) + " probe tap --log " + q(r.tap) + " --socket " + q(r.p.sock)
	group := func(ev string) []map[string]any {
		g := map[string]any{"hooks": []map[string]any{{"type": "command", "command": cmd, "timeout": 10}}}
		if ev == evPostToolUse || ev == "PreToolUse" {
			g["matcher"] = "*"
		}
		return []map[string]any{g}
	}
	events := append(slices.Clone(probeHookEvents), probeObserveEvents...)
	hooks := map[string]any{}
	for _, ev := range events {
		if r.name == transcript.AgentDevin && strings.HasPrefix(ev, "Subagent") {
			continue // Devin has no subagent events: run_subagent's tool hooks mark it
		}
		hooks[ev] = group(ev)
	}
	write := func(path string, v any) error {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		b, _ := json.MarshalIndent(v, "", "  ")
		return os.WriteFile(path, b, 0o600)
	}
	r.env, r.home = r.p.harnessEnv(r.name), r.p.scratchHome(r.name)
	switch r.name {
	case transcript.AgentClaude:
		return write(filepath.Join(r.proj, ".claude", "settings.json"), map[string]any{"hooks": hooks})
	case transcript.AgentCodex:
		l, err := copyLogin(codexAuthPath(r.p.home), filepath.Join(r.home, "auth.json"))
		if err != nil {
			return err
		}
		r.login = &l
		cfg := fmt.Sprintf("[projects.%q]\ntrust_level = \"trusted\"\n", r.proj)
		if err := os.WriteFile(filepath.Join(r.home, "config.toml"), []byte(cfg), 0o600); err != nil {
			return err
		}
		return write(filepath.Join(r.proj, ".codex", "hooks.json"), map[string]any{"hooks": hooks})
	case transcript.AgentDevin:
		data := os.Getenv("XDG_DATA_HOME")
		if data == "" {
			data = filepath.Join(r.p.home, ".local", "share")
		}
		l, err := copyLogin(filepath.Join(data, "devin", "credentials.toml"), filepath.Join(r.home, ".local", "share", "devin", "credentials.toml"))
		if err != nil {
			return err
		}
		r.login = &l
		return write(filepath.Join(r.proj, ".devin", "hooks.v1.json"), hooks)
	case transcript.AgentOpencode:
		// The plugin, in the scratch config's global plugin directory, as
		// setup installs it; its hook is the tap (harnessEnv).
		dir := filepath.Join(r.home, "config", "opencode", "plugins")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, opencodeplugin.FileName), opencodeplugin.Source, 0o600); err != nil {
			return err
		}
		// opencode needs no login for its free models; a login the user has
		// is copied like the other harnesses'.
		data := os.Getenv("XDG_DATA_HOME")
		if data == "" {
			data = filepath.Join(r.p.home, ".local", "share")
		}
		if src := filepath.Join(data, "opencode", "auth.json"); func() bool { _, err := os.Stat(src); return err == nil }() {
			l, err := copyLogin(src, filepath.Join(r.home, "data", "opencode", "auth.json"))
			if err != nil {
				return err
			}
			r.login = &l
		}
		return write(filepath.Join(r.proj, "opencode.json"), map[string]any{"$schema": "https://opencode.ai/config.json", "autoupdate": false,
			"share": "disabled", "permission": map[string]any{"bash": "allow", "edit": "allow"}})
	}
	return fmt.Errorf("unknown harness %s", r.name)
}

// removeLogin deletes the scratch copy of the login file, and warns
// loudly when the harness refreshed it during the run.
func (r *harnessRun) removeLogin() {
	if r.login == nil {
		return
	}
	if err := r.login.release(); err != nil {
		fmt.Fprintf(r.p.log, "probe: WARNING: %s: %v\n", r.name, err)
	}
}

// loginCopy is a harness's login file copied into a scratch home.
type loginCopy struct {
	src, dst string
	orig     []byte // src's content when copied
}

// copyLogin copies a harness's login file into the scratch home (0600).
// The user's file is only read, never written.
func copyLogin(src, dst string) (loginCopy, error) {
	b, err := os.ReadFile(src)
	if err != nil {
		return loginCopy{}, fmt.Errorf("the harness login file: %w (log in first)", err)
	}
	l := loginCopy{src: src, dst: dst, orig: b}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return l, err
	}
	return l, os.WriteFile(dst, b, 0o600)
}

// release deletes the copy. A copy that changed means the harness
// refreshed its login during the run; Codex's refresh tokens are single
// use, so the user's own file may now hold a used refresh token. The
// probe never writes the user's harness files: it says so, loudly.
func (l loginCopy) release() error {
	now, err := os.ReadFile(l.dst)
	_ = os.Remove(l.dst)
	if err != nil || bytes.Equal(now, l.orig) {
		return nil
	}
	return fmt.Errorf("the harness refreshed its login in the probe's copy during the run. Its refresh token is single use, so %s may now hold a used one and the next refresh may fail; if the harness then asks you to log in, run `codex login`", l.src)
}

// codexRefreshWindow is how far ahead the probe looks for a Codex token
// refresh: a full run takes minutes; the rest is margin.
const codexRefreshWindow = 30 * time.Minute

// Codex refreshes its ChatGPT login when the access token is within 5
// minutes of expiry, or 8 days after last_refresh (codex-rs login, 0.160).
const (
	codexRefreshBeforeExpiry = 5 * time.Minute
	codexRefreshInterval     = 8 * 24 * time.Hour
)

// codexRefreshDue reports whether Codex could refresh the login in auth
// (its auth.json) before now+window: the probe then skips Codex rather
// than let a refresh in its copy use up the user's single-use refresh
// token. A login with no ChatGPT tokens (an API key) never refreshes. A
// ChatGPT login whose expiry cannot be read counts as due.
func codexRefreshDue(auth []byte, now time.Time, window time.Duration) (bool, string) {
	var a struct {
		Tokens *struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"tokens"`
		LastRefresh string `json:"last_refresh"`
	}
	if err := json.Unmarshal(auth, &a); err != nil {
		return true, "auth.json is not JSON"
	}
	if a.Tokens == nil || a.Tokens.RefreshToken == "" {
		return false, ""
	}
	until := now.Add(window)
	last, err := time.Parse(time.RFC3339Nano, a.LastRefresh)
	if err != nil {
		return true, "last_refresh is missing or unreadable"
	}
	if due := last.Add(codexRefreshInterval); !due.After(until) {
		return true, fmt.Sprintf("the 8-day refresh is due at %s", due.UTC().Format(time.RFC3339))
	}
	exp, ok := jwtExpiry(a.Tokens.AccessToken)
	if !ok {
		return true, "the access token's expiry is unreadable"
	}
	if due := exp.Add(-codexRefreshBeforeExpiry); !due.After(until) {
		return true, fmt.Sprintf("the access token expires at %s", exp.UTC().Format(time.RFC3339))
	}
	return false, ""
}

// jwtExpiry reads a JWT's exp claim without verifying it.
func jwtExpiry(tok string) (time.Time, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, false
	}
	var c struct {
		Exp float64 `json:"exp"`
	}
	if json.Unmarshal(b, &c) != nil || c.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(c.Exp), 0), true
}

// codexAuthPath is the user's Codex login file.
func codexAuthPath(home string) string {
	src := os.Getenv("CODEX_HOME")
	if src == "" {
		src = filepath.Join(home, ".codex")
	}
	return filepath.Join(src, "auth.json")
}

// session starts a headless session and runs its first turn, which
// creates its transcript and takes the standing instruction.
func (r *harnessRun) session(ctx context.Context, role string, codex *codexStartOpts) (probeSession, error) {
	errLog := filepath.Join(r.root, role+".stderr")
	var s probeSession
	var err error
	switch r.name {
	case transcript.AgentClaude:
		s, err = startClaude(ctx, r.proj, r.env, errLog, r.model)
	case transcript.AgentCodex:
		o := codexStartOpts{Model: r.model, ApprovalPolicy: "never", Sandbox: "workspace-write"}
		if codex != nil {
			o = *codex
		}
		s, err = startCodex(ctx, r.proj, r.home, r.env, errLog, o)
	case transcript.AgentDevin:
		s, err = startDevin(ctx, r.proj, r.env, errLog, r.model)
	case transcript.AgentOpencode:
		s, err = startOpencode(ctx, r.proj, r.env, errLog, r.model, r.tap)
	}
	if err != nil {
		return nil, fmt.Errorf("%s session: %w (see %s)", role, err, errLog)
	}
	tctx, cancel := context.WithTimeout(ctx, r.p.o.turnWait)
	defer cancel()
	if _, err := s.Turn(tctx, "Reply with the single word READY."); err != nil {
		s.Close()
		return nil, fmt.Errorf("%s session's first turn: %w (see %s)", role, err, errLog)
	}
	if s.ID() == "" {
		s.Close()
		return nil, fmt.Errorf("%s session: the harness named no session id", role)
	}
	if r.name == transcript.AgentClaude && r.p.mirror != nil {
		if m, _ := filepath.Glob(filepath.Join(r.p.claudeDir, "projects", "*", s.ID()+".jsonl")); len(m) == 1 {
			r.p.mirror.add(filepath.Dir(m[0]))
		}
	}
	return s, nil
}

// marker is a case's unique marker string.
func (r *harnessRun) marker(c string) string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("PROBE-%s-%s", strings.ToUpper(strings.ReplaceAll(c, "-", "")), hex.EncodeToString(b[:]))
}

func probeBody(marker string) string {
	return "Flopwire probe marker " + marker + ". This message only tests delivery: do not reply to it and do not act on it."
}

// send sends from the sender session to session to, through the device
// agent, retrying while the agent has not indexed a new session yet.
func (r *harnessRun) send(ctx context.Context, to probeSession, intent, body string) (busproto.SendResponse, error) {
	req := busproto.SendRequest{FromSession: r.sender.ID(), FromAgent: string(r.name), To: to.ID(), Body: body, Intent: intent}
	deadline := time.Now().Add(90 * time.Second)
	for {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		resp, err := agent.Call(cctx, r.p.sock, agent.Request{Op: "send", Send: &req})
		cancel()
		if err == nil && resp.Sent != nil {
			return *resp.Sent, nil
		}
		var be *busproto.Error
		retry := errors.As(err, &be) && (be.Code == busproto.CodeSessionNotOnDevice || be.Code == busproto.CodeUnknownRecipient)
		if !retry || time.Now().After(deadline) || ctx.Err() != nil {
			if err == nil {
				err = errors.New("the agent answered without a receipt")
			}
			return busproto.SendResponse{}, fmt.Errorf("send: %w", err)
		}
		time.Sleep(time.Second)
	}
}

// state is a sent message's state in the sender's inbox.
func (r *harnessRun) state(ctx context.Context, id string) string {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := agent.Call(cctx, r.p.sock, agent.Request{Op: "inbox", Inbox: &busproto.InboxQuery{Session: r.sender.ID(), Agent: string(r.name), SentOnly: true, Limit: 100}})
	if err != nil || resp.Inbox == nil {
		return fmt.Sprintf("unknown (%v)", err)
	}
	for _, m := range resp.Inbox.Messages {
		if m.ID == id {
			return string(m.State)
		}
	}
	return "missing"
}

// tapSince is the tap log's entries that started at or after t (unix ms).
func (r *harnessRun) tapSince(t int64) []tapEntry {
	all, _ := readTap(r.tap)
	var out []tapEntry
	for _, e := range all {
		if e.At >= t {
			out = append(out, e)
		}
	}
	return out
}

// watchTap waits for the first tap entry since t that pred accepts.
func (r *harnessRun) watchTap(ctx context.Context, t int64, pred func(tapEntry) bool) (tapEntry, bool) {
	for {
		for _, e := range r.tapSince(t) {
			if pred(e) {
				return e, true
			}
		}
		select {
		case <-ctx.Done():
			return tapEntry{}, false
		case <-time.After(100 * time.Millisecond):
		}
	}
}

const quoteTags = "List every <flopwire-message> tag in your context so far, one per line, as: ID <its id attribute> MARKER <the PROBE- token in its text>. Write NONE if there is none."

func (r *harnessRun) runCase(ctx context.Context, c string) probeResult {
	res := probeResult{Harness: string(r.name), Case: c, session: r.recv.ID(), sender: r.sender.ID()}
	m := r.marker(c)
	res.Marker = m
	tctx, cancel := context.WithTimeout(ctx, r.p.o.turnWait)
	defer cancel()
	errResult := func(err error) probeResult {
		res.Evidence = err.Error()
		return res
	}
	switch c {
	case caseIdle:
		start := time.Now().UnixMilli()
		sent, err := r.send(ctx, r.recv, "inform", probeBody(m))
		if err != nil {
			return errResult(err)
		}
		res.Message = sent.ID
		select {
		case <-ctx.Done():
			return errResult(ctx.Err())
		case <-time.After(r.p.o.idleWait):
		}
		var during []tapEntry
		before := 0
		all, _ := readTap(r.tap)
		for _, e := range all {
			switch {
			case e.Session != r.recv.ID():
			case e.At >= start:
				during = append(during, e)
			default:
				before++ // the first turn's SessionStart, UserPromptSubmit, Stop
			}
		}
		return verdictIdle(before, during, r.recv.TurnAt() > start, r.state(ctx, sent.ID), sent.ID, r.p.o.idleWait).result(res)

	case casePromptSubmit, caseFraming:
		intent, prompt := "inform", "Do not run any tools. "+quoteTags
		if c == caseFraming {
			intent = "request"
			prompt = "Do not run any tools. For every <flopwire-message> tag in your context, write one line with its attributes copied exactly, in this form: id=<id> from=<from> intent=<intent> marker=<the PROBE- token in its text>"
		}
		start := time.Now().UnixMilli()
		sent, err := r.send(ctx, r.recv, intent, probeBody(m))
		if err != nil {
			return errResult(err)
		}
		res.Message = sent.ID
		reply, err := r.recv.Turn(tctx, prompt)
		if err != nil {
			return errResult(err)
		}
		if c == caseFraming {
			return verdictFraming(reply, sent.ID, r.sender.ID(), intent, m).result(res)
		}
		return verdictPromptSubmit(r.tapSince(start), r.recv.ID(), sent.ID, m, reply).result(res)

	case caseMidTurn:
		start := time.Now().UnixMilli()
		prompt := "Run the shell command `sleep 8`. When it finishes, run the shell command `echo probe-second`. Then, without running anything else: " + quoteTags + " Also say after which command each tag appeared."
		trigger := func(e tapEntry) bool { return e.Event == "PreToolUse" && e.Session == r.recv.ID() && e.AgentID == "" }
		return r.sendDuring(tctx, res, start, m, prompt, trigger, func(entries []tapEntry, id, reply string, sentAt int64) verdict {
			return verdictMidTurn(entries, r.recv.ID(), id, m, reply, sentAt)
		})

	case caseSubagent:
		start := time.Now().UnixMilli()
		task := "Run the shell command `sleep 6`, then `echo sub-one`, then `echo sub-two`. Then report the id attribute of every <flopwire-message> tag in your context, or NONE."
		var how string
		switch r.name {
		case transcript.AgentClaude:
			how = "Use the Agent tool (subagent_type general-purpose, in the foreground) to start one subagent with this task: "
		case transcript.AgentCodex:
			how = "Spawn one subagent with the spawn_agent tool, with this task: "
		case transcript.AgentDevin:
			how = "Use the run_subagent tool to start one subagent with this task: "
		case transcript.AgentOpencode:
			how = "Use the task tool (subagent_type general) to start one subagent with this task: "
		}
		prompt := how + task + " Wait until the subagent has finished. Then run the shell command `echo parent-after`. Then: " + quoteTags + " Finally write SUBAGENT-SAW: and what the subagent reported."
		devinRun := false
		trigger := func(e tapEntry) bool {
			if e.Session != r.recv.ID() || e.Event != "PreToolUse" {
				return false
			}
			if e.AgentID != "" { // Claude Code, Codex, opencode: the subagent's first tool
				return true
			}
			if e.Tool == "run_subagent" {
				devinRun = true
				return false
			}
			return devinRun // Devin: a tool after run_subagent started is the subagent's
		}
		return r.sendDuring(tctx, res, start, m, prompt, trigger, func(entries []tapEntry, id, reply string, _ int64) verdict {
			seen, path, err := r.subagentTranscript(entries, m)
			return verdictSubagent(entries, r.recv.ID(), id, m, reply, seen, path, err)
		})

	case caseGuardian:
		return r.guardian(ctx, res, m)
	}
	return errResult(fmt.Errorf("unknown case %s", c))
}

// sendDuring runs a turn and sends the message when the tap log shows
// trigger, then judges the run.
func (r *harnessRun) sendDuring(ctx context.Context, res probeResult, start int64, m, prompt string, trigger func(tapEntry) bool,
	judge func(entries []tapEntry, id, reply string, sentAt int64) verdict) probeResult {
	type sendOut struct {
		id  string
		at  int64
		err error
	}
	wctx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	sentc := make(chan sendOut, 1)
	go func() {
		if _, ok := r.watchTap(wctx, start, trigger); !ok {
			sentc <- sendOut{err: errors.New("the trigger never fired (the model ran no matching tool)")}
			return
		}
		at := time.Now().UnixMilli()
		sent, err := r.send(ctx, r.recv, "inform", probeBody(m))
		sentc <- sendOut{id: sent.ID, at: at, err: err}
	}()
	reply, terr := r.recv.Turn(ctx, prompt)
	stopWatch()
	s := <-sentc
	res.Message = s.id
	if s.err != nil {
		res.Evidence = s.err.Error()
		if terr != nil {
			res.Evidence += "; turn: " + terr.Error()
		}
		return res
	}
	if terr != nil {
		res.Evidence = "turn: " + terr.Error()
		return res
	}
	return judge(r.tapSince(start), s.id, reply, s.at).result(res)
}

// subagentTranscript reports whether the subagent's own transcript holds
// the marker: Claude Code's subagents/agent-<id>.jsonl, Codex's child
// rollout, Devin's subagent nodes in its store. A transcript that cannot
// be read is an error, never a pass.
func (r *harnessRun) subagentTranscript(entries []tapEntry, marker string) (*bool, string, error) {
	if r.name == transcript.AgentDevin {
		return devinSubagentSeen(filepath.Join(r.home, ".local", "share", "devin", "cli", "sessions.db"), r.recv.ID(), marker)
	}
	if r.name == transcript.AgentOpencode {
		// The subagent is a child session; the plugin names it as agent_id.
		for _, e := range entries {
			if e.AgentID != "" {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				seen, err := opencode.SessionContains(ctx, r.p.opencodeDB(), e.AgentID, marker)
				if err != nil {
					return nil, r.p.opencodeDB(), err
				}
				return &seen, r.p.opencodeDB() + "#" + e.AgentID, nil
			}
		}
		return nil, "", nil
	}
	path := ""
	for _, e := range entries {
		if e.AgentID == "" {
			continue
		}
		switch r.name {
		case transcript.AgentClaude:
			path = claudeSubagentTranscript(hookInput{TranscriptPath: e.Transcript, AgentID: e.AgentID, AgentTranscriptPath: e.AgentTranscript})
		case transcript.AgentCodex:
			path = e.Transcript
			if e.AgentTranscript != "" {
				path = e.AgentTranscript
			}
		}
		if path != "" {
			break
		}
	}
	if path == "" {
		return nil, "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, path, err
	}
	seen := strings.Contains(string(b), marker)
	return &seen, path, nil
}

// devinSubagentSeen reads Devin's store for the marker in a subagent's
// nodes.
func devinSubagentSeen(db, session, marker string) (*bool, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	seen, err := devin.SubagentNodesContain(ctx, db, session, marker)
	if err != nil {
		return nil, db, err
	}
	return &seen, db, nil
}

// guardian: a Codex thread whose approvals go to the auto-review subagent
// ("guardian"); the message is sent when a review starts.
func (r *harnessRun) guardian(ctx context.Context, res probeResult, m string) probeResult {
	s, err := r.session(ctx, "guardian", &codexStartOpts{Model: r.model, ApprovalPolicy: "on-request", Reviewer: "auto_review", Sandbox: "workspace-write"})
	if err != nil {
		res.Evidence = err.Error()
		return res
	}
	defer s.Close()
	g := s.(*codexSession)
	res.session = g.ID()
	start := time.Now().UnixMilli()
	type sendOut struct {
		id  string
		err error
	}
	sentc := make(chan sendOut, 1)
	fired := false
	g.setObserver(func(n rpcMsg) {
		if n.Method != "item/autoApprovalReview/started" || fired {
			return
		}
		fired = true
		go func() {
			sent, err := r.send(ctx, g, "inform", probeBody(m))
			sentc <- sendOut{sent.ID, err}
		}()
	})
	tctx, cancel := context.WithTimeout(ctx, r.p.o.turnWait)
	defer cancel()
	prompt := "Run `curl -sS -o /dev/null -w '%{http_code}' https://example.com`. The sandbox blocks network access, so run it with escalated permissions (sandbox_permissions require_escalated) and a one-line justification. Then run the shell command `echo after-review`. Then, without running anything else: " + quoteTags
	reply, terr := g.Turn(tctx, prompt)
	g.setObserver(nil)
	var s0 sendOut
	select {
	case s0 = <-sentc:
	case <-time.After(30 * time.Second):
		s0.err = errors.New("no auto-review pass started")
	}
	res.Message = s0.id
	if terr != nil && s0.err == nil {
		res.Evidence = "turn: " + terr.Error()
		return res
	}
	var rs, re int64
	if revs := g.Reviews(); len(revs) > 0 {
		rs, re = revs[0].Start, revs[0].End
		if re == 0 {
			re = time.Now().UnixMilli()
		}
	}
	if s0.err != nil && rs != 0 {
		res.Evidence = s0.err.Error()
		return res
	}
	return verdictGuardian(r.tapSince(start), g.ID(), s0.id, m, reply, rs, re).result(res)
}

// --- report ---

func writeProbeTable(w io.Writer, rep probeReport) error {
	fmt.Fprintf(w, "flopwire probe %s (%s, flopwire %s)\n", rep.Date.Format("2006-01-02 15:04Z"), rep.Mode, rep.Flopwire)
	for _, h := range rep.Harness {
		fmt.Fprintf(w, "  %s: %s, model %s\n", h.Name, h.Version, h.Model)
	}
	for _, s := range rep.Skipped {
		fmt.Fprintf(w, "  SKIPPED %s\n", s)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "HARNESS\tCASE\tRESULT\tEVIDENCE")
	for _, r := range rep.Results {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Harness, r.Case, r.verdict(), r.Evidence)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(w, "%d passed, %d failed\n", len(rep.Results)-rep.failed(), rep.failed())
	return nil
}

// probeMarkdown is the run as a notes section.
func probeMarkdown(rep probeReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n\n", rep.Date.Format("2006-01-02 15:04Z"))
	fmt.Fprintf(&b, "flopwire %s, %s agent.", rep.Flopwire, rep.Mode)
	for _, h := range rep.Harness {
		fmt.Fprintf(&b, " %s: %s, model %s.", h.Name, h.Version, h.Model)
	}
	for _, s := range rep.Skipped {
		fmt.Fprintf(&b, " Skipped %s.", s)
	}
	b.WriteString("\n\n| Harness | Case | Result | Evidence |\n|---|---|---|---|\n")
	esc := func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "|", `\|`), "\n", " ") }
	// The notes are committed: the scratch and home paths and full session
	// ids (a FAIL row's evidence may hold them) are shortened.
	var pairs []string
	if rep.Dir != "" {
		pairs = append(pairs, rep.Dir, "<scratch>")
	}
	if rep.home != "" && rep.home != "/" {
		pairs = append(pairs, rep.home, "~")
	}
	for _, r := range rep.Results {
		for _, id := range []string{r.session, r.sender} {
			if len(id) > 8 {
				pairs = append(pairs, id, clip(id, 8))
			}
		}
	}
	scrub := strings.NewReplacer(pairs...)
	for _, r := range rep.Results {
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", r.Harness, r.Case, r.verdict(), esc(scrub.Replace(r.Evidence)))
	}
	fmt.Fprintf(&b, "\n%d passed, %d failed.\n", len(rep.Results)-rep.failed(), rep.failed())
	return b.String()
}

const probeNotesHeader = "# `flopwire probe` runs\n\nOne section per hand run of `flopwire probe --notes`, oldest first. See\n[`docs/probe.md`](../../docs/probe.md) for what each case proves.\n"

// appendProbeNotes appends the run to the notes file, creating it with a
// header.
func appendProbeNotes(path string, rep probeReport) error {
	head := ""
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if _, err := os.Stat(filepath.Dir(path)); err != nil {
			return fmt.Errorf("probe --notes: %w (run it from the repository root, or pass --notes-file)", err)
		}
		head = probeNotesHeader
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	w.WriteString(head)
	w.WriteString("\n" + probeMarkdown(rep))
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
