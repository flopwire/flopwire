package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/domain"
	"github.com/go-chi/chi/v5"
)

// Bus serves the message bus (notes/message-bus/plan.md); internal/bus
// implements it over Postgres. Writes (send, claim, ack, accept, revoke)
// audit themselves in their transaction; a failure the caller should see
// is a *busproto.Error.
type Bus interface {
	Send(ctx context.Context, c busproto.Caller, req busproto.SendRequest) (busproto.SendResponse, error)
	// Poll records the device's presence and holds until it has a message
	// newer than the cursor or the wait ends.
	Poll(ctx context.Context, c busproto.Caller, req busproto.PollRequest) (busproto.PollResponse, error)
	Claim(ctx context.Context, c busproto.Caller, req busproto.ClaimRequest) (busproto.ClaimResponse, error)
	Ack(ctx context.Context, c busproto.Caller, req busproto.AckRequest) (busproto.AckResponse, error)
	Peers(ctx context.Context, c busproto.Caller, q busproto.PeersQuery) (busproto.PeersResponse, error)
	Inbox(ctx context.Context, c busproto.Caller, q busproto.InboxQuery) (busproto.InboxResponse, error)
	Accepts(ctx context.Context, userID string) (busproto.AcceptsResponse, error)
	Accept(ctx context.Context, c busproto.Caller, sender string) (busproto.AcceptResponse, error)
	Revoke(ctx context.Context, c busproto.Caller, sender string) (busproto.AcceptResponse, error)
}

func (a *API) busRoutes(r chi.Router) {
	r.Post(busproto.PathSend, a.busDevice(a.busSend))
	r.Post(busproto.PathPoll, a.busDevice(a.busPoll))
	r.Post(busproto.PathClaim, a.busDevice(a.busClaim))
	r.Post(busproto.PathAck, a.busDevice(a.busAck))
	r.Get(busproto.PathPeers, a.busDevice(a.busPeers))
	r.Get(busproto.PathInbox, a.busDevice(a.busInbox))
	// Accepting a sender is the person's act (B7, plan §4 "Permission"):
	// a login session only, never a device token an agent could use.
	r.Get(busproto.PathAccepts, a.busLogin(a.busAccepts))
	r.Post(busproto.PathAccepts, a.busLogin(a.busAccept))
	r.Delete(busproto.PathAccepts+"/{user}", a.busLogin(a.busRevoke))
}

// busDevice admits a person's enrolled device credential: the device whose
// sessions send and receive. Not a login session, a minted token (a
// sandbox) or a service identity.
func (a *API) busDevice(next http.HandlerFunc) http.HandlerFunc {
	return a.busOnly(func(p principal) bool {
		return p.Credential.Kind == domain.CredentialDevice && p.Credential.DeviceID != "" && p.User.IdentityType == domain.IdentityHuman
	}, busproto.CodeDeviceRequired, "the message bus needs an enrolled device credential", next)
}

// busLogin admits a person's login session.
func (a *API) busLogin(next http.HandlerFunc) http.HandlerFunc {
	return a.busOnly(func(p principal) bool {
		return p.Credential.Kind == domain.CredentialSession && p.User.IdentityType == domain.IdentityHuman
	}, busproto.CodeLoginRequired, "accepting or revoking a sender needs your login session (the web console or `flopwire login`), not a device token", next)
}

func (a *API) busOnly(ok func(principal) bool, code, detail string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := mustPrincipal(r)
		if !ok(p) {
			if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "authorization.failed", "request", r.URL.Path, map[string]any{"reason": code}) {
				return
			}
			problemCode(w, http.StatusForbidden, code, detail)
			return
		}
		if a.bus == nil {
			problem(w, http.StatusNotImplemented, "this server has no message bus")
			return
		}
		next(w, r)
	}
}

func (a *API) caller(r *http.Request) busproto.Caller {
	p := mustPrincipal(r)
	return busproto.Caller{UserID: p.User.ID, DeviceID: p.Credential.DeviceID, ClientIP: a.clientIP(r)}
}

// busFailed answers a bus error. A refusal of the caller's session (403)
// is audited as an authorization failure first.
func (a *API) busFailed(w http.ResponseWriter, r *http.Request, err error) {
	var be *busproto.Error
	switch {
	case errors.As(err, &be):
		if be.Status == http.StatusForbidden {
			p := mustPrincipal(r)
			if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "authorization.failed", "request", r.URL.Path, map[string]any{"reason": be.Code}) {
				return
			}
		}
		if be.Status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "600")
		}
		out := map[string]any{"type": "about:blank", "title": http.StatusText(be.Status), "status": be.Status, "code": be.Code, "detail": be.Detail}
		if be.MessageID != "" {
			out["message_id"] = be.MessageID
		}
		if len(be.Candidates) > 0 {
			out["candidates"] = be.Candidates
		}
		writeJSON(w, be.Status, out)
	case errors.Is(err, context.Canceled) && r.Context().Err() != nil:
		// The device gave up on the request; nobody reads an answer.
	default:
		a.log.Error("message bus", "path", r.URL.Path, "err", err)
		problem(w, http.StatusInternalServerError, "message bus request failed")
	}
}

