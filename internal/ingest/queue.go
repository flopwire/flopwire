package ingest

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/flopwire/flopwire/internal/domain"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Queue runs parses. The durable queue is source_parse_state (requested_seq
// > parsed_seq); this is its in-memory front: a bounded channel fed by
// flushes and by a sweep of the table, drained by a fixed pool of workers,
// with at most one parse per source at a time. Backpressure: when the
// durable backlog passes MaxBacklog, flushes are refused with a retryable
// 503 until the workers catch up.
type Queue struct {
	Pool    *pgxpool.Pool
	Objects Objects
	Log     *slog.Logger
	// Workers parse in parallel. Default 4.
	Workers int
	// RefreshInterval pauses the single idle-reparse worker between sources. Default 2s.
	RefreshInterval time.Duration
	// MaxBacklog pending sources before flushes are refused; 0: 20000.
	MaxBacklog int64
	// Sweep is the interval of the table sweep. Default 2s.
	Sweep time.Duration
	// lastHidden is when the sweep last extended hides and purged expired
	// ones (sweep goroutine only).
	lastHidden time.Time
	// MaxAttempts consecutive failures quarantine a source: it is no
	// longer retried until Release. Default 12 (about 2h of backoff).
	MaxAttempts int

	once        sync.Once
	ch          chan string
	refreshWake chan struct{}
	// refreshMu guards refreshBacklog, the stale sources nextRefresh has
	// listed and not yet handed out.
	refreshMu      sync.Mutex
	refreshBacklog []string
	mu             sync.Mutex
	queued         map[string]bool
	running        map[string]bool
	backlog        atomic.Int64
	oldest         atomic.Int64 // unix ns of the oldest pending request; 0: none
	failing        atomic.Int64
	quarantined    atomic.Int64
	// masks is the redacted-line catalog, kept between parses.
	masks maskCache
}

func (q *Queue) init() {
	q.once.Do(func() {
		if q.Workers <= 0 {
			q.Workers = 4
		}
		if q.RefreshInterval <= 0 {
			q.RefreshInterval = 2 * time.Second
		}
		if q.MaxBacklog <= 0 {
			q.MaxBacklog = 20000
		}
		if q.Sweep <= 0 {
			q.Sweep = 2 * time.Second
		}
		if q.MaxAttempts <= 0 {
			q.MaxAttempts = 12
		}
		if q.Log == nil {
			q.Log = slog.Default()
		}
		q.ch = make(chan string, 1024)
		q.refreshWake = make(chan struct{}, 1)
		q.queued, q.running = map[string]bool{}, map[string]bool{}
	})
}

// Notify asks for a parse of the source. It never blocks: a full channel
// leaves the request to the next sweep.
func (q *Queue) Notify(id string) {
	q.init()
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.queued[id] {
		return
	}
	select {
	case q.ch <- id:
		q.queued[id] = true
	default:
	}
}

// Overloaded reports a backlog past MaxBacklog, as a flush refusal.
func (q *Queue) Overloaded() *Error {
	q.init()
	if q.backlog.Load() <= q.MaxBacklog {
		return nil
	}
	return &Error{http.StatusServiceUnavailable, "parse_backlog", "the server is behind on parsing; retry later"}
}

// Status reports the parse backlog: pending sources, the backlog at which
// flushes are refused, the age of the oldest request, sources whose last
// attempt failed, and quarantined sources.
func (q *Queue) Status() (pending, capacity int64, lag time.Duration, failing, quarantined int64) {
	q.init()
	if o := q.oldest.Load(); o > 0 {
		lag = time.Since(time.Unix(0, o))
	}
	return q.backlog.Load(), q.MaxBacklog, lag, q.failing.Load(), q.quarantined.Load()
}

