package devicesync

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/flopwire/flopwire/internal/syncproto"
)

// Cadence is a trailing-edge debounce with a maximum wait: a source flushes
// Debounce after its last change, and never later than MaxWait after the
// first unflushed change.
type Cadence struct {
	Debounce, MaxWait time.Duration
}

// Notice records why a source was handed to sync. Historical admission does
// not establish a new change merely because indexing started or rules changed.
type Notice struct {
	Kind       NoticeKind
	ActivityAt time.Time
}

type NoticeKind uint8

const (
	NoticeHistorical NoticeKind = iota
	NoticeChanged
)

// SchedulerConfig tunes flush cadence and retry. Zero fields take defaults.
type SchedulerConfig struct {
	Workers           int // default one; opt-in two
	NegotiatedWorkers func(context.Context, int) (int, error)

	Append     Cadence       // append-only sources: 300ms, max 2s (spec §6.4)
	Document   Cadence       // rewritten documents and SQLite exports: 2.5s, max 10s
	BackoffMin time.Duration // first retry after a failure: 1s
	BackoffMax time.Duration // retry ceiling, for the server and for each source: 30s
	// Repin, when set, is how sync stopped by a permanent error (a TLS pin
	// mismatch) comes back without a restart: it re-reads the saved
	// server pin and, when it changed, installs it in the transport and
	// returns true. It runs on the scheduler goroutine while no flush is
	// in progress, on Recheck (`flopwire login` asks the agent for one) and
	// every RepinEvery (default 30s) while stopped.
	Repin      func() bool
	RepinEvery time.Duration
}

func (c *SchedulerConfig) defaults() {
	if c.Workers != 2 {
		c.Workers = 1
	}
	if c.Append == (Cadence{}) {
		c.Append = Cadence{300 * time.Millisecond, 2 * time.Second}
	}
	if c.Document == (Cadence{}) {
		c.Document = Cadence{2500 * time.Millisecond, 10 * time.Second}
	}
	if c.BackoffMin == 0 {
		c.BackoffMin = time.Second
	}
	if c.BackoffMax == 0 {
		c.BackoffMax = 30 * time.Second
	}
	if c.RepinEvery == 0 {
		c.RepinEvery = 30 * time.Second
	}
}

type job struct {
	spec   SourceSpec
	lane   queueLane
	hints  scheduleHints
	queued *queueEntry
	action syncAction // default captures; continuations upload the saved version
	export []byte     // latest export bytes; nil for files
	// exportFn produces the export when the flush runs (NotifyExportFunc),
	// so a queue of exports costs no memory while the server is down.
	exportFn ExportFunc
	first    time.Time
	timer    *time.Timer
}

// failure is a source whose last flush failed for a reason of its own (an
// unreadable file, a failing export, a rejected request): it backs off
// alone while the other sources carry on.
type failure struct {
	backoff  time.Duration
	retryAt  time.Time
	since    time.Time
	attempts int
	err      error
}

// Scheduler debounces change notifications into flushes, runs them on one
// workers, and retries with exponential backoff and jitter: globally while
// the server is unreachable, per source when one source fails on its own.
// A hook Flush resets the backoff. Notify and Flush never block on the
// network, so local indexing is never held up by sync.
type Scheduler struct {
	sy  *Syncer
	cfg SchedulerConfig

	mu             sync.Mutex
	waiting        map[string]*job // debouncing
	ready          map[string]*job // due now (or at retryAt)
	queue          turnQueue       // indexed eligible lanes and delayed retries
	wake           chan struct{}
	gateEpoch      uint64 // changes when newer global pressure, halt, or re-pin is installed
	backoff        time.Duration
	retryAt        time.Time // server backoff: no flush before this
	lastErr        error
	down           bool
	running        int // admitted turns, including permission acquisition
	active         map[string]*job
	workers        int
	concurrencyErr error
	negotiateAt    time.Time
	pressure       bool
	serialClamp    bool
	seal           map[string]*time.Timer
	failing        map[string]*failure // per-source backoff, by path
	halted         error               // a permanent error (TLS pin mismatch): no flush until Repin succeeds
	recheck        bool                // Recheck asked for a Repin now
	repinAt        time.Time           // while halted: the next periodic Repin
	filter         func(SourceSpec) bool
	bound          func(SourceSpec) (int64, bool)
	authorize      func(context.Context, SourceSpec) (*CaptureAuthorization, error)
}

