package bus_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
)

// Seed only durable public status metadata, as migration 012's backfill
// does. No message body or transcript is needed for the old backlog.
func seedFailureBacklog(tm *team, session string, count int) {
	tm.t.Helper()
	tm.exec(`INSERT INTO bus_delivery_failures
    (message_id,from_user,from_device,from_session,from_agent,state,reason,created_at)
    SELECT 'mfair-old-'||lpad(i::text,3,'0'),$1,$2,$3,'claude','expired','expired',$4
    FROM generate_series(1,$5::int) i`, tm.gary, tm.garyMac.DeviceID, session, tm.now.Add(-time.Hour), count)
}

func TestDeliveryFailureFairBacklog(t *testing.T) {
	for _, tc := range []struct {
		name     string
		oldLive  bool
		busy     bool
		backfill bool
	}{
		{"absent-sender", false, true, false},
		{"open-idle-sender", true, true, false},
		{"new-sender-not-yet-busy", true, false, false},
		{"unoffered-backfill", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tm := newTeam(t)
			ctx := context.Background()
			if !tc.backfill {
				seedFailureBacklog(tm, "fair-old", 55)
			}
			old := live("fair-old", "claude", "/synthetic/api", false)
			active := live("fair-new", "claude", "/synthetic/api", tc.busy)
			sessions := []busproto.PresenceSession{active}
			if tc.oldLive {
				sessions = append(sessions, old)
			}
			seen := map[string]string{}
			first := tm.present(tm.garyMac, sessions...)
			want := 0
			if tc.oldLive && !tc.backfill {
				want = 50
			}
			if len(first.Failures) != want {
				t.Fatalf("initial backlog batch = %d, want %d", len(first.Failures), want)
			}
			for _, n := range first.Failures {
				seen[n.ID] = n.Token
			}
			if tc.backfill {
				seedFailureBacklog(tm, "fair-old", 55)
			}
			out := tm.mustSend(tm.garyMac, active.SessionID, "g-lin", "synthetic fairness message")
			if got := tm.present(tm.garyLinux, live2("g-lin-3333")); len(got.Messages) != 1 || got.Messages[0].ID != out.ID {
				t.Fatal("positive queued message did not reach recipient")
			}
			if _, err := tm.s.Ack(ctx, tm.garyLinux, busproto.AckRequest{SessionEnded: []string{out.ID}}); err != nil {
				t.Fatal(err)
			}
			// No fixture-clock advance: offer rotation must be independent
			// of the two-minute lease deadline, even on immediate polls.
			gotNew := false
			for range 2 {
				got := tm.present(tm.garyMac, sessions...)
				if len(got.Failures) > 50 {
					t.Fatal("unbounded failure batch")
				}
				for _, n := range got.Failures {
					if n.ID == out.ID {
						gotNew = true
						if n.Session != active.SessionID || n.Reason != "session_ended" {
							t.Fatal("wrong new sender status")
						}
					}
					if token, ok := seen[n.ID]; ok && token != n.Token {
						t.Fatal("rotation replaced a valid ownership token")
					}
					seen[n.ID] = n.Token
				}
			}
			if !gotNew {
				t.Fatal("new sender starved behind unprinted backlog")
			}
			if tc.oldLive && len(seen) != 56 {
				t.Fatalf("old retries did not rotate: saw %d of 56 notices", len(seen))
			}
			if !tc.oldLive && len(seen) != 1 {
				t.Fatal("absent sender occupied the failure batch")
			}
			if tm.count(`SELECT count(*) FROM bus_delivery_failures WHERE from_user=$1 AND acked_at IS NULL`, tm.gary) != 56 {
				t.Fatal("offering cleared unprinted statuses")
			}
			// Every old status remains eligible when its sender returns;
			// unprinted retries rotate without requiring an acknowledgment.
			returned := map[string]bool{}
			for range 2 {
				for _, n := range tm.present(tm.garyMac, old).Failures {
					returned[n.ID] = true
				}
			}
			for i := 1; i <= 55; i++ {
				if !returned[fmt.Sprintf("mfair-old-%03d", i)] {
					t.Fatalf("old notice %d lost on sender return", i)
				}
			}
		})
	}
}