func (a *API) busSend(w http.ResponseWriter, r *http.Request) {
	var in busproto.SendRequest
	if !decode(w, r, &in) {
		return
	}
	out, err := a.bus.Send(r.Context(), a.caller(r), in)
	if err != nil {
		a.busFailed(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// busPoll is the long poll. An answer with messages is audited with their
// ids (bus.poll); an empty one is not, since every device polls about
// twice a minute.
func (a *API) busPoll(w http.ResponseWriter, r *http.Request) {
	var in busproto.PollRequest
	if !decode(w, r, &in) {
		return
	}
	c := a.caller(r)
	out, err := a.bus.Poll(r.Context(), c, in)
	if err != nil {
		a.busFailed(w, r, err)
		return
	}
	if n := len(out.Messages) + len(out.Claimable); n > 0 {
		ids := make([]string, 0, n)
		for _, m := range out.Messages {
			ids = append(ids, m.ID)
		}
		for _, m := range out.Claimable {
			ids = append(ids, m.Message.ID)
		}
		if !a.auditOK(w, r, c.UserID, c.DeviceID, "bus.poll", "bus_message", "", map[string]any{"result_ids": ids, "cursor": out.Cursor}) {
			return
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) busClaim(w http.ResponseWriter, r *http.Request) {
	var in busproto.ClaimRequest
	if !decode(w, r, &in) {
		return
	}
	out, err := a.bus.Claim(r.Context(), a.caller(r), in)
	if err != nil {
		a.busFailed(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) busAck(w http.ResponseWriter, r *http.Request) {
	var in busproto.AckRequest
	if !decode(w, r, &in) {
		return
	}
	out, err := a.bus.Ack(r.Context(), a.caller(r), in)
	if err != nil {
		a.busFailed(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// busPeers lists live sessions; the audit keeps the filters and the
// sessions listed, as sessions.list does.
func (a *API) busPeers(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	q := busproto.PeersQuery{Session: v.Get("session"), Repo: v.Get("repo"), User: v.Get("user"), Agent: v.Get("agent")}
	c := a.caller(r)
	out, err := a.bus.Peers(r.Context(), c, q)
	if err != nil {
		a.busFailed(w, r, err)
		return
	}
	ids := make([]string, len(out.Peers))
	for i, p := range out.Peers {
		ids[i] = p.Session
	}
	if !a.auditOK(w, r, c.UserID, c.DeviceID, "bus.peers", "session", q.Session,
		map[string]any{"repo": q.Repo, "user": q.User, "agent": q.Agent, "result_ids": ids}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// busInbox lists a session's messages; the audit keeps the ids returned.
func (a *API) busInbox(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	q := busproto.InboxQuery{Session: v.Get("session"), Agent: v.Get("agent"), Thread: v.Get("thread"), Before: v.Get("before")}
	if s := v.Get("sent"); s != "" {
		sent, err := strconv.ParseBool(s)
		if err != nil {
			problem(w, 400, "sent: true or false")
			return
		}
		q.SentOnly = sent
	}
	if s := v.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			problem(w, 400, "limit: a positive number")
			return
		}
		q.Limit = n
	}
	c := a.caller(r)
	out, err := a.bus.Inbox(r.Context(), c, q)
	if err != nil {
		a.busFailed(w, r, err)
		return
	}
	ids := make([]string, len(out.Messages))
	for i, m := range out.Messages {
		ids[i] = m.ID
	}
	if !a.auditOK(w, r, c.UserID, c.DeviceID, "bus.inbox", "session", q.Session,
		map[string]any{"sent_only": q.SentOnly, "thread": q.Thread, "result_ids": ids}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) busAccepts(w http.ResponseWriter, r *http.Request) {
	out, err := a.bus.Accepts(r.Context(), mustPrincipal(r).User.ID)
	if err != nil {
		a.busFailed(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) busAccept(w http.ResponseWriter, r *http.Request) {
	var in busproto.AcceptRequest
	if !decode(w, r, &in) {
		return
	}
	out, err := a.bus.Accept(r.Context(), a.caller(r), in.Sender)
	if err != nil {
		a.busFailed(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) busRevoke(w http.ResponseWriter, r *http.Request) {
	out, err := a.bus.Revoke(r.Context(), a.caller(r), chi.URLParam(r, "user"))
	if err != nil {
		a.busFailed(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
