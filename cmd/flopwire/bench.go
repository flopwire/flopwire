package main

// `flopwire bench acceptance` runs the local-track acceptance checks of spec
// §11.2 against this device's real transcripts, read-only, in a scratch
// directory, and prints a pass/fail table. Parts run separately (--only)
// so each stays within a bounded run; results merge into one JSON file.
// scripts/acceptance.sh runs every part, including the oracle sample.
// With --home it reads a synthetic corpus instead (bench_ab.go), and
// --exe measures another flopwire binary with this harness.

import (
	"bufio"
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
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/flopwire/flopwire/internal/transcript/claude"
)

const benchUsage = `usage: flopwire bench acceptance --scratch DIR [--home DIR] [--exe BIN] [--only index,fresh,queries,report] [--json PATH]
       flopwire bench compare [--strict] OLD|DIR NEW
       flopwire bench corpus --out DIR [--seed N] [--size 1.5GB] [--verify]
       flopwire bench ab --a BIN --b BIN --home DIR --scratch DIR --out DIR [--runs 3]`

func benchCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(benchUsage)
	}
	switch args[0] {
	case "compare":
		return benchCompare(args[1:], os.Stdout)
	case "corpus":
		return benchCorpus(ctx, args[1:], os.Stdout)
	case "ab":
		return benchAB(ctx, args[1:], os.Stdout)
	case "acceptance":
		return benchAcceptance(ctx, args[1:])
	}
	return errors.New(benchUsage)
}

// harnessFlags are the corpus flags of acceptance and ab. With --home, the
// harness roots default to that directory's layout (a synthetic corpus,
// see internal/synthcorpus) instead of this user's.
type harnessFlags struct {
	home, claude, codex, devin *string
	fs                         *flag.FlagSet
}

func addHarnessFlags(fs *flag.FlagSet) *harnessFlags {
	h := &harnessFlags{fs: fs}
	h.home = fs.String("home", "", "corpus home: read .claude/projects, .codex and .local/share/devin under it instead of $HOME's (a synthetic corpus)")
	h.claude = fs.String("claude-projects", "", "Claude projects root (read-only; default <home>/.claude/projects)")
	h.codex = fs.String("codex-home", "", "Codex home (read-only; default <home>/.codex)")
	h.devin = fs.String("devin-db", "", "Devin sessions.db; copied before use (default <home>/.local/share/devin/cli/sessions.db)")
	return h
}

// resolve returns the harness roots and the home that ~/ in a query's repo
// names.
func (h *harnessFlags) resolve() (home, claudeDir, codexHome, devinDB string) {
	home = *h.home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	pick := func(v, def string) string {
		if v != "" {
			return v
		}
		return def
	}
	return home, pick(*h.claude, filepath.Join(home, ".claude", "projects")), pick(*h.codex, filepath.Join(home, ".codex")),
		pick(*h.devin, filepath.Join(home, ".local", "share", "devin", "cli", "sessions.db"))
}

func benchAcceptance(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bench acceptance", flag.ContinueOnError)
	scratch := fs.String("scratch", "", "scratch directory for indexes, copies and results (required)")
	only := fs.String("only", "index,fresh,queries,report", "parts to run: index, fresh, queries, report")
	hf := addHarnessFlags(fs)
	exeFlag := fs.String("exe", "", "flopwire binary under test (default this one)")
	queries := fs.String("queries", "testdata/acceptance/queries.yaml", "query set")
	index := fs.String("index", "", "index for the query part (default the one the index part built)")
	idleAfter := fs.Duration("idle-after", 60*time.Second, "how long the agent idles before its memory is read")
	jsonOut := fs.String("json", "", "with the report part, also write the acceptance record (docs/perf/README.md) to this file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *scratch == "" {
		return errors.New("--scratch is required")
	}
	if err := os.MkdirAll(*scratch, 0o755); err != nil {
		return err
	}
	exe := *exeFlag
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return err
		}
	}
	home, claudeDir, codexHome, devinDB := hf.resolve()
	b := &bench{exe: exe, scratch: *scratch, claude: claudeDir, codex: codexHome, devin: devinDB, home: home}
	if *only != "report" {
		var err error
		b.opencodeDB, err = b.probeAgent(ctx)
		if err != nil {
			return err
		}
	}
	res, err := b.load()
	if err != nil {
		return err
	}
	for _, part := range strings.Split(*only, ",") {
		if part == "report" {
			if err := b.report(res); err != nil {
				return err
			}
			if *jsonOut == "" {
				return nil
			}
			rec := newAccRecord(res, b.oracleReports(), buildRecordOf(), machineInfo(), time.Now())
			if err := writeAccRecord(*jsonOut, rec); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "bench: record written to %s; for a release commit it as docs/perf/%s\n", *jsonOut, recordFileName(rec))
			return nil
		}
		if err := b.runPart(ctx, res, part, *queries, *index, *idleAfter); err != nil {
			return err
		}
		if err := b.save(res); err != nil {
			return err
		}
	}
	return nil
}

