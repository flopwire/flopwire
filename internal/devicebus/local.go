package devicebus

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/flopwire/flopwire/internal/bus"
	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/redact"
)

// Send sends a message from one of the device's sessions: through the
// server when one is configured, else into the local inbox. A refusal is a
// *busproto.Error with the server's codes either way.
func (b *Bus) Send(ctx context.Context, req busproto.SendRequest) (busproto.SendResponse, error) {
	if b.Local() {
		return b.sendLocal(ctx, req)
	}
	if err := b.notWithheld(ctx, req.FromSession, req.FromAgent); err != nil {
		return busproto.SendResponse{}, err
	}
	// The body and refs pass the device redactor before they leave the
	// machine (plan §4 "Redaction"), as transcript text does. Masks keep
	// the length, so the server's cap and duplicate check see the same
	// sizes; it finds nothing left to mask, so its counts come from here.
	counts := redactSend(&req)
	srv, _ := b.cfg.Connect()
	out, err := srv.Send(ctx, req)
	if err == nil && len(counts) > 0 {
		if out.Redactions == nil {
			out.Redactions = map[string]int{}
		}
		for rule, n := range counts {
			out.Redactions[rule] += n
		}
	}
	return out, err
}

// redactSend masks secrets in a send's body and refs in place and counts
// them by rule.
func redactSend(req *busproto.SendRequest) map[string]int {
	counts := map[string]int{}
	mask := func(s string) string {
		masked, matches := redact.Redact([]byte(s))
		for _, m := range matches {
			counts[m.Rule]++
		}
		return string(masked)
	}
	req.Body = mask(req.Body)
	if len(req.Refs) > 0 {
		refs := make([]string, len(req.Refs))
		for i, r := range req.Refs {
			refs[i] = mask(r)
		}
		req.Refs = refs
	}
	return counts
}

// Peers lists live sessions: the organization's from the server, or the
// device's own without one.
func (b *Bus) Peers(ctx context.Context, q busproto.PeersQuery) (busproto.PeersResponse, error) {
	if b.Local() {
		return b.peersLocal(ctx, q)
	}
	if err := b.notWithheld(ctx, q.Session, ""); err != nil {
		return busproto.PeersResponse{}, err
	}
	srv, _ := b.cfg.Connect()
	return srv.Peers(ctx, q)
}

// Inbox lists a session's messages, received and sent, newest first.
func (b *Bus) Inbox(ctx context.Context, q busproto.InboxQuery) (busproto.InboxResponse, error) {
	if b.Local() {
		return b.inboxLocal(ctx, q)
	}
	if err := b.notWithheld(ctx, q.Session, q.Agent); err != nil {
		return busproto.InboxResponse{}, err
	}
	srv, _ := b.cfg.Connect()
	return srv.Inbox(ctx, q)
}

// notWithheld refuses to name a session in a request to the server unless
// the device knows it and the path rules let it reach the server: the
// request would tell the server its id (and a send, its body). A live
// session is judged by presence; one that is not live (quiet past the
// live window, or not in presence yet) by Known. A session the device does
// not know at all is refused too: its path rules cannot be judged, and the
// server would refuse it anyway, but only after the id and body left.
func (b *Bus) notWithheld(ctx context.Context, session, agent string) error {
	if session == "" {
		return nil
	}
	all, err := b.sessions(ctx)
	if err != nil {
		return err
	}
	found, withheld := false, false
	for _, s := range all {
		if s.SessionID == session && (agent == "" || s.Agent == agent) {
			found, withheld = true, withheld || s.Withheld
		}
	}
	if !found {
		b.mu.Lock()
		known := b.cfg.Known
		b.mu.Unlock()
		if known != nil {
			stored, err := known(ctx, session)
			if err != nil {
				return err
			}
			for _, s := range stored {
				if s.SessionID == session && (agent == "" || s.Agent == agent) {
					found, withheld = true, withheld || s.Withheld
				}
			}
		}
	}
	switch {
	case withheld:
		return fail(http.StatusForbidden, busproto.CodeSessionNotOnDevice, "session %s is kept off the server by a path rule; it cannot use messaging", session)
	case !found:
		return fail(http.StatusForbidden, busproto.CodeSessionNotOnDevice, "session %s is not indexed on this device yet; try again in a few seconds", session)
	}
	return nil
}

