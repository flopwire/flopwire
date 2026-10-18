package agent

import (
	"context"
	"encoding/json"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
)

func TestDiagnosticsBackgroundCallsites(t *testing.T) {
	f := newCoworkFixture(t)
	a := f.a
	// Disabled stores do not count. Enabled polls include a busy-store skip
	// and a canceled lock wait, even when neither reaches a database read.
	a.pollStore(ctx, &storeState{}, false, false)
	d := &storeState{path: "/synthetic/missing"}
	d.mu.Lock()
	a.pollStore(ctx, d, false, false)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	a.pollStore(canceled, d, false, true)
	d.mu.Unlock()
	d.last = time.Now()
	a.pollStore(ctx, d, false, false) // throttled poll

	// An ignored write still counts as an event handled by the daemon.
	a.dirEvent(ctx, watchEvent{path: "/synthetic/untracked", modify: true}, nil)
	a.mu.Lock()
	a.busy = 1
	a.mu.Unlock()
	a.shrinkIfIdle(ctx) // busy early return does not trim
	a.mu.Lock()
	a.busy = 0
	a.mu.Unlock()
	a.shrinkIfIdle(ctx)
	want := map[string]uint64{"store_poll": 3, "watcher_event": 1, "idle_trim": 1}
	for _, op := range a.Diagnostics().Operations {
		if n, ok := want[op.Name]; ok {
			if op.Calls != n || op.Completed != n || op.InFlight != 0 || op.TotalWallNS == 0 {
				t.Errorf("%s: %+v, want %d completed calls", op.Name, op, n)
			}
			delete(want, op.Name)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing operation counters: %v", want)
	}
}

func TestDiagnosticsReadOnlyControl(t *testing.T) {
	// A snapshot requires neither an open index nor an available agent lock.
	a := &Agent{}
	a.mu.Lock()
	a.coworkMu.Lock()
	defer a.mu.Unlock()
	defer a.coworkMu.Unlock()
	before := a.Diagnostics()
	server, client := net.Pipe()
	defer client.Close()
	go a.serveConn(context.Background(), server)
	if err := json.NewEncoder(client).Encode(Request{Op: "diagnostics"}); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := json.NewDecoder(client).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || response.Diagnostics == nil {
		t.Fatalf("response: %+v", response)
	}
	if !reflect.DeepEqual(before.Operations, response.Diagnostics.Operations) {
		t.Fatal("snapshot changed operation counters")
	}
}

func TestDiagnosticsCallsites(t *testing.T) {
	f := newCoworkFixture(t)
	before := f.a.Diagnostics()
	if err := f.a.sweep(ctx); err != nil {
		t.Fatal(err)
	}
	_, _ = f.a.authorizeCapture(ctx, devicesync.SourceSpec{Path: "/synthetic/missing"})
	after := f.a.Diagnostics()
	want := map[string]uint64{"sweep": 1, "discover_all": 1, "authorize_capture": 1, "refresh_cowork": 2, "refresh_cowork_lock_wait": 2, "captured_claude_children": 2, "refresh_from_sweep": 1, "refresh_from_authorization": 1}
	for i, op := range after.Operations {
		if n, ok := want[op.Name]; ok {
			if op.Calls-before.Operations[i].Calls != n || op.Completed-before.Operations[i].Completed != n || op.InFlight != 0 {
				t.Errorf("%s: before %+v, after %+v", op.Name, before.Operations[i], op)
			}
		}
	}
}

func TestDiagnosticsIndexAndFastLane(t *testing.T) {
	f := newCoworkFixture(t)
	coworkMetadata(t, f, nil, nil, nil)
	path := coworkTranscript(t, f)
	f.once()
	before := f.a.Diagnostics()
	f.a.mu.Lock()
	target := f.a.targets[path]
	f.a.mu.Unlock()
	if target == nil {
		t.Fatal("synthetic transcript was not discovered")
	}
	if err := f.a.index(ctx, target); err != nil {
		t.Fatal(err)
	}
	f.a.fastLane(ctx)
	after := f.a.Diagnostics()
	for i, op := range after.Operations {
		if op.Name == "index_target" || op.Name == "fast_lane" {
			if op.Calls-before.Operations[i].Calls != 1 || op.Completed-before.Operations[i].Completed != 1 || op.InFlight != 0 {
				t.Errorf("%s: before %+v, after %+v", op.Name, before.Operations[i], op)
			}
		}
	}
}
