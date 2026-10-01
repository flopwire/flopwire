package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/devicebus"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/domain"
	"github.com/flopwire/flopwire/internal/localindex"
	"golang.org/x/sys/unix"
)

func TestHookSource(t *testing.T) {
	for _, tc := range []struct{ in, path, session string }{
		// Claude Code hooks (Stop, PostToolUse) on stdin.
		{`{"session_id":"abc","transcript_path":"/h/.claude/projects/p/abc.jsonl","hook_event_name":"Stop"}`, "/h/.claude/projects/p/abc.jsonl", "abc"},
		// Codex notify passes a JSON argument naming the thread.
		{`{"type":"agent-turn-complete","thread-id":"019a-t","turn-id":"1"}`, "", "019a-t"},
		{`not json`, "", ""},
	} {
		p, s := hookSource([]byte(tc.in))
		if p != tc.path || s != tc.session {
			t.Errorf("%s: got %q %q", tc.in, p, s)
		}
	}
}

// A hook never fails because the agent is not running.
func TestAgentFlushWithoutAgent(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "none.sock")
	err := agentFlush(context.Background(), []string{"--socket", sock}, strings.NewReader(`{"transcript_path":"/x.jsonl"}`))
	if err != nil {
		t.Fatalf("flush with no agent: %v", err)
	}
	if err := agentFlush(context.Background(), []string{"--socket", sock}, strings.NewReader(``)); err == nil {
		t.Fatal("flush with no source should fail")
	}
	_ = os.Remove(sock)
}

// A hook never fails because the agent is busy or went away mid-call
// (found by the two-device e2e, scenario d: a hook flush of a 72MB
// transcript timed out, and one cut off by a crash returned EOF). An
// explicit --path still reports it.
func TestAgentFlushAgentGoneMidCall(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "a.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			bufio.NewReader(c).ReadBytes('\n')
			c.Close() // the agent dies before it answers
		}
	}()
	hook := strings.NewReader(`{"transcript_path":"/x.jsonl","hook_event_name":"Stop"}`)
	if err := agentFlush(context.Background(), []string{"--socket", sock}, hook); err != nil {
		t.Fatalf("hook flush: %v", err)
	}
	if err := agentFlush(context.Background(), []string{"--socket", sock, "--path", "/x.jsonl"}, strings.NewReader("")); err == nil {
		t.Fatal("explicit flush hid the error")
	}
}

// D12: one indexer per index. A second daemon fails fast naming the
// holder; --once asks the running agent for a pass over the control socket
// and waits for it; with no agent answering it waits for the lock.
func TestAgentOneWriter(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(dir, "index.db")
	sock := filepath.Join(dir, "a.sock")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	held, err := localindex.Open(db, localindex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = openAgentIndex(ctx, db, localindex.Options{}, false, sock, log)
	if want := fmt.Sprintf("agent already running (pid %d)", os.Getpid()); err == nil || err.Error() != want {
		t.Fatalf("second daemon: %v, want %q", err, want)
	}

	// --once with a running agent: a pass request for this index.
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan agent.Request, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		line, _ := bufio.NewReader(c).ReadBytes('\n')
		var req agent.Request
		json.Unmarshal(line, &req)
		got <- req
		c.Write([]byte(`{"ok":true}` + "\n"))
		c.Close()
	}()
	s, err := openAgentIndex(ctx, db, localindex.Options{}, true, sock, log)
	if err != nil || s != nil {
		t.Fatalf("--once with an agent: %v %v", s, err)
	}
	if req := <-got; req.Op != "pass" || req.Index != db {
		t.Fatalf("request %+v", req)
	}
	ln.Close()
	os.Remove(sock)

	// --once while the holder does not answer: wait until it exits.
	time.AfterFunc(300*time.Millisecond, func() { held.Close() })
	s, err = openAgentIndex(ctx, db, localindex.Options{}, true, sock, log)
	if err != nil || s == nil {
		t.Fatalf("--once after the holder exits: %v", err)
	}
	s.Close()
}