func fail(status int, code, format string, args ...any) *busproto.Error {
	return &busproto.Error{Status: status, Code: code, Detail: fmt.Sprintf(format, args...)}
}

func badRequest(format string, args ...any) *busproto.Error {
	return fail(http.StatusBadRequest, busproto.CodeBadRequest, format, args...)
}

// newID is a message id as the server makes them: "m" and 16 hex digits.
func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "m" + hex.EncodeToString(b[:])
}

// sendInput is a send request checked and redacted, as the server does
// (internal/bus Send): the same caps, the same redactor.
type sendInput struct {
	intent     busproto.Intent
	to, body   string
	sha        []byte
	refs       []string
	replyTo    string
	repo       string
	redactions map[string]int
}

func checkSend(req busproto.SendRequest) (sendInput, error) {
	var in sendInput
	intent, err := busproto.ParseIntent(req.Intent)
	if err != nil {
		return in, badRequest("%s", err.Error())
	}
	to := strings.TrimSpace(req.To)
	switch {
	case to == "":
		return in, badRequest("to is required: a session id prefix or @user")
	case strings.TrimSpace(req.Body) == "":
		return in, badRequest("body is required")
	case len(req.Body) > busproto.MaxBodyBytes:
		return in, badRequest("body is %d bytes; the cap is %d (send longer material by ref)", len(req.Body), busproto.MaxBodyBytes)
	case !utf8.ValidString(req.Body):
		return in, badRequest("body must be UTF-8")
	case len(req.Refs) > busproto.MaxRefs:
		return in, badRequest("at most %d refs", busproto.MaxRefs)
	case len(req.ReplyTo) > 64 || len(req.Repo) > 4096:
		return in, badRequest("reply_to or repo is too long")
	}
	in.redactions = map[string]int{}
	for _, r := range req.Refs {
		r = strings.TrimSpace(r)
		if r == "" || len(r) > busproto.MaxRefBytes || !utf8.ValidString(r) {
			return in, badRequest("a ref is an archive address of at most %d bytes", busproto.MaxRefBytes)
		}
		masked, matches := redact.Redact([]byte(r))
		for _, m := range matches {
			in.redactions[m.Rule]++
		}
		in.refs = append(in.refs, string(masked))
	}
	masked, matches := redact.Redact([]byte(req.Body))
	for _, m := range matches {
		in.redactions[m.Rule]++
	}
	sum := sha256.Sum256(masked)
	in.intent, in.to, in.body, in.sha = intent, to, string(masked), sum[:]
	in.replyTo, in.repo = strings.TrimSpace(req.ReplyTo), strings.TrimSpace(req.Repo)
	return in, nil
}

// localUserID is the user id local envelopes carry.
func (b *Bus) localUserID() string { return "local:" + b.cfg.User }

// isLocalUser reports whether an @address names the device's person.
func (b *Bus) isLocalUser(name string) bool {
	name = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(name), "@"))
	return strings.EqualFold(name, b.cfg.User) || name == b.localUserID()
}

// eligible is the server's @user rule (internal/bus): a session on the
// repo the message is routed by, or any session while none of the
// person's live sessions is on it. The sender is never eligible.
func eligible(toRepo string, from busproto.Envelope, v Session, all []Session) bool {
	if v.SessionID == from.From && v.Agent == from.FromAgent {
		return false
	}
	if toRepo == "" || bus.RepoName(v.Repo) == toRepo {
		return true
	}
	for _, o := range all {
		if bus.RepoName(o.Repo) == toRepo {
			return false
		}
	}
	return true
}

// pickLocal chooses the session a local @user message goes to, in the
// order claimOrder uses with a server.
func pickLocal(e busproto.Envelope, all []Session) (Session, bool) {
	var ok []Session
	for _, v := range all {
		if eligible(e.ToRepo, e, v, all) {
			ok = append(ok, v)
		}
	}
	if len(ok) == 0 {
		return Session{}, false
	}
	ids := make([]string, len(ok))
	for i, v := range ok {
		ids[i] = v.SessionID
	}
	first := claimOrder(busproto.Claimable{Message: e, Sessions: ids}, ok)[0]
	for _, v := range ok {
		if v.SessionID == first {
			return v, true
		}
	}
	return Session{}, false
}

