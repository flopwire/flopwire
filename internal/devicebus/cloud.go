package devicebus

// Vendor cloud sessions (plan §6, B6; issue #63). The device lists its
// person's cloud sessions with each vendor adapter (internal/vendorcloud)
// every CloudEvery and reports them to the server as the person's
// (busproto.PollRequest.Cloud); without a server they are addressable on
// the device. A message to one reaches the local inbox like any other:
// with a server the device claims it first, and only while the session
// runs a turn, so that one of the person's devices pushes it.
//
// The pusher pushes a session's queued messages only while the vendor
// reports a turn running, so a push never starts a turn (B3). It leases
// them as a hook does (Take), renders them after the cloud instruction
// (busrender.CloudContext: the session gets them as its owner's input, so
// the framing and the authority rule travel in the text), pushes, and
// confirms on the vendor's acknowledgement. A failed push ends the lease
// at once: the next presence tick retries it, marked a redelivery, and
// after MaxAttempts failures the message is undelivered with reason
// push_failed. A push the vendor refuses because the session is archived
// or exited makes the session's waiting messages undelivered with reason
// session_ended. Read receipts come from the vendor's record where it has
// one (vendorcloud.Pushed.ReadAt, vendorcloud.Reader).

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/bus"
	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/busrender"
	"github.com/flopwire/flopwire/internal/vendorcloud"
)

// cloudList is one adapter's last listing.
type cloudList struct {
	sessions []Session
	at       time.Time // when it was listed
	failure  string    // the last listing's error, logged once until it changes
}

// cloudKeep is how long a listing stays in use after its adapter last
// listed successfully: a vendor that fails once does not end its sessions
// at the server, one that keeps failing does.
func (b *Bus) cloudKeep() time.Duration { return 3 * b.cfg.CloudEvery }

// cloudLease is how long one push may hold its messages: well under the
// two leases past which expireLeases takes a lease for one handed out
// before the clock stepped back (another goroutine's clock read may come
// just before the lease's).
func (b *Bus) cloudLease() time.Duration { return 3 * b.cfg.Lease / 2 }

// readWindow is how long after a delivery the pusher looks for the
// message in its session's record (vendorcloud.Reader).
const readWindow = time.Hour

// pendingRead is a delivered cloud message not seen read yet.
type pendingRead struct {
	session, agent string
	delivered      time.Time
}

// CloudSessions returns the person's cloud sessions from the adapters'
// last listings, oldest listing dropped (cloudKeep).
func (b *Bus) CloudSessions() []Session {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.cfg.Now()
	var out []Session
	for _, a := range b.cfg.Cloud {
		l, ok := b.cloud[a.Agent()]
		if !ok || now.Sub(l.at) > b.cloudKeep() {
			continue
		}
		out = append(out, l.sessions...)
	}
	return out
}

// cloudSession is the cloud session (id, agent) from the last listings.
func (b *Bus) cloudSession(id, agent string) (Session, bool) {
	for _, s := range b.CloudSessions() {
		if s.SessionID == id && (agent == "" || s.Agent == agent) {
			return s, true
		}
	}
	return Session{}, false
}

// cloudPresence is what a poll reports of the cloud sessions, in a
// stable order.
func cloudPresence(all []Session) []busproto.PresenceSession {
	out := make([]busproto.PresenceSession, 0, len(all))
	for _, s := range all {
		out = append(out, s.PresenceSession)
	}
	slices.SortFunc(out, func(a, b busproto.PresenceSession) int {
		if c := strings.Compare(a.Agent, b.Agent); c != 0 {
			return c
		}
		return strings.Compare(a.SessionID, b.SessionID)
	})
	if len(out) > busproto.MaxPresence {
		out = out[:busproto.MaxPresence]
	}
	return out
}

func (b *Bus) kickPush() {
	select {
	case b.pushWake <- struct{}{}:
	default:
	}
}