// A permanent sync error (TLS pin mismatch) shows in `agent status`.
func TestAgentStatusShowsStoppedSync(t *testing.T) {
	var b strings.Builder
	printAgentStatus(&b, agent.Response{Sync: &devicesync.Status{Stopped: "server certificate sha256:ab does not match the pinned fingerprint", Queued: 3}})
	if out := b.String(); !strings.Contains(out, "sync: stopped until the server is re-pinned (flopwire login --fingerprint): server certificate sha256:ab") {
		t.Fatalf("status output:\n%s", out)
	}
}

// D18: after a new deny, `agent status` says that server copies stay.
func TestAgentStatusShowsServerCopies(t *testing.T) {
	var b strings.Builder
	printAgentStatus(&b, agent.Response{Sync: &devicesync.Status{}, ServerCopies: &agent.ServerCopies{Count: 3, Sessions: []string{"claude:s1 (user rule \"deny /x\")"}, At: time.Now()}})
	out := b.String()
	for _, want := range []string{"removed 3 sessions", "stay on the server", "claude:s1", "and 2 more"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

// D18 on the server: sources it refused by an admin rule show in status.
func TestAgentStatusShowsServerRefusals(t *testing.T) {
	var b strings.Builder
	printAgentStatus(&b, agent.Response{Sync: &devicesync.Status{RefusedCount: 2,
		Refused: []devicesync.SourceRefusal{{Path: "/h/.claude/projects/-w/s.jsonl", Rule: "~/clients/acme"}}}})
	out := b.String()
	for _, want := range []string{"server refused 2 sources by admin path rule", "/h/.claude/projects/-w/s.jsonl", "rule: ~/clients/acme", "and 1 more"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

// The message bus state shows in `agent status`, with the inbox counts.
func TestAgentStatusShowsBus(t *testing.T) {
	for _, c := range []struct {
		st   devicebus.Status
		want []string
	}{
		{devicebus.Status{State: devicebus.StateConnected, Sessions: 3, Pending: 2, Unacked: 1, Held: 4},
			[]string{"messaging: connected; 3 live sessions reported", "messages: 2 pending delivery, 1 receipts unsent, 4 held for your acceptance"}},
		{devicebus.Status{State: devicebus.StateBackoff, LastError: "connection refused", RetryAt: time.Now()},
			[]string{"messaging: server unreachable, retry at", "connection refused"}},
		{devicebus.Status{State: devicebus.StateStopped, LastError: "the server refused this device's credential: run flopwire login"},
			[]string{"messaging: stopped: the server refused this device's credential"}},
		{devicebus.Status{State: devicebus.StateLocal, Sessions: 2}, []string{"messaging: local (no server: between this device's sessions); 2 live sessions"}},
	} {
		var b strings.Builder
		st := c.st
		printAgentStatus(&b, agent.Response{Bus: &st})
		for _, want := range c.want {
			if !strings.Contains(b.String(), want) {
				t.Fatalf("missing %q in:\n%s", want, b.String())
			}
		}
	}
}

func TestAgentStatusShowsPlacements(t *testing.T) {
	var b strings.Builder
	printAgentStatus(&b, agent.Response{Placements: map[string]int{localindex.PlacedByCwd: 12, localindex.PlacedByBranch: 3, localindex.PlacedByNone: 1}})
	out := b.String()
	for _, want := range []string{"sessions placed by", "cwd           12", "branch        3", "unplaceable   1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Index(out, "cwd ") > strings.Index(out, "branch ") {
		t.Fatalf("methods out of order:\n%s", out)
	}
}

// The lock descriptor passed across the re-exec gets close-on-exec back,
// so no child process inherits the index lock. The descriptor is a raw one,
// as after an exec: inheritedLock's *os.File is its only owner. (A second
// *os.File on the same number closed it twice, and the second close hit a
// descriptor another test had opened meanwhile.)
func TestInheritedLockIsCloseOnExec(t *testing.T) {
	fd, err := unix.Open(filepath.Join(t.TempDir(), "index.db.lock"), unix.O_RDWR|unix.O_CREAT, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envLockFD, strconv.Itoa(fd))
	got := inheritedLock(filepath.Join(t.TempDir(), "index.db"))
	if got == nil {
		unix.Close(fd)
		t.Fatal("no inherited lock")
	}
	flags, err := unix.FcntlInt(got.Fd(), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("inherited lock fd has close-on-exec cleared")
	}
	if os.Getenv(envLockFD) != "" {
		t.Fatal("env not cleared")
	}
	got.Close()
	// Nothing else owns the number: a finalizer run now must not close a
	// descriptor opened after it.
	other, err := os.Create(filepath.Join(t.TempDir(), "other"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	runtime.GC()
	runtime.GC()
	time.Sleep(20 * time.Millisecond)
	if _, err := other.Write([]byte("x")); err != nil {
		t.Fatalf("a later descriptor was closed under us: %v", err)
	}
}

// The agent's Repin re-reads the saved pin and token: a new pin or token
// for the same server is installed in the sync client; the same pin and
// token, or another server, is not.
func TestSyncRepinReadsSavedPin(t *testing.T) {
	oldPin, newPin := client.FingerprintPrefix+strings.Repeat("11", 32), client.FingerprintPrefix+strings.Repeat("22", 32)
	cfg := client.Config{Server: "https://flopwire.test:8443", Token: "dev", TLSFingerprint: oldPin}
	saved := cfg
	tr := newSyncTransport(cfg, func() (client.Config, error) { return saved, nil }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if tr.repin() {
		t.Fatal("repinned with an unchanged pin")
	}
	saved.Server, saved.TLSFingerprint = "https://other.test", newPin
	if tr.repin() {
		t.Fatal("repinned to another server")
	}
	before := tr.current().HTTP
	saved.Server = "https://flopwire.test:8443/"
	if !tr.repin() || tr.current().HTTP == before {
		t.Fatal("a new pin for the same server was not installed")
	}
	if tr.repin() {
		t.Fatal("repinned twice for one change")
	}
	saved.Token = "after-login"
	if !tr.repin() || tr.current().Token != "after-login" {
		t.Fatal("a new token for the same server was not installed")
	}
}

func TestAgentStatusShowsRedactions(t *testing.T) {
	var b strings.Builder
	printAgentStatus(&b, agent.Response{Sync: &devicesync.Status{Redactions: map[string]int64{"github-token": 2, "assignment": 5}, RedactedSources: 3}})
	out := b.String()
	if !strings.Contains(out, "redacted before upload: 7 secrets in 3 sources") || strings.Index(out, "assignment") > strings.Index(out, "github-token") {
		t.Fatalf("status:\n%s", out)
	}
	b.Reset()
	printAgentStatus(&b, agent.Response{Sync: &devicesync.Status{}})
	if !strings.Contains(b.String(), "redacted before upload: nothing so far") {
		t.Fatalf("status:\n%s", b.String())
	}
}

func TestWriteHidden(t *testing.T) {
	var b strings.Builder
	writeHidden(&b, domain.HiddenSummary{Total: 3, PurgeAfter: "168h0m0s",
		ByRule: []domain.HiddenCount{{Rule: "~/clients/acme", Sessions: 3}}, ByUser: []domain.HiddenCount{{Email: "a@x.test", Sessions: 3}}})
	out := b.String()
	for _, want := range []string{"3 sessions hidden", "~/clients/acme", "a@x.test", "168h"} {
		if !strings.Contains(out, want) {
			t.Errorf("preview %q lacks %q", out, want)
		}
	}
}

// A local inbox that cannot be opened turns messaging off; the agent
// still runs (indexing and upload never wait on messaging).
func TestAgentRunsWithDamagedInbox(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(dir, "config.json"))
	t.Setenv("FLOPWIRE_INDEX", filepath.Join(t.TempDir(), "index.db"))
	if err := os.WriteFile(filepath.Join(dir, "bus.db"), []byte(strings.Repeat("not a database ", 16)), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	sock := filepath.Join(shortSockDir(t), "a.sock")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := runAgent(ctx, []string{"--socket", sock, "--claude-projects", empty, "--codex-home", empty, "--devin-db", "-", "--no-sync"})
		done <- err
	}()
	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case err := <-done:
			cancel()
			t.Fatalf("agent stopped: %v", err)
		default:
		}
		if r, err := agent.Call(ctx, sock, agent.Request{Op: "status"}); err == nil {
			if r.Bus != nil {
				t.Fatalf("messaging on with a damaged inbox: %+v", r.Bus)
			}
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("agent never answered")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
}

// shortSockDir is a directory short enough for a unix socket path.
func shortSockDir(t *testing.T) string {
	d, err := os.MkdirTemp("/tmp", "fws")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}