// Quarantined lists quarantined sources, most recent first.
func (q *Queue) Quarantined(ctx context.Context, limit int) ([]domain.QuarantinedSource, error) {
	rows, err := q.Pool.Query(ctx, `SELECT s.id::text,s.path,s.agent,s.device_id::text,p.attempts,p.last_error,p.quarantined_at
		FROM source_parse_state p JOIN sources s ON s.id=p.source_id WHERE p.quarantined_at IS NOT NULL
		ORDER BY p.quarantined_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.QuarantinedSource, error) {
		var s domain.QuarantinedSource
		err := row.Scan(&s.SourceID, &s.Path, &s.Agent, &s.DeviceID, &s.Attempts, &s.LastError, &s.QuarantinedAt)
		return s, err
	})
}

// Release lifts a source's quarantine and asks for a full re-parse, with
// its audit event in the same transaction. It reports whether the source
// was quarantined.
func (q *Queue) Release(ctx context.Context, sourceID string, audit domain.AuditEvent) (bool, error) {
	if _, err := uuid.Parse(sourceID); err != nil {
		return false, nil
	}
	released := false
	err := pgx.BeginTxFunc(ctx, q.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE source_parse_state SET quarantined_at=NULL,attempts=0,next_attempt_at=NULL,reparse=true,
				requested_seq=requested_seq+1,requested_at=now() WHERE source_id=$1 AND quarantined_at IS NOT NULL`, sourceID)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		released = true
		return store.InsertAudit(ctx, tx, audit)
	})
	if err != nil || !released {
		return false, err
	}
	q.Notify(sourceID)
	return true, nil
}

// Run parses until ctx ends.
func (q *Queue) Run(ctx context.Context) {
	q.init()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); q.refresh(ctx) }()
	for range q.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case id := <-q.ch:
					q.work(ctx, id)
				}
			}
		}()
	}
	t := time.NewTicker(q.Sweep)
	defer t.Stop()
	for {
		q.sweep(ctx)
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-t.C:
		}
	}
}

func (q *Queue) work(ctx context.Context, id string) {
	q.runParse(ctx, id, q.ParseSource)
}

func (q *Queue) runParse(ctx context.Context, id string, parse func(context.Context, string) error) {
	q.mu.Lock()
	delete(q.queued, id)
	if q.running[id] {
		q.mu.Unlock()
		return // the sweep brings it back once the running parse ends
	}
	q.running[id] = true
	q.mu.Unlock()
	err := parse(ctx, id)
	q.mu.Lock()
	delete(q.running, id)
	q.mu.Unlock()
	if err == nil || ctx.Err() != nil {
		return
	}
	if errors.Is(err, errPurgeBusy) || errors.Is(err, ErrArchiveChanged) || errors.Is(err, errParseBusy) {
		q.later(ctx, id)
		return
	}
	q.failed(ctx, id, err)
}

// purgeRetry is how long a parse waits for a busy purge lock.
const purgeRetry = 5 * time.Second

// later requeues a parse that could not run yet, without counting an
// attempt: the sweep picks it up after purgeRetry.
func (q *Queue) later(ctx context.Context, id string) {
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := q.Pool.Exec(bctx, `UPDATE source_parse_state SET next_attempt_at=now()+make_interval(secs => $2) WHERE source_id=$1`,
		id, purgeRetry.Seconds()); err != nil {
		q.Log.Warn("ingest: requeue after busy ingest lock", "source", id, "error", err)
	}
}

// failed records a failed parse: the backoff doubles per consecutive
// failure (to an hour), and MaxAttempts of them quarantine the source.
func (q *Queue) failed(ctx context.Context, id string, err error) {
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	msg := strings.ToValidUTF8(err.Error(), "?")
	if len(msg) > 2000 {
		msg = strings.ToValidUTF8(msg[:2000], "")
	}
	var attempts int
	var quarantined bool
	qerr := q.Pool.QueryRow(bctx, `UPDATE source_parse_state SET attempts=attempts+1,last_error=$2,
			next_attempt_at=now()+make_interval(secs => LEAST(3600, 2^LEAST(attempts,12)) * $3),
			quarantined_at=CASE WHEN attempts+1>=$4 THEN COALESCE(quarantined_at,now()) END
		WHERE source_id=$1 RETURNING attempts,quarantined_at IS NOT NULL`, id, msg, 0.8+0.4*rand.Float64(), q.MaxAttempts).Scan(&attempts, &quarantined)
	switch {
	case qerr != nil:
		q.Log.Warn("ingest: parse failed; will retry", "source", id, "error", err)
	case quarantined:
		q.Log.Error("ingest: parse failed repeatedly; source quarantined until an administrator releases it", "source", id, "attempts", attempts, "error", err)
	default:
		q.Log.Warn("ingest: parse failed; will retry", "source", id, "attempts", attempts, "error", err)
	}
}