// listCloud lists each adapter's sessions once. A session on a repo the
// path rules withhold (by name) is left out: nothing about it may reach
// the server.
func (b *Bus) listCloud(ctx context.Context) {
	b.mu.Lock()
	withheld := b.cfg.RepoWithheld
	b.mu.Unlock()
	for _, a := range b.cfg.Cloud {
		lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		got, err := a.List(lctx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// A vendor CLI without a login fails every listing: say so
			// once, not every CloudEvery.
			b.mu.Lock()
			l := b.cloud[a.Agent()]
			repeat := l.failure == err.Error()
			l.failure = err.Error()
			b.cloud[a.Agent()] = l
			b.mu.Unlock()
			if !repeat {
				b.log.Warn("devicebus: cloud sessions cannot be listed", "agent", a.Agent(), "err", err)
			}
			continue
		}
		var keep []Session
		for _, s := range got {
			if s.Agent != a.Agent() || s.ID == "" {
				continue
			}
			if s.Repo != "" && withheld != nil {
				// By name: "owner/name" is no path on this device.
				if w, err := withheld(ctx, bus.RepoName(s.Repo)); err != nil || w {
					continue
				}
			}
			keep = append(keep, Session{PresenceSession: busproto.PresenceSession{SessionID: s.ID, Agent: s.Agent, Repo: s.Repo,
				Branch: s.Branch, Title: s.Title, Busy: s.Running}, Cloud: true})
		}
		b.mu.Lock()
		b.cloud[a.Agent()] = cloudList{sessions: keep, at: b.cfg.Now()}
		b.mu.Unlock()
	}
	b.kickPush()
}

// runCloud lists the cloud sessions every CloudEvery and pushes what is
// due on every presence tick, until ctx ends.
func (b *Bus) runCloud(ctx context.Context) {
	list := time.NewTimer(0)
	defer list.Stop()
	tick := time.NewTicker(b.cfg.PresenceEvery)
	defer tick.Stop()
	reads := map[string]pendingRead{}
	var readsAt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-list.C:
			b.listCloud(ctx)
			list.Reset(b.cfg.CloudEvery)
		case <-tick.C:
		case <-b.pushWake:
		}
		b.pushCloud(ctx, reads)
		if now := b.cfg.Now(); now.Sub(readsAt) >= b.cfg.CloudEvery {
			readsAt = now
			b.readCloud(ctx, reads)
		}
	}
}

// pushCloud pushes the queued messages of every cloud session that runs a
// turn now.
func (b *Bus) pushCloud(ctx context.Context, reads map[string]pendingRead) {
	adapters := map[string]vendorcloud.Adapter{}
	for _, a := range b.cfg.Cloud {
		adapters[a.Agent()] = a
	}
	for _, s := range b.CloudSessions() {
		a := adapters[s.Agent]
		if a == nil || !s.Busy {
			continue
		}
		if err := b.pushSession(ctx, a, s, reads); err != nil && ctx.Err() == nil {
			b.log.Warn("devicebus: cloud push", "agent", s.Agent, "session", s.SessionID, "err", err)
		}
	}
}

// pushSession pushes one batch of the session's queued messages.
func (b *Bus) pushSession(ctx context.Context, a vendorcloud.Adapter, s Session, reads map[string]pendingRead) error {
	ins := busrender.EncodedLen(busrender.CloudInstruction(s.Agent)) + busrender.SepLen
	lim := Limit{Count: busrender.HookMessages, Bytes: max(1, busrender.HookBytes-ins), Sep: busrender.SepLen, Size: busrender.Size}
	lease := b.cloudLease()
	_, msgs, err := b.st.take(ctx, s.SessionID, s.Agent, b.cfg.Now(), lim, lease, b.cfg.MaxAttempts, nil)
	if err != nil || len(msgs) == 0 {
		return err
	}
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	text := busrender.CloudContext(s.Agent, msgs, nil, busrender.HookBytes)
	pctx, cancel := context.WithTimeout(ctx, lease-lease/8)
	pushed, err := a.Push(pctx, s.SessionID, text)
	cancel()
	switch {
	case err == nil:
		if err := b.Confirm(ctx, s.SessionID, ids); err != nil {
			return err
		}
		now := b.cfg.Now()
		if !pushed.ReadAt.IsZero() {
			rs := make([]Read, len(ids))
			for i, id := range ids {
				rs[i] = Read{Session: s.SessionID, Agent: s.Agent, ID: id, At: pushed.ReadAt}
			}
			return b.MarkRead(ctx, rs)
		}
		if _, ok := a.(vendorcloud.Reader); ok {
			for _, id := range ids {
				reads[id] = pendingRead{session: s.SessionID, agent: s.Agent, delivered: now}
			}
		}
		return nil
	case errors.Is(err, vendorcloud.ErrGone):
		n, gerr := b.st.endCloud(ctx, s.SessionID, s.Agent)
		if gerr != nil {
			return gerr
		}
		b.dropCloud(s)
		b.endedMarked(n)
		return err
	default:
		n, ferr := b.st.pushFailed(ctx, ids, b.cfg.MaxAttempts)
		if ferr != nil {
			return ferr
		}
		if n > 0 {
			b.log.Warn("devicebus: messages undelivered: every push into their cloud session failed", "count", n, "attempts", b.cfg.MaxAttempts)
			if !b.Local() {
				b.kickAcks()
			}
		}
		return err
	}
}

