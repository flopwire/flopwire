// Package agent is the device agent (spec §6.1-6.5 of
// notes/local-search/README.md): one process per device that finds every
// Claude Code, Codex and Devin transcript, keeps the local index
// (internal/localindex) current, and, when a server is configured, hands
// changed sources to devicesync for upload.
//
// Change detection has three lanes, cheapest first:
//
//   - The sweep (every 30-60s) lists every transcript directory and stats
//     every tracked file. A file is parsed only when its gate tuple (file
//     identity, size, ctime) moved or is racy; transcript.Decide then
//     chooses append, rewrite or nothing. The sweep is the source of truth.
//   - Directory events (kqueue on macOS, inotify on Linux) on a bounded set
//     of active directories wake the agent when a file is created. Files
//     are never watched one by one: kqueue needs a descriptor per watched
//     file, and fsnotify's kqueue backend opens every file of a watched
//     directory.
//   - The fast lane re-stats "hot" files (changed in the last few minutes)
//     and Devin's sessions.db and -wal every 500ms, so a line a live session
//     appends is indexed within about a second without per-file watches.
//
// Hooks (Claude Stop/PostToolUse, Codex notify) call `flopwire agent flush`,
// which asks the running agent over a unix socket to index and upload one
// source at once.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/flopwire/flopwire/internal/devicebus"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/fsprobe"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/retrieval/local"
	"github.com/flopwire/flopwire/internal/sqlitemem"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/flopwire/flopwire/internal/transcript/codex"
	"github.com/flopwire/flopwire/internal/transcript/devin"
)

// Sync is the part of devicesync.Scheduler the agent drives. Every call
// returns at once; the network never blocks indexing.
type Sync interface {
	Notify(devicesync.SourceSpec)
	NotifyExportFunc(devicesync.SourceSpec, func(context.Context) ([]byte, error))
	Flush(devicesync.SourceSpec)
}

var _ Sync = (*devicesync.Scheduler)(nil)

// Config configures an Agent. Zero fields take defaults.
type Config struct {
	ClaudeProjects string // default claude.ProjectsRoot (CLAUDE_CONFIG_DIR or ~/.claude/projects)
	CodexHome      string // default codex.Home() (CODEX_HOME or ~/.codex)
	DevinDB        string // default devin.DefaultPath; "-" disables Devin

	Sweep      time.Duration // full sweep interval; default 45s
	FastLane   time.Duration // hot-file and Devin re-stat interval; default 500ms
	HotWindow  time.Duration // a file changed this recently is re-stated every fast-lane tick; default 10m
	WarmWindow time.Duration // ... this recently, every 10 ticks; default 48h
	MaxHot     int           // files re-stated per tick; default 512
	MaxWatch   int           // watched directories; default 192
	Workers    int           // parse workers; default min(GOMAXPROCS, 6)
	BatchRows  int           // messages per ApplyBatch; default 250
	// LineBudget bounds the memory of lines over 1MB being decoded at once
	// across workers (Codex rollouts have lines up to 14MB); default 16MB.
	LineBudget int64

	Sync   Sync // nil: local indexing only
	Logger *slog.Logger

	// Path rules (D18, see policy.go). UserRules is a file of user rules,
	// one per line, re-read when it changes; UserRuleList holds more user
	// rules. Unplaceable is the user's setting for sessions with no
	// placement ("local", the default, "upload" or "exclude"). AdminRules
	// fetches the server's rules and unplaceable floor, checked at start
	// and every AdminEvery (default 10m); the last answer is kept in
	// AdminRulesCache. Home is what "~" in a rule means (default the
	// user's home directory).
	UserRules       string
	UserRuleList    []string
	Unplaceable     string
	AdminRules      func(context.Context) (AdminPolicy, error)
	AdminRulesCache string
	AdminEvery      time.Duration
	Home            string
	// Withhold asks the server to delete what it holds of a session a
	// later directory moved to local or deny after some of it was
	// uploaded (the member self-delete, addressed by session: it cascades
	// to subagents and tombstones the session). nil: the copies stay and
	// the status says so. It returns nil when the server deleted it or
	// holds nothing of it; the owed deletion is retried until then
	// (placements.withhold).
	Withhold func(context.Context, localindex.Withhold) error

	// Bus is the message bus (devicebus): the agent gives it presence and
	// session lookups, runs it with Run, and answers the control socket's
	// pending, held, send, peers and inbox requests with it. nil: no
	// messaging.
	Bus *devicebus.Bus
	// Console is the web console page where the user reviews held
	// messages ("" without a server); the held notice names it.
	Console string
}

