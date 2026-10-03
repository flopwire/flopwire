package bus_test

import (
	"context"
	"slices"
	"testing"

	"github.com/flopwire/flopwire/internal/busproto"
)

// A device reports that the session holding a message ended before any
// hook delivered it (issue #67): the message is undelivered with reason
// session_ended, and the sender's inbox says so. The report is taken also
// after the device's presence dropped the session (the report and the
// next poll race), but never from a device that does not hold the message.
func TestSessionEndedReport(t *testing.T) {
	tm := newTeam(t)
	ctx := context.Background()
	out := tm.mustSend(tm.garyMac, "g-api-1111", "g-lin", "to a session that will end")
	// The ended session is gone from linux's presence before the report.
	tm.present(tm.garyLinux)
	// A device cannot report a message to a session another device holds
	// (live in its presence, or uploaded from it).
	onMac := tm.mustSend(tm.garyMac, "g-api-1111", "g-web", "to a live session on the mac")
	tm.present(tm.garyLinux, live2("g-other-0000"))
	if ack, err := tm.s.Ack(ctx, tm.garyLinux, busproto.AckRequest{SessionEnded: []string{onMac.ID}}); err != nil || !slices.Equal(ack.Rejected, []string{onMac.ID}) {
		t.Fatalf("a device reported a session live on another device as ended: %+v %v", ack, err)
	}
	ack, err := tm.s.Ack(ctx, tm.garyLinux, busproto.AckRequest{SessionEnded: []string{out.ID}})
	if err != nil || !slices.Equal(ack.Acked, []string{out.ID}) {
		t.Fatalf("session ended report: %+v %v", ack, err)
	}
	if again, err := tm.s.Ack(ctx, tm.garyLinux, busproto.AckRequest{SessionEnded: []string{out.ID}}); err != nil || len(again.Acked) != 1 {
		t.Fatalf("reported again: %+v %v", again, err)
	}
	sent, err := tm.s.Inbox(ctx, tm.garyMac, busproto.InboxQuery{Session: "g-api-1111", SentOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var m *busproto.InboxItem
	for i := range sent.Messages {
		if sent.Messages[i].ID == out.ID {
			m = &sent.Messages[i]
		}
	}
	if m == nil || m.State != busproto.StateUndelivered || m.Reason != busproto.ReasonSessionEnded || m.DeliveredAt != nil {
		t.Fatalf("sender's inbox: %+v", m)
	}
	// Never offered again, nor acknowledged as delivered afterwards.
	if got := tm.present(tm.garyLinux, live2("g-lin-3333")); slices.ContainsFunc(got.Messages, func(e busproto.Envelope) bool { return e.ID == out.ID }) {
		t.Fatal("an undelivered message was offered again")
	}
	if ack, _ := tm.s.Ack(ctx, tm.garyLinux, busproto.AckRequest{IDs: []string{out.ID}}); len(ack.Rejected) != 1 {
		t.Fatalf("delivered after session_ended: %+v", ack)
	}

	// A claimed @user message: only the claiming device reports it.
	if _, err := tm.s.Accept(ctx, busproto.Caller{UserID: tm.alex}, "gary"); err != nil {
		t.Fatal(err)
	}
	u := tm.mustSend(tm.garyMac, "g-web-2222", "@alex", "to a person", func(r *busproto.SendRequest) { r.Repo = "api" })
	tm.present(tm.alexMac, live("a-api-4444", "claude", "/Users/alex/code/api", false))
	if _, err := tm.s.Claim(ctx, tm.alexMac, busproto.ClaimRequest{MessageID: u.ID, SessionID: "a-api-4444"}); err != nil {
		t.Fatal(err)
	}
	other := tm.device(tm.alex)
	if ack, _ := tm.s.Ack(ctx, other, busproto.AckRequest{SessionEnded: []string{u.ID}}); len(ack.Rejected) != 1 {
		t.Fatalf("another device reported a claimed message: %+v", ack)
	}
	tm.present(tm.alexMac)
	if ack, _ := tm.s.Ack(ctx, tm.alexMac, busproto.AckRequest{SessionEnded: []string{u.ID}}); len(ack.Acked) != 1 {
		t.Fatalf("claimed message report: %+v", ack)
	}
	if st := tm.state(u.ID); st != string(busproto.StateUndelivered) {
		t.Fatalf("claimed message after its session ended: %s", st)
	}
}

func live2(id string) busproto.PresenceSession { return live(id, "codex", "/home/gary/api", false) }
