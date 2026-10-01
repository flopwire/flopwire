package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/flopwire/flopwire/internal/busproto"
)

// A bus refusal decodes into *busproto.Error with its candidates; a 401 or
// an answer without a bus code (a server with no bus, a proxy page) is an
// *APIError; queries carry their filters.
func TestBusErrors(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case busproto.PathSend:
			w.WriteHeader(409)
			w.Write([]byte(`{"type":"about:blank","status":409,"code":"ambiguous_recipient","detail":"0b7e matches 2 sessions","candidates":[{"session":"0b7e1","user":"a@x"},{"session":"0b7e2","user":"b@x"}]}`))
		case busproto.PathPoll:
			w.WriteHeader(401)
			w.Write([]byte(`{"type":"about:blank","status":401,"code":"credential_invalid","detail":"no"}`))
		case busproto.PathAck:
			w.WriteHeader(501)
			w.Write([]byte(`{"type":"about:blank","status":501,"detail":"this server has no message bus"}`))
		case busproto.PathPeers:
			gotQuery = r.URL.RawQuery
			w.Write([]byte(`{"peers":[{"session":"s1","agent":"codex","user":"a@x","user_id":"u","busy":true,"own":true,"seen_at":"2026-10-01T00:00:00Z"}]}`))
		}
	}))
	defer srv.Close()
	b := Bus{Server: srv.URL, Token: "tok", HTTP: srv.Client()}
	ctx := context.Background()
	_, err := b.Send(ctx, busproto.SendRequest{FromSession: "s", To: "0b7e", Body: "x"})
	var be *busproto.Error
	if !errors.As(err, &be) || be.Code != busproto.CodeAmbiguousRecipient || len(be.Candidates) != 2 || be.Status != 409 {
		t.Fatalf("send: %v", err)
	}
	var ae *APIError
	if _, err := b.Poll(ctx, busproto.PollRequest{}); !errors.As(err, &ae) || ae.StatusCode != 401 || errors.As(err, &be) {
		t.Fatalf("poll 401: %v", err)
	}
	if _, err := b.Ack(ctx, busproto.AckRequest{IDs: []string{"m1"}}); !errors.As(err, &ae) || ae.StatusCode != 501 {
		t.Fatalf("ack 501: %v", err)
	}
	out, err := b.Peers(ctx, busproto.PeersQuery{Session: "s0", Repo: "api"})
	if err != nil || len(out.Peers) != 1 || !out.Peers[0].Busy || gotQuery != "repo=api&session=s0" {
		t.Fatalf("peers: %+v %v %q", out, err, gotQuery)
	}
}
