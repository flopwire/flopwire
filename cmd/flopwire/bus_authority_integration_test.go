package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/devicebus"
)

type observedAuthorityServer struct {
	devicebus.Server
	acks  *atomic.Int32
	polls *atomic.Int32
}

func (s observedAuthorityServer) Poll(ctx context.Context, _ busproto.PollRequest) (busproto.PollResponse, error) {
	s.polls.Add(1)
	<-ctx.Done()
	return busproto.PollResponse{}, ctx.Err()
}
func (s observedAuthorityServer) Ack(ctx context.Context, req busproto.AckRequest) (busproto.AckResponse, error) {
	s.acks.Add(int32(len(req.IDs)))
	return s.Server.Ack(ctx, req)
}

// Both authorities use the real API, independent Postgres databases and enrolled
// devices. No poll runs in the new scope until cached hook delivery is checked.
func TestAuthorityInboxAcrossRealServers(t *testing.T) {
	a, b := newAcceptServer(t), newAcceptServer(t)
	dir := t.TempDir()
	enroll := func(s acceptServer) client.Config {
		out := apiCall(t, s.url, s.gary, "POST", "/v1/devices", map[string]string{"name": "receiver", "platform": "test"})
		return client.Config{Server: s.url, Token: out["token"].(string), DeviceID: out["device"].(map[string]any)["id"].(string)}
	}
	ca, cb := enroll(a), enroll(b)
	var bAcks, bPolls atomic.Int32
	otherDevice := enroll(a)
	open := func(cc client.Config, connected bool) *devicebus.Bus {
		t.Helper()
		p, err := authorityBusPath(dir, cc, connected)
		if err != nil {
			t.Fatal(err)
		}
		cfg := devicebus.Config{Logger: quiet, AckDelay: time.Millisecond, PollWait: time.Millisecond}
		if connected {
			cfg.Connect = busConnect(cc, func() (client.Config, error) { return cc, nil })
			if cc.Server == b.url {
				cfg.Connect = func() (devicebus.Server, string) {
					return observedAuthorityServer{Server: client.Bus{Server: cc.Server, Token: cc.Token}, acks: &bAcks, polls: &bPolls}, cc.Token
				}
			}
		}
		bus, err := devicebus.Open(p, cfg)
		if err != nil {
			t.Fatal(err)
		}
		bus.SetSources(func(context.Context) ([]devicebus.Session, error) {
			return []devicebus.Session{{PresenceSession: busproto.PresenceSession{SessionID: "receiver-1111", Agent: "claude", Repo: "/test"}, LastActive: time.Now()}}, nil
		}, nil)
		return bus
	}
	run := func(bus *devicebus.Bus) func() {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- bus.Run(ctx) }()
		return func() {
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
	}
	wait := func(fn func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !fn() {
			if time.Now().After(deadline) {
				t.Fatal("transition did not complete")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	// Register the receiver before sending, independently of its polling loop.
	apiCall(t, a.url, ca.Token, "POST", busproto.PathPoll, busproto.PollRequest{Sessions: []busproto.PresenceSession{{SessionID: "receiver-1111", Agent: "claude", Repo: "/test"}}})
	receipt := a.send(t, "receiver-1111", "confirmed on A")
	pending := a.send(t, "receiver-1111", "pending on A")
	ba := open(ca, true)
	stop := run(ba)
	wait(func() bool { return ba.Status(t.Context()).Pending == 2 })
	stop()
	got, err := ba.Take(t.Context(), "receiver-1111", "", devicebus.Limit{})
	if err != nil || len(got) != 2 {
		t.Fatalf("A take: %+v %v", got, err)
	}
	if err := ba.Confirm(t.Context(), "receiver-1111", []string{receipt.ID}); err != nil {
		t.Fatal(err)
	}
	// Requeue the unconfirmed lease so restart must preserve both body and receipt.
	if err := ba.Requeue(t.Context(), []string{pending.ID}); err != nil {
		t.Fatal(err)
	}
	ba.Close()
	for _, scope := range []struct {
		cc        client.Config
		connected bool
	}{{cb, false}, {cb, true}, {otherDevice, true}} {
		other := open(scope.cc, scope.connected)
		got, err := other.Take(t.Context(), "receiver-1111", "", devicebus.Limit{})
		if err != nil || len(got) != 0 {
			t.Fatalf("foreign cached delivery: %+v %v", got, err)
		}
		stop = run(other)
		if scope.connected && scope.cc.Server == b.url {
			wait(func() bool { return bPolls.Load() > 0 })
		}
		// Allow the startup receipt worker to complete its batching delay while
		// B's initial poll remains blocked, so it cannot reconcile away old rows.
		time.Sleep(20 * time.Millisecond)
		if other.Status(t.Context()).Unacked != 0 {
			t.Fatal("foreign scope inherited owed receipts")
		}
		stop()
		other.Close()
	}
	if bAcks.Load() != 0 {
		t.Fatal("A receipt was sent to B while its initial poll was blocked")
	}
	// B's database must never contain A's IDs.
	var count int
	if err := b.pool.QueryRow(t.Context(), `SELECT count(*) FROM bus_messages WHERE id::text = ANY($1)`, []string{receipt.ID, pending.ID}).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("A message appeared at B")
	}
	ba = open(ca, true)
	got, err = ba.Take(t.Context(), "receiver-1111", "", devicebus.Limit{})
	if err != nil || len(got) != 1 || got[0].ID != pending.ID || got[0].Body != "pending on A" {
		t.Fatalf("A restart lost pending body: %+v %v", got, err)
	}
	stop = run(ba)
	wait(func() bool {
		var out busproto.InboxResponse
		err := (client.HTTP{Server: a.url, Token: a.garyDevice}).JSON(t.Context(), "GET", busproto.PathInbox+"?session=gary-1111&sent=true", nil, &out)
		for _, m := range out.Messages {
			if m.ID == receipt.ID && m.State == busproto.StateDelivered {
				return err == nil
			}
		}
		return false
	})
	stop()
	ba.Close()
}
