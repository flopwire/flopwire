package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/devicebus"
)

// The agent rotates the device credential at startup when it was never
// rotated, and daily after. Between the server's commit and the config
// save the server refuses the old token while the config still names it.
// A bus call that read the old token in that window waits for the
// rotation (it holds the config lock) and retries once with the new
// token, as sync does (issue #110: a send failed with 401). A token the
// server refuses that no rotation replaced is not retried.
func TestBusConnectFollowsARotationInFlight(t *testing.T) {
	calls := map[string]func(devicebus.Server) error{
		"send":  func(s devicebus.Server) error { _, err := s.Send(t.Context(), busproto.SendRequest{}); return err },
		"poll":  func(s devicebus.Server) error { _, err := s.Poll(t.Context(), busproto.PollRequest{}); return err },
		"claim": func(s devicebus.Server) error { _, err := s.Claim(t.Context(), busproto.ClaimRequest{}); return err },
		"ack":   func(s devicebus.Server) error { _, err := s.Ack(t.Context(), busproto.AckRequest{}); return err },
		"peers": func(s devicebus.Server) error { _, err := s.Peers(t.Context(), busproto.PeersQuery{}); return err },
		"inbox": func(s devicebus.Server) error { _, err := s.Inbox(t.Context(), busproto.InboxQuery{}); return err },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			var refusedOnce sync.Once
			refused := make(chan struct{})
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Header.Get("Authorization") != "Bearer new" {
					refusedOnce.Do(func() { close(refused) })
					w.Header().Set("Content-Type", "application/problem+json")
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte(`{"code":"credential_rotated","detail":"credential was rotated; log in again"}`))
					return
				}
				_, _ = w.Write([]byte(`{}`))
			}))
			t.Cleanup(srv.Close)
			t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
			t.Setenv(client.EnvToken, "")
			t.Setenv(client.EnvServer, "")
			old := client.Config{Server: srv.URL, Token: "old", DeviceID: "d1"}
			if err := client.Save(old); err != nil {
				t.Fatal(err)
			}
			connect := busConnect(old, client.Load)

			// A refused token with no rotation behind it: one request, 401.
			s, _ := connect()
			var ae *client.APIError
			if err := call(s); !errors.As(err, &ae) || ae.StatusCode != http.StatusUnauthorized || requests.Load() != 1 {
				t.Fatalf("refused credential: %v after %d requests", err, requests.Load())
			}

			// A rotation committed at the server, its save not yet done.
			s, _ = connect() // reads the old token
			locked, rotated := make(chan struct{}), make(chan error, 1)
			go func() {
				rotated <- client.WithConfigLock(context.Background(), func() error {
					close(locked)
					<-refused
					next := old
					next.Token = "new"
					return client.Save(next)
				})
			}()
			<-locked
			if err := call(s); err != nil {
				t.Fatalf("call during a rotation: %v", err)
			}
			if err := <-rotated; err != nil {
				t.Fatal(err)
			}
		})
	}
}