// SetFilter installs a check run before each flush: a source it rejects
// is dropped from the queue without uploading (the device agent's path
// rules, which can change after a source was captured).
func (s *Scheduler) SetFilter(fn func(SourceSpec) bool) {
	s.mu.Lock()
	s.filter = fn
	s.mu.Unlock()
}

// SetBound installs a limit on how much of a file source a flush
// captures (Syncer.SyncUpTo): fn returns the bound, or false for none. A
// negative bound drops the source from the queue, as the filter does.
// It is asked before the filter.
func (s *Scheduler) SetBound(fn func(SourceSpec) (int64, bool)) {
	s.mu.Lock()
	s.bound = fn
	s.mu.Unlock()
}

// SetAuthorize installs a lease acquired for each flush, including retries.
// Returning nil authorization leaves ordinary collectors unchanged. Any
// returned lease is released even when acquisition also returns an error.
func (s *Scheduler) SetAuthorize(fn func(context.Context, SourceSpec) (*CaptureAuthorization, error)) {
	s.mu.Lock()
	s.authorize = fn
	s.mu.Unlock()
}

func NewScheduler(sy *Syncer, cfg SchedulerConfig) *Scheduler {
	cfg.defaults()
	workers := min(cfg.Workers, sy.cfg.UploadWorkers)
	if cfg.NegotiatedWorkers != nil {
		workers = 1
	}
	// Shared public and scheduler admission starts at the same conservative limit.
	limitErr := sy.setOperationLimit(workers)
	if limitErr != nil {
		workers = 1
		_ = sy.setOperationLimit(1)
	}
	return &Scheduler{sy: sy, cfg: cfg, waiting: map[string]*job{}, ready: map[string]*job{}, seal: map[string]*time.Timer{},
		active: map[string]*job{}, workers: workers, concurrencyErr: limitErr, failing: map[string]*failure{}, queue: newTurnQueue(), wake: make(chan struct{}, 1)}
}

// Notify admits a historical source. Use NotifyWithNotice for verified changes.
func (s *Scheduler) Notify(spec SourceSpec) { s.NotifyWithNotice(spec, Notice{}) }

func (s *Scheduler) NotifyWithNotice(spec SourceSpec, notice Notice) {
	s.notifyWithNotice(spec, nil, nil, notice)
}

// NotifyExport reports a new version of an in-memory export (SyncExport).
func (s *Scheduler) NotifyExport(spec SourceSpec, data []byte) {
	spec.Export = true
	s.notify(spec, data, nil)
}

// NotifyExportFunc is NotifyExport with the export produced by fn when the
// flush runs, not now (SyncExportFunc). A device with many changed exports
// (a first sync of Devin's store, or a long outage) then holds one export
// in memory at a time instead of all of them, and an exporter that appends
// exports only what changed since the last flush.
func (s *Scheduler) NotifyExportFunc(spec SourceSpec, fn ExportFunc) {
	s.NotifyExportFuncWithNotice(spec, fn, Notice{})
}

func (s *Scheduler) NotifyExportFuncWithNotice(spec SourceSpec, fn ExportFunc, notice Notice) {
	spec.Export = true
	s.notifyWithNotice(spec, nil, fn, notice)
}

func (s *Scheduler) notify(spec SourceSpec, data []byte, fn ExportFunc) {
	s.notifyWithNotice(spec, data, fn, Notice{})
}

