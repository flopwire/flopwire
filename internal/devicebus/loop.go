package devicebus

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/bus"
	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/redact"
	"github.com/flopwire/flopwire/internal/syncproto"
)

// presenceCache keeps the last presence briefly: a send, a peers call and
// the loop's check may all ask within a second. Guarded by Bus.mu.
type presenceCache struct {
	at  time.Time
	all []Session
}

const presenceFresh = time.Second

// sessions is the device's live sessions, at most presenceFresh old.
func (b *Bus) sessions(ctx context.Context) ([]Session, error) {
	b.mu.Lock()
	fn := b.cfg.Presence
	if c := b.presence; !c.at.IsZero() && b.cfg.Now().Sub(c.at) < presenceFresh {
		b.mu.Unlock()
		return c.all, nil
	}
	b.mu.Unlock()
	if fn == nil {
		return nil, nil
	}
	all, err := fn(ctx)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.presence = presenceCache{at: b.cfg.Now(), all: all}
	b.mu.Unlock()
	return all, nil
}

// serverPresence is what a poll reports: the live sessions the path rules
// let reach the server, the most recently active first when there are
// more than the server takes, in a stable order.
func serverPresence(all []Session) []busproto.PresenceSession {
	var keep []Session
	for _, s := range all {
		if !s.Withheld {
			keep = append(keep, s)
		}
	}
	if len(keep) > busproto.MaxPresence {
		slices.SortStableFunc(keep, func(a, b Session) int { return b.LastActive.Compare(a.LastActive) })
		keep = keep[:busproto.MaxPresence]
	}
	out := make([]busproto.PresenceSession, len(keep))
	for i, s := range keep {
		out[i] = s.PresenceSession
		// The title comes from the local index, often the first prompt,
		// unredacted; the server shows it to every member in peers.
		if masked, ms := redact.Redact([]byte(s.Title)); len(ms) > 0 {
			out[i].Title = string(masked)
		}
	}
	slices.SortFunc(out, func(a, b busproto.PresenceSession) int {
		if c := strings.Compare(a.Agent, b.Agent); c != 0 {
			return c
		}
		return strings.Compare(a.SessionID, b.SessionID)
	})
	return out
}

func jitter(d time.Duration) time.Duration { return d/2 + rand.N(d/2+1) }

// pollAnswer is one poll's outcome.
type pollAnswer struct {
	resp busproto.PollResponse
	err  error
	key  string
}

// halt is why polling stopped: a refused credential or pin (stopped), or
// a server without the bus (disabled). It ends when the saved credential
// changes, or, for disabled, when DisabledEvery passed.
type halt struct {
	state, reason string
	key           string
	until         time.Time // disabled: ask again then
}

// runServer holds the poll until ctx ends.
func (b *Bus) runServer(ctx context.Context) {
	answers := make(chan pollAnswer, 1)
	tick := time.NewTicker(b.cfg.PresenceEvery)
	defer tick.Stop()
	purge := time.NewTicker(time.Minute)
	defer purge.Stop()
	var (
		inflight bool
		cancel   context.CancelFunc = func() {}
		sent     []busproto.PresenceSession
		started  time.Time
		cursor   int64
		gen      int64 // the person's generation in the last answer
		backoff  time.Duration
		next     time.Time // no poll before this
		stop     *halt
		skip     = map[string]bool{} // claimable ids not to claim again
	)
	b.kickAcks() // receipts owed from before a restart
	for {
		now := b.cfg.Now()
		if stop != nil && !now.Before(next) {
			// Halted: has the saved credential changed, or is the disabled
			// wait over?
			_, key := b.cfg.Connect()
			if key != stop.key || stop.state == StateDisabled && !now.Before(stop.until) {
				b.log.Info("devicebus: resuming", "was", stop.state)
				stop, backoff = nil, 0
				b.setStatus(func(s *Status) { s.State, s.LastError = StateConnecting, "" })
			} else {
				next = now.Add(b.cfg.RepinEvery)
			}
		}
		if !inflight && stop == nil && !now.Before(next) {
			srv, key := b.cfg.Connect()
			// A presence that cannot be read keeps the last one: an empty
			// report would end every session at the server.
			if all, err := b.sessions(ctx); err != nil {
				b.log.Warn("devicebus: presence", "err", err)
			} else {
				cur := serverPresence(all)
				if !slices.Equal(cur, sent) {
					// Changed since the last poll (between polls, or during
					// a backoff): reset the cursor, as the in-flight check
					// below does.
					cursor = 0
				}
				sent = cur
			}
			pctx, pcancel := context.WithCancel(ctx)
			cancel = pcancel
			inflight, started = true, now
			req := busproto.PollRequest{Sessions: sent, Cursor: cursor, Gen: gen, WaitSeconds: int(b.cfg.PollWait / time.Second)}
			b.setStatus(func(s *Status) { s.Sessions = len(sent) })
			go func() {
				resp, err := srv.Poll(pctx, req)
				answers <- pollAnswer{resp: resp, err: err, key: key}
				pcancel()
			}()
		}
		var wait <-chan time.Time
		if !inflight {
			wait = time.After(max(next.Sub(b.cfg.Now()), time.Millisecond))
		}
		select {
		case <-ctx.Done():
			if inflight {
				cancel()
				<-answers
			}
			return
		case <-purge.C:
			if err := b.st.purge(ctx, b.cfg.Now()); err != nil && ctx.Err() == nil {
				b.log.Warn("devicebus: purge", "err", err)
			}
		case <-b.recheck:
			if stop != nil {
				next = b.cfg.Now()
			}
		case <-wait:
		case <-tick.C:
			b.expireLeases(ctx)
			// A session started, ended, or turned busy or idle: the server
			// records presence when a poll starts, so start another. Its
			// cursor is reset: a message already older than the cursor
			// that the new presence makes deliverable (a session newly
			// reported) would otherwise wait for the poll to time out.
			if inflight && b.cfg.Now().Sub(started) >= time.Second {
				all, err := b.sessions(ctx)
				if err == nil && !slices.Equal(serverPresence(all), sent) {
					cancel()
					cursor = 0
				}
			}
		case a := <-answers:
			inflight = false
			cancel()
			if ctx.Err() != nil {
				return
			}
			if errors.Is(a.err, context.Canceled) {
				next = started.Add(250 * time.Millisecond)
				continue
			}
			if a.err != nil {
				stop, backoff, next = b.pollFailed(ctx, a, backoff)
				if stop != nil {
					cursor = 0
				}
				continue
			}
			backoff = 0
			cursor, gen = a.resp.Cursor, a.resp.Gen
			if err := b.answered(ctx, a.resp, skip); err != nil {
				// A claim could not be made (or the inbox not written):
				// back off, then ask for the whole set again.
				backoff = min(max(2*backoff, b.cfg.BackoffMin), b.cfg.BackoffMax)
				d := jitter(backoff)
				next, cursor = b.cfg.Now().Add(d), 0
				b.log.Warn("devicebus: poll answer", "err", err, "retry_in", d)
				b.setStatus(func(s *Status) { s.State, s.LastError, s.RetryAt = StateBackoff, err.Error(), next })
				continue
			}
			// The next poll holds at the server; the gap only guards
			// against a server that answers at once every time.
			next = started.Add(250 * time.Millisecond)
		}
	}
}

