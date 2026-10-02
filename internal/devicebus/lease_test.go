package devicebus

import (
	"database/sql"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
)

// A hook that took a message and died before printing it (killed by the
// harness's timeout, or anything else) never confirms it. The message is
// not lost: after the lease it is offered to the session's next hook,
// marked as a redelivery.
func TestTakenButNeverConfirmedIsOfferedAgain(t *testing.T) {
	lb := newLocalBus(t)
	out, err := lb.send(t, "aaaa1111", "bbbb", "the message a killed hook took")
	if err != nil {
		t.Fatal(err)
	}
	got, err := lb.Take(ctx, "bbbb3333", "", Limit{})
	if err != nil || len(got) != 1 || got[0].ID != out.ID || got[0].Attempt != 1 {
		t.Fatalf("first take: %+v %v", got, err)
	}
	// The hook is killed here: no print, no confirmation. Within the lease
	// nobody else gets it.
	lb.advance(LeaseFor - time.Second)
	if again, _ := lb.Take(ctx, "bbbb3333", "", Limit{}); len(again) != 0 {
		t.Fatalf("offered again inside the lease: %v", ids(again))
	}
	lb.advance(time.Second)
	again, err := lb.Take(ctx, "bbbb3333", "", Limit{})
	if err != nil || len(again) != 1 || again[0].ID != out.ID {
		t.Fatalf("after the lease: %v %v (the message was lost)", ids(again), err)
	}
	if again[0].Attempt != 2 {
		t.Fatalf("redelivery not marked: attempt %d", again[0].Attempt)
	}
}

// Take then Confirm: the message is delivered, the sender sees it
// delivered, and it is never offered again, also after the lease time.
func TestLeaseConfirmDelivers(t *testing.T) {
	lb := newLocalBus(t)
	out, err := lb.send(t, "aaaa1111", "bbbb", "hello")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := lb.Take(ctx, "bbbb3333", "", Limit{})
	if len(got) != 1 {
		t.Fatalf("take: %v", ids(got))
	}
	// Leased is not delivered: the sender still sees it queued, and status
	// counts it pending.
	if st := sentState(t, lb, "aaaa1111", out.ID); st != busproto.StateQueued {
		t.Fatalf("sender sees a leased message as %s", st)
	}
	if st := lb.Status(ctx); st.Pending != 1 {
		t.Fatalf("pending while leased: %d", st.Pending)
	}
	if err := lb.Confirm(ctx, "bbbb3333", []string{out.ID}); err != nil {
		t.Fatal(err)
	}
	if st := sentState(t, lb, "aaaa1111", out.ID); st != busproto.StateDelivered {
		t.Fatalf("sender sees a confirmed message as %s", st)
	}
	lb.advance(2 * LeaseFor)
	lb.expireLeases(ctx)
	if again, _ := lb.Take(ctx, "bbbb3333", "", Limit{}); len(again) != 0 {
		t.Fatalf("delivered twice: %v", ids(again))
	}
	// A confirmation names only its own session's messages.
	out2, _ := lb.send(t, "aaaa1111", "bbbb", "second")
	lb.Take(ctx, "bbbb3333", "", Limit{})
	if err := lb.Confirm(ctx, "aaaa2222", []string{out2.ID}); err != nil {
		t.Fatal(err)
	}
	if st := sentState(t, lb, "aaaa1111", out2.ID); st != busproto.StateQueued {
		t.Fatalf("another session confirmed it: %s", st)
	}
}

