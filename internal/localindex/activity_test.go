package localindex

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

func TestSessionActivitiesBatchesAndKeepsLatest(t *testing.T) {
	s := openTest(t, DetailColumn)
	var sessions []string
	for i := 0; i < 805; i++ {
		session := fmt.Sprintf("session-%d", i)
		sessions = append(sessions, session)
		if _, err := s.wdb.Exec(`INSERT INTO conversations(agent,session_id,device_id,last_activity_at) VALUES('devin',?,'d',?)`, session, int64(1000+i)); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct {
		agent, session, device string
		at                     any
	}{
		{"devin", "session-0", "other", int64(9000)},
		{"claude", "session-0", "d", int64(99999)},
		{"devin", "undated", "d", nil},
	} {
		if _, err := s.wdb.Exec(`INSERT INTO conversations(agent,session_id,device_id,last_activity_at) VALUES(?,?,?,?)`, row.agent, row.session, row.device, row.at); err != nil {
			t.Fatal(err)
		}
	}
	sessions = append(sessions, "unknown", "undated", "session-0")
	got, err := s.SessionActivities(context.Background(), transcript.AgentDevin, sessions)
	if err != nil {
		t.Fatal(err)
	}
	if !got["session-0"].Equal(time.UnixMilli(9000)) || !got["session-804"].Equal(time.UnixMilli(1804)) {
		t.Fatalf("incorrect activity at batch bounds: %+v %+v", got["session-0"], got["session-804"])
	}
	if !got["unknown"].IsZero() || !got["undated"].IsZero() {
		t.Fatal("unknown activity acquired a timestamp")
	}
	if len(got) != 807 {
		t.Fatalf("sessions=%d", len(got))
	}
}

func TestSessionActivitiesHonorsCancellation(t *testing.T) {
	s := openTest(t, DetailColumn)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.SessionActivities(ctx, transcript.AgentDevin, []string{"s"}); err != context.Canceled {
		t.Fatalf("error=%v", err)
	}
	got, err := s.SessionActivities(context.Background(), transcript.AgentDevin, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty query: %+v %v", got, err)
	}
}
