package devicebus

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
)

func storedState(t *testing.T, st *store, id string) (string, int) {
	t.Helper()
	var state string
	var attempts int
	if err := st.db.QueryRow(`SELECT state,attempts FROM devbus_messages WHERE id=?`, id).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	return state, attempts
}

func closedLocalRetry(t *testing.T) (*localBus, busproto.SendResponse) {
	t.Helper()
	lb := newLocalBus(t)
	source, err := lb.send(t, "aaaa1111", "bbbb", "synthetic closed retry")
	if err != nil {
		t.Fatal(err)
	}
	retryEnded(t, lb, source.ID)
	target := sess(refB.Session, refB.Agent, "/src/web", false)
	lb.p.set(sess("aaaa1111", "claude", "/src/api", true), sess("aaaa2222", "codex", "/src/api", false))
	lb.SetSources(lb.p.get, func(context.Context, string) ([]Session, error) { return []Session{target}, nil })
	if err := lb.st.noteLive(ctx, []Session{target}, lb.cfg.Now()); err != nil {
		t.Fatal(err)
	}
	if err := lb.End(ctx, refB, lb.cfg.Now()); err != nil {
		t.Fatal(err)
	}
	retry, err := lb.send(t, "aaaa1111", refB.Session, "synthetic closed retry", func(r *busproto.SendRequest) { r.RetryOf = source.ID })
	if err != nil {
		t.Fatal(err)
	}
	if retry.To.Live || retry.Admission != busproto.AdmissionNextResume {
		t.Fatalf("closed admission: %+v", retry)
	}
	return lb, retry
}

