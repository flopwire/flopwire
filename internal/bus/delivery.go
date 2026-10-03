package bus

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/jackc/pgx/v5"
)

// envCols and envFrom select a message as an Envelope (scanEnvelope). The
// two emails are scalar lookups by primary key: a join lets the planner
// scan the users table.
const (
	envCols = `m.id,m.seq,m.thread_id,COALESCE(m.reply_to,''),m.from_session,m.from_agent,m.from_user::text,(SELECT email FROM users WHERE id=m.from_user),
		m.from_repo,m.from_branch,m.sender,m.intent,m.body,m.refs,m.created_at,m.expires_at,COALESCE(m.to_session,''),COALESCE(m.to_agent,''),
		m.to_user::text,(SELECT email FROM users WHERE id=m.to_user),m.to_repo,m.addressed`
	envFrom = `bus_messages m`
)

func scanEnvelope(r pgx.Row, extra ...any) (busproto.Envelope, error) {
	var e busproto.Envelope
	var intent string
	err := r.Scan(append([]any{&e.ID, &e.Seq, &e.ThreadID, &e.ReplyTo, &e.From, &e.FromAgent, &e.UserID, &e.User, &e.Repo, &e.Branch,
		&e.Sender, &intent, &e.Body, &e.Refs, &e.Sent, &e.ExpiresAt, &e.ToSession, &e.ToAgent, &e.ToUserID, &e.ToUser, &e.ToRepo, &e.Addressed}, extra...)...)
	e.Intent = busproto.Intent(intent)
	if len(e.Refs) == 0 {
		e.Refs = nil
	}
	return e, err
}

func validPresence(in []busproto.PresenceSession) ([]busproto.PresenceSession, error) {
	if len(in) > busproto.MaxPresence {
		return nil, badRequest("at most %d sessions per poll", busproto.MaxPresence)
	}
	seen := map[[2]string]int{}
	var out []busproto.PresenceSession
	for _, p := range in {
		p.SessionID, p.Agent = strings.TrimSpace(p.SessionID), strings.TrimSpace(p.Agent)
		if p.SessionID == "" || p.Agent == "" || len(p.SessionID) > 256 || len(p.Agent) > 64 ||
			len(p.Repo) > 4096 || len(p.Branch) > 1024 || len(p.Title) > 1024 {
			return nil, badRequest("each session needs session_id and agent (and bounded repo, branch and title)")
		}
		k := [2]string{p.Agent, p.SessionID}
		if i, ok := seen[k]; ok {
			out[i] = p
			continue
		}
		seen[k] = len(out)
		out = append(out, p)
	}
	return out, nil
}

// Presence statements.
const (
	// ForeignSessionsSQL lists which of the ids $1 are sessions of a person
	// other than $2, uploaded or in that person's presence (kept a day): a
	// device cannot claim them as its own. A new session is in presence
	// before it is uploaded, and peers lists its id.
	ForeignSessionsSQL = `SELECT session_id FROM conversations WHERE (session_id COLLATE "C")=ANY($1::text[]) AND user_id<>$2
		UNION SELECT session_id FROM bus_presence WHERE (session_id COLLATE "C")=ANY($1::text[]) AND user_id<>$2`
	clearPresenceSQL  = `DELETE FROM bus_presence WHERE device_id=$1 AND (agent,session_id) NOT IN (SELECT * FROM unnest($2::text[],$3::text[]))`
	upsertPresenceSQL = `INSERT INTO bus_presence(device_id,user_id,agent,session_id,repo,branch,title,busy,seen_at)
		SELECT $1,$2,a,s,r,b,t,busy,$9 FROM unnest($3::text[],$4::text[],$5::text[],$6::text[],$7::text[],$8::bool[]) AS x(a,s,r,b,t,busy)
		ON CONFLICT (device_id,agent,session_id) DO UPDATE SET user_id=EXCLUDED.user_id,repo=EXCLUDED.repo,branch=EXCLUDED.branch,
			title=EXCLUDED.title,busy=EXCLUDED.busy,seen_at=EXCLUDED.seen_at`
)

// heartbeat replaces the device's presence with sessions. An id that is
// another person's session (uploaded or in their presence) is not recorded
// (returned): a device could otherwise pose as that session to send, spend
// its limits, or make it ambiguous to address.
func (s *Store) heartbeat(ctx context.Context, c busproto.Caller, sessions []busproto.PresenceSession, now time.Time) ([]string, error) {
	var ignored []string
	err := inTx(ctx, s.Pool, func(tx pgx.Tx) error {
		ids := make([]string, len(sessions))
		for i, p := range sessions {
			ids[i] = p.SessionID
		}
		rows, err := tx.Query(ctx, ForeignSessionsSQL, ids, c.UserID)
		if err != nil {
			return err
		}
		if ignored, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return err
		}
		var a, sid, repo, branch, title []string
		var busy []bool
		for _, p := range sessions {
			if slices.Contains(ignored, p.SessionID) {
				continue
			}
			a, sid, repo, branch, title, busy = append(a, p.Agent), append(sid, p.SessionID), append(repo, p.Repo), append(branch, p.Branch), append(title, p.Title), append(busy, p.Busy)
		}
		if _, err := tx.Exec(ctx, clearPresenceSQL, c.DeviceID, a, sid); err != nil {
			return err
		}
		if len(a) > 0 {
			if _, err := tx.Exec(ctx, upsertPresenceSQL, c.DeviceID, c.UserID, a, sid, repo, branch, title, busy, now); err != nil {
				return err
			}
		}
		return nil
	})
	return ignored, err
}

