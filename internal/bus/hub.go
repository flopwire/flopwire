package bus

import "sync"

// hub wakes long polls in this process. A poll takes its person's channel
// before it reads, and a write that makes a message deliverable closes the
// channel after it commits, so a poll never sleeps through a commit that
// its read missed. A server runs as one process (one Compose service); a
// second process's polls would see a message at their next poll instead.
type hub struct {
	mu    sync.Mutex
	waits map[string]chan struct{}
}

// wait returns the channel the next notify(key) closes.
func (h *hub) wait(key string) <-chan struct{} {
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
	return ch
}

// notify wakes every poll waiting on key.
func (h *hub) notify(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ch, ok := h.waits[key]; ok {
		close(ch)
		delete(h.waits, key)
	}
}
