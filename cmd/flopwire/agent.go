package main

// `flopwire agent run` is the device agent (internal/agent); `flopwire agent
// flush` is what harness hooks call. See docs/agent.md.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/devicebus"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/retrieval/local"
	"github.com/flopwire/flopwire/internal/sqlitemem"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/flopwire/flopwire/internal/transcript/codex"
	"github.com/flopwire/flopwire/internal/vendorcloud"
)

func agentCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: flopwire agent run|flush|status [flags]")
	}
	switch args[0] {
	case "run":
		return agentRun(ctx, args[1:])
	case "flush":
		return agentFlush(ctx, args[1:], os.Stdin)
	case "status":
		fs := flag.NewFlagSet("agent status", flag.ContinueOnError)
		asJSON := fs.Bool("json", false, "print JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("usage: flopwire agent status [--json]")
		}
		return agentStatusOutput(ctx, os.Stdout, *asJSON)
	default:
		return fmt.Errorf("unknown agent command %q (want run, flush or status)", args[0])
	}
}

// indexPath is the local index: $FLOPWIRE_INDEX, else
// <os.UserCacheDir()>/flopwire/index.db. The local query verbs (A3,
// internal/retrieval/local.IndexPath) use the same rule.
func indexPath() (string, error) {
	if p := os.Getenv("FLOPWIRE_INDEX"); p != "" {
		return p, nil
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "flopwire", "index.db"), nil
}

// configDir is the directory of the client config; the index, the sync
// spool and the control socket live beside it.
func configDir() (string, error) {
	p, err := client.Path()
	if err != nil {
		return "", err
	}
	return filepath.Dir(p), nil
}

// pathRuleFiles are the user's path rules file and the admin rules cache
// in the config directory dir.
func pathRuleFiles(dir string) (user, adminCache string) {
	return filepath.Join(dir, "path-rules"), filepath.Join(dir, "admin-path-rules.json")
}

// reexecAfter: an initial pass that indexed at least this many sources was
// a bulk load. The agent then re-executes itself, so the long-running
// process starts from a small heap: neither Go (on macOS) nor SQLite's
// allocator hands a bulk load's peak back to the system promptly.
const reexecAfter = 200

const envReexec = "FLOPWIRE_AGENT_REEXECED"

// envLockFD passes the index lock across the re-exec: the descriptor stays
// open (and locked) in the new process, so no other writer can take the
// index in between (decision D12).
const envLockFD = "FLOPWIRE_AGENT_LOCK_FD"

func agentRun(ctx context.Context, args []string) error {
	lock, err := runAgent(ctx, args)
	if err != nil || lock == nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := keepAcrossExec(lock); err != nil {
		return err
	}
	env := append(os.Environ(), envReexec+"=1", fmt.Sprintf("%s=%d", envLockFD, lock.Fd()))
	return syscall.Exec(exe, append([]string{os.Args[0]}, os.Args[1:]...), env)
}

// keepAcrossExec clears close-on-exec on f.
func keepAcrossExec(f *os.File) error {
	_, err := unix.FcntlInt(f.Fd(), unix.F_SETFD, 0)
	return err
}

// inheritedLock is the index lock a re-executing agent passed down, if any.
func inheritedLock(dbPath string) *os.File {
	v := os.Getenv(envLockFD)
	os.Unsetenv(envLockFD)
	fd, err := strconv.Atoi(v)
	if v == "" || err != nil {
		return nil
	}
	// Close-on-exec was cleared to pass the lock down; set it again so a
	// child process never inherits the lock and holds the index after the
	// agent dies.
	unix.CloseOnExec(fd)
	return os.NewFile(uintptr(fd), localindex.LockPath(dbPath))
}