func TestDeliveryFailureFairBacklogResumeOwnership(t *testing.T) {
	tm := newTeam(t)
	ctx := context.Background()
	seedFailureBacklog(tm, "fair-resume", 55)
	p := live("fair-resume", "claude", "/synthetic/api", false)
	first := tm.present(tm.garyMac, p)
	if len(first.Failures) != 50 {
		t.Fatal("missing initial ownership batch")
	}
	n := first.Failures[0]
	tm.advance(time.Second)
	// Latest trusted presence routes the sender to Linux, but the Mac's
	// already imported batch retains its ownership until the lease ends.
	resumed := tm.present(tm.garyLinux, p)
	if len(resumed.Failures) != 5 {
		t.Fatalf("resume should import only unowned remainder: %d", len(resumed.Failures))
	}
	if got := tm.present(tm.garyMac); len(got.Failures) != 0 {
		t.Fatal("old device continued offering after sender resumed elsewhere")
	}
	ack, err := tm.s.Ack(ctx, tm.garyLinux, busproto.AckRequest{Failures: []busproto.FailureAck{{ID: n.ID, Token: n.Token}}})
	if err != nil || len(ack.Failures) != 0 || len(ack.FailureRejected) != 1 {
		t.Fatalf("new device accepted old ownership: %+v %v", ack, err)
	}
	tm.advance(busproto.FailureLease)
	moved := tm.present(tm.garyLinux, p)
	found := false
	for _, v := range moved.Failures {
		if v.ID == n.ID {
			found = true
			if v.Token == n.Token {
				t.Fatal("expired ownership token reused on resume")
			}
		}
	}
	if !found {
		t.Fatal("expired old batch did not reach resumed sender")
	}
	ack, err = tm.s.Ack(ctx, tm.garyMac, busproto.AckRequest{Failures: []busproto.FailureAck{{ID: n.ID, Token: n.Token}}})
	if err != nil || len(ack.Failures) != 0 || len(ack.FailureRejected) != 1 {
		t.Fatalf("stale owner acknowledged moved notice: %+v %v", ack, err)
	}
	if tm.count(`SELECT count(*) FROM bus_delivery_failures WHERE from_user=$1 AND acked_at IS NULL`, tm.gary) != 55 {
		t.Fatal("resume discarded unprinted statuses")
	}
}

func TestDeliveryFailureRequiresFreshTrustedPresence(t *testing.T) {
	for _, mode := range []string{"absent", "stale", "cloud", "revoked"} {
		t.Run(mode, func(t *testing.T) {
			tm := newTeam(t)
			seedFailureBacklog(tm, "fair-trust", 1)
			p := live("fair-trust", "claude", "/synthetic/api", false)
			switch mode {
			case "stale":
				tm.present(tm.garyLinux, p)
				tm.advance(busproto.PresenceTTL + busproto.FailureLease)
			case "cloud":
				if _, err := tm.s.Poll(context.Background(), tm.garyLinux, busproto.PollRequest{Cloud: []busproto.PresenceSession{p}}); err != nil {
					t.Fatal(err)
				}
			case "revoked":
				tm.present(tm.garyLinux, p)
				tm.exec(`UPDATE devices SET revoked_at=$2 WHERE id=$1`, tm.garyLinux.DeviceID, tm.now)
				tm.advance(busproto.FailureLease)
				// Refresh its presence timestamp directly to distinguish a
				// revoked device from merely stale presence.
				tm.exec(`UPDATE bus_presence SET seen_at=$2 WHERE device_id=$1`, tm.garyLinux.DeviceID, tm.now)
			}
			if got := tm.present(tm.garyMac); len(got.Failures) != 0 {
				t.Fatal("untrusted or absent presence fell back to sending device")
			}
			if tm.count(`SELECT count(*) FROM bus_delivery_failures WHERE acked_at IS NULL AND from_user=$1`, tm.gary) != 1 {
				t.Fatal("unprinted notice lost without trusted presence")
			}
			if got := tm.present(tm.garyMac, p); len(got.Failures) != 1 {
				t.Fatal("trusted sender return did not recover durable notice")
			}
		})
	}
}

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
			tm.advance(time.Second)
			again := tm.present(tm.garyMac, live("g-api-1111", "claude", "/synthetic/api", false))
			if len(again.Failures) != 1 || again.Failures[0].Token != n.Token || !again.Failures[0].ValidUntil.After(n.ValidUntil) {
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
