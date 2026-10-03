package devicebus

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/perfguard"
)

// inboxItem is the message id in the session's inbox (sent or received).
func inboxItem(t *testing.T, b *Bus, session, id string) busproto.InboxItem {
	t.Helper()
	in, err := b.Inbox(ctx, busproto.InboxQuery{Session: session})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range in.Messages {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("%s not in %s's inbox", id, session)
	return busproto.InboxItem{}
}

// Local: a delivered message seen in the recipient's hook context is read,
// for the sender and the recipient, at the time the transcript recorded
// it. Only the first sighting counts.
func TestReadLocal(t *testing.T) {
	lb := newLocalBus(t)
	out, err := lb.send(t, "aaaa1111", "bbbb", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := deliver(lb.Bus, "bbbb3333", "", Limit{}); err != nil || len(got) != 1 {
		t.Fatalf("deliver: %v %v", ids(got), err)
	}
	lb.advance(3 * time.Second)
	seen := lb.now.Add(-time.Second)
	// The later sighting comes first in the batch (a redelivery printed it
	// again): the earliest wins.
	if err := lb.MarkRead(ctx, []Read{{Session: "bbbb3333", Agent: "claude", ID: out.ID, At: seen.Add(time.Second)}, {Session: "bbbb3333", Agent: "claude", ID: out.ID, At: seen}}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"aaaa1111", "bbbb3333"} {
		m := inboxItem(t, lb.Bus, s, out.ID)
		if m.State != busproto.StateRead || m.ReadAt == nil || !m.ReadAt.Equal(seen) || m.DeliveredAt == nil {
			t.Fatalf("%s sees %s read_at %v, want read at %v", s, m.State, m.ReadAt, seen)
		}
	}
	// A later sighting (the message printed again) changes nothing.
	if err := lb.MarkRead(ctx, []Read{{Session: "bbbb3333", Agent: "claude", ID: out.ID, At: seen.Add(-time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	if m := inboxItem(t, lb.Bus, "aaaa1111", out.ID); !m.ReadAt.Equal(seen) {
		t.Fatalf("read_at moved to %v", m.ReadAt)
	}
}

// Only the recipient session's own sighting of a message a hook took
// counts: not another session's, not another harness's session of the
// same id, not one of a message no hook ever took.
func TestReadNeedsTheRecipientsSighting(t *testing.T) {
	lb := newLocalBus(t)
	taken, _ := lb.send(t, "aaaa1111", "bbbb", "taken")
	if got, _ := deliver(lb.Bus, "bbbb3333", "", Limit{}); len(got) != 1 {
		t.Fatal("not delivered")
	}
	queued, _ := lb.send(t, "aaaa1111", "bbbb", "never taken")
	now := lb.now
	err := lb.MarkRead(ctx, []Read{
		{Session: "aaaa2222", Agent: "codex", ID: taken.ID, At: now},   // another session
		{Session: "bbbb3333", Agent: "codex", ID: taken.ID, At: now},   // another harness
		{Session: "bbbb3333", Agent: "claude", ID: queued.ID, At: now}, // no hook took it
		{Session: "bbbb3333", Agent: "claude", ID: "mnope", At: now},
	})
	if err != nil {
		t.Fatal(err)
	}
	if m := inboxItem(t, lb.Bus, "aaaa1111", taken.ID); m.State != busproto.StateDelivered || m.ReadAt != nil {
		t.Fatalf("taken: %s %v", m.State, m.ReadAt)
	}
	if m := inboxItem(t, lb.Bus, "aaaa1111", queued.ID); m.State != busproto.StateQueued || m.ReadAt != nil {
		t.Fatalf("queued: %s %v", m.State, m.ReadAt)
	}
}

// The transcript can show the message before the hook's confirmation
// arrives. The sighting is kept, and the message reads as read once it is
// delivered, never before, and never read before it was delivered.
func TestReadSeenWhileLeased(t *testing.T) {
	lb := newLocalBus(t)
	out, _ := lb.send(t, "aaaa1111", "bbbb", "hello")
	if got, _ := lb.Take(ctx, "bbbb3333", "", Limit{}); len(got) != 1 {
		t.Fatal("not taken")
	}
	seen := lb.now
	if err := lb.MarkRead(ctx, []Read{{Session: "bbbb3333", Agent: "claude", ID: out.ID, At: seen}}); err != nil {
		t.Fatal(err)
	}
	if m := inboxItem(t, lb.Bus, "aaaa1111", out.ID); m.State != busproto.StateQueued || m.ReadAt != nil {
		t.Fatalf("leased: %s %v", m.State, m.ReadAt)
	}
	lb.advance(time.Second)
	if err := lb.Confirm(ctx, "bbbb3333", []string{out.ID}); err != nil {
		t.Fatal(err)
	}
	m := inboxItem(t, lb.Bus, "aaaa1111", out.ID)
	if m.State != busproto.StateRead || m.ReadAt == nil || m.DeliveredAt == nil || !m.ReadAt.Equal(*m.DeliveredAt) {
		t.Fatalf("confirmed: %s read %v delivered %v, want read at delivery", m.State, m.ReadAt, m.DeliveredAt)
	}
}

// A sighting time in the future (a clock ahead of the agent's) is cut to
// now.
func TestReadAtNeverInTheFuture(t *testing.T) {
	lb := newLocalBus(t)
	out, _ := lb.send(t, "aaaa1111", "bbbb", "hello")
	deliver(lb.Bus, "bbbb3333", "", Limit{})
	if err := lb.MarkRead(ctx, []Read{{Session: "bbbb3333", Agent: "claude", ID: out.ID, At: lb.now.Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	if m := inboxItem(t, lb.Bus, "aaaa1111", out.ID); m.ReadAt == nil || !m.ReadAt.Equal(lb.now) {
		t.Fatalf("read_at %v, want %v", m.ReadAt, lb.now)
	}
}

// With a server: a read receipt goes in the ack batch after the delivery
// receipt was taken, once. A rejected one is not sent again.
func TestReadReceiptsGoToTheServer(t *testing.T) {
	srv := newFakeServer()
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(srv, nil), &presenceSrc{})
	msgs := []busproto.Envelope{env("mr1", "s1"), env("mr2", "s1"), env("mr3", "s1")}
	if err := b.st.reconcile(ctx, msgs, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, _ := deliver(b, "s1", "", Limit{}); len(got) != 3 {
		t.Fatalf("delivered %v", ids(got))
	}
	srv.mu.Lock()
	srv.readFn = func(r busproto.ReadReceipt) bool { return r.ID != "mr3" }
	srv.mu.Unlock()
	run(t, b)
	waitFor(t, "delivery receipts", func() bool { return len(srv.ackedIDs()) == 3 })
	at := time.Now().UTC().Truncate(time.Millisecond) // after the delivery
	if err := b.MarkRead(ctx, []Read{{Session: "s1", Agent: "claude", ID: "mr1", At: at}, {Session: "s1", Agent: "claude", ID: "mr3", At: at}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "read receipts", func() bool { return len(srv.readReceipts()) == 2 })
	got := srv.readReceipts()
	slices.SortFunc(got, func(a, b busproto.ReadReceipt) int { return compareStr(a.ID, b.ID) })
	if got[0].ID != "mr1" || got[0].Session != "s1" || got[0].Agent != "claude" || !got[0].At.Equal(at) || got[1].ID != "mr3" {
		t.Fatalf("read receipts %+v", got)
	}
	// Nothing owed: neither the taken one nor the rejected one goes again.
	b.kickAcks()
	time.Sleep(50 * time.Millisecond)
	if n := len(srv.readReceipts()); n != 2 {
		t.Fatalf("read receipts sent %d times", n)
	}
	if st := b.Status(ctx); st.Unacked != 0 {
		t.Fatalf("unacked %d", st.Unacked)
	}
}

// A read receipt waits for its delivery receipt: the server marks read
// only a message it holds as delivered.
func TestReadReceiptWaitsForTheDeliveryReceipt(t *testing.T) {
	srv := newFakeServer()
	release := make(chan struct{})
	srv.ackFn = func(ids []string) (busproto.AckResponse, error) {
		<-release
		return busproto.AckResponse{Acked: ids, Rejected: []string{}}, nil
	}
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(srv, nil), &presenceSrc{})
	if err := b.st.reconcile(ctx, []busproto.Envelope{env("mw1", "s1")}, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	deliver(b, "s1", "", Limit{})
	if err := b.MarkRead(ctx, []Read{{Session: "s1", Agent: "claude", ID: "mw1", At: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	run(t, b)
	waitFor(t, "the delivery receipt", func() bool { srv.mu.Lock(); defer srv.mu.Unlock(); return len(srv.ackReqs) == 1 })
	srv.mu.Lock()
	first := srv.ackReqs[0]
	srv.mu.Unlock()
	if len(first.Read) != 0 || !slices.Equal(first.IDs, []string{"mw1"}) {
		t.Fatalf("first batch %+v: a read before its delivery", first)
	}
	close(release)
	waitFor(t, "the read receipt", func() bool { return len(srv.readReceipts()) == 1 })
}

func compareStr(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Performance guard (notes/perf-guards.md): a sighting finds its message
// by primary key and the owed receipts come from their partial index, so
// neither reads the whole inbox.
func TestPerfReadPlans(t *testing.T) {
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(nil, nil), nil)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{markReadSQL, []any{1, "m1", "s1", "claude"}},
		{readAckSQL, []any{"m1"}},
		{owedReadsSQL, []any{100}},
		{readAckedSQL, []any{"done", "m1"}},
	} {
		perfguard.AssertSQLitePlan(t, b.st.db, nil, q.sql, q.args...)
	}
}

// A redelivery (the first hook printed the message and its confirmation
// was lost; the next hook printed it again, marked): two sightings, one
// read_at, from the first. The first sighting came before the delivery
// was confirmed, so read_at is the delivery's time.
func TestReadRedeliveryKeepsTheFirstSighting(t *testing.T) {
	lb := newLocalBus(t)
	out, _ := lb.send(t, "aaaa1111", "bbbb", "hello")
	if got, _ := lb.Take(ctx, "bbbb3333", "", Limit{}); len(got) != 1 || got[0].Attempt != 1 {
		t.Fatalf("first take %+v", got)
	}
	first := lb.now
	if err := lb.MarkRead(ctx, []Read{{Session: "bbbb3333", Agent: "claude", ID: out.ID, At: first}}); err != nil {
		t.Fatal(err)
	}
	lb.advance(LeaseFor)
	again, _ := lb.Take(ctx, "bbbb3333", "", Limit{})
	if len(again) != 1 || again[0].Attempt != 2 {
		t.Fatalf("redelivery %+v", again)
	}
	if err := lb.Confirm(ctx, "bbbb3333", []string{out.ID}); err != nil {
		t.Fatal(err)
	}
	confirmed := lb.now
	lb.advance(time.Second)
	if err := lb.MarkRead(ctx, []Read{{Session: "bbbb3333", Agent: "claude", ID: out.ID, At: lb.now}}); err != nil {
		t.Fatal(err)
	}
	if m := inboxItem(t, lb.Bus, "aaaa1111", out.ID); m.State != busproto.StateRead || !m.ReadAt.Equal(confirmed) {
		t.Fatalf("redelivered: %s read_at %v, want %v", m.State, m.ReadAt, confirmed)
	}
}

// With a server, a sighting made while the message was leased owes its
// read receipt once the hook confirms it.
func TestReadReceiptForASightingWhileLeased(t *testing.T) {
	srv := newFakeServer()
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(srv, nil), &presenceSrc{})
	if err := b.st.reconcile(ctx, []busproto.Envelope{env("ml1", "s1")}, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Take(ctx, "s1", "", Limit{}); len(got) != 1 {
		t.Fatal("not taken")
	}
	if err := b.MarkRead(ctx, []Read{{Session: "s1", Agent: "claude", ID: "ml1", At: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	run(t, b)
	time.Sleep(50 * time.Millisecond)
	if n := len(srv.readReceipts()); n != 0 {
		t.Fatalf("read receipt for a leased message: %+v", srv.readReceipts())
	}
	if err := b.Confirm(ctx, "s1", []string{"ml1"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the read receipt", func() bool { return len(srv.readReceipts()) == 1 })
}

// A message whose delivery receipt the server rejected (expired, held
// again, delivered elsewhere) can never take a read receipt: the server
// marks read only a message it holds as delivered. A sighting of it, before
// or after the rejection, owes nothing, so status does not count a receipt
// that is never sent.
func TestReadReceiptNotOwedAfterARejectedDelivery(t *testing.T) {
	srv := newFakeServer()
	srv.ackFn = func(ids []string) (busproto.AckResponse, error) {
		return busproto.AckResponse{Acked: []string{}, Rejected: ids}, nil
	}
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(srv, nil), &presenceSrc{})
	if err := b.st.reconcile(ctx, []busproto.Envelope{env("mx1", "s1"), env("mx2", "s1")}, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Take(ctx, "s1", "", Limit{}); len(got) != 2 {
		t.Fatal("not taken")
	}
	// mx1 is seen while leased, so its receipt is owed at the confirmation.
	if err := b.MarkRead(ctx, []Read{{Session: "s1", Agent: "claude", ID: "mx1", At: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if err := b.Confirm(ctx, "s1", []string{"mx1", "mx2"}); err != nil {
		t.Fatal(err)
	}
	run(t, b)
	waitFor(t, "the rejected delivery receipts", func() bool { return len(srv.ackedIDs()) == 2 })
	// mx2 is seen after the rejection.
	if err := b.MarkRead(ctx, []Read{{Session: "s1", Agent: "claude", ID: "mx2", At: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "nothing owed", func() bool { return b.Status(ctx).Unacked == 0 })
	if n := len(srv.readReceipts()); n != 0 {
		t.Fatalf("read receipts sent for rejected deliveries: %+v", srv.readReceipts())
	}
}
