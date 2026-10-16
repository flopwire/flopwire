package devicebus

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
)

// replayingRetryServer represents a server that already accepted this retry.
// Privacy must be checked before even an idempotent replay reaches that server.
type replayingRetryServer struct{ *fakeServer }

func (s replayingRetryServer) Send(ctx context.Context, req busproto.SendRequest) (busproto.SendResponse, error) {
	if _, err := s.fakeServer.Send(ctx, req); err != nil {
		return busproto.SendResponse{}, err
	}
	return busproto.SendResponse{ID: "cached-successor", RetryOf: req.RetryOf, Replayed: true, State: busproto.StateDelivered}, nil
}

func TestRetryReplayChecksCurrentDevicePrivacyBeforeServerSend(t *testing.T) {
	for _, which := range []string{"request-ref", "source-ref", "sender"} {
		t.Run(which, func(t *testing.T) {
			fake := newFakeServer()
			p := &presenceSrc{}
			sender := sess("open-1111", "claude", "/src/api", true)
			p.set(sender)
			b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), testConfig(replayingRetryServer{fake}, nil), p)
			now := time.Now().UTC()
			source := busproto.Envelope{ID: "cached-source", RetryOf: "earlier-source", ThreadID: "original-thread", From: sender.SessionID, FromAgent: sender.Agent, ToSession: "recipient-2222", ToAgent: "codex", Body: "synthetic retry", Refs: []string{"archive-public/1"}, Sent: now, ExpiresAt: now.Add(time.Hour)}
			if which == "source-ref" {
				source.Refs = []string{"newly-private/1"}
			}
			successor := source
			successor.ID, successor.RetryOf = "cached-successor", source.ID
			if err := b.st.reconcile(ctx, []busproto.Envelope{source, successor}, nil, now); err != nil {
				t.Fatal(err)
			}
			req := busproto.SendRequest{FromSession: sender.SessionID, FromAgent: sender.Agent, To: source.ToSession, Body: source.Body, RetryOf: source.ID, Refs: []string{"archive-public/1"}}
			if which == "request-ref" {
				req.Refs = []string{"newly-private/1"}
			}
			// A path rule changes after both source and successor were imported.
			b.SetWithheld(func(_ context.Context, ref string) (string, error) {
				if ref == "newly-private/1" {
					return "newly-private", nil
				}
				return "", nil
			}, nil)
			if which == "sender" {
				sender.Withheld = true
				p.set(sender)
				b.mu.Lock()
				b.presence = presenceCache{}
				b.mu.Unlock()
			}
			_, err := b.Send(ctx, req)
			want := busproto.CodeWithheldSession
			if which == "sender" {
				want = busproto.CodeSessionNotOnDevice
			}
			if code(err) != want {
				t.Fatalf("privacy refusal: %v", err)
			}
			fake.mu.Lock()
			n := len(fake.sends)
			fake.mu.Unlock()
			if n != 0 {
				t.Fatalf("server received %d retry submissions after privacy changed", n)
			}
		})
	}
}

func TestRetryOfSurvivesServerImportReconcileReopenAndTake(t *testing.T) {
	for _, mode := range []string{"reconcile", "claim"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bus.db")
			st, err := openStore(path)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			e := busproto.Envelope{ID: "retry-successor", RetryOf: "terminal-source", ThreadID: "original-thread", ToSession: "recipient-1111", ToAgent: "codex", From: "sender-2222", FromAgent: "claude", Body: "synthetic retry body", Sent: now, ExpiresAt: now.Add(time.Hour)}
			if mode == "claim" {
				err = st.addClaimed(ctx, e)
			} else {
				err = st.reconcile(ctx, []busproto.Envelope{e}, nil, now)
			}
			if err != nil {
				st.db.Close()
				t.Fatal(err)
			}
			// A later server reconciliation updates the same row through upsertServer.
			if err = st.reconcile(ctx, []busproto.Envelope{e}, nil, now); err != nil {
				st.db.Close()
				t.Fatal(err)
			}
			if err = st.db.Close(); err != nil {
				t.Fatal(err)
			}
			st, err = openStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer st.db.Close()
			_, snapshot, err := st.take(ctx, e.ToSession, e.ToAgent, now, Limit{}, LeaseFor, 3, nil)
			if err != nil || len(snapshot) != 1 {
				t.Fatalf("take snapshot: %+v %v", snapshot, err)
			}
			got := snapshot[0]
			if got.RetryOf != e.RetryOf || got.ThreadID != e.ThreadID || got.ID != e.ID {
				t.Fatalf("retry metadata lost: %+v", got)
			}
			if _, err = st.confirm(ctx, e.ToSession, []string{e.ID}, now); err != nil {
				t.Fatal(err)
			}
			// This snapshot is what the agent passes to the delivered-output wrapper.
			if snapshot[0].RetryOf != e.RetryOf {
				t.Fatalf("confirmation changed retry snapshot: %+v", snapshot[0])
			}
		})
	}
}

