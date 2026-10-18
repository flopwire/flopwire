package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/localindex"
)

// Unlike scheduler-injection tests, this follows a native transcript through
// discovery, placement authorization, capture, and acknowledgment. Only the
// remote transport is synthetic; no scheduler filters are disabled.
func TestOnceDiscoveredUploadCompletionParity(t *testing.T) {
	for _, minimal := range []bool{false, true} {
		for _, delegated := range []bool{false, true} {
			t.Run(fmt.Sprintf("minimal=%v/delegated=%v", minimal, delegated), func(t *testing.T) {
				dir := t.TempDir()
				log := slog.New(slog.NewTextHandler(io.Discard, nil))
				st, err := localindex.Open(filepath.Join(dir, "index.db"), localindex.Options{SyncOnly: minimal, DeferCommit: true})
				if err != nil {
					t.Fatal(err)
				}
				defer st.Close()
				ss, err := devicesync.OpenStore(filepath.Join(dir, "sync.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer ss.Close()
				spool, err := devicesync.OpenSpool(filepath.Join(dir, "spool"), 1<<20)
				if err != nil {
					t.Fatal(err)
				}
				tr := &onceTransport{entered: make(chan struct{}, 1), release: make(chan struct{})}
				sy, err := devicesync.NewSyncer(devicesync.Config{Logger: log, SealAfter: -1}, ss, spool, tr)
				if err != nil {
					t.Fatal(err)
				}
				defer sy.Close()
				sc := devicesync.NewScheduler(sy, devicesync.SchedulerConfig{})
				root := filepath.Join(dir, "claude")
				project := filepath.Join(root, "synthetic")
				if err := os.MkdirAll(project, 0700); err != nil {
					t.Fatal(err)
				}
				sid := "0b7e2c1a-0000-4000-8000-000000000901"
				body := fmt.Sprintf(`{"type":"user","cwd":%q,"sessionId":%q,"message":{"role":"user","content":"synthetic upload parity"},"uuid":"c2000000-0000-4000-8000-000000000901","timestamp":"2026-09-23T11:00:00.000Z"}`+"\n", dir, sid)
				if err := os.WriteFile(filepath.Join(project, sid+".jsonl"), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
				cfg := agent.Config{ClaudeProjects: root, CodexHome: filepath.Join(dir, "codex"), DesktopCodeRoot: "-", CoworkRoot: "-", DevinDB: "-", OpencodeDB: "-", Sync: sc, Server: "https://synthetic.example", DeviceID: "device", Logger: log, Sweep: time.Hour, FastLane: time.Hour}
				cfg.DrainSync = func(ctx context.Context, d time.Duration) error { return drainSync(ctx, sc, d, log) }
				a := agent.New(st, cfg)
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 2)
				go func() { done <- sc.Run(ctx) }()
				processes := 1
				if delegated {
					processes++
					go func() { done <- a.Run(ctx) }()
				}
				defer func() {
					cancel()
					for range processes {
						<-done
					}
				}()
				mode := localindex.ModeFull
				if minimal {
					mode = localindex.ModeSyncOnly
				}
				invoke := func(wait time.Duration) error {
					if delegated {
						return a.OncePass(ctx, agent.OnceRequest{Mode: mode, Connection: "upload", Server: cfg.Server, DeviceID: cfg.DeviceID, SyncTimeout: wait})
					}
					if err := a.Once(ctx); err != nil {
						return err
					}
					return drainSync(ctx, sc, wait, log)
				}
				result := make(chan error, 1)
				go func() { result <- invoke(3 * time.Second) }()
				select {
				case <-tr.entered:
				case err := <-result:
					t.Fatalf("completed before discovered upload: %v", err)
				case <-time.After(5 * time.Second):
					t.Fatal("discovered transcript never reached transport")
				}
				if err := <-result; err == nil || !strings.Contains(err.Error(), "upload not finished after 3s") {
					t.Fatalf("blocked upload result=%v", err)
				}
				go func() { result <- invoke(5 * time.Second) }()
				select {
				case err := <-result:
					t.Fatalf("returned before acknowledgment: %v", err)
				case <-time.After(40 * time.Millisecond):
				}
				close(tr.release)
				select {
				case err := <-result:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("did not complete after acknowledgment")
				}
				var rows int
				if err := st.DB().QueryRow("SELECT count(*) FROM messages").Scan(&rows); err != nil {
					t.Fatal(err)
				}
				if minimal && rows != 0 || !minimal && rows == 0 {
					t.Fatalf("minimal=%v retained rows=%d", minimal, rows)
				}
			})
		}
	}
}
