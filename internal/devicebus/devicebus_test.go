package devicebus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/client"
)

var ctx = context.Background()

// fakeServer is the bus routes in memory. A poll waits until the test
// answers it (answer) or the poll is cancelled.
type fakeServer struct {
	mu       sync.Mutex
	polls    []busproto.PollRequest
	pollCh   chan pollReply
	claims   []busproto.ClaimRequest
	claimFn  func(busproto.ClaimRequest) (busproto.ClaimResponse, error)
	acks     [][]string
	gone     []string // undelivered reports
	ended    []string // session_ended reports
	failed   []string // push_failed reports
	ackFn    func([]string) (busproto.AckResponse, error)
	ackReqs  []busproto.AckRequest
	reads    []busproto.ReadReceipt          // read receipts taken
	readFn   func(busproto.ReadReceipt) bool // takes a read receipt; nil: all
	sends    []busproto.SendRequest
	sendFn   func(busproto.SendRequest) error // refuses a send; nil: none
	peers    []busproto.PeersQuery
	pollErr  error // answered at once while set
	answered chan struct{}
}

type pollReply struct {
	resp busproto.PollResponse
	err  error
}

func newFakeServer() *fakeServer {
	return &fakeServer{pollCh: make(chan pollReply), answered: make(chan struct{}, 100)}
}

func (f *fakeServer) Poll(ctx context.Context, req busproto.PollRequest) (busproto.PollResponse, error) {
	f.mu.Lock()
	f.polls = append(f.polls, req)
	perr := f.pollErr
	f.mu.Unlock()
	if perr != nil {
		return busproto.PollResponse{}, perr
	}
	select {
	case r := <-f.pollCh:
		return r.resp, r.err
	case <-ctx.Done():
		return busproto.PollResponse{}, ctx.Err()
	}
}

func (f *fakeServer) Claim(_ context.Context, req busproto.ClaimRequest) (busproto.ClaimResponse, error) {
	f.mu.Lock()
	f.claims = append(f.claims, req)
	fn := f.claimFn
	f.mu.Unlock()
	return fn(req)
}

func (f *fakeServer) Ack(_ context.Context, req busproto.AckRequest) (busproto.AckResponse, error) {
	f.mu.Lock()
	f.ackReqs = append(f.ackReqs, req)
	if len(req.IDs) > 0 {
		f.acks = append(f.acks, slices.Clone(req.IDs))
	}
	f.gone = append(f.gone, req.Undelivered...)
	f.ended = append(f.ended, req.SessionEnded...)
	f.failed = append(f.failed, req.PushFailed...)
	fn, readFn := f.ackFn, f.readFn
	read, rejected := []string{}, []string{}
	for _, r := range req.Read {
		f.reads = append(f.reads, r)
		if readFn == nil || readFn(r) {
			read = append(read, r.ID)
		} else {
			rejected = append(rejected, r.ID)
		}
	}
	f.mu.Unlock()
	all := slices.Concat(req.IDs, req.Undelivered, req.SessionEnded, req.PushFailed)
	if len(all) == 0 {
		return busproto.AckResponse{Acked: []string{}, Rejected: []string{}, Read: read, ReadRejected: rejected}, nil
	}
	var out busproto.AckResponse
	var err error
	if fn == nil {
		out = busproto.AckResponse{Acked: all, Rejected: []string{}}
	} else {
		out, err = fn(all)
	}
	out.Read, out.ReadRejected = read, rejected
	return out, err
}

func (f *fakeServer) readReceipts() []busproto.ReadReceipt {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reads)
}

func (f *fakeServer) Send(_ context.Context, req busproto.SendRequest) (busproto.SendResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendFn != nil {
		if err := f.sendFn(req); err != nil {
			return busproto.SendResponse{}, err
		}
	}
	f.sends = append(f.sends, req)
	return busproto.SendResponse{ID: "msent", State: busproto.StateQueued}, nil
}

func (f *fakeServer) Peers(_ context.Context, q busproto.PeersQuery) (busproto.PeersResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.peers = append(f.peers, q)
	return busproto.PeersResponse{}, nil
}

func (f *fakeServer) Inbox(context.Context, busproto.InboxQuery) (busproto.InboxResponse, error) {
	return busproto.InboxResponse{}, nil
}

func (f *fakeServer) pollCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.polls)
}

func (f *fakeServer) lastPoll() busproto.PollRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.polls[len(f.polls)-1]
}

func (f *fakeServer) ackedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, a := range f.acks {
		out = append(out, a...)
	}
	return out
}

func (f *fakeServer) endedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ended)
}

func (f *fakeServer) undeliveredIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.gone)
}

// presenceSrc is a presence the test changes.
type presenceSrc struct {
	mu  sync.Mutex
	all []Session
}

func (p *presenceSrc) set(s ...Session) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.all = s
}

func (p *presenceSrc) get(context.Context) ([]Session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.all), nil
}

