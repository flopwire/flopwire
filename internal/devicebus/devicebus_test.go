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
	ackFn    func([]string) (busproto.AckResponse, error)
	sends    []busproto.SendRequest
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
	f.acks = append(f.acks, slices.Clone(req.IDs))
	fn := f.ackFn
	f.mu.Unlock()
	if fn == nil {
		return busproto.AckResponse{Acked: req.IDs, Rejected: []string{}}, nil
	}
	return fn(req.IDs)
}

func (f *fakeServer) Send(_ context.Context, req busproto.SendRequest) (busproto.SendResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, req)
	return busproto.SendResponse{ID: "msent", State: busproto.StateQueued}, nil
}

func (f *fakeServer) Peers(context.Context, busproto.PeersQuery) (busproto.PeersResponse, error) {
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
	got, err := b.Pending(ctx, "s1", "")
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
	if got, _ := b.Pending(ctx, "s2", ""); !slices.Equal(ids(got), []string{"m4"}) {
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
	if got, _ := b.Pending(ctx, "s1", ""); len(got) != 0 {
		t.Fatalf("delivered message delivered again: %v", ids(got))
	}
	if owed, _ := b.st.owed(ctx, 10); !slices.Equal(owed, []string{"m1"}) {
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
	if got, _ := b.Pending(ctx, "s1", ""); !slices.Equal(ids(got), []string{"m5"}) {
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
	if got, _ := b.Pending(ctx, "s1", ""); !slices.Equal(ids(got), []string{"mnew"}) {
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
				got, err := b.Pending(ctx, "s1", "claude")
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
	if got, _ := b.Pending(ctx, "s1", ""); len(got) > 0 {
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
	if got, _ := b.Pending(ctx, "s1", ""); !slices.Equal(ids(got), []string{"ma"}) {
		t.Fatalf("pending: %v", ids(got))
	}
	b.Close() // stopped before the receipt went out

	p := &presenceSrc{}
	b2 := openBus(t, path, testConfig(srv, nil), p)
	if got, _ := b2.Pending(ctx, "s1", ""); len(got) != 0 {
		t.Fatalf("delivered message pending again after restart: %v", ids(got))
	}
	run(t, b2)
	waitFor(t, "the receipt owed before the restart", func() bool { return slices.Contains(srv.ackedIDs(), "ma") })
	if got, _ := b2.Pending(ctx, "s2", ""); !slices.Equal(ids(got), []string{"mb"}) {
		t.Fatalf("undelivered message lost in restart: %v", ids(got))
	}
}

// Receipts go in batches of at most busproto.MaxAck; rejected ids are not
// sent again.
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
	if got, _ := b.Pending(ctx, "s1", ""); len(got) != 150 {
		t.Fatalf("pending %d", len(got))
	}
	run(t, b)
	waitFor(t, "every receipt", func() bool { return len(srv.ackedIDs()) == 150 })
	srv.mu.Lock()
	batches := len(srv.acks)
	for _, a := range srv.acks {
		if len(a) > busproto.MaxAck {
			t.Errorf("a batch of %d ids", len(a))
		}
	}
	srv.mu.Unlock()
	if batches != 2 {
		t.Errorf("%d batches for 150 receipts", batches)
	}
	if owed, _ := b.st.owed(ctx, 200); len(owed) != 0 {
		t.Fatalf("still owed: %v", owed)
	}
	// A rejected id is not sent again.
	b.kickAcks()
	time.Sleep(50 * time.Millisecond)
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
	if got, _ := b.Pending(ctx, "here", ""); !slices.Equal(ids(got), []string{"mwon"}) {
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
	srv.pollCh <- pollReply{resp: busproto.PollResponse{Cursor: 9}}
	waitFor(t, "the second poll", func() bool { return srv.pollCount() == 2 })
	if srv.lastPoll().Cursor != 9 {
		t.Fatalf("cursor %d", srv.lastPoll().Cursor)
	}
	p.set(sess("s1", "claude", "/src/api", true), secret) // s1 turns busy
	waitFor(t, "a poll with the new presence", func() bool { return srv.pollCount() == 3 })
	last := srv.lastPoll()
	if len(last.Sessions) != 1 || !last.Sessions[0].Busy || last.Cursor != 0 {
		t.Fatalf("poll after the change: %+v", last)
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

// Requeue undoes a Pending whose caller never got the answer: the message
// is pending again and its receipt is no longer owed.
func TestRequeueWithdrawsTheReceipt(t *testing.T) {
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(newFakeServer(), nil), nil)
	if err := b.st.reconcile(ctx, []busproto.Envelope{env("mq", "s1")}, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Pending(ctx, "s1", ""); len(got) != 1 {
		t.Fatalf("pending: %v", ids(got))
	}
	if err := b.Requeue(ctx, []string{"mq"}); err != nil {
		t.Fatal(err)
	}
	if owed, _ := b.st.owed(ctx, 10); len(owed) != 0 {
		t.Fatalf("receipt still owed: %v", owed)
	}
	if got, _ := b.Pending(ctx, "s1", ""); !slices.Equal(ids(got), []string{"mq"}) {
		t.Fatalf("pending after requeue: %v", ids(got))
	}
	if owed, _ := b.st.owed(ctx, 10); !slices.Equal(owed, []string{"mq"}) {
		t.Fatalf("receipt after the second take: %v", owed)
	}
}
