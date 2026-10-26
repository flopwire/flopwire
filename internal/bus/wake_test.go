package bus_test

import (
	"context"
	"testing"

	"github.com/flopwire/flopwire/internal/busproto"
)

func TestExplicitWakeRequiresOwnLiveSupportedRecipient(t *testing.T) {
	tm := newTeam(t)
	target := live("g-wake-0001", "codex", "/x", false)
	target.WakeSupported = true
	tm.present(tm.garyLinux, target)
	wake := func(r *busproto.SendRequest) { r.Wake = true }
	before := tm.count(`SELECT count(*) FROM bus_messages`)
	for _, to := range []string{"g-web", "a-api", "@gary"} {
		if _, err := tm.send(tm.garyMac, "g-api-1111", to, "not sent", wake); code(err) != busproto.CodeBadRequest {
			t.Fatalf("target %s: %v", to, err)
		}
	}
	if n := tm.count(`SELECT count(*) FROM bus_messages`); n != before {
		t.Fatal("invalid wakes stored messages")
	}
	r := tm.mustSend(tm.garyMac, "g-api-1111", target.SessionID, "wake me", wake)
	if !r.WakeRequested || !r.To.WakeSupported {
		t.Fatalf("receipt: %+v", r)
	}
	polled := tm.present(tm.garyLinux, target)
	if len(polled.Messages) != 1 || !polled.Messages[0].WakeRequested {
		t.Fatalf("wake not routed across devices: %+v", polled)
	}
	inbox, err := tm.s.Inbox(context.Background(), tm.garyMac, busproto.InboxQuery{Session: "g-api-1111", SentOnly: true})
	if err != nil || len(inbox.Messages) != 1 || !inbox.Messages[0].WakeRequested {
		t.Fatalf("wake not retained: %+v %v", inbox, err)
	}
	// Ordinary sends never inherit wake, even in a reply to a wake request.
	reply := tm.mustSend(tm.garyLinux, target.SessionID, "g-api", "reply", replyTo(r.ID))
	if reply.WakeRequested {
		t.Fatal("reply inherited wake")
	}
	// An older receiving device stops advertising support and rejects new wakes.
	target.WakeSupported = false
	tm.present(tm.garyLinux, target)
	if _, err := tm.send(tm.garyMac, "g-api-1111", target.SessionID, "old recipient", wake); code(err) != busproto.CodeBadRequest {
		t.Fatalf("old recipient: %v", err)
	}
}
