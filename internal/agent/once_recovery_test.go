package agent

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

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