// Poll records the device's presence and answers its deliverable set,
// holding up to WaitSeconds while nothing is newer than the cursor. It
// returns ctx's error when the device gives up first.
func (s *Store) Poll(ctx context.Context, c busproto.Caller, req busproto.PollRequest) (busproto.PollResponse, error) {
	sessions, err := validPresence(req.Sessions)
	if err != nil {
		return busproto.PollResponse{}, err
	}
	if req.WaitSeconds < 0 || req.Cursor < 0 {
		return busproto.PollResponse{}, badRequest("wait_seconds and cursor must not be negative")
	}
	wait := min(time.Duration(req.WaitSeconds)*time.Second, busproto.PollWait)
	ignored, err := s.heartbeat(ctx, c, sessions, s.now())
	if err != nil {
		return busproto.PollResponse{}, err
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	timedOut := wait == 0
	for {
		woken, gen := s.hub.wait(c.UserID)
		out, newest, err := s.deliverable(ctx, c, s.now())
		if err != nil {
			return busproto.PollResponse{}, err
		}
		// A generation the device has not seen means the person's set
		// changed since its last answer, maybe by shrinking (a revoke
		// re-held a message), which the cursor cannot show.
		if newest > req.Cursor || gen != req.Gen || timedOut {
			out.Cursor, out.Gen, out.Ignored = max(newest, req.Cursor), gen, ignored
			return out, nil
		}
		select {
		case <-woken:
		case <-timer.C:
			timedOut = true
		case <-ctx.Done():
			return busproto.PollResponse{}, ctx.Err()
		}
	}
}

// DeliverableSQL is the person $1's deliverable messages (expiring after
// $2) that the device $3 should see: those to its sessions (live in its
// presence or uploaded from it), those it claimed, and unclaimed @user
// messages (the caller filters them by eligibility).
const DeliverableSQL = `SELECT ` + envCols + ` FROM ` + envFrom + `
	WHERE m.to_user=$1 AND m.state IN ('queued','claimed') AND m.expires_at>$2 AND (
		(m.state='claimed' AND m.claimed_device=$3)
		OR (m.state='queued' AND m.to_session IS NULL)
		OR (m.state='queued' AND m.addressed='session' AND (
			EXISTS(SELECT 1 FROM bus_presence p WHERE p.device_id=$3 AND p.agent=m.to_agent AND p.session_id=m.to_session)
			OR EXISTS(SELECT 1 FROM conversations c WHERE c.device_id=$3 AND c.agent=m.to_agent AND c.session_id=m.to_session))))
	ORDER BY m.seq LIMIT 500`

// HeldSQL summarizes the held messages to the person $1 expiring after $2,
// per sender.
const HeldSQL = `SELECT m.from_user::text,(SELECT email FROM users WHERE id=m.from_user),count(*),min(m.created_at) FROM bus_messages m
	WHERE m.to_user=$1 AND m.state='held' AND m.expires_at>$2 GROUP BY m.from_user ORDER BY 2`

func held(ctx context.Context, q querier, userID string, now time.Time) ([]busproto.HeldSender, error) {
	rows, err := q.Query(ctx, HeldSQL, userID, now)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (busproto.HeldSender, error) {
		var h busproto.HeldSender
		return h, r.Scan(&h.UserID, &h.User, &h.Count, &h.Oldest)
	})
	if out == nil {
		out = []busproto.HeldSender{}
	}
	return out, err
}

// deliverable reads the device's set and its highest seq.
func (s *Store) deliverable(ctx context.Context, c busproto.Caller, now time.Time) (busproto.PollResponse, int64, error) {
	out := busproto.PollResponse{Messages: []busproto.Envelope{}, Claimable: []busproto.Claimable{}}
	rows, err := s.Pool.Query(ctx, DeliverableSQL, c.UserID, now, c.DeviceID)
	if err != nil {
		return out, 0, err
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (busproto.Envelope, error) { return scanEnvelope(r) })
	if err != nil {
		return out, 0, err
	}
	var live []liveSession
	var newest int64
	for i, e := range all {
		if e.ToSession != "" {
			out.Messages = append(out.Messages, e)
			newest = max(newest, e.Seq)
			continue
		}
		if live == nil {
			if live, err = userLive(ctx, s.Pool, c.UserID, now); err != nil {
				return out, 0, err
			}
			// Busy sessions first: a message there arrives at once.
			slices.SortStableFunc(live, func(a, b liveSession) int {
				switch {
				case a.busy && !b.busy:
					return -1
				case b.busy && !a.busy:
					return 1
				}
				return 0
			})
		}
		var mine []string
		for _, v := range live {
			if v.device == c.DeviceID && !(v.id == e.From && v.agent == e.FromAgent) && eligible(e.ToRepo, v, live) {
				mine = append(mine, v.id)
			}
		}
		if len(mine) > 0 {
			out.Claimable = append(out.Claimable, busproto.Claimable{Message: all[i], Sessions: mine})
			newest = max(newest, e.Seq)
		}
	}
	if out.Held, err = held(ctx, s.Pool, c.UserID, now); err != nil {
		return out, 0, err
	}
	return out, newest, nil
}

