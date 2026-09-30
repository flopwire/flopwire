package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// D12: `agent run --once` against a running agent asks it for a pass over
// the control socket; the pass indexes what changed before it answers.
func TestControlPass(t *testing.T) {
	f := newFixture(t, "-")
	f.cfg.Sweep, f.cfg.FastLane = time.Hour, time.Hour // only the pass can index
	f.a = New(f.store, f.cfg)
	runCtx, cancel := context.WithCancel(ctx)
	sock := filepath.Join(shortTemp(t), "a.sock")
	done := make(chan error, 2)
	go func() { done <- f.a.Run(runCtx) }()
	go func() { done <- f.a.Serve(runCtx, sock) }()
	defer func() {
		cancel()
		<-done
		<-done
	}()
	waitFor(t, func() bool { _, err := Call(ctx, sock, Request{Op: "ping"}); return err == nil })
	f.a.WaitIdle()

	appendFile(t, f.path(alphaRel), claudeUser("c1000000-0000-4000-8000-0000000000af", "pass indexed pangolin"))
	if _, err := Call(ctx, sock, Request{Op: "pass", Index: f.store.Path()}); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(f.find("pass indexed pangolin", false)) != 1 {
		t.Error("line not indexed when the pass returned")
	}
	if _, err := Call(ctx, sock, Request{Op: "pass", Index: filepath.Join(t.TempDir(), "other.db")}); err == nil {
		t.Error("pass for another index accepted")
	}
}

// A pass (pollDevin with wait) waits for a Devin poll already running,
// then polls itself, so `agent run --once` returns with the store indexed.
// Without wait the poll is skipped.
func TestPassWaitsForDevinPoll(t *testing.T) {
	path, _ := buildDevin(t)
	f := newFixture(t, path)
	f.a.devin.mu.Lock() // a poll in flight
	f.a.pollDevin(ctx, true, false)
	if n := f.a.stats.DevinPolls.Load(); n != 0 {
		t.Fatalf("a poll ran while another held the lock (%d)", n)
	}
	done := make(chan struct{})
	go func() { f.a.pollDevin(ctx, true, true); close(done) }()
	select {
	case <-done:
		t.Fatal("the waiting poll returned while another was running")
	case <-time.After(200 * time.Millisecond):
	}
	f.a.devin.mu.Unlock()
	<-done
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT count(*) FROM conversations WHERE agent = 'devin'`); n == 0 {
		t.Fatal("Devin not indexed when the waiting poll returned")
	}
}

// `flopwire redact` asks the running agent to mask a message in its index.
func TestControlRedact(t *testing.T) {
	f := newFixture(t, "-")
	f.cfg.Sweep, f.cfg.FastLane = time.Hour, time.Hour
	f.a = New(f.store, f.cfg)
	runCtx, cancel := context.WithCancel(ctx)
	sock := filepath.Join(shortTemp(t), "a.sock")
	done := make(chan error, 2)
	go func() { done <- f.a.Run(runCtx) }()
	go func() { done <- f.a.Serve(runCtx, sock) }()
	defer func() {
		cancel()
		<-done
		<-done
	}()
	waitFor(t, func() bool { _, err := Call(ctx, sock, Request{Op: "ping"}); return err == nil })
	appendFile(t, f.path(alphaRel), claudeUser("c1000000-0000-4000-8000-0000000000b1", "keep this\nsecret codename BLUEFALCON-9"))
	if _, err := Call(ctx, sock, Request{Op: "pass", Index: f.store.Path()}); err != nil {
		t.Fatal(err)
	}
	var session string
	var ordinal int64
	if err := f.store.DB().QueryRow(`SELECT c.session_id, m.ordinal FROM messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE m.native_id LIKE 'c1000000-0000-4000-8000-0000000000b1%'`).Scan(&session, &ordinal); err != nil {
		t.Fatal(err)
	}
	if _, err := Call(ctx, sock, Request{Op: "redact", Address: fmt.Sprintf("%s/%d:2-2", session[:8], ordinal)}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("an ambiguous session prefix: %v", err)
	}
	resp, err := Call(ctx, sock, Request{Op: "redact", Address: fmt.Sprintf("%s/%d:2-2", session, ordinal)})
	if err != nil || resp.Redacted != 1 {
		t.Fatalf("redact: %+v %v", resp, err)
	}
	if len(f.find("BLUEFALCON", false)) != 0 || len(f.find("keep this", false)) != 1 {
		t.Fatal("local index not redacted as asked")
	}
	if _, err := Call(ctx, sock, Request{Op: "redact", Address: "nosuchsession/1"}); err == nil {
		t.Fatal("unknown address accepted")
	}
}
