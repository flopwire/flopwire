package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/syncproto"
)

type oncePermanentError struct{}

func (oncePermanentError) Error() string   { return "synthetic pin rejection" }
func (oncePermanentError) Permanent() bool { return true }

type onceTransport struct {
	fail    atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (tr *onceTransport) Has(_ context.Context, h []syncproto.Hash) ([]syncproto.Hash, error) {
	return h, nil
}
func (tr *onceTransport) Flush(ctx context.Context, r *syncproto.FlushRequest) (*syncproto.FlushResponse, error) {
	select {
	case tr.entered <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-tr.release:
	}
	if tr.fail.Load() {
		return nil, oncePermanentError{}
	}
	io.Copy(io.Discard, r.Payload)
	// Fixtures fit in a provisional tail, with no finalized entries.
	resp := &syncproto.FlushResponse{Version: syncproto.Version, Status: syncproto.StatusOK, Generation: r.Header.Generation, TailAcked: true}
	if r.Header.Tail != nil {
		resp.TailOffset = r.Header.Tail.Offset
		resp.TailSize = r.Header.Tail.Size
	}
	return resp, nil
}

// A real daemon and socket must keep delegated completion behind the actual
// scheduler's acknowledgment, under either retention mode. The transport alone
// is synthetic; it gates the server response without a network or Docker.
func TestDelegatedOnceCompletion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		minimal bool
		device  string
	}{{"full", false, "device-a"}, {"sync-only", true, "device-a"}, {"minted-token", false, ""}} {
		t.Run(tc.name, func(t *testing.T) {
			minimal := tc.minimal
			dir := t.TempDir()
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			st, err := localindex.Open(filepath.Join(dir, "index.db"), localindex.Options{DeferCommit: true, SyncOnly: minimal})
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
			cfg := agent.Config{ClaudeProjects: filepath.Join(dir, "claude"), CodexHome: filepath.Join(dir, "codex"), DesktopCodeRoot: "-", CoworkRoot: "-", DevinDB: "-", OpencodeDB: "-", Sync: sc, Server: "https://server.example", DeviceID: tc.device, Logger: log, Sweep: time.Hour, FastLane: time.Hour}
			if tc.device == "" {
				cfg.SyncTokenFingerprint = fmt.Sprintf("%x", sha256.Sum256([]byte("synthetic-token")))
			}
			drainStarted := make(chan struct{}, 4)
			drainCancelled := make(chan struct{}, 1)
			cfg.DrainSync = func(ctx context.Context, d time.Duration) error {
				drainStarted <- struct{}{}
				err := drainSync(ctx, sc, d, log)
				if errors.Is(err, context.Canceled) {
					drainCancelled <- struct{}{}
				}
				return err
			}
			a := agent.New(st, cfg)
			// This scheduler fixture queues a synthetic source independently of
			// discovery; policy filtering is covered by the agent tests.
			sc.SetFilter(nil)
			sc.SetBound(nil)
			sc.SetAuthorize(nil)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			socketDir, err := os.MkdirTemp("/tmp", "fw-once-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(socketDir)
			sock := filepath.Join(socketDir, "a.sock")
			done := make(chan error, 3)
			go func() { done <- a.Run(ctx) }()
			go func() { done <- a.Serve(ctx, sock) }()
			go func() { done <- sc.Run(ctx) }()
			defer func() {
				cancel()
				for range 3 {
					<-done
				}
			}()
			startup, startupCancel := context.WithTimeout(ctx, 3*time.Second)
			defer startupCancel()
			for {
				c, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
				_, err := agent.Call(c, sock, agent.Request{Op: "ping"})
				cancel()
				if err == nil {
					break
				}
				select {
				case <-startup.Done():
					t.Fatal(startup.Err())
				case <-time.After(time.Millisecond):
				}
			}
			t.Setenv("FLOPWIRE_CONFIG", filepath.Join(dir, "config.json"))
			t.Setenv(client.EnvToken, "")
			if err := client.Save(client.Config{Server: cfg.Server, Token: "synthetic-token", DeviceID: cfg.DeviceID}); err != nil {
				t.Fatal(err)
			}
			callCLI := func(extra ...string) error {
				args := []string{"--once", "--db", st.Path(), "--socket", sock, "--mem-limit=0"}
				args = append(args, extra...)
				_, err := runAgent(ctx, args)
				return err
			}
			if err := callCLI("--sync-only=" + map[bool]string{true: "false", false: "true"}[minimal]); err == nil {
				t.Fatal("CLI accepted conflicting retention")
			}
			if err := callCLI("--no-sync", "--sync-only=false"); err == nil {
				t.Fatal("CLI accepted --no-sync against upload daemon")
			}
			contract := agent.OnceRequest{Mode: map[bool]string{false: localindex.ModeFull, true: localindex.ModeSyncOnly}[minimal], Connection: "upload", Server: cfg.Server, DeviceID: cfg.DeviceID, TokenFingerprint: cfg.SyncTokenFingerprint, SyncTimeout: 30 * time.Millisecond}
			call := func(c context.Context, r agent.OnceRequest) error {
				_, err := openAgentIndex(c, st.Path(), localindex.Options{SyncOnly: minimal}, true, sock, log, r)
				return err
			}
			for _, bad := range []agent.OnceRequest{
				{Mode: map[bool]string{true: localindex.ModeFull, false: localindex.ModeSyncOnly}[minimal], Connection: "upload"},
				{Mode: contract.Mode, Connection: "local"},
				{Mode: contract.Mode, Connection: "upload", Server: "https://other.example", DeviceID: cfg.DeviceID},
				{Mode: contract.Mode, Connection: "upload", Server: cfg.Server, DeviceID: "device-b"},
			} {
				if err := call(ctx, bad); err == nil {
					t.Fatalf("accepted incompatible contract: %+v", bad)
				}
			}
			if tc.device == "" {
				bad := contract
				bad.TokenFingerprint = fmt.Sprintf("%x", sha256.Sum256([]byte("other-token")))
				if err := call(ctx, bad); err == nil || !strings.Contains(err.Error(), "different credential") {
					t.Fatalf("mismatched token: %v", err)
				}
				if err := client.Save(client.Config{Server: cfg.Server, Token: "other-token"}); err != nil {
					t.Fatal(err)
				}
				if err := callCLI(); err == nil || !strings.Contains(err.Error(), "different credential") {
					t.Fatalf("CLI mismatched token: %v", err)
				}
				if err := client.Save(client.Config{Server: cfg.Server, Token: "synthetic-token"}); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(dir, "pending.jsonl")
			if err := os.WriteFile(path, []byte("synthetic pending upload\n"), 0600); err != nil {
				t.Fatal(err)
			}
			sc.Flush(devicesync.SourceSpec{Path: path})
			select {
			case <-tr.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("scheduler did not reach transport")
			}
			if err := callCLI("--sync-only="+map[bool]string{true: "true", false: "false"}[minimal], "--sync-timeout=30ms"); err == nil || !strings.Contains(err.Error(), "upload not finished after 30ms") {
				t.Fatalf("blocked completion: %v", err)
			}
			<-drainStarted
			c, stop := context.WithCancel(ctx)
			result := make(chan error, 1)
			contract.SyncTimeout = time.Hour
			go func() { result <- call(c, contract) }()
			<-drainStarted
			stop()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("call ignored cancellation")
			}
			select {
			case <-drainCancelled:
			case <-time.After(time.Second):
				t.Fatal("daemon did not cancel drain after client left")
			}
			result = make(chan error, 1)
			go func() { result <- call(ctx, contract) }()
			<-drainStarted
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
			tr.fail.Store(true)
			if err := os.WriteFile(path, []byte("synthetic changed upload\n"), 0600); err != nil {
				t.Fatal(err)
			}
			sc.Flush(devicesync.SourceSpec{Path: path})
			until := time.Now().Add(3 * time.Second)
			for sc.Progress().Stopped == "" {
				if time.Now().After(until) {
					t.Fatal("scheduler did not report permanent rejection")
				}
				time.Sleep(time.Millisecond)
			}
			if err := call(ctx, contract); err == nil || !strings.Contains(err.Error(), "upload stopped: synthetic pin rejection") {
				t.Fatalf("delegated permanent failure: %v", err)
			}
			if err := drainSync(ctx, sc, contract.SyncTimeout, log); err == nil || !strings.Contains(err.Error(), "upload stopped: synthetic pin rejection") {
				t.Fatalf("fresh permanent failure: %v", err)
			}

		})
	}
}