// runAgent runs the agent. It returns the held index lock when the agent
// should re-execute itself (after a bulk load).
func runAgent(ctx context.Context, args []string) (reexecLock *os.File, err error) {
	fs := flag.NewFlagSet("agent run", flag.ContinueOnError)
	dbPath := fs.String("db", "", "local index database (default $FLOPWIRE_INDEX, else <user cache dir>/flopwire/index.db)")
	once := fs.Bool("once", false, "index everything that changed, upload it (with a configured server or FLOPWIRE_TOKEN), then exit")
	syncWait := fs.Duration("sync-timeout", 5*time.Minute, "with --once: how long to wait for the upload to finish")
	noSync := fs.Bool("no-sync", false, "never upload, even with a configured server")
	rebuildIndex := fs.Bool("rebuild-index", false, "drop the local index's rows and search files and index every transcript again; keeps sync state, placements and local redactions (see docs/agent.md#recover-the-local-index)")
	syncOnly := fs.Bool("sync-only", false, `upload only: keep no local message index (local grep/search/read then need --server); default from the client config's "mode"`)
	sweep := fs.Duration("sweep", envDuration("FLOPWIRE_SWEEP", 45*time.Second), "full sweep interval")
	workers := fs.Int("workers", 0, "parse workers (default GOMAXPROCS)")
	claudeDir := fs.String("claude-projects", "", "Claude projects root (default CLAUDE_CONFIG_DIR/projects or ~/.claude/projects)")
	codexHome := fs.String("codex-home", "", "Codex home (default CODEX_HOME or ~/.codex)")
	devinDB := fs.String("devin-db", "", `Devin sessions.db (default FLOPWIRE_DEVIN_DB or ~/.local/share/devin/cli/sessions.db; "-" disables)`)
	opencodeDB := fs.String("opencode-db", "", `opencode.db (default FLOPWIRE_OPENCODE_DB, OPENCODE_DB, or opencode.db under XDG_DATA_HOME or ~/.local/share/opencode; "-" disables)`)
	socket := fs.String("socket", "", "control socket (default <config dir>/agent.sock)")
	spoolCap := fs.Int64("spool-cap", 1<<30, "sync spool cap in bytes")
	cpuProfile := fs.String("cpuprofile", "", "write a CPU profile")
	memProfile := fs.String("memprofile", "", "write a heap profile at the heap's peak")
	memLimit := fs.Int64("mem-limit", 192<<20, "soft limit on Go memory in bytes, as GOMEMLIMIT (which overrides it); 0 disables")
	gcPercent := fs.Int("gc-percent", 30, "Go GC target percent, as GOGC (which overrides it)")
	verbose := fs.Bool("v", false, "log every sweep")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	cc, ccErr := client.Load()
	if err := resolveSyncOnly(fs, syncOnly, cc, ccErr); err != nil {
		return nil, err
	}
	if *syncOnly && *noSync {
		return nil, errors.New("--sync-only and --no-sync are opposites; pick one")
	}
	if *syncOnly && (ccErr != nil || cc.Server == "" || cc.Token == "") {
		return nil, errors.New("sync-only mode needs a server: run flopwire login and enroll first")
	}
	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)
	dir, err := configDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if *dbPath == "" {
		if *dbPath, err = indexPath(); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o700); err != nil {
		return nil, err
	}
	if *socket == "" {
		*socket = filepath.Join(dir, "agent.sock")
	}
	if *cpuProfile != "" {
		f, err := os.Create(*cpuProfile)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			return nil, err
		}
		defer pprof.StopCPUProfile()
	}
	// Bulk indexing allocates fast and the parse workers leave most cores
	// idle while the index writers work, so a tighter GC costs little wall
	// time and keeps the resident set near the live heap.
	if *memLimit > 0 && os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(*memLimit)
	}
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(*gcPercent)
	}
	if *memProfile != "" {
		go peakHeapProfile(ctx, *memProfile)
	}
	// SQLite's allocator keeps emptied regions mapped; handing them back
	// every second cut a bulk load's peak footprint by about 70MB.
	go sqlitemem.TrimEvery(ctx, time.Second)
	if p := os.Getenv("FLOPWIRE_MEMTRACE"); p != "" {
		if os.Getenv(envReexec) != "" {
			p += ".after-reexec"
		}
		enableSQLiteMemStatus()
		tctx, stop := context.WithCancel(ctx)
		defer stop()
		go memTrace(tctx, p, time.Second)
	}
	// The agent reads only source rows: a small read pool with small caches
	// keeps its footprint flat as the index grows.
	var a *agent.Agent
	opts := localindex.Options{ReadConns: 2, ReadCacheMB: 2, WriteCacheMB: 8, ShardCacheMB: 4, DeferCommit: true,
		SyncOnly: *syncOnly, LockFile: inheritedLock(*dbPath),
		OnCommitError: func(err error) {
			log.Error("agent: index commit", "err", err)
			if a != nil {
				a.ResetGates()
			}
		}}
	// A re-executed agent (it holds the lock already) rebuilt before.
	opts.RebuildIndex = *rebuildIndex && opts.LockFile == nil
	store, err := openAgentIndex(ctx, *dbPath, opts, *once, *socket, log)
	if err != nil || store == nil {
		return nil, err // store == nil: the running agent did the pass
	}
	defer store.Close()

	cfg := agent.Config{ClaudeProjects: *claudeDir, CodexHome: *codexHome, DevinDB: *devinDB, OpencodeDB: *opencodeDB, Sweep: *sweep, Workers: *workers, Logger: log}
	// Path rules (D18): the user's in <config dir>/path-rules, the client
	// config's denylist and unplaceable setting, the server's (admin)
	// cached beside them.
	cfg.UserRules, cfg.AdminRulesCache = pathRuleFiles(dir)
	if ccErr == nil {
		cfg.UserRuleList = cc.Denylist
		cfg.Unplaceable = cc.Unplaceable
		if cc.Server != "" && cc.Token != "" && !*noSync {
			cfg.AdminRules = adminRules(cc)
			cfg.Withhold = withholdSession(cc, client.Load)
		}
	}
	var startSyncRun func()
	var sched *devicesync.Scheduler
	if !*noSync {
		s, tr, start, closeSync, err := startSync(ctx, store, dir, *spoolCap, deviceDirs(*claudeDir, *codexHome), log)
		if err != nil {
			return nil, err
		}
		if s != nil {
			defer closeSync()
			sched, cfg.Sync, startSyncRun = s, s, start
			// An enrolled device rotates its credential daily; a minted
			// token (FLOPWIRE_TOKEN) never rotates.
			if !*once && !tr.env {
				go rotateLoop(ctx, tr, time.Hour, time.Now, log)
			}
		}
	}
	if !*once {
		// Messaging (devicebus): through the server when this device syncs
		// with one, else between the device's own sessions.
		var connect func() (devicebus.Server, string)
		var noDevice func() string
		if ccErr == nil && cc.Server != "" && cc.Token != "" && !*noSync {
			connect = busConnect(cc, client.Load)
			noDevice = busNoDevice(client.Load)
			cfg.Console = strings.TrimRight(cc.Server, "/") + consoleRoute
		}
		// Vendor cloud sessions (Claude cloud, Devin cloud) through the
		// vendors' CLIs installed here; FLOPWIRE_CLOUD=off turns it off.
		if b := openBus(filepath.Join(dir, "bus.db"), devicebus.Config{Connect: connect, NoDevice: noDevice, Logger: log, Cloud: vendorcloud.Default()}); b != nil {
			defer b.Close()
			cfg.Bus = b
		}
	}
	a = agent.New(store, cfg) // installs the path rules filter on sched
	if startSyncRun != nil {
		startSyncRun()
	}
	start := time.Now()
	if *once || os.Getenv(envReexec) == "" {
		err := a.Once(ctx)
		st := a.Stats()
		log.Info("agent: pass done", "wall", time.Since(start).Round(time.Millisecond), "indexed", st.Indexed.Load(),
			"appends", st.Appends.Load(), "rewrites", st.Rewrites.Load(), "unchanged", st.Unchanged.Load(),
			"companions", st.Companions.Load(), "devin_polls", st.DevinPolls.Load(), "errors", st.Errors.Load())
		if *once && err == nil && sched != nil {
			err = drainSync(ctx, sched, *syncWait, log)
		}
		if *once || err != nil {
			return nil, err
		}
		if st.Indexed.Load() >= reexecAfter {
			log.Info("agent: bulk load done; restarting to release its memory")
			return store.LockFile(), nil
		}
	}
	go func() {
		if err := a.Serve(ctx, *socket); err != nil && ctx.Err() == nil {
			log.Error("agent: control socket", "err", err)
		}
	}()
	log.Info("agent: running", "db", *dbPath, "socket", *socket, "sync", cfg.Sync != nil, "sync_only", *syncOnly)
	if err := a.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return nil, err
	}
	return nil, nil
}

