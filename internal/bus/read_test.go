package bus_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
)

func readOf(id, session, agent string, at time.Time) busproto.ReadReceipt {
	return busproto.ReadReceipt{ID: id, Session: session, Agent: agent, At: at}
}

func (f *fixture) readAt(id string) *time.Time {
	f.t.Helper()
	var at *time.Time
	if err := f.pool.QueryRow(context.Background(), `SELECT read_at FROM bus_messages WHERE id=$1`, id).Scan(&at); err != nil {
		f.t.Fatal(err)
	}
	return at
}

// A read receipt from the device holding the recipient session marks a
// delivered message read once: the first receipt's time, never before the
// delivery and never after now. The sender's and the recipient's inbox show
// it read; a second receipt (the message printed again) changes nothing.
func TestReadReceipt(t *testing.T) {
	tm := newTeam(t)
	ctx := context.Background()
	out := tm.mustSend(tm.garyMac, "g-api-1111", "g-lin", "hello")
	// A receipt before the delivery receipt: not delivered, rejected.
	ack, err := tm.s.Ack(ctx, tm.garyLinux, busproto.AckRequest{Read: []busproto.ReadReceipt{readOf(out.ID, "g-lin-3333", "codex", tm.now)}})
	if err != nil || !slices.Equal(ack.ReadRejected, []string{out.ID}) || len(ack.Read) != 0 || tm.readAt(out.ID) != nil {
		t.Fatalf("read before delivered: %+v %v", ack, err)
	}
	tm.advance(time.Second)
	delivered := tm.now
	// Delivery and read in one batch: the delivery applies first.
	early := delivered.Add(-time.Minute) // the transcript's clock: before the server's delivery
	ack, err = tm.s.Ack(ctx, tm.garyLinux, busproto.AckRequest{IDs: []string{out.ID}, Read: []busproto.ReadReceipt{readOf(out.ID, "g-lin-3333", "codex", early)}})
	if err != nil || !slices.Equal(ack.Acked, []string{out.ID}) || !slices.Equal(ack.Read, []string{out.ID}) || len(ack.ReadRejected) != 0 {
		t.Fatalf("deliver and read: %+v %v", ack, err)
	}
	if st, at := tm.state(out.ID), tm.readAt(out.ID); st != "read" || at == nil || !at.Equal(delivered) {
		t.Fatalf("state %s read_at %v, want read at the delivery %v", st, at, delivered)
	}
	tm.advance(time.Minute)
	ack, err = tm.s.Ack(ctx, tm.garyLinux, busproto.AckRequest{Read: []busproto.ReadReceipt{readOf(out.ID, "g-lin-3333", "codex", tm.now)}})
	if err != nil || !slices.Equal(ack.Read, []string{out.ID}) {
		t.Fatalf("second receipt: %+v %v", ack, err)
	}
	if at := tm.readAt(out.ID); !at.Equal(delivered) {
		t.Fatalf("read_at moved to %v", at)
	}
	for _, q := range []struct {
		c busproto.Caller
		s string
	}{{tm.garyMac, "g-api-1111"}, {tm.garyLinux, "g-lin-3333"}} {
		in, err := tm.s.Inbox(ctx, q.c, busproto.InboxQuery{Session: q.s})
		if err != nil || len(in.Messages) != 1 || in.Messages[0].State != busproto.StateRead || in.Messages[0].ReadAt == nil || in.Messages[0].DeliveredAt == nil {
			t.Fatalf("%s inbox %+v %v", q.s, in, err)
		}
	}
	// Acking the delivery again is still a no-op.
	if again, err := tm.s.Ack(ctx, tm.garyLinux, busproto.AckRequest{IDs: []string{out.ID}}); err != nil || !slices.Equal(again.Acked, []string{out.ID}) {
		t.Fatalf("ack after read: %+v %v", again, err)
	}
	if n := tm.auditCount("bus.read"); n != 3 {
		t.Fatalf("bus.read audits %d, want 3", n)
	}
	// A future time is cut to now.
	out2 := tm.mustSend(tm.garyMac, "g-api-1111", "g-lin", "second")
	tm.s.Ack(ctx, tm.garyLinux, busproto.AckRequest{IDs: []string{out2.ID}})
	tm.advance(time.Second)
	tm.s.Ack(ctx, tm.garyLinux, busproto.AckRequest{Read: []busproto.ReadReceipt{readOf(out2.ID, "g-lin-3333", "codex", tm.now.Add(time.Hour))}})
	if at := tm.readAt(out2.ID); at == nil || !at.Equal(tm.now) {
		t.Fatalf("future read_at %v, want %v", at, tm.now)
	}
}

