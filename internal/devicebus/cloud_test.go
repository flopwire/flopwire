package devicebus

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/busrender"
	"github.com/flopwire/flopwire/internal/vendorcloud"
)

// fakeCloud is a vendor adapter the test drives: what List returns, what
// a push answers, and what the vendor's record shows read.
type fakeCloud struct {
	agent  string
	mu     sync.Mutex
	list   []vendorcloud.Session
	pushFn func(id, text string) (vendorcloud.Pushed, error)
	pushes []string // the texts pushed
	seen   map[string]time.Time
	reader bool
}

func (f *fakeCloud) Agent() string { return f.agent }

func (f *fakeCloud) List(context.Context) ([]vendorcloud.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.list), nil
}

func (f *fakeCloud) set(s ...vendorcloud.Session) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.list = s
}

func (f *fakeCloud) Push(_ context.Context, id, text string) (vendorcloud.Pushed, error) {
	f.mu.Lock()
	f.pushes = append(f.pushes, text)
	fn := f.pushFn
	f.mu.Unlock()
	if fn == nil {
		return vendorcloud.Pushed{}, nil
	}
	return fn(id, text)
}

func (f *fakeCloud) pushed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.pushes)
}

// readerCloud is a fakeCloud with a record to read (vendorcloud.Reader).
type readerCloud struct{ *fakeCloud }

func (r readerCloud) Seen(_ context.Context, _ string, ids []string) (map[string]time.Time, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]time.Time{}
	for _, id := range ids {
		if at, ok := r.seen[id]; ok {
			out[id] = at
		}
	}
	return out, nil
}

func cloudSess(id string, running bool) vendorcloud.Session {
	return vendorcloud.Session{Agent: "claude", ID: id, Title: "cloud task", Repo: "acme/api", Branch: "claude/x", Running: running}
}

// newCloudBus is a bus without a server, with three local sessions and
// the cloud adapter a.
func newCloudBus(t *testing.T, a vendorcloud.Adapter) *Bus {
	t.Helper()
	cfg := testConfig(nil, nil)
	cfg.Cloud, cfg.CloudEvery = []vendorcloud.Adapter{a}, 30*time.Millisecond
	p := &presenceSrc{}
	p.set(sess("aaaa1111", "claude", "/src/api", true), sess("bbbb3333", "claude", "/src/web", false))
	return openBus(t, filepath.Join(t.TempDir(), "bus.db"), cfg, p)
}