// runPart runs one measuring part (index, fresh or queries) into res.
func (b *bench) runPart(ctx context.Context, res *accResults, part, queries, index string, idleAfter time.Duration) error {
	var err error
	switch part {
	case "index":
		res.Index, err = b.index(ctx, idleAfter)
	case "fresh":
		res.Fresh, err = b.fresh(ctx)
	case "queries":
		if index == "" {
			index = b.indexPath()
		}
		res.Queries, err = b.queries(ctx, queries, index)
	default:
		return fmt.Errorf("unknown part %q", part)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", part, err)
	}
	return nil
}

type bench struct {
	exe, scratch         string
	claude, codex, devin string
	home                 string // what ~/ in a query's repo means
	opencodeDB           bool   // set only after a valid capability probe
}

type accResults struct {
	Index   *indexResult   `json:"index,omitempty"`
	Fresh   *freshResult   `json:"fresh,omitempty"`
	Queries *queriesResult `json:"queries,omitempty"`
}

func (b *bench) resultsPath() string { return filepath.Join(b.scratch, "acceptance.json") }
func (b *bench) indexPath() string   { return filepath.Join(b.scratch, "index", "index.db") }

func (b *bench) load() (*accResults, error) {
	var r accResults
	data, err := os.ReadFile(b.resultsPath())
	if errors.Is(err, os.ErrNotExist) {
		return &r, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, json.Unmarshal(data, &r)
}

func (b *bench) save(r *accResults) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(b.resultsPath(), data, 0o644)
}

// --- a. full-corpus index, idle memory, no-change sweep ---

type indexResult struct {
	At         time.Time `json:"at"`
	WallS      float64   `json:"wall_s"`
	CPUS       float64   `json:"cpu_s"`
	PeakRSSMB  float64   `json:"peak_rss_mb"`
	IndexMB    float64   `json:"index_mb"`
	Rows       int64     `json:"rows"`
	Log        string    `json:"pass_log"`
	IdleRSSMB  float64   `json:"idle_rss_mb"`
	IdleFootMB float64   `json:"idle_footprint_mb"`
	// IdleAnonMB is the idle agent's anonymous memory, the idle.rss
	// metric: RssAnon on Linux, the physical footprint on macOS, total
	// RSS elsewhere. Total RSS also counts file-backed pages (the binary,
	// the index files the agent mapped or read), which come and go with
	// the page cache: one A/B run showed +12% idle RSS that was no change
	// in the agent's own memory.
	IdleAnonMB  float64   `json:"idle_anon_mb"`
	SweepCPUMs  []float64 `json:"sweep_cpu_ms"` // no-change sweeps after the first
	SweepFiles  int       `json:"sweep_files"`
	LoadAverage string    `json:"load_average"`
	CorpusFiles int64     `json:"corpus_files"`
	CorpusBytes int64     `json:"corpus_bytes"`
}

func (b *bench) agentArgs(db string, extra ...string) []string {
	return b.withAgentCapabilities(append([]string{"agent", "run", "--no-sync", "--db", db, "--claude-projects", b.claude, "--codex-home", b.codex,
		"--devin-db", filepath.Join(b.scratch, "devin", "sessions.db")}, extra...))
}

func (b *bench) withAgentCapabilities(args []string) []string {
	if b.opencodeDB {
		return append(args, "--opencode-db", "-")
	}
	return args
}

// Probe outside measured launches. Go's flag help may exit nonzero; only
// recognizable agent-run help permits treating a missing flag as legacy.
func (b *bench) probeAgent(ctx context.Context) (bool, error) {
	return b.probeAgentTimeout(ctx, 10*time.Second)
}

