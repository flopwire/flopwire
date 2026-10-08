package ingest

import (
	"fmt"
	"net/http"
	"sync"
)

// FlushAdmission bounds active HTTP flushes across Servers sharing this owner.
// Each device has one slot. It is process-local, not a cross-server database
// lock. Production must supply an owner sized for its configured pool headroom.
type FlushAdmission struct {
	mu     sync.Mutex
	limit  int
	active map[string]*flushTicket
}

// NewFlushAdmission sets a positive global limit; per-device uploads stay serial.
func NewFlushAdmission(limit int) (*FlushAdmission, error) {
	if limit < 1 {
		return nil, fmt.Errorf("flush admission: global limit must be positive")
	}
	return &FlushAdmission{limit: limit, active: make(map[string]*flushTicket)}, nil
}

func (a *FlushAdmission) acquire(device string) (*flushTicket, *Error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active[device] != nil {
		return nil, &Error{http.StatusTooManyRequests, "flush_in_progress", "another flush of this device is in progress; retry later"}
	}
	if len(a.active) >= a.limit {
		return nil, &Error{http.StatusServiceUnavailable, "server_busy", "flush capacity is in use; retry later"}
	}
	t := &flushTicket{owner: a, device: device}
	a.active[device] = t
	return t, nil
}

type flushTicket struct {
	owner  *FlushAdmission
	device string
	once   sync.Once
}

// Release runs only when the handler has stopped reading and applying the
// request. Cancellation alone does not release a still-running request.
func (t *flushTicket) release() {
	t.once.Do(func() {
		a := t.owner
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.active[t.device] == t {
			delete(a.active, t.device)
		}
	})
}

func (s *Server) flushAdmission() *FlushAdmission {
	if s.Admission != nil {
		return s.Admission
	}
	// Direct constructors retain a bounded, serial default. This fallback
	// does not infer or reserve database headroom; production supplies it.
	s.admissionOnce.Do(func() { s.defaultAdmission, _ = NewFlushAdmission(1) })
	return s.defaultAdmission
}