func (c *Config) defaults() {
	home, _ := os.UserHomeDir()
	if c.ClaudeProjects == "" {
		c.ClaudeProjects = claude.ProjectsRoot(os.Getenv, home)
	}
	if c.CodexHome == "" {
		c.CodexHome = codex.Home()
	}
	if c.DevinDB == "" {
		c.DevinDB = devin.DefaultPath(home)
	}
	if c.Sweep <= 0 {
		c.Sweep = 45 * time.Second
	}
	if c.FastLane <= 0 {
		c.FastLane = 500 * time.Millisecond
	}
	if c.HotWindow <= 0 {
		c.HotWindow = 10 * time.Minute
	}
	if c.WarmWindow <= 0 {
		c.WarmWindow = 48 * time.Hour
	}
	if c.MaxHot <= 0 {
		c.MaxHot = 512
	}
	if c.MaxWatch <= 0 {
		c.MaxWatch = 192
	}
	if c.Workers <= 0 {
		// Parsing is not the bottleneck of a bulk load (the index writers
		// are); each worker holds a batch and a line in memory.
		c.Workers = min(runtime.GOMAXPROCS(0), 6)
	}
	if c.LineBudget <= 0 {
		c.LineBudget = 16 << 20
	}
	if c.BatchRows <= 0 {
		c.BatchRows = 250
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.AdminEvery <= 0 {
		c.AdminEvery = 10 * time.Minute
	}
	if c.Home == "" {
		c.Home = defaultHome()
	}
}

// Agent indexes one device's transcripts. Create it with New, then call
// Run (or Once).
type Agent struct {
	cfg    Config
	store  *localindex.Store
	claude transcript.Parser
	codex  transcript.Parser
	log    *slog.Logger
	// onReads, when set (tests), takes the read sightings instead of
	// the message bus.
	onReads func([]devicebus.Read)

	mu        sync.Mutex
	targets   map[string]*target
	pass      uint64           // discovery passes
	stubbed   map[string]bool  // orphaned Claude sessions given a stub row
	companion map[string]int64 // companion sizes recorded at startup
	urgent    queue            // hook flushes, fast-lane and directory-event hits
	normal    queue            // sweep hits
	// background holds unchanged sources indexed by an older parser
	// version, re-parsed only when nothing else is queued, and only once
	// Run started (bgOn): a pass (Once) never waits for them (D16).
	background queue
	bgOn       bool
	bgWG       sync.WaitGroup  // background work outside the workers (Devin)
	wake       chan struct{}   // a job was queued
	busy       int             // jobs running
	idle       *sync.Cond      // signalled when the queue drains
	orphanSrc  int64           // pseudo source holding orphan stubs
	notified   map[string]bool // sources handed to sync since start
	devin      devinState
	ticks      int // fast-lane ticks
	// discovered is closed once the first full discovery pass merged (or
	// Run gave up before one): a hook flush waits for it, since before it
	// the tracked set holds only placeholders and lookups by session fail.
	discovered     chan struct{}
	discoveredOnce sync.Once

	// Path rules (policy.go, placement.go). pol, devinModes, places and
	// folders are guarded by mu; polMu serializes loading and applying
	// rules.
	pol        *policyView
	devinModes map[string]pathpolicy.Mode
	places     map[placeKey]placed // stored placements
	placesOK   bool                // places holds what the index stored
	folders    map[string]string   // decoded Claude project folders
	phys       map[string]string   // physicalPath, until the rules change
	polMu      sync.Mutex
	withholdMu sync.Mutex // one sendWithholds at a time
	// The recovery pass (recover.go). recoverDue and recoverAt are guarded
	// by mu; wtCache by recoverMu, held for a whole pass.
	recoverDue     bool      // a placement saved since the last pass may need one
	recoverAt      time.Time // the last pass
	recoverRunning atomic.Bool
	recoverMu      sync.Mutex
	wtCache        map[string]wtScan
	// serverCopies is the D18 notice for status; guarded by mu.
	serverCopies *ServerCopies
	polLoaded    bool
	polApplied   bool
	userStamp    fileStamp
	adminRaw     AdminPolicy
	adminAt      time.Time // last admin fetch

	stats Stats

	// Process checks and the clock for presence (presence.go); tests
	// replace them.
	pidAlive  func(pid int) bool
	procStart func(pid int) (time.Time, bool)
	procName  func(pid int) string
	// codexWriter probes a Codex writer lock (codexlock.go); tests replace it.
	codexWriter func(lock string) int
	now         func() time.Time
	rollouts    rolloutState // Codex busy or idle, by rollout
	turns       hookTurns    // busy or idle from hook events, by session
}

// Stats counts the agent's work since start.
type Stats struct {
	Sweeps, Indexed, Appends, Rewrites, Unchanged, Renames, Companions, DevinPolls atomic.Int64
	Errors                                                                         atomic.Int64
}

// New returns an agent writing to store.
func New(store *localindex.Store, cfg Config) *Agent {
	cfg.defaults()
	budget := transcript.NewLineBudget(cfg.LineBudget)
	a := &Agent{cfg: cfg, store: store, log: cfg.Logger,
		claude:  &claude.Parser{Lines: transcript.LineReaderOptions{Budget: budget}},
		codex:   &codex.Parser{LineOptions: transcript.LineReaderOptions{Budget: budget}},
		targets: map[string]*target{}, stubbed: map[string]bool{}, notified: map[string]bool{},
		wake: make(chan struct{}, 1), discovered: make(chan struct{}), pol: &policyView{},
		places: map[placeKey]placed{}, folders: map[string]string{}, phys: map[string]string{}, wtCache: map[string]wtScan{},
		pidAlive: processAlive, procStart: processStart, procName: local.ProcName, codexWriter: codexWriter, now: time.Now}
	a.idle = sync.NewCond(&a.mu)
	if cfg.DevinDB != "-" {
		a.devin.path = cfg.DevinDB
	}
	// The sync scheduler uploads what an earlier run queued as soon as it
	// runs: check it against the rules on disk and the stored placements
	// from the start (D18). Without the placements, a queued session whose
	// worktree is gone would be placed again, more weakly, from its
	// transcript.
	a.preloadPolicy()
	if err := a.loadPlaces(context.Background()); err != nil {
		a.log.Error("agent: reading stored placements; no upload until they load", "err", err)
	}
	if f, ok := cfg.Sync.(interface {
		SetFilter(func(devicesync.SourceSpec) bool)
	}); ok {
		f.SetFilter(a.allowUpload)
	}
	if b, ok := cfg.Sync.(interface {
		SetBound(func(devicesync.SourceSpec) (int64, bool))
	}); ok {
		b.SetBound(a.uploadBound)
	}
	if cfg.Bus != nil {
		cfg.Bus.SetSources(a.BusPresence, a.BusKnown)
	}
	return a
}

// Stats returns the live counters.
func (a *Agent) Stats() *Stats { return &a.stats }

// Once runs one full pass (discovery, index everything that changed, Devin)
// with the worker pool and returns when the queue is drained. The initial
// index of a device is one Once.
func (a *Agent) Once(ctx context.Context) error {
	if err := a.load(ctx); err != nil {
		return err
	}
	a.refreshAdmin(ctx)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	a.startWorkers(ctx, &wg)
	t0 := time.Now()
	err := a.sweep(ctx)
	a.WaitIdle()
	a.log.Debug("agent: parse queue drained", "wall", time.Since(t0))
	err = errors.Join(err, a.store.Sync(ctx))
	a.log.Debug("agent: index committed", "wall", time.Since(t0))
	// Sessions of deleted worktrees wait for the recovery pass; with their
	// placements settled, a second sweep indexes or offers them.
	if a.recoverAndEnforce(ctx) {
		err = errors.Join(err, a.sweep(ctx))
		a.WaitIdle()
		err = errors.Join(err, a.store.Sync(ctx))
	}
	cancel()
	a.kick()
	wg.Wait()
	return err
}

// Run indexes until ctx ends: an initial pass, then sweeps, the fast lane
// and directory events. It returns ctx.Err() on shutdown.
func (a *Agent) Run(ctx context.Context) error {
	defer a.markDiscovered() // a flush never waits on a Run that ended
	if err := a.load(ctx); err != nil {
		return err
	}
	a.refreshAdmin(ctx)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.mu.Lock()
	a.bgOn = true
	a.mu.Unlock()
	defer a.bgWG.Wait()
	a.kickWithholds(ctx) // owed from before a restart
	if a.cfg.Bus != nil {
		// Messaging runs beside indexing; a failing server only makes it
		// back off (devicebus.Run).
		a.bgWG.Add(1)
		go func() {
			defer a.bgWG.Done()
			_ = a.cfg.Bus.Run(ctx)
		}()
	}
	var wg sync.WaitGroup
	a.startWorkers(ctx, &wg)
	defer wg.Wait()
	defer a.kick()

	w := newWatcher(a.log)
	defer w.close()
	// Watch before the first pass lists anything: a file created after the
	// listing but before its directory was watched would wait for the next
	// sweep. The pass then makes more directories worth watching (session
	// directories of hot transcripts); watchNew lists those as it adds them.
	a.rewatch(w)
	if err := a.sweep(ctx); err != nil {
		a.log.Error("agent: sweep", "err", err)
	}
	a.markDiscovered() // also when the pass failed: flushes fall back to discoverDir
	if testHookAfterFirstPass != nil {
		testHookAfterFirstPass()
	}
	a.watchNew(ctx, w, "")
	recovered := make(chan struct{}, 1)
	a.maybeRecover(ctx, recovered, false)

	sweep := time.NewTicker(a.cfg.Sweep)
	defer sweep.Stop()
	admin := time.NewTicker(a.cfg.AdminEvery)
	defer admin.Stop()
	fast := time.NewTicker(a.cfg.FastLane)
	defer fast.Stop()
	// Shrink once shortly after start, not only after the first idle
	// sweep: the startup load and first pass leave caches full.
	settle := time.After(10 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-settle:
			a.shrinkIfIdle(ctx)
		case <-sweep.C:
			a.periodicSweep(ctx, w, recovered)
		case <-recovered:
			// Placements settled or changed: index and offer what waited.
			if err := a.sweep(ctx); err != nil {
				a.log.Error("agent: sweep", "err", err)
			}
		case <-fast.C:
			a.fastLane(ctx)
			// A new session in a directory already gone waits for the
			// pass: start it without waiting for the sweep.
			a.maybeRecover(ctx, recovered, true)
		case <-admin.C:
			a.bgWG.Add(1)
			go func() {
				defer a.bgWG.Done()
				a.refreshAdmin(ctx)
				a.sendWithholds(ctx)
			}()
		case ev := <-w.events:
			a.dirEvent(ctx, ev, w)
		}
	}
}