func (s *Scheduler) notifyWithNotice(spec SourceSpec, data []byte, fn ExportFunc, notice Notice) {
	cad := s.cfg.Append
	if spec.rewriteProne() || spec.Export {
		cad = s.cfg.Document
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if j := s.ready[spec.Path]; j != nil {
		applyNotice(j, notice, now)
		j.action = captureSource
		j.spec, j.export, j.exportFn = spec, data, fn // already due; carry the newest bytes
		s.indexReadyLocked(spec.Path, j, now)
		return
	}
	j := s.waiting[spec.Path]
	if j == nil {
		j = &job{first: now}
		s.waiting[spec.Path] = j
	} else {
		j.timer.Stop()
	}
	applyNotice(j, notice, now)
	j.action = captureSource
	j.spec, j.export, j.exportFn = spec, data, fn
	delay := min(cad.Debounce, j.first.Add(cad.MaxWait).Sub(now))
	j.timer = time.AfterFunc(max(delay, 0), func() { s.debounced(spec.Path, j) })
}

// debounced is a debounce timer firing. A timer whose Stop came too late
// (it fired while notify or Flush held the lock) finds its job no longer
// waiting: the job is already due, or even running, and must not be
// queued a second time, where a later notify would rewrite it under the
// running flush.
func (s *Scheduler) debounced(path string, j *job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.waiting[path] == j {
		s.dueLocked(path, j)
	}
}

// Flush makes a hook-triggered source due immediately in the interactive lane.
// It retains the position of an already queued interactive job and resets
// outage and source backoff. Admission cooldowns and Retry-After still apply.
func (s *Scheduler) Flush(spec SourceSpec) {
	s.mu.Lock()
	var he *syncproto.HTTPError
	now := time.Now()
	respectDeadline := errors.As(s.lastErr, &he) && now.Before(he.RetryAt)
	busyCooldown := syncproto.Busy(s.lastErr) && now.Before(s.retryAt)
	if !busyCooldown && !respectDeadline {
		s.backoff, s.retryAt = 0, time.Time{}
	}
	if f := s.failing[spec.Path]; f != nil {
		f.backoff, f.retryAt = 0, time.Time{}
	}
	// One critical section: a job taken from waiting must not be queued
	// again after the worker picked it up (see debounced).
	defer s.mu.Unlock()
	j := s.waiting[spec.Path]
	if j == nil {
		j = s.ready[spec.Path]
	}
	if j != nil {
		if j.timer != nil {
			j.timer.Stop()
		}
		j.spec = spec
	} else {
		j = &job{spec: spec}
	}
	j.action = captureSource
	j.lane = interactiveLane
	if now.After(j.hints.ActivityAt) {
		j.hints.ActivityAt = now
	}
	if j.hints.WaitingSince.IsZero() {
		j.hints.WaitingSince = now
	}
	s.dueLocked(spec.Path, j)
}

func (s *Scheduler) due(path string, j *job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dueLocked(path, j)
}

func applyNotice(j *job, notice Notice, now time.Time) {
	if notice.Kind == NoticeChanged && j.lane < changedLane {
		j.lane = changedLane
	}
	if notice.ActivityAt.After(j.hints.ActivityAt) {
		j.hints.ActivityAt = notice.ActivityAt
	}
	if j.hints.WaitingSince.IsZero() {
		j.hints.WaitingSince = now
	}
}

func (s *Scheduler) indexReadyLocked(path string, j *job, now time.Time) {
	if s.active[path] != nil {
		s.queue.remove(j)
		return
	}
	due := now
	if f := s.failing[path]; f != nil && now.Before(f.retryAt) {
		due = f.retryAt
	}
	s.queue.insert(path, j, due, now)
}

func (s *Scheduler) dueLocked(path string, j *job) {
	if s.waiting[path] == j {
		delete(s.waiting, path)
	}
	if old := s.ready[path]; old != nil && old != j {
		mergeJobHints(j, old)
		s.queue.remove(old)
	}
	if j.hints.WaitingSince.IsZero() {
		j.hints.WaitingSince = time.Now()
	}
	s.ready[path] = j
	s.indexReadyLocked(path, j, time.Now())
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// sealLater re-checks a source once it has been quiet for SealAfter, so
// its provisional tail gets sealed. A source is re-checked only while it
// has a provisional tail (tail): once sealed and unchanged there is
// nothing left to seal, and its next change notifies it again. An export
// is re-checked with the same export function, since re-checking one
// means exporting it again (D21). Called with s.mu held.
func (s *Scheduler) sealLater(j *job, tail bool) {
	spec := j.spec
	after := s.sy.cfg.SealAfter
	if t := s.seal[spec.Path]; t != nil {
		t.Stop()
		delete(s.seal, spec.Path)
	}
	if after <= 0 || !tail || spec.Export && j.export == nil && j.exportFn == nil {
		return
	}
	export, fn := j.export, j.exportFn
	s.seal[spec.Path] = time.AfterFunc(after+time.Second, func() {
		s.mu.Lock()
		delete(s.seal, spec.Path)
		s.mu.Unlock()
		if spec.Export {
			s.notify(spec, export, fn)
		} else {
			s.Notify(spec)
		}
	})
}

// Recheck asks a stopped scheduler to re-read the saved pin now (Repin),
// and resume if it changed. It returns at once.
func (s *Scheduler) Recheck() {
	s.mu.Lock()
	s.recheck = true
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// repin runs Repin while sync is stopped, when asked or when due, and
// resumes sync when it installed a new pin. Called on the Run goroutine.
func (s *Scheduler) repin(now time.Time) { s.repinContext(context.Background(), now) }

func (s *Scheduler) repinContext(ctx context.Context, now time.Time) {
	s.mu.Lock()
	due := s.halted != nil && s.cfg.Repin != nil && (s.recheck || !now.Before(s.repinAt))
	s.recheck = false
	if due {
		s.repinAt = now.Add(s.cfg.RepinEvery)
	}
	s.mu.Unlock()
	if !due {
		return
	}
	changed := false
	if err := s.sy.withIdleOperations(ctx, func() error { changed = s.cfg.Repin(); return nil }); err != nil || !changed {
		return
	}
	s.mu.Lock()
	s.gateEpoch++
	s.halted, s.lastErr, s.down, s.backoff, s.retryAt = nil, nil, false, 0, time.Time{}
	s.serialClamp, s.pressure = false, false
	s.negotiateAt = time.Time{}
	s.mu.Unlock()
	s.sy.cfg.Logger.Info("devicesync: server pin changed; sync resumed")
}

// Status is the sync state for `flopwire agent status`.
type Status struct {
	UploadWorkers    int           `json:"upload_workers"`
	ConcurrencyError string        `json:"concurrency_error,omitempty"`
	ServerDown       bool          `json:"server_down"`
	ServerBusy       bool          `json:"server_busy,omitempty"`
	RetryAt          time.Time     `json:"retry_at,omitzero"`
	LastError        string        `json:"last_error,omitempty"` // the last server (transport) error
	Stopped          string        `json:"stopped,omitempty"`    // a permanent error: no uploads until the server is re-pinned
	Queued           int           `json:"queued"`
	SpoolBytes       int64         `json:"spool_bytes"`
	SpoolBlocked     bool          `json:"spool_blocked"` // the spool hit its cap; captures of rewritten sources are paused
	Failing          []SourceError `json:"failing,omitempty"`
	// Redactions: secrets masked before upload in every source's current
	// generation, per rule; RedactedSources: sources with any.
	Redactions      map[string]int64 `json:"redactions,omitempty"`
	RedactedSources int64            `json:"redacted_sources,omitempty"`
	// Refused lists sources the server refused by an admin path rule
	// (the agent's own rules should have kept them local); RefusedCount
	// counts them all.
	Refused      []SourceRefusal `json:"refused,omitempty"`
	RefusedCount int             `json:"refused_count,omitempty"`
}

// SourceError is a source that keeps failing on its own (an unreadable
// file, a failing export): it retries with its own backoff.
type SourceError struct {
	Path     string    `json:"path"`
	Error    string    `json:"error"`
	Attempts int       `json:"attempts"`
	Since    time.Time `json:"since"`
	RetryAt  time.Time `json:"retry_at"`
}

func (s *Scheduler) Status() Status {
	st, _ := s.StatusContext(context.Background())
	return st
}

// Progress returns live upload state without querying optional database
// diagnostics. It is suitable for waiting for uploads while capture owns the
// store connection. An empty queue does not establish discovery or search coverage.
func (s *Scheduler) Progress() Status { return s.status() }

// StatusContext returns live scheduler health even when optional redaction
// totals cannot be read. The caller must report that error as unavailable,
// rather than interpret absent totals as zero redactions.
func (s *Scheduler) StatusContext(ctx context.Context) (Status, error) {
	st := s.status()
	// Outside s.mu: the store query may wait for a capture's transaction.
	red, n, err := s.sy.store.RedactionTotals(ctx)
	if err != nil {
		return st, err
	}
	st.Redactions, st.RedactedSources = red, n
	return st, nil
}

func (s *Scheduler) status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{UploadWorkers: s.workers, ServerDown: s.down, RetryAt: s.retryAt, Queued: len(s.ready) + len(s.waiting) + s.running,
		SpoolBytes: s.sy.spool.Used(), SpoolBlocked: s.sy.spool.Blocked()}
	if s.concurrencyErr != nil {
		st.ConcurrencyError = s.concurrencyErr.Error()
	}
	st.ServerBusy = syncproto.Busy(s.lastErr) && time.Now().Before(s.retryAt)
	if s.lastErr != nil {
		st.LastError = s.lastErr.Error()
	}
	if s.halted != nil {
		st.Stopped = s.halted.Error()
	}
	for path, f := range s.failing {
		st.Failing = append(st.Failing, SourceError{Path: path, Error: f.err.Error(), Attempts: f.attempts, Since: f.since, RetryAt: f.retryAt})
	}
	slices.SortFunc(st.Failing, func(a, b SourceError) int { return strings.Compare(a.Path, b.Path) })
	st.Refused, st.RefusedCount = s.sy.Refused()
	return st
}

// next peeks eligible indexed lanes without spending scheduling credit.
// Called with s.mu held; backoff and permanent pin failure gate all lanes.
func (s *Scheduler) next(now time.Time) (*queueEntry, time.Duration) {
	if s.halted != nil {
		if s.cfg.Repin == nil {
			return nil, time.Hour
		}
		return nil, max(s.repinAt.Sub(now), time.Millisecond)
	}
	if s.pressure && s.running > 0 {
		return nil, time.Hour
	}
	if len(s.ready) == 0 {
		return nil, time.Hour
	}
	if now.Before(s.retryAt) {
		return nil, s.retryAt.Sub(now)
	}
	return s.queue.peek(now)
}

// jitter: equal jitter, half fixed and half random.
func jitter(d time.Duration) time.Duration { return d/2 + rand.N(d/2+1) }

// Run processes due flushes until ctx ends. It first queues every source
// the store still has unacknowledged data for: the watermark is the queue.
func (s *Scheduler) Run(ctx context.Context) error {
	specs, err := s.sy.store.pendingSources(ctx)
	if err != nil {
		return err
	}
	for _, sp := range specs {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.mu.Lock()
		old := &job{spec: sp.Spec, hints: sp.Hints}
		if existing := s.ready[sp.Spec.Path]; existing != nil {
			mergeJobHints(existing, old)
			s.indexReadyLocked(sp.Spec.Path, existing, time.Now())
		} else if existing := s.waiting[sp.Spec.Path]; existing != nil {
			mergeJobHints(existing, old)
		} else {
			s.dueLocked(sp.Spec.Path, old)
		}
		s.mu.Unlock()
	}
	return s.coordinate(ctx)
}

// A result is delivered only after permission callbacks and operation release.
type turnResult struct {
	ctx           context.Context
	path          string
	job           *job
	epoch         uint64
	pending       syncOutcome
	err           error
	tail, dropped bool
}

func (s *Scheduler) perform(ctx context.Context, path string, j *job, epoch uint64, filter func(SourceSpec) bool, bound func(SourceSpec) (int64, bool), authorize func(context.Context, SourceSpec) (*CaptureAuthorization, error)) turnResult {
	r := turnResult{ctx: ctx, path: path, job: j, epoch: epoch}
	upTo, bounded := boundOf(bound, j.spec)
	if filter != nil && !filter(j.spec) || bounded && upTo < 0 {
		r.dropped = true
		return r
	}
	if ctx.Err() != nil {
		r.err = ctx.Err()
		return r
	}
	r.pending, r.err = s.executeTurn(ctx, j, upTo, bounded, authorize)
	if r.err != nil {
		j.action = captureSource
	}
	r.tail = r.err == nil && r.pending == syncDone && s.sy.provisional(ctx, path)
	return r
}

func (s *Scheduler) takeLocked(now time.Time) (*queueEntry, uint64) {
	e, _ := s.next(now)
	if e == nil {
		return nil, 0
	}
	e, _ = s.queue.take(now)
	delete(s.ready, e.path)
	s.active[e.path] = e.job
	s.running = len(s.active)
	return e, s.gateEpoch
}

// coordinate owns admission and completion. At most two workers return results;
// newer same-path jobs remain coalesced but unindexed until completion.
func (s *Scheduler) coordinate(ctx context.Context) error {
	results := make(chan turnResult, 2)
	cancels := map[string]context.CancelFunc{}
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	stopping := false
	consume := func(r turnResult) {
		cancel := cancels[r.path]
		delete(cancels, r.path)
		halt := s.complete(r)
		cancel()
		if halt {
			for _, cancel := range cancels {
				cancel()
			}
		}
	}
	for {
		if ctx.Err() != nil && !stopping {
			stopping = true
			for _, cancel := range cancels {
				cancel()
			}
		}
		// Observe returned pressure before filling a newly free worker slot.
		select {
		case r := <-results:
			consume(r)
			continue
		default:
		}
		if len(cancels) == 0 {
			if stopping {
				return ctx.Err()
			}
			s.repinContext(ctx, time.Now())
			s.negotiate(ctx, time.Now())
		}
		s.mu.Lock()
		limit := s.workers
		if s.pressure {
			limit = 1
		}
		var entry *queueEntry
		var epoch uint64
		negotiationDue := s.cfg.NegotiatedWorkers != nil && s.halted == nil && !time.Now().Before(s.negotiateAt)
		if !stopping && !negotiationDue && len(cancels) < limit {
			entry, epoch = s.takeLocked(time.Now())
		}
		filter, bound, authorize := s.filter, s.bound, s.authorize
		_, wait := s.next(time.Now())
		if s.cfg.NegotiatedWorkers != nil && s.halted == nil && len(cancels) == 0 && !s.negotiateAt.IsZero() {
			wait = min(wait, max(time.Until(s.negotiateAt), time.Millisecond))
		}
		s.mu.Unlock()
		if entry != nil {
			wctx, cancel := context.WithCancel(ctx)
			cancels[entry.path] = cancel
			go func(e *queueEntry, epoch uint64) {
				results <- s.perform(wctx, e.path, e.job, epoch, filter, bound, authorize)
			}(entry, epoch)
			continue
		}
		if stopping || len(cancels) >= limit {
			wait = time.Hour
		}
		if wait <= 0 {
			wait = time.Hour
		}
		timer.Reset(wait)
		done := ctx.Done()
		if stopping {
			done = nil
		}
		select {
		case r := <-results:
			consume(r)
		case <-done:
			stopping = true
			for _, cancel := range cancels {
				cancel()
			}
		case <-s.wake:
		case <-timer.C:
		}
	}
}

// Capability publication runs only after scheduler turns and public operations
// drain. Nonpermanent failures leave ordinary serial sync available.
func (s *Scheduler) negotiate(ctx context.Context, now time.Time) {
	s.mu.Lock()
	due := s.cfg.NegotiatedWorkers != nil && s.halted == nil && !now.Before(s.negotiateAt)
	requested := s.cfg.Workers
	s.mu.Unlock()
	if !due {
		return
	}
	published := false
	publish := func(workers int, err error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.negotiateAt = now.Add(s.cfg.RepinEvery)
		s.concurrencyErr = err
		s.workers = workers
		if err == nil {
			s.serialClamp = false
			s.gateEpoch++
		} else if syncproto.Permanent(err) {
			s.gateEpoch++
			s.halted, s.lastErr = err, err
			s.repinAt = now.Add(s.cfg.RepinEvery)
		}
		published = true
	}
	err := s.sy.withIdleOperations(ctx, func() error {
		workers, err := s.cfg.NegotiatedWorkers(ctx, requested)
		if err == nil && (workers < 1 || workers > requested || workers > s.sy.cfg.UploadWorkers) {
			err = errors.New("devicesync: invalid negotiated upload worker count")
		}
		if err != nil {
			workers = 1
		}
		if limitErr := s.sy.setOperationLimit(workers); limitErr != nil {
			err = errors.Join(err, limitErr)
			workers = 1
			_ = s.sy.setOperationLimit(1)
		}
		publish(workers, err)
		return err
	})
	if !published {
		_ = s.sy.setOperationLimit(1)
		publish(1, err)
	}
}

// runOnce selects weighted serial turns until none is eligible or the
// server fails. A source failing on its own backs off alone.
func (s *Scheduler) runOnce(ctx context.Context) {
	for ctx.Err() == nil {
		s.mu.Lock()
		entry, epoch := s.takeLocked(time.Now())
		filter, bound, authorize := s.filter, s.bound, s.authorize
		s.mu.Unlock()
		if entry == nil {
			return
		}
		if s.complete(s.perform(ctx, entry.path, entry.job, epoch, filter, bound, authorize)) {
			return
		}
	}
}

func (s *Scheduler) complete(r turnResult) bool {
	ctx, path, j, dispatchEpoch, pending, err, tail := r.ctx, r.path, r.job, r.epoch, r.pending, r.err, r.tail

	s.mu.Lock()
	delete(s.active, path)
	s.running = len(s.active)
	defer func() {
		if newer := s.ready[path]; newer != nil {
			s.indexReadyLocked(path, newer, time.Now())
		}
		s.mu.Unlock()
	}()
	if r.dropped {
		delete(s.failing, path)
		return false
	}
	s.mergeNewerHintsLocked(path, j)
	if ctx.Err() != nil {
		s.restoreCancelledLocked(path, j)
		return false
	}
	if err == nil {
		s.clearPressureLocked(dispatchEpoch)
		delete(s.failing, path)
		if pending != syncDone {
			j.action = captureSource
			if pending == uploadPending {
				j.action = resumeUpload
			}
			if s.ready[path] == nil && s.waiting[path] == nil {
				s.dueLocked(path, j)
			}
		} else {
			s.sealLater(j, tail)
		}
		return false
	}
	if syncproto.Permanent(err) {
		// Retrying cannot help (a TLS pin mismatch): keep the source
		// queued and stop until the saved pin changes (Repin).
		if s.ready[path] == nil && s.waiting[path] == nil {
			s.dueLocked(path, retryJob(j))
		}
		s.gateEpoch++
		s.halted, s.lastErr = err, err
		s.repinAt = time.Now().Add(s.cfg.RepinEvery)
		s.sy.cfg.Logger.Error("devicesync: sync stopped until the server is re-pinned", "err", err)
		return true
	}
	transport := syncproto.Retryable(err) && !errors.Is(err, ErrSourceChanged) && !errors.Is(err, ErrSpoolFull)
	// A genuine error retries capture, while newer notifications keep their bytes.
	if s.ready[path] == nil && s.waiting[path] == nil {
		s.dueLocked(path, retryJob(j))
	}
	var legacy *syncproto.HTTPError
	if transport && errors.As(err, &legacy) && legacy.Body.Code == "flush_in_progress" {
		s.workers = 1
		s.serialClamp = true
		_ = s.sy.setOperationLimit(1)
	}
	if transport && dispatchEpoch != s.gateEpoch {
		return false
	}
	if transport {
		s.pressure = true
		s.gateEpoch++
		s.backoff = min(max(2*s.backoff, s.cfg.BackoffMin), s.cfg.BackoffMax)
		d := jitter(s.backoff)
		now := time.Now()
		var he *syncproto.HTTPError
		if errors.As(err, &he) {
			d = max(d, he.RetryAt.Sub(now))
		}
		busy := syncproto.Busy(err)
		s.retryAt, s.lastErr, s.down = now.Add(d), err, !busy
		s.sy.cfg.Logger.Info("devicesync: sync backing off", "server_busy", busy, "retry_in", d, "err", err)
		return false
	}
	f := s.failing[path]
	if f == nil {
		f = &failure{since: time.Now()}
		s.failing[path] = f
	}
	f.backoff = min(max(2*f.backoff, s.cfg.BackoffMin), s.cfg.BackoffMax)
	d := jitter(f.backoff)
	f.retryAt, f.err = time.Now().Add(d), err
	f.attempts++
	if queued := s.ready[path]; queued != nil {
		s.indexReadyLocked(path, queued, time.Now())
	}
	s.sy.cfg.Logger.Warn("devicesync: flush failed, will retry", "path", path, "retry_in", d, "attempts", f.attempts, "err", err)
	return false
}

// clearPressureLocked reconciles only global health, after the turn's durable
// acknowledgment. An older success must not erase a newer cooldown or stop;
// its source still receives normal completion bookkeeping. Called with s.mu held.
// This does not enable concurrent execution or replace drain-before-repin.
func (s *Scheduler) clearPressureLocked(dispatchEpoch uint64) {
	if dispatchEpoch != s.gateEpoch || s.halted != nil {
		return
	}
	s.backoff, s.down, s.lastErr = 0, false, nil
	s.pressure = false
}

// Cancellation must retain an uncaptured notification as well as durable
// pending bytes. A notification received during the operation takes precedence.
// Called with s.mu held.
func (s *Scheduler) restoreCancelledLocked(path string, j *job) {
	s.mergeNewerHintsLocked(path, j)
	if s.ready[path] == nil && s.waiting[path] == nil {
		s.dueLocked(path, j)
	}
}

func retryJob(j *job) *job {
	return &job{spec: j.spec, export: j.export, exportFn: j.exportFn, lane: j.lane, hints: j.hints}
}

// Metadata merges never replace a newer notification's capture/action payload.
func (s *Scheduler) mergeNewerHintsLocked(path string, j *job) *job {
	if newer := s.ready[path]; newer != nil {
		mergeJobHints(newer, j)
		s.indexReadyLocked(path, newer, time.Now())
		return newer
	}
	if newer := s.waiting[path]; newer != nil {
		mergeJobHints(newer, j)
		return newer
	}
	return nil
}

// executeTurn persists primary scheduling facts on an admitted path owner. Notify
// stays nonblocking and never writes SQLite or acquires capture permission.
func (s *Scheduler) executeTurn(ctx context.Context, j *job, upTo int64, bounded bool, authorize func(context.Context, SourceSpec) (*CaptureAuthorization, error)) (syncOutcome, error) {
	h, err := s.sy.store.loadScheduleHints(ctx, j.spec.Path)
	if err != nil {
		return syncDone, err
	}
	mergeJobHints(j, &job{hints: h})
	if err := s.sy.store.admitScheduleHints(ctx, j.spec.Path, j.hints); err != nil {
		return syncDone, err
	}
	outcome, err := s.syncJobTurn(ctx, j, upTo, bounded, authorize)
	if err != nil {
		return outcome, err
	}
	if err := ctx.Err(); err != nil {
		return outcome, err
	}
	now := time.Now()
	s.mu.Lock()
	newer := s.mergeNewerHintsLocked(j.spec.Path, j)
	h = j.hints
	h.WaitingSince = now
	if newer != nil {
		if newer.hints.ActivityAt.After(h.ActivityAt) {
			h.ActivityAt = newer.hints.ActivityAt
		}
	} else if outcome == syncDone {
		h.WaitingSince = time.Time{}
	}
	s.mu.Unlock()
	if err := s.sy.store.serviceScheduleHints(ctx, j.spec.Path, h); err != nil {
		return outcome, err
	}
	if err := ctx.Err(); err != nil {
		return outcome, err
	}
	// Publish successful service age only after durable bookkeeping. Failed or
	// canceled turns retain their previous waiting age and retry lineage.
	s.mu.Lock()
	newer = s.mergeNewerHintsLocked(j.spec.Path, j)
	j.hints.WaitingSince = now
	if newer != nil {
		newer.hints.WaitingSince = now
		if s.ready[j.spec.Path] == newer {
			s.indexReadyLocked(j.spec.Path, newer, now)
		}
	}
	s.mu.Unlock()
	return outcome, nil
}

func boundOf(fn func(SourceSpec) (int64, bool), spec SourceSpec) (int64, bool) {
	if fn == nil {
		return 0, false
	}
	return fn(spec)
}

func (s *Scheduler) syncJobTurn(ctx context.Context, j *job, upTo int64, bounded bool, authorize func(context.Context, SourceSpec) (*CaptureAuthorization, error)) (pending syncOutcome, result error) {
	lease, err := s.sy.acquireOperation(ctx, j.spec.Path)
	if err != nil {
		return syncDone, err
	}
	defer lease.release()
	var auth *CaptureAuthorization
	if authorize != nil {
		var err error
		auth, err = authorize(ctx, j.spec)
		if auth != nil {
			if auth.Release != nil {
				defer auth.Release()
			}
			if auth.OnError != nil {
				defer func() {
					if result != nil {
						auth.OnError(ctx, result)
					}
				}()
			}
		}
		if err != nil {
			return syncDone, err
		}
	}
	var export ExportFunc
	if j.exportFn != nil {
		export = j.exportFn
	} else if j.export != nil {
		export = func(context.Context, []byte) (Export, error) { return Export{Data: j.export}, nil }
	}
	if !bounded {
		upTo = -1
	}
	return s.sy.syncTurnWithScratch(ctx, j.spec, export, upTo, auth, j.action, lease.scratch)
}
