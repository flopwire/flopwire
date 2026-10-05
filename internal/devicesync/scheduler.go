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

// SchedulerConfig tunes flush cadence and retry. Zero fields take defaults.
type SchedulerConfig struct {
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
	export []byte // latest export bytes; nil for files
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
// worker, and retries with exponential backoff and jitter: globally while
// the server is unreachable, per source when one source fails on its own.
// A hook Flush resets the backoff. Notify and Flush never block on the
// network, so local indexing is never held up by sync.
type Scheduler struct {
	sy  *Syncer
	cfg SchedulerConfig

	mu      sync.Mutex
	waiting map[string]*job // debouncing
	ready   map[string]*job // due now (or at retryAt)
	order   []string        // ready paths in arrival order
	wake    chan struct{}
	backoff time.Duration
	retryAt time.Time // server backoff: no flush before this
	lastErr error
	down    bool
	running int // 1 while a flush is in progress
	seal    map[string]*time.Timer
	failing map[string]*failure // per-source backoff, by path
	halted  error               // a permanent error (TLS pin mismatch): no flush until Repin succeeds
	recheck bool                // Recheck asked for a Repin now
	repinAt time.Time           // while halted: the next periodic Repin
	filter  func(SourceSpec) bool
	bound   func(SourceSpec) (int64, bool)
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

func NewScheduler(sy *Syncer, cfg SchedulerConfig) *Scheduler {
	cfg.defaults()
	return &Scheduler{sy: sy, cfg: cfg, waiting: map[string]*job{}, ready: map[string]*job{}, seal: map[string]*time.Timer{},
		failing: map[string]*failure{}, wake: make(chan struct{}, 1)}
}

// Notify reports that a source changed. Call it on line completion, FS
// events, and sweep hits.
func (s *Scheduler) Notify(spec SourceSpec) { s.notify(spec, nil, nil) }

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
	spec.Export = true
	s.notify(spec, nil, fn)
}

func (s *Scheduler) notify(spec SourceSpec, data []byte, fn ExportFunc) {
	cad := s.cfg.Append
	if spec.rewriteProne() || spec.Export {
		cad = s.cfg.Document
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if j := s.ready[spec.Path]; j != nil {
		j.spec, j.export, j.exportFn = spec, data, fn // already due; carry the newest bytes
		return
	}
	j := s.waiting[spec.Path]
	if j == nil {
		j = &job{first: now}
		s.waiting[spec.Path] = j
	} else {
		j.timer.Stop()
	}
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

// Flush makes a source due immediately, skipping the debounce (hooks:
// Claude Stop / PostToolUse and the Codex equivalents), and puts it at the
// head of the queue: a backlog (a device's first sync, catch-up after an
// outage) must not delay the session the user is working in. It resets
// an outage backoff and the source's own, so a hook retries at once.
// Admission cooldowns and explicit Retry-After deadlines still apply.
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
	if j != nil {
		j.timer.Stop()
		j.spec = spec
	} else {
		j = &job{spec: spec}
	}
	s.dueLocked(spec.Path, j)
	if i := slices.Index(s.order, spec.Path); i > 0 {
		s.order = slices.Insert(slices.Delete(s.order, i, i+1), 0, spec.Path)
	}
}

func (s *Scheduler) due(path string, j *job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dueLocked(path, j)
}

func (s *Scheduler) dueLocked(path string, j *job) {
	if s.waiting[path] == j {
		delete(s.waiting, path)
	}
	if s.ready[path] == nil {
		s.order = append(s.order, path)
	}
	s.ready[path] = j
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
func (s *Scheduler) repin(now time.Time) {
	s.mu.Lock()
	due := s.halted != nil && s.cfg.Repin != nil && (s.recheck || !now.Before(s.repinAt))
	s.recheck = false
	if due {
		s.repinAt = now.Add(s.cfg.RepinEvery)
	}
	s.mu.Unlock()
	if !due || !s.cfg.Repin() {
		return
	}
	s.mu.Lock()
	s.halted, s.lastErr, s.down, s.backoff, s.retryAt = nil, nil, false, 0, time.Time{}
	s.mu.Unlock()
	s.sy.cfg.Logger.Info("devicesync: server pin changed; sync resumed")
}

// Status is the sync state for `flopwire agent status`.
type Status struct {
	ServerDown   bool          `json:"server_down"`
	ServerBusy   bool          `json:"server_busy,omitempty"`
	RetryAt      time.Time     `json:"retry_at,omitzero"`
	LastError    string        `json:"last_error,omitempty"` // the last server (transport) error
	Stopped      string        `json:"stopped,omitempty"`    // a permanent error: no uploads until the server is re-pinned
	Queued       int           `json:"queued"`
	SpoolBytes   int64         `json:"spool_bytes"`
	SpoolBlocked bool          `json:"spool_blocked"` // the spool hit its cap; captures of rewritten sources are paused
	Failing      []SourceError `json:"failing,omitempty"`
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
	st := s.status()
	// Outside s.mu: the store query may wait for a capture's transaction.
	if red, n, err := s.sy.store.RedactionTotals(context.Background()); err == nil {
		st.Redactions, st.RedactedSources = red, n
	}
	return st
}

func (s *Scheduler) status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{ServerDown: s.down, RetryAt: s.retryAt, Queued: len(s.ready) + len(s.waiting) + s.running,
		SpoolBytes: s.sy.spool.Used(), SpoolBlocked: s.sy.spool.Blocked()}
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

// next returns the index in order of the first source due at now, or -1
// and how long until one is. Called with s.mu held.
func (s *Scheduler) next(now time.Time) (int, time.Duration) {
	if s.halted != nil {
		if s.cfg.Repin == nil {
			return -1, time.Hour
		}
		return -1, max(s.repinAt.Sub(now), time.Millisecond)
	}
	if len(s.order) == 0 {
		return -1, time.Hour
	}
	if now.Before(s.retryAt) {
		return -1, s.retryAt.Sub(now)
	}
	wait := time.Hour
	for i, path := range s.order {
		f := s.failing[path]
		if f == nil || !now.Before(f.retryAt) {
			return i, 0
		}
		wait = min(wait, f.retryAt.Sub(now))
	}
	return -1, wait
}

// jitter: equal jitter, half fixed and half random.
func jitter(d time.Duration) time.Duration { return d/2 + rand.N(d/2+1) }

// Run processes due flushes until ctx ends. It first queues every source
// the store still has unacknowledged data for: the watermark is the queue.
func (s *Scheduler) Run(ctx context.Context) error {
	specs, err := s.sy.store.PendingSpecs(ctx)
	if err != nil {
		return err
	}
	for _, sp := range specs {
		s.due(sp.Path, &job{spec: sp})
	}
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		s.repin(time.Now())
		s.mu.Lock()
		_, wait := s.next(time.Now())
		s.mu.Unlock()
		if wait > 0 {
			timer.Reset(wait)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-s.wake:
			case <-timer.C:
			}
			continue
		}
		s.runOnce(ctx)
	}
}

// runOnce syncs due sources in arrival order until none is due or the
// server fails. A source failing on its own backs off alone.
func (s *Scheduler) runOnce(ctx context.Context) {
	for {
		s.mu.Lock()
		i, _ := s.next(time.Now())
		if i < 0 {
			s.mu.Unlock()
			return
		}
		path := s.order[i]
		s.order = slices.Delete(s.order, i, i+1)
		j := s.ready[path]
		delete(s.ready, path)
		s.running = 1
		filter, bound := s.filter, s.bound
		s.mu.Unlock()

		upTo, bounded := boundOf(bound, j.spec)
		if filter != nil && !filter(j.spec) || bounded && upTo < 0 {
			s.mu.Lock()
			s.running = 0
			delete(s.failing, path)
			s.mu.Unlock()
			continue
		}
		var err error
		if j.spec.Export {
			if j.exportFn != nil {
				err = s.sy.SyncExportFunc(ctx, j.spec, j.exportFn)
			} else if j.export != nil {
				err = s.sy.SyncExport(ctx, j.spec, j.export)
			} else {
				err = s.sy.Resume(ctx, j.spec)
			}
		} else if bounded {
			err = s.sy.SyncUpTo(ctx, j.spec, upTo)
		} else {
			err = s.sy.Sync(ctx, j.spec)
		}
		tail := err == nil && s.sy.provisional(ctx, j.spec.Path)
		s.mu.Lock()
		s.running = 0
		if ctx.Err() != nil {
			s.mu.Unlock()
			return
		}
		if err == nil {
			s.backoff, s.down, s.lastErr = 0, false, nil
			delete(s.failing, path)
			s.sealLater(j, tail)
			s.mu.Unlock()
			continue
		}
		if syncproto.Permanent(err) {
			// Retrying cannot help (a TLS pin mismatch): keep the source
			// queued and stop until the saved pin changes (Repin).
			if s.ready[path] == nil && s.waiting[path] == nil {
				s.ready[path] = &job{spec: j.spec, export: j.export, exportFn: j.exportFn}
				s.order = append([]string{path}, s.order...)
			}
			s.halted, s.lastErr = err, err
			s.repinAt = time.Now().Add(s.cfg.RepinEvery)
			s.mu.Unlock()
			s.sy.cfg.Logger.Error("devicesync: sync stopped until the server is re-pinned", "err", err)
			return
		}
		transport := syncproto.Retryable(err) && !errors.Is(err, ErrSourceChanged) && !errors.Is(err, ErrSpoolFull)
		// Requeue, keeping newer notifications: at the front when the
		// server failed (every source waits for it), else at the back.
		if s.ready[path] == nil && s.waiting[path] == nil {
			s.ready[path] = &job{spec: j.spec, export: j.export, exportFn: j.exportFn}
			if transport {
				s.order = append([]string{path}, s.order...)
			} else {
				s.order = append(s.order, path)
			}
		}
		if transport {
			s.backoff = min(max(2*s.backoff, s.cfg.BackoffMin), s.cfg.BackoffMax)
			d := jitter(s.backoff)
			now := time.Now()
			var he *syncproto.HTTPError
			if errors.As(err, &he) {
				d = max(d, he.RetryAt.Sub(now))
			}
			busy := syncproto.Busy(err)
			s.retryAt, s.lastErr, s.down = now.Add(d), err, !busy
			s.mu.Unlock()
			s.sy.cfg.Logger.Info("devicesync: sync backing off", "server_busy", busy, "retry_in", d, "err", err)
			return
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
		s.mu.Unlock()
		s.sy.cfg.Logger.Warn("devicesync: flush failed, will retry", "path", path, "retry_in", d, "attempts", f.attempts, "err", err)
	}
}

func boundOf(fn func(SourceSpec) (int64, bool), spec SourceSpec) (int64, bool) {
	if fn == nil {
		return 0, false
	}
	return fn(spec)
}