// periodicSweep is Run's sweep tick: a full pass, a listing of each
// directory it made worth watching, the recovery pass when due, and a
// memory trim when idle. Over unchanged files it stats each tracked file
// and lists each transcript directory once, and opens no file and runs no
// per-file query (TestNoChangeSweepScales).
func (a *Agent) periodicSweep(ctx context.Context, w *watcher, recovered chan<- struct{}) {
	if err := a.sweep(ctx); err != nil {
		a.log.Error("agent: sweep", "err", err)
	}
	a.watchNew(ctx, w, "")
	a.maybeRecover(ctx, recovered, false)
	a.shrinkIfIdle(ctx)
}

// load reads the gate state of every indexed source, so a restart parses
// nothing that did not change.
func (a *Agent) load(ctx context.Context) error {
	srcs, err := a.store.ListSources(ctx, "", "")
	if err != nil {
		return err
	}
	if err := a.loadPlaces(ctx); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.targets) > 0 {
		return nil // already loaded
	}
	for _, st := range srcs {
		if st.Source.StorageKind == transcript.StorageSQLite {
			if st.Source.Path == a.devin.path {
				a.devin.sourceID = st.ID
				if st.Watermark != nil {
					a.devin.indexedWith = st.Source.Parser
				}
			}
			continue
		}
		if st.Watermark == nil {
			continue
		}
		t := a.targets[st.Source.Path]
		if t != nil && t.sourceID > st.ID {
			continue // an older identity at the same path
		}
		// A placeholder: the first discovery pass fills in the rest.
		a.targets[st.Source.Path] = &target{path: st.Source.Path, kind: kindTranscript, src: st.Source,
			seen: st.Watermark.Identity, seenAt: st.Watermark.SampledAt, sourceID: st.ID, indexedWith: sourceVersion(st),
			scanned: st.Watermark.Offset}
	}
	a.companion, err = a.store.CompanionSizes(ctx)
	return err
}

