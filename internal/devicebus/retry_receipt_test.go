package devicebus

import (
	"context"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
)

// Build a real failed source through the session lifecycle, then retry either
// while it is closed or after its next hook revives it. No message state is set
// directly by the fixture.
func retryReceiptFixture(t *testing.T, closed bool) (*localBus, busproto.SendResponse, func() busproto.SendResponse) {
	t.Helper()
	lb := newLocalBus(t)
	source, err := lb.send(t, "aaaa1111", "bbbb", "synthetic retry receipt")
	if err != nil {
		t.Fatal(err)
	}
	target := sess(refB.Session, refB.Agent, "/src/web", false)
	if err = lb.st.noteLive(ctx, []Session{target}, lb.cfg.Now()); err != nil {
		t.Fatal(err)
	}
	lb.advance(time.Millisecond)
	if err = lb.End(ctx, refB, lb.cfg.Now()); err != nil {
		t.Fatal(err)
	}
	if item := sentItem(t, lb.Bus, "aaaa1111", source.ID); item.State != busproto.StateUndelivered || item.Reason != busproto.ReasonSessionEnded {
		t.Fatalf("real failed source: %+v", item)
	}
	if closed {
		lb.p.set(sess("aaaa1111", "claude", "/src/api", true))
		lb.SetSources(lb.p.get, func(context.Context, string) ([]Session, error) { return []Session{target}, nil })
	} else {
		lb.advance(time.Millisecond)
		if err = lb.Revive(ctx, refB.Session, lb.cfg.Now()); err != nil {
			t.Fatal(err)
		}
	}
	submit := func() busproto.SendResponse {
		t.Helper()
		got, err := lb.send(t, "aaaa1111", refB.Session, "synthetic retry receipt", func(r *busproto.SendRequest) { r.RetryOf = source.ID })
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	retry := submit()
	if retry.Replayed {
		t.Fatal("first retry was replayed")
	}
	return lb, retry, submit
}

func assertRetryReplayMatchesInbox(t *testing.T, lb *localBus, retry busproto.SendResponse, submit func() busproto.SendResponse, want busproto.State) {
	t.Helper()
	replay := submit()
	item := sentItem(t, lb.Bus, "aaaa1111", retry.ID)
	if !replay.Replayed || replay.ID != retry.ID || replay.State != want || replay.State != item.State || replay.Reason != item.Reason || replay.Admission != retry.Admission {
		t.Fatalf("replay %+v, inbox %+v, want %s", replay, item, want)
	}
}

func TestLocalRetryReplayReportsActualReadReceipt(t *testing.T) {
	lb, retry, submit := retryReceiptFixture(t, false)
	assertRetryReplayMatchesInbox(t, lb, retry, submit, busproto.StateQueued)
	got, err := lb.Take(ctx, refB.Session, refB.Agent, Limit{})
	if err != nil || len(got) != 1 || got[0].ID != retry.ID {
		t.Fatalf("take: %+v %v", got, err)
	}
	assertRetryReplayMatchesInbox(t, lb, retry, submit, busproto.StateQueued)
	lb.advance(time.Millisecond)
	if err = lb.MarkRead(ctx, []Read{{Session: refB.Session, Agent: refB.Agent, ID: retry.ID, At: lb.cfg.Now()}}); err != nil {
		t.Fatal(err)
	}
	// A transcript sighting counts publicly only after delivery confirmation.
	assertRetryReplayMatchesInbox(t, lb, retry, submit, busproto.StateQueued)
	if err = lb.Confirm(ctx, refB.Session, []string{retry.ID}); err != nil {
		t.Fatal(err)
	}
	assertRetryReplayMatchesInbox(t, lb, retry, submit, busproto.StateRead)
	lb.advance(busproto.DefaultTTL + time.Second)
	assertRetryReplayMatchesInbox(t, lb, retry, submit, busproto.StateRead)
}

func TestLocalRetryReplayReportsClockDerivedExpiry(t *testing.T) {
	for _, closed := range []bool{false, true} {
		for _, take := range []bool{false, true} {
			name := map[bool]string{false: "ordinary", true: "next-resume"}[closed] + "/" + map[bool]string{false: "unleased", true: "leased"}[take]
			t.Run(name, func(t *testing.T) {
				lb, retry, submit := retryReceiptFixture(t, closed)
				if take {
					got, err := lb.Take(ctx, refB.Session, refB.Agent, Limit{})
					if err != nil || len(got) != 1 || got[0].ID != retry.ID {
						t.Fatalf("take: %+v %v", got, err)
					}
				}
				assertRetryReplayMatchesInbox(t, lb, retry, submit, busproto.StateQueued)
				lb.advance(busproto.DefaultTTL)
				assertRetryReplayMatchesInbox(t, lb, retry, submit, busproto.StateExpired)
				var n int
				if err := lb.st.db.QueryRow(`SELECT count(*) FROM devbus_retries`).Scan(&n); err != nil || n != 1 {
					t.Fatalf("expiry replay changed successor identity: %d %v", n, err)
				}
			})
		}
	}
}
