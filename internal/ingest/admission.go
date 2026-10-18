package ingest

import (
	"fmt"
	"net/http"
	"sync"
)

// FlushAdmission bounds active HTTP flushes across Servers sharing this owner.
// Each device has a configured HTTP limit and each exact device/path one owner.
// It is process-local, not a cross-server database
// lock. Production must supply an owner sized for its configured pool headroom.
type FlushAdmission struct {
	mu          sync.Mutex
	limit       int
	deviceLimit int
	active      map[*flushTicket]struct{}
	devices     map[string]int
	paths       map[flushPathKey]*pathTicket
}

// NewFlushAdmission sets a positive global limit; per-device uploads stay serial.
func NewFlushAdmission(limit int) (*FlushAdmission, error) {
	return NewFlushAdmissionPerDevice(limit, 1)
}

// NewFlushAdmissionPerDevice explicitly opts in to one or two requests per
// device. The global limit still accounts for every admitted request.
func NewFlushAdmissionPerDevice(limit, perDevice int) (*FlushAdmission, error) {
	if limit < 1 {
		return nil, fmt.Errorf("flush admission: global limit must be positive")
	}
	if perDevice != 1 && perDevice != 2 {
		return nil, fmt.Errorf("flush admission: per-device limit must be 1 or 2")
	}
	if limit < perDevice {
		return nil, fmt.Errorf("flush admission: global limit %d cannot support per-device limit %d", limit, perDevice)
	}
	return &FlushAdmission{limit: limit, deviceLimit: perDevice, active: make(map[*flushTicket]struct{}), devices: make(map[string]int), paths: make(map[flushPathKey]*pathTicket)}, nil
}

func (a *FlushAdmission) acquire(device string) (*flushTicket, *Error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.devices[device] >= a.deviceLimit {
		code := "flush_in_progress" // Retain the legacy serial refusal contract.
		if a.deviceLimit == 2 {
			code = "device_busy"
		}
		return nil, &Error{http.StatusTooManyRequests, code, "this device's flush capacity is in use; retry later"}
	}
	if len(a.active) >= a.limit {
		return nil, &Error{http.StatusServiceUnavailable, "server_busy", "flush capacity is in use; retry later"}
	}
	t := &flushTicket{owner: a, device: device}
	a.active[t] = struct{}{}
	a.devices[device]++
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
		if _, ok := a.active[t]; ok {
			delete(a.active, t)
			a.devices[t.device]--
			if a.devices[t.device] == 0 {
				delete(a.devices, t.device)
			}
		}
	})
}

// flushPathKey uses the same literal device/path identity as the database.
// File identity, generation and source attributes must not bypass this owner.
type flushPathKey struct{ device, path string }

func (a *FlushAdmission) acquirePath(device, path string) (*pathTicket, *Error) {
	if device == "" || path == "" {
		return nil, badRequest("flush path admission needs device and path")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	key := flushPathKey{device, path}
	if a.paths[key] != nil {
		return nil, &Error{http.StatusTooManyRequests, "source_busy", "another flush of this device path is in progress; retry later"}
	}
	ticket := &pathTicket{owner: a, key: key}
	a.paths[key] = ticket
	return ticket, nil
}

type pathTicket struct {
	owner *FlushAdmission
	key   flushPathKey
	once  sync.Once
}

// Actual Flush return releases the path. Context cancellation alone cannot
// release an operation that is still reading or applying its request.
func (t *pathTicket) release() {
	t.once.Do(func() {
		a := t.owner
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.paths[t.key] == t {
			delete(a.paths, t.key)
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