func TestRetryMappingPurgePreservesSingleSuccessorContract(t *testing.T) {
	for _, expired := range []string{"source", "successor"} {
		t.Run(expired, func(t *testing.T) {
			lb := newLocalBus(t)
			source, err := lb.send(t, "aaaa1111", "bbbb", "synthetic retention retry")
			if err != nil {
				t.Fatal(err)
			}
			retryEnded(t, lb, source.ID)
			retry := func(r *busproto.SendRequest) { r.RetryOf = source.ID }
			successor, err := lb.send(t, "aaaa1111", "bbbb", "synthetic retention retry", retry)
			if err != nil {
				t.Fatal(err)
			}
			expiredID := source.ID
			if expired == "successor" {
				expiredID = successor.ID
			}
			if _, err = lb.st.db.Exec(`UPDATE devbus_messages SET expires_at=? WHERE id=?`, ms(lb.cfg.Now().Add(-localKeep-time.Hour)), expiredID); err != nil {
				t.Fatal(err)
			}
			if err = lb.st.purge(ctx, lb.cfg.Now()); err != nil {
				t.Fatal(err)
			}
			var mappings int
			if err = lb.st.db.QueryRow(`SELECT count(*) FROM devbus_retries WHERE source_id=?`, source.ID).Scan(&mappings); err != nil {
				t.Fatal(err)
			}
			want := 0
			if expired == "successor" {
				want = 1
			}
			if mappings != want {
				t.Fatalf("%s expired: got %d mappings, want %d", expired, mappings, want)
			}
			if _, err = lb.send(t, "aaaa1111", "bbbb", "synthetic retention retry", retry); code(err) != busproto.CodeNotFound {
				t.Fatalf("retry after %s expiry: %v", expired, err)
			}
			if err = lb.st.db.QueryRow(`SELECT count(*) FROM devbus_retries WHERE source_id=?`, source.ID).Scan(&mappings); err != nil || mappings != want {
				t.Fatalf("expiry retry changed mapping: %d %v", mappings, err)
			}
		})
	}
}

func TestExplicitRetryDoesNotExcludeUnrelatedDuplicate(t *testing.T) {
	lb := newLocalBus(t)
	source, err := lb.send(t, "aaaa1111", "bbbb", "synthetic unrelated duplicate")
	if err != nil {
		t.Fatal(err)
	}
	retryEnded(t, lb, source.ID)
	if _, err = lb.st.db.Exec(`INSERT INTO devbus_messages(id,origin,seq,to_session,to_agent,from_session,from_agent,thread_id,to_key,body_sha,envelope,state,created_at,expires_at)
 SELECT 'unrelated-message',origin,seq,to_session,to_agent,from_session,from_agent,thread_id,to_key,body_sha,envelope,'delivered',created_at,expires_at FROM devbus_messages WHERE id=?`, source.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = lb.send(t, "aaaa1111", "bbbb", "synthetic unrelated duplicate", func(r *busproto.SendRequest) { r.RetryOf = source.ID }); code(err) != busproto.CodeDuplicate {
		t.Fatalf("retry bypassed unrelated duplicate: %v", err)
	}
	var mappings int
	if err = lb.st.db.QueryRow(`SELECT count(*) FROM devbus_retries`).Scan(&mappings); err != nil || mappings != 0 {
		t.Fatalf("duplicate consumed retry: %d %v", mappings, err)
	}
}
