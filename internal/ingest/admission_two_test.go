package ingest

import (
	"context"
	"encoding/json"
	"github.com/flopwire/flopwire/internal/syncproto"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTwoFlushAdmissionCountsRequestsAndRetainsCancelledOwners(t *testing.T) {
	for _, pair := range [][2]int{{1, 2}, {3, 0}, {3, 3}, {0, 1}} {
		if _, err := NewFlushAdmissionPerDevice(pair[0], pair[1]); err == nil {
			t.Fatal("invalid admission configuration accepted")
		}
	}
	a, err := NewFlushAdmissionPerDevice(2, 2)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Admission: a}
	var bodies []*admissionBody
	var finished []chan struct{}
	var cancels []context.CancelFunc
	for i := 0; i < 2; i++ {
		body := &admissionBody{started: make(chan struct{}), finish: make(chan error, 1)}
		done := make(chan struct{})
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		bodies = append(bodies, body)
		finished = append(finished, done)
		go func() { defer close(done); s.ServeSync(httptest.NewRecorder(), admissionRequest(ctx, body), "one") }()
		waitAdmissionSignal(t, body.started)
	}
	if admissionCount(a) != 2 {
		t.Fatal("device counted instead of request tickets")
	}
	requireAdmissionRefusal(t, s, "one", 429, "device_busy")
	requireAdmissionRefusal(t, s, "other", 503, "server_busy")
	cancels[0]()
	requireAdmissionRefusal(t, s, "one", 429, "device_busy")
	bodies[0].finish <- context.Canceled
	waitAdmissionSignal(t, finished[0])
	replacement, err := a.acquire("one")
	if err != nil {
		t.Fatal(err)
	}
	replacement.release()
	replacement.release()
	if admissionCount(a) != 1 {
		t.Fatal("duplicate release affected other active request")
	}
	cancels[1]()
	bodies[1].finish <- io.EOF
	waitAdmissionSignal(t, finished[1])
	if admissionCount(a) != 0 || len(a.devices) != 0 {
		t.Fatal("idle tickets/device counters retained")
	}
}

func TestTwoFlushCapabilitiesRequireExplicitBoundedRequest(t *testing.T) {
	a, err := NewFlushAdmissionPerDevice(2, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		query        string
		want, status int
	}{
		{"", 1, 200}, {"?max_concurrent_flushes=1", 1, 200}, {"?max_concurrent_flushes=2", 2, 200},
		{"?max_concurrent_flushes=0", 0, 400}, {"?max_concurrent_flushes=3", 0, 400},
		{"?max_concurrent_flushes=2&max_concurrent_flushes=1", 0, 400}, {"?device=x", 0, 400},
		{"?max_concurrent_flushes=2&other=x", 0, 400}, {"?max_concurrent_flushes=%zz", 0, 400},
	} {
		r := httptest.NewRequest(http.MethodGet, "/v1/sync/capabilities"+tc.query, nil)
		r.Header.Set(syncproto.HeaderVersion, "1")
		w := httptest.NewRecorder()
		(&Server{Admission: a}).ServeSync(w, r, "device")
		if w.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.query, w.Code, w.Body.String())
		}
		if tc.status == 200 {
			var out struct {
				Limit int `json:"max_concurrent_flushes"`
			}
			if err = json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Limit != tc.want {
				t.Fatal(w.Body.String())
			}
		}
	}
	// A new client explicitly requesting two must still observe serial when
	// the server has not opted in. No configuration is inferred from versions.
	r := httptest.NewRequest(http.MethodGet, syncproto.PathCapabilities+"?max_concurrent_flushes=2", nil)
	r.Header.Set(syncproto.HeaderVersion, "1")
	w := httptest.NewRecorder()
	new(Server).ServeSync(w, r, "device")
	var serial syncproto.CapabilitiesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &serial); err != nil || w.Code != 200 || serial.MaxConcurrentFlushes != 1 {
		t.Fatalf("default server advertised parallelism: %d %s", w.Code, w.Body.String())
	}
}