func (b *bench) probeAgentTimeout(ctx context.Context, timeout time.Duration) (bool, error) {
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(pctx, b.exe, "agent", "run", "-h")
	cmd.Env = b.agentEnv()
	cmd.WaitDelay = 100 * time.Millisecond
	var out bytes.Buffer
	// Limit retained output even if a broken binary floods its help stream.
	cmd.Stdout = &benchHelpWriter{buf: &out}
	cmd.Stderr = cmd.Stdout
	err := cmd.Run()
	if pctx.Err() != nil {
		return false, fmt.Errorf("agent run help: %w", pctx.Err())
	}
	var exit *exec.ExitError
	if err != nil && (!errors.As(err, &exit) || exit.ExitCode() < 0) {
		return false, fmt.Errorf("agent run help: %w", err)
	}
	help := out.String()
	flags := regexp.MustCompile(`(?m)^  -([a-zA-Z0-9][a-zA-Z0-9-]*)(?:[ \t].*)?$`).FindAllStringSubmatch(help, -1)
	if !strings.Contains(help, "Usage of agent run:\n") || len(flags) == 0 ||
		strings.Contains(help, "flag provided but not defined:") || len(help) >= 64<<10 {
		return false, fmt.Errorf("agent run help: malformed output (%v)\n%s", err, tail(help, 2000))
	}
	for _, f := range flags {
		if f[1] == "opencode-db" {
			return true, nil
		}
	}
	return false, nil
}

type benchHelpWriter struct{ buf *bytes.Buffer }

func (w *benchHelpWriter) Write(p []byte) (int, error) {
	n := len(p)
	if left := (64 << 10) - w.buf.Len(); left > 0 {
		if len(p) > left {
			p = p[:left]
		}
		_, _ = w.buf.Write(p)
	}
	return n, nil
}

func (b *bench) agentEnv() []string {
	home := filepath.Join(b.scratch, "home")
	_ = os.MkdirAll(home, 0o700)
	return append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"XDG_DATA_HOME="+filepath.Join(home, ".local", "share"), "XDG_CACHE_HOME="+filepath.Join(home, ".cache"),
		"XDG_STATE_HOME="+filepath.Join(home, ".local", "state"),
		"CLAUDE_CONFIG_DIR="+filepath.Join(home, ".claude"), "CODEX_HOME="+filepath.Join(home, ".codex"),
		"FLOPWIRE_CONFIG="+filepath.Join(home, ".config", "flopwire", "config.json"),
		"FLOPWIRE_CLOUD=off", "FLOPWIRE_TOKEN=", "FLOPWIRE_SERVER=", "FLOPWIRE_FINGERPRINT=",
		"FLOPWIRE_OPENCODE_DB=-", "OPENCODE_DB=-", "FLOPWIRE_INDEX=")
}

func (b *bench) index(ctx context.Context, idleAfter time.Duration) (*indexResult, error) {
	// Devin's store is copied: the agent opens it read-only, but a copy
	// keeps the run independent of a live Devin writing to it.
	if err := os.MkdirAll(filepath.Join(b.scratch, "devin"), 0o755); err != nil {
		return nil, err
	}
	for _, sfx := range []string{"", "-wal"} {
		if err := copyPath(b.devin+sfx, filepath.Join(b.scratch, "devin", "sessions.db"+sfx)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	dir := filepath.Dir(b.indexPath())
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	r := &indexResult{At: time.Now(), LoadAverage: loadAverage()}
	cs := corpusSize(b.claude, b.codex, b.devin)
	r.CorpusFiles, r.CorpusBytes = cs.Files, cs.Bytes
	fmt.Fprintln(os.Stderr, "bench: indexing the full corpus into", dir)
	cmd := exec.CommandContext(ctx, b.exe, b.agentArgs(b.indexPath(), "--once")...)
	cmd.Env = b.agentEnv()
	var logBuf bytes.Buffer
	cmd.Stderr = &logBuf
	start := time.Now()
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("agent --once: %w\n%s", err, tail(logBuf.String(), 2000))
	}
	r.WallS = time.Since(start).Seconds()
	if ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
		r.PeakRSSMB = maxRSSMB(ru)
		r.CPUS = time.Duration(ru.Utime.Nano() + ru.Stime.Nano()).Seconds()
	}
	for _, l := range strings.Split(logBuf.String(), "\n") {
		if strings.Contains(l, "pass done") {
			r.Log = l
		}
	}
	r.IndexMB = float64(dirSize(dir)) / 1e6
	r.Rows = countRows(ctx, b.indexPath())

	// A fresh process on the built index stands in for the agent after
	// its post-bulk re-exec: same binary, same index, empty heap.
	fmt.Fprintf(os.Stderr, "bench: idling the agent for %s\n", idleAfter)
	rctx, cancel := context.WithTimeout(ctx, idleAfter+30*time.Second)
	defer cancel()
	cmd = exec.CommandContext(rctx, b.exe, b.agentArgs(b.indexPath(), "--sweep", "5s", "-v", "--socket", filepath.Join(b.scratch, "agent.sock"))...)
	cmd.Env = b.agentEnv()
	pipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	sweeps := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(pipe)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "agent: sweep") {
				select {
				case sweeps <- sc.Text():
				default:
				}
			}
		}
	}()
	select {
	case <-time.After(idleAfter):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	pid := cmd.Process.Pid
	r.IdleRSSMB = psRSSMB(pid)
	r.IdleFootMB = footprintMB(pid)
	r.IdleAnonMB = anonMB(pid, r.IdleFootMB, r.IdleRSSMB)
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
	close(sweeps)
	cpuRe := regexp.MustCompile(`cpu=([0-9.]+)(µs|ms|s)`)
	filesRe := regexp.MustCompile(`files=([0-9]+)`)
	n := 0
	for l := range sweeps {
		if n++; n == 1 {
			continue // the startup sweep also loads state
		}
		if m := cpuRe.FindStringSubmatch(l); m != nil {
			d, _ := time.ParseDuration(m[1] + m[2])
			r.SweepCPUMs = append(r.SweepCPUMs, float64(d.Microseconds())/1000)
		}
		if m := filesRe.FindStringSubmatch(l); m != nil {
			r.SweepFiles, _ = strconv.Atoi(m[1])
		}
	}
	return r, nil
}