func cloudSent(t *testing.T, b *Bus, from, id string) busproto.InboxItem {
	t.Helper()
	in, err := b.Inbox(ctx, busproto.InboxQuery{Session: from, SentOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range in.Messages {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("%s is not in %s's sent messages", id, from)
	return busproto.InboxItem{}
}

// A cloud session is listed in peers and addressable; a message to it is
// pushed only while the vendor reports a turn running, framed by the
// cloud instruction, and is then delivered.
func TestCloudPushOnlyWhileRunning(t *testing.T) {
	fc := &fakeCloud{agent: "claude"}
	fc.set(cloudSess("session_01idle", false))
	b := newCloudBus(t, fc)
	run(t, b)
	waitFor(t, "the cloud listing", func() bool { return len(b.CloudSessions()) == 1 })
	peers, err := b.Peers(ctx, busproto.PeersQuery{Session: "aaaa1111", Repo: "api"})
	if err != nil || !slices.ContainsFunc(peers.Peers, func(p busproto.Peer) bool { return p.Session == "session_01idle" && p.Cloud && p.Device == "" }) {
		t.Fatalf("peers = %+v, %v", peers, err)
	}
	out, err := b.Send(ctx, busproto.SendRequest{FromSession: "aaaa1111", To: "session_01", Body: "please rebase", Intent: "request"})
	if err != nil {
		t.Fatal(err)
	}
	if !out.To.Cloud || !out.To.Live || out.To.Busy || out.To.Session != "session_01idle" {
		t.Fatalf("receipt: %+v", out.To)
	}
	time.Sleep(200 * time.Millisecond) // several ticks
	if got := fc.pushed(); len(got) != 0 {
		t.Fatalf("pushed into an idle session: %q", got)
	}
	if st := cloudSent(t, b, "aaaa1111", out.ID); st.State != busproto.StateQueued {
		t.Fatalf("state while idle = %s", st.State)
	}
	fc.set(cloudSess("session_01idle", true))
	waitFor(t, "the push", func() bool { return len(fc.pushed()) == 1 })
	text := fc.pushed()[0]
	if !strings.HasPrefix(text, busrender.CloudInstruction("claude")) || !strings.Contains(text, `<flopwire-message id="`+out.ID+`"`) || strings.Contains(text, "Reply with") {
		t.Fatalf("pushed text:\n%s", text)
	}
	waitFor(t, "delivered", func() bool { return cloudSent(t, b, "aaaa1111", out.ID).State == busproto.StateDelivered })
	time.Sleep(100 * time.Millisecond)
	if n := len(fc.pushed()); n != 1 {
		t.Fatalf("pushed %d times", n)
	}
}

// A failed push is retried on the next tick, marked a redelivery, and
// after MaxAttempts failures the message is undelivered (push_failed).
func TestCloudPushFailureRetriesThenUndelivered(t *testing.T) {
	fc := &fakeCloud{agent: "claude", pushFn: func(string, string) (vendorcloud.Pushed, error) {
		return vendorcloud.Pushed{}, errors.New("vendor down")
	}}
	fc.set(cloudSess("session_01fail", true))
	b := newCloudBus(t, fc)
	run(t, b)
	waitFor(t, "the cloud listing", func() bool { return len(b.CloudSessions()) == 1 })
	out, err := b.Send(ctx, busproto.SendRequest{FromSession: "aaaa1111", To: "session_01fail", Body: "x"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "undelivered", func() bool { return cloudSent(t, b, "aaaa1111", out.ID).State == busproto.StateUndelivered })
	if st := cloudSent(t, b, "aaaa1111", out.ID); st.Reason != busproto.ReasonPushFailed {
		t.Fatalf("reason = %q", st.Reason)
	}
	n := takeFailures(t, b, "aaaa1111", "claude")
	if len(n) != 1 || n[0].ID != out.ID || n[0].Reason != "push_failed" {
		t.Fatalf("cloud sender status: %+v", n)
	}
	got := fc.pushed()
	if len(got) != MaxAttempts {
		t.Fatalf("pushed %d times, want %d", len(got), MaxAttempts)
	}
	// The instruction explains the attribute; the wrapper carries it.
	marked := func(text string) bool { return strings.Contains(text, `" redelivery="true" sent=`) }
	if marked(got[0]) || !marked(got[1]) || !marked(got[2]) {
		t.Fatal("retries are not marked as redeliveries")
	}
}

// A push the vendor refuses because the session is archived or exited
// ends the session's waiting messages (session_ended).
func TestCloudPushToGoneSession(t *testing.T) {
	fc := &fakeCloud{agent: "claude", pushFn: func(string, string) (vendorcloud.Pushed, error) {
		return vendorcloud.Pushed{}, vendorcloud.ErrGone
	}}
	fc.set(cloudSess("session_01gone", true))
	b := newCloudBus(t, fc)
	run(t, b)
	waitFor(t, "the cloud listing", func() bool { return len(b.CloudSessions()) == 1 })
	out, err := b.Send(ctx, busproto.SendRequest{FromSession: "aaaa1111", To: "session_01gone", Body: "x"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "undelivered", func() bool { return cloudSent(t, b, "aaaa1111", out.ID).State == busproto.StateUndelivered })
	if st := cloudSent(t, b, "aaaa1111", out.ID); st.Reason != busproto.ReasonSessionEnded {
		t.Fatalf("reason = %q", st.Reason)
	}
	if n := len(fc.pushed()); n != 1 {
		t.Fatalf("pushed %d times into a gone session", n)
	}
}

// Read receipts: from the push itself (Devin's echo and first output), or
// later from the vendor's record (Claude's session events).
func TestCloudReadReceipts(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Millisecond)
	fc := &fakeCloud{agent: "claude", pushFn: func(string, string) (vendorcloud.Pushed, error) { return vendorcloud.Pushed{ReadAt: at}, nil }}
	fc.set(cloudSess("session_01read", true))
	b := newCloudBus(t, fc)
	run(t, b)
	waitFor(t, "the cloud listing", func() bool { return len(b.CloudSessions()) == 1 })
	out, _ := b.Send(ctx, busproto.SendRequest{FromSession: "aaaa1111", To: "session_01read", Body: "x"})
	waitFor(t, "read from the push", func() bool { return cloudSent(t, b, "aaaa1111", out.ID).State == busproto.StateRead })

	rc := readerCloud{&fakeCloud{agent: "claude", seen: map[string]time.Time{}}}
	rc.set(cloudSess("session_01rec", true))
	b2 := newCloudBus(t, rc)
	run(t, b2)
	waitFor(t, "the cloud listing", func() bool { return len(b2.CloudSessions()) == 1 })
	out2, _ := b2.Send(ctx, busproto.SendRequest{FromSession: "aaaa1111", To: "session_01rec", Body: "y"})
	waitFor(t, "delivered", func() bool { return cloudSent(t, b2, "aaaa1111", out2.ID).State == busproto.StateDelivered })
	rc.mu.Lock()
	rc.seen[out2.ID] = time.Now().UTC()
	rc.mu.Unlock()
	waitFor(t, "read from the record", func() bool { return cloudSent(t, b2, "aaaa1111", out2.ID).State == busproto.StateRead })
}

// With a server: the device reports its cloud sessions in the poll, claims
// a cloud message only while the session runs a turn, pushes it, and
// acknowledges it; a push that keeps failing is reported push_failed.
func TestCloudClaimPushAndAckWithServer(t *testing.T) {
	srv := newFakeServer()
	srv.claimFn = func(req busproto.ClaimRequest) (busproto.ClaimResponse, error) {
		e := env(req.MessageID, req.SessionID)
		return busproto.ClaimResponse{Message: e}, nil
	}
	fc := &fakeCloud{agent: "claude"}
	fc.set(cloudSess("session_01srv", false))
	cfg := testConfig(srv, nil)
	cfg.Cloud, cfg.CloudEvery = []vendorcloud.Adapter{fc}, 30*time.Millisecond
	p := &presenceSrc{}
	p.set(sess("aaaa1111", "claude", "/src/api", true))
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), cfg, p)
	run(t, b)
	waitFor(t, "a poll with the cloud session", func() bool {
		return srv.pollCount() > 0 && slices.ContainsFunc(srv.lastPoll().Cloud, func(s busproto.PresenceSession) bool { return s.SessionID == "session_01srv" })
	})
	if got := srv.lastPoll().Cloud[0]; got.Busy || got.Repo != "acme/api" || got.Agent != "claude" {
		t.Fatalf("cloud presence = %+v", got)
	}
	m := env("mcloud", "session_01srv")
	offer := busproto.PollResponse{Cursor: 3, Messages: []busproto.Envelope{}, Claimable: []busproto.Claimable{{Message: m, Sessions: []string{"session_01srv"}, Cloud: true}}}
	srv.pollCh <- pollReply{resp: offer}
	time.Sleep(150 * time.Millisecond)
	srv.mu.Lock()
	claims := len(srv.claims)
	srv.mu.Unlock()
	if claims != 0 {
		t.Fatal("claimed a message for a session that runs no turn")
	}
	// The session starts a turn: the changed presence repolls, the offer
	// comes again, and the device claims and pushes.
	fc.set(cloudSess("session_01srv", true))
	waitFor(t, "a poll with the running session", func() bool {
		c := srv.lastPoll().Cloud
		return len(c) == 1 && c[0].Busy
	})
	srv.pollCh <- pollReply{resp: offer}
	waitFor(t, "the push", func() bool { return len(fc.pushed()) == 1 })
	waitFor(t, "the receipt", func() bool { return slices.Contains(srv.ackedIDs(), "mcloud") })
	srv.mu.Lock()
	c := srv.claims[0]
	srv.mu.Unlock()
	if c.SessionID != "session_01srv" || c.Agent != "claude" {
		t.Fatalf("claim = %+v", c)
	}

	fc.mu.Lock()
	fc.pushFn = func(string, string) (vendorcloud.Pushed, error) {
		return vendorcloud.Pushed{}, errors.New("vendor down")
	}
	fc.mu.Unlock()
	m2 := env("mcloud2", "session_01srv")
	srv.pollCh <- pollReply{resp: busproto.PollResponse{Cursor: 4, Messages: []busproto.Envelope{m}, Claimable: []busproto.Claimable{{Message: m2, Sessions: []string{"session_01srv"}, Cloud: true}}}}
	waitFor(t, "the push_failed report", func() bool {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return slices.Contains(srv.failed, "mcloud2")
	})
}

// A cloud session on a repository whose name the path rules withhold is
// not listed: nothing about it may reach the server.
func TestCloudSessionOnWithheldRepoIsNotListed(t *testing.T) {
	fc := &fakeCloud{agent: "claude"}
	secret := cloudSess("session_01secret", true)
	secret.Repo = "acme/secret"
	fc.set(cloudSess("session_01open", true), secret)
	b := newCloudBus(t, fc)
	var asked []string
	var mu sync.Mutex
	b.SetWithheld(nil, func(_ context.Context, repo string) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, repo)
		return repo == "secret", nil
	})
	run(t, b)
	waitFor(t, "the cloud listing", func() bool { return len(b.CloudSessions()) == 1 })
	if got := b.CloudSessions()[0].SessionID; got != "session_01open" {
		t.Fatalf("listed %s", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(asked, "secret") {
		t.Fatalf("the path rules were asked about %q, want the repo name", asked)
	}
}

// A device that dies after leasing a cloud message for a push, before the
// vendor answered, pushes it again after it restarts and the lease ends:
// once, marked a redelivery (the vendor may have taken the first push),
// and then it is delivered. Nothing is lost while the device returns.
func TestCloudPushAfterDyingMidPush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bus.db")
	var mu sync.Mutex
	at := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return at }
	cfg := testConfig(nil, nil)
	cfg.Now = clock
	fc := &fakeCloud{agent: "claude"}
	fc.set(cloudSess("session_01dies", true))
	cfg.Cloud, cfg.CloudEvery = []vendorcloud.Adapter{fc}, 30*time.Millisecond
	p := &presenceSrc{}
	p.set(sess("aaaa1111", "claude", "/src/api", true))
	b1, err := Open(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	b1.SetSources(p.get, func(context.Context, string) ([]Session, error) { return nil, nil })
	b1.listCloud(ctx)
	out, err := b1.Send(ctx, busproto.SendRequest{FromSession: "aaaa1111", To: "session_01dies", Body: "please rebase", Intent: "inform"})
	if err != nil {
		t.Fatal(err)
	}
	// The push's lease is taken; the device dies before the vendor answers.
	lim := Limit{Count: busrender.HookMessages, Bytes: busrender.HookBytes, Sep: busrender.SepLen, Size: busrender.Size}
	if _, got, err := b1.st.take(ctx, "session_01dies", "claude", clock(), lim, b1.cloudLease(), b1.cfg.MaxAttempts, nil); err != nil || len(got) != 1 {
		t.Fatalf("take = %v, %v", got, err)
	}
	b1.Close()

	mu.Lock()
	at = at.Add(b1.cloudLease() + time.Second)
	mu.Unlock()
	b2 := openBus(t, path, cfg, p)
	run(t, b2)
	waitFor(t, "delivered after the restart", func() bool { return cloudSent(t, b2, "aaaa1111", out.ID).State == busproto.StateDelivered })
	got := fc.pushed()
	if len(got) != 1 || !strings.Contains(got[0], `redelivery="true"`) {
		t.Fatalf("pushes after the restart = %q, want one marked a redelivery", got)
	}
}

// The listing is up to CloudEvery old. A message due for a session the
// listing shows running is pushed only after the vendor confirms a turn
// still runs: a push into a session whose turn ended since would start
// a turn (B3).
func TestCloudPushRechecksTheTurnBeforePushing(t *testing.T) {
	fc := &fakeCloud{agent: "claude"}
	fc.set(cloudSess("session_01ended", true))
	cfg := testConfig(nil, nil)
	cfg.Cloud, cfg.CloudEvery = []vendorcloud.Adapter{fc}, time.Hour
	p := &presenceSrc{}
	p.set(sess("aaaa1111", "claude", "/src/api", true))
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), cfg, p)
	run(t, b)
	waitFor(t, "the cloud listing", func() bool { return len(b.CloudSessions()) == 1 })
	fc.set(cloudSess("session_01ended", false)) // the turn ended after the listing
	out, err := b.Send(ctx, busproto.SendRequest{FromSession: "aaaa1111", To: "session_01ended", Body: "x"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // several ticks
	if got := fc.pushed(); len(got) != 0 {
		t.Fatalf("pushed into a session whose turn had ended: %d pushes", len(got))
	}
	if st := cloudSent(t, b, "aaaa1111", out.ID); st.State != busproto.StateQueued {
		t.Fatalf("state = %s", st.State)
	}
	fc.set(cloudSess("session_01ended", true))
	b.listCloud(ctx) // the next listing
	waitFor(t, "the push once a turn runs", func() bool { return len(fc.pushed()) == 1 })
}
