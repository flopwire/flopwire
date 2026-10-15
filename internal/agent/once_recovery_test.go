package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/fsprobe"
	"github.com/flopwire/flopwire/internal/localindex"
)

// Discovery of a deleted-worktree transcript must settle its placement before
// one-shot completion drains uploads. Keep recovery behind a mutex barrier so
// periodic daemon recovery cannot make the ordering pass accidentally.
func TestOnceCompletionWaitsForDeletedWorktreeRecovery(t *testing.T) {
	for _, delegated := range []bool{false, true} {
		name := "fresh"
		if delegated {
			name = "delegated"
		}
		t.Run(name, func(t *testing.T) {
			main, wt := repoWithWorktree(t)
			f, _, _ := rulesFixture(t, "-", "allow "+main)
			cwd := filepath.Join(wt, "src")
			path := f.deletedWorktreeSession(main, wt, false, claudeRecord(goneSid, cwd, `"gitBranch":"app-fix-agent",`, "synthetic recovered upload", 1))
			runCtx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var workers sync.WaitGroup
			if delegated {
				if err := f.a.load(runCtx); err != nil {
					t.Fatal(err)
				}
				f.a.startWorkers(runCtx, &workers)
				defer func() { cancel(); f.a.kick(); workers.Wait() }()
				f.a.markDiscovered()
			}
			drained := make(chan bool, 1)
			ack := make(chan struct{})
			f.a.cfg.Server = "https://synthetic.example"
			f.a.cfg.DeviceID = "device"
			f.a.cfg.DrainSync = func(ctx context.Context, _ time.Duration) error {
				_, offered := f.rec.spec(path)
				drained <- offered
				select {
				case <-ack:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			f.a.recoverMu.Lock()
			locked := true
			defer func() {
				if locked {
					f.a.recoverMu.Unlock()
				}
			}()
			result := make(chan error, 1)
			go func() {
				if delegated {
					result <- f.a.OncePass(runCtx, OnceRequest{Mode: localindex.ModeFull, Connection: "upload", Server: f.a.cfg.Server, DeviceID: "device", SyncTimeout: time.Second})
				} else {
					err := f.a.Once(runCtx)
					if err == nil {
						err = f.a.cfg.DrainSync(runCtx, time.Second)
					}
					result <- err
				}
			}()
			// Wait until the sweep actually discovered the unsettled placement.
			waitFor(t, func() bool {
				f.a.mu.Lock()
				defer f.a.mu.Unlock()
				_, ok := f.a.places[placeKey{agent: "claude", session: goneSid}]
				return ok
			})
			select {
			case offered := <-drained:
				t.Fatalf("drained before recovery; recovered transcript offered=%v", offered)
			case err := <-result:
				t.Fatalf("completed before recovery: %v", err)
			case <-time.After(40 * time.Millisecond):
			}
			f.a.recoverMu.Unlock()
			locked = false
			select {
			case offered := <-drained:
				if !offered {
					t.Fatal("upload drain omitted recovered transcript")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("did not reach upload drain after recovery")
			}
			select {
			case err := <-result:
				t.Fatalf("completed before upload acknowledgment: %v", err)
			default:
			}
			close(ack)
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			if got := f.placement(goneSid); got.main != main || got.how != localindex.PlacedByBranch {
				t.Fatalf("placement=%+v", got)
			}
		})
	}
}

// recoveryBarrierLog observes real sweep/recovery boundaries without adding
// production hooks. The wrapped handler keeps the fixture's usual logging.
type recoveryBarrierLog struct {
	slog.Handler
	handle func(slog.Record)
}

func (h recoveryBarrierLog) Enabled(context.Context, slog.Level) bool { return true }
func (h recoveryBarrierLog) Handle(ctx context.Context, r slog.Record) error {
	h.handle(r)
	return h.Handler.Handle(ctx, r)
}

// Run may win recovery while a delegated pass is finishing its first sweep.
// Block Run's subsequent discovery at the filesystem boundary: the delegated
// pass must supply its own second sweep even when recovery finds nothing new.
func TestOnceCompletionAfterBackgroundRecoveryWins(t *testing.T) {
	for _, fastLane := range []time.Duration{time.Hour, time.Millisecond} {
		t.Run(fastLane.String(), func(t *testing.T) { onceAfterBackgroundRecovery(t, fastLane) })
	}
}

func onceAfterBackgroundRecovery(t *testing.T, fastLane time.Duration) {
	main, wt := repoWithWorktree(t)
	f, _, _ := rulesFixture(t, "-", "allow "+main)
	for _, root := range []string{f.cfg.ClaudeProjects, f.cfg.CodexHome} {
		if err := os.RemoveAll(root); err != nil {
			t.Fatal(err)
		}
	}
	// No signal matches this removed worktree. Recovery settles checked_at
	// without calling repoChanged, so its second sweep is essential.
	path := f.deletedWorktreeSession(main, wt, true, claudeRecord(goneSid, filepath.Join(wt, "src"), "", "synthetic unmatched recovered upload", 1))
	runCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	startup := make(chan struct{})
	releaseRun := make(chan struct{})
	passReached := make(chan struct{})
	releasePass := make(chan struct{})
	daemonBlocked := make(chan struct{})
	releaseDaemon := make(chan struct{})
	testHookAfterFirstPass = func() {
		close(startup)
		select {
		case <-releaseRun:
		case <-runCtx.Done():
		}
	}
	defer func() { testHookAfterFirstPass = nil }()
	var sweeps atomic.Int32
	var recovered, blocked atomic.Bool
	f.a.log = slog.New(recoveryBarrierLog{Handler: f.a.log.Handler(), handle: func(r slog.Record) {
		if r.Message == "agent: sweep" && sweeps.Add(1) == 2 {
			close(passReached)
			select {
			case <-releasePass:
			case <-runCtx.Done():
			}
		}
		if r.Message == "agent: looked for the checkouts of deleted worktrees" {
			recovered.Store(true)
		}
	}})
	fsprobe.SetHook(func(op fsprobe.Op, path string) {
		if op == fsprobe.OpList && path == f.cfg.ClaudeProjects && recovered.Load() && blocked.CompareAndSwap(false, true) {
			close(daemonBlocked)
			select {
			case <-releaseDaemon:
			case <-runCtx.Done():
			}
		}
	})
	defer fsprobe.SetHook(nil)
	// The long interval proves completion without help from a periodic
	// fast lane; the short interval also exercises normal daemon pressure.
	f.a.cfg.FastLane = fastLane
	f.a.cfg.Sweep = time.Hour
	f.a.cfg.Server = "https://synthetic.example"
	f.a.cfg.DeviceID = "device"
	drained := make(chan bool, 1)
	f.a.cfg.DrainSync = func(context.Context, time.Duration) error { _, ok := f.rec.spec(path); drained <- ok; return nil }
	runDone := make(chan error, 1)
	go func() { runDone <- f.a.Run(runCtx) }()
	defer func() { cancel(); <-runDone }()
	select {
	case <-startup:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not discover transcript")
	}
	result := make(chan error, 1)
	go func() {
		result <- f.a.OncePass(runCtx, OnceRequest{Mode: localindex.ModeFull, Connection: "upload", Server: f.a.cfg.Server, DeviceID: "device", SyncTimeout: time.Second})
	}()
	select {
	case <-passReached:
	case <-time.After(5 * time.Second):
		t.Fatal("pass did not finish discovery")
	}
	f.a.WaitIdle()
	close(releaseRun)
	select {
	case <-daemonBlocked:
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not finish background recovery and start another sweep")
	}
	// Recovery's capture lock spans enforcement; wait for that too.
	f.a.captureScopeMu.Lock()
	f.a.captureScopeMu.Unlock()
	close(releasePass)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("delegated pass did not complete")
	}
	if !<-drained {
		t.Fatal("delegated drain omitted transcript settled by background recovery")
	}
	if p := f.placement(goneSid); p.checked == 0 || p.main != "" {
		t.Fatalf("expected settled unmatched placement, got %+v", p)
	}
	close(releaseDaemon)
}