func sess(id, agent, repo string, busy bool) Session {
	return Session{PresenceSession: busproto.PresenceSession{SessionID: id, Agent: agent, Repo: repo, Branch: "main", Busy: busy}, LastActive: time.Now()}
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testConfig is fast timings for tests.
func testConfig(srv Server, key *string) Config {
	cfg := Config{Logger: quiet(), User: "gary", PresenceEvery: 20 * time.Millisecond, BackoffMin: 10 * time.Millisecond,
		BackoffMax: 40 * time.Millisecond, RepinEvery: 20 * time.Millisecond, AckDelay: 10 * time.Millisecond}
	if srv != nil {
		var mu sync.Mutex
		cfg.Connect = func() (Server, string) {
			mu.Lock()
			defer mu.Unlock()
			if key == nil {
				return srv, "k"
			}
			return srv, *key
		}
	}
	return cfg
}

func openBus(t *testing.T, path string, cfg Config, p *presenceSrc) *Bus {
	t.Helper()
	b, err := Open(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	if p != nil {
		b.SetSources(p.get, func(_ context.Context, prefix string) ([]Session, error) {
			all, _ := p.get(ctx)
			var out []Session
			for _, s := range all {
				if len(s.SessionID) >= len(prefix) && s.SessionID[:len(prefix)] == prefix {
					out = append(out, s)
				}
			}
			return out, nil
		})
	}
	return b
}

func run(t *testing.T, b *Bus) {
	t.Helper()
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { b.Run(rctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var seq int64

func env(id, to string) busproto.Envelope {
	seq++
	now := time.Now().UTC().Truncate(time.Millisecond)
	return busproto.Envelope{ID: id, ThreadID: id, From: "alex-sess", FromAgent: "claude", User: "alex@example.test", Sender: busproto.SenderTeammate,
		Intent: busproto.IntentInform, Body: "body of " + id, Sent: now, ExpiresAt: now.Add(time.Hour), ToSession: to, ToAgent: "claude",
		ToUser: "gary@example.test", Addressed: "session", Seq: seq}
}

// deliver is what a hook does: take the session's messages, print them,
// confirm them.
func deliver(b *Bus, session, agent string, lim Limit) ([]busproto.Envelope, error) {
	got, err := b.Take(ctx, session, agent, lim)
	if err != nil || len(got) == 0 {
		return got, err
	}
	return got, b.Confirm(ctx, session, ids(got))
}

func ids(es []busproto.Envelope) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.ID
	}
	return out
}

// The inbox follows each poll's whole set: new messages are added, a
// queued message missing from the set is dropped, a delivered one is
// kept (so it is not delivered twice if it is listed again), and a
// delivered message listed again owes its receipt again.
func TestReconcile(t *testing.T) {
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(newFakeServer(), nil), nil)
	now := time.Now()
	m1, m2, m3 := env("m1", "s1"), env("m2", "s1"), env("m3", "s2")
	if err := b.st.reconcile(ctx, []busproto.Envelope{m1, m2, m3}, nil, now); err != nil {
		t.Fatal(err)
	}
	got, err := deliver(b, "s1", "", Limit{})
	if err != nil || !slices.Equal(ids(got), []string{"m1", "m2"}) {
		t.Fatalf("pending s1: %v %v", ids(got), err)
	}
	if got[0].Body != "body of m1" || got[0].User != "alex@example.test" || got[0].Sender != busproto.SenderTeammate {
		t.Fatalf("envelope not kept as the server set it: %+v", got[0])
	}
	// m3 (queued) and m1 (delivered) leave the set; m4 arrives.
	m4 := env("m4", "s2")
	if err := b.st.reconcile(ctx, []busproto.Envelope{m2, m4}, nil, now); err != nil {
		t.Fatal(err)
	}
	if got, _ := deliver(b, "s2", "", Limit{}); !slices.Equal(ids(got), []string{"m4"}) {
		t.Fatalf("pending s2 after m3 left the set: %v", ids(got))
	}
	// m1 is listed again (held, then released): not delivered twice, and
	// its receipt is owed again.
	if err := b.st.acked(ctx, []string{"m1", "m2", "m4"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := b.st.reconcile(ctx, []busproto.Envelope{m1}, nil, now); err != nil {
		t.Fatal(err)
	}
	if got, _ := deliver(b, "s1", "", Limit{}); len(got) != 0 {
		t.Fatalf("delivered message delivered again: %v", ids(got))
	}
	if owed, _ := b.st.owed(ctx, "owed", 10); !slices.Equal(owed, []string{"m1"}) {
		t.Fatalf("owed after m1 was listed again: %v", owed)
	}
	// A claimable id the inbox holds is kept although Messages lacks it.
	m5 := env("m5", "s1")
	if err := b.st.addClaimed(ctx, m5); err != nil {
		t.Fatal(err)
	}
	if err := b.st.reconcile(ctx, nil, []string{"m5"}, now); err != nil {
		t.Fatal(err)
	}
	if got, _ := deliver(b, "s1", "", Limit{}); !slices.Equal(ids(got), []string{"m5"}) {
		t.Fatalf("claimed message dropped: %v", ids(got))
	}
}

// Expired messages are not delivered.
func TestPendingSkipsExpired(t *testing.T) {
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(newFakeServer(), nil), nil)
	old := env("mold", "s1")
	old.ExpiresAt = time.Now().Add(-time.Second)
	if err := b.st.reconcile(ctx, []busproto.Envelope{old, env("mnew", "s1")}, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, _ := deliver(b, "s1", "", Limit{}); !slices.Equal(ids(got), []string{"mnew"}) {
		t.Fatalf("pending: %v", ids(got))
	}
}

// Several hooks of one session ask at once: each message goes to exactly
// one of them.
func TestPendingExactlyOnceConcurrent(t *testing.T) {
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(newFakeServer(), nil), nil)
	const n = 60
	var all []busproto.Envelope
	for i := range n {
		all = append(all, env(fmt.Sprintf("m%03d", i), "s1"))
	}
	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 20 {
				got, err := deliver(b, "s1", "claude", Limit{})
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				for _, e := range got {
					seen[e.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	// Messages arrive while the hooks ask.
	go func() {
		close(start)
		for i := 0; i < n; i += 10 {
			if err := b.st.reconcile(ctx, all[:i+10], nil, time.Now()); err != nil {
				t.Error(err)
			}
		}
	}()
	wg.Wait()
	if got, _ := deliver(b, "s1", "", Limit{}); len(got) > 0 {
		mu.Lock()
		for _, e := range got {
			seen[e.ID]++
		}
		mu.Unlock()
	}
	if len(seen) != n {
		t.Fatalf("%d of %d messages delivered", len(seen), n)
	}
	for id, c := range seen {
		if c != 1 {
			t.Fatalf("%s delivered %d times", id, c)
		}
	}
}

// The inbox survives a restart: undelivered messages are still pending,
// and receipts owed before the restart are sent after it.
func TestRestartKeepsInboxAndReceipts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bus.db")
	srv := newFakeServer()
	b, err := Open(path, testConfig(srv, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.st.reconcile(ctx, []busproto.Envelope{env("ma", "s1"), env("mb", "s2")}, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, _ := deliver(b, "s1", "", Limit{}); !slices.Equal(ids(got), []string{"ma"}) {
		t.Fatalf("pending: %v", ids(got))
	}
	b.Close() // stopped before the receipt went out

	p := &presenceSrc{}
	b2 := openBus(t, path, testConfig(srv, nil), p)
	if got, _ := deliver(b2, "s1", "", Limit{}); len(got) != 0 {
		t.Fatalf("delivered message pending again after restart: %v", ids(got))
	}
	run(t, b2)
	waitFor(t, "the receipt owed before the restart", func() bool { return slices.Contains(srv.ackedIDs(), "ma") })
	if got, _ := deliver(b2, "s2", "", Limit{}); !slices.Equal(ids(got), []string{"mb"}) {
		t.Fatalf("undelivered message lost in restart: %v", ids(got))
	}
}

// drainAcks runs the receipt batcher's sends (sendAcks) until nothing is
// owed, as runAcks does after a kick, without its timers. It returns the
// number of batches sent.
func drainAcks(t *testing.T, b *Bus) int {
	t.Helper()
	for batches := 0; ; batches++ {
		n, err := b.sendAcks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return batches
		}
		if batches > 100 {
			t.Fatal("receipts never settle")
		}
	}
}

// Receipts go in batches of at most busproto.MaxAck; rejected ids are not
// sent again. The batcher is driven directly (drainAcks): no timers, so
// the server's view and the inbox are read only after each batch settled
// (issue #104: the fake server counted a batch before the inbox recorded
// its answer).
func TestAckBatchingAndRejected(t *testing.T) {
	srv := newFakeServer()
	srv.ackFn = func(ids []string) (busproto.AckResponse, error) {
		out := busproto.AckResponse{}
		for _, id := range ids {
			if id == "m007" {
				out.Rejected = append(out.Rejected, id)
			} else {
				out.Acked = append(out.Acked, id)
			}
		}
		return out, nil
	}
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(srv, nil), &presenceSrc{})
	var all []busproto.Envelope
	for i := range 150 {
		all = append(all, env(fmt.Sprintf("m%03d", i), "s1"))
	}
	if err := b.st.reconcile(ctx, all, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, _ := deliver(b, "s1", "", Limit{}); len(got) != 150 {
		t.Fatalf("pending %d", len(got))
	}
	if n := drainAcks(t, b); n != 2 {
		t.Errorf("%d batches for 150 receipts", n)
	}
	if got := len(srv.ackedIDs()); got != 150 {
		t.Fatalf("%d receipts sent", got)
	}
	srv.mu.Lock()
	for _, a := range srv.acks {
		if len(a) > busproto.MaxAck {
			t.Errorf("a batch of %d ids", len(a))
		}
	}
	srv.mu.Unlock()
	if owed, _ := b.st.owed(ctx, "owed", 200); len(owed) != 0 {
		t.Fatalf("still owed: %v", owed)
	}
	// A rejected id is not sent again.
	if n := drainAcks(t, b); n != 0 {
		t.Fatalf("%d more batches after every receipt settled", n)
	}
	n := 0
	for _, id := range srv.ackedIDs() {
		if id == "m007" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("rejected id sent %d times", n)
	}
}

// claimOrder: on the routed repo first, then busy, then the most recently
// active.
func TestClaimOrder(t *testing.T) {
	now := time.Now()
	a := sess("a", "claude", "/src/web", true)
	b := sess("b", "codex", "/x/api", false)
	b.LastActive = now.Add(-time.Hour)
	c := sess("c", "claude", "/y/api", false)
	c.LastActive = now
	d := sess("d", "claude", "/src/other", false)
	e := env("mu", "")
	e.Addressed, e.ToSession, e.ToRepo = "user", "", "api"
	got := claimOrder(busproto.Claimable{Message: e, Sessions: []string{"d", "a", "b", "c"}}, []Session{a, b, c, d})
	if want := []string{"c", "b", "a", "d"}; !slices.Equal(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
}

// Review of #125: for a message routed by remote, a session on the
// remote comes first, then one that reported no remote whose root has
// the remote's name, then the rest; without a server the same order
// decides which sessions may take it.
func TestClaimOrderByRemote(t *testing.T) {
	on := sess("on", "claude", "/home/a/web-local", false)
	on.Remote = "github.com/acme/web"
	bare := sess("bare", "claude", "/home/a/web", true)
	fork := sess("fork", "codex", "/home/a/fork/web", true)
	fork.Remote = "github.com/other/web"
	e := env("mu", "")
	e.Addressed, e.ToSession, e.ToRepo = "user", "", "github.com/acme/web"
	got := claimOrder(busproto.Claimable{Message: e, Sessions: []string{"fork", "bare", "on"}}, []Session{on, bare, fork})
	if want := []string{"on", "bare", "fork"}; !slices.Equal(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
	if !eligible(e.ToRepo, e, bare, []Session{bare, fork}) || eligible(e.ToRepo, e, fork, []Session{bare, fork}) {
		t.Fatal("without a session on the remote, the one with no remote and the name is the route")
	}
	if eligible(e.ToRepo, e, bare, []Session{on, bare, fork}) {
		t.Fatal("a session on the remote goes before one with no remote")
	}
}

// An @user message offered by the poll is claimed for one session; a
// session the server no longer holds is skipped for the next; a claim
// lost to another device drops the message and it is not claimed again.
func TestClaimFlow(t *testing.T) {
	srv := newFakeServer()
	srv.claimFn = func(req busproto.ClaimRequest) (busproto.ClaimResponse, error) {
		switch {
		case req.MessageID == "mlost":
			return busproto.ClaimResponse{}, &busproto.Error{Status: 409, Code: busproto.CodeAlreadyClaimed}
		case req.SessionID == "gone":
			return busproto.ClaimResponse{}, &busproto.Error{Status: 403, Code: busproto.CodeSessionNotOnDevice}
		}
		e := env(req.MessageID, req.SessionID)
		e.Addressed = "user"
		return busproto.ClaimResponse{Message: e}, nil
	}
	p := &presenceSrc{}
	p.set(sess("gone", "claude", "/src/api", true), sess("here", "claude", "/src/api", false))
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(srv, nil), p)
	run(t, b)
	waitFor(t, "a poll", func() bool { return srv.pollCount() == 1 })
	won, lost := env("mwon", ""), env("mlost", "")
	won.Addressed, lost.Addressed = "user", "user"
	offer := busproto.PollResponse{Cursor: 5, Messages: []busproto.Envelope{}, Claimable: []busproto.Claimable{
		{Message: won, Sessions: []string{"gone", "here"}}, {Message: lost, Sessions: []string{"here"}}}}
	srv.pollCh <- pollReply{resp: offer}
	waitFor(t, "the next poll", func() bool { return srv.pollCount() == 2 })
	if got, _ := deliver(b, "here", "", Limit{}); !slices.Equal(ids(got), []string{"mwon"}) {
		t.Fatalf("pending: %v", ids(got))
	}
	if srv.lastPoll().Cursor != 5 {
		t.Fatalf("cursor not passed on: %d", srv.lastPoll().Cursor)
	}
	// The same offer again: mlost is not claimed a second time.
	srv.pollCh <- pollReply{resp: offer}
	waitFor(t, "a third poll", func() bool { return srv.pollCount() == 3 })
	srv.mu.Lock()
	n := 0
	for _, c := range srv.claims {
		if c.MessageID == "mlost" {
			n++
		}
	}
	srv.mu.Unlock()
	if n != 1 {
		t.Fatalf("lost message claimed %d times", n)
	}
	if h := b.Status(ctx); h.State != StateConnected {
		t.Fatalf("status %+v", h)
	}
}

// Presence changes: the poll in flight is cancelled and a new one carries
// the new presence with the cursor reset. A withheld session is never
// reported.
func TestPresenceChangeRepolls(t *testing.T) {
	srv := newFakeServer()
	p := &presenceSrc{}
	secret := sess("secret", "codex", "/src/client", true)
	secret.Withheld = true
	p.set(sess("s1", "claude", "/src/api", false), secret)
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(srv, nil), p)
	run(t, b)
	waitFor(t, "a poll", func() bool { return srv.pollCount() == 1 })
	if got := srv.lastPoll().Sessions; len(got) != 1 || got[0].SessionID != "s1" {
		t.Fatalf("first poll presence: %+v", got)
	}
	srv.pollCh <- pollReply{resp: busproto.PollResponse{Cursor: 9, Gen: 4}}
	waitFor(t, "the second poll", func() bool { return srv.pollCount() == 2 })
	if srv.lastPoll().Cursor != 9 || srv.lastPoll().Gen != 4 {
		t.Fatalf("cursor %d gen %d", srv.lastPoll().Cursor, srv.lastPoll().Gen)
	}
	p.set(sess("s1", "claude", "/src/api", true), secret) // s1 turns busy
	waitFor(t, "a poll with the new presence", func() bool { return srv.pollCount() == 3 })
	last := srv.lastPoll()
	if len(last.Sessions) != 1 || !last.Sessions[0].Busy || last.Cursor != 0 {
		t.Fatalf("poll after the change: %+v", last)
	}
}

// A session's title (often its first prompt, from the local index, which
// is not redacted) passes the device redactor before a poll reports it:
// the server shows it to every member in peers.
func TestPresenceTitleRedacted(t *testing.T) {
	srv := newFakeServer()
	p := &presenceSrc{}
	tok := "gh" + "p_" + strings.Repeat("aB3dE5", 6)
	s := sess("s1", "claude", "/src/api", false)
	s.Title = "deploy with " + tok + " today"
	p.set(s)
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(srv, nil), p)
	run(t, b)
	waitFor(t, "a poll", func() bool { return srv.pollCount() == 1 })
	got := srv.lastPoll().Sessions
	if len(got) != 1 || strings.Contains(got[0].Title, tok) || !strings.HasPrefix(got[0].Title, "deploy with ") {
		t.Fatalf("presence title: %+v", got)
	}
}

// Presence that changes while no poll is in flight (here during a
// backoff) also resets the cursor: the poll that first reports a new
// session asks for the whole set at once instead of holding on a cursor
// that is past messages the new session makes deliverable.
func TestPresenceChangeBetweenPollsResetsCursor(t *testing.T) {
	srv := newFakeServer()
	p := &presenceSrc{}
	p.set(sess("s1", "claude", "/src/api", false))
	var off atomic.Int64 // the test's clock runs ahead to expire the presence cache
	cfg := testConfig(srv, nil)
	cfg.Now = func() time.Time { return time.Now().Add(time.Duration(off.Load())) }
	cfg.BackoffMin, cfg.BackoffMax = 400*time.Millisecond, 400*time.Millisecond
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), cfg, p)
	run(t, b)
	waitFor(t, "a poll", func() bool { return srv.pollCount() == 1 })
	srv.pollCh <- pollReply{resp: busproto.PollResponse{Cursor: 9}}
	waitFor(t, "the second poll", func() bool { return srv.pollCount() == 2 })
	srv.pollCh <- pollReply{err: errors.New("connection reset")}
	waitFor(t, "backing off", func() bool { return b.Status(ctx).State == StateBackoff })
	p.set(sess("s1", "claude", "/src/api", false), sess("s2", "codex", "/src/api", false))
	off.Add(int64(2 * time.Second))
	waitFor(t, "the poll after the backoff", func() bool { return srv.pollCount() >= 3 })
	srv.mu.Lock()
	third := srv.polls[2]
	srv.mu.Unlock()
	if len(third.Sessions) != 2 || third.Cursor != 0 {
		t.Fatalf("first poll with the new session: %d sessions, cursor %d", len(third.Sessions), third.Cursor)
	}
}

// A refused pin stops polling (as sync stops) until the saved credential
// changes; a network failure backs off and retries.
func TestPermanentErrorStopsUntilRepin(t *testing.T) {
	srv := newFakeServer()
	key := "k1"
	var keyMu sync.Mutex
	cfg := testConfig(srv, nil)
	cfg.Connect = func() (Server, string) {
		keyMu.Lock()
		defer keyMu.Unlock()
		return srv, key
	}
	srv.pollErr = &client.PinError{Got: "sha256:00"}
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), cfg, &presenceSrc{})
	run(t, b)
	waitFor(t, "stopped", func() bool { return b.Status(ctx).State == StateStopped })
	n := srv.pollCount()
	time.Sleep(150 * time.Millisecond) // several RepinEvery
	if srv.pollCount() != n {
		t.Fatalf("polled while stopped: %d then %d", n, srv.pollCount())
	}
	srv.mu.Lock()
	srv.pollErr = errors.New("connection refused")
	srv.mu.Unlock()
	keyMu.Lock()
	key = "k2" // flopwire login saved a new pin
	keyMu.Unlock()
	b.Recheck()
	waitFor(t, "backing off on a network failure", func() bool { return b.Status(ctx).State == StateBackoff })
	m := srv.pollCount()
	waitFor(t, "retries", func() bool { return srv.pollCount() >= m+2 })
	srv.mu.Lock()
	srv.pollErr = nil
	srv.mu.Unlock()
	waitFor(t, "a poll waiting", func() bool {
		select {
		case srv.pollCh <- pollReply{resp: busproto.PollResponse{Cursor: 1}}:
			return true
		default:
			return false
		}
	})
	waitFor(t, "connected", func() bool { return b.Status(ctx).State == StateConnected })
}

// A server without the bus (501) disables messaging rather than retrying
// every second.
func TestServerWithoutBusDisables(t *testing.T) {
	srv := newFakeServer()
	srv.pollErr = &client.APIError{StatusCode: http.StatusNotImplemented}
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(srv, nil), &presenceSrc{})
	run(t, b)
	waitFor(t, "disabled", func() bool { return b.Status(ctx).State == StateDisabled })
	n := srv.pollCount()
	time.Sleep(100 * time.Millisecond)
	if srv.pollCount() != n {
		t.Fatalf("polled while disabled")
	}
}

// A send naming a session the path rules keep off the server is refused
// on the device: the request would tell the server its id.
func TestSendFromWithheldSessionRefused(t *testing.T) {
	srv := newFakeServer()
	p := &presenceSrc{}
	secret := sess("secret-1", "claude", "/src/client", true)
	secret.Withheld = true
	p.set(secret, sess("open-1", "claude", "/src/api", true))
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(srv, nil), p)
	var be *busproto.Error
	if _, err := b.Send(ctx, busproto.SendRequest{FromSession: "secret-1", To: "abcd", Body: "x"}); !errors.As(err, &be) || be.Code != busproto.CodeSessionNotOnDevice {
		t.Fatalf("send from a withheld session: %v", err)
	}
	if _, err := b.Inbox(ctx, busproto.InboxQuery{Session: "secret-1"}); !errors.As(err, &be) {
		t.Fatalf("inbox of a withheld session: %v", err)
	}
	if _, err := b.Send(ctx, busproto.SendRequest{FromSession: "open-1", To: "abcd", Body: "x"}); err != nil {
		t.Fatal(err)
	}
	if len(srv.sends) != 1 {
		t.Fatalf("server got %d sends", len(srv.sends))
	}
}