// sweep enqueues due requests, refreshes the backlog counters, and
// applies the admin path rules to stored conversations (EnforceRules).
func (q *Queue) sweep(ctx context.Context) {
	hiddenToo := time.Since(q.lastHidden) >= hiddenPass
	if hiddenToo {
		q.lastHidden = time.Now()
	}
	if e, err := q.enforceRules(ctx, hiddenToo); err != nil && ctx.Err() == nil {
		q.Log.Warn("ingest: applying path rules to stored conversations; will retry", "hidden", e.Hidden, "restored", e.Restored, "purged", e.Purged, "error", err)
	}
	rows, err := q.Pool.Query(ctx, `SELECT source_id::text FROM source_parse_state
		WHERE requested_seq>parsed_seq AND quarantined_at IS NULL AND (next_attempt_at IS NULL OR next_attempt_at<=now())
		ORDER BY requested_at LIMIT $1`, cap(q.ch))
	if err == nil {
		ids, _ := pgx.CollectRows(rows, pgx.RowTo[string])
		for _, id := range ids {
			q.Notify(id)
		}
	}
	var n, failing, quarantined int64
	var oldest *time.Time
	if err := q.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE quarantined_at IS NULL), min(requested_at) FILTER (WHERE quarantined_at IS NULL),
			count(*) FILTER (WHERE attempts>0 AND quarantined_at IS NULL), count(*) FILTER (WHERE quarantined_at IS NOT NULL)
		FROM source_parse_state WHERE requested_seq>parsed_seq OR quarantined_at IS NOT NULL`).Scan(&n, &oldest, &failing, &quarantined); err == nil {
		q.backlog.Store(n)
		q.failing.Store(failing)
		q.quarantined.Store(quarantined)
		if oldest != nil {
			q.oldest.Store(oldest.UnixNano())
		} else {
			q.oldest.Store(0)
		}
	}
}

// Drain parses every pending source now, in the caller's goroutine, and
// reports the first error. For tests and one-shot rebuilds.
func (q *Queue) Drain(ctx context.Context) error {
	q.init()
	for {
		rows, err := q.Pool.Query(ctx, `SELECT source_id::text FROM source_parse_state WHERE requested_seq>parsed_seq AND quarantined_at IS NULL ORDER BY requested_at LIMIT 256`)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			id, err := q.nextRefresh(ctx)
			if err != nil || id == "" {
				return err
			}
			ids = []string{id}
		}
		for _, id := range ids {
			if err := q.ParseSource(ctx, id); err != nil {
				return err
			}
		}
	}
}

// Redactions sums the redaction records of every live source's latest
// generation (device) and of its parse (server).
func (q *Queue) Redactions(ctx context.Context) (domain.RedactionStatus, error) {
	st := domain.RedactionStatus{Device: map[string]int64{}, Server: map[string]int64{}, Rules: map[string]int64{}}
	const latest = `SELECT g.* FROM sources s JOIN LATERAL (SELECT * FROM generations g WHERE g.source_id=s.id ORDER BY generation DESC LIMIT 1) g ON true
		WHERE s.tombstoned_at IS NULL`
	collect := func(sql string, into map[string]int64) error {
		rows, err := q.Pool.Query(ctx, sql)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			var n int64
			if err := rows.Scan(&k, &n); err != nil {
				return err
			}
			into[k] = n
		}
		return rows.Err()
	}
	if err := collect(`SELECT e.key, sum(e.value::bigint)::bigint FROM (`+latest+`) g, jsonb_each_text(COALESCE(g.redactions,'{}')) e GROUP BY e.key`, st.Device); err != nil {
		return st, err
	}
	if err := collect(`SELECT e.key, sum(e.value::bigint)::bigint FROM source_parse_state p JOIN sources s ON s.id=p.source_id,
		jsonb_each_text(p.server_redactions) e WHERE s.tombstoned_at IS NULL GROUP BY e.key`, st.Server); err != nil {
		return st, err
	}
	if err := collect(`SELECT COALESCE(g.redaction_rules,''), count(*) FROM (`+latest+`) g GROUP BY 1`, st.Rules); err != nil {
		return st, err
	}
	st.SourcesUnredacted = st.Rules[""]
	delete(st.Rules, "")
	err := q.Pool.QueryRow(ctx, `SELECT count(*) FROM (`+latest+`) g WHERE g.redactions IS NOT NULL AND g.redactions<>'{}'::jsonb`).Scan(&st.SourcesRedacted)
	return st, err
}
