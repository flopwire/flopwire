package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/devicebus"
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

// Without a server, the control socket routes between the device's own
// sessions: send, pending (each message once), peers, inbox, held, and the
// bus in status.
func TestControlBusLocal(t *testing.T) {
	f := newFixture(t, "-")
	f.cfg.Sweep, f.cfg.FastLane = time.Hour, time.Hour
	b, err := devicebus.Open(filepath.Join(t.TempDir(), "bus.db"), devicebus.Config{User: "gary", Logger: f.cfg.Logger})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	f.cfg.Bus = b
	f.a = New(f.store, f.cfg)
	f.once()
	f.a.now = func() time.Time { return f.alphaLast().Add(5 * time.Minute) }
	live, err := f.a.BusPresence(ctx)
	if err != nil || len(live) < 2 {
		t.Fatalf("presence: %d sessions, %v", len(live), err)
	}
	from, to := live[0], live[1]

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

	resp, err := Call(ctx, sock, Request{Op: "send", Send: &busproto.SendRequest{FromSession: from.SessionID, To: to.SessionID, Body: "local hello", Intent: "request"}})
	if err != nil || resp.Sent == nil || resp.Sent.State != busproto.StateQueued || resp.Sent.Sender != busproto.SenderOwn {
		t.Fatalf("send: %+v %v", resp.Sent, err)
	}
	// A refusal comes back as a *busproto.Error with its code.
	_, err = Call(ctx, sock, Request{Op: "send", Send: &busproto.SendRequest{FromSession: from.SessionID, To: to.SessionID, Body: "local hello"}})
	var be *busproto.Error
	if !errors.As(err, &be) || be.Code != busproto.CodeDuplicate || be.MessageID == "" {
		t.Fatalf("duplicate over the socket: %v", err)
	}
	// Several hooks of the recipient ask at once: one gets the message.
	got := make(chan []busproto.Envelope, 4)
	for range 4 {
		go func() {
			r, err := Call(ctx, sock, Request{Op: "pending", Session: to.SessionID})
			if err != nil {
				t.Error(err)
			}
			got <- r.Messages
		}()
	}
	n := 0
	for range 4 {
		for _, e := range <-got {
			if e.Body != "local hello" || e.From != from.SessionID || e.User != "gary" {
				t.Errorf("envelope %+v", e)
			}
			n++
		}
	}
	if n != 1 {
		t.Fatalf("delivered %d times", n)
	}
	r, err := Call(ctx, sock, Request{Op: "peers", Peers: &busproto.PeersQuery{Session: from.SessionID}})
	if err != nil || r.Peers == nil || slices.ContainsFunc(r.Peers.Peers, func(p busproto.Peer) bool { return p.Session == from.SessionID }) || len(r.Peers.Peers) == 0 {
		t.Fatalf("peers: %+v %v", r.Peers, err)
	}
	r, err = Call(ctx, sock, Request{Op: "inbox", Inbox: &busproto.InboxQuery{Session: from.SessionID}})
	if err != nil || r.Inbox == nil || len(r.Inbox.Messages) != 2 {
		t.Fatalf("inbox: %+v %v", r.Inbox, err)
	}
	if r, err := Call(ctx, sock, Request{Op: "held"}); err != nil || len(r.Held) != 0 {
		t.Fatalf("held: %+v %v", r, err)
	}
	r, err = Call(ctx, sock, Request{Op: "status"})
	if err != nil || r.Bus == nil || r.Bus.State != devicebus.StateLocal {
		t.Fatalf("status: %+v %v", r.Bus, err)
	}
	if _, err := Call(ctx, sock, Request{Op: "send"}); err == nil {
		t.Fatal("send without a request accepted")
	}
}

// A hook that gave up (its 200 ms budget ran out) before the agent
// answered pending never prints the messages. The agent finds the closed
// connection when it answers and queues them again, so the session's next
// hook gets them; they are not lost.
func TestControlPendingRequeuedWhenTheHookIsGone(t *testing.T) {
	f := newFixture(t, "-")
	b, err := devicebus.Open(filepath.Join(t.TempDir(), "bus.db"), devicebus.Config{User: "gary", Logger: f.cfg.Logger})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	f.cfg.Bus = b
	f.a = New(f.store, f.cfg)
	live := []devicebus.Session{
		{PresenceSession: busproto.PresenceSession{SessionID: "from-1111", Agent: "claude", Repo: "/src/api"}, LastActive: time.Now()},
		{PresenceSession: busproto.PresenceSession{SessionID: "to-2222", Agent: "claude", Repo: "/src/api"}, LastActive: time.Now()},
	}
	b.SetSources(func(context.Context) ([]devicebus.Session, error) { return live, nil }, nil)
	if _, err := b.Send(ctx, busproto.SendRequest{FromSession: "from-1111", To: "to-2222", Body: "hello"}); err != nil {
		t.Fatal(err)
	}

	hook, agentEnd := net.Pipe()
	done := make(chan struct{})
	go func() { f.a.serveConn(ctx, agentEnd); close(done) }()
	if _, err := hook.Write([]byte(`{"op":"pending","session":"to-2222"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	hook.Close() // the hook's deadline passed: it exits without reading
	<-done

	got, err := b.Pending(ctx, "to-2222", "")
	if err != nil || len(got) != 1 || got[0].Body != "hello" {
		t.Fatalf("message after the hook gave up: %+v %v", got, err)
	}
}