// sweep runs a full discovery pass, queues every file whose gate moved and
// polls Devin.
func (a *Agent) sweep(ctx context.Context) error {
	t0, cpu0 := time.Now(), cpuTime()
	a.refreshPolicy(ctx, false)
	f, err := a.discoverAll()
	if err != nil {
		return err
	}
	queued := a.merge(ctx, f, true)
	a.stats.Sweeps.Add(1)
	a.pollDevin(ctx, false, false)
	a.log.Debug("agent: sweep", "files", len(f.targets), "queued", queued, "wall", time.Since(t0), "cpu", cpuTime()-cpu0)
	return nil
}

// merge folds a discovery pass into the tracked set, stats every listed
// file and queues those whose gate moved. A full pass also retires tracked
// files it no longer lists.
func (a *Agent) merge(ctx context.Context, f *found, full bool) int {
	a.applyStubs(ctx, f)
	a.mu.Lock()
	a.pass++
	pass := a.pass
	var list []*target
	for _, nt := range f.targets {
		t := a.targets[nt.path]
		if t == nil {
			t = nt
			a.targets[nt.path] = t
		} else if t.parser == nil && t.kind == kindTranscript {
			// Loaded placeholder: take the discovered description.
			t.src, t.parser, t.root = nt.src, nt.parser, nt.root
			t.src.Path = t.path
		} else if t.kind == kindCompanion {
			t.parent, t.owner, t.role = nt.parent, nt.owner, nt.role
		}
		t.listed = pass
		list = append(list, t)
	}
	var gone []*target
	if full {
		for p, t := range a.targets {
			if t.listed != pass {
				delete(a.targets, p)
				gone = append(gone, t)
			}
		}
	}
	a.mu.Unlock()

	// Vanished files keep their rows live (see vanished); sync sees the
	// disappearance and salvages what it had not uploaded.
	for _, t := range gone {
		a.vanished(t)
	}
	// Queue oldest-modified first, so the row ids of a bulk load follow time
	// rather than path order (L7).
	type statted struct {
		t   *target
		id  transcript.Identity
		mod int64
	}
	sts := make([]statted, 0, len(list))
	for _, t := range list {
		fi, err := fsprobe.Stat(t.path)
		if err != nil {
			continue // gone; the next full pass retires it
		}
		sts = append(sts, statted{t, transcript.IdentityOf(fi), fi.ModTime().UnixNano()})
	}
	sort.SliceStable(sts, func(i, j int) bool { return sts[i].mod < sts[j].mod })
	n := 0
	now := time.Now()
	for _, s := range sts {
		if a.gateID(s.t, s.id, now, false) {
			n++
		}
	}
	if full {
		a.markDiscovered()
	}
	return n
}