// resolveSyncOnly settles the index mode: the --sync-only flag when given
// (--sync-only=false forces a full index), else the client config's
// "mode" ("full", the default, or "sync-only").
func resolveSyncOnly(fs *flag.FlagSet, syncOnly *bool, cc client.Config, ccErr error) error {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == "sync-only" })
	if set || ccErr != nil {
		return nil
	}
	switch cc.Mode {
	case "", localindex.ModeFull:
	case localindex.ModeSyncOnly:
		*syncOnly = true
	default:
		return fmt.Errorf("client config: mode %q not understood (want %q or %q)", cc.Mode, localindex.ModeFull, localindex.ModeSyncOnly)
	}
	return nil
}

// agentLockGrace is how long a daemon waits for the index lock before it
// reports another agent: longer than flopwire mcp takes to create an empty
// index under it.
const agentLockGrace = 3 * time.Second

// openAgentIndex opens the index for writing. The index takes one writer
// (decision D12): when another process holds it, a daemon fails after
// agentLockGrace, and --once asks the running agent for a pass over the
// control socket and waits for it, returning a nil store. A --once whose lock holder does not
// answer on the socket (a daemon still in its initial pass, or another
// --once) waits until it answers or releases the lock.
func openAgentIndex(ctx context.Context, dbPath string, opts localindex.Options, once bool, socket string, log *slog.Logger) (*localindex.Store, error) {
	waiting := false
	start := time.Now()
	for {
		store, err := localindex.Open(dbPath, opts)
		var locked *localindex.LockedError
		if !errors.As(err, &locked) {
			return store, err
		}
		if !once || opts.RebuildIndex {
			// flopwire mcp holds the lock for a moment while it creates an
			// empty index; an agent holds it for good.
			if time.Since(start) < agentLockGrace {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(100 * time.Millisecond):
				}
				continue
			}
			return nil, fmt.Errorf("agent already running (pid %d)", locked.PID)
		}
		_, err = agent.Call(ctx, socket, agent.Request{Op: "pass", Index: dbPath})
		if err == nil {
			log.Info("agent: the running agent did the pass", "pid", locked.PID)
			return nil, nil
		}
		if !strings.HasPrefix(err.Error(), "agent not running") {
			return nil, fmt.Errorf("agent (pid %d): %w", locked.PID, err)
		}
		if !waiting {
			waiting = true
			log.Info("agent: another process holds the index; waiting for it to answer on the control socket or exit", "pid", locked.PID, "socket", socket)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// peakHeapProfile writes a heap profile whenever the heap reaches a new
// peak, so the file ends up describing the peak.
func peakHeapProfile(ctx context.Context, path string) {
	var peak uint64
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	n := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		if n++; n%40 == 0 {
			slog.Debug("agent: memory", "sys_mb", ms.Sys>>20, "heap_sys_mb", ms.HeapSys>>20, "heap_inuse_mb", ms.HeapInuse>>20,
				"heap_released_mb", ms.HeapReleased>>20, "stack_mb", ms.StackSys>>20, "other_mb", (ms.Sys-ms.HeapSys-ms.StackSys)>>20)
		}
		if ms.HeapInuse <= peak+peak/20 {
			continue
		}
		peak = ms.HeapInuse
		if f, err := os.Create(path); err == nil {
			pprof.WriteHeapProfile(f)
			f.Close()
		}
	}
}

// adminRules fetches the server's path rules over the configured client,
// which carries the TLS pin: an unpinned client would reject a self-signed
// server, or trust a CA-signed impostor. The client config is read again
// at each fetch (savedConfig), so a pin or token saved for the same server
// while the agent runs (a re-pin, a token rotation) is used; the HTTP
// client is kept while the pin stays. This is the admin side of the
// re-pin that syncRepin does for sync: rules are fetched even when sync
// has nothing to upload and so never stops on the old pin.
func adminRules(cc client.Config) func(context.Context) (agent.AdminPolicy, error) {
	return adminRulesFrom(cc, client.Load)
}

// withholdSession is the Config.Withhold for the configured server, with
// the token and TLS pin followed like adminRulesFrom.
func withholdSession(cc client.Config, load func() (client.Config, error)) func(context.Context, localindex.Withhold) error {
	var mu sync.Mutex
	server, _ := client.NormalizeServer(cc.Server)
	pin, hc := cc.TLSFingerprint, cc.HTTPClient()
	return func(ctx context.Context, w localindex.Withhold) error {
		mu.Lock()
		cur := cc
		if c, same := savedConfig(server, load); same {
			cur = c
		}
		if cur.TLSFingerprint != pin {
			pin, hc = cur.TLSFingerprint, cur.HTTPClient()
		}
		send := agent.WithholdSession(cur.Server, cur.Token, hc)
		mu.Unlock()
		return send(ctx, w)
	}
}

// openBus opens the local message inbox. One that cannot be opened (a
// damaged file, a full disk) turns messaging off with an error in the
// log; it never stops indexing and upload.
func openBus(path string, cfg devicebus.Config) *devicebus.Bus {
	b, err := devicebus.Open(path, cfg)
	if err != nil {
		if cfg.Logger != nil {
			cfg.Logger.Error("agent: messaging off: the local inbox cannot be opened", "path", path, "err", err)
		}
		return nil
	}
	return b
}

// busConnect is devicebus's Connect for the configured server: the saved
// token and TLS pin are followed as adminRulesFrom follows them, and the
// key changes when either does, so a bus stopped by a refused credential
// or pin resumes after `flopwire login`. A call refused with 401 re-reads
// the config under its lock and retries once when the token changed: the
// agent's own rotation refuses the old token at its commit, before the
// config save that names the new one (issue #110), as syncTransport does.
func busConnect(cc client.Config, load func() (client.Config, error)) func() (devicebus.Server, string) {
	var mu sync.Mutex
	server, _ := client.NormalizeServer(cc.Server)
	pin, hc := cc.TLSFingerprint, cc.HTTPClient()
	current := func() (client.Bus, string) {
		mu.Lock()
		defer mu.Unlock()
		cur := cc
		if c, same := savedConfig(server, load); same {
			cur = c
		}
		if cur.TLSFingerprint != pin {
			pin, hc = cur.TLSFingerprint, cur.HTTPClient()
		}
		sum := sha256.Sum256([]byte(cur.Token + "\x00" + cur.TLSFingerprint))
		return client.Bus{Server: cur.Server, Token: cur.Token, HTTP: hc}, hex.EncodeToString(sum[:8])
	}
	return func() (devicebus.Server, string) {
		b, key := current()
		return rotatingBus{bus: b, saved: func(ctx context.Context) (client.Bus, bool) {
			var next client.Bus
			if err := client.WithConfigLock(ctx, func() error { next, _ = current(); return nil }); err != nil {
				return b, false
			}
			return next, next.Token != b.Token
		}}, key
	}
}

// busNoDevice is devicebus's NoDevice: the server takes the bus only from
// an enrolled device credential, so a minted FLOPWIRE_TOKEN or a login
// from before device credentials (no device id) would have every poll
// refused. A config that cannot be read says nothing: Connect keeps the
// last one.
func busNoDevice(load func() (client.Config, error)) func() string {
	return func() string {
		cc, err := load()
		switch {
		case err != nil || cc.Token == "":
			return ""
		case cc.FromEnv:
			return "messaging needs an enrolled device credential, and FLOPWIRE_TOKEN is a minted token: unset FLOPWIRE_TOKEN and run flopwire login"
		case cc.DeviceID == "":
			return "messaging needs an enrolled device credential, and this login predates them: run flopwire login"
		}
		return ""
	}
}

// rotatingBus is client.Bus with one retry when a 401 raced a rotation:
// saved reads the config once the rotation holding its lock is done, and
// reports whether the token changed. The server checks the credential
// before anything else, so a refused call did nothing and a retry cannot
// send twice.
type rotatingBus struct {
	bus   client.Bus
	saved func(context.Context) (client.Bus, bool)
}

func retryRotated[Req, Resp any](ctx context.Context, r rotatingBus, req Req, call func(client.Bus, context.Context, Req) (Resp, error)) (Resp, error) {
	out, err := call(r.bus, ctx, req)
	if _, denied := unauthorized(err); !denied {
		return out, err
	}
	if next, changed := r.saved(ctx); changed {
		return call(next, ctx, req)
	}
	return out, err
}

func (r rotatingBus) Send(ctx context.Context, req busproto.SendRequest) (busproto.SendResponse, error) {
	return retryRotated(ctx, r, req, client.Bus.Send)
}

func (r rotatingBus) Poll(ctx context.Context, req busproto.PollRequest) (busproto.PollResponse, error) {
	return retryRotated(ctx, r, req, client.Bus.Poll)
}

func (r rotatingBus) Claim(ctx context.Context, req busproto.ClaimRequest) (busproto.ClaimResponse, error) {
	return retryRotated(ctx, r, req, client.Bus.Claim)
}

func (r rotatingBus) Ack(ctx context.Context, req busproto.AckRequest) (busproto.AckResponse, error) {
	return retryRotated(ctx, r, req, client.Bus.Ack)
}

func (r rotatingBus) Peers(ctx context.Context, q busproto.PeersQuery) (busproto.PeersResponse, error) {
	return retryRotated(ctx, r, q, client.Bus.Peers)
}

func (r rotatingBus) Inbox(ctx context.Context, q busproto.InboxQuery) (busproto.InboxResponse, error) {
	return retryRotated(ctx, r, q, client.Bus.Inbox)
}

func adminRulesFrom(cc client.Config, load func() (client.Config, error)) func(context.Context) (agent.AdminPolicy, error) {
	var mu sync.Mutex
	server, _ := client.NormalizeServer(cc.Server)
	pin, hc := cc.TLSFingerprint, cc.HTTPClient()
	return func(ctx context.Context) (agent.AdminPolicy, error) {
		mu.Lock()
		cur := cc
		if c, same := savedConfig(server, load); same {
			cur = c
		}
		if cur.TLSFingerprint != pin {
			pin, hc = cur.TLSFingerprint, cur.HTTPClient()
		}
		fetch := agent.FetchAdminRules(cur.Server, cur.Token, hc)
		mu.Unlock()
		return fetch(ctx)
	}
}

// savedConfig re-reads the client config: what `flopwire login` saved while
// the agent runs. same reports whether it is a usable config (with a
// token) for server (normalized). A config for another server is not
// followed: the device's state belongs to this one, so that needs a
// restart.
func savedConfig(server string, load func() (client.Config, error)) (cc client.Config, same bool) {
	cc, err := load()
	if err != nil || cc.Token == "" {
		return client.Config{}, false
	}
	s, err := client.NormalizeServer(cc.Server)
	return cc, err == nil && s == server
}

// startSync wires devicesync when this device has a server and a token.
// Its tables live in the index database, on a connection of their own.
// Sync is optional: without a config it returns nil and indexing runs
// alone; with one, a server that is down is normal and only delays upload.
// The scheduler starts uploading when start is called: after the agent
// installed its path rules filter (D18), so nothing queued by an earlier
// run uploads before the rules are checked.
// deviceDirs is what the agent reports to the server of this device's
// file system (the home "~" means, and the harness directories), resolved
// as the agent resolves them.
func deviceDirs(claudeProjects, codexHome string) syncproto.DeviceDirs {
	home, _ := os.UserHomeDir()
	if home != "" {
		home = filepath.Clean(home)
	}
	if claudeProjects == "" {
		claudeProjects = claude.ProjectsRoot(os.Getenv, home)
	}
	if codexHome == "" {
		codexHome = codex.Home()
	}
	return syncproto.DeviceDirs{Home: home, ClaudeProjects: claudeProjects, CodexHome: codexHome}
}

func startSync(ctx context.Context, store *localindex.Store, dir string, spoolCap int64, dev syncproto.DeviceDirs, log *slog.Logger) (*devicesync.Scheduler, *syncTransport, func(), func(), error) {
	cfg, err := client.Load()
	if err != nil {
		log.Info("agent: no server configured; indexing locally only", "reason", err)
		return nil, nil, nil, nil, nil
	}
	db, err := sql.Open("sqlite", "file:"+store.Path()+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, nil, nil, nil, err
	}
	db.SetMaxOpenConns(1)
	st, err := devicesync.NewStore(db)
	if err != nil {
		db.Close()
		return nil, nil, nil, nil, err
	}
	spool, err := devicesync.OpenSpool(filepath.Join(dir, "spool"), spoolCap)
	if err != nil {
		db.Close()
		return nil, nil, nil, nil, err
	}
	tr := newSyncTransport(cfg, client.Load, log)
	live := local.LiveReporter(local.NewDetector(), 30*time.Second, syncproto.MaxLive)
	sy, err := devicesync.NewSyncer(devicesync.Config{Logger: log, Device: dev, Live: live}, st, spool, tr)
	if err != nil {
		db.Close()
		return nil, nil, nil, nil, err
	}
	sched := devicesync.NewScheduler(sy, devicesync.SchedulerConfig{Repin: tr.repin})
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	start := func() {
		go func() {
			defer close(done)
			if err := sched.Run(sctx); err != nil && sctx.Err() == nil {
				log.Error("agent: sync", "err", err)
			}
		}()
	}
	return sched, tr, start, func() { cancel(); <-done; sy.Close(); db.Close() }, nil
}

// drainSync waits, after a --once pass, until the scheduler has uploaded
// everything the pass queued: a sandbox runs `flopwire agent run --once` at
// its end and exits. It fails when sync stopped (a refused credential, a
// pin mismatch) or the wait ran out.
func drainSync(ctx context.Context, sched *devicesync.Scheduler, wait time.Duration, log *slog.Logger) error {
	deadline := time.Now().Add(wait)
	for {
		st := sched.Status()
		switch {
		case st.Stopped != "":
			return fmt.Errorf("upload stopped: %s", st.Stopped)
		case st.Queued == 0:
			log.Info("agent: upload done")
			return nil
		case time.Now().After(deadline):
			return fmt.Errorf("upload not finished after %s: %d sources queued (last error: %s)", wait, st.Queued, st.LastError)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// notifyRepin tells a running agent that a server pin was saved, so a sync
// stopped by a pin mismatch resumes. A missing agent is not an error.
func notifyRepin(ctx context.Context) {
	dir, err := configDir()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, _ = agent.Call(ctx, filepath.Join(dir, "agent.sock"), agent.Request{Op: "repin"})
}

// agentFlush asks the running agent to index and upload one source now.
// The source comes from --path or --session, else from hook input: a JSON
// object on stdin (Claude Code hooks: transcript_path, session_id) or as
// the last argument (Codex notify: thread-id). A missing agent is not an
// error, so a hook never fails because the agent is stopped.
func agentFlush(ctx context.Context, args []string, stdin io.Reader) error {
	fs := flag.NewFlagSet("agent flush", flag.ContinueOnError)
	path := fs.String("path", "", "transcript path")
	session := fs.String("session", "", "session id (Codex thread id)")
	socket := fs.String("socket", "", "control socket (default <config dir>/agent.sock)")
	timeout := fs.Duration("timeout", 10*time.Second, "give up after")
	if err := fs.Parse(args); err != nil {
		return err
	}
	fromHook := *path == "" && *session == ""
	if fromHook {
		var raw []byte
		if fs.NArg() > 0 {
			raw = []byte(fs.Arg(fs.NArg() - 1))
		} else if f, ok := stdin.(*os.File); !ok || !term.IsTerminal(int(f.Fd())) {
			raw, _ = io.ReadAll(io.LimitReader(stdin, 1<<20))
		}
		*path, *session = hookSource(raw)
	}
	if *path == "" && *session == "" {
		return errors.New("agent flush: need --path, --session, or hook JSON with transcript_path or a session id")
	}
	if *socket == "" {
		dir, err := configDir()
		if err != nil {
			return err
		}
		*socket = filepath.Join(dir, "agent.sock")
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	resp, err := agent.Call(ctx, *socket, agent.Request{Op: "flush", Path: *path, Session: *session})
	// A flush from a hook only shortens latency: the agent's fast lane and
	// sweep index and upload the source anyway. So a hook never fails
	// because of the agent: not running, busy past the timeout, restarted
	// mid-call, or not tracking the file yet. An explicit --path or
	// --session still reports those errors, except a stopped agent.
	if err != nil && (fromHook || strings.HasPrefix(err.Error(), "agent not running")) {
		fmt.Fprintln(os.Stderr, "flopwire: agent flush:", err)
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Println(resp.Path)
	return nil
}

// agentStatus prints the running agent's upload state: whether the server
// is reachable, the queue and spool, and each source that keeps failing.
func agentStatus(ctx context.Context, w io.Writer) error {
	return agentStatusOutput(ctx, w, false)
}
func agentStatusOutput(ctx context.Context, w io.Writer, asJSON bool) error {
	dir, err := configDir()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := agent.Call(ctx, filepath.Join(dir, "agent.sock"), agent.Request{Op: "status"})
	if asJSON {
		if err != nil {
			return err
		}
		return json.NewEncoder(w).Encode(resp)
	}
	if err == nil {
		printAgentStatus(w, resp)
	}
	if cfg, cfgErr := client.Load(); cfgErr == nil {
		credentialStatus(w, cfg, time.Now())
	}
	return err

}

// placementOrder is how status lists placement methods.
var placementOrder = []string{localindex.PlacedByCwd, localindex.PlacedByWorktree, localindex.PlacedByFolder, localindex.PlacedByRemote,
	localindex.PlacedByBranch, localindex.PlacedByWorktreeAdd, localindex.PlacedByCommit, localindex.PlacedByNone}

// printAgentStatus renders a status answer.
func printAgentStatus(w io.Writer, resp agent.Response) {
	fmt.Fprintln(w, "agent: running")
	if resp.Extraction != nil {
		printExtractionSummary(w, resp.Extraction)
	}
	st := resp.Sync
	if c := resp.ServerCopies; c != nil {
		fmt.Fprintf(w, "path rules removed %d sessions from the local index (latest %s).\n", c.Count, c.At.Local().Format(time.DateTime))
		fmt.Fprintln(w, "  Uploaded copies stay on the server unless an admin rule covers them (the server deletes those); for the rest the owner or an admin must delete them there:")
		for _, s := range c.Sessions {
			fmt.Fprintf(w, "  %s\n", s)
		}
		if n := c.Count - len(c.Sessions); n > 0 {
			fmt.Fprintf(w, "  and %d more (see the agent log)\n", n)
		}
	}
	if len(resp.Placements) > 0 {
		fmt.Fprintln(w, "sessions placed by (for path rules):")
		for _, how := range placementOrder {
			if n := resp.Placements[how]; n > 0 {
				fmt.Fprintf(w, "  %-13s %d\n", how, n)
			}
		}
		for how, n := range resp.Placements {
			if !slices.Contains(placementOrder, how) {
				fmt.Fprintf(w, "  %-13s %d\n", how, n)
			}
		}
	}
	printBusStatus(w, resp.Bus)
	if st == nil {
		fmt.Fprintln(w, "sync: off (no server configured)")
		return
	}
	switch {
	case strings.Contains(st.Stopped, reloginPrefix):
		fmt.Fprintf(w, "sync: stopped: %s\n", st.Stopped)
	case st.Stopped != "":
		fmt.Fprintf(w, "sync: stopped until the server is re-pinned (flopwire login --fingerprint): %s\n", st.Stopped)
	case st.ServerDown:
		fmt.Fprintf(w, "sync: server unreachable, retry at %s: %s\n", st.RetryAt.Local().Format(time.TimeOnly), st.LastError)
	default:
		fmt.Fprintln(w, "sync: ok")
	}
	fmt.Fprintf(w, "queued: %d sources; spool: %d bytes", st.Queued, st.SpoolBytes)
	if st.SpoolBlocked {
		fmt.Fprint(w, " (full: captures of rewritten sources paused)")
	}
	fmt.Fprintln(w)
	printRedactions(w, st.Redactions, st.RedactedSources)
	if st.RefusedCount > 0 {
		fmt.Fprintf(w, "server refused %d sources by admin path rule (not stored there):\n", st.RefusedCount)
		for _, r := range st.Refused {
			fmt.Fprintf(w, "  %s\n    rule: %s\n", r.Path, r.Rule)
		}
		if n := st.RefusedCount - len(st.Refused); n > 0 {
			fmt.Fprintf(w, "  and %d more\n", n)
		}
	}
	if len(st.Failing) > 0 {
		fmt.Fprintf(w, "failing sources (%d), each retrying on its own:\n", len(st.Failing))
		for _, f := range st.Failing {
			fmt.Fprintf(w, "  %s\n    %s (%d attempts since %s)\n", f.Path, f.Error, f.Attempts, f.Since.Local().Format(time.DateTime))
		}
	}
}

// printBusStatus renders the message bus state (devicebus.Status).
func printBusStatus(w io.Writer, b *devicebus.Status) {
	if b == nil {
		return
	}
	switch b.State {
	case devicebus.StateLocal:
		fmt.Fprintf(w, "messaging: local (no server: between this device's sessions); %d live sessions\n", b.Sessions)
	case devicebus.StateConnected:
		fmt.Fprintf(w, "messaging: connected; %d live sessions reported\n", b.Sessions)
	case devicebus.StateBackoff:
		fmt.Fprintf(w, "messaging: server unreachable, retry at %s: %s\n", b.RetryAt.Local().Format(time.TimeOnly), b.LastError)
	case devicebus.StateStopped, devicebus.StateDisabled:
		fmt.Fprintf(w, "messaging: %s: %s\n", b.State, b.LastError)
	default:
		fmt.Fprintf(w, "messaging: %s\n", b.State)
	}
	fmt.Fprintf(w, "messages: %d pending delivery, %d receipts unsent, %d held for your acceptance\n", b.Pending, b.Unacked, b.Held)
	if b.Cloud > 0 {
		fmt.Fprintf(w, "cloud sessions: %d listed (Claude Code cloud, Devin cloud)\n", b.Cloud)
	}
	for _, h := range b.HeldSenders {
		fmt.Fprintf(w, "  held from %s: %d %s, oldest %s\n", busproto.Preview(h.User), h.Count, plural(h.Count, "message", "messages"), h.Oldest.Local().Format(time.DateTime))
	}
	if len(b.HeldSenders) > 0 {
		fmt.Fprintln(w, "  review them in the web console, or run flopwire accepts --text in a terminal")
	}
}

// printRedactions shows what the redactor masked before upload (counts
// only; notes/redaction.md).
func printRedactions(w io.Writer, counts map[string]int64, sources int64) {
	var total int64
	rules := make([]string, 0, len(counts))
	for r, n := range counts {
		total += n
		rules = append(rules, r)
	}
	if total == 0 {
		fmt.Fprintln(w, "redacted before upload: nothing so far")
		return
	}
	slices.SortFunc(rules, func(a, b string) int {
		if counts[a] != counts[b] {
			return int(counts[b] - counts[a])
		}
		return strings.Compare(a, b)
	})
	fmt.Fprintf(w, "redacted before upload: %d secrets in %d sources\n", total, sources)
	for _, r := range rules {
		fmt.Fprintf(w, "  %-24s %d\n", r, counts[r])
	}
}

// hookSource reads a transcript path or session id from hook input.
func hookSource(raw []byte) (path, session string) {
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil {
		return "", ""
	}
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := in[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	return str("transcript_path", "rollout_path"), str("session_id", "thread-id", "thread_id")
}