// Claim takes an @user message for one live session on the caller's
// device. The message row is locked, so of two devices claiming at once
// one wins and the other gets already_claimed. Claiming again for the same
// session returns the message.
func (s *Store) Claim(ctx context.Context, c busproto.Caller, req busproto.ClaimRequest) (busproto.ClaimResponse, error) {
	var out busproto.ClaimResponse
	if req.MessageID == "" || len(req.MessageID) > 64 {
		return out, badRequest("message_id is required")
	}
	now := s.now()
	err := inTx(ctx, s.Pool, func(tx pgx.Tx) error {
		sess, err := s.deviceSession(ctx, tx, c, strings.TrimSpace(req.SessionID), strings.TrimSpace(req.Agent), now)
		if err != nil {
			return err
		}
		if !sess.live {
			return fail(http.StatusForbidden, busproto.CodeSessionNotOnDevice, "session %s is not live on this device; a claim needs a live session", sess.id)
		}
		var toUser, addressed, state, claimedBy, claimedDevice, toRepo, fromSession, fromAgent string
		var expires time.Time
		err = tx.QueryRow(ctx, `SELECT to_user::text,addressed,state,COALESCE(claimed_by,''),COALESCE(claimed_device::text,''),to_repo,expires_at,from_session,from_agent
			FROM bus_messages WHERE id=$1 FOR NO KEY UPDATE`, req.MessageID).Scan(&toUser, &addressed, &state, &claimedBy, &claimedDevice, &toRepo, &expires, &fromSession, &fromAgent)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (toUser != c.UserID || addressed != "user")) {
			return fail(http.StatusNotFound, busproto.CodeNotFound, "no @user message %s for you", req.MessageID)
		}
		if err != nil {
			return err
		}
		switch busproto.State(state) {
		case busproto.StateClaimed, busproto.StateDelivered, busproto.StateRead:
			if claimedDevice == c.DeviceID && claimedBy == sess.id {
				return s.readEnvelope(ctx, tx, req.MessageID, &out.Message)
			}
			return fail(http.StatusConflict, busproto.CodeAlreadyClaimed, "message %s was claimed by session %s", req.MessageID, claimedBy)
		case busproto.StateQueued:
		default:
			return fail(http.StatusConflict, busproto.CodeNotEligible, "message %s is %s", req.MessageID, state)
		}
		if fromSession == sess.id && fromAgent == sess.agent {
			return fail(http.StatusConflict, busproto.CodeNotEligible, "message %s was sent by this session", req.MessageID)
		}
		if !now.Before(expires) {
			return fail(http.StatusConflict, busproto.CodeNotEligible, "message %s expired", req.MessageID)
		}
		live, err := userLive(ctx, tx, c.UserID, now)
		if err != nil {
			return err
		}
		if !eligible(toRepo, liveSession{device: c.DeviceID, agent: sess.agent, id: sess.id, repo: sess.repo}, live) {
			return fail(http.StatusConflict, busproto.CodeNotEligible, "message %s is for a session on %s, and one is live", req.MessageID, toRepo)
		}
		if _, err := tx.Exec(ctx, `UPDATE bus_messages SET state='claimed',to_session=$2,to_agent=$3,claimed_by=$2,claimed_device=$4,claimed_at=$5 WHERE id=$1`,
			req.MessageID, sess.id, sess.agent, c.DeviceID, now); err != nil {
			return err
		}
		if err := audit(ctx, tx, c, now, "bus.claim", "bus_message", req.MessageID, map[string]any{"session": sess.id, "agent": sess.agent}); err != nil {
			return err
		}
		return s.readEnvelope(ctx, tx, req.MessageID, &out.Message)
	})
	return out, err
}

func (s *Store) readEnvelope(ctx context.Context, q querier, id string, out *busproto.Envelope) error {
	e, err := scanEnvelope(q.QueryRow(ctx, `SELECT `+envCols+` FROM `+envFrom+` WHERE m.id=$1`, id))
	*out = e
	return err
}

// heldByDevice is AckSQL's and UndeliveredSQL's test that the device $3
// holds a message: claimed by it (also when a revoke held the claimed
// message again after its hook printed it), or queued to a session on it.
const heldByDevice = `(
		(m.state IN ('claimed','held') AND m.claimed_device=$3)
		OR (m.state='queued' AND m.addressed='session' AND (
			EXISTS(SELECT 1 FROM bus_presence p WHERE p.device_id=$3 AND p.agent=m.to_agent AND p.session_id=m.to_session)
			OR EXISTS(SELECT 1 FROM conversations c WHERE c.device_id=$3 AND c.agent=m.to_agent AND c.session_id=m.to_session))))`

// AckSQL marks delivered those of the messages $1 to the person $2 that
// the device $3 holds.
const AckSQL = `UPDATE bus_messages m SET state='delivered',delivered_at=$4
	WHERE m.id=ANY($1::text[]) AND m.to_user=$2 AND ` + heldByDevice + `
	RETURNING m.id`