// markDiscovered releases flushes waiting for the first discovery pass.
func (a *Agent) markDiscovered() {
	a.discoveredOnce.Do(func() { close(a.discovered) })
}

// waitDiscovered waits for the first discovery pass (see markDiscovered)
// or for ctx to end.
func (a *Agent) waitDiscovered(ctx context.Context) error {
	select {
	case <-a.discovered:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// gate stats t and queues it when its tuple moved or is racy. urgent puts
// it at the front of the queue.
func (a *Agent) gate(t *target, now time.Time, urgent bool) bool {
	id, err := transcript.StatIdentity(t.path)
	if err != nil {
		return false // gone; the next full pass retires it
	}
	return a.gateID(t, id, now, urgent)
}

// gateID is gate with the identity already sampled.
func (a *Agent) gateID(t *target, id transcript.Identity, now time.Time, urgent bool) bool {
	a.mu.Lock()
	// Hand every source to sync once per process start (and again after
	// the path rules change), so a server configured after the index was
	// built still gets the backlog; the syncer's own watermark makes it a
	// no-op for synced sources.
	first := a.cfg.Sync != nil && !a.notified[t.path]
	if first {
		a.notified[t.path] = true
	}
	queued := a.gateLocked(t, id, now, urgent)
	a.mu.Unlock()
	if first && !a.notify(t) {
		// Not placed yet (a young transcript that names no directory):
		// hand it over at a later pass, even if the file never changes.
		a.mu.Lock()
		delete(a.notified, t.path)
		a.mu.Unlock()
	}
	return queued
}

func (a *Agent) gateLocked(t *target, id transcript.Identity, now time.Time, urgent bool) bool {
	if t.kind == kindCompanion && t.seen == (transcript.Identity{}) {
		if n, ok := a.companion[t.path]; ok && n == id.Size {
			// Recorded before this start; companions are written once.
			t.seen, t.seenAt = id, now.UnixNano()
			return false
		}
	}
	if id == t.seen && !t.racy() {
		if t.stale() {
			a.enqueueBackgroundLocked(t)
		}
		return false
	}
	if t.seen != (transcript.Identity{}) && id != t.seen {
		t.hotUntil = now.Add(a.cfg.HotWindow) // changed after we knew it
	} else if t.seen == (transcript.Identity{}) {
		// Not indexed yet: hot as it will be once indexed (seen.CTime), so
		// the rewatch right after this pass watches its session directory
		// instead of the one after the next pass.
		if until := time.Unix(0, id.CTime).Add(a.cfg.HotWindow); until.After(t.hotUntil) {
			t.hotUntil = until
		}
	}
	a.enqueueLocked(t, urgent)
	return true
}

func (a *Agent) enqueueLocked(t *target, urgent bool) {
	if t.queued {
		switch {
		case t.inBackground:
			// A change beats a background re-parse: the change is indexed
			// with a full re-parse anyway (indexTranscript).
		case !urgent:
			return
		case a.urgent.has(t):
			return
		}
		// Promote: push takes it out of the queue it is in.
	}
	t.queued, t.inBackground = true, false
	if urgent {
		a.urgent.push(t)
	} else {
		a.normal.push(t)
	}
	a.kick()
}

// enqueueBackgroundLocked queues t for a re-parse when nothing else waits.
func (a *Agent) enqueueBackgroundLocked(t *target) {
	if t.queued {
		return
	}
	t.queued, t.inBackground = true, true
	a.background.push(t)
	if a.bgOn {
		a.kick()
	}
}

func (a *Agent) kick() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

func (a *Agent) startWorkers(ctx context.Context, wg *sync.WaitGroup) {
	for range a.cfg.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.worker(ctx)
		}()
	}
}