// resolveLocal resolves a session id prefix among the device's sessions:
// live ones (presence) and the ones it holds (Known). One session is one
// (id, agent); a live row describes it over a stored one.
func (b *Bus) resolveLocal(ctx context.Context, prefix string, live []Session) (Session, bool, error) {
	if len(prefix) < busproto.MinPrefix || len(prefix) > 256 || strings.ContainsAny(prefix, " \t\r\n") {
		return Session{}, false, badRequest("to: a session id prefix of at least %d characters, or @user", busproto.MinPrefix)
	}
	type key struct{ id, agent string }
	var order []key
	found := map[key]Session{}
	isLive := map[key]bool{}
	for _, s := range live {
		if strings.HasPrefix(s.SessionID, prefix) {
			k := key{s.SessionID, s.Agent}
			if _, ok := found[k]; !ok {
				order = append(order, k)
			}
			found[k], isLive[k] = s, true
		}
	}
	b.mu.Lock()
	known := b.cfg.Known
	b.mu.Unlock()
	if known != nil {
		stored, err := known(ctx, prefix)
		if err != nil {
			return Session{}, false, err
		}
		for _, s := range stored {
			k := key{s.SessionID, s.Agent}
			if _, ok := found[k]; !ok {
				order = append(order, k)
				found[k] = s
			}
		}
	}
	switch len(order) {
	case 0:
		return Session{}, false, fail(http.StatusNotFound, busproto.CodeUnknownRecipient, "no session id starts with %s; flopwire peers lists live sessions", prefix)
	case 1:
		return found[order[0]], isLive[order[0]], nil
	}
	e := fail(http.StatusConflict, busproto.CodeAmbiguousRecipient, "%s matches %d sessions; use a longer prefix", prefix, len(order))
	slices.SortFunc(order, func(x, y key) int { return strings.Compare(x.id+x.agent, y.id+y.agent) })
	for i, k := range order {
		if i == 10 {
			break
		}
		s := found[k]
		e.Candidates = append(e.Candidates, busproto.Candidate{Session: s.SessionID, Agent: s.Agent, User: b.cfg.User, Repo: s.Repo, Branch: s.Branch, Title: s.Title, Live: isLive[k]})
	}
	return Session{}, false, e
}