// UndeliveredSQL marks undelivered (reason $4) those of the messages $1 to
// the person $2 that the device $3 holds and gave up on: no hook confirmed
// printing them. The sender's inbox shows the state, so the sender can
// send again.
const UndeliveredSQL = `UPDATE bus_messages m SET state='undelivered',reason=$4
	WHERE m.id=ANY($1::text[]) AND m.to_user=$2 AND ` + heldByDevice + `
	RETURNING m.id`

// heldSession is ReadSQL's and readBeforeSQL's test that the device $3
// holds the session a message was delivered to: it claimed the message for
// it, reports the session in its presence, or uploaded its transcript.
const heldSession = `(m.claimed_device=$3
		OR EXISTS(SELECT 1 FROM bus_presence p WHERE p.device_id=$3 AND p.agent=m.to_agent AND p.session_id=m.to_session)
		OR EXISTS(SELECT 1 FROM conversations c WHERE c.device_id=$3 AND c.agent=m.to_agent AND c.session_id=m.to_session))`

// ReadSQL marks read the messages of the read receipts (ids $1, sessions
// $5, agents $6, times $7) that were delivered to that session and agent
// of the person $2 on a session the device $3 holds. read_at is the
// receipt's time cut to [delivered_at, $4]: the device's clock is not the
// server's. Each message is looked up by its id.
const ReadSQL = `UPDATE bus_messages m SET state='read', read_at=LEAST($4::timestamptz, GREATEST(r.at, m.delivered_at))
	FROM unnest($1::text[], $5::text[], $6::text[], $7::timestamptz[]) AS r(id, session, agent, at)
	WHERE m.id=r.id AND m.to_user=$2 AND m.state='delivered' AND m.to_session=r.session AND m.to_agent=r.agent AND ` + heldSession + `
	RETURNING m.id`

// readBeforeSQL: which of the receipts ($1, $4, $5; not $6) name a message
// read before, under the same conditions as ReadSQL.
const readBeforeSQL = `SELECT m.id FROM bus_messages m, unnest($1::text[], $4::text[], $5::text[]) AS r(id, session, agent)
	WHERE m.id=r.id AND m.to_user=$2 AND m.state='read' AND m.to_session=r.session AND m.to_agent=r.agent AND ` + heldSession + `
		AND NOT (m.id=ANY($6::text[]))`