// A withheld session that is not live (idle past the live window, or not
// in the last presence yet) is still refused: its id and body never reach
// the server. So is a session the device does not know at all, whose path
// rules it cannot judge.
func TestSendFromWithheldSessionNotLiveRefused(t *testing.T) {
	srv := newFakeServer()
	p := &presenceSrc{}
	p.set(sess("open-1", "claude", "/src/api", true))
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(srv, nil), p)
	secret := sess("secret-2", "claude", "/src/client", false)
	secret.Withheld = true
	stored := []Session{secret, sess("stored-1", "codex", "/src/api", false)}
	b.SetSources(p.get, func(_ context.Context, prefix string) ([]Session, error) {
		var out []Session
		for _, s := range stored {
			if strings.HasPrefix(s.SessionID, prefix) {
				out = append(out, s)
			}
		}
		return out, nil
	})
	for _, from := range []string{"secret-2", "nobody-9"} {
		var be *busproto.Error
		if _, err := b.Send(ctx, busproto.SendRequest{FromSession: from, To: "abcd", Body: "private text"}); !errors.As(err, &be) || be.Code != busproto.CodeSessionNotOnDevice {
			t.Fatalf("send from %s: %v", from, err)
		}
		if _, err := b.Inbox(ctx, busproto.InboxQuery{Session: from}); !errors.As(err, &be) || be.Code != busproto.CodeSessionNotOnDevice {
			t.Fatalf("inbox of %s: %v", from, err)
		}
		if _, err := b.Peers(ctx, busproto.PeersQuery{Session: from}); !errors.As(err, &be) || be.Code != busproto.CodeSessionNotOnDevice {
			t.Fatalf("peers for %s: %v", from, err)
		}
	}
	srv.mu.Lock()
	n := len(srv.sends)
	srv.mu.Unlock()
	if n != 0 {
		t.Fatalf("server got %d sends from withheld or unknown sessions", n)
	}
	// A stored session the rules let through may still send.
	if _, err := b.Send(ctx, busproto.SendRequest{FromSession: "stored-1", To: "abcd", Body: "x"}); err != nil {
		t.Fatal(err)
	}
}