// A device marks read only a message delivered to a session it holds, for
// that session: not another device's session (also the same person's), not
// another person's message, not under another session or harness.
func TestReadReceiptAuthorization(t *testing.T) {
	tm := newTeam(t)
	ctx := context.Background()
	out := tm.mustSend(tm.garyMac, "g-api-1111", "g-lin", "hello")
	if ack, err := tm.s.Ack(ctx, tm.garyLinux, busproto.AckRequest{IDs: []string{out.ID}}); err != nil || len(ack.Acked) != 1 {
		t.Fatalf("deliver: %+v %v", ack, err)
	}
	now := tm.now
	for name, tc := range map[string]struct {
		c busproto.Caller
		r busproto.ReadReceipt
	}{
		"another device of the person": {tm.garyMac, readOf(out.ID, "g-lin-3333", "codex", now)},
		"another person":               {tm.alexMac, readOf(out.ID, "g-lin-3333", "codex", now)},
		"another session":              {tm.garyLinux, readOf(out.ID, "g-other-5555", "codex", now)},
		"another harness":              {tm.garyLinux, readOf(out.ID, "g-lin-3333", "claude", now)},
		"unknown message":              {tm.garyLinux, readOf("mnope", "g-lin-3333", "codex", now)},
	} {
		ack, err := tm.s.Ack(ctx, tc.c, busproto.AckRequest{Read: []busproto.ReadReceipt{tc.r}})
		if err != nil || len(ack.Read) != 0 || !slices.Equal(ack.ReadRejected, []string{tc.r.ID}) {
			t.Errorf("%s: %+v %v", name, ack, err)
		}
	}
	if st, at := tm.state(out.ID), tm.readAt(out.ID); st != "delivered" || at != nil {
		t.Fatalf("state %s read_at %v after refused receipts", st, at)
	}
	// The device that uploaded the session, without presence, may.
	tm.uploaded(tm.garyMac, "claude", "g-up-6666", "/Users/gary/src/api")
	up := tm.mustSend(tm.garyLinux, "g-lin-3333", "g-up-6666", "to an uploaded session")
	if ack, err := tm.s.Ack(ctx, tm.garyMac, busproto.AckRequest{IDs: []string{up.ID}, Read: []busproto.ReadReceipt{readOf(up.ID, "g-up-6666", "claude", tm.now)}}); err != nil ||
		!slices.Equal(ack.Read, []string{up.ID}) {
		t.Fatalf("uploaded session: %+v %v", ack, err)
	}
}

// An @user message is read by the session that claimed it, on the
// claiming device.
func TestReadReceiptClaimed(t *testing.T) {
	tm := newTeam(t)
	ctx := context.Background()
	out := tm.mustSend(tm.garyMac, "g-api-1111", "@gary", "to whoever", func(r *busproto.SendRequest) { r.Repo = "*" })
	if _, err := tm.s.Claim(ctx, tm.garyLinux, busproto.ClaimRequest{MessageID: out.ID, SessionID: "g-lin-3333", Agent: "codex"}); err != nil {
		t.Fatal(err)
	}
	ack, err := tm.s.Ack(ctx, tm.garyLinux, busproto.AckRequest{IDs: []string{out.ID}, Read: []busproto.ReadReceipt{readOf(out.ID, "g-lin-3333", "codex", tm.now)}})
	if err != nil || !slices.Equal(ack.Read, []string{out.ID}) || tm.state(out.ID) != "read" {
		t.Fatalf("claimed: %+v %v state %s", ack, err, tm.state(out.ID))
	}
}

// Ack bounds its whole batch, read receipts included, and refuses a
// malformed receipt.
func TestReadReceiptValidation(t *testing.T) {
	tm := newTeam(t)
	ctx := context.Background()
	var reads []busproto.ReadReceipt
	for range busproto.MaxAck {
		reads = append(reads, readOf("m1", "g-lin-3333", "codex", tm.now))
	}
	if _, err := tm.s.Ack(ctx, tm.garyLinux, busproto.AckRequest{IDs: []string{"m0"}, Read: reads}); code(err) != busproto.CodeBadRequest {
		t.Fatalf("over MaxAck: %v", err)
	}
	for _, r := range []busproto.ReadReceipt{readOf("", "g-lin-3333", "codex", tm.now), readOf("m1", "", "codex", tm.now), readOf("m1", "g-lin-3333", "codex", time.Time{})} {
		if _, err := tm.s.Ack(ctx, tm.garyLinux, busproto.AckRequest{Read: []busproto.ReadReceipt{r}}); code(err) != busproto.CodeBadRequest {
			t.Errorf("%+v: %v", r, err)
		}
	}
}
