package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/busrender"
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

// busFixture is an agent with a local bus and two live sessions.
func busFixture(t *testing.T) (*fixture, *devicebus.Bus) {
	t.Helper()
	f := newFixture(t, "-")
	b, err := devicebus.Open(filepath.Join(t.TempDir(), "bus.db"), devicebus.Config{User: "gary", Logger: f.cfg.Logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	f.cfg.Bus = b
	f.a = New(f.store, f.cfg)
	live := []devicebus.Session{
		{PresenceSession: busproto.PresenceSession{SessionID: "from-1111", Agent: "claude", Repo: "/src/api"}, LastActive: time.Now()},
		{PresenceSession: busproto.PresenceSession{SessionID: "to-2222", Agent: "claude", Repo: "/src/api"}, LastActive: time.Now()},
	}
	b.SetSources(func(context.Context) ([]devicebus.Session, error) { return live, nil }, nil)
	return f, b
}

// ask sends one request over a pipe and returns the answer.
func ask(t *testing.T, a *Agent, req Request) Response {
	t.Helper()
	hook, agentEnd := net.Pipe()
	go a.serveConn(ctx, agentEnd)
	defer hook.Close()
	b, _ := json.Marshal(req)
	if _, err := hook.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(hook).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// hookAsk is what a hook does: ask for pending messages, print them,
// confirm them.
func hookAsk(t *testing.T, a *Agent, req Request) Response {
	t.Helper()
	r := ask(t, a, req)
	if len(r.Messages) > 0 || r.Instruct {
		ids := make([]string, len(r.Messages))
		for i, m := range r.Messages {
			ids[i] = m.ID
		}
		if c := ask(t, a, Request{Op: "confirm", Session: req.Session, IDs: ids, Instruction: r.Instruct}); !c.OK {
			t.Fatalf("confirm: %s", c.Error)
		}
	}
	return r
}

// Two SessionStart hooks for one session (two hook configs, as when Devin
// also runs .claude/settings.json) get the standing instruction once; a
// later start of another kind (a compaction) gets it again; a prompt hook
// of a session that had it does not.
func TestControlPendingInstructOnce(t *testing.T) {
	f, _ := busFixture(t)
	r1 := hookAsk(t, f.a, Request{Op: "pending", Session: "to-2222", Start: "startup"})
	r2 := hookAsk(t, f.a, Request{Op: "pending", Session: "to-2222", Start: "startup"})
	if !r1.OK || !r1.Instruct || r2.Instruct {
		t.Fatalf("instruct: first %+v, second %+v", r1, r2)
	}
	if r := hookAsk(t, f.a, Request{Op: "pending", Session: "to-2222", Start: "compact", HookStart: time.Now().Add(time.Second).UnixMilli()}); !r.Instruct {
		t.Fatal("a compaction did not get the instruction again")
	}
	if r := hookAsk(t, f.a, Request{Op: "pending", Session: "to-2222"}); r.Instruct {
		t.Fatal("a prompt hook got the instruction again")
	}
	if r := hookAsk(t, f.a, Request{Op: "pending", Session: "other-3333", Start: "startup"}); !r.Instruct {
		t.Fatal("another session did not get the instruction")
	}
}

// The SessionStart hook took the instruction and was killed before it
// printed (#101): the session's next prompt or tool hook prints it, before
// the messages, once the lease ends; a hook inside the lease gets neither.
func TestControlInstructionAfterAKilledSessionStart(t *testing.T) {
	prev := devicebus.LeaseFor
	devicebus.LeaseFor = 300 * time.Millisecond
	t.Cleanup(func() { devicebus.LeaseFor = prev })
	f, b := busFixture(t)
	if r := ask(t, f.a, Request{Op: "pending", Session: "to-2222", Start: "startup"}); !r.Instruct {
		t.Fatal("SessionStart did not get the instruction")
	}
	// Killed: no confirm. A message arrives.
	if _, err := b.Send(ctx, busproto.SendRequest{FromSession: "from-1111", To: "to-2222", Body: "after a killed start"}); err != nil {
		t.Fatal(err)
	}
	if r := hookAsk(t, f.a, Request{Op: "pending", Session: "to-2222"}); r.Instruct || len(r.Messages) != 0 {
		t.Fatalf("inside the lease: instruct %v, %d messages", r.Instruct, len(r.Messages))
	}
	time.Sleep(devicebus.LeaseFor + 50*time.Millisecond)
	if r := hookAsk(t, f.a, Request{Op: "pending", Session: "to-2222"}); !r.Instruct || len(r.Messages) != 1 {
		t.Fatalf("after the lease: instruct %v, %d messages", r.Instruct, len(r.Messages))
	}
	time.Sleep(devicebus.LeaseFor + 50*time.Millisecond)
	if r := hookAsk(t, f.a, Request{Op: "pending", Session: "to-2222"}); r.Instruct {
		t.Fatal("printed twice")
	}
}

// A SessionStart hook that left before the answer gives the instruction
// back, so the next start for that session prints it.
func TestControlInstructReleasedWhenTheHookIsGone(t *testing.T) {
	f, _ := busFixture(t)
	hook, agentEnd := net.Pipe()
	done := make(chan struct{})
	go func() { f.a.serveConn(ctx, agentEnd); close(done) }()
	hook.Write([]byte(`{"op":"pending","session":"to-2222","start":"startup"}` + "\n"))
	hook.Close()
	<-done
	if r := ask(t, f.a, Request{Op: "pending", Session: "to-2222", Start: "startup"}); !r.Instruct {
		t.Fatal("instruction lost with the hook that gave up")
	}
}

// pending's limit and byte bound leave the rest queued for the next call;
// a start that takes the instruction leaves less room for messages.
func TestControlPendingBounded(t *testing.T) {
	f, b := busFixture(t)
	for i := range 4 {
		if _, err := b.Send(ctx, busproto.SendRequest{FromSession: "from-1111", To: "to-2222", Body: fmt.Sprintf("hello %d %s", i, strings.Repeat("x", 300))}); err != nil {
			t.Fatal(err)
		}
	}
	r := hookAsk(t, f.a, Request{Op: "pending", Session: "to-2222", Limit: 3})
	if len(r.Messages) != 3 || !r.Instruct {
		t.Fatalf("limit 3 took %d (instruct %v)", len(r.Messages), r.Instruct)
	}
	one := busrender.Size(r.Messages[0])
	for i := range 2 {
		if _, err := b.Send(ctx, busproto.SendRequest{FromSession: "from-1111", To: "to-2222", Body: fmt.Sprintf("again %d %s", i, strings.Repeat("y", 300))}); err != nil {
			t.Fatal(err)
		}
	}
	// Room for two messages without the instruction, one with it (a
	// compaction renews it).
	max := 2*one + busrender.SepLen + busrender.EncodedLen(busrender.StandingInstruction) + busrender.SepLen - 1
	r = hookAsk(t, f.a, Request{Op: "pending", Session: "to-2222", MaxBytes: max, Start: "compact", HookStart: time.Now().Add(time.Second).UnixMilli()})
	if !r.Instruct || len(r.Messages) != 1 || !strings.HasPrefix(r.Messages[0].Body, "hello 3") {
		t.Fatalf("with the instruction: instruct %v, %d messages", r.Instruct, len(r.Messages))
	}
	r = hookAsk(t, f.a, Request{Op: "pending", Session: "to-2222", MaxBytes: max})
	if len(r.Messages) != 2 {
		t.Fatalf("without the instruction: %d messages", len(r.Messages))
	}
}

// A message ref the local index holds comes back with an excerpt; one it
// does not hold comes back without.
func TestControlPendingRefExcerpts(t *testing.T) {
	prev := ExcerptBudget
	ExcerptBudget = 10 * time.Second // the race detector on a loaded machine
	t.Cleanup(func() { ExcerptBudget = prev })
	f, b := busFixture(t)
	f.once()
	ids, err := f.store.SessionsWithPrefix(ctx, "", 1)
	if err != nil || len(ids) != 1 {
		t.Fatalf("no indexed session: %v", err)
	}
	row, err := f.store.FirstMessage(ctx, ids[0])
	if err != nil || row == nil {
		t.Fatalf("first message: %v", err)
	}
	ref := fmt.Sprintf("%s/%d", ids[0], row.Ordinal)
	if _, err := b.Send(ctx, busproto.SendRequest{FromSession: "from-1111", To: "to-2222", Body: "see", Refs: []string{ref, "zzzz9999/1"}}); err != nil {
		t.Fatal(err)
	}
	r := hookAsk(t, f.a, Request{Op: "pending", Session: "to-2222"})
	want := strings.Join(strings.Fields(row.Text), " ")
	if ex := r.Excerpts[ref]; ex == "" || !strings.Contains(ex, want[:min(len(want), 20)]) {
		t.Fatalf("excerpt for %s: %q (text %q)", ref, ex, want)
	}
	if _, ok := r.Excerpts["zzzz9999/1"]; ok {
		t.Fatal("excerpt for a session the index does not hold")
	}
	// Past the budget, refs come back without excerpts and pending still
	// answers.
	ExcerptBudget = 0
	if _, err := b.Send(ctx, busproto.SendRequest{FromSession: "from-1111", To: "to-2222", Body: "see again", Refs: []string{ref}}); err != nil {
		t.Fatal(err)
	}
	if r := hookAsk(t, f.a, Request{Op: "pending", Session: "to-2222"}); !r.OK || len(r.Messages) != 1 || len(r.Excerpts) != 0 {
		t.Fatalf("past the budget: ok %v, %d messages, excerpts %v", r.OK, len(r.Messages), r.Excerpts)
	}
}

// pending leases; confirm delivers. A hook that took messages and never
// confirmed them (killed before printing) leaves them leased: the next
// hook inside the lease gets nothing, the first after it gets them again,
// marked as a redelivery.
func TestControlPendingLeaseAndConfirm(t *testing.T) {
	prev := devicebus.LeaseFor
	devicebus.LeaseFor = 300 * time.Millisecond
	t.Cleanup(func() { devicebus.LeaseFor = prev })
	f, b := busFixture(t)
	if _, err := b.Send(ctx, busproto.SendRequest{FromSession: "from-1111", To: "to-2222", Body: "taken by a hook that dies"}); err != nil {
		t.Fatal(err)
	}
	r := ask(t, f.a, Request{Op: "pending", Session: "to-2222"})
	if len(r.Messages) != 1 || r.Messages[0].Attempt != 1 {
		t.Fatalf("first pending: %+v", r.Messages)
	}
	id := r.Messages[0].ID
	if r := ask(t, f.a, Request{Op: "pending", Session: "to-2222"}); len(r.Messages) != 0 {
		t.Fatalf("offered twice inside the lease: %+v", r.Messages)
	}
	time.Sleep(devicebus.LeaseFor + 50*time.Millisecond)
	r = hookAsk(t, f.a, Request{Op: "pending", Session: "to-2222"})
	if len(r.Messages) != 1 || r.Messages[0].ID != id || r.Messages[0].Attempt != 2 {
		t.Fatalf("after the lease: %+v", r.Messages)
	}
	time.Sleep(devicebus.LeaseFor + 50*time.Millisecond)
	if r := ask(t, f.a, Request{Op: "pending", Session: "to-2222"}); len(r.Messages) != 0 {
		t.Fatalf("offered again after the confirmation: %+v", r.Messages)
	}
	in, err := b.Inbox(ctx, busproto.InboxQuery{Session: "from-1111", SentOnly: true})
	if err != nil || len(in.Messages) != 1 || in.Messages[0].State != busproto.StateDelivered {
		t.Fatalf("sender's inbox: %+v %v", in, err)
	}
	if r := ask(t, f.a, Request{Op: "confirm"}); r.OK {
		t.Fatal("confirm without a session accepted")
	}
}

// A send from a session the agent has not indexed waits up to
// devicebus.PlaceWait, but a hook's pending call meanwhile, from another
// session or from the waiting one, answers within the hook's 200 ms
// budget: the wait holds only its own connection.
func TestControlPendingDuringPlacementWait(t *testing.T) {
	f, _ := busFixture(t)
	sent := make(chan Response, 1)
	go func() {
		sent <- ask(t, f.a, Request{Op: "send", Send: &busproto.SendRequest{FromSession: "new-9999", To: "to-2222", Body: "hi"}})
	}()
	time.Sleep(100 * time.Millisecond)
	for _, s := range []string{"to-2222", "new-9999"} {
		// The best of three: a loaded test machine can stall one call
		// past the budget; a call that waited for the placement could not
		// get under it at all.
		best := time.Hour
		for range 3 {
			start := time.Now()
			r := ask(t, f.a, Request{Op: "pending", Session: s})
			if !r.OK {
				t.Fatalf("pending for %s during a placement wait: %s", s, r.Error)
			}
			best = min(best, time.Since(start))
		}
		if best > 200*time.Millisecond {
			t.Fatalf("pending for %s during a placement wait took %s", s, best)
		}
	}
	select {
	case r := <-sent:
		t.Fatalf("the send did not wait: %+v", r)
	default:
	}
	if r := <-sent; r.OK || r.BusError == nil || r.BusError.Code != busproto.CodeSessionNotOnDevice {
		t.Fatalf("send from a session never indexed: %+v", r)
	}
}

// A status whose client hangs up stops its work (the index scan) within a
// tick, instead of running on and holding a read connection.
func TestStatusContextEndsWhenClientLeaves(t *testing.T) {
	sock := filepath.Join(shortTemp(t), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	client, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	server, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	ctx, stop := untilClientLeaves(t.Context(), server)
	defer stop()
	select {
	case <-ctx.Done():
		t.Fatal("cancelled while the client waits")
	case <-time.After(50 * time.Millisecond):
	}
	client.Close()
	select {
	case <-ctx.Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("the client left and the status context lives on")
	}
}