// pollFailed classifies a failed poll: a refused credential or pin stops
// polling until the saved one changes (as sync does), a server without the
// bus disables it for a while, anything else backs off.
func (b *Bus) pollFailed(ctx context.Context, a pollAnswer, backoff time.Duration) (*halt, time.Duration, time.Time) {
	now := b.cfg.Now()
	var be *busproto.Error
	var ae *client.APIError
	var h *halt
	switch {
	case errors.As(a.err, &ae) && ae.StatusCode == http.StatusUnauthorized:
		if _, key := b.cfg.Connect(); key != a.key {
			return nil, 0, now // the token was rotated meanwhile: retry with it
		}
		h = &halt{state: StateStopped, reason: "the server refused this device's credential: run flopwire login"}
	case syncproto.Permanent(a.err):
		h = &halt{state: StateStopped, reason: a.err.Error()}
	case errors.As(a.err, &ae) && (ae.StatusCode == http.StatusNotImplemented || ae.StatusCode == http.StatusNotFound):
		h = &halt{state: StateDisabled, reason: "the server has no message bus", until: now.Add(b.cfg.DisabledEvery)}
	case errors.As(a.err, &be) && be.Code == busproto.CodeDeviceRequired:
		h = &halt{state: StateDisabled, reason: be.Detail, until: now.Add(b.cfg.DisabledEvery)}
	}
	if h != nil {
		h.key = a.key
		b.log.Error("devicebus: messaging "+h.state, "reason", h.reason)
		b.setStatus(func(s *Status) { s.State, s.LastError, s.RetryAt = h.state, h.reason, time.Time{} })
		return h, 0, now.Add(b.cfg.RepinEvery)
	}
	backoff = min(max(2*backoff, b.cfg.BackoffMin), b.cfg.BackoffMax)
	if errors.As(a.err, &be) && be.Status == http.StatusBadRequest {
		backoff = b.cfg.BackoffMax // our request is wrong; retrying soon will not fix it
	}
	d := jitter(backoff)
	next := now.Add(d)
	b.log.Warn("devicebus: poll failed", "err", a.err, "retry_in", d)
	b.setStatus(func(s *Status) { s.State, s.LastError, s.RetryAt = StateBackoff, a.err.Error(), next })
	return nil, backoff, next
}

