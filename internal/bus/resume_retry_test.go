package bus_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
)

func TestInactiveRetryAdmissionAndRecipientCapability(t *testing.T) {
	f := newFixture(t)
	c := f.device(f.user("admissionretry"))
	ctx := context.Background()
	sourceSession := live("admission-source", "claude", "/retry", false)
	targetSession := live("admission-target", "codex", "/retry", false)
	f.present(c, sourceSession, targetSession)
	f.uploaded(c, "codex", targetSession.SessionID, "/retry")
	req := busproto.SendRequest{FromSession: sourceSession.SessionID, To: targetSession.SessionID, Body: "synthetic first resume retry"}
	source, err := f.s.Send(ctx, c, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Ack(ctx, c, busproto.AckRequest{SessionEnded: []string{source.ID}}); err != nil {
		t.Fatal(err)
	}
	// The old session just ended. No time advance or grace-period wait.
	f.present(c, sourceSession)
	req.RetryOf = source.ID
	next, err := f.s.Send(ctx, c, req)
	if err != nil {
		t.Fatal(err)
	}
	if next.Admission != busproto.AdmissionNextResume || next.To.Live || next.State != busproto.StateQueued {
		t.Fatalf("inactive receipt: %+v", next)
	}
	oldPoll, err := f.s.Poll(ctx, c, busproto.PollRequest{Sessions: []busproto.PresenceSession{sourceSession, targetSession}})
	if err != nil {
		t.Fatal(err)
	}
	if len(oldPoll.Messages) != 0 || len(oldPoll.Claimable) != 0 {
		t.Fatalf("old recipient saw retry: %+v", oldPoll)
	}
	if _, err := f.s.Claim(ctx, c, busproto.ClaimRequest{MessageID: next.ID, SessionID: targetSession.SessionID, Agent: "codex"}); err == nil {
		t.Fatal("old recipient claimed direct retry")
	}
	capable, err := f.s.Poll(ctx, c, busproto.PollRequest{Sessions: []busproto.PresenceSession{sourceSession, targetSession}, ResumeRetries: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(capable.Messages) != 1 || capable.Messages[0].Admission != busproto.AdmissionNextResume {
		t.Fatalf("capable recipient: %+v", capable)
	}
	replay, err := f.s.Send(ctx, c, req)
	if err != nil || !replay.Replayed || replay.Admission != busproto.AdmissionNextResume {
		t.Fatalf("live replay changed admission: %+v %v", replay, err)
	}
	// A different source retried while live follows the ordinary live boundary.
	req.RetryOf = ""
	req.Body = "synthetic live retry"
	liveSource, err := f.s.Send(ctx, c, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Ack(ctx, c, busproto.AckRequest{SessionEnded: []string{liveSource.ID}}); err != nil {
		t.Fatal(err)
	}
	req.RetryOf = liveSource.ID
	liveRetry, err := f.s.Send(ctx, c, req)
	if err != nil || liveRetry.Admission != "" {
		t.Fatalf("live retry: %+v %v", liveRetry, err)
	}
	oldLive, err := f.s.Poll(ctx, c, busproto.PollRequest{Sessions: []busproto.PresenceSession{sourceSession, targetSession}})
	if err != nil || len(oldLive.Messages) != 0 || len(oldLive.Claimable) != 0 {
		t.Fatalf("old recipient saw live retry: %+v %v", oldLive, err)
	}
	if _, err := f.s.Claim(ctx, c, busproto.ClaimRequest{MessageID: liveRetry.ID, SessionID: targetSession.SessionID, Agent: "codex"}); err == nil {
		t.Fatal("old direct claim exposed live retry")
	}
}

func TestRetrySuccessorTombstoneAndSourceCleanup(t *testing.T) {
	f := newFixture(t)
	c := f.device(f.user("tombstoneretry"))
	ctx := context.Background()
	f.present(c, live("tombstone-source", "claude", "/retry", false), live("tombstone-target", "codex", "/retry", false))
	req := busproto.SendRequest{FromSession: "tombstone-source", To: "tombstone-target", Body: "synthetic tombstone retry"}
	source, err := f.s.Send(ctx, c, req)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE bus_messages SET state='undelivered',reason='session_ended' WHERE id=$1`, source.ID)
	req.RetryOf = source.ID
	next, err := f.s.Send(ctx, c, req)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`DELETE FROM bus_messages WHERE id=$1`, next.ID)
	_, err = f.s.Send(ctx, c, req)
	var failure *busproto.Error
	if !errors.As(err, &failure) || failure.Code != busproto.CodeNotFound {
		t.Fatalf("deleted successor receipt: %v", err)
	}
	if f.count(`SELECT count(*) FROM bus_retries WHERE source_id=$1`, source.ID) != 1 || f.count(`SELECT count(*) FROM bus_messages WHERE retry_of=$1`, source.ID) != 0 {
		t.Fatal("successor recreated or receipt lost")
	}
	if _, err := f.s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if f.count(`SELECT count(*) FROM bus_retries WHERE source_id=$1`, source.ID) != 1 {
		t.Fatal("live source lost tombstone")
	}
	f.exec(`UPDATE bus_messages SET expires_at=$2 WHERE id=$1`, source.ID, f.now.Add(-f.s.RetentionPeriod()-time.Hour))
	if _, err := f.s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if f.count(`SELECT count(*) FROM bus_retries WHERE source_id=$1`, source.ID) != 0 {
		t.Fatal("purged source retained tombstone")
	}
}

func TestRetryRacesLateSourceAcknowledgements(t *testing.T) {
	f := newFixture(t)
	c := f.device(f.user("lateackretry"))
	ctx := context.Background()
	f.present(c, live("late-source", "claude", "/retry", false), live("late-target", "codex", "/retry", false))
	req := busproto.SendRequest{FromSession: "late-source", To: "late-target", Body: "synthetic late ack retry"}
	source, err := f.s.Send(ctx, c, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Ack(ctx, c, busproto.AckRequest{SessionEnded: []string{source.ID}}); err != nil {
		t.Fatal(err)
	}
	req.RetryOf = source.ID
	var wg sync.WaitGroup
	errCh := make(chan error, 3)
	wg.Add(3)
	go func() { defer wg.Done(); _, err := f.s.Send(ctx, c, req); errCh <- err }()
	go func() {
		defer wg.Done()
		ack, err := f.s.Ack(ctx, c, busproto.AckRequest{IDs: []string{source.ID}})
		if err == nil && len(ack.Rejected) != 1 {
			err = errors.New("late delivery ack accepted")
		}
		errCh <- err
	}()
	go func() {
		defer wg.Done()
		_, err := f.s.Ack(ctx, c, busproto.AckRequest{Undelivered: []string{source.ID}})
		errCh <- err
	}()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.count(`SELECT count(*) FROM bus_messages WHERE id=$1 AND state='undelivered' AND reason='session_ended' AND delivered_at IS NULL`, source.ID) != 1 {
		t.Fatal("late ack mutated terminal source")
	}
	if f.count(`SELECT count(*) FROM bus_retries WHERE source_id=$1`, source.ID) != 1 {
		t.Fatal("retry race lost stable successor")
	}
}

func TestResumeRetryCapabilityGatesCloudClaims(t *testing.T) {
	f := newFixture(t)
	c := f.device(f.user("cloudadmission"))
	ctx := context.Background()
	sender := live("cloud-admission-source", "claude", "/retry", false)
	target := live("cloud-admission-target", "claude", "/retry", false)
	if _, err := f.s.Poll(ctx, c, busproto.PollRequest{Sessions: []busproto.PresenceSession{sender}, Cloud: []busproto.PresenceSession{target}}); err != nil {
		t.Fatal(err)
	}
	req := busproto.SendRequest{FromSession: sender.SessionID, To: target.SessionID, Body: "synthetic cloud admission retry"}
	source, err := f.s.Send(ctx, c, req)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE bus_messages SET state='undelivered',reason='session_ended' WHERE id=$1`, source.ID)
	// Uploaded history resolves the same canonical session while no cloud
	// presence remains. Reappearing as cloud must retain its admission gate.
	f.uploaded(c, target.Agent, target.SessionID, "/retry")
	f.exec(`DELETE FROM bus_presence WHERE session_id=$1`, target.SessionID)
	req.RetryOf = source.ID
	next, err := f.s.Send(ctx, c, req)
	if err != nil || next.Admission != busproto.AdmissionNextResume {
		t.Fatalf("cloud retry: %+v %v", next, err)
	}
	pollReq := busproto.PollRequest{Sessions: []busproto.PresenceSession{sender}, Cloud: []busproto.PresenceSession{target}}
	old, err := f.s.Poll(ctx, c, pollReq)
	if err != nil || len(old.Messages) != 0 || len(old.Claimable) != 0 {
		t.Fatalf("old cloud poll: %+v %v", old, err)
	}
	claim := busproto.ClaimRequest{MessageID: next.ID, SessionID: target.SessionID, Agent: target.Agent}
	if _, err := f.s.Claim(ctx, c, claim); err == nil {
		t.Fatal("old cloud claim bypassed gate")
	}
	pollReq.ResumeRetries = true
	supported, err := f.s.Poll(ctx, c, pollReq)
	if err != nil || len(supported.Claimable) != 1 || supported.Claimable[0].Message.Admission != busproto.AdmissionNextResume {
		t.Fatalf("supported cloud poll: %+v %v", supported, err)
	}
	claim.ResumeRetries = true
	got, err := f.s.Claim(ctx, c, claim)
	if err != nil || got.Message.Admission != busproto.AdmissionNextResume {
		t.Fatalf("supported cloud claim: %+v %v", got, err)
	}
	claim.ResumeRetries = false
	if _, err := f.s.Claim(ctx, c, claim); err == nil {
		t.Fatal("old claim replay bypassed gate")
	}
	req.RetryOf = ""
	req.Body = "synthetic live cloud retry"
	liveSource, err := f.s.Send(ctx, c, req)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE bus_messages SET state='undelivered',reason='session_ended' WHERE id=$1`, liveSource.ID)
	req.RetryOf = liveSource.ID
	liveRetry, err := f.s.Send(ctx, c, req)
	if err != nil || liveRetry.Admission != "" {
		t.Fatalf("live cloud retry: %+v %v", liveRetry, err)
	}
	pollReq.ResumeRetries = false
	oldLive, err := f.s.Poll(ctx, c, pollReq)
	if err != nil || len(oldLive.Messages) != 0 || len(oldLive.Claimable) != 0 {
		t.Fatalf("old cloud recipient saw live retry: %+v %v", oldLive, err)
	}
	claim.MessageID = liveRetry.ID
	if _, err := f.s.Claim(ctx, c, claim); err == nil {
		t.Fatal("old cloud claim exposed live retry")
	}
}
