package bus_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
)

func (f *fixture) sweep() {
	f.t.Helper()
	if _, err := f.s.Sweep(context.Background()); err != nil {
		f.t.Fatal(err)
	}
}

// Messages in a final state go with their audit rows once their expiry is
// more than the retention past; newer ones stay (#70).
func TestRetentionDeletesFinalMessagesPastTheWindow(t *testing.T) {
	tm := newTeam(t)
	ctx := context.Background()
	lin := live("g-lin-3333", "codex", "/home/gary/api", false)
	t0 := tm.now

	// One message in each final state, sent at t0.
	delivered := tm.mustSend(tm.garyMac, "g-api-1111", "g-lin", "delivered")
	read := tm.mustSend(tm.garyMac, "g-api-1111", "g-lin", "read")
	gone := tm.mustSend(tm.garyMac, "g-api-1111", "g-lin", "undelivered")
	expired := tm.mustSend(tm.garyMac, "g-web-2222", "a-api", "held, then expired")
	_, err := tm.send(tm.garyMac, "g-api-1111", "g-lin", "delivered")
	var be *busproto.Error
	if code(err) != busproto.CodeDuplicate || !errors.As(err, &be) {
		t.Fatalf("duplicate: %v", err)
	}
	refused := be.MessageID
	if got := tm.present(tm.garyLinux, lin); len(got.Messages) != 3 {
		t.Fatalf("poll %+v", got.Messages)
	}
	if _, err := tm.s.Ack(ctx, tm.garyLinux, busproto.AckRequest{IDs: []string{delivered.ID, read.ID}, Undelivered: []string{gone.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := tm.s.Ack(ctx, tm.garyLinux, busproto.AckRequest{Read: []busproto.ReadReceipt{{ID: read.ID, Session: "g-lin-3333", Agent: "codex", At: t0}}}); err != nil {
		t.Fatal(err)
	}
	old := []string{delivered.ID, read.ID, gone.ID, expired.ID, refused}

	// Two days later: a reply to the delivered one, and a new message.
	tm.advance(48 * time.Hour)
	tm.presence()
	reply := tm.mustSend(tm.garyLinux, "g-lin-3333", "g-api", "answer", replyTo(delivered.ID))
	fresh := tm.mustSend(tm.garyMac, "g-api-1111", "g-lin", "fresh")
	if got := tm.present(tm.garyLinux, lin); len(got.Messages) != 1 {
		t.Fatalf("poll %+v", got.Messages)
	}
	if _, err := tm.s.Ack(ctx, tm.garyLinux, busproto.AckRequest{IDs: []string{fresh.ID}}); err != nil {
		t.Fatal(err)
	}
	newer := []string{reply.ID, fresh.ID}

	oldAudit := `SELECT count(*) FROM audit_events WHERE target_type='bus_message' AND (target_id=ANY($1) OR (target_id='' AND created_at<=$2))`
	before := tm.count(oldAudit, old, t0)
	if before != len(old)+2 { // a send each, a bus.deliver and a bus.read
		t.Fatalf("old audit rows %d", before)
	}
	// Retention counts from expiry: just inside the window nothing goes.
	tm.now = t0.Add(busproto.DefaultTTL + busproto.DefaultRetention - time.Second)
	n, err := tm.s.Sweep(ctx)
	if err != nil || n.Deleted != 0 || n.Audit != 0 {
		t.Fatalf("sweep inside the window: %+v %v", n, err)
	}
	if got := tm.count(`SELECT count(*) FROM bus_messages WHERE id=ANY($1)`, old); got != len(old) {
		t.Fatalf("old rows inside the window: %d", got)
	}
	tm.advance(2 * time.Second)
	n, err = tm.s.Sweep(ctx)
	if err != nil || n.Deleted != int64(len(old)) || n.Audit != int64(before) {
		t.Fatalf("sweep past the window: %+v %v (want %d audit)", n, err, before)
	}
	if got := tm.count(`SELECT count(*) FROM bus_messages WHERE id=ANY($1)`, old); got != 0 {
		t.Fatalf("old rows left: %d", got)
	}
	if got := tm.count(oldAudit, old, t0); got != 0 {
		t.Fatalf("old audit rows left: %d", got)
	}
	// The newer messages and their audit rows stay; the reply loses only
	// its reply_to.
	if got := tm.count(`SELECT count(*) FROM bus_messages WHERE id=ANY($1)`, newer); got != len(newer) {
		t.Fatalf("newer rows: %d", got)
	}
	if got := tm.count(`SELECT count(*) FROM audit_events WHERE action='bus.send' AND target_id=ANY($1)`, newer); got != len(newer) {
		t.Fatalf("newer audit rows: %d", got)
	}
	if got := tm.count(`SELECT count(*) FROM bus_messages WHERE id=$1 AND reply_to IS NULL AND thread_id=$2`, reply.ID, delivered.ID); got != 1 {
		t.Fatal("reply after its parent was deleted")
	}
	// A late receipt for a deleted message is rejected, not an error, so
	// the device stops owing it.
	tm.presence()
	ack, err := tm.s.Ack(ctx, tm.garyLinux, busproto.AckRequest{IDs: []string{gone.ID},
		Read: []busproto.ReadReceipt{{ID: delivered.ID, Session: "g-lin-3333", Agent: "codex", At: tm.now}}})
	if err != nil || len(ack.Rejected) != 1 || len(ack.ReadRejected) != 1 {
		t.Fatalf("receipts for deleted messages: %+v %v", ack, err)
	}
	// The sender's inbox no longer lists the deleted messages.
	in, err := tm.s.Inbox(ctx, tm.garyMac, busproto.InboxQuery{Session: "g-api-1111"})
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, m := range in.Messages {
		listed[m.ID] = true
	}
	if len(listed) != 2 || !listed[fresh.ID] || !listed[reply.ID] {
		t.Fatalf("inbox after the sweep %+v, want %s and %s", in.Messages, fresh.ID, reply.ID)
	}
}

// A claimed message the device never acknowledged is not deleted while it
// may still be delivered, however short the retention: it expires at its
// 24 h expiry first and goes a retention after that.
func TestRetentionKeepsAClaimedMessageUntilItExpires(t *testing.T) {
	tm := newTeam(t)
	ctx := context.Background()
	tm.s.Retention = time.Hour
	msg := tm.mustSend(tm.garyMac, "g-api-1111", "@gary", "claim me", func(r *busproto.SendRequest) { r.Repo = "*" })
	if _, err := tm.s.Claim(ctx, tm.garyLinux, busproto.ClaimRequest{MessageID: msg.ID, SessionID: "g-lin-3333"}); err != nil {
		t.Fatal(err)
	}
	tm.advance(busproto.DefaultTTL - time.Minute) // far past the retention, before the expiry
	tm.sweep()
	if got := tm.state(msg.ID); got != "claimed" {
		t.Fatalf("before expiry: %s", got)
	}
	if tm.auditCount("bus.claim") != 1 || tm.auditCount("bus.send") != 1 {
		t.Fatal("audit rows of a live message deleted")
	}
	tm.advance(time.Minute)
	tm.sweep()
	if got := tm.state(msg.ID); got != "expired" {
		t.Fatalf("at expiry: %s", got)
	}
	tm.advance(time.Hour)
	tm.sweep()
	if got := tm.state(msg.ID); got != "expired" {
		t.Fatalf("a retention after expiry: %s", got)
	}
	tm.advance(time.Second)
	tm.sweep()
	if got := tm.count(`SELECT count(*) FROM bus_messages WHERE id=$1`, msg.ID); got != 0 {
		t.Fatal("expired message past retention kept")
	}
	if tm.auditCount("bus.claim") != 0 || tm.auditCount("bus.send") != 0 {
		t.Fatal("its audit rows kept")
	}
}

// One sweep deletes at most purgeBatch*purgeBatches messages; the next
// sweep continues.
func TestRetentionCapsEachSweep(t *testing.T) {
	tm := newTeam(t)
	const total = 10_500
	tm.exec(`INSERT INTO bus_messages(id,thread_id,from_user,from_agent,from_session,to_user,to_agent,to_session,addressed,sender,intent,body,body_sha,state,created_at,expires_at)
		SELECT 'old'||i,'old'||i,$1,'claude','g-api-1111',$1,'codex','g-lin-3333','session','own','inform','x',sha256('x'::bytea),'delivered',$2,$3
		FROM generate_series(1,$4) i`, tm.gary, tm.now.Add(-30*24*time.Hour), tm.now.Add(-29*24*time.Hour), total)
	n, err := tm.s.Sweep(context.Background())
	if err != nil || n.Deleted != 10_000 {
		t.Fatalf("first sweep %+v %v", n, err)
	}
	n, err = tm.s.Sweep(context.Background())
	if err != nil || n.Deleted != total-10_000 {
		t.Fatalf("second sweep %+v %v", n, err)
	}
}