func (a *Agent) worker(ctx context.Context) {
	for {
		a.mu.Lock()
		var t *target
		switch {
		case a.urgent.len() > 0:
			t = a.urgent.pop()
		case a.normal.len() > 0:
			t = a.normal.pop()
		case a.bgOn && a.background.len() > 0:
			t = a.background.pop()
		}
		if t != nil {
			t.queued, t.inBackground = false, false
			a.busy++
		}
		more := a.urgent.len()+a.normal.len() > 0 || a.bgOn && a.background.len() > 0
		a.mu.Unlock()
		if t == nil {
			select {
			case <-ctx.Done():
				return
			case <-a.wake:
				continue
			}
		}
		if more {
			a.kick() // wake a sibling for the rest
		}
		if err := a.index(ctx, t); err != nil && ctx.Err() == nil {
			a.stats.Errors.Add(1)
			a.log.Warn("agent: index", "path", t.path, "err", err)
		}
		a.mu.Lock()
		a.busy--
		if a.busy == 0 && a.urgent.len()+a.normal.len() == 0 {
			a.idle.Broadcast()
		}
		a.mu.Unlock()
	}
}

// WaitIdle blocks until the queue is empty and no job runs.
func (a *Agent) WaitIdle() {
	a.mu.Lock()
	for a.busy > 0 || a.urgent.len()+a.normal.len() > 0 {
		a.idle.Wait()
	}
	a.mu.Unlock()
}

func (a *Agent) index(ctx context.Context, t *target) error {
	if t.kind == kindCompanion {
		return a.indexCompanion(ctx, t)
	}
	_, err := a.indexTranscript(ctx, t)
	return err
}