func TestClosedRetryWaitsInsideEndGraceUntilFirstReceivingHook(t *testing.T) {
	lb, retry := closedLocalRetry(t)
	lb.settleEnded(ctx)
	if state, attempts := storedState(t, lb.st, retry.ID); state != waitingResume || attempts != 0 {
		t.Fatalf("before hook: %s attempts%d", state, attempts)
	}
	if item := sentItem(t, lb.Bus, "aaaa1111", retry.ID); item.State != busproto.StateQueued || item.Admission != busproto.AdmissionNextResume {
		t.Fatalf("public inbox: %+v", item)
	}
	if counts, err := lb.st.counts(ctx, lb.cfg.Now()); err != nil || counts.pending != 1 {
		t.Fatalf("pending: %+v %v", counts, err)
	}
	if msgs, err := lb.Take(ctx, refB.Session, "codex", Limit{}); err != nil || len(msgs) != 0 {
		t.Fatalf("wrong agent hook: %+v %v", msgs, err)
	}
	lb.advance(time.Second)
	if err := lb.Revive(ctx, refB.Session, lb.cfg.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := lb.Take(ctx, refB.Session, refB.Agent, Limit{})
	if err != nil || len(got) != 1 || got[0].ID != retry.ID || got[0].Attempt != 1 {
		t.Fatalf("first hook: %+v %v", got, err)
	}
	if got[0].Admission != busproto.AdmissionNextResume {
		t.Fatalf("lost admission: %+v", got[0])
	}
	if other, err := lb.Take(ctx, refB.Session, refB.Agent, Limit{}); err != nil || len(other) != 0 {
		t.Fatalf("concurrent hook: %+v %v", other, err)
	}
	if err := lb.Confirm(ctx, refB.Session, []string{retry.ID}); err != nil {
		t.Fatal(err)
	}
	if state, _ := storedState(t, lb.st, retry.ID); state != "delivered" {
		t.Fatalf("confirmed state: %s", state)
	}
}

func TestWaitingRetryExpiresWithoutHookAndRetainsFailure(t *testing.T) {
	lb, retry := closedLocalRetry(t)
	lb.advance(busproto.DefaultTTL + time.Second)
	if failures, err := lb.TakeFailures(ctx, "aaaa1111", "claude"); err != nil || len(failures) != 2 { // source session_ended and retry expiry
		t.Fatalf("failure notices: %+v %v", failures, err)
	} else {
		found := false
		for _, f := range failures {
			if f.ID == retry.ID {
				found = true
				if f.State != busproto.StateExpired {
					t.Fatalf("retry failure: %+v", f)
				}
			}
		}
		if !found {
			t.Fatal("no retry expiry notice")
		}
	}
	if got, err := lb.Take(ctx, refB.Session, refB.Agent, Limit{}); err != nil || len(got) != 0 {
		t.Fatalf("expired hook: %+v %v", got, err)
	}
	lb.advance(localKeep + time.Second)
	if err := lb.st.purge(ctx, lb.cfg.Now()); err != nil {
		t.Fatal(err)
	}
	if has, err := lb.st.has(ctx, retry.ID); err != nil || has {
		t.Fatalf("retained expired envelope: %v %v", has, err)
	}
}

func TestRetryAdmissionConsumedByLeaseExpiryButRestoredByUnusedFirstTake(t *testing.T) {
	for _, unused := range []bool{true, false} {
		t.Run(map[bool]string{true: "unused", false: "lease-expired"}[unused], func(t *testing.T) {
			lb, retry := closedLocalRetry(t)
			got, err := lb.Take(ctx, refB.Session, refB.Agent, Limit{})
			if err != nil || len(got) != 1 {
				t.Fatalf("take: %+v %v", got, err)
			}
			if unused {
				if err := lb.Requeue(ctx, []string{retry.ID}); err != nil {
					t.Fatal(err)
				}
				lb.settleEnded(ctx)
				if state, n := storedState(t, lb.st, retry.ID); state != waitingResume || n != 0 {
					t.Fatalf("unused take: %s attempts%d", state, n)
				}
				got, err = lb.Take(ctx, refB.Session, refB.Agent, Limit{})
				if err != nil || len(got) != 1 || got[0].Attempt != 1 {
					t.Fatalf("restored admission: %+v %v", got, err)
				}
			}
			lb.advance(lb.cfg.Lease + time.Millisecond)
			lb.expireLeases(ctx)
			lb.settleEnded(ctx)
			if state, _ := storedState(t, lb.st, retry.ID); state != "undelivered" {
				t.Fatalf("ended after real lease expiry: %s", state)
			}
		})
	}
}

func TestServerResumeAdmissionImportDoesNotResetAfterFirstTake(t *testing.T) {
	for _, claim := range []bool{false, true} {
		t.Run(map[bool]string{false: "poll", true: "claim"}[claim], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bus.db")
			st, err := openStore(path)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			e := env("resume-retry", "session-resume")
			e.RetryOf = "source"
			e.Admission = busproto.AdmissionNextResume
			e.Sent = now
			e.ExpiresAt = now.Add(time.Hour)
			if claim {
				err = st.addClaimed(ctx, e)
			} else {
				err = st.reconcile(ctx, []busproto.Envelope{e}, nil, now)
			}
			if err != nil {
				t.Fatal(err)
			}
			if state, _ := storedState(t, st, e.ID); state != waitingResume {
				t.Fatalf("import: %s", state)
			}
			if err = st.noteLive(ctx, []Session{sess(e.ToSession, e.ToAgent, "/src/api", true)}, now); err != nil {
				t.Fatal(err)
			}
			if _, err = st.end(ctx, Ref{Agent: e.ToAgent, Session: e.ToSession}, now, now); err != nil {
				t.Fatal(err)
			}
			if state, _ := storedState(t, st, e.ID); state != waitingResume {
				t.Fatalf("end inside EndGrace: %s", state)
			}
			if err = st.db.Close(); err != nil {
				t.Fatal(err)
			}
			st, err = openStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer st.db.Close()
			_, got, err := st.take(ctx, e.ToSession, e.ToAgent, now, Limit{}, LeaseFor, 3, nil)
			if err != nil || len(got) != 1 {
				t.Fatalf("reopened take: %+v %v", got, err)
			}
			if err = st.reconcile(ctx, []busproto.Envelope{e}, nil, now); err != nil {
				t.Fatal(err)
			}
			if state, n := storedState(t, st, e.ID); state != "leased" || n != 1 {
				t.Fatalf("reconcile reset lease: %s attempts%d", state, n)
			}
			if _, err = expireLeases(ctx, st.db, now.Add(LeaseFor+time.Millisecond), LeaseFor, 3); err != nil {
				t.Fatal(err)
			}
			if err = st.reconcile(ctx, []busproto.Envelope{e}, nil, now); err != nil {
				t.Fatal(err)
			}
			if state, n := storedState(t, st, e.ID); state != "queued" || n != 1 {
				t.Fatalf("reconcile restored admission: %s attempts%d", state, n)
			}
			// A later unused take cannot restore admission consumed by the first lease.
			_, _, err = st.take(ctx, e.ToSession, e.ToAgent, now.Add(LeaseFor+time.Millisecond), Limit{}, LeaseFor, 3, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = st.untake(ctx, []string{e.ID}); err != nil {
				t.Fatal(err)
			}
			if state, n := storedState(t, st, e.ID); state != "queued" || n != 1 {
				t.Fatalf("later unused take: %s attempts%d", state, n)
			}
		})
	}
}

func TestWaitingRetryCountsTowardCapacityAndReplayIsPublicQueued(t *testing.T) {
	lb, retry := closedLocalRetry(t)
	for i := 1; i < busproto.MaxUndelivered; i++ {
		if _, err := lb.st.db.Exec(`INSERT INTO devbus_messages(id,origin,seq,to_session,to_agent,from_session,from_agent,thread_id,to_key,envelope,state,created_at,expires_at)
 SELECT ?,'local',seq,to_session,to_agent,'other','other','other-thread',to_key,envelope,'waiting_resume',created_at,expires_at FROM devbus_messages WHERE id=?`, fmt.Sprintf("waiting-capacity-%d", i), retry.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := lb.send(t, "aaaa1111", refB.Session, "new message blocked by waiting retries"); code(err) != busproto.CodeRecipientFull {
		t.Fatalf("waiting capacity: %v", err)
	}
	var source string
	if err := lb.st.db.QueryRow(`SELECT source_id FROM devbus_retries WHERE successor_id=?`, retry.ID).Scan(&source); err != nil {
		t.Fatal(err)
	}
	replay, err := lb.send(t, "aaaa1111", refB.Session, "synthetic closed retry", func(r *busproto.SendRequest) { r.RetryOf = source })
	if err != nil || !replay.Replayed || replay.ID != retry.ID || replay.State != busproto.StateQueued || replay.Admission != busproto.AdmissionNextResume {
		t.Fatalf("waiting replay: %+v %v", replay, err)
	}
}

func TestRetryAdmittedWhileLiveEndsNormally(t *testing.T) {
	lb := newLocalBus(t)
	source, err := lb.send(t, "aaaa1111", "bbbb", "synthetic live retry")
	if err != nil {
		t.Fatal(err)
	}
	retryEnded(t, lb, source.ID)
	retry, err := lb.send(t, "aaaa1111", "bbbb", "synthetic live retry", func(r *busproto.SendRequest) { r.RetryOf = source.ID })
	if err != nil {
		t.Fatal(err)
	}
	if retry.Admission != "" || !retry.To.Live {
		t.Fatalf("live admission: %+v", retry)
	}
	if err := lb.st.noteLive(ctx, []Session{sess(refB.Session, refB.Agent, "/src/web", false)}, lb.cfg.Now()); err != nil {
		t.Fatal(err)
	}
	if err := lb.End(ctx, refB, lb.cfg.Now()); err != nil {
		t.Fatal(err)
	}
	if item := sentItem(t, lb.Bus, "aaaa1111", retry.ID); item.State != busproto.StateUndelivered || item.Reason != busproto.ReasonSessionEnded {
		t.Fatalf("live retry after end: %+v", item)
	}
}

func TestWaitingServerRetryRevocationRemovesUnleasedMessage(t *testing.T) {
	st, err := openStore(filepath.Join(t.TempDir(), "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.db.Close()
	now := time.Now().UTC()
	e := env("revoked-retry", "recipient")
	e.Admission = busproto.AdmissionNextResume
	if err := st.reconcile(ctx, []busproto.Envelope{e}, nil, now); err != nil {
		t.Fatal(err)
	}
	if err := st.reconcile(ctx, nil, nil, now); err != nil {
		t.Fatal(err)
	}
	if has, err := st.has(ctx, e.ID); err != nil || has {
		t.Fatalf("waiting message after revocation: %v %v", has, err)
	}
}

func TestWaitingCloudRetryStartsAtFirstRunningPush(t *testing.T) {
	fc := &fakeCloud{agent: "claude"}
	fc.set(cloudSess("session_retry", false))
	b := newCloudBus(t, fc)
	if err := b.listOne(ctx, fc); err != nil {
		t.Fatal(err)
	}
	e := env("cloud-resume-retry", "session_retry")
	e.ToAgent = "claude"
	e.RetryOf = "source"
	e.Admission = busproto.AdmissionNextResume
	if err := b.st.addClaimed(ctx, e); err != nil {
		t.Fatal(err)
	}
	// Neither an idle listing nor a terminal vendor observation consumes admission.
	if _, err := b.st.endCloud(ctx, e.ToSession, e.ToAgent); err != nil {
		t.Fatal(err)
	}
	b.pushCloud(ctx, map[string]pendingRead{})
	if state, n := storedState(t, b.st, e.ID); state != waitingResume || n != 0 {
		t.Fatalf("idle cloud: %s attempts%d", state, n)
	}
	if got := fc.pushed(); len(got) != 0 {
		t.Fatalf("idle pushes: %q", got)
	}
	fc.set(cloudSess("session_retry", true))
	if err := b.listOne(ctx, fc); err != nil {
		t.Fatal(err)
	}
	b.pushCloud(ctx, map[string]pendingRead{})
	if got := fc.pushed(); len(got) != 1 {
		t.Fatalf("running pushes: %q", got)
	}
	if state, n := storedState(t, b.st, e.ID); state != "delivered" || n != 1 {
		t.Fatalf("cloud confirmation: %s attempts%d", state, n)
	}
}

func TestResumeRetryCapabilityAdvertisedForPollAndBothClaims(t *testing.T) {
	srv := newFakeServer()
	p := &presenceSrc{}
	p.set(sess("recipient-1111", "claude", "/src/api", true))
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(srv, nil), p)
	srv.claimFn = func(req busproto.ClaimRequest) (busproto.ClaimResponse, error) {
		return busproto.ClaimResponse{Message: env(req.MessageID, req.SessionID)}, nil
	}
	e := env("claim-local-resume", "recipient-1111")
	if err := b.claim(ctx, busproto.Claimable{Message: e, Sessions: []string{e.ToSession}}, []Session{sess(e.ToSession, "claude", "/src/api", true)}, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	e.ID = "claim-cloud-resume"
	if err := b.claimCloud(ctx, busproto.Claimable{Message: e}, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	for _, req := range srv.claims {
		if !req.ResumeRetries {
			t.Errorf("claim omitted capability: %+v", req)
		}
	}
	srv.mu.Unlock()
	run(t, b)
	waitFor(t, "resume-capable poll", func() bool { srv.mu.Lock(); defer srv.mu.Unlock(); return len(srv.polls) > 0 })
	srv.mu.Lock()
	defer srv.mu.Unlock()
	for _, req := range srv.polls {
		if !req.ResumeRetries {
			t.Errorf("poll omitted capability: %+v", req)
		}
	}
}
