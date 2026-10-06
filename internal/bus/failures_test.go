package bus_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
)

func TestDeliveryFailureCrossDeviceAllIntentsAndAcknowledgement(t *testing.T) {
	for _, in := range []busproto.Intent{busproto.IntentRequest, busproto.IntentInform, busproto.IntentDone} {
		t.Run(string(in), func(t *testing.T) {
			tm := newTeam(t)
			ctx := context.Background()
			out := tm.mustSend(tm.garyMac, "g-api-1111", "g-lin", "synthetic-private-body", intent(in))
			if _, err := tm.s.Ack(ctx, tm.garyLinux, busproto.AckRequest{SessionEnded: []string{out.ID}}); err != nil {
				t.Fatal(err)
			}
			got := tm.present(tm.garyMac, live("g-api-1111", "claude", "/synthetic/api", false))
			if len(got.Failures) != 1 || got.Failures[0].ID != out.ID || got.Failures[0].Reason != "session_ended" {
				t.Fatalf("sender poll: %+v", got.Failures)
			}
			wire, _ := json.Marshal(got.Failures)
			if strings.Contains(string(wire), "synthetic-private-body") {
				t.Fatal("status leaked body")
			}
			n := got.Failures[0]
			again := tm.present(tm.garyMac, live("g-api-1111", "claude", "/synthetic/api", false))
			if len(again.Failures) != 1 || again.Failures[0].Token != n.Token {
				t.Fatalf("duplicate poll changed live lease: %+v", again.Failures)
			}
			if got := tm.present(tm.garyLinux, live2("g-lin-3333")); len(got.Failures) != 0 {
				t.Fatal("recipient got sender status")
			}
			bad, err := tm.s.Ack(ctx, tm.garyLinux, busproto.AckRequest{Failures: []busproto.FailureAck{{ID: n.ID, Token: n.Token}}})
			if err != nil || len(bad.Failures) != 0 {
				t.Fatalf("foreign-device ack: %+v %v", bad, err)
			}
			for range 2 {
				ack, err := tm.s.Ack(ctx, tm.garyMac, busproto.AckRequest{Failures: []busproto.FailureAck{{ID: n.ID, Token: n.Token}}})
				if err != nil || len(ack.Failures) != 1 {
					t.Fatalf("ack: %+v %v", ack, err)
				}
			}
			if got := tm.present(tm.garyMac, live("g-api-1111", "claude", "/synthetic/api", false)); len(got.Failures) != 0 {
				t.Fatal("status repeated after durable ack")
			}
		})
	}
}

func TestDeliveryFailureExpiryRetentionAndResumeOnAnotherDevice(t *testing.T) {
	tm := newTeam(t)
	ctx := context.Background()
	out := tm.mustSend(tm.garyMac, "g-api-1111", "g-lin", "synthetic-expiry")
	tm.advance(busproto.DefaultTTL + busproto.DefaultRetention + time.Second)
	tm.sweep()
	if tm.count(`SELECT count(*) FROM bus_messages WHERE id=$1`, out.ID) != 0 {
		t.Fatal("message not purged")
	}
	// Its sender resumes on another trusted device after the old presence
	// expired. The durable body-free notice survives retention.
	got := tm.present(tm.garyLinux, live("g-api-1111", "claude", "/synthetic/api", false))
	if len(got.Failures) != 1 || got.Failures[0].ID != out.ID || got.Failures[0].Reason != "expired" {
		t.Fatalf("resumed sender notice: %+v", got.Failures)
	}
	n := got.Failures[0]
	// A lease prevents simultaneous delivery to the old device. A later
	// resume can move ownership only after the server lease expires.
	tm.advance(busproto.PresenceTTL + 3*time.Minute)
	moved := tm.present(tm.garyMac, live("g-api-1111", "claude", "/synthetic/api", false))
	if len(moved.Failures) != 1 || moved.Failures[0].Token == n.Token {
		t.Fatalf("lease reassignment: %+v", moved.Failures)
	}
	ack, err := tm.s.Ack(ctx, tm.garyLinux, busproto.AckRequest{Failures: []busproto.FailureAck{{ID: n.ID, Token: n.Token}}})
	if err != nil || len(ack.Failures) != 0 {
		t.Fatalf("stale lease acknowledged reassignment: %+v %v", ack, err)
	}
}

func TestDeliveryIdleEvidenceRoundTrip(t *testing.T) {
	tm := newTeam(t)
	at := tm.now.Add(-2 * time.Hour)
	p := live("g-lin-3333", "codex", "/synthetic/api", false)
	p.IdleSince = at
	p.IdleKnown = true
	tm.present(tm.garyLinux, p)
	out := tm.mustSend(tm.garyMac, "g-api-1111", "g-lin", "synthetic idle receipt")
	if !out.To.IdleSince.Equal(at) || !out.To.IdleKnown || out.To.IdleSeconds == nil || *out.To.IdleSeconds != 7200 {
		t.Fatalf("send idle: %+v", out.To)
	}
	peers, err := tm.s.Peers(context.Background(), tm.garyMac, busproto.PeersQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range peers.Peers {
		if p.Session == "g-lin-3333" {
			if !p.IdleKnown || !p.IdleSince.Equal(at) || p.IdleSeconds == nil || *p.IdleSeconds != 7200 {
				t.Fatalf("peer idle: %+v", p)
			}
			return
		}
	}
	t.Fatal("missing peer")
}