// dropCloud leaves a session the vendor reported gone out of the
// listings until the next one.
func (b *Bus) dropCloud(s Session) {
	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.cloud[s.Agent]
	l.sessions = slices.DeleteFunc(slices.Clone(l.sessions), func(v Session) bool { return v.SessionID == s.SessionID })
	b.cloud[s.Agent] = l
}

// readCloud asks the adapters that keep a record (vendorcloud.Reader)
// whether delivered messages were read, and drops those past readWindow.
func (b *Bus) readCloud(ctx context.Context, reads map[string]pendingRead) {
	if len(reads) == 0 {
		return
	}
	now := b.cfg.Now()
	type key struct{ session, agent string }
	bySession := map[key][]string{}
	for id, r := range reads {
		if now.Sub(r.delivered) > readWindow {
			delete(reads, id)
			continue
		}
		k := key{r.session, r.agent}
		bySession[k] = append(bySession[k], id)
	}
	for _, a := range b.cfg.Cloud {
		reader, ok := a.(vendorcloud.Reader)
		if !ok {
			continue
		}
		for k, ids := range bySession {
			if k.agent != a.Agent() {
				continue
			}
			rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			seen, err := reader.Seen(rctx, k.session, ids)
			cancel()
			if err != nil {
				if ctx.Err() == nil {
					b.log.Warn("devicebus: cloud read receipts", "agent", k.agent, "session", k.session, "err", err)
				}
				continue
			}
			var rs []Read
			for id, at := range seen {
				rs = append(rs, Read{Session: k.session, Agent: k.agent, ID: id, At: at})
				delete(reads, id)
			}
			if err := b.MarkRead(ctx, rs); err != nil && ctx.Err() == nil {
				b.log.Warn("devicebus: cloud read receipts", "err", err)
			}
		}
	}
}

// pushFailed ends the leases of messages whose push failed: each is queued
// again for the next try, or, leased maxAttempts times, undelivered with
// reason push_failed (reported to the server). It returns how many became
// undelivered.
func (s *store) pushFailed(ctx context.Context, ids []string, maxAttempts int) (int64, error) {
	var gone int64
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		for _, id := range ids {
			res, err := tx.ExecContext(ctx, `UPDATE devbus_messages SET state='undelivered', reason=?, lease_until=NULL,
					ack=CASE WHEN origin='server' THEN 'report' ELSE '' END
				WHERE id=? AND state='leased' AND attempts>=?`, busproto.ReasonPushFailed, id, maxAttempts)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			gone += n
			if _, err := tx.ExecContext(ctx, `UPDATE devbus_messages SET state='queued', lease_until=NULL WHERE id=? AND state='leased'`, id); err != nil {
				return err
			}
		}
		return nil
	})
	return gone, err
}

// endCloud marks undelivered (session_ended) the waiting messages of a
// cloud session its vendor reported archived or exited, and queues the
// reports of those from the server. It returns how many it marked.
func (s *store) endCloud(ctx context.Context, session, agent string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE devbus_messages SET state='undelivered', reason=?, lease_until=NULL,
			ack=CASE WHEN origin='server' THEN 'report' ELSE '' END
		WHERE to_session=? AND to_agent=? AND state IN ('queued','leased')`, busproto.ReasonSessionEnded, session, agent)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