// fastLane re-stats files likely to be live, and Devin's store. Hot files
// (changed within HotWindow, or flagged by a hook or a directory event)
// are re-stated every tick; warm files (changed within WarmWindow) every
// warmEvery ticks; the rest wait for the sweep. The change time comes from
// the last stat, so a session that goes quiet cools down by itself.
func (a *Agent) fastLane(ctx context.Context) {
	now := time.Now()
	a.ticks++
	warm := a.ticks%warmEvery == 0
	hotSince := now.Add(-a.cfg.HotWindow).UnixNano()
	warmSince := now.Add(-a.cfg.WarmWindow).UnixNano()
	a.mu.Lock()
	var hot []*target
	for _, t := range a.targets {
		// Companions are written once; their arrival is a directory event
		// or a sweep hit. Re-stating them would only crowd live transcripts
		// out of the MaxHot budget after many were written at once.
		if t.kind != kindTranscript {
			continue
		}
		if t.hotUntil.After(now) || t.seen.CTime > hotSince || warm && t.seen.CTime > warmSince {
			hot = append(hot, t)
		}
	}
	a.mu.Unlock()
	if len(hot) > a.cfg.MaxHot {
		sort.Slice(hot, func(i, j int) bool { return hot[i].seen.CTime > hot[j].seen.CTime })
		hot = hot[:a.cfg.MaxHot]
	}
	for _, t := range hot {
		a.gate(t, now, true)
	}
	a.pollDevin(ctx, false, false)
}

// warmEvery is how many fast-lane ticks pass between re-stats of warm files.
const warmEvery = 10

// dirEvent handles a watcher event: a directory whose entries changed
// (kqueue), or a file created or written in a watched directory (inotify).
// A write to a file the agent does not track is ignored: its creation was
// an event already, and rescanning a project on every write of some other
// file would cost a discovery per write.
func (a *Agent) dirEvent(ctx context.Context, ev watchEvent, w *watcher) {
	p := ev.path
	a.mu.Lock()
	t := a.targets[p]
	a.mu.Unlock()
	if t != nil {
		a.gate(t, time.Now(), true)
		return
	}
	if ev.modify {
		return
	}
	if fi, err := fsprobe.Stat(p); err == nil && !fi.IsDir() {
		p = filepath.Dir(p)
	}
	a.scanDir(ctx, p, w)
}

// scanDir lists what a directory may have gained and gates it.
func (a *Agent) scanDir(ctx context.Context, dir string, w *watcher) {
	if a.devin.path != "" && dir == filepath.Dir(a.devin.path) {
		a.pollDevin(ctx, false, false)
		return
	}
	f, ok := a.discoverDir(dir)
	if !ok {
		return
	}
	if f == nil {
		// The project list changed: a full pass, then the new project
		// directories are watched and listed below like any other.
		if err := a.sweep(ctx); err != nil {
			a.log.Error("agent: sweep", "err", err)
		}
		if testHookAfterRootSweep != nil {
			testHookAfterRootSweep()
		}
	} else {
		a.mu.Lock()
		now := time.Now()
		for _, nt := range f.targets {
			if a.targets[nt.path] == nil {
				nt.hotUntil = now.Add(a.cfg.HotWindow) // a new file is a live one
			}
		}
		a.mu.Unlock()
		a.mergeUrgent(ctx, f)
	}
	// New directories may need watching: a new session file, or a hot
	// session's subagents/ or tool-results/ directory, which usually appears
	// empty and gets its first file a moment later.
	a.watchNew(ctx, w, dir)
}

// watchNew rewatches, then lists each newly watched directory other than
// skip (just listed by the caller): a file created there after the last
// listing but before the watch existed raised no event, and would
// otherwise wait for the next sweep. Watch first, then list.
func (a *Agent) watchNew(ctx context.Context, w *watcher, skip string) {
	added := a.rewatch(w)
	listed := map[string]bool{}
	for _, d := range added {
		if d == skip {
			continue
		}
		// A Claude directory lists its whole project: once per project.
		key := d
		if rel, ok := under(a.cfg.ClaudeProjects, d); ok {
			key = filepath.Join(a.cfg.ClaudeProjects, firstElem(rel))
		}
		if listed[key] {
			continue
		}
		listed[key] = true
		if f, ok := a.discoverDir(d); ok && f != nil {
			a.mergeUrgent(ctx, f)
		}
	}
}

// testHookAfterRootSweep runs between scanDir's full pass and its rewatch.
var testHookAfterRootSweep func()

// testHookAfterFirstPass runs between Run's first pass and its watchNew.
var testHookAfterFirstPass func()

