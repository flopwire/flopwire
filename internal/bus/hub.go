package bus

import "sync"

// hub wakes long polls in this process. A poll takes its person's channel
// before it reads, and a write that changes what the person's devices
// should hold (a send made deliverable, an accept, a revoke) closes the
// channel after it commits, so a poll never sleeps through a commit that
// its read missed. Each notify also advances the person's generation,
// which a poll answer carries: a device whose next poll names an older
// generation is answered at once, so a change that lands between two
// polls (a revoke that re-held a message the device holds) reaches the
// device without waiting out the poll. A server runs as one process (one
// Compose service); a second process's polls would see a change at their
// next poll instead.
type hub struct {
	mu    sync.Mutex
	waits map[string]chan struct{}
	gens  map[string]int64
}

// wait returns the channel the next notify(key) closes, and key's
// generation now.
func (h *hub) wait(key string) (<-chan struct{}, int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.waits == nil {
		h.waits = map[string]chan struct{}{}
	}
	ch, ok := h.waits[key]
	if !ok {
		ch = make(chan struct{})
		h.waits[key] = ch
	}
	return ch, h.gens[key]
}

// notify advances key's generation and wakes every poll waiting on key.
func (h *hub) notify(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gens == nil {
		h.gens = map[string]int64{}
	}
	h.gens[key]++
	if ch, ok := h.waits[key]; ok {
		close(ch)
		delete(h.waits, key)
	}
}