// A send through the server passes the device redactor first: the secret
// never leaves the machine, and the counts reach the sender.
func TestSendRedactsBeforeTheServer(t *testing.T) {
	srv := newFakeServer()
	p := &presenceSrc{}
	p.set(sess("open-1", "claude", "/src/api", true))
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(srv, nil), p)
	tok := "gh" + "p_" + strings.Repeat("aB3dE5", 6)
	out, err := b.Send(ctx, busproto.SendRequest{FromSession: "open-1", To: "abcd", Body: "use " + tok + " for the push", Refs: []string{"ref " + tok}})
	if err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	got := srv.sends[0]
	srv.mu.Unlock()
	if strings.Contains(got.Body, tok) || strings.Contains(strings.Join(got.Refs, " "), tok) {
		t.Fatalf("secret sent to the server: %+v", got)
	}
	if len(got.Body) != len("use "+tok+" for the push") {
		t.Fatalf("mask changed the body's length: %q", got.Body)
	}
	if out.Redactions["github-token"] != 2 {
		t.Fatalf("redactions: %+v", out.Redactions)
	}
}

// Requeue undoes a Take whose caller never got the answer: the message is
// pending again, unmarked, and owes no receipt until a hook confirms it.
func TestRequeueWithdrawsTheReceipt(t *testing.T) {
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(newFakeServer(), nil), nil)
	if err := b.st.reconcile(ctx, []busproto.Envelope{env("mq", "s1")}, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Take(ctx, "s1", "", Limit{}); len(got) != 1 {
		t.Fatalf("pending: %v", ids(got))
	}
	if err := b.Requeue(ctx, []string{"mq"}); err != nil {
		t.Fatal(err)
	}
	if owed, _ := b.st.owed(ctx, "owed", 10); len(owed) != 0 {
		t.Fatalf("receipt still owed: %v", owed)
	}
	if got, _ := deliver(b, "s1", "", Limit{}); !slices.Equal(ids(got), []string{"mq"}) || got[0].Attempt != 1 {
		t.Fatalf("pending after requeue: %v %+v", ids(got), got)
	}
	if owed, _ := b.st.owed(ctx, "owed", 10); !slices.Equal(owed, []string{"mq"}) {
		t.Fatalf("receipt after the second take: %v", owed)
	}
}

