package localindex

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Query budgets (decision D8). A query runs for at most Budget.Timeout
// and, for find, verifies at most Budget.VerifyBytes of stored text
// against its pattern. When either runs out it stops and returns what it
// has, flagged in Partial; it is never an error.
const (
	DefaultTimeout     = 10 * time.Second
	MaxTimeout         = 60 * time.Second
	DefaultVerifyBytes = 1 << 30
)

// Budget bounds one query. The zero value is the default.
type Budget struct {
	Timeout     time.Duration // default DefaultTimeout, at most MaxTimeout
	VerifyBytes int64         // stored text verified by find; default DefaultVerifyBytes
}

func (b Budget) norm() Budget {
	if b.Timeout <= 0 {
		b.Timeout = DefaultTimeout
	}
	b.Timeout = min(b.Timeout, MaxTimeout)
	if b.VerifyBytes <= 0 {
		b.VerifyBytes = DefaultVerifyBytes
	}
	return b
}

// Why a query stopped before it checked every candidate.
const (
	ReasonTimeout        = "timeout"         // Budget.Timeout ran out
	ReasonVerifyBudget   = "verify_budget"   // Budget.VerifyBytes ran out
	ReasonScanLimit      = "scan_limit"      // an unindexed scan read its MaxScan rows
	ReasonCandidateLimit = "candidate_limit" // a caller's bound on verified candidates
)

// Partial reports how much of a query ran.
type Partial struct {
	Truncated bool   // stopped before checking every candidate, without reaching the limit
	Reason    string // one of the Reason constants when Truncated
	Checked   int    // candidates checked, newest message first
	Total     int    // candidates in all; -1 when unknown (an unindexed scan, a timeout before counting)
	Elapsed   time.Duration
}

func (p *Partial) stop(reason string) {
	p.Truncated, p.Reason = true, reason
}

// deadline runs a query under the budget's timeout.
type deadline struct {
	ctx    context.Context // the budgeted context
	parent context.Context
	cancel context.CancelFunc
	start  time.Time
}

func newDeadline(parent context.Context, b Budget) *deadline {
	ctx, cancel := context.WithTimeout(parent, b.Timeout)
	return &deadline{ctx: ctx, parent: parent, cancel: cancel, start: time.Now()}
}

// expired reports whether the budget (not the caller) ended the query.
func (d *deadline) expired() bool {
	return d.ctx.Err() != nil && d.parent.Err() == nil
}

// settle turns an error caused by the budget's timeout into a truncated
// result; the caller's own cancellation stays an error.
func (d *deadline) settle(p *Partial, err error) error {
	p.Elapsed = time.Since(d.start)
	if err != nil && d.expired() && (errors.Is(err, context.DeadlineExceeded) || isInterrupt(err)) {
		p.stop(ReasonTimeout)
		return nil
	}
	if err == nil && d.expired() && !p.Truncated {
		p.stop(ReasonTimeout)
	}
	return err
}

// isInterrupt matches SQLite's error for a statement interrupted by its
// context.
func isInterrupt(err error) bool {
	return errors.Is(err, context.Canceled) || strings.Contains(strings.ToLower(err.Error()), "interrupted")
}
