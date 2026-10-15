package agent

import (
	"context"
	"encoding/json"
	"net"
	"reflect"
	"testing"

	"github.com/flopwire/flopwire/internal/devicesync"
)

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