// The held notice names each sender at most once per NoticeEvery, across
// every hook that asks (one device, many sessions), and names a sender
// again a day later while their messages are still held.
func TestHeldNoticeOncePerSenderPerDay(t *testing.T) {
	st, err := openStore(filepath.Join(t.TempDir(), "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.db.Close() })
	ctx := context.Background()
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	alex := busproto.HeldSender{User: "alex@example.test", UserID: "u-alex", Count: 1}
	sam := busproto.HeldSender{User: "sam@example.test", UserID: "u-sam", Count: 3}
	ids := func(hs []busproto.HeldSender) string {
		var out []string
		for _, h := range hs {
			out = append(out, h.UserID)
		}
		return strings.Join(out, ",")
	}
	for _, c := range []struct {
		at   time.Duration
		held []busproto.HeldSender
		want string
	}{
		{0, []busproto.HeldSender{alex}, "u-alex"},
		{time.Minute, []busproto.HeldSender{alex}, ""},               // another session's prompt
		{time.Hour, []busproto.HeldSender{alex, sam}, "u-sam"},       // a new sender is named at once
		{23 * time.Hour, []busproto.HeldSender{alex, sam}, ""},       // within the day
		{24 * time.Hour, []busproto.HeldSender{alex, sam}, "u-alex"}, // alex again a day later
		{25 * time.Hour, []busproto.HeldSender{alex, sam}, "u-sam"},
	} {
		got, err := st.notice(ctx, c.held, now.Add(c.at), NoticeEvery)
		if err != nil || ids(got) != c.want {
			t.Fatalf("at +%s: %q %v, want %q", c.at, ids(got), err, c.want)
		}
	}
}

// A send whose refs or recipient prefix name a session the path rules
// keep off the server is refused on the device, before any request: the
// server would learn the session's id (issue #71). The check covers every
// ref, and a send naming no withheld session goes through.
func TestSendNamingWithheldSessionRefused(t *testing.T) {
	srv := newFakeServer()
	p := &presenceSrc{}
	p.set(sess("open-1", "claude", "/src/api", true))
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(srv, nil), p)
	const secret = "5ec2e7aa-0000-4000-8000-000000000001"
	var asked []string
	b.SetWithheld(func(_ context.Context, ref string) (string, error) {
		asked = append(asked, ref)
		if strings.HasPrefix(ref, "5ec2e7aa") || strings.HasSuffix(ref, secret+".jsonl:3") {
			return secret, nil
		}
		return "", nil
	}, nil)
	for _, req := range []busproto.SendRequest{
		{FromSession: "open-1", To: "@alex", Body: "see the ref", Refs: []string{"4c19e0d2/12", "5ec2e7aa/28672"}},
		{FromSession: "open-1", To: "@alex", Body: "see the ref", Refs: []string{"/home/g/.claude/projects/-src-client/" + secret + ".jsonl:3"}},
		{FromSession: "open-1", To: "5ec2e7aa", Body: "hello"},
	} {
		var be *busproto.Error
		_, err := b.Send(ctx, req)
		if !errors.As(err, &be) || be.Code != busproto.CodeWithheldSession || be.Status != http.StatusForbidden || !strings.Contains(be.Detail, secret) || !strings.Contains(be.Detail, "path rule") {
			t.Fatalf("send %+v: %v", req, err)
		}
	}
	srv.mu.Lock()
	n := len(srv.sends)
	srv.mu.Unlock()
	if n != 0 {
		t.Fatalf("server got %d sends naming a withheld session", n)
	}
	asked = nil
	if _, err := b.Send(ctx, busproto.SendRequest{FromSession: "open-1", To: "4c19e0d2", Body: "fine", Refs: []string{"4c19e0d2/12"}}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(asked, []string{"4c19e0d2", "4c19e0d2/12"}) {
		t.Fatalf("checked %q", asked)
	}
	// An @user recipient names no session.
	asked = nil
	if _, err := b.Send(ctx, busproto.SendRequest{FromSession: "open-1", To: "@alex", Body: "fine too"}); err != nil || len(asked) != 0 {
		t.Fatalf("@user: %v, checked %q", err, asked)
	}
}

// Control characters (but newline and tab) never leave the device: a body
// of them grew six-fold as JSON (issue #71). A CR or CRLF is a newline.
func TestSendDropsControlCharacters(t *testing.T) {
	srv := newFakeServer()
	p := &presenceSrc{}
	p.set(sess("open-1", "claude", "/src/api", true))
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(srv, nil), p)
	refs := []string{"4c19e0d2/12\x1b[2J"}
	if _, err := b.Send(ctx, busproto.SendRequest{FromSession: "open-1", To: "@alex", Body: "a\x00b\x1b[31mc\x7fd\u009be\r\nf\rg\th\ni", Refs: refs}); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	got := srv.sends[0]
	srv.mu.Unlock()
	if got.Body != "ab[31mcde\nf\ng\th\ni" || !slices.Equal(got.Refs, []string{"4c19e0d2/12[2J"}) {
		t.Fatalf("sent %q %q", got.Body, got.Refs)
	}
	if refs[0] != "4c19e0d2/12\x1b[2J" {
		t.Fatal("the caller's refs were changed")
	}
	// The local inbox gets the same text.
	lb := openBus(t, filepath.Join(t.TempDir(), "local.db"), testConfig(nil, nil), p)
	p.set(sess("open-1", "claude", "/src/api", true), sess("open-2", "claude", "/src/api", true))
	if _, err := lb.Send(ctx, busproto.SendRequest{FromSession: "open-1", To: "open-2", Body: "x\x07y\r\nz"}); err != nil {
		t.Fatal(err)
	}
	in, err := lb.Inbox(ctx, busproto.InboxQuery{Session: "open-2"})
	if err != nil || len(in.Messages) != 1 || in.Messages[0].Body != "xy\nz" {
		t.Fatalf("local inbox: %+v %v", in, err)
	}
}

// An @user send's repo, and a peers filter or any of its roots, that the
// path rules keep off the server is refused on the device before any
// request: the server would learn the repo's path or name (issue #71).
func TestRequestNamingWithheldRepoRefused(t *testing.T) {
	srv := newFakeServer()
	p := &presenceSrc{}
	p.set(sess("open-1", "claude", "/src/api", true))
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(srv, nil), p)
	var asked []string
	b.SetWithheld(nil, func(_ context.Context, repo string) (bool, error) {
		asked = append(asked, repo)
		return repo == "/src/client" || repo == "client", nil
	})
	refused := func(what string, err error) {
		t.Helper()
		var be *busproto.Error
		if !errors.As(err, &be) || be.Code != busproto.CodeWithheldRepo || be.Status != http.StatusForbidden || !strings.Contains(be.Detail, "path rule") {
			t.Fatalf("%s: %v", what, err)
		}
	}
	for _, repo := range []string{"/src/client", "client"} {
		_, err := b.Send(ctx, busproto.SendRequest{FromSession: "open-1", To: "@alex", Body: "hi", Repo: repo})
		refused("send repo "+repo, err)
		_, err = b.Peers(ctx, busproto.PeersQuery{Session: "open-1", Repo: repo})
		refused("peers repo "+repo, err)
	}
	_, err := b.Peers(ctx, busproto.PeersQuery{Session: "open-1", Repo: "api", Roots: []string{"/src/api", "/src/client"}})
	refused("peers root", err)
	srv.mu.Lock()
	sends, peers := len(srv.sends), len(srv.peers)
	srv.mu.Unlock()
	if sends != 0 || peers != 0 {
		t.Fatalf("server got %d sends and %d peers queries naming a withheld repo", sends, peers)
	}
	// Other repos, "*" and none go through; a session recipient's repo is
	// not checked (the server ignores it).
	asked = nil
	for _, req := range []busproto.SendRequest{
		{FromSession: "open-1", To: "@alex", Body: "a", Repo: "/src/api"},
		{FromSession: "open-1", To: "@alex", Body: "b", Repo: "*"},
		{FromSession: "open-1", To: "@alex", Body: "c"},
	} {
		if _, err := b.Send(ctx, req); err != nil {
			t.Fatalf("send %+v: %v", req, err)
		}
	}
	if _, err := b.Peers(ctx, busproto.PeersQuery{Session: "open-1", Repo: "api", Roots: []string{"/src/api"}}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(asked, []string{"/src/api", "api", "/src/api"}) {
		t.Fatalf("checked %q", asked)
	}
}

// shortPlaceWait shortens PlaceWait for one test.
func shortPlaceWait(t *testing.T, d time.Duration) {
	was := PlaceWait
	PlaceWait = d
	t.Cleanup(func() { PlaceWait = was })
}

func (f *fakeServer) sendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sends)
}

