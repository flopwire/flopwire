package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/devicebus"
	"github.com/flopwire/flopwire/internal/localindex"
)

type excerptContractServer struct {
	client.Bus
	read func(context.Context, string) (string, error)
}

func (s excerptContractServer) ReadReference(ctx context.Context, ref string) (string, error) {
	return s.read(ctx, ref)
}

// Connected devices use authenticated reference reads regardless of which
// transcripts or message rows happen to exist on the receiving device.
func TestControlPendingExcerptIndexModeContract(t *testing.T) {
	const text = "Synthetic authority excerpt with one precise fact."
	const want = "user: " + text
	for _, c := range []struct {
		name                           string
		invalidate                     string
		syncOnly, remoteOnly, rejected bool
	}{
		{name: "full"}, {name: "sync-only", syncOnly: true},
		{name: "remote-only", remoteOnly: true}, {name: "rejected-does-not-fallback", rejected: true},
		{name: "repin-invalidates", invalidate: "repin"}, {name: "redact-invalidates", invalidate: "redact"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, "-")
			if err := os.WriteFile(f.path(alphaRel), []byte(claudeUser("c9000000-0000-4000-8000-000000000099", "Different local text must never replace the authorized shared excerpt.")), 0600); err != nil {
				t.Fatal(err)
			}
			if c.syncOnly {
				store, err := localindex.Open(filepath.Join(t.TempDir(), "sync-only.db"), localindex.Options{DeferCommit: true, SyncOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { store.Close() })
				f.store = store
			}
			ref := fmt.Sprintf("%s/0", alphaID)
			if c.remoteOnly {
				ref = "remote99-0000-4000-8000-000000000099/0"
			}
			now := time.Now().UTC()
			envelope := busproto.Envelope{ID: "m-synthetic-authority", ThreadID: "m-synthetic-authority", From: "remote-sender", FromAgent: "claude", User: "sender@example.test", UserID: "synthetic-sender", Sender: busproto.SenderTeammate, Intent: busproto.IntentInform, Body: "Inspect the synthetic fact", Refs: []string{ref}, Sent: now, ExpiresAt: now.Add(time.Hour), ToSession: "to-2222", ToAgent: "claude", ToUser: "recipient@example.test", ToUserID: "synthetic-recipient", Addressed: "session", Seq: 1}
			reads := make(chan struct{}, 10)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case busproto.PathPoll:
					_ = json.NewEncoder(w).Encode(busproto.PollResponse{Cursor: 1, Now: time.Now().UTC(), Messages: []busproto.Envelope{envelope}})
				case busproto.PathAck:
					_ = json.NewEncoder(w).Encode(busproto.AckResponse{})
				case "/synthetic/reference":
					if r.URL.Query().Get("ref") != ref {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					select {
					case reads <- struct{}{}:
					default:
					}
					if r.Header.Get("Authorization") != "Bearer synthetic-device-token" {
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					if c.rejected {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					_ = json.NewEncoder(w).Encode(want)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			hc := server.Client()
			busClient := excerptContractServer{Bus: client.Bus{Server: server.URL, Token: "synthetic-device-token", HTTP: hc}}
			busClient.read = func(ctx context.Context, ref string) (string, error) {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/synthetic/reference?ref="+ref, nil)
				if err != nil {
					return "", err
				}
				req.Header.Set("Authorization", "Bearer synthetic-device-token")
				response, err := hc.Do(req)
				if err != nil {
					return "", err
				}
				defer response.Body.Close()
				if response.StatusCode != http.StatusOK {
					return "", &client.APIError{StatusCode: response.StatusCode}
				}
				var excerpt string
				err = json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&excerpt)
				return excerpt, err
			}
			b, err := devicebus.Open(filepath.Join(t.TempDir(), "bus.db"), devicebus.Config{Connect: func() (devicebus.Server, string) { return busClient, "synthetic-credential" }, Logger: f.cfg.Logger})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { b.Close() })
			f.cfg.Bus = b
			f.a = New(f.store, f.cfg)
			f.once()
			b.SetSources(func(context.Context) ([]devicebus.Session, error) {
				return []devicebus.Session{{PresenceSession: busproto.PresenceSession{SessionID: "to-2222", Agent: "claude"}, LastActive: time.Now()}}, nil
			}, nil)
			runCtx, cancel := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() { defer close(done); _ = b.Run(runCtx) }()
			t.Cleanup(func() { cancel(); <-done })
			deadline := time.Now().Add(10 * time.Second)
			for b.Status(ctx).Pending != 1 {
				if time.Now().After(deadline) {
					t.Fatal("timed out waiting for synthetic server delivery")
				}
				time.Sleep(5 * time.Millisecond)
			}
			response := hookAsk(t, f.a, Request{Op: "pending", Session: "to-2222"})
			raw, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("synthetic pending response: %s", raw)
			if !response.OK || len(response.Messages) != 1 {
				t.Fatalf("pending failed: %+v", response)
			}
			expected := want
			if c.rejected {
				expected = ""
			}
			if got := response.Excerpts[ref]; got != expected {
				t.Errorf("excerpt = %q, want %q", got, expected)
			}
			select {
			case <-reads:
			default:
				t.Error("authenticated reference reader was never called")
			}
			if c.invalidate != "" {
				// Stop refreshes so the control action is the only cache writer.
				cancel()
				<-done
				if before := b.ReferenceExcerpts(response.Messages); before[ref] != want {
					t.Fatalf("no snapshot before invalidation: %v", before)
				}
				request := Request{Op: c.invalidate}
				if c.invalidate == "redact" {
					request.Address = ref
				}
				result := ask(t, f.a, request)
				if !result.OK {
					t.Fatalf("%s failed: %s", c.invalidate, result.Error)
				}
				if after := b.ReferenceExcerpts(response.Messages); len(after) != 0 {
					t.Fatalf("%s retained snapshot: %v", c.invalidate, after)
				}
			}
		})
	}
}