// validReads checks read receipts and keeps the earliest of each id.
func validReads(in []busproto.ReadReceipt) ([]busproto.ReadReceipt, error) {
	var out []busproto.ReadReceipt
	for _, r := range in {
		if r.ID == "" || len(r.ID) > 64 || r.Session == "" || len(r.Session) > 256 || r.Agent == "" || len(r.Agent) > 64 || r.At.IsZero() {
			return nil, badRequest("read: each receipt names a message id (at most 64 bytes), a session, an agent and a time")
		}
		if i := slices.IndexFunc(out, func(o busproto.ReadReceipt) bool { return o.ID == r.ID }); i >= 0 {
			if r.At.Before(out[i].At) {
				out[i] = r
			}
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// EndedSQL marks undelivered (reason $4) those of the queued messages $1
// to the person $2 whose session ended on the device $3 before a hook
// delivered them (#67) and that no other device holds: their session is
// not live in another device's presence nor uploaded from one. Ack first
// runs UndeliveredSQL over the report (the messages the device holds),
// then this over the rest: the device's report races its next poll,
// which drops the ended session from its presence, and a session that
// ended before it was uploaded is then on no device.
const EndedSQL = `UPDATE bus_messages m SET state='undelivered',reason=$4
	WHERE m.id=ANY($1::text[]) AND m.to_user=$2 AND m.state='queued' AND m.addressed='session'
		AND NOT EXISTS(SELECT 1 FROM bus_presence p WHERE (p.session_id COLLATE "C")=(m.to_session COLLATE "C") AND p.agent=m.to_agent AND p.device_id<>$3)
		AND NOT EXISTS(SELECT 1 FROM conversations c WHERE (c.session_id COLLATE "C")=(m.to_session COLLATE "C") AND c.agent=m.to_agent AND c.device_id<>$3)
	RETURNING m.id`

// Ack records that hooks printed the messages in IDs (delivered_at), that
// the device gave up on those in Undelivered and SessionEnded, and that the
// messages of the Read receipts were read (read_at, after the deliveries of
// this batch). Acking a message already delivered to the person (or
// reporting one already undelivered, or a receipt for one already read) is
// a no-op that reports it taken.
func (s *Store) Ack(ctx context.Context, c busproto.Caller, req busproto.AckRequest) (busproto.AckResponse, error) {
	out := busproto.AckResponse{Acked: []string{}, Rejected: []string{}, Read: []string{}, ReadRejected: []string{}}
	if n := len(req.IDs) + len(req.Undelivered) + len(req.SessionEnded) + len(req.Read); n == 0 || n > busproto.MaxAck {
		return out, badRequest("ids, undelivered, session_ended and read: 1 to %d entries", busproto.MaxAck)
	}
	reads, err := validReads(req.Read)
	if err != nil {
		return out, err
	}
	var ids, gone, ended []string
	for _, l := range []struct {
		in  []string
		out *[]string
	}{{req.IDs, &ids}, {req.Undelivered, &gone}, {req.SessionEnded, &ended}} {
		for _, id := range l.in {
			if len(id) > 64 {
				return out, badRequest("ids: a message id is at most 64 bytes")
			}
			if !slices.Contains(ids, id) && !slices.Contains(gone, id) && !slices.Contains(ended, id) {
				*l.out = append(*l.out, id)
			}
		}
	}
	now := s.now()
	err = inTx(ctx, s.Pool, func(tx pgx.Tx) error {
		acked, before, err := settle(ctx, tx, AckSQL, ackedBeforeSQL, ids, c, now)
		if err != nil {
			return err
		}
		undelivered, already, err := settle(ctx, tx, UndeliveredSQL, undeliveredBeforeSQL, gone, c, busproto.ReasonUnconfirmed)
		if err != nil {
			return err
		}
		endedHeld, _, err := settle(ctx, tx, UndeliveredSQL, undeliveredBeforeSQL, ended, c, busproto.ReasonSessionEnded)
		if err != nil {
			return err
		}
		rest := slices.DeleteFunc(slices.Clone(ended), func(id string) bool { return slices.Contains(endedHeld, id) })
		endedFree, _, err := settle(ctx, tx, EndedSQL, undeliveredBeforeSQL, rest, c, busproto.ReasonSessionEnded)
		if err != nil {
			return err
		}
		endedNow := append([]string{}, slices.Concat(endedHeld, endedFree)...) // never nil: $3 of undeliveredBeforeSQL
		var endedBefore []string
		if len(ended) > 0 {
			rows, err := tx.Query(ctx, undeliveredBeforeSQL, ended, c.UserID, endedNow)
			if err != nil {
				return err
			}
			if endedBefore, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
				return err
			}
		}
		for _, id := range slices.Concat(ids, gone, ended) {
			if slices.Contains(acked, id) || slices.Contains(before, id) || slices.Contains(undelivered, id) || slices.Contains(already, id) ||
				slices.Contains(endedNow, id) || slices.Contains(endedBefore, id) {
				out.Acked = append(out.Acked, id)
			} else {
				out.Rejected = append(out.Rejected, id)
			}
		}
		if len(ids)+len(gone)+len(ended) > 0 {
			meta := map[string]any{"delivered": acked, "already": len(before), "rejected": out.Rejected}
			if len(gone) > 0 {
				meta["undelivered"] = undelivered
			}
			if len(ended) > 0 {
				meta["session_ended"] = endedNow
			}
			if err := audit(ctx, tx, c, now, "bus.deliver", "bus_message", "", meta); err != nil {
				return err
			}
		}
		if len(reads) == 0 {
			return nil
		}
		read, readBefore, err := s.markRead(ctx, tx, c, reads, now)
		if err != nil {
			return err
		}
		for _, r := range reads {
			if slices.Contains(read, r.ID) || slices.Contains(readBefore, r.ID) {
				out.Read = append(out.Read, r.ID)
			} else {
				out.ReadRejected = append(out.ReadRejected, r.ID)
			}
		}
		return audit(ctx, tx, c, now, "bus.read", "bus_message", "", map[string]any{"read": read, "already": len(readBefore), "rejected": out.ReadRejected})
	})
	if err != nil {
		return busproto.AckResponse{}, err
	}
	return out, nil
}

// markRead runs ReadSQL and readBeforeSQL over the receipts: the ids it
// marked read, and those read before.
func (s *Store) markRead(ctx context.Context, tx pgx.Tx, c busproto.Caller, reads []busproto.ReadReceipt, now time.Time) (read, before []string, err error) {
	ids, sessions, agents, ats := make([]string, len(reads)), make([]string, len(reads)), make([]string, len(reads)), make([]time.Time, len(reads))
	for i, r := range reads {
		ids[i], sessions[i], agents[i], ats[i] = r.ID, r.Session, r.Agent, r.At
	}
	rows, err := tx.Query(ctx, ReadSQL, ids, c.UserID, c.DeviceID, now, sessions, agents, ats)
	if err != nil {
		return nil, nil, err
	}
	if read, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return nil, nil, err
	}
	if read == nil {
		read = []string{}
	}
	rows, err = tx.Query(ctx, readBeforeSQL, ids, c.UserID, c.DeviceID, sessions, agents, read)
	if err != nil {
		return nil, nil, err
	}
	before, err = pgx.CollectRows(rows, pgx.RowTo[string])
	return read, before, err
}

// settle runs one of Ack's updates (AckSQL or UndeliveredSQL, with arg
// as $4) over ids, and its before query: the ids it changed, and those
// already in its end state.
func settle(ctx context.Context, tx pgx.Tx, update, beforeSQL string, ids []string, c busproto.Caller, arg any) (changed, before []string, err error) {
	changed, before = []string{}, []string{}
	if len(ids) == 0 {
		return changed, before, nil
	}
	rows, err := tx.Query(ctx, update, ids, c.UserID, c.DeviceID, arg)
	if err != nil {
		return nil, nil, err
	}
	if changed, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return nil, nil, err
	}
	if changed == nil {
		changed = []string{}
	}
	rows, err = tx.Query(ctx, beforeSQL, ids, c.UserID, changed)
	if err != nil {
		return nil, nil, err
	}
	before, err = pgx.CollectRows(rows, pgx.RowTo[string])
	return changed, before, err
}

