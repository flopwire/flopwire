package devicesync

import (
	"os"
	"sync"
)

// descriptorOwner owns cached descriptors, including retired entries still
// borrowed by a reader. Its mutex never spans reads, SQLite or transport.
// The surrounding Syncer remains serial; this does not dispatch workers.
type descriptorOwner struct {
	mu           sync.Mutex
	current      map[int64]*descriptorEntry
	slots, limit int
	closed       bool
}

type descriptorEntry struct {
	f         *os.File
	borrowers int
}

type descriptorBorrow struct {
	owner    *descriptorOwner
	sourceID int64
	entry    *descriptorEntry
	once     sync.Once
}

func newDescriptorOwner(limit int) *descriptorOwner {
	return &descriptorOwner{current: make(map[int64]*descriptorEntry), limit: limit}
}

// canRetain is a serial capture pre-open check, not a reservation.
func (o *descriptorOwner) canRetain(id int64) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	old := o.current[id]
	return !o.closed && (o.slots < o.limit || old != nil && old.borrowers == 0)
}

// retain transfers custody only on success. Replacement retires the previous
// entry before admission; retired borrowers continue to consume a slot.
func (o *descriptorOwner) retain(id int64, f *os.File) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed || f == nil {
		return false
	}
	if old := o.current[id]; old != nil && old.f == f {
		return true
	}
	o.retireLocked(id)
	if o.slots >= o.limit {
		return false
	}
	o.current[id] = &descriptorEntry{f: f}
	o.slots++
	return true
}

func (o *descriptorOwner) borrow(id int64) *descriptorBorrow {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	e := o.current[id]
	if e == nil {
		return nil
	}
	e.borrowers++
	return &descriptorBorrow{owner: o, sourceID: id, entry: e}
}

func (b *descriptorBorrow) file() *os.File { return b.entry.f }

func (b *descriptorBorrow) release() {
	b.once.Do(func() {
		o := b.owner
		o.mu.Lock()
		defer o.mu.Unlock()
		b.entry.borrowers--
		if b.entry.borrowers == 0 && o.current[b.sourceID] != b.entry {
			b.entry.f.Close()
			o.slots--
		}
	})
}

func (o *descriptorOwner) retire(id int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.retireLocked(id)
}

func (o *descriptorOwner) retireLocked(id int64) {
	e := o.current[id]
	if e == nil {
		return
	}
	delete(o.current, id)
	if e.borrowers == 0 {
		e.f.Close()
		o.slots--
	}
}

func (o *descriptorOwner) close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = true
	for id := range o.current {
		o.retireLocked(id)
	}
}