// A brand-new session that sends before the agent indexed it is not
// refused: the bus asks the agent to place it, waits up to PlaceWait, and
// sends once it appears (issue #71).
func TestSendFromNewSessionWaitsForIndex(t *testing.T) {
	srv := newFakeServer()
	p := &presenceSrc{}
	p.set(sess("open-1", "claude", "/src/api", true))
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(srv, nil), p)
	var placed atomic.Int32
	b.SetPlace(func(_ context.Context, session string) error {
		placed.Add(1)
		go func() {
			time.Sleep(150 * time.Millisecond) // indexing
			p.set(sess("open-1", "claude", "/src/api", true), sess(session, "claude", "/src/api", true))
		}()
		return nil
	})
	if _, err := b.Send(ctx, busproto.SendRequest{FromSession: "new-1", FromAgent: "claude", To: "abcd", Body: "x"}); err != nil {
		t.Fatalf("send from a session indexed within the wait: %v", err)
	}
	if placed.Load() != 1 || srv.sendCount() != 1 {
		t.Fatalf("placed %d times, %d sends", placed.Load(), srv.sendCount())
	}
	// Never indexed: refused once the wait is over, as not indexed yet,
	// which a retry may fix.
	shortPlaceWait(t, 100*time.Millisecond)
	b.SetPlace(nil)
	var be *busproto.Error
	if _, err := b.Send(ctx, busproto.SendRequest{FromSession: "never-1", To: "abcd", Body: "x"}); !errors.As(err, &be) ||
		be.Code != busproto.CodeSessionNotOnDevice || !strings.Contains(be.Detail, "not indexed") || strings.Contains(be.Detail, "path rule") {
		t.Fatalf("send from an unknown session: %v", err)
	}
}