// PeersSQL is every live session (seen since $1), with its person, device
// and uploaded transcript looked up by key (Peers drops disabled people,
// revoked devices and hidden sessions).
const PeersSQL = `SELECT p.session_id,p.agent,p.user_id::text,
		(SELECT email FROM users WHERE id=p.user_id),(SELECT name FROM users WHERE id=p.user_id),(SELECT disabled FROM users WHERE id=p.user_id),
		COALESCE((SELECT name FROM devices WHERE id=p.device_id),''),COALESCE((SELECT revoked_at IS NOT NULL FROM devices WHERE id=p.device_id),false),
		p.repo,p.branch,p.title,
		COALESCE((SELECT title FROM conversations c WHERE c.device_id=p.device_id AND c.agent=p.agent AND c.session_id=p.session_id),''),
		COALESCE((SELECT hidden_at IS NOT NULL FROM conversations c WHERE c.device_id=p.device_id AND c.agent=p.agent AND c.session_id=p.session_id),false),
		p.busy,p.seen_at
	FROM bus_presence p WHERE p.seen_at>$1 LIMIT 2000`

func userMatches(filter string, p busproto.Peer) bool {
	f := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(filter), "@"))
	if f == "" {
		return true
	}
	local, _, _ := strings.Cut(strings.ToLower(p.User), "@")
	return f == p.UserID || f == strings.ToLower(p.User) || f == local || f == strings.ToLower(p.UserName)
}

// Peers lists live sessions across the organization: the caller's own
// person first, then by person, busy before idle. A session live on two
// devices is listed once, from the latest report.
func (s *Store) Peers(ctx context.Context, c busproto.Caller, q busproto.PeersQuery) (busproto.PeersResponse, error) {
	rows, err := s.Pool.Query(ctx, PeersSQL, s.now().Add(-busproto.PresenceTTL))
	if err != nil {
		return busproto.PeersResponse{}, err
	}
	type row struct {
		busproto.Peer
		disabled, revoked, hidden bool
		uploadedTitle             string
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var p row
		return p, r.Scan(&p.Session, &p.Agent, &p.UserID, &p.User, &p.UserName, &p.disabled, &p.Device, &p.revoked,
			&p.Repo, &p.Branch, &p.Title, &p.uploadedTitle, &p.hidden, &p.Busy, &p.SeenAt)
	})
	if err != nil {
		return busproto.PeersResponse{}, err
	}
	out := busproto.PeersResponse{Peers: []busproto.Peer{}}
	index := map[[3]string]int{}
	for _, r := range all {
		p := r.Peer
		if p.Title == "" {
			p.Title = r.uploadedTitle
		}
		if r.disabled || r.revoked || r.hidden || p.Session == q.Session || !RepoMatches(q.Repo, q.Roots, p.Repo) || !userMatches(q.User, p) || (q.Agent != "" && !strings.EqualFold(q.Agent, p.Agent)) {
			continue
		}
		p.Own = p.UserID == c.UserID
		k := [3]string{p.Session, p.Agent, p.UserID}
		if i, ok := index[k]; ok {
			if p.SeenAt.After(out.Peers[i].SeenAt) {
				out.Peers[i] = p
			}
			continue
		}
		index[k] = len(out.Peers)
		out.Peers = append(out.Peers, p)
	}
	slices.SortFunc(out.Peers, func(a, b busproto.Peer) int {
		switch {
		case a.Own != b.Own:
			if a.Own {
				return -1
			}
			return 1
		case a.User != b.User:
			return strings.Compare(a.User, b.User)
		case a.Busy != b.Busy:
			if a.Busy {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Session, b.Session)
	})
	return out, nil
}

// InboxSQL is the session $1's messages: received by it ($2 is its person;
// held and refused ones are not shown to the recipient, nor expired ones
// from another person it does not accept: the sweep expires held messages
// too, B7) and sent by it,
// optionally only sent ($3), in thread $4 (”), before the keyset
// ($5 time, $6 id), newest first, $7 rows.
const InboxSQL = `SELECT * FROM (
	SELECT ` + envCols + `,'received' AS direction,m.state,m.reason,m.delivered_at,m.read_at FROM ` + envFrom + `
	WHERE m.to_session=$1 AND m.to_user=$2 AND m.state NOT IN ('held','refused') AND NOT $3
		AND (m.state<>'expired' OR m.sender='own' OR EXISTS(SELECT 1 FROM bus_accepts a WHERE a.recipient_user=$2 AND a.sender_user=m.from_user))
	UNION ALL
	SELECT ` + envCols + `,'sent',m.state,m.reason,m.delivered_at,m.read_at FROM ` + envFrom + `
	WHERE m.from_session=$1 AND m.from_user=$2) x
	WHERE ($4='' OR thread_id=$4) AND ($5::timestamptz IS NULL OR (created_at,id)<($5,$6))
	ORDER BY created_at DESC,id DESC LIMIT $7`