// answered folds a poll answer into the inbox: reconcile against the whole
// set, then claim what is offered.
func (b *Bus) answered(ctx context.Context, resp busproto.PollResponse, skip map[string]bool) error {
	now := b.cfg.Now()
	b.mu.Lock()
	b.held = resp.Held
	b.status.State, b.status.LastError, b.status.RetryAt, b.status.LastPoll = StateConnected, "", time.Time{}, now
	b.mu.Unlock()
	if len(resp.Ignored) > 0 {
		b.log.Warn("devicebus: the server ignored sessions it holds as another person's", "sessions", resp.Ignored)
	}
	offered := make([]string, len(resp.Claimable))
	for i, c := range resp.Claimable {
		offered[i] = c.Message.ID
	}
	if err := b.st.reconcile(ctx, resp.Messages, offered, now); err != nil {
		return err
	}
	b.kickAcks() // a message listed again owes its receipt again
	for id := range skip {
		if !slices.Contains(offered, id) {
			delete(skip, id)
		}
	}
	all, _ := b.sessions(ctx)
	for _, c := range resp.Claimable {
		if skip[c.Message.ID] {
			continue
		}
		if ok, err := b.st.has(ctx, c.Message.ID); err != nil {
			return err
		} else if ok {
			continue
		}
		if err := b.claim(ctx, c, all, skip); err != nil {
			return err
		}
	}
	return nil
}

// claimOrder ranks the sessions a claimable message may go to: one on the
// repo it is routed by first (the server's rule: the repo, then
// anywhere), then busy before idle (it arrives at once), then the most
// recently active, then the server's order.
func claimOrder(c busproto.Claimable, all []Session) []string {
	byID := map[string]Session{}
	for _, s := range all {
		byID[s.SessionID] = s
	}
	out := slices.Clone(c.Sessions)
	rank := func(id string) (onRepo, busy bool, at time.Time) {
		s, ok := byID[id]
		if !ok {
			return false, false, time.Time{}
		}
		return c.Message.ToRepo != "" && bus.RepoName(s.Repo) == c.Message.ToRepo, s.Busy, s.LastActive
	}
	slices.SortStableFunc(out, func(x, y string) int {
		xr, xb, xt := rank(x)
		yr, yb, yt := rank(y)
		switch {
		case xr != yr:
			if xr {
				return -1
			}
			return 1
		case xb != yb:
			if xb {
				return -1
			}
			return 1
		}
		return yt.Compare(xt)
	})
	return out
}

// claim takes one @user message for one of the device's sessions. Lost to
// another device, or no longer eligible: it is dropped (skip). A session
// the server no longer holds as live: the next one is tried.
func (b *Bus) claim(ctx context.Context, c busproto.Claimable, all []Session, skip map[string]bool) error {
	srv, _ := b.cfg.Connect()
	agents := map[string]string{}
	for _, s := range all {
		agents[s.SessionID] = s.Agent
	}
	for _, id := range claimOrder(c, all) {
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		resp, err := srv.Claim(cctx, busproto.ClaimRequest{MessageID: c.Message.ID, SessionID: id, Agent: agents[id]})
		cancel()
		var be *busproto.Error
		switch {
		case err == nil:
			b.log.Debug("devicebus: claimed", "id", c.Message.ID, "session", id)
			return b.st.addClaimed(ctx, resp.Message)
		case errors.As(err, &be) && (be.Code == busproto.CodeAlreadyClaimed || be.Code == busproto.CodeNotEligible || be.Code == busproto.CodeNotFound):
			skip[c.Message.ID] = true
			return nil
		case errors.As(err, &be) && be.Code == busproto.CodeSessionNotOnDevice:
			continue
		default:
			return err
		}
	}
	return nil // no session took it; the next poll offers it again
}

func (b *Bus) kickAcks() {
	select {
	case b.ackWake <- struct{}{}:
	default:
	}
}

// runAcks sends delivery receipts and undelivered reports in batches of
// up to busproto.MaxAck ids, retrying with backoff. A rejected id is not deliverable by this device;
// it is not sent again.
func (b *Bus) runAcks(ctx context.Context) {
	var backoff time.Duration
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.ackWake:
		}
		// Let the hooks of one burst add their deliveries to the batch.
		select {
		case <-ctx.Done():
			return
		case <-time.After(b.cfg.AckDelay):
		}
		for {
			ids, err := b.st.owed(ctx, "owed", busproto.MaxAck)
			var gone []string
			if err == nil && len(ids) < busproto.MaxAck {
				gone, err = b.st.owed(ctx, "report", busproto.MaxAck-len(ids))
			}
			if err != nil || len(ids)+len(gone) == 0 {
				if err != nil && ctx.Err() == nil {
					b.log.Warn("devicebus: receipts", "err", err)
				}
				break
			}
			srv, _ := b.cfg.Connect()
			actx, cancel := context.WithTimeout(ctx, 30*time.Second)
			resp, err := srv.Ack(actx, busproto.AckRequest{IDs: ids, Undelivered: gone})
			cancel()
			if err == nil {
				err = b.st.acked(ctx, resp.Acked, resp.Rejected)
			}
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				backoff = min(max(2*backoff, b.cfg.BackoffMin), b.cfg.BackoffMax)
				d := jitter(backoff)
				b.log.Warn("devicebus: receipts failed", "err", err, "retry_in", d)
				select {
				case <-ctx.Done():
					return
				case <-time.After(d):
				}
				continue
			}
			backoff = 0
			if len(resp.Rejected) > 0 {
				b.log.Info("devicebus: receipts rejected (delivered elsewhere, held again or expired)", "ids", resp.Rejected)
			}
		}
	}
}