func sentState(t *testing.T, lb *localBus, session, id string) busproto.State {
	t.Helper()
	in, err := lb.Inbox(ctx, busproto.InboxQuery{Session: session, SentOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range in.Messages {
		if m.ID == id {
			return m.State
		}
	}
	t.Fatalf("%s not in %s's inbox", id, session)
	return ""
}

// The hook printed the message and its confirmation was lost (or each hook
// was killed): the message is offered again, marked, at most MaxAttempts
// times in all. Then it is undelivered, the sender's inbox says so, and no
// hook gets it again. The loop's tick settles the last lease without
// another hook.
func TestUnconfirmedLeasesEndUndelivered(t *testing.T) {
	lb := newLocalBus(t)
	out, err := lb.send(t, "aaaa1111", "bbbb", "never confirmed")
	if err != nil {
		t.Fatal(err)
	}
	var attempts []int
	for range MaxAttempts + 2 {
		for _, e := range func() []busproto.Envelope { g, _ := lb.Take(ctx, "bbbb3333", "", Limit{}); return g }() {
			attempts = append(attempts, e.Attempt)
		}
		lb.advance(LeaseFor)
		lb.expireLeases(ctx)
	}
	if !slices.Equal(attempts, []int{1, 2, 3}) {
		t.Fatalf("offers: %v, want attempts 1, 2, 3", attempts)
	}
	in, err := lb.Inbox(ctx, busproto.InboxQuery{Session: "aaaa1111", SentOnly: true})
	if err != nil || len(in.Messages) != 1 {
		t.Fatalf("sender inbox: %+v %v", in, err)
	}
	if m := in.Messages[0]; m.ID != out.ID || m.State != busproto.StateUndelivered || m.Reason != busproto.ReasonUnconfirmed || m.DeliveredAt != nil {
		t.Fatalf("sender sees %s (%s)", m.State, m.Reason)
	}
	if st := lb.Status(ctx); st.Pending != 0 {
		t.Fatalf("an undelivered message counts as pending: %d", st.Pending)
	}
	// It no longer counts against the recipient's undelivered cap, and the
	// sender can send it again.
	if _, err := lb.send(t, "aaaa1111", "bbbb", "never confirmed, again"); err != nil {
		t.Fatal(err)
	}
}

// A message taken by a hook whose lease is outstanding holds back the
// session's newer messages: when the lease ends, the old message comes
// first, then the newer ones, in one call, each once.
func TestLeaseKeepsOrder(t *testing.T) {
	lb := newLocalBus(t)
	m1, _ := lb.send(t, "aaaa1111", "bbbb", "first")
	if got, _ := lb.Take(ctx, "bbbb3333", "", Limit{}); !slices.Equal(ids(got), []string{m1.ID}) {
		t.Fatalf("take: %v", ids(got))
	}
	lb.advance(time.Second)
	m2, _ := lb.send(t, "aaaa1111", "bbbb", "second")
	lb.advance(time.Second)
	m3, _ := lb.send(t, "aaaa2222", "bbbb", "third")
	if got, _ := lb.Take(ctx, "bbbb3333", "", Limit{}); len(got) != 0 {
		t.Fatalf("newer messages overtook a leased one: %v", ids(got))
	}
	lb.advance(LeaseFor)
	got, err := deliver(lb.Bus, "bbbb3333", "", Limit{})
	if err != nil || !slices.Equal(ids(got), []string{m1.ID, m2.ID, m3.ID}) {
		t.Fatalf("after the lease: %v %v", ids(got), err)
	}
	if got[0].Attempt != 2 || got[1].Attempt != 1 || got[2].Attempt != 1 {
		t.Fatalf("attempts: %d %d %d", got[0].Attempt, got[1].Attempt, got[2].Attempt)
	}
	// Another session's messages are not held back by this session's lease.
	m4, _ := lb.send(t, "aaaa1111", "bbbb", "fourth")
	lb.Take(ctx, "bbbb3333", "", Limit{})
	o, _ := lb.send(t, "bbbb3333", "aaaa2222", "to the other session")
	if got, _ := lb.Take(ctx, "aaaa2222", "", Limit{}); !slices.Equal(ids(got), []string{o.ID}) {
		t.Fatalf("other session: %v (lease on %s)", ids(got), m4.ID)
	}
}

// A confirmation that arrives after the lease ended still counts when no
// other hook took the message meanwhile: the print happened.
func TestLateConfirmCounts(t *testing.T) {
	lb := newLocalBus(t)
	out, _ := lb.send(t, "aaaa1111", "bbbb", "slow hook")
	lb.Take(ctx, "bbbb3333", "", Limit{})
	lb.advance(LeaseFor + time.Second)
	lb.expireLeases(ctx)
	if err := lb.Confirm(ctx, "bbbb3333", []string{out.ID}); err != nil {
		t.Fatal(err)
	}
	if got, _ := lb.Take(ctx, "bbbb3333", "", Limit{}); len(got) != 0 {
		t.Fatalf("offered again after a late confirmation: %v", ids(got))
	}
	if st := sentState(t, lb, "aaaa1111", out.ID); st != busproto.StateDelivered {
		t.Fatalf("state %s", st)
	}
}

// Hooks of one session run at once (two hook configs, Devin and a Claude
// settings file): each message goes to one of them, once, and the others
// get nothing while its lease is out.
func TestConcurrentHooksOneLease(t *testing.T) {
	lb := newLocalBus(t)
	for i := range 4 {
		if _, err := lb.send(t, "aaaa1111", "bbbb", "concurrent "+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var takers int
	seen := map[string]int{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			got, err := lb.Take(ctx, "bbbb3333", "", Limit{})
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(got) > 0 {
				takers++
			}
			for _, e := range got {
				seen[e.ID]++
			}
		})
	}
	wg.Wait()
	if takers != 1 || len(seen) != 4 {
		t.Fatalf("%d hooks took messages, %d distinct", takers, len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("%s taken %d times", id, n)
		}
	}
}

// With a server, the receipt (delivered_at) goes out only after the
// confirmation, and a message no hook confirmed is reported undelivered.
func TestServerAckAfterConfirmAndUndeliveredReport(t *testing.T) {
	srv := newFakeServer()
	cfg := testConfig(srv, nil)
	cfg.Lease = 30 * time.Millisecond
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), cfg, &presenceSrc{})
	if err := b.st.reconcile(ctx, []busproto.Envelope{env("mc", "s1"), env("mu", "s2")}, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	run(t, b)
	if got, _ := b.Take(ctx, "s1", "", Limit{}); !slices.Equal(ids(got), []string{"mc"}) {
		t.Fatalf("take: %v", ids(got))
	}
	time.Sleep(10 * cfg.AckDelay)
	if a := srv.ackedIDs(); len(a) != 0 {
		t.Fatalf("receipt before the confirmation: %v", a)
	}
	if err := b.Confirm(ctx, "s1", []string{"mc"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the receipt after the confirmation", func() bool { return slices.Equal(srv.ackedIDs(), []string{"mc"}) })

	// mu: taken MaxAttempts times, never confirmed.
	for i := range MaxAttempts {
		waitFor(t, "the next lease", func() bool {
			got, _ := b.Take(ctx, "s2", "", Limit{})
			return len(got) == 1 && got[0].Attempt == i+1
		})
	}
	waitFor(t, "the undelivered report", func() bool { return slices.Contains(srv.undeliveredIDs(), "mu") })
	if slices.Contains(srv.ackedIDs(), "mu") {
		t.Fatal("an unconfirmed message was acknowledged as delivered")
	}
	waitFor(t, "the report settled", func() bool { n, _ := b.st.owed(ctx, "report", 10); return len(n) == 0 })
}

// An inbox from an earlier build (another schema version) is recreated:
// the agent keeps messaging instead of failing on a missing column.
func TestInboxFromAnEarlierBuildIsRecreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bus.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE devbus_messages (id TEXT PRIMARY KEY, origin TEXT NOT NULL, state TEXT NOT NULL, refuse_reason TEXT);
		INSERT INTO devbus_messages VALUES('mold','local','queued','')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	lb := newLocalBus(t)
	b := openBus(t, path, lb.cfg, lb.p)
	if _, err := b.Send(ctx, busproto.SendRequest{FromSession: "aaaa1111", To: "bbbb", Body: "after the upgrade"}); err != nil {
		t.Fatal(err)
	}
	if got, err := deliver(b, "bbbb3333", "", Limit{}); err != nil || len(got) != 1 {
		t.Fatalf("take on a recreated inbox: %v %v", ids(got), err)
	}
}