// Inbox lists a session's threads, received and sent, newest first, with
// each message's state. Undelivered messages past their expiry show as
// expired before the sweep marks them.
func (s *Store) Inbox(ctx context.Context, c busproto.Caller, q busproto.InboxQuery) (busproto.InboxResponse, error) {
	out := busproto.InboxResponse{Messages: []busproto.InboxItem{}}
	limit := q.Limit
	switch {
	case limit == 0:
		limit = busproto.InboxDefaultLimit
	case limit < 0 || limit > busproto.InboxMaxLimit:
		return out, badRequest("limit: 1 to %d", busproto.InboxMaxLimit)
	}
	var before *time.Time
	var beforeID string
	if q.Before != "" {
		ts, id, ok := strings.Cut(q.Before, "|")
		t, err := time.Parse(time.RFC3339Nano, ts)
		if !ok || err != nil || id == "" {
			return out, badRequest("before: a next value from an earlier page")
		}
		before, beforeID = &t, id
	}
	now := s.now()
	sess, err := s.deviceSession(ctx, s.Pool, c, strings.TrimSpace(q.Session), strings.TrimSpace(q.Agent), now)
	if err != nil {
		return out, err
	}
	rows, err := s.Pool.Query(ctx, InboxSQL, sess.id, c.UserID, q.SentOnly, q.Thread, before, beforeID, limit+1)
	if err != nil {
		return out, err
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (busproto.InboxItem, error) {
		var it busproto.InboxItem
		var state string
		e, err := scanEnvelope(r, &it.Direction, &state, &it.Reason, &it.DeliveredAt, &it.ReadAt)
		it.Envelope, it.State = e, busproto.State(state)
		return it, err
	})
	if err != nil {
		return out, err
	}
	if len(items) > limit {
		items = items[:limit]
		last := items[limit-1]
		out.Next = last.Sent.UTC().Format(time.RFC3339Nano) + "|" + last.ID
	}
	for i := range items {
		switch items[i].State {
		case busproto.StateQueued, busproto.StateHeld, busproto.StateClaimed:
			if !now.Before(items[i].ExpiresAt) {
				items[i].State = busproto.StateExpired
			}
		}
	}
	out.Messages = items
	return out, nil
}

// Accepts lists whom the person accepts messages from and who is held.
func (s *Store) Accepts(ctx context.Context, userID string) (busproto.AcceptsResponse, error) {
	out := busproto.AcceptsResponse{Accepted: []busproto.Accepted{}}
	rows, err := s.Pool.Query(ctx, `SELECT a.sender_user::text,u.email,a.created_at FROM bus_accepts a JOIN users u ON u.id=a.sender_user
		WHERE a.recipient_user=$1 ORDER BY u.email`, userID)
	if err != nil {
		return out, err
	}
	if out.Accepted, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (busproto.Accepted, error) {
		var a busproto.Accepted
		return a, r.Scan(&a.UserID, &a.User, &a.AcceptedAt)
	}); err != nil {
		return out, err
	}
	if out.Accepted == nil {
		out.Accepted = []busproto.Accepted{}
	}
	out.Held, err = held(ctx, s.Pool, userID, s.now())
	return out, err
}

// HeldListSQL is the person $1's held messages expiring after $2, newest
// first, with enough of each to decide on its sender: the sending session
// and the body's head (Held cuts it to a preview; the whole body never
// leaves the server). Bounded: a sender's held messages count against no
// limit of the recipient's, so their number is not.
const HeldListSQL = `SELECT m.id,m.from_user::text,(SELECT email FROM users WHERE id=m.from_user),(SELECT name FROM users WHERE id=m.from_user),
		m.from_agent,m.from_session,m.from_repo,m.from_branch,m.intent,m.addressed,left(m.body,2000),octet_length(m.body),cardinality(m.refs),m.created_at,m.expires_at
	FROM bus_messages m WHERE m.to_user=$1 AND m.state='held' AND m.expires_at>$2 ORDER BY m.created_at DESC,m.id DESC LIMIT 1000`

// Held lists the person's held messages by sender (B7), for their human to
// review before accepting: previews only, at most HeldPerSender each. It
// serves a login session only (internal/api): an agent must never read
// what a sender its human has not accepted wrote.
func (s *Store) Held(ctx context.Context, userID string) (busproto.HeldResponse, error) {
	out := busproto.HeldResponse{Senders: []busproto.HeldGroup{}}
	now := s.now()
	summary, err := held(ctx, s.Pool, userID, now)
	if err != nil {
		return out, err
	}
	rows, err := s.Pool.Query(ctx, HeldListSQL, userID, now)
	if err != nil {
		return out, err
	}
	type row struct {
		busproto.HeldMessage
		userID, user, name, head string
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var v row
		var intent string
		err := r.Scan(&v.ID, &v.userID, &v.user, &v.name, &v.Agent, &v.Session, &v.Repo, &v.Branch, &intent, &v.Addressed, &v.head, &v.Bytes, &v.Refs, &v.Sent, &v.ExpiresAt)
		v.Intent, v.Repo, v.Preview = busproto.Intent(intent), RepoName(v.Repo), busproto.Preview(v.head)
		return v, err
	})
	if err != nil {
		return out, err
	}
	index := map[string]int{}
	for _, h := range summary {
		index[h.UserID] = len(out.Senders)
		out.Senders = append(out.Senders, busproto.HeldGroup{HeldSender: h, Messages: []busproto.HeldMessage{}})
	}
	for _, v := range all {
		i, ok := index[v.userID]
		if !ok {
			continue // held after the summary was read; the next read lists it
		}
		g := &out.Senders[i]
		g.UserName = v.name
		if v.Sent.After(g.Newest) {
			g.Newest = v.Sent
		}
		if len(g.Messages) < busproto.HeldPerSender {
			g.Messages = append(g.Messages, v.HeldMessage)
		}
	}
	for i := range out.Senders {
		out.Senders[i].More = max(0, out.Senders[i].Count-len(out.Senders[i].Messages))
	}
	return out, nil
}

