package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/devicebus"
)

func TestAuthorityInboxRetainsOnlyItsOwnBodiesAndReceipts(t *testing.T) {
	var ackA, ackB atomic.Int32
	makeServer := func(offer bool, acks *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == busproto.PathPoll {
				out := busproto.PollResponse{Now: time.Now()}
				if offer {
					out.Messages = []busproto.Envelope{
						{ID: "receipt-a", Body: "private A receipt", ToSession: "session", ExpiresAt: time.Now().Add(time.Hour)},
						{ID: "body-a", Body: "private A pending body", ToSession: "pending", ExpiresAt: time.Now().Add(time.Hour)},
					}
				}
				json.NewEncoder(w).Encode(out)
			} else if r.URL.Path == busproto.PathAck {
				var req busproto.AckRequest
				json.NewDecoder(r.Body).Decode(&req)
				acks.Add(int32(len(req.IDs)))
				w.Write([]byte(`{}`))
			} else {
				w.Write([]byte(`{}`))
			}
		}))
	}
	a := makeServer(true, &ackA)
	defer a.Close()
	b := makeServer(false, &ackB)
	defer b.Close()
	dir := t.TempDir()
	ca := client.Config{Server: a.URL, Token: "old", DeviceID: "device-a"}
	cb := client.Config{Server: b.URL, Token: "other", DeviceID: "device-a"}
	open := func(cc client.Config, connected bool) *devicebus.Bus {
		t.Helper()
		path, err := authorityBusPath(dir, cc, connected)
		if err != nil {
			t.Fatal(err)
		}
		cfg := devicebus.Config{AckDelay: time.Millisecond, PollWait: time.Millisecond}
		if connected {
			cfg.Connect = busConnect(cc, func() (client.Config, error) { return cc, nil })
		}
		bus, err := devicebus.Open(path, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return bus
	}
	run := func(bus *devicebus.Bus) func() {
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
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
		until := time.Now().Add(5 * time.Second)
		for !fn() {
			if time.Now().After(until) {
				t.Fatal("timed out")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	ba := open(ca, true)
	stop := run(ba)
	wait(func() bool {
		got, err := ba.Take(t.Context(), "session", "", devicebus.Limit{})
		if err != nil {
			t.Fatal(err)
		}
		return len(got) == 1
	})
	stop()
	if err := ba.Confirm(t.Context(), "session", []string{"receipt-a"}); err != nil {
		t.Fatal(err)
	}
	ba.Close()
	for _, connected := range []bool{true, false} {
		other := open(cb, connected)
		got, err := other.Take(t.Context(), "pending", "", devicebus.Limit{})
		if err != nil || len(got) != 0 {
			t.Fatalf("foreign body: %+v, %v", got, err)
		}
		stop = run(other)
		time.Sleep(30 * time.Millisecond)
		stop()
		other.Close()
	}
	if ackB.Load() != 0 || ackA.Load() != 0 {
		t.Fatalf("receipt crossed authority: A=%d B=%d", ackA.Load(), ackB.Load())
	}
	ca.Token = "rotated"
	ba = open(ca, true)
	got, err := ba.Take(t.Context(), "pending", "", devicebus.Limit{})
	if err != nil || len(got) != 1 || got[0].Body != "private A pending body" {
		t.Fatalf("same authority lost body: %+v %v", got, err)
	}
	stop = run(ba)
	wait(func() bool { return ackA.Load() > 0 })
	stop()
	ba.Close()
	if ackB.Load() != 0 {
		t.Fatal("B received A receipt")
	}
}

func TestAuthorityBusPathIdentity(t *testing.T) {
	cfg := client.Config{Server: "https://EXAMPLE.test:443/", DeviceID: "d1", Token: "a"}
	p, err := authorityBusPath("root", cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Server = "https://example.test"
	cfg.Token = "b"
	cfg.TLSFingerprint = "new pin"
	same, _ := authorityBusPath("root", cfg, true)
	if same != p {
		t.Fatal("rotation or URL spelling changed inbox")
	}
	cfg.DeviceID = "d2"
	other, _ := authorityBusPath("root", cfg, true)
	if other == p {
		t.Fatal("devices share inbox")
	}
	local, _ := authorityBusPath("root", cfg, false)
	if local == p || filepath.Base(local) == "bus.db" {
		t.Fatal("standalone shares an unbound inbox")
	}
}

func TestBusConnectKeepsEnrolledDeviceAuthority(t *testing.T) {
	var token string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token = r.Header.Get("Authorization")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	initial := client.Config{Server: srv.URL, DeviceID: "d1", Token: "original"}
	saved := initial
	saved.DeviceID = "d2"
	saved.Token = "other-device"
	connect := busConnect(initial, func() (client.Config, error) { return saved, nil })
	s, _ := connect()
	if _, err := s.Ack(t.Context(), busproto.AckRequest{IDs: []string{"old-device-receipt"}}); err != nil {
		t.Fatal(err)
	}
	if token != "Bearer original" {
		t.Fatalf("old receipt used new device credential: %q", token)
	}
	saved = initial
	saved.Token = "rotated"
	s, _ = connect()
	if _, err := s.Ack(t.Context(), busproto.AckRequest{}); err != nil {
		t.Fatal(err)
	}
	if token != "Bearer rotated" {
		t.Fatalf("same-device rotation ignored: %q", token)
	}
	initial.DeviceID = ""
	saved = initial
	saved.Token = "different-principal"
	connect = busConnect(initial, func() (client.Config, error) { return saved, nil })
	s, _ = connect()
	if _, err := s.Ack(t.Context(), busproto.AckRequest{}); err != nil {
		t.Fatal(err)
	}
	if token != "Bearer original" {
		t.Fatalf("unbound principal changed: %q", token)
	}
}

func TestAuthorityBusPathUnboundTokens(t *testing.T) {
	cfg := client.Config{Server: "https://example.test", Token: "principal-a"}
	a, _ := authorityBusPath("root", cfg, true)
	cfg.Token = "principal-b"
	b, _ := authorityBusPath("root", cfg, true)
	if a == b {
		t.Fatal("unbound token principals share inbox")
	}
	cfg.DeviceID = "principal-b"
	enrolled, _ := authorityBusPath("root", cfg, true)
	if enrolled == b {
		t.Fatal("token and device namespaces overlap")
	}
}

func TestLegacyInboxPreservedWithoutResuming(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "bus.db")
	legacy, err := devicebus.Open(legacyPath, devicebus.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(busproto.Envelope{ID: "legacy-private", Body: "legacy private body", ToSession: "session", ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO devbus_messages(id,origin,seq,to_session,to_agent,from_session,from_agent,thread_id,envelope,state,created_at,expires_at,ack) VALUES('legacy-private','server',1,'session','','','','',?,'delivered',?,?, 'owed')`, string(envelope), time.Now().UnixMilli(), time.Now().Add(time.Hour).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	pending, err := json.Marshal(busproto.Envelope{ID: "legacy-pending", Body: "legacy pending private body", ToSession: "session", ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO devbus_messages(id,origin,seq,to_session,to_agent,from_session,from_agent,thread_id,envelope,state,created_at,expires_at) VALUES('legacy-pending','server',2,'session','','','','',?,'queued',?,?)`, string(pending), time.Now().UnixMilli(), time.Now().Add(time.Hour).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == busproto.PathAck {
			requests.Add(1)
		}
		json.NewEncoder(w).Encode(busproto.PollResponse{Now: time.Now()})
	}))
	defer srv.Close()
	cc := client.Config{Server: srv.URL, Token: "device-token", DeviceID: "device"}
	var logs bytes.Buffer
	for _, connected := range []bool{false, true} {
		cfg := devicebus.Config{Logger: slog.New(slog.NewTextHandler(&logs, nil)), AckDelay: time.Millisecond}
		if connected {
			cfg.Connect = busConnect(cc, func() (client.Config, error) { return cc, nil })
		}
		fresh := openAuthorityBus(dir, cc, cfg)
		if fresh == nil {
			t.Fatal("fresh authority inbox failed")
		}
		got, err := fresh.Take(t.Context(), "session", "", devicebus.Limit{})
		if err != nil || len(got) != 0 {
			t.Fatalf("resumed legacy body: %+v %v", got, err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- fresh.Run(ctx) }()
		time.Sleep(30 * time.Millisecond)
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if err := fresh.Close(); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("legacy inbox modified")
	}
	if requests.Load() != 0 {
		t.Fatal("legacy receipt resumed")
	}
	if !strings.Contains(logs.String(), "legacy inbox preserved without resuming") {
		t.Fatalf("migration not logged: %s", logs.String())
	}
}
