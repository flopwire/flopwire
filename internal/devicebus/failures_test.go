package devicebus

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
)

func takeFailures(t *testing.T, b *Bus, session, agent string) []busproto.DeliveryFailure {
	t.Helper()
	out, err := b.TakeFailures(ctx, session, agent)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFailureNoticeAllIntents(t *testing.T) {
	for _, intent := range []string{"request", "inform", "done"} {
		t.Run(intent, func(t *testing.T) {
			lb := newLocalBus(t)
			out, err := lb.send(t, "aaaa1111", "bbbb", "synthetic-private-body", func(r *busproto.SendRequest) { r.Intent = intent })
			if err != nil {
				t.Fatal(err)
			}
			if err := lb.End(ctx, refB, lb.cfg.Now()); err != nil {
				t.Fatal(err)
			}
			if got := takeFailures(t, lb.Bus, "aaaa1111", "codex"); len(got) != 0 {
				t.Fatal("notice crossed harness isolation")
			}
			if got := takeFailures(t, lb.Bus, "bbbb3333", "claude"); len(got) != 0 {
				t.Fatal("notice reached recipient")
			}
			n := takeFailures(t, lb.Bus, "aaaa1111", "claude")
			if len(n) != 1 || n[0].ID != out.ID || n[0].Reason != "session_ended" {
				t.Fatalf("notice: %+v", n)
			}
			if err := lb.Confirm(ctx, "aaaa1111", []string{n[0].LeaseID}); err != nil {
				t.Fatal(err)
			}
			if got := takeFailures(t, lb.Bus, "aaaa1111", "claude"); len(got) != 0 {
				t.Fatal("notice repeated after ack")
			}
			if err := lb.Revive(ctx, "bbbb3333", lb.cfg.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if got := takeFailures(t, lb.Bus, "aaaa1111", "claude"); len(got) != 0 {
				t.Fatal("recipient resume renewed a settled notice")
			}
		})
	}
}

func TestFailureNoticeExpiryWhileSenderIdleSurvivesRetention(t *testing.T) {
	lb := newLocalBus(t)
	out, err := lb.send(t, "aaaa1111", "bbbb", "synthetic-expiry")
	if err != nil {
		t.Fatal(err)
	}
	// No sender hook, and no recipient hook. Purge captures status before
	// deleting the expired local envelope, without waking the sender.
	lb.advance(busproto.DefaultTTL + localKeep + time.Second)
	if err := lb.st.purge(ctx, lb.cfg.Now()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := lb.st.db.QueryRow(`SELECT count(*) FROM devbus_messages WHERE id=?`, out.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("purge %d %v", count, err)
	}
	n := takeFailures(t, lb.Bus, "aaaa1111", "claude")
	if len(n) != 1 || n[0].Reason != "expired" || n[0].ID != out.ID {
		t.Fatalf("notice lost while idle: %+v", n)
	}
}

func TestFailureNoticeConcurrentHooksStaleAckAndRestart(t *testing.T) {
	lb := newLocalBus(t)
	out, err := lb.send(t, "aaaa1111", "bbbb", "synthetic-concurrency")
	if err != nil {
		t.Fatal(err)
	}
	if err := lb.End(ctx, refB, lb.cfg.Now()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan []busproto.DeliveryFailure, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() { n, err := lb.TakeFailures(ctx, "aaaa1111", "claude"); results <- n; errs <- err })
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first []busproto.DeliveryFailure
	for n := range results {
		first = append(first, n...)
	}
	if len(first) != 1 || first[0].ID != out.ID {
		t.Fatalf("concurrent lease: %+v", first)
	}
	lb.advance(lb.cfg.Lease + time.Second)
	second := takeFailures(t, lb.Bus, "aaaa1111", "claude")
	if len(second) != 1 || second[0].Attempt != 2 || second[0].LeaseID == first[0].LeaseID {
		t.Fatalf("lease expiry: %+v", second)
	}
	if err := lb.Confirm(ctx, "aaaa1111", []string{first[0].LeaseID}); err != nil {
		t.Fatal(err)
	}
	var confirmed sql.NullInt64
	if err := lb.st.db.QueryRow(`SELECT confirmed_at FROM devbus_failures WHERE id=?`, out.ID).Scan(&confirmed); err != nil || confirmed.Valid {
		t.Fatalf("stale hook acknowledged replacement: %v %v", confirmed, err)
	}
	// Opening the same durable database simulates restart. Active lease
	// remains exclusive; expiry offers it again, with no attempt limit.
	var path string
	rows, err := lb.st.db.Query(`PRAGMA database_list`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var seq int
		var name string
		if err := rows.Scan(&seq, &name, &path); err != nil {
			t.Fatal(err)
		}
	}
	rows.Close()
	restarted, err := Open(path, lb.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if got := takeFailures(t, restarted, "aaaa1111", "claude"); len(got) != 0 {
		t.Fatal("restart ignored active lease")
	}
	lb.advance(lb.cfg.Lease + time.Second)
	third := takeFailures(t, restarted, "aaaa1111", "claude")
	if len(third) != 1 || third[0].Attempt != 3 {
		t.Fatalf("restart lost notice: %+v", third)
	}
	if err := restarted.Confirm(ctx, "aaaa1111", []string{third[0].LeaseID}); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Confirm(ctx, "aaaa1111", []string{third[0].LeaseID}); err != nil {
		t.Fatal(err)
	}
	if got := takeFailures(t, lb.Bus, "aaaa1111", "claude"); len(got) != 0 {
		t.Fatal("ack repeated notice")
	}
}

func TestFailureNoticeServerPollDedupAndAck(t *testing.T) {
	lb := newLocalBus(t)
	n := busproto.DeliveryFailure{ID: "mremote", Session: "aaaa1111", Agent: "claude", State: busproto.StateUndelivered, Reason: "push_failed", Token: "server-lease", ValidUntil: lb.cfg.Now().Add(time.Minute)}
	for range 2 {
		if err := lb.st.importFailures(ctx, []busproto.DeliveryFailure{n}, lb.cfg.Now()); err != nil {
			t.Fatal(err)
		}
	}
	got := takeFailures(t, lb.Bus, "aaaa1111", "claude")
	if len(got) != 1 || got[0].Reason != "push_failed" {
		t.Fatalf("poll status: %+v", got)
	}
	if err := lb.Confirm(ctx, "aaaa1111", []string{got[0].LeaseID}); err != nil {
		t.Fatal(err)
	}
	if err := lb.st.importFailures(ctx, []busproto.DeliveryFailure{n}, lb.cfg.Now()); err != nil {
		t.Fatal(err)
	}
	if got := takeFailures(t, lb.Bus, "aaaa1111", "claude"); len(got) != 0 {
		t.Fatal("duplicate poll reprinted acknowledged status")
	}
	owed, err := lb.st.failureAcks(ctx, 10)
	if err != nil || len(owed) != 1 || owed[0].Token != n.Token {
		t.Fatalf("ack: %+v %v", owed, err)
	}
	if err := lb.st.failureAcked(ctx, []string{n.ID}, owed); err != nil {
		t.Fatal(err)
	}
	if owed, err := lb.st.failureAcks(ctx, 10); err != nil || len(owed) != 0 {
		t.Fatalf("ack not durable: %+v %v", owed, err)
	}
}

func TestFailureNoticeVersionFiveUpgradePreservesMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bus.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	st := &store{db: db}
	// Populate durable message, receipt and lifecycle state, then mark the
	// fixture as the production v5 schema. The additive status upgrade must retain each.
	if _, err := st.db.Exec(`INSERT INTO devbus_messages(id,origin,seq,to_session,to_agent,from_session,from_agent,thread_id,envelope,state,created_at,expires_at,ack)
 VALUES('mupgrade','server',1,'recipient','claude','sender','claude','mupgrade','{}','undelivered',1,2,'report');
 INSERT INTO devbus_sessions(agent,session_id,ended_at) VALUES('claude','recipient',3);
 PRAGMA user_version=5;`); err != nil {
		t.Fatal(err)
	}
	st.db.Close()
	st, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.db.Close()
	var count int
	if err := st.db.QueryRow(`SELECT count(*) FROM devbus_failures`).Scan(&count); err != nil {
		t.Fatalf("notice table not created: %v", err)
	}
	var ack string
	var ended int64
	if err := st.db.QueryRow(`SELECT ack FROM devbus_messages WHERE id='mupgrade'`).Scan(&ack); err != nil || ack != "report" {
		t.Fatalf("lost receipt %s %v", ack, err)
	}
	if err := st.db.QueryRow(`SELECT ended_at FROM devbus_sessions WHERE session_id='recipient'`).Scan(&ended); err != nil || ended != 3 {
		t.Fatalf("lost lifecycle %d %v", ended, err)
	}
}

func TestFailureNoticeUnconfirmedHasNoNoticeAttemptLimit(t *testing.T) {
	lb := newLocalBus(t)
	out, err := lb.send(t, "aaaa1111", "bbbb", "synthetic lost print")
	if err != nil {
		t.Fatal(err)
	}
	for range lb.cfg.MaxAttempts {
		if got, err := lb.Take(ctx, "bbbb3333", "claude", Limit{}); err != nil || len(got) != 1 {
			t.Fatalf("recipient lease: %+v %v", got, err)
		}
		lb.advance(lb.cfg.Lease + time.Second)
		lb.expireLeases(ctx)
	}
	for i := 1; i <= MaxAttempts+2; i++ {
		n := takeFailures(t, lb.Bus, "aaaa1111", "claude")
		if len(n) != 1 || n[0].ID != out.ID || n[0].Reason != "unconfirmed" || n[0].Attempt != i {
			t.Fatalf("status attempt %d: %+v", i, n)
		}
		lb.advance(lb.cfg.Lease + time.Second)
	}
}

func TestFailureNoticeRejectedAckDoesNotSpin(t *testing.T) {
	lb := newLocalBus(t)
	n := busproto.DeliveryFailure{ID: "mrejected", Session: "aaaa1111", Agent: "claude", State: busproto.StateUndelivered, Reason: "session_ended", Token: "old-token", ValidUntil: lb.cfg.Now().Add(time.Minute)}
	if err := lb.st.importFailures(ctx, []busproto.DeliveryFailure{n}, lb.cfg.Now()); err != nil {
		t.Fatal(err)
	}
	got := takeFailures(t, lb.Bus, "aaaa1111", "claude")
	if err := lb.Confirm(ctx, "aaaa1111", []string{got[0].LeaseID}); err != nil {
		t.Fatal(err)
	}
	owed, err := lb.st.failureAcks(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := lb.st.failureRejected(ctx, []string{n.ID}, owed); err != nil {
		t.Fatal(err)
	}
	if err := lb.st.importFailures(ctx, []busproto.DeliveryFailure{n}, lb.cfg.Now()); err != nil {
		t.Fatal(err)
	}
	if acks, err := lb.st.failureAcks(ctx, 10); err != nil || len(acks) != 0 {
		t.Fatalf("stale-token ack spun: %+v %v", acks, err)
	}
	n.Token = "new-token"
	if err := lb.st.importFailures(ctx, []busproto.DeliveryFailure{n}, lb.cfg.Now()); err != nil {
		t.Fatal(err)
	}
	if acks, err := lb.st.failureAcks(ctx, 10); err != nil || len(acks) != 1 || acks[0].Token != n.Token {
		t.Fatalf("fresh lease ack lost: %+v %v", acks, err)
	}
	if got := takeFailures(t, lb.Bus, "aaaa1111", "claude"); len(got) != 0 {
		t.Fatal("fresh server lease caused duplicate local print")
	}
}

func TestLocalIdleReceiptAndPeerUseWitnessedTransition(t *testing.T) {
	lb := newLocalBus(t)
	idle := lb.cfg.Now().Add(-2 * time.Hour)
	s := sess("bbbb3333", "claude", "/synthetic/api", false)
	s.IdleSince = idle
	s.IdleKnown = true
	lb.p.set(sess("aaaa1111", "claude", "/synthetic/api", true), s)
	out, err := lb.send(t, "aaaa1111", "bbbb", "synthetic idle receipt")
	if err != nil {
		t.Fatal(err)
	}
	if !out.To.IdleKnown || !out.To.IdleSince.Equal(idle) || out.To.IdleSeconds == nil || *out.To.IdleSeconds != 7200 {
		t.Fatalf("idle receipt: %+v", out.To)
	}
	peers, err := lb.Peers(ctx, busproto.PeersQuery{Session: "aaaa1111"})
	if err != nil {
		t.Fatal(err)
	}
	if len(peers.Peers) != 1 || !peers.Peers[0].IdleSince.Equal(idle) || peers.Peers[0].IdleSeconds == nil || *peers.Peers[0].IdleSeconds != 7200 {
		t.Fatalf("idle peer: %+v", peers)
	}
}

func TestFailureNoticePollLeaseUsesConservativeRequestTime(t *testing.T) {
	lb := newLocalBus(t)
	asked := lb.cfg.Now()
	serverNow := asked.Add(time.Hour)
	resp := busproto.PollResponse{Now: serverNow, Failures: []busproto.DeliveryFailure{{ID: "mskew", Session: "aaaa1111", Agent: "claude", State: busproto.StateUndelivered, Reason: "unconfirmed", Token: "lease", ValidUntil: serverNow.Add(2 * time.Minute)}}}
	// A very delayed response must not extend ownership beyond the
	// server's lease. Learned TTL skew alone would extend it by 30 s.
	lb.advance(30 * time.Second)
	if err := lb.answered(ctx, resp, asked, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	var until int64
	if err := lb.st.db.QueryRow(`SELECT valid_until FROM devbus_failures WHERE id='mskew'`).Scan(&until); err != nil {
		t.Fatal(err)
	}
	if until != ms(asked.Add(2*time.Minute)) {
		t.Fatalf("ownership stretched by slow poll: %v", time.UnixMilli(until))
	}
}

type failureAckServer struct {
	*fakeServer
	omit bool
}

func (f *failureAckServer) Ack(ctx context.Context, r busproto.AckRequest) (busproto.AckResponse, error) {
	out, err := f.fakeServer.Ack(ctx, r)
	if !f.omit {
		for _, n := range r.Failures {
			out.Failures = append(out.Failures, n.ID)
		}
	}
	return out, err
}

func TestFailureNoticeAckBatchAndMissingServerSupport(t *testing.T) {
	for _, omit := range []bool{false, true} {
		t.Run(fmt.Sprint(omit), func(t *testing.T) {
			srv := &failureAckServer{fakeServer: newFakeServer(), omit: omit}
			b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(srv, nil), nil)
			now := b.cfg.Now()
			n := busproto.DeliveryFailure{ID: "mstatus", Session: "sender", Agent: "claude", State: busproto.StateUndelivered, Reason: "session_ended", Token: "token", ValidUntil: now.Add(time.Minute)}
			if err := b.st.importFailures(ctx, []busproto.DeliveryFailure{n}, now); err != nil {
				t.Fatal(err)
			}
			taken := takeFailures(t, b, "sender", "claude")
			if len(taken) != 1 {
				t.Fatal("missing status")
			}
			if err := b.Confirm(ctx, "sender", []string{taken[0].LeaseID}); err != nil {
				t.Fatal(err)
			}
			count, _, err := b.sendAcks(ctx)
			if omit {
				if err == nil {
					t.Fatal("missing ack treated as success, allowing a hot retry loop")
				}
				if owed, err := b.st.failureAcks(ctx, 10); err != nil || len(owed) != 1 {
					t.Fatal("missing ack lost durable receipt")
				}
			} else {
				if err != nil || count != 1 {
					t.Fatalf("ack batch: %d %v", count, err)
				}
				if count, _, err := b.sendAcks(ctx); err != nil || count != 0 {
					t.Fatalf("ack repeated: %d %v", count, err)
				}
			}
		})
	}
}

func TestFailureNoticeLeaseRecoversAfterClockStepBack(t *testing.T) {
	lb := newLocalBus(t)
	if _, err := lb.send(t, "aaaa1111", "bbbb", "synthetic clock rollback"); err != nil {
		t.Fatal(err)
	}
	if err := lb.End(ctx, refB, lb.cfg.Now()); err != nil {
		t.Fatal(err)
	}
	first := takeFailures(t, lb.Bus, "aaaa1111", "claude")
	if len(first) != 1 {
		t.Fatal("missing initial status")
	}
	lb.advance(-time.Hour)
	second := takeFailures(t, lb.Bus, "aaaa1111", "claude")
	if len(second) != 1 || second[0].Attempt != 2 || second[0].LeaseID == first[0].LeaseID {
		t.Fatalf("clock rollback blocked notice: %+v", second)
	}
}

func TestFailureNoticeServerLeaseRecoversAfterClockStepBackAndPoll(t *testing.T) {
	lb := newLocalBus(t)
	lb.advance(time.Date(2026, time.October, 6, 12, 0, 0, 0, time.UTC).Sub(lb.cfg.Now()))
	asked := lb.cfg.Now()
	serverNow := asked.Add(time.Hour)
	n := busproto.DeliveryFailure{ID: "mrollback", Session: "aaaa1111", Agent: "claude", State: busproto.StateUndelivered, Reason: "unconfirmed", Token: "server-lease", ValidUntil: serverNow.Add(busproto.FailureLease)}
	resp := busproto.PollResponse{Now: serverNow, Failures: []busproto.DeliveryFailure{n}}
	if err := lb.answered(ctx, resp, asked, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	first := takeFailures(t, lb.Bus, n.Session, n.Agent)
	if len(first) != 1 || first[0].ID != n.ID || first[0].Attempt != 1 {
		t.Fatalf("initial server notice: %+v", first)
	}
	// Both stored deadlines now look far ahead. The local hook lease may
	// recover, but server ownership must wait for a normalized poll import.
	lb.advance(-2 * busproto.FailureLease)
	if got := takeFailures(t, lb.Bus, n.Session, n.Agent); len(got) != 0 {
		t.Fatalf("server ownership survived large rollback: %+v", got)
	}
	asked = lb.cfg.Now()
	resp.Now = serverNow.Add(time.Second)
	resp.Failures[0].ValidUntil = resp.Now.Add(busproto.FailureLease)
	if err := lb.answered(ctx, resp, asked, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	var until int64
	if err := lb.st.db.QueryRow(`SELECT valid_until FROM devbus_failures WHERE id=?`, n.ID).Scan(&until); err != nil {
		t.Fatal(err)
	}
	if until != ms(asked.Add(busproto.FailureLease)) {
		t.Fatalf("poll did not normalize ownership to request clock: %v", time.UnixMilli(until))
	}
	second := takeFailures(t, lb.Bus, n.Session, n.Agent)
	if len(second) != 1 || second[0].ID != n.ID || second[0].Attempt != 2 || second[0].LeaseID == first[0].LeaseID {
		t.Fatalf("poll did not recover server notice: %+v", second)
	}
	if err := lb.Confirm(ctx, n.Session, []string{first[0].LeaseID}); err != nil {
		t.Fatal(err)
	}
	var confirmed sql.NullInt64
	var leaseID string
	if err := lb.st.db.QueryRow(`SELECT confirmed_at,lease_id FROM devbus_failures WHERE id=?`, n.ID).Scan(&confirmed, &leaseID); err != nil {
		t.Fatal(err)
	}
	if confirmed.Valid || leaseID != second[0].LeaseID {
		t.Fatalf("stale hook cleared replacement: confirmed=%v lease=%q", confirmed, leaseID)
	}
	if got := takeFailures(t, lb.Bus, n.Session, n.Agent); len(got) != 0 {
		t.Fatalf("replacement lease lost exclusivity: %+v", got)
	}
	if err := lb.Confirm(ctx, n.Session, []string{second[0].LeaseID}); err != nil {
		t.Fatal(err)
	}
	if owed, err := lb.st.failureAcks(ctx, 10); err != nil || len(owed) != 1 || owed[0].ID != n.ID || owed[0].Token != n.Token {
		t.Fatalf("replacement confirmation lost server ack: %+v %v", owed, err)
	}
}

func TestFailureNoticeSmallClockStepBackPreservesLocalLease(t *testing.T) {
	lb := newLocalBus(t)
	lb.advance(time.Date(2026, time.October, 6, 12, 0, 0, 0, time.UTC).Sub(lb.cfg.Now()))
	out, err := lb.send(t, "aaaa1111", "bbbb", "synthetic small clock rollback")
	if err != nil {
		t.Fatal(err)
	}
	if err := lb.End(ctx, refB, lb.cfg.Now()); err != nil {
		t.Fatal(err)
	}
	first := takeFailures(t, lb.Bus, "aaaa1111", "claude")
	if len(first) != 1 || first[0].ID != out.ID || first[0].Attempt != 1 {
		t.Fatalf("initial local notice: %+v", first)
	}
	// A rollback of one lease duration puts the existing deadline exactly
	// two lease durations ahead. It must remain exclusive through equality.
	for _, step := range []time.Duration{-lb.cfg.Lease / 2, -lb.cfg.Lease / 2} {
		lb.advance(step)
		if got := takeFailures(t, lb.Bus, "aaaa1111", "claude"); len(got) != 0 {
			t.Fatalf("small rollback prematurely reoffered local notice: %+v", got)
		}
	}
	lb.advance(-time.Millisecond)
	second := takeFailures(t, lb.Bus, "aaaa1111", "claude")
	if len(second) != 1 || second[0].ID != out.ID || second[0].Attempt != 2 || second[0].LeaseID == first[0].LeaseID {
		t.Fatalf("local notice did not recover beyond rollback cutoff: %+v", second)
	}
	if err := lb.Confirm(ctx, "aaaa1111", []string{first[0].LeaseID}); err != nil {
		t.Fatal(err)
	}
	var confirmed sql.NullInt64
	var leaseID string
	if err := lb.st.db.QueryRow(`SELECT confirmed_at,lease_id FROM devbus_failures WHERE id=?`, out.ID).Scan(&confirmed, &leaseID); err != nil {
		t.Fatal(err)
	}
	if confirmed.Valid || leaseID != second[0].LeaseID {
		t.Fatalf("stale hook cleared replacement: confirmed=%v lease=%q", confirmed, leaseID)
	}
	if got := takeFailures(t, lb.Bus, "aaaa1111", "claude"); len(got) != 0 {
		t.Fatalf("replacement lease lost exclusivity: %+v", got)
	}
	if err := lb.Confirm(ctx, "aaaa1111", []string{second[0].LeaseID}); err != nil {
		t.Fatal(err)
	}
	if got := takeFailures(t, lb.Bus, "aaaa1111", "claude"); len(got) != 0 {
		t.Fatalf("confirmed replacement reoffered: %+v", got)
	}
}
