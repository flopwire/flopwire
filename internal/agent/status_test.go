package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
)

type statusSync struct {
	*recorder
	st devicesync.Status
}

func (s statusSync) Status() devicesync.Status { return s.st }

// A capture lease can span a server request. A status caller must still see
// sync health, and must not mistake omitted Cowork counters for zero holds.
func TestStatusDuringCoworkScopeLease(t *testing.T) {
	f := newCoworkFixture(t)
	coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
	f.a.refreshCowork(ctx)
	f.a.cfg.Sync = statusSync{st: devicesync.Status{Queued: 17, SpoolBytes: 1234}}
	f.a.captureScopeMu.Lock()
	locked := true
	defer func() {
		if locked {
			f.a.captureScopeMu.Unlock()
		}
	}()
	done := make(chan Response, 1)
	go func() { done <- ask(t, f.a, Request{Op: "status"}) }()
	var resp Response
	select {
	case resp = <-done:
	case <-time.After(time.Second):
		t.Fatal("status waited for the in-flight capture lease")
	}
	if !resp.OK || resp.Cowork != nil || resp.Unavailable["cowork"] == "" || resp.Sync == nil || resp.Sync.Queued != 17 || resp.Sync.SpoolBytes != 1234 {
		t.Fatalf("partial status: %+v", resp)
	}
	if resp.Extraction == nil {
		t.Fatal("busy Cowork hid available extraction diagnostics")
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["cowork"]; ok {
		t.Fatal("busy Cowork emitted misleading zero counters")
	}
	f.a.captureScopeMu.Unlock()
	locked = false
	resp = ask(t, f.a, Request{Op: "status"})
	if !resp.OK || resp.Cowork == nil || resp.Cowork.Held != 1 || resp.Unavailable["cowork"] != "" {
		t.Fatalf("available status after lease release: %+v", resp)
	}
}

type blockedStatusSync struct{ statusSync }

func (s blockedStatusSync) StatusContext(ctx context.Context) (devicesync.Status, error) {
	<-ctx.Done()
	return s.st, ctx.Err()
}

func TestStatusBudgetPreservesHealthAndOmitsUnavailableDiagnostics(t *testing.T) {
	f := newFixture(t, "-")
	f.a.cfg.Sync = blockedStatusSync{statusSync{st: devicesync.Status{Queued: 19, ServerDown: true}}}
	start := time.Now()
	resp := ask(t, f.a, Request{Op: "status"})
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("status exceeded CLI headroom: %s", elapsed)
	}
	if !resp.OK || resp.Sync == nil || resp.Sync.Queued != 19 || !resp.Sync.ServerDown {
		t.Fatalf("lost sync health: %+v", resp)
	}
	if resp.Extraction != nil || resp.Cowork != nil || resp.Unavailable["redactions"] == "" || resp.Unavailable["extraction"] == "" || resp.Unavailable["cowork"] == "" {
		t.Fatalf("unavailable diagnostics: %+v", resp)
	}
}

// The status op reports the sync state, including sources that keep
// failing on their own (D15).
func TestStatusReportsFailingSources(t *testing.T) {
	f := newFixture(t, "-")
	want := devicesync.Status{Queued: 2, Failing: []devicesync.SourceError{{Path: "devin:x#s1", Error: "export failed", Attempts: 3}}}
	f.cfg.Sync = statusSync{f.rec, want}
	f.a = New(f.store, f.cfg)
	runCtx, cancel := context.WithCancel(ctx)
	sock := filepath.Join(shortTemp(t), "a.sock")
	done := make(chan error, 1)
	go func() { done <- f.a.Serve(runCtx, sock) }()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool { _, err := Call(ctx, sock, Request{Op: "ping"}); return err == nil })
	resp, err := Call(ctx, sock, Request{Op: "status"})
	if err != nil || resp.Sync == nil {
		t.Fatalf("status: %+v %v", resp, err)
	}
	if got := resp.Sync; got.Queued != 2 || len(got.Failing) != 1 || got.Failing[0].Path != "devin:x#s1" || got.Failing[0].Attempts != 3 {
		t.Fatalf("status %+v", got)
	}
}

type recheckSync struct {
	*recorder
	n *atomic.Int32
}

func (s recheckSync) Recheck() { s.n.Add(1) }

// `flopwire login` sends the repin op after saving a pin: the agent asks
// its scheduler to re-read the pin, so a stopped sync resumes without a
// restart.
func TestRepinOpRechecksSync(t *testing.T) {
	f := newFixture(t, "-")
	var n atomic.Int32
	f.cfg.Sync = recheckSync{f.rec, &n}
	f.a = New(f.store, f.cfg)
	runCtx, cancel := context.WithCancel(ctx)
	sock := filepath.Join(shortTemp(t), "a.sock")
	done := make(chan error, 1)
	go func() { done <- f.a.Serve(runCtx, sock) }()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool { _, err := Call(ctx, sock, Request{Op: "ping"}); return err == nil })
	if _, err := Call(ctx, sock, Request{Op: "repin"}); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 1 {
		t.Fatalf("%d rechecks, want 1", n.Load())
	}
}