// sendLocal routes a message between the device's own sessions: the
// server's checks, limits and envelope, stored straight into the local
// inbox. Every message is from the user's own session (sender own).
func (b *Bus) sendLocal(ctx context.Context, req busproto.SendRequest) (busproto.SendResponse, error) {
	var out busproto.SendResponse
	in, err := checkSend(req)
	if err != nil {
		return out, err
	}
	live, err := b.sessions(ctx)
	if err != nil {
		return out, err
	}
	fromID, fromAgent := strings.TrimSpace(req.FromSession), strings.TrimSpace(req.FromAgent)
	if fromID == "" {
		return out, badRequest("a session id is required")
	}
	var from *Session
	for i, s := range live {
		if s.SessionID == fromID && (fromAgent == "" || s.Agent == fromAgent) {
			if from != nil && from.Agent != s.Agent {
				return out, badRequest("session %s exists for %s and %s: name the agent", fromID, from.Agent, s.Agent)
			}
			from = &live[i]
		}
	}
	if from == nil {
		return out, fail(http.StatusForbidden, busproto.CodeSessionNotOnDevice, "session %s is not live on this device", fromID)
	}
	now := b.cfg.Now().UTC().Truncate(time.Microsecond)
	e := busproto.Envelope{ID: newID(), From: from.SessionID, FromAgent: from.Agent, User: b.cfg.User, UserID: b.localUserID(),
		Repo: from.Repo, Branch: from.Branch, Sender: busproto.SenderOwn, Intent: in.intent, Body: in.body, Refs: in.refs,
		Sent: now, ExpiresAt: now.Add(busproto.DefaultTTL), ToUser: b.cfg.User, ToUserID: b.localUserID()}
	var to Session
	toKey := ""
	if strings.HasPrefix(in.to, "@") {
		if !b.isLocalUser(in.to) {
			return out, fail(http.StatusNotFound, busproto.CodeUnknownRecipient, "no person matches %s: without a server only @%s (this device's user) can be addressed", in.to, b.cfg.User)
		}
		e.Addressed, toKey = "user", "user"
		switch in.repo {
		case "*":
		case "":
			e.ToRepo = bus.RepoName(from.Repo)
		default:
			e.ToRepo = bus.RepoName(in.repo)
		}
		out.To = busproto.Recipient{User: b.cfg.User, UserID: b.localUserID(), Repo: e.ToRepo}
		for _, v := range live {
			if eligible(e.ToRepo, e, v, live) {
				out.To.Live = true
				out.To.Busy = out.To.Busy || v.Busy
			}
		}
		// Taken now when a session may have it; else the loop gives it to
		// the first eligible session that appears (runLocal).
		if v, ok := pickLocal(e, live); ok {
			to = v
		}
	} else {
		v, isLive, err := b.resolveLocal(ctx, in.to, live)
		if err != nil {
			return out, err
		}
		if v.SessionID == from.SessionID && v.Agent == from.Agent {
			return out, badRequest("a session cannot message itself")
		}
		to, toKey = v, "session:"+v.Agent+":"+v.SessionID
		e.Addressed = "session"
		out.To = busproto.Recipient{Session: v.SessionID, Agent: v.Agent, User: b.cfg.User, UserID: b.localUserID(), Repo: v.Repo, Branch: v.Branch, Live: isLive, Busy: isLive && v.Busy}
	}
	e.ToSession, e.ToAgent = to.SessionID, to.Agent
	var refusal *busproto.Error
	err = inTx(ctx, b.st.db, func(tx *sql.Tx) error {
		e.ThreadID = e.ID
		if refusal, err = b.checkLocal(ctx, tx, &e, in, toKey, now); err != nil {
			return err
		}
		state := busproto.StateQueued
		if refusal != nil {
			state = busproto.StateRefused
		}
		var seq int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(max(seq),0)+1 FROM devbus_messages WHERE origin='local'`).Scan(&seq); err != nil {
			return err
		}
		e.Seq = seq
		raw, err := json.Marshal(e)
		if err != nil {
			return err
		}
		reason := ""
		if refusal != nil {
			reason = refusal.Code
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO devbus_messages(id,origin,seq,to_session,to_agent,from_session,from_agent,thread_id,to_key,body_sha,envelope,state,refuse_reason,created_at,expires_at)
			VALUES(?,'local',?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			e.ID, seq, e.ToSession, e.ToAgent, e.From, e.FromAgent, e.ThreadID, toKey, in.sha, string(raw), string(state), reason, ms(now), ms(e.ExpiresAt))
		return err
	})
	if err != nil {
		return busproto.SendResponse{}, err
	}
	if refusal != nil {
		refusal.MessageID = e.ID
		return busproto.SendResponse{}, refusal
	}
	out.ID, out.ThreadID, out.State, out.Sender, out.Intent, out.Sent, out.ExpiresAt = e.ID, e.ThreadID, busproto.StateQueued, e.Sender, e.Intent, e.Sent, e.ExpiresAt
	if len(in.redactions) > 0 {
		out.Redactions = in.redactions
	}
	return out, nil
}

// checkLocal applies reply_to and the server's loop and volume limits
// (plan §3) to the local inbox. A refusal is returned, not failed: the
// message is still stored, as refused, so the sender's inbox lists it.
func (b *Bus) checkLocal(ctx context.Context, tx *sql.Tx, e *busproto.Envelope, in sendInput, toKey string, now time.Time) (*busproto.Error, error) {
	if in.replyTo != "" {
		var thread, raw string
		err := tx.QueryRowContext(ctx, `SELECT thread_id,envelope FROM devbus_messages WHERE id=? AND origin='local' AND state<>'refused'`, in.replyTo).Scan(&thread, &raw)
		if isNoRows(err) {
			return nil, fail(http.StatusNotFound, busproto.CodeNotFound, "reply_to %s: no such message sent or received by you", in.replyTo)
		}
		if err != nil {
			return nil, err
		}
		var parent busproto.Envelope
		if err := json.Unmarshal([]byte(raw), &parent); err != nil {
			return nil, err
		}
		e.ThreadID, e.ReplyTo = thread, in.replyTo
		if parent.Intent == busproto.IntentDone {
			return fail(http.StatusConflict, busproto.CodeReplyToDone, "%s closed its thread (intent done); it must not be answered", in.replyTo), nil
		}
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM devbus_messages WHERE origin='local' AND from_session=? AND created_at>? AND state<>'refused'
		AND body_sha=? AND to_key=?`, e.From, ms(now.Add(-busproto.DuplicateWindow)), in.sha, toKey).Scan(&n); err != nil {
		return nil, err
	}
	if n > 0 {
		return fail(http.StatusConflict, busproto.CodeDuplicate, "dropped: this session sent the same text to the same recipient in the last %s", busproto.DuplicateWindow), nil
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM devbus_messages WHERE origin='local' AND from_session=? AND created_at>? AND state<>'refused'`,
		e.From, ms(now.Add(-time.Hour))).Scan(&n); err != nil {
		return nil, err
	}
	if n >= busproto.SessionPerHour {
		return fail(http.StatusTooManyRequests, busproto.CodeSessionRate, "this session sent %d messages in the last hour; the limit is %d", n, busproto.SessionPerHour), nil
	}
	if e.ReplyTo != "" {
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM devbus_messages WHERE origin='local' AND thread_id=? AND created_at>? AND state<>'refused'`,
			e.ThreadID, ms(now.Add(-time.Hour))).Scan(&n); err != nil {
			return nil, err
		}
		if n >= busproto.ThreadPerHour {
			return fail(http.StatusTooManyRequests, busproto.CodeThreadRate, "thread %s had %d messages in the last hour; the limit is %d", e.ThreadID, n, busproto.ThreadPerHour), nil
		}
	}
	// Undelivered messages to the recipient: to the session, or, for
	// @user, those no session has taken yet.
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM devbus_messages WHERE origin='local' AND to_key=? AND state='queued' AND expires_at>?
		AND (to_key<>'user' OR to_session='')`, toKey, ms(now)).Scan(&n); err != nil {
		return nil, err
	}
	if n >= busproto.MaxUndelivered {
		return fail(http.StatusConflict, busproto.CodeRecipientFull, "the recipient has %d undelivered messages; the limit is %d", n, busproto.MaxUndelivered), nil
	}
	return nil, nil
}

// claimLocal gives each local @user message no session has taken to an
// eligible live session, as a claim would with a server.
func (b *Bus) claimLocal(ctx context.Context, live []Session) error {
	rows, err := b.st.db.QueryContext(ctx, `SELECT envelope FROM devbus_messages WHERE origin='local' AND to_session='' AND state='queued' AND expires_at>?`, ms(b.cfg.Now()))
	if err != nil {
		return err
	}
	waiting, err := decodeEnvelopes(rows)
	if err != nil {
		return err
	}
	for _, e := range waiting {
		v, ok := pickLocal(e, live)
		if !ok {
			continue
		}
		e.ToSession, e.ToAgent = v.SessionID, v.Agent
		raw, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if _, err := b.st.db.ExecContext(ctx, `UPDATE devbus_messages SET to_session=?, to_agent=?, envelope=? WHERE id=? AND to_session=''`,
			v.SessionID, v.Agent, string(raw), e.ID); err != nil {
			return err
		}
	}
	return nil
}

// runLocal keeps local @user messages moving to sessions as they appear,
// and drops what passed its retention.
func (b *Bus) runLocal(ctx context.Context) {
	tick := time.NewTicker(b.cfg.PresenceEvery)
	defer tick.Stop()
	purge := time.NewTicker(time.Minute)
	defer purge.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-purge.C:
			if err := b.st.purge(ctx, b.cfg.Now()); err != nil && ctx.Err() == nil {
				b.log.Warn("devicebus: purge", "err", err)
			}
		case <-tick.C:
			live, err := b.sessions(ctx)
			if err != nil {
				if ctx.Err() == nil {
					b.log.Warn("devicebus: presence", "err", err)
				}
				continue
			}
			b.setStatus(func(s *Status) { s.Sessions = len(live) })
			if err := b.claimLocal(ctx, live); err != nil && ctx.Err() == nil {
				b.log.Warn("devicebus: local routing", "err", err)
			}
		}
	}
}

// peersLocal lists the device's live sessions, the calling one left out,
// busy first.
func (b *Bus) peersLocal(ctx context.Context, q busproto.PeersQuery) (busproto.PeersResponse, error) {
	live, err := b.sessions(ctx)
	if err != nil {
		return busproto.PeersResponse{}, err
	}
	host, _ := os.Hostname()
	now := b.cfg.Now()
	out := busproto.PeersResponse{Peers: []busproto.Peer{}}
	for _, s := range live {
		if s.SessionID == q.Session || !bus.RepoMatches(q.Repo, q.Roots, s.Repo) || (q.Agent != "" && !strings.EqualFold(q.Agent, s.Agent)) ||
			(q.User != "" && !b.isLocalUser(q.User)) {
			continue
		}
		out.Peers = append(out.Peers, busproto.Peer{Session: s.SessionID, Agent: s.Agent, User: b.cfg.User, UserID: b.localUserID(), UserName: b.cfg.User,
			Device: host, Repo: s.Repo, Branch: s.Branch, Title: s.Title, Busy: s.Busy, Own: true, SeenAt: now})
	}
	slices.SortFunc(out.Peers, func(a, b busproto.Peer) int {
		if a.Busy != b.Busy {
			if a.Busy {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Session, b.Session)
	})
	return out, nil
}

// inboxLocal lists a session's local messages, received and sent, newest
// first, in the server's page shape.
func (b *Bus) inboxLocal(ctx context.Context, q busproto.InboxQuery) (busproto.InboxResponse, error) {
	out := busproto.InboxResponse{Messages: []busproto.InboxItem{}}
	limit := q.Limit
	switch {
	case limit == 0:
		limit = busproto.InboxDefaultLimit
	case limit < 0 || limit > busproto.InboxMaxLimit:
		return out, badRequest("limit: 1 to %d", busproto.InboxMaxLimit)
	}
	session := strings.TrimSpace(q.Session)
	if session == "" {
		return out, badRequest("a session id is required")
	}
	beforeAt, beforeID := int64(1<<62), ""
	if q.Before != "" {
		ts, id, ok := strings.Cut(q.Before, "|")
		t, err := time.Parse(time.RFC3339Nano, ts)
		if !ok || err != nil || id == "" {
			return out, badRequest("before: a next value from an earlier page")
		}
		beforeAt, beforeID = ms(t), id
	}
	rows, err := b.st.db.QueryContext(ctx, `SELECT envelope,state,refuse_reason,delivered_at,
			CASE WHEN to_session=? AND (?='' OR to_agent=?) AND NOT (from_session=? AND (?='' OR from_agent=?)) THEN 'received' ELSE 'sent' END
		FROM devbus_messages WHERE origin='local' AND (
			(to_session=? AND (?='' OR to_agent=?) AND state<>'refused' AND NOT ?)
			OR (from_session=? AND (?='' OR from_agent=?)))
		AND (?='' OR thread_id=?) AND (created_at<? OR (created_at=? AND id<?))
		ORDER BY created_at DESC, id DESC LIMIT ?`,
		session, q.Agent, q.Agent, session, q.Agent, q.Agent,
		session, q.Agent, q.Agent, q.SentOnly,
		session, q.Agent, q.Agent,
		q.Thread, q.Thread, beforeAt, beforeAt, beforeID, limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	now := b.cfg.Now()
	for rows.Next() {
		var raw, state, reason, dir string
		var delivered sql.NullInt64
		if err := rows.Scan(&raw, &state, &reason, &delivered, &dir); err != nil {
			return out, err
		}
		var it busproto.InboxItem
		if err := json.Unmarshal([]byte(raw), &it.Envelope); err != nil {
			return out, err
		}
		it.Direction, it.State, it.RefuseReason = dir, busproto.State(state), reason
		if delivered.Valid {
			t := time.UnixMilli(delivered.Int64).UTC()
			it.DeliveredAt = &t
		}
		if it.State == busproto.StateQueued && !now.Before(it.ExpiresAt) {
			it.State = busproto.StateExpired
		}
		out.Messages = append(out.Messages, it)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if len(out.Messages) > limit {
		out.Messages = out.Messages[:limit]
		last := out.Messages[limit-1]
		out.Next = last.Sent.UTC().Format(time.RFC3339Nano) + "|" + last.ID
	}
	return out, nil
}