// countRows reads the row count with the sqlite3 shell (the agent has
// exited), if
// installed (0 otherwise).
func countRows(ctx context.Context, db string) int64 {
	out, err := exec.CommandContext(ctx, "sqlite3", db, "SELECT count(*) FROM messages").Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return n
}

// --- b. freshness ---

type freshResult struct {
	At      time.Time `json:"at"`
	Session string    `json:"session"`
	SizeMB  float64   `json:"size_mb"`
	Ms      []float64 `json:"ms"`
	P50     float64   `json:"p50_ms"`
	P95     float64   `json:"p95_ms"`
	Max     float64   `json:"max_ms"`
}

func (b *bench) fresh(ctx context.Context) (*freshResult, error) {
	// The newest real Claude session of 1-64MB, copied into a scratch
	// projects root; the real harness directories are never written.
	sessions, err := claude.Discover(b.claude)
	if err != nil {
		return nil, err
	}
	var pick string
	var newest time.Time
	for _, s := range sessions {
		if fi, err := os.Stat(s.Transcript); err == nil && s.Transcript != "" && fi.Size() >= 1<<20 && fi.Size() <= 64<<20 && fi.ModTime().After(newest) {
			pick, newest = s.Transcript, fi.ModTime()
		}
	}
	if pick == "" {
		return nil, errors.New("no Claude session between 1MB and 64MB")
	}
	dir := filepath.Join(b.scratch, "fresh")
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	proj := filepath.Join(dir, "projects", filepath.Base(filepath.Dir(pick)))
	if err := os.MkdirAll(proj, 0o755); err != nil {
		return nil, err
	}
	dst := filepath.Join(proj, filepath.Base(pick))
	sessionID, cwd, err := copyCompleteLines(pick, dst)
	if err != nil {
		return nil, err
	}
	fi, _ := os.Stat(dst)
	r := &freshResult{At: time.Now(), Session: sessionID, SizeMB: float64(fi.Size()) / 1e6}
	db := filepath.Join(dir, "index.db")
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	env := b.agentEnv()
	cmd := exec.CommandContext(rctx, b.exe, b.withAgentCapabilities([]string{"agent", "run", "--no-sync", "--db", db, "--claude-projects", filepath.Join(dir, "projects"),
		"--codex-home", filepath.Join(dir, "nocodex"), "--devin-db", "-", "--socket", filepath.Join(dir, "agent.sock")})...)
	cmd.Env = env
	var logBuf bytes.Buffer
	cmd.Stderr = &logBuf
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() { _ = cmd.Process.Signal(syscall.SIGTERM); _ = cmd.Wait() }()
	findable := func(needle string) (bool, error) {
		query := exec.CommandContext(ctx, b.exe, "grep", "-F", "--index", db, "--include-self", "--json", needle)
		query.Env = env
		out, err := query.Output()
		if err != nil {
			return false, nil // the index may not exist yet
		}
		var res struct{ Hits []json.RawMessage }
		if err := json.Unmarshal(out, &res); err != nil {
			return false, err
		}
		return len(res.Hits) > 0, nil
	}
	// Wait for the initial index of the copy: its last line is findable.
	if err := waitFor(ctx, 2*time.Minute, func() (bool, error) {
		if !strings.Contains(logBuf.String(), "agent: running") {
			return false, nil
		}
		return findable(sessionID[:8])
	}); err != nil {
		return nil, fmt.Errorf("initial index: %w\n%s", err, tail(logBuf.String(), 2000))
	}
	for i := range 20 {
		needle := fmt.Sprintf("flopwirefresh%d%d", time.Now().UnixNano(), i)
		line := fmt.Sprintf(`{"parentUuid":null,"isSidechain":false,"userType":"external","cwd":%q,"sessionId":%q,"version":"2.1.0","type":"user","message":{"role":"user","content":%q},"uuid":"f0000000-0000-4000-8000-%012d","timestamp":%q}`+"\n",
			cwd, sessionID, "acceptance probe "+needle, i, time.Now().UTC().Format(time.RFC3339Nano))
		f, err := os.OpenFile(dst, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return nil, err
		}
		_, err = f.WriteString(line)
		f.Close()
		if err != nil {
			return nil, err
		}
		t0 := time.Now()
		if err := waitFor(ctx, 10*time.Second, func() (bool, error) { return findable(needle) }); err != nil {
			return nil, fmt.Errorf("append %d: %w", i, err)
		}
		r.Ms = append(r.Ms, float64(time.Since(t0).Microseconds())/1000)
		// Appends at varied phases of the fast lane.
		time.Sleep(time.Duration(150+97*i%600) * time.Millisecond)
	}
	s := slices.Sorted(slices.Values(r.Ms))
	r.P50, r.P95, r.Max = pct(s, 0.5), pct(s, 0.95), s[len(s)-1]
	return r, nil
}

