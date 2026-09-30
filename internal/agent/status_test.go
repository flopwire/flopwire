package agent

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/flopwire/flopwire/internal/devicesync"
)

type statusSync struct {
	*recorder
	st devicesync.Status
}

func (s statusSync) Status() devicesync.Status { return s.st }

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
