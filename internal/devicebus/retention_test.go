package devicebus_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/devicebus"
)

// offlineAcks makes a device's receipts fail while offline, and records
// every receipt batch that reached the server.
type offlineAcks struct {
	mu      sync.Mutex
	offline bool
	sent    []busproto.AckRequest
}

// gated is one connection of the device, through o.
type gated struct {
	devicebus.Server
	o *offlineAcks
}

func (g gated) Ack(ctx context.Context, req busproto.AckRequest) (busproto.AckResponse, error) {
	o := g.o
	o.mu.Lock()
	off := o.offline
	o.mu.Unlock()
	if off {
		return busproto.AckResponse{}, errors.New("offline")
	}
	resp, err := g.Server.Ack(ctx, req)
	if err == nil {
		o.mu.Lock()
		o.sent = append(o.sent, req)
		o.mu.Unlock()
	}
	return resp, err
}

func (o *offlineAcks) setOffline(v bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.offline = v
}

// mentions counts the batches that reached the server and named id as a
// delivery receipt (read false) or a read receipt (read true).
func (o *offlineAcks) mentions(id string, read bool) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, r := range o.sent {
		if (!read && slices.Contains(r.IDs, id)) || (read && slices.ContainsFunc(r.Read, func(x busproto.ReadReceipt) bool { return x.ID == id })) {
			n++
		}
	}
	return n
}

// A device offline for longer than the retention, with a delivery receipt
// and a read receipt owed for messages the server has since deleted, sends
// each once when it is back: the server rejects them, the device stops
// owing them, and the other receipts of the same batches are taken (#70).
func TestReceiptsForMessagesTheServerDeleted(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	gary := s.member("gary@example.test")
	lap := s.agent(s.device(gary), live("g-lap-1111", "claude", "/src/api", true))
	gate := &offlineAcks{}
	desk := s.agentVia(s.device(gary), func(srv devicebus.Server) devicebus.Server {
		return gated{Server: srv, o: gate}
	}, live("g-desk-2222", "codex", "/home/g/api", true))
	reported(t, lap, "g-desk-2222")
	reported(t, desk, "g-lap-1111")

	send := func(body string) string {
		out, err := lap.Send(ctx, busproto.SendRequest{FromSession: "g-lap-1111", To: "g-desk", Body: body})
		if err != nil {
			t.Fatal(err)
		}
		return out.ID
	}
	deliver := func(want ...string) {
		var got []string
		waitFor(t, "delivery on the desktop", func() bool {
			es, err := desk.hook(ctx, "g-desk-2222")
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range es {
				got = append(got, e.ID)
			}
			return len(got) >= len(want)
		})
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("delivered %v, want %v", got, want)
		}
	}
	// gone and kept are delivered and their receipts taken.
	gone, kept := send("deleted before its read receipt"), send("kept")
	deliver(gone, kept)
	for _, id := range []string{gone, kept} {
		waitFor(t, "the delivery receipt", func() bool { return sentState(t, lap, "g-lap-1111", id) == busproto.StateDelivered })
	}
	// Offline: both are read, and two more are delivered; every receipt
	// is owed.
	gate.setOffline(true)
	lost, late := send("deleted before its delivery receipt"), send("late but kept")
	deliver(lost, late)
	at := time.Now().UTC()
	if err := desk.MarkRead(ctx, []devicebus.Read{{Session: "g-desk-2222", Agent: "codex", ID: gone, At: at}, {Session: "g-desk-2222", Agent: "codex", ID: kept, At: at}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "receipts owed", func() bool { return desk.Status(ctx).Unacked == 4 })

	// The server's retention deletes gone and lost: a sweep sees their
	// expiry more than the retention past.
	if _, err := s.pool.Exec(ctx, `UPDATE bus_messages SET expires_at=now()-interval '9 days' WHERE id=ANY($1)`, []string{gone, lost}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.bus.Sweep(ctx); err != nil || n.Deleted != 2 {
		t.Fatalf("sweep %+v %v", n, err)
	}

	gate.setOffline(false)
	waitFor(t, "receipts settled", func() bool { return desk.Status(ctx).Unacked == 0 })
	waitFor(t, "the late delivery receipt", func() bool { return sentState(t, lap, "g-lap-1111", late) == busproto.StateDelivered })
	waitFor(t, "the read receipt", func() bool { return sentState(t, lap, "g-lap-1111", kept) == busproto.StateRead })
	// Well past the backoff and the poll: neither rejected receipt is sent
	// again.
	time.Sleep(time.Second)
	if n := gate.mentions(gone, true); n != 1 {
		t.Fatalf("the read receipt for deleted message %s reached the server %d times", gone, n)
	}
	if n := gate.mentions(lost, false); n != 1 {
		t.Fatalf("the delivery receipt for deleted message %s reached the server %d times", lost, n)
	}
	if st := desk.Status(ctx); st.Unacked != 0 || st.State != devicebus.StateConnected {
		t.Fatalf("desk status %+v", st)
	}
}