func waitFor(ctx context.Context, d time.Duration, cond func() (bool, error)) error {
	deadline := time.Now().Add(d)
	for {
		ok, err := cond()
		if err != nil || ok {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not done after %s", d)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// copyCompleteLines copies src's complete lines to dst and returns the
// session id and cwd of the first line that has them.
func copyCompleteLines(src, dst string) (session, cwd string, err error) {
	in, err := os.Open(src)
	if err != nil {
		return "", "", err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return "", "", err
	}
	defer out.Close()
	r := bufio.NewReaderSize(in, 1<<20)
	for {
		line, rerr := r.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			if _, err := out.Write(line); err != nil {
				return "", "", err
			}
			if session == "" {
				var rec struct {
					SessionID string `json:"sessionId"`
					Cwd       string `json:"cwd"`
				}
				if json.Unmarshal(line, &rec) == nil && rec.SessionID != "" {
					session, cwd = rec.SessionID, rec.Cwd
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	if session == "" {
		return "", "", fmt.Errorf("%s: no session id", src)
	}
	return session, cwd, nil
}

// --- d. query set ---

// querySpec is one entry of testdata/acceptance/queries.yaml.
type querySpec struct {
	Name   string `yaml:"name"`
	Source string `yaml:"source"` // the cass invocation it came from
	Verb   string `yaml:"verb"`   // find (grep -F, or grep with regex) | search
	Query  string `yaml:"query"`
	Regex  bool   `yaml:"regex"`
	Repo   string `yaml:"repo"`
	Agent  string `yaml:"agent"`
	Since  string `yaml:"since"`
	Until  string `yaml:"until"`
	Limit  int    `yaml:"limit"`
	Expect struct {
		MinHits  int      `yaml:"min_hits"`
		MaxHits  int      `yaml:"max_hits"` // 0: no bound; a filter query sets it to prove the filter drops rows
		Sessions []string `yaml:"sessions"` // each must appear among the hits (session id prefix)
	} `yaml:"expect"`
	Evidence string `yaml:"evidence"` // how the expectation was derived
}

type queryResult struct {
	Name     string    `json:"name"`
	Hits     int       `json:"hits"`
	Ms       []float64 `json:"ms"` // cold, then warm runs
	WarmMs   float64   `json:"warm_ms"`
	OK       bool      `json:"ok"`
	Problems []string  `json:"problems,omitempty"` // with stderr; terminal and scratch only
	Reasons  []string  `json:"reasons,omitempty"`  // path-free, for the record
	// Reads round-trips the printed addresses: read ADDRESS[:LINE] of up to
	// readsPerQuery hits must focus that message; ReadMs are their times.
	Reads   int       `json:"reads"`
	ReadsOK int       `json:"reads_ok"`
	ReadMs  []float64 `json:"read_ms,omitempty"`
}

// readsPerQuery bounds the address round-trips per query.
const readsPerQuery = 5

type queriesResult struct {
	At      time.Time     `json:"at"`
	Results []queryResult `json:"results"`
}

func (b *bench) queries(ctx context.Context, path, db string) (*queriesResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var specs []querySpec
	if err := yaml.Unmarshal(data, &specs); err != nil {
		return nil, err
	}
	env := b.agentEnv()
	res := &queriesResult{At: time.Now()}
	for _, q := range specs {
		verb := q.Verb
		if verb == "find" {
			verb = "grep"
		}
		args := []string{verb, "--index", db, "--include-self", "--json"}
		// Repos are written ~/Code/NAME so the fixture names no home directory.
		if rest, ok := strings.CutPrefix(q.Repo, "~/"); ok {
			q.Repo = filepath.Join(b.home, rest)
		}
		for _, kv := range [][2]string{{"repo", q.Repo}, {"agent", q.Agent}, {"since", q.Since}, {"until", q.Until}} {
			if kv[1] != "" {
				args = append(args, "--"+kv[0], kv[1])
			}
		}
		if q.Limit > 0 {
			args = append(args, "--limit", strconv.Itoa(q.Limit))
		}
		if verb == "grep" && !q.Regex {
			args = append(args, "-F")
		}
		args = append(args, "--", q.Query)
		qr := queryResult{Name: q.Name}
		var out []byte
		for range 4 {
			t0 := time.Now()
			cmd := exec.CommandContext(ctx, b.exe, args...)
			cmd.Env = env
			o, err := cmd.Output()
			qr.Ms = append(qr.Ms, float64(time.Since(t0).Microseconds())/1000)
			if err != nil {
				qr.problem("query command: "+exitReason(err), fmt.Sprintf("%v: %s", err, errText(err)))
				break
			}
			out = o
		}
		var parsed struct {
			Hits []struct {
				SessionID string `json:"session_id"`
				MessageID string `json:"message_id"`
				Address   string `json:"address"`
				Lines     []struct {
					N     int  `json:"n"`
					Match bool `json:"match"`
				} `json:"lines"`
				TextLine int `json:"text_line"`
			} `json:"hits"`
		}
		if out != nil {
			if err := json.Unmarshal(out, &parsed); err != nil {
				qr.problem("query output is not JSON", err.Error())
			}
		}
		qr.Hits = len(parsed.Hits)
		if len(qr.Ms) > 1 {
			qr.WarmMs = median(qr.Ms[1:])
		}
		if qr.Hits < q.Expect.MinHits {
			qr.problem(fmt.Sprintf("%d hits, want >= %d", qr.Hits, q.Expect.MinHits), "")
		}
		if q.Expect.MaxHits > 0 && qr.Hits > q.Expect.MaxHits {
			qr.problem(fmt.Sprintf("%d hits, want <= %d", qr.Hits, q.Expect.MaxHits), "")
		}
		for _, want := range q.Expect.Sessions {
			found := false
			for _, h := range parsed.Hits {
				found = found || strings.HasPrefix(h.SessionID, want)
			}
			if !found {
				qr.problem("missing session "+want, "")
			}
		}
		for i, h := range parsed.Hits {
			if i >= readsPerQuery {
				break
			}
			addr, line := h.Address, h.TextLine
			for _, l := range h.Lines {
				if l.Match {
					line = l.N
					break
				}
			}
			if line > 0 {
				addr += ":" + strconv.Itoa(line)
			}
			qr.Reads++
			t0 := time.Now()
			cmd := exec.CommandContext(ctx, b.exe, "read", "--index", db, "--json", addr)
			cmd.Env = env
			o, err := cmd.Output()
			qr.ReadMs = append(qr.ReadMs, float64(time.Since(t0).Microseconds())/1000)
			var cx struct {
				Focus string `json:"focus"`
				Line  int    `json:"line"`
			}
			switch {
			case err != nil:
				qr.problem("read: "+exitReason(err), fmt.Sprintf("%s: %v: %s", addr, err, errText(err)))
			case json.Unmarshal(o, &cx) != nil || cx.Focus != h.MessageID || cx.Line != line:
				qr.problem("read: wrong focus", fmt.Sprintf("%s: focus %s line %d, want %s line %d", addr, cx.Focus, cx.Line, h.MessageID, line))
			default:
				qr.ReadsOK++
			}
		}
		qr.OK = len(qr.Problems) == 0
		res.Results = append(res.Results, qr)
		fmt.Fprintf(os.Stderr, "bench: %-40s %4d hits  warm %6.1fms  reads %d/%d  %v\n", q.Name, qr.Hits, qr.WarmMs, qr.ReadsOK, qr.Reads, qr.Problems)
	}
	return res, nil
}

func errText(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return tail(string(ee.Stderr), 300)
	}
	return ""
}

// --- report ---

// Memory targets. Spec §11.2 set 300MB peak and 50MB idle; Gary relaxed
// them to 600MB and 120MB (A4), since the device agent's bulk load runs
// once per device and the idle numbers were already acceptable.
const (
	peakRSSTarget = 600
	idleRSSTarget = 120
)

// oracleReport is what the oracle sample tests write
// (internal/transcript/*/oracle_corpus_test.go).
type oracleReport struct {
	Agent       string         `json:"agent"`
	Files       int            `json:"files"`
	Convs       int            `json:"conversations"`
	Matched     int            `json:"matched"`
	Categories  map[string]int `json:"categories"`
	ParseErrors int            `json:"parse_errors"`
}

// oracleReports reads the oracle sample reports of this scratch directory.
func (b *bench) oracleReports() []oracleReport {
	var out []oracleReport
	reports, _ := filepath.Glob(filepath.Join(b.scratch, "oracle", "report-*.json"))
	for _, p := range reports {
		var o oracleReport
		if data, err := os.ReadFile(p); err == nil && json.Unmarshal(data, &o) == nil && o.Convs > 0 {
			out = append(out, o)
		}
	}
	return out
}

func (b *bench) report(r *accResults) error {
	type row struct{ check, target, measured, pass string }
	var rows []row
	yes := func(ok bool) string {
		if ok {
			return "PASS"
		}
		return "FAIL"
	}
	if x := r.Index; x != nil {
		rows = append(rows,
			row{"a. full index wall", "< 5 min", fmt.Sprintf("%s (cpu %.0fs, load %s)", (time.Duration(x.WallS) * time.Second).String(), x.CPUS, x.LoadAverage), yes(x.WallS < indexWallLimitS)},
			row{"a. full index peak RSS", "< 600MB (spec: 300MB)", fmt.Sprintf("%.0fMB (%d rows, index %.1fGB)", x.PeakRSSMB, x.Rows, x.IndexMB/1000), yes(x.PeakRSSMB < peakRSSTarget)},
			row{"a. idle agent after 60s", "< 120MB anonymous (spec: 50MB)", fmt.Sprintf("anonymous %.0fMB (RSS %.0fMB, footprint %.0fMB)", x.IdleAnonMB, x.IdleRSSMB, x.IdleFootMB), yes(x.IdleAnonMB < idleRSSTarget)})
		if len(x.SweepCPUMs) > 0 {
			s := slices.Sorted(slices.Values(x.SweepCPUMs))
			rows = append(rows, row{"a. no-change sweep CPU", "< 1s", fmt.Sprintf("median %.0fms, max %.0fms (%d sweeps, %d files)", median(s), s[len(s)-1], len(s), x.SweepFiles), yes(s[len(s)-1] < sweepCPULimitMs)})
		}
	}
	if x := r.Fresh; x != nil {
		rows = append(rows, row{"b. live line findable", "~2s", fmt.Sprintf("p50 %.0fms, p95 %.0fms, max %.0fms (n=%d, CLI find polls)", x.P50, x.P95, x.Max, len(x.Ms)), yes(x.P95 < freshP95LimitMs)})
	}
	for _, o := range b.oracleReports() {
		label := "c. FAD 0.3.1 parity sample: " + o.Agent
		if a, ok := strings.CutPrefix(o.Agent, "agentsview-"); ok {
			label = "c. agentsview parity sample: " + a
		}
		rows = append(rows, row{label, "match or documented",
			fmt.Sprintf("%d/%d conversations match (%.0f%%), %d files, %d parse errors", o.Matched, o.Convs, 100*float64(o.Matched)/float64(o.Convs), o.Files, o.ParseErrors),
			yes(o.ParseErrors == 0)})
	}
	if x := r.Queries; x != nil {
		ok, fast, reads, readsOK := 0, 0, 0, 0
		var warm, cold, readMs []float64
		for _, q := range x.Results {
			reads, readsOK = reads+q.Reads, readsOK+q.ReadsOK
			readMs = append(readMs, q.ReadMs...)
			if q.OK {
				ok++
			}
			if q.WarmMs < queryWarmLimitMs {
				fast++
			}
			warm = append(warm, q.WarmMs)
			if len(q.Ms) > 0 {
				cold = append(cold, q.Ms[0])
			}
		}
		slices.Sort(cold)
		sort.Float64s(warm)
		rows = append(rows,
			row{"d. query set expected hits", "all", fmt.Sprintf("%d/%d", ok, len(x.Results)), yes(ok == len(x.Results))},
			row{"d. query set latency (warm, CLI)", "< 200ms each", fmt.Sprintf("%d/%d under; median %.0fms, max %.0fms (first run: median %.0fms, max %.0fms)", fast, len(x.Results), median(warm), warm[len(warm)-1], median(cold), cold[len(cold)-1]), yes(fast == len(x.Results))})
		if len(readMs) > 0 {
			sort.Float64s(readMs)
			rows = append(rows, row{"d. hit addresses round-trip through read", "all", fmt.Sprintf("%d/%d; read median %.0fms, max %.0fms", readsOK, reads, median(readMs), readMs[len(readMs)-1]), yes(readsOK == reads)})
		}
	}
	w := [3]int{5, 6, 8}
	for _, r := range rows {
		w[0], w[1], w[2] = max(w[0], len(r.check)), max(w[1], len(r.target)), max(w[2], len(r.measured))
	}
	fmt.Printf("%-*s  %-*s  %-*s  %s\n", w[0], "check", w[1], "target", w[2], "measured", "result")
	for _, r := range rows {
		fmt.Printf("%-*s  %-*s  %-*s  %s\n", w[0], r.check, w[1], r.target, w[2], r.measured, r.pass)
	}
	return nil
}

// --- helpers ---

func pct(sorted []float64, q float64) float64 {
	return sorted[int(q*float64(len(sorted)-1)+0.5)]
}

func median(v []float64) float64 {
	s := slices.Sorted(slices.Values(v))
	return pct(s, 0.5)
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

func copyPath(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func dirSize(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

func psRSSMB(pid int) float64 {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	kb, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return kb / 1024
}

// anonMB is a process's anonymous resident memory: RssAnon from
// /proc/<pid>/status on Linux, else the macOS footprint (foot), else total
// RSS (rss) when neither is available.
func anonMB(pid int, foot, rss float64) float64 {
	if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid)); err == nil {
		if mb, ok := procStatusMB(data, "RssAnon"); ok {
			return mb
		}
	}
	if foot > 0 {
		return foot
	}
	return rss
}

// procStatusMB reads a "Key:   1234 kB" line of /proc/<pid>/status.
func procStatusMB(status []byte, key string) (float64, bool) {
	for _, l := range strings.Split(string(status), "\n") {
		v, ok := strings.CutPrefix(l, key+":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) != 2 || f[1] != "kB" {
			return 0, false
		}
		kb, err := strconv.ParseFloat(f[0], 64)
		return kb / 1024, err == nil
	}
	return 0, false
}

// footprintMB reads macOS's physical footprint (what Activity Monitor
// shows as Memory); 0 elsewhere.
func footprintMB(pid int) float64 {
	out, err := exec.Command("footprint", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	m := regexp.MustCompile(`Footprint: ([0-9.]+) (KB|MB|GB)`).FindSubmatch(out)
	if m == nil {
		return 0
	}
	v, _ := strconv.ParseFloat(string(m[1]), 64)
	switch string(m[2]) {
	case "KB":
		v /= 1024
	case "GB":
		v *= 1024
	}
	return v
}

// maxRSSMB converts ru_maxrss: bytes on macOS, KB on Linux.
func maxRSSMB(ru *syscall.Rusage) float64 {
	if runtime.GOOS == "darwin" {
		return float64(ru.Maxrss) / (1 << 20)
	}
	return float64(ru.Maxrss) / 1024
}

func loadAverage() string {
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(data)); len(f) >= 3 {
			return strings.Join(f[:3], " ")
		}
	}
	out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return ""
	}
	return strings.Trim(strings.TrimSpace(string(out)), "{} ")
}

// problem records why a query failed. reason is short and path-free; it
// goes into the committed acceptance record (docs/perf/). detail (stderr,
// addresses) is printed to the terminal and kept in the scratch results only.
func (q *queryResult) problem(reason, detail string) {
	q.Reasons = append(q.Reasons, reason)
	if detail != "" {
		reason += ": " + detail
	}
	q.Problems = append(q.Problems, reason)
}

// exitReason names how a subprocess failed without its stderr or paths.
func exitReason(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ProcessState.String() // "exit status 1", "signal: killed"
	}
	return "failed to start"
}