// A session whose path rules are not decided yet (no complete line has
// named its directory) is waited for, and refused as not indexed yet
// rather than as kept off the server by a path rule; nothing reaches the
// server meanwhile.
func TestSendFromUnplacedSession(t *testing.T) {
	srv := newFakeServer()
	p := &presenceSrc{}
	young := sess("young-1", "claude", "", true)
	young.Withheld, young.Unplaced = true, true
	p.set(young)
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(srv, nil), p)
	shortPlaceWait(t, 100*time.Millisecond)
	var be *busproto.Error
	if _, err := b.Send(ctx, busproto.SendRequest{FromSession: "young-1", To: "abcd", Body: "x"}); !errors.As(err, &be) ||
		be.Code != busproto.CodeSessionNotOnDevice || !strings.Contains(be.Detail, "not indexed") || strings.Contains(be.Detail, "path rule") {
		t.Fatalf("send from an unplaced session: %v", err)
	}
	if srv.sendCount() != 0 {
		t.Fatal("an unplaced session's send reached the server")
	}
	shortPlaceWait(t, 2*time.Second)
	go func() {
		time.Sleep(150 * time.Millisecond)
		p.set(sess("young-1", "claude", "/src/api", true)) // its first line named an allowed directory
	}()
	if _, err := b.Send(ctx, busproto.SendRequest{FromSession: "young-1", To: "abcd", Body: "x"}); err != nil {
		t.Fatalf("send once placed within the wait: %v", err)
	}
	// Placed and withheld: refused at once, as before.
	secret := sess("young-1", "claude", "/src/client", true)
	secret.Withheld = true
	p.set(secret)
	b.mu.Lock()
	b.presence = presenceCache{}
	b.mu.Unlock()
	start := time.Now()
	if _, err := b.Send(ctx, busproto.SendRequest{FromSession: "young-1", To: "abcd", Body: "y"}); !errors.As(err, &be) || !strings.Contains(be.Detail, "path rule") {
		t.Fatalf("send from a withheld session: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("a withheld session waited for placement")
	}
}

// A session the device knows but the server has not had in a poll yet
// is reported at once and the send asked again, within PlaceWait.
func TestSendBeforeFirstPresenceReport(t *testing.T) {
	srv := newFakeServer()
	srv.sendFn = func(req busproto.SendRequest) error {
		for _, s := range srv.polls[len(srv.polls)-1].Sessions {
			if s.SessionID == req.FromSession {
				return nil
			}
		}
		return &busproto.Error{Status: 403, Code: busproto.CodeSessionNotOnDevice, Detail: "session " + req.FromSession + " is not on this device"}
	}
	p := &presenceSrc{}
	p.set(sess("open-1", "claude", "/src/api", true))
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(srv, nil), p)
	run(t, b)
	waitFor(t, "the first poll", func() bool { return srv.pollCount() > 0 })
	p.set(sess("open-1", "claude", "/src/api", true), sess("new-3", "claude", "/src/api", true))
	start := time.Now()
	if _, err := b.Send(ctx, busproto.SendRequest{FromSession: "new-3", To: "abcd", Body: "x"}); err != nil {
		t.Fatalf("send before the first presence report: %v", err)
	}
	if d := time.Since(start); d > PlaceWait {
		t.Fatalf("took %s", d)
	}
}

// A hook's pending call for a session presence does not list yet reports
// it now, not at the next presence check.
func TestNudgeRepolls(t *testing.T) {
	srv := newFakeServer()
	p := &presenceSrc{}
	p.set(sess("open-1", "claude", "/src/api", true))
	cfg := testConfig(srv, nil)
	cfg.PresenceEvery = time.Hour // no presence check during the test
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), cfg, p)
	run(t, b)
	waitFor(t, "the first poll", func() bool { return srv.pollCount() > 0 })
	p.set(sess("open-1", "claude", "/src/api", true), sess("new-4", "claude", "/src/api", true))
	b.Nudge("open-1", "claude") // listed: nothing to do
	time.Sleep(400 * time.Millisecond)
	if n := srv.pollCount(); n != 1 {
		t.Fatalf("a nudge for a listed session repolled: %d polls", n)
	}
	b.Nudge("new-4", "claude")
	waitFor(t, "a poll reporting new-4", func() bool {
		return slices.ContainsFunc(srv.lastPoll().Sessions, func(s busproto.PresenceSession) bool { return s.SessionID == "new-4" })
	})
}