// Acceptance and sweep statements.
const (
	// releaseHeldSQL releases the held messages from $2 to $1 that expire
	// after $3, with a new seq so the recipient's poll wakes: queued, or
	// claimed again by the device that claimed one before a revoke (its
	// hook may have printed it, so no other device may take it).
	releaseHeldSQL = `UPDATE bus_messages SET state=CASE WHEN claimed_device IS NULL THEN 'queued' ELSE 'claimed' END,seq=nextval('bus_messages_seq')
		WHERE to_user=$1 AND from_user=$2 AND state='held' AND expires_at>$3`
	// reholdSQL holds the undelivered messages from $2 to $1 again: queued
	// ones, and @user ones a device claimed and has not acknowledged. The
	// claim is kept: that device's hook may have printed the message
	// before the revoke landed, so its late receipt is taken (AckSQL) and
	// an accept returns the message to that device, never to another.
	reholdSQL = `UPDATE bus_messages SET state='held'
		WHERE to_user=$1 AND from_user=$2 AND state IN ('queued','claimed')`
	// expireSQL expires a batch of undelivered messages past $1.
	expireSQL = `UPDATE bus_messages SET state='expired' WHERE id IN (
		SELECT id FROM bus_messages WHERE state IN ('queued','held','claimed') AND expires_at<=$1 LIMIT 1000)`
	// dropPresenceSQL drops presence older than $1.
	dropPresenceSQL = `DELETE FROM bus_presence WHERE seen_at<$1`
	// ackedBeforeSQL: which of $1 (not $3) were delivered to $2 before.
	ackedBeforeSQL       = `SELECT id FROM bus_messages WHERE id=ANY($1::text[]) AND to_user=$2 AND state IN ('delivered','read') AND NOT (id=ANY($3::text[]))`
	undeliveredBeforeSQL = `SELECT id FROM bus_messages WHERE id=ANY($1::text[]) AND to_user=$2 AND state='undelivered' AND NOT (id=ANY($3::text[]))`
)

// Accept lets the person c.UserID receive messages from sender (B7) and
// releases that sender's held messages to them.
func (s *Store) Accept(ctx context.Context, c busproto.Caller, sender string) (busproto.AcceptResponse, error) {
	var out busproto.AcceptResponse
	now := s.now()
	err := inTx(ctx, s.Pool, func(tx pgx.Tx) error {
		p, err := resolvePerson(ctx, tx, sender)
		if err != nil {
			return err
		}
		if p.id == c.UserID {
			return badRequest("your own sessions need no acceptance")
		}
		if err := lockAccept(ctx, tx, c.UserID, p.id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO bus_accepts(recipient_user,sender_user,created_at) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, c.UserID, p.id, now); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, releaseHeldSQL, c.UserID, p.id, now)
		if err != nil {
			return err
		}
		out = busproto.AcceptResponse{User: p.email, UserID: p.id, Accepted: true, Released: int(tag.RowsAffected())}
		return audit(ctx, tx, c, now, "bus.accept", "user", p.id, map[string]any{"released": out.Released})
	})
	if err != nil {
		return busproto.AcceptResponse{}, err
	}
	s.hub.notify(c.UserID)
	return out, nil
}

// Revoke withdraws an acceptance: later messages from sender are held, and
// so are its queued messages not yet delivered or claimed.
func (s *Store) Revoke(ctx context.Context, c busproto.Caller, sender string) (busproto.AcceptResponse, error) {
	var out busproto.AcceptResponse
	now := s.now()
	err := inTx(ctx, s.Pool, func(tx pgx.Tx) error {
		p, err := resolvePerson(ctx, tx, sender)
		if err != nil {
			return err
		}
		if err := lockAccept(ctx, tx, c.UserID, p.id); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM bus_accepts WHERE recipient_user=$1 AND sender_user=$2`, c.UserID, p.id)
		if err != nil {
			return err
		}
		was := tag.RowsAffected() > 0
		if tag, err = tx.Exec(ctx, reholdSQL, c.UserID, p.id); err != nil {
			return err
		}
		out = busproto.AcceptResponse{User: p.email, UserID: p.id, Reheld: int(tag.RowsAffected())}
		return audit(ctx, tx, c, now, "bus.revoke", "user", p.id, map[string]any{"was_accepted": was, "reheld": out.Reheld})
	})
	if err != nil {
		return busproto.AcceptResponse{}, err
	}
	// The recipient's devices drop the re-held messages from their inbox
	// at once: their polls answer on the new generation.
	s.hub.notify(c.UserID)
	return out, nil
}

// Sweep marks undelivered messages past their expiry expired and drops
// presence a day stale. It returns how many messages expired.
func (s *Store) Sweep(ctx context.Context) (int64, error) {
	now := s.now()
	var n int64
	for {
		tag, err := s.Pool.Exec(ctx, expireSQL, now)
		if err != nil {
			return n, err
		}
		n += tag.RowsAffected()
		if tag.RowsAffected() < 1000 {
			break
		}
	}
	_, err := s.Pool.Exec(ctx, dropPresenceSQL, now.Add(-24*time.Hour))
	return n, err
}
