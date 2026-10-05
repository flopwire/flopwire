package bus_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
)

// count runs a count(*) query.
func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// Repeated refusals of one session with one code within the hour are one
// stored row and one audit row, with a count (#70). They still count
// toward the hourly ceilings (TestLimits).
func TestRefusalsCoalesce(t *testing.T) {
	tm := newTeam(t)
	ctx := context.Background()
	for i := range busproto.SessionPerHour {
		tm.mustSend(tm.garyMac, "g-api-1111", "g-lin", fmt.Sprintf("n%d", i))
	}
	refusedRows := `SELECT count(*) FROM bus_messages WHERE from_session='g-api-1111' AND state='refused'`
	refusedAudit := `SELECT count(*) FROM audit_events WHERE action='bus.send' AND metadata->>'refuse_reason' IS NOT NULL`
	var first string
	for i := range 100 {
		_, err := tm.send(tm.garyMac, "g-api-1111", "g-lin", fmt.Sprintf("loop %d", i))
		var be *busproto.Error
		if code(err) != busproto.CodeSessionRate || !errors.As(err, &be) {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if first == "" {
			first = be.MessageID
		}
		if be.MessageID != first {
			t.Fatalf("attempt %d refused as %s, want %s", i, be.MessageID, first)
		}
		if i%10 == 9 && i < 99 {
			tm.advance(time.Second)
			tm.presence()
		}
	}
	if got := tm.count(refusedRows); got != 1 {
		t.Fatalf("refused rows %d, want 1", got)
	}
	if got := tm.count(refusedAudit); got != 1 {
		t.Fatalf("refusal audit rows %d, want 1", got)
	}
	if got := tm.count(`SELECT attempts FROM bus_messages WHERE id=$1 AND last_at=$2`, first, tm.now); got != 100 {
		t.Fatalf("attempts %d", got)
	}
	in, err := tm.s.Inbox(ctx, tm.garyMac, busproto.InboxQuery{Session: "g-api-1111", SentOnly: true, Limit: busproto.InboxMaxLimit})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range in.Messages {
		found = found || m.ID == first
		switch {
		case m.ID == first && (m.Attempts != 100 || m.LastAt == nil || !m.LastAt.Equal(tm.now)):
			t.Fatalf("refused row in the inbox %+v", m)
		case m.ID != first && (m.Attempts != 0 || m.LastAt != nil):
			t.Fatalf("a single send with attempts %+v", m)
		}
	}
	if !found || len(in.Messages) != busproto.SessionPerHour+1 {
		t.Fatalf("inbox lists %d, refused row found %v", len(in.Messages), found)
	}
	// Another code is another row; so is another session.
	if _, err := tm.send(tm.garyMac, "g-web-2222", "g-lin", "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := tm.send(tm.garyMac, "g-web-2222", "g-lin", "x"); code(err) != busproto.CodeDuplicate {
		t.Fatalf("duplicate: %v", err)
	}
	if got := tm.count(`SELECT count(*) FROM bus_messages WHERE state='refused'`); got != 2 {
		t.Fatalf("refused rows across sessions %d", got)
	}
	// An hour after the first refusal, a refusal starts a new row.
	tm.now = tm.now.Add(time.Hour)
	tm.presence()
	// Spread over recipients: each takes at most MaxUndelivered.
	for i := range busproto.SessionPerHour {
		tm.mustSend(tm.garyMac, "g-api-1111", []string{"g-web", "a-api"}[i%2], fmt.Sprintf("h2 %d", i))
	}
	if _, err := tm.send(tm.garyMac, "g-api-1111", "g-lin", "again"); code(err) != busproto.CodeSessionRate {
		t.Fatalf("next hour: %v", err)
	}
	if got := tm.count(refusedRows); got != 2 {
		t.Fatalf("refused rows in the next hour %d, want 2", got)
	}
}

// A refusal coalesces only with one to the same recipient in the same
// thread: the sender's inbox must show a refusal for each recipient and
// thread it tried, and the refusal's id names a message to that
// recipient in that thread.
func TestRefusalsCoalescePerRecipientAndThread(t *testing.T) {
	tm := newTeam(t)
	ctx := context.Background()
	refusedID := func(err error, want string) string {
		t.Helper()
		var be *busproto.Error
		if code(err) != want || !errors.As(err, &be) {
			t.Fatalf("want %s: %v", want, err)
		}
		return be.MessageID
	}
	// Duplicates to two recipients.
	tm.mustSend(tm.garyMac, "g-api-1111", "g-lin", "same text")
	_, err := tm.send(tm.garyMac, "g-api-1111", "g-lin", "same text")
	toLin := refusedID(err, busproto.CodeDuplicate)
	tm.mustSend(tm.garyMac, "g-api-1111", "g-web", "same text")
	_, err = tm.send(tm.garyMac, "g-api-1111", "g-web", "same text")
	toWeb := refusedID(err, busproto.CodeDuplicate)
	if toWeb == toLin {
		t.Fatalf("a refusal to g-web was recorded as the one to g-lin (%s)", toLin)
	}
	if got := tm.count(`SELECT count(*) FROM bus_messages WHERE id=$1 AND to_session='g-web-2222' AND state='refused'`, toWeb); got != 1 {
		t.Fatalf("refused row %s is not to g-web", toWeb)
	}
	// Replies refused in two threads (each closed by done).
	d1 := tm.mustSend(tm.garyLinux, "g-lin-3333", "g-api", "first done", intent(busproto.IntentDone))
	d2 := tm.mustSend(tm.garyLinux, "g-lin-3333", "g-api", "second done", intent(busproto.IntentDone))
	_, err = tm.send(tm.garyMac, "g-api-1111", "g-lin", "thanks 1", replyTo(d1.ID))
	in1 := refusedID(err, busproto.CodeReplyToDone)
	_, err = tm.send(tm.garyMac, "g-api-1111", "g-lin", "thanks 2", replyTo(d2.ID))
	in2 := refusedID(err, busproto.CodeReplyToDone)
	if in1 == in2 {
		t.Fatalf("a refusal in thread %s was recorded in thread %s", d2.ThreadID, d1.ThreadID)
	}
	in, err := tm.s.Inbox(ctx, tm.garyMac, busproto.InboxQuery{Session: "g-api-1111", SentOnly: true, Thread: d2.ThreadID})
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Messages) != 1 || in.Messages[0].ID != in2 || in.Messages[0].State != busproto.StateRefused {
		t.Fatalf("thread %s in the sender's inbox: %+v", d2.ThreadID, in.Messages)
	}
	// A root send's refusal does not coalesce into a reply's.
	tm.mustSend(tm.garyMac, "g-api-1111", "g-lin", "root text")
	_, err = tm.send(tm.garyMac, "g-api-1111", "g-lin", "thanks 1", replyTo(d1.ID))
	if again := refusedID(err, busproto.CodeReplyToDone); again != in1 {
		t.Fatalf("same thread again: %s, want %s", again, in1)
	}
	_, err = tm.send(tm.garyMac, "g-api-1111", "g-lin", "root text")
	if root := refusedID(err, busproto.CodeDuplicate); root != toLin {
		t.Fatalf("root duplicate to g-lin: %s, want %s", root, toLin)
	}
}
