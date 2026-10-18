package devicesync

import "sync"

type stallKey struct {
	sourceID   int64
	generation int64
}

// stallOwner keeps cumulative no-progress responses across scheduler turns.
// It contains no durable generation or permission state. Per-path dispatch must
// still exclude stale completions before parallel workers can use it.
type stallOwner struct {
	mu     sync.Mutex
	counts map[stallKey]int
}

// record returns the cumulative count and removes the bucket on the third
// stall, so a failed operation can retry after backoff without permanent poison.
func (s *stallOwner) record(key stallKey) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := s.counts[key] + 1
	if count >= 3 {
		delete(s.counts, key)
	} else {
		if s.counts == nil {
			s.counts = make(map[stallKey]int)
		}
		s.counts[key] = count
	}
	return count
}

func (s *stallOwner) forget(key stallKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.counts, key)
}