// Without a server, a session that sends before presence lists it is
// waited for too.
func TestLocalSendFromNewSessionWaits(t *testing.T) {
	p := &presenceSrc{}
	p.set(sess("bbbb3333", "claude", "/src/web", false))
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(nil, nil), p)
	go func() {
		time.Sleep(150 * time.Millisecond)
		p.set(sess("bbbb3333", "claude", "/src/web", false), sess("aaaa1111", "claude", "/src/api", true))
	}()
	if _, err := b.Send(ctx, busproto.SendRequest{FromSession: "aaaa1111", To: "bbbb", Body: "x"}); err != nil {
		t.Fatalf("local send from a new session: %v", err)
	}
}

// A session the server refuses although a poll reported it (and one that
// presence never reports, so no poll can) does not cost a repoll and a
// PlaceWait on every send: the wait is bounded, and later sends return
// the server's refusal at once.
func TestSendRefusedAfterReportDoesNotRepollEachSend(t *testing.T) {
	shortPlaceWait(t, 400*time.Millisecond)
	srv := newFakeServer()
	srv.sendFn = func(req busproto.SendRequest) error {
		return &busproto.Error{Status: 403, Code: busproto.CodeSessionNotOnDevice, Detail: "session " + req.FromSession + " is not on this device"}
	}
	p := &presenceSrc{}
	p.set(sess("stuck-1", "claude", "/src/api", true))
	cfg := testConfig(srv, nil)
	cfg.PresenceEvery = time.Hour // only a repoll starts a poll
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), cfg, p)
	run(t, b)
	waitFor(t, "the first poll", func() bool { return srv.pollCount() > 0 })
	before := srv.pollCount()
	var be *busproto.Error
	for i := range 4 {
		start := time.Now()
		_, err := b.Send(ctx, busproto.SendRequest{FromSession: "stuck-1", To: "abcd", Body: fmt.Sprint("x", i)})
		if !errors.As(err, &be) || be.Code != busproto.CodeSessionNotOnDevice {
			t.Fatalf("send %d: %v", i, err)
		}
		if d := time.Since(start); i > 0 && d > 150*time.Millisecond {
			t.Fatalf("send %d from a session the server refused after a report waited %s", i, d)
		}
	}
	if n := srv.pollCount() - before; n > 1 {
		t.Fatalf("4 refused sends started %d polls", n)
	}

	// Known but never in presence (not live): no poll can report it.
	p.set()
	b.mu.Lock()
	b.presence = presenceCache{}
	b.mu.Unlock()
	b.SetSources(p.get, func(context.Context, string) ([]Session, error) {
		return []Session{sess("quiet-1", "claude", "/src/api", false)}, nil
	})
	before = srv.pollCount()
	start := time.Now()
	if _, err := b.Send(ctx, busproto.SendRequest{FromSession: "quiet-1", To: "abcd", Body: "y"}); !errors.As(err, &be) || be.Code != busproto.CodeSessionNotOnDevice {
		t.Fatalf("send from a known, not live session: %v", err)
	}
	if d := time.Since(start); d > 150*time.Millisecond {
		t.Fatalf("a session no poll can report waited %s", d)
	}
	time.Sleep(100 * time.Millisecond)
	if n := srv.pollCount() - before; n != 0 {
		t.Fatalf("a session no poll can report started %d polls", n)
	}
}

// A hook's nudge for a session presence never lists (one the agent does
// not track) does not restart the server's long poll on every hook call.
func TestNudgeUnlistedSessionBounded(t *testing.T) {
	srv := newFakeServer()
	p := &presenceSrc{}
	p.set(sess("open-1", "claude", "/src/api", true))
	cfg := testConfig(srv, nil)
	cfg.PresenceEvery = time.Hour
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), cfg, p)
	run(t, b)
	waitFor(t, "the first poll", func() bool { return srv.pollCount() > 0 })
	before := srv.pollCount()
	for range 10 {
		b.Nudge("ghost-1", "codex")
		time.Sleep(60 * time.Millisecond)
	}
	if n := srv.pollCount() - before; n > 1 {
		t.Fatalf("10 nudges for an unlisted session in 600ms started %d polls", n)
	}
}

// Without a server, a new session the index already holds (Known) but
// presence does not list yet is waited for until presence lists it:
// sendLocal takes the sender from presence.
func TestLocalSendFromIndexedSessionWaitsForPresence(t *testing.T) {
	p := &presenceSrc{}
	p.set(sess("bbbb3333", "claude", "/src/web", false))
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(nil, nil), p)
	b.SetSources(p.get, func(_ context.Context, prefix string) ([]Session, error) {
		if strings.HasPrefix("aaaa1111", prefix) {
			return []Session{sess("aaaa1111", "claude", "/src/api", true)}, nil
		}
		return nil, nil
	})
	go func() {
		time.Sleep(150 * time.Millisecond)
		p.set(sess("bbbb3333", "claude", "/src/web", false), sess("aaaa1111", "claude", "/src/api", true))
	}()
	if _, err := b.Send(ctx, busproto.SendRequest{FromSession: "aaaa1111", To: "bbbb", Body: "x"}); err != nil {
		t.Fatalf("local send from a session the index holds before presence lists it: %v", err)
	}
}

// CleanRef leaves a transcript path without newline or tab as it is, and
// the withheld check sees each ref as it leaves (cleaned): control
// characters or a newline around a withheld session's id do not hide it.
func TestCleanRefKeepsWithheldCheck(t *testing.T) {
	for _, r := range []string{"/Users/g/My Project/.claude/projects/-src-api/4c19e0d2.jsonl:3", "4c19e0d2/12:4", "~/x y/z.jsonl"} {
		if got := CleanRef(r); got != r {
			t.Fatalf("CleanRef(%q) = %q", r, got)
		}
	}
	srv := newFakeServer()
	p := &presenceSrc{}
	p.set(sess("open-1", "claude", "/src/api", true))
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(srv, nil), p)
	const secret = "5ec2e7aa-0000-4000-8000-000000000001"
	b.SetWithheld(func(_ context.Context, ref string) (string, error) {
		if strings.HasPrefix(ref, "5ec2e7aa") {
			return secret, nil
		}
		return "", nil
	}, nil)
	for _, ref := range []string{"\n5ec2e7aa/28672", "\x005ec2e7aa\t/1", "5ec2\x01e7aa/2"} {
		var be *busproto.Error
		if _, err := b.Send(ctx, busproto.SendRequest{FromSession: "open-1", To: "@alex", Body: "x", Refs: []string{ref}}); !errors.As(err, &be) || be.Code != busproto.CodeWithheldSession {
			t.Fatalf("ref %q: %v", ref, err)
		}
	}
	if srv.sendCount() != 0 {
		t.Fatal("a ref naming a withheld session reached the server")
	}
}
