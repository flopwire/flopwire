package devicebus

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
)

// An idle server answers only when the requested hold expires. A changed
// presence can cancel that hold before any response reaches the device.
type holdingServer struct {
	*fakeServer
	initialReply bool
	messages     []busproto.Envelope
}

func (s holdingServer) Poll(ctx context.Context, req busproto.PollRequest) (busproto.PollResponse, error) {
	s.mu.Lock()
	s.polls = append(s.polls, req)
	first := len(s.polls) == 1 && s.initialReply
	s.mu.Unlock()
	if first {
		return busproto.PollResponse{Cursor: 9, Gen: 4, Now: time.Now()}, nil
	}
	timer := time.NewTimer(time.Duration(req.WaitSeconds) * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return busproto.PollResponse{}, ctx.Err()
	case <-timer.C:
		return busproto.PollResponse{Cursor: 9, Gen: 4, Now: time.Now(), Messages: s.messages}, nil
	}
}

func TestPollConnectsBeforeIdleHold(t *testing.T) {
	srv := holdingServer{fakeServer: newFakeServer()}
	p := &presenceSrc{}
	p.set(sess("s1", "claude", "/src/api", false))
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(srv, nil), p)
	run(t, b)
	waitFor(t, "initial connection before the 25-second idle hold", func() bool { return b.Status(ctx).State == StateConnected })
	waitFor(t, "long poll after initial response", func() bool { return srv.pollCount() >= 2 })
	if req := srv.lastPoll(); req.WaitSeconds != 25 || req.Cursor != 9 || req.Gen != 4 {
		t.Fatalf("steady poll: %+v", req)
	}
}

func TestCancelledPresencePollMakesProgress(t *testing.T) {
	srv := holdingServer{fakeServer: newFakeServer(), initialReply: true, messages: []busproto.Envelope{env("progress-message", "s1")}}
	p := &presenceSrc{}
	s := sess("s1", "claude", "/src/api", false)
	p.set(s)
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(srv, nil), p)
	run(t, b)
	waitFor(t, "initial poll", func() bool { return srv.pollCount() >= 2 })
	// Repeated busy flips arrive sooner than the server's 25-second hold.
	// Every flip must eventually produce a response, not only a heartbeat.
	for i := 0; i < 3; i++ {
		before := b.Status(ctx).LastPoll
		s.Busy = !s.Busy
		p.set(s)
		waitFor(t, "response despite presence churn", func() bool { return b.Status(ctx).LastPoll.After(before) })
		waitFor(t, "updated presence", func() bool { r := srv.lastPoll(); return len(r.Sessions) == 1 && r.Sessions[0].Busy == s.Busy })
	}
	got, err := deliver(b, "s1", "claude", Limit{})
	if err != nil || len(got) != 1 || got[0].ID != "progress-message" {
		t.Fatalf("delivery after cancellation: %v %v", ids(got), err)
	}
	if again, err := deliver(b, "s1", "claude", Limit{}); err != nil || len(again) != 0 {
		t.Fatalf("duplicate delivery: %v %v", ids(again), err)
	}
}
