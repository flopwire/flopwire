package devicesync

import (
	"context"
	"errors"
	"sync"
)

var errOperationsClosed = errors.New("devicesync: syncer operations are closed")

// operationOwner admits a complete logical call on one exact source path and
// one fixed workspace. Waiters own neither. Capture and state work still use
// Syncer.mu; only admitted operations may overlap transport.
type operationOwner struct {
	mu      sync.Mutex
	slots   []*syncScratch
	limit   int
	active  map[string]*operationLease
	changed chan struct{}
	closed  bool
	idle    bool // exclusive admission pause while a drained config callback runs
}

type operationLease struct {
	owner   *operationOwner
	path    string
	scratch *syncScratch
	once    sync.Once
}

func newOperationOwner(slots []*syncScratch) *operationOwner {
	return &operationOwner{slots: slots, limit: len(slots), active: make(map[string]*operationLease), changed: make(chan struct{})}
}

func (o *operationOwner) signalLocked() {
	close(o.changed)
	o.changed = make(chan struct{})
}

func (o *operationOwner) acquire(ctx context.Context, path string) (*operationLease, error) {
	if path == "" {
		return nil, errors.New("devicesync: empty operation path")
	}
	for {
		o.mu.Lock()
		if o.closed {
			o.mu.Unlock()
			return nil, errOperationsClosed
		}
		if err := ctx.Err(); err != nil {
			o.mu.Unlock()
			return nil, err
		}
		if !o.idle && len(o.active) < o.limit && o.active[path] == nil {
			for _, scratch := range o.slots {
				used := false
				for _, lease := range o.active {
					if lease.scratch == scratch {
						used = true
						break
					}
				}
				if !used {
					lease := &operationLease{owner: o, path: path, scratch: scratch}
					o.active[path] = lease
					o.mu.Unlock()
					return lease, nil
				}
			}
		}
		changed := o.changed
		o.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (l *operationLease) release() {
	l.once.Do(func() {
		o := l.owner
		o.mu.Lock()
		defer o.mu.Unlock()
		if o.active[l.path] == l {
			delete(o.active, l.path)
			o.signalLocked()
		}
	})
}

// withIdle excludes new public and scheduler admission while waiting for actual
// releases and running fn. No owner mutex spans fn or transport.
func (o *operationOwner) withIdle(ctx context.Context, fn func() error) error {
	if fn == nil {
		return errors.New("devicesync: missing idle callback")
	}
	for {
		o.mu.Lock()
		if o.closed {
			o.mu.Unlock()
			return errOperationsClosed
		}
		if err := ctx.Err(); err != nil {
			o.mu.Unlock()
			return err
		}
		if !o.idle {
			o.idle = true
			o.signalLocked()
			o.mu.Unlock()
			break
		}
		changed := o.changed
		o.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
	defer func() { o.mu.Lock(); o.idle = false; o.signalLocked(); o.mu.Unlock() }()
	for {
		o.mu.Lock()
		closed, active, changed := o.closed, len(o.active), o.changed
		o.mu.Unlock()
		if closed {
			return errOperationsClosed
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if active == 0 {
			return fn()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (o *operationOwner) close() {
	o.mu.Lock()
	o.closed = true
	o.signalLocked()
	for len(o.active) != 0 || o.idle {
		changed := o.changed
		o.mu.Unlock()
		<-changed
		o.mu.Lock()
	}
	o.mu.Unlock()
}

func (s *Syncer) acquireOperation(ctx context.Context, path string) (*operationLease, error) {
	return s.operations.acquire(ctx, path)
}

func (s *Syncer) withIdleOperations(ctx context.Context, fn func() error) error {
	return s.operations.withIdle(ctx, fn)
}

// setOperationLimit changes admission, not ownership of in-flight work. A
// decrease takes effect immediately; an increase requires a drained callback.
func (s *Syncer) setOperationLimit(limit int) error {
	o := s.operations
	o.mu.Lock()
	defer o.mu.Unlock()
	if limit < 1 || limit > 2 || limit > len(o.slots) {
		return errors.New("devicesync: operation limit exceeds configured workspace capacity")
	}
	if o.closed {
		return errOperationsClosed
	}
	if limit > o.limit && (!o.idle || len(o.active) != 0) {
		return errors.New("devicesync: operation limit increase requires drained admission")
	}
	o.limit = limit
	o.signalLocked()
	return nil
}