// mergeUrgent is merge for a partial pass: no retirement, urgent queue.
func (a *Agent) mergeUrgent(ctx context.Context, f *found) {
	a.applyStubs(ctx, f)
	a.mu.Lock()
	var list []*target
	for _, nt := range f.targets {
		t := a.targets[nt.path]
		if t == nil {
			t = nt
			a.targets[nt.path] = t
		}
		list = append(list, t)
	}
	a.mu.Unlock()
	now := time.Now()
	for _, t := range list {
		a.gate(t, now, true)
	}
}

// FlushPath indexes the source at path (or the transcript whose session
// key is session) now, then asks sync to upload it without the debounce.
// Hooks reach it through the control socket.
//
// A flush that arrives before the first discovery pass (a hook firing
// while the agent starts) waits for it: until then a session id resolves
// to nothing, and a partial merge would make load skip the stored gates.
func (a *Agent) FlushPath(ctx context.Context, path, session string) (string, error) {
	if err := a.waitDiscovered(ctx); err != nil {
		return "", err
	}
	t := a.lookup(path, session)
	if t == nil && path != "" {
		// Unknown yet: a file created since the last pass.
		if f, ok := a.discoverDir(filepath.Dir(path)); ok && f != nil {
			a.mergeUrgent(ctx, f)
		}
		t = a.lookup(path, session)
	}
	if t == nil {
		return "", fmt.Errorf("agent: no tracked transcript for path %q session %q", path, session)
	}
	a.mu.Lock()
	t.hotUntil = time.Now().Add(a.cfg.HotWindow)
	a.mu.Unlock()
	if err := a.index(ctx, t); err != nil {
		return t.path, err
	}
	if err := a.store.Sync(ctx); err != nil { // findable when the hook returns
		return t.path, err
	}
	if a.cfg.Sync != nil && a.uploadable(t) {
		a.cfg.Sync.Flush(a.specOf(t))
	}
	return t.path, nil
}

func (a *Agent) lookup(path, session string) *target {
	a.mu.Lock()
	defer a.mu.Unlock()
	if path != "" {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		if t := a.targets[path]; t != nil {
			return t
		}
		if r, err := fsprobe.EvalSymlinks(path); err == nil {
			if t := a.targets[r]; t != nil {
				return t
			}
		}
	}
	if session != "" {
		for _, t := range a.targets {
			if t.kind == kindTranscript && t.src.SessionKey == session {
				return t
			}
		}
	}
	return nil
}

// vanished handles a tracked file that disappeared. Its rows stay live:
// the dominant cause is Claude's retention cleanup (cleanupPeriodDays), and
// the index exists so that history outlives the harness's copy. A file
// that moved is picked up at its new path by identity (indexTranscript).
func (a *Agent) vanished(t *target) {
	a.notify(t) // the syncer salvages and forgets it
}

// refreshAdmin re-reads the path rules, fetching the server's when a
// fetch is due (at most once a minute).
func (a *Agent) refreshAdmin(ctx context.Context) {
	a.polMu.Lock()
	due := a.cfg.AdminRules != nil && time.Since(a.adminAt) >= time.Minute
	if due {
		a.adminAt = time.Now()
	}
	a.polMu.Unlock()
	a.refreshPolicy(ctx, due)
}

// shrinkIfIdle hands caches back when nothing is queued: an idle agent
// should hold little memory (spec §11.2: under 50MB).
func (a *Agent) shrinkIfIdle(ctx context.Context) {
	a.mu.Lock()
	idle := a.busy == 0 && a.urgent.len()+a.normal.len()+a.background.len() == 0
	a.mu.Unlock()
	if !idle {
		return
	}
	if err := a.store.ShrinkMemory(ctx); err != nil && ctx.Err() == nil {
		a.log.Debug("agent: shrink memory", "err", err)
	}
	sqlitemem.Trim()
	debug.FreeOSMemory()
}

// ResetGates forgets what every tracked file was last seen as, so the next
// sweep checks each against the watermarks in the index. The store calls it
// (Options.OnCommitError) when a deferred commit failed: the index lost
// work the gates believe is done.
func (a *Agent) ResetGates() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, t := range a.targets {
		t.seen, t.seenAt = transcript.Identity{}, 0
	}
	a.devin.db, a.devin.wal = transcript.Identity{}, transcript.Identity{}
	a.log.Error("agent: index commit failed; re-checking every source at the next sweep")
}

func cpuTime() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
