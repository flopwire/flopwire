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
