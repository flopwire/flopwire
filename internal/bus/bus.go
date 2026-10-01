// Package bus is the server side of the message bus (notes/message-bus/
// plan.md §3, §4): presence, send with its hold rule and limits, the long
// poll, claims, delivery receipts, peers, inboxes and acceptance. The wire
// types are in internal/busproto; internal/api serves the routes.
//
// Every write commits together with its audit event, so a send, claim,
// receipt, accept or revoke that cannot be audited does not happen.
package bus

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/domain"
	"github.com/flopwire/flopwire/internal/redact"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store serves the bus over Postgres.
type Store struct {
	Pool *pgxpool.Pool
	// Now is the clock; nil is time.Now. Tests move it to check expiry
	// and the hourly limits.
	Now func() time.Time
	hub hub
}

func (s *Store) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC().Truncate(time.Microsecond)
	}
	return s.Now().UTC().Truncate(time.Microsecond)
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func fail(status int, code, format string, args ...any) *busproto.Error {
	return &busproto.Error{Status: status, Code: code, Detail: fmt.Sprintf(format, args...)}
}

func badRequest(format string, args ...any) *busproto.Error {
	return fail(http.StatusBadRequest, busproto.CodeBadRequest, format, args...)
}

// newID is a message id: "m" and 16 hex digits.
func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "m" + hex.EncodeToString(b[:])
}

// RepoName is the name a repo is matched by: the last element of its root.
func RepoName(repo string) string {
	repo = strings.TrimRight(repo, "/")
	if i := strings.LastIndexByte(repo, '/'); i >= 0 {
		return repo[i+1:]
	}
	return repo
}

// repoMatches applies a repo filter: an absolute path matches that root
// and every directory under it; anything else matches the repo name.
func repoMatches(filter, repo string) bool {
	if filter == "" {
		return true
	}
	if strings.HasPrefix(filter, "/") {
		f := strings.TrimRight(filter, "/")
		return repo == f || strings.HasPrefix(repo, f+"/")
	}
	return RepoName(repo) == RepoName(filter)
}

func inTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func audit(ctx context.Context, tx pgx.Tx, c busproto.Caller, now time.Time, action, targetType, target string, meta map[string]any) error {
	if c.ClientIP != "" {
		meta["client_ip"] = c.ClientIP
	}
	return store.InsertAudit(ctx, tx, domain.AuditEvent{ID: uuid.NewString(), ActorID: c.UserID, DeviceID: c.DeviceID,
		Action: action, TargetType: targetType, TargetID: target, Metadata: meta, CreatedAt: now})
}

// session is one session as presence or an uploaded transcript knows it.
type session struct {
	id, agent, userID, user string
	repo, branch, title     string
	busy, live              bool
	deviceID                string
}

// SessionOnDeviceSQL finds a session the device $1 reported live since $4
// or uploaded: id $2, agent $3 (” for any). Live rows come first.
const SessionOnDeviceSQL = `SELECT agent,repo,branch,title,busy,true FROM bus_presence
	WHERE device_id=$1 AND session_id=$2 AND ($3='' OR agent=$3) AND seen_at>$4
	UNION ALL
	SELECT agent,COALESCE(repo_root,cwd,''),COALESCE(branches[cardinality(branches)],''),COALESCE(title,''),false,false FROM conversations
	WHERE device_id=$1 AND session_id=$2 AND ($3='' OR agent=$3) AND hidden_at IS NULL`

// deviceSession is the caller's session id on its own device: in its
// current presence, or a transcript it uploaded (a new session may not
// have uploaded yet). Anything else is session_not_on_device.
func (s *Store) deviceSession(ctx context.Context, q querier, c busproto.Caller, id, agent string, now time.Time) (session, error) {
	if id == "" || len(id) > 256 || len(agent) > 64 {
		return session{}, badRequest("a session id is required")
	}
	rows, err := q.Query(ctx, SessionOnDeviceSQL, c.DeviceID, id, agent, now.Add(-busproto.PresenceTTL))
	if err != nil {
		return session{}, err
	}
	defer rows.Close()
	var found []session
	for rows.Next() {
		v := session{id: id, userID: c.UserID, deviceID: c.DeviceID}
		if err := rows.Scan(&v.agent, &v.repo, &v.branch, &v.title, &v.busy, &v.live); err != nil {
			return session{}, err
		}
		found = append(found, v)
	}
	if err := rows.Err(); err != nil {
		return session{}, err
	}
	if len(found) == 0 {
		return session{}, fail(http.StatusForbidden, busproto.CodeSessionNotOnDevice, "session %s is not on this device: it is neither live in this device's presence nor uploaded from it", id)
	}
	slices.SortStableFunc(found, func(a, b session) int {
		switch {
		case a.live && !b.live:
			return -1
		case b.live && !a.live:
			return 1
		}
		return 0
	})
	for _, f := range found[1:] {
		if f.agent != found[0].agent {
			return session{}, badRequest("session %s exists for %s and %s: name the agent", id, found[0].agent, f.agent)
		}
	}
	return found[0], nil
}

// person is a user an @address or accept names.
type person struct{ id, email, name string }

// UserLookupSQL finds the enabled human users that $1 names: their id, email,
// email local part or name, case-insensitively. The users table is small.
const UserLookupSQL = `SELECT id::text,email,name FROM users WHERE NOT disabled AND identity_type='human'
	AND (id::text=$1 OR lower(email)=lower($1) OR lower(split_part(email,'@',1))=lower($1) OR lower(name)=lower($1))
	ORDER BY email LIMIT 20`

// resolvePerson resolves a person; an exact id or email wins over a local
// part or name that several people share.
func resolvePerson(ctx context.Context, q querier, name string) (person, error) {
	name = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(name), "@"))
	if name == "" || len(name) > 320 {
		return person{}, badRequest("name a person: @email, @name or a user id")
	}
	rows, err := q.Query(ctx, UserLookupSQL, name)
	if err != nil {
		return person{}, err
	}
	ps, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (person, error) {
		var p person
		return p, r.Scan(&p.id, &p.email, &p.name)
	})
	if err != nil {
		return person{}, err
	}
	for _, p := range ps {
		if p.id == name || strings.EqualFold(p.email, name) {
			return p, nil
		}
	}
	switch len(ps) {
	case 0:
		return person{}, fail(http.StatusNotFound, busproto.CodeUnknownRecipient, "no person matches @%s", name)
	case 1:
		return ps[0], nil
	}
	e := fail(http.StatusConflict, busproto.CodeAmbiguousRecipient, "@%s matches %d people; use an email", name, len(ps))
	for _, p := range ps {
		e.Candidates = append(e.Candidates, busproto.Candidate{User: p.email})
	}
	return person{}, e
}

// SessionPrefixSQL finds the sessions whose id starts with the LIKE
// pattern $1: live in presence (seen since $2), or uploaded and visible.
// Subagent transcripts and service identities' uploads are left out: no
// hook delivers to them.
const SessionPrefixSQL = `SELECT p.session_id,p.agent,p.user_id::text,u.email,p.repo,p.branch,p.title,p.busy,true
	FROM bus_presence p JOIN users u ON u.id=p.user_id LEFT JOIN devices d ON d.id=p.device_id
	WHERE p.session_id COLLATE "C" LIKE $1 AND p.seen_at>$2 AND (p.device_id IS NULL OR d.revoked_at IS NULL) AND NOT u.disabled AND u.identity_type='human'
	UNION ALL
	SELECT c.session_id,c.agent,c.user_id::text,u.email,COALESCE(c.repo_root,c.cwd,''),COALESCE(c.branches[cardinality(c.branches)],''),COALESCE(c.title,''),false,false
	FROM conversations c JOIN users u ON u.id=c.user_id
	WHERE c.session_id COLLATE "C" LIKE $1 AND c.hidden_at IS NULL AND c.depth=0 AND NOT u.disabled AND u.identity_type='human'
	LIMIT 500`

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// resolveSession resolves a session id prefix to one session. One session
// is one (id, agent, person); a live row describes it over an uploaded one.
func resolveSession(ctx context.Context, q querier, prefix string, now time.Time) (session, error) {
	if len(prefix) < busproto.MinPrefix || len(prefix) > 256 || strings.ContainsAny(prefix, " \t\r\n") {
		return session{}, badRequest("to: a session id prefix of at least %d characters, or @user", busproto.MinPrefix)
	}
	rows, err := q.Query(ctx, SessionPrefixSQL, likeEscape(prefix)+"%", now.Add(-busproto.PresenceTTL))
	if err != nil {
		return session{}, err
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (session, error) {
		var v session
		return v, r.Scan(&v.id, &v.agent, &v.userID, &v.user, &v.repo, &v.branch, &v.title, &v.busy, &v.live)
	})
	if err != nil {
		return session{}, err
	}
	type key struct{ id, agent, user string }
	var order []key
	byKey := map[key]session{}
	for _, v := range all {
		k := key{v.id, v.agent, v.userID}
		cur, seen := byKey[k]
		switch {
		case !seen:
			order = append(order, k)
			byKey[k] = v
		case v.live && (!cur.live || v.busy):
			byKey[k] = v
		}
	}
	switch len(order) {
	case 0:
		return session{}, fail(http.StatusNotFound, busproto.CodeUnknownRecipient, "no session id starts with %s; flopwire peers lists live sessions", prefix)
	case 1:
		return byKey[order[0]], nil
	}
	e := fail(http.StatusConflict, busproto.CodeAmbiguousRecipient, "%s matches %d sessions; use a longer prefix", prefix, len(order))
	slices.SortFunc(order, func(a, b key) int { return strings.Compare(a.id+a.agent+a.user, b.id+b.agent+b.user) })
	for i, k := range order {
		if i == 10 {
			break
		}
		v := byKey[k]
		e.Candidates = append(e.Candidates, busproto.Candidate{Session: v.id, Agent: v.agent, User: v.user, Repo: v.repo, Branch: v.branch, Title: v.title, Live: v.live})
	}
	return session{}, e
}

// UserLiveSQL is the person $1's live sessions (seen since $2), every
// device.
const UserLiveSQL = `SELECT COALESCE(device_id::text,''),agent,session_id,repo,busy FROM bus_presence WHERE user_id=$1 AND seen_at>$2`

type liveSession struct {
	device, agent, id, repo string
	busy                    bool
}

func userLive(ctx context.Context, q querier, userID string, now time.Time) ([]liveSession, error) {
	rows, err := q.Query(ctx, UserLiveSQL, userID, now.Add(-busproto.PresenceTTL))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (liveSession, error) {
		var v liveSession
		return v, r.Scan(&v.device, &v.agent, &v.id, &v.repo, &v.busy)
	})
}

// eligible reports whether an @user message routed to repo name toRepo may
// go to session v: one on that repo, or any session while none of the
// person's live sessions is on it (plan §3: the repo first, then
// anywhere).
func eligible(toRepo string, v liveSession, all []liveSession) bool {
	if toRepo == "" || RepoName(v.repo) == toRepo {
		return true
	}
	for _, o := range all {
		if RepoName(o.repo) == toRepo {
			return false
		}
	}
	return true
}

// Send stores a message from one of the caller's sessions and reports where
// it is going. A send a limit refuses is stored as refused (the sender's
// inbox lists it) and returned as a *busproto.Error carrying its id.
func (s *Store) Send(ctx context.Context, c busproto.Caller, req busproto.SendRequest) (busproto.SendResponse, error) {
	var out busproto.SendResponse
	intent, err := busproto.ParseIntent(req.Intent)
	if err != nil {
		return out, badRequest("%s", err.Error())
	}
	to := strings.TrimSpace(req.To)
	switch {
	case to == "":
		return out, badRequest("to is required: a session id prefix or @user")
	case strings.TrimSpace(req.Body) == "":
		return out, badRequest("body is required")
	case len(req.Body) > busproto.MaxBodyBytes:
		return out, badRequest("body is %d bytes; the cap is %d (send longer material by ref)", len(req.Body), busproto.MaxBodyBytes)
	case !utf8.ValidString(req.Body):
		return out, badRequest("body must be UTF-8")
	case len(req.Refs) > busproto.MaxRefs:
		return out, badRequest("at most %d refs", busproto.MaxRefs)
	case len(req.ReplyTo) > 64 || len(req.Repo) > 4096:
		return out, badRequest("reply_to or repo is too long")
	}
	refs := make([]string, 0, len(req.Refs))
	for _, r := range req.Refs {
		r = strings.TrimSpace(r)
		if r == "" || len(r) > busproto.MaxRefBytes || !utf8.ValidString(r) {
			return out, badRequest("a ref is an archive address of at most %d bytes", busproto.MaxRefBytes)
		}
		refs = append(refs, r)
	}
	masked, matches := redact.Redact([]byte(req.Body))
	redactions := map[string]int{}
	for _, m := range matches {
		redactions[m.Rule]++
	}
	body := string(masked)
	sum := sha256.Sum256(masked)
	now := s.now()
	var refusal *busproto.Error
	err = inTx(ctx, s.Pool, func(tx pgx.Tx) error {
		from, err := s.deviceSession(ctx, tx, c, strings.TrimSpace(req.FromSession), strings.TrimSpace(req.FromAgent), now)
		if err != nil {
			return err
		}
		m := message{id: newID(), from: from, intent: intent, body: body, sha: sum[:], refs: refs, created: now, expires: now.Add(busproto.DefaultTTL)}
		if strings.HasPrefix(to, "@") {
			p, err := resolvePerson(ctx, tx, to)
			if err != nil {
				return err
			}
			m.addressed, m.toUser, m.toEmail = "user", p.id, p.email
			switch repo := strings.TrimSpace(req.Repo); repo {
			case "*":
			case "":
				m.toRepo = RepoName(from.repo)
			default:
				m.toRepo = RepoName(repo)
			}
		} else {
			v, err := resolveSession(ctx, tx, to, now)
			if err != nil {
				return err
			}
			if v.id == from.id && v.agent == from.agent {
				return badRequest("a session cannot message itself")
			}
			m.addressed, m.toUser, m.toEmail, m.toSession, m.toAgent = "session", v.userID, v.user, v.id, v.agent
			out.To = busproto.Recipient{Session: v.id, Agent: v.agent, User: v.user, UserID: v.userID, Repo: v.repo, Branch: v.branch, Live: v.live, Busy: v.busy}
		}
		if err := lockSend(ctx, tx, m); err != nil {
			return err
		}
		m.thread = m.id
		if refusal, err = s.check(ctx, tx, c, &m, strings.TrimSpace(req.ReplyTo), now); err != nil {
			return err
		}
		m.sender = busproto.SenderTeammate
		if m.toUser == c.UserID {
			m.sender = busproto.SenderOwn
		}
		m.state = busproto.StateQueued
		if refusal != nil {
			m.state, m.refused = busproto.StateRefused, refusal.Code
		} else if m.sender == busproto.SenderTeammate {
			// Held until commit: an accept or revoke of this sender waits
			// for this message, so it releases or re-holds it.
			if err := lockAccept(ctx, tx, m.toUser, c.UserID); err != nil {
				return err
			}
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM bus_accepts WHERE recipient_user=$1 AND sender_user=$2)`, m.toUser, c.UserID).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				m.state = busproto.StateHeld
			}
		}
		if err := m.insert(ctx, tx, c); err != nil {
			return err
		}
		meta := map[string]any{"to": to, "to_user": m.toUser, "addressed": m.addressed, "from_session": from.id, "from_agent": from.agent,
			"intent": string(intent), "thread_id": m.thread, "state": string(m.state), "body_bytes": len(body), "refs": len(refs), "sender": m.sender}
		if m.toSession != "" {
			meta["to_session"] = m.toSession
		}
		if m.toRepo != "" {
			meta["to_repo"] = m.toRepo
		}
		if m.replyTo != "" {
			meta["reply_to"] = m.replyTo
		}
		if refusal != nil {
			meta["refuse_reason"] = refusal.Code
		}
		if len(redactions) > 0 {
			meta["redactions"] = redactions
		}
		if err := audit(ctx, tx, c, now, "bus.send", "bus_message", m.id, meta); err != nil {
			return err
		}
		if refusal != nil {
			refusal.MessageID = m.id
			return nil
		}
		if m.addressed == "user" {
			live, err := userLive(ctx, tx, m.toUser, now)
			if err != nil {
				return err
			}
			out.To = busproto.Recipient{User: m.toEmail, UserID: m.toUser, Repo: m.toRepo}
			for _, v := range live {
				if eligible(m.toRepo, v, live) {
					out.To.Live = true
					out.To.Busy = out.To.Busy || v.busy
				}
			}
		}
		out.ID, out.ThreadID, out.State, out.Sender, out.Intent, out.Sent, out.ExpiresAt = m.id, m.thread, m.state, m.sender, intent, now, m.expires
		if len(redactions) > 0 {
			out.Redactions = redactions
		}
		return nil
	})
	if err != nil {
		return busproto.SendResponse{}, err
	}
	if refusal != nil {
		return busproto.SendResponse{}, refusal
	}
	if out.State == busproto.StateQueued {
		s.hub.notify(out.To.UserID)
	}
	return out, nil
}

// message is a message being stored.
type message struct {
	id, thread, replyTo        string
	from                       session
	addressed                  string
	toUser, toEmail, toRepo    string
	toSession, toAgent, sender string
	intent                     busproto.Intent
	body                       string
	sha                        []byte
	refs                       []string
	state                      busproto.State
	refused                    string
	created, expires           time.Time
}

func (m *message) insert(ctx context.Context, tx pgx.Tx, c busproto.Caller) error {
	_, err := tx.Exec(ctx, `INSERT INTO bus_messages(id,thread_id,reply_to,from_user,from_device,from_agent,from_session,from_repo,from_branch,
		to_user,to_agent,to_session,to_repo,addressed,sender,intent,body,body_sha,refs,state,refuse_reason,created_at,expires_at)
		VALUES($1,$2,NULLIF($3,''),$4,NULLIF($5,'')::uuid,$6,$7,$8,$9,$10,NULLIF($11,''),NULLIF($12,''),$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)`,
		m.id, m.thread, m.replyTo, c.UserID, c.DeviceID, m.from.agent, m.from.id, m.from.repo, m.from.branch,
		m.toUser, m.toAgent, m.toSession, m.toRepo, m.addressed, m.sender, string(m.intent), m.body, m.sha, m.refs, string(m.state), m.refused, m.created, m.expires)
	return err
}

// lockSend serializes sends from one session and sends to one recipient,
// so two concurrent sends cannot both pass a limit with one slot left.
// The two locks are taken in one order.
func lockSend(ctx context.Context, tx pgx.Tx, m message) error {
	to := "bus:to-user:" + m.toUser
	if m.toSession != "" {
		to = "bus:to-session:" + m.toAgent + ":" + m.toSession
	}
	keys := []string{"bus:from:" + m.from.agent + ":" + m.from.id, to}
	slices.Sort(keys)
	for _, k := range keys {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, k); err != nil {
			return err
		}
	}
	return nil
}

// lockAccept serializes a send from sender to recipient with an accept or
// revoke between them. Without it a send that read the acceptance could
// commit its message queued after a revoke re-held the sender's messages,
// or one that read none could commit it held after an accept released
// them. A send takes it last, after lockSend and the thread lock; an
// accept or revoke takes no other advisory lock.
func lockAccept(ctx context.Context, tx pgx.Tx, recipient, sender string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "bus:accept:"+recipient+":"+sender)
	return err
}

// Queries the send limits run; each is served by an index (bus_test.go).
const (
	// ReplyToSQL loads the message $1 a reply names.
	ReplyToSQL = `SELECT thread_id,intent,from_user::text,to_user::text FROM bus_messages WHERE id=$1 AND state<>'refused'`
	// DuplicateSQL: the session $1 sent the body $2 to the same recipient
	// ($3 person, $4 addressing, $5 session) since $6.
	DuplicateSQL = `SELECT EXISTS(SELECT 1 FROM bus_messages WHERE from_session=$1 AND created_at>$6 AND state<>'refused'
		AND body_sha=$2 AND to_user=$3 AND addressed=$4 AND ($4='user' OR to_session=$5))`
	// SessionSendsSQL counts the session $1's sends since $2.
	SessionSendsSQL = `SELECT count(*) FROM bus_messages WHERE from_session=$1 AND created_at>$2 AND state<>'refused'`
	// ThreadSendsSQL counts the thread $1's messages since $2.
	ThreadSendsSQL = `SELECT count(*) FROM bus_messages WHERE thread_id=$1 AND created_at>$2 AND state<>'refused'`
	// SessionPendingSQL counts the session $1's undelivered messages
	// that expire after $2. Held messages count only for their own
	// sender $3: a sender the recipient has not accepted cannot fill the
	// recipient for everyone else.
	SessionPendingSQL = `SELECT count(*) FROM bus_messages WHERE to_session=$1 AND state IN ('queued','held','claimed') AND expires_at>$2
		AND (state<>'held' OR from_user=$3)`
	// UserPendingSQL counts the person $1's unclaimed @user messages that
	// expire after $2, held ones only from the sender $3.
	UserPendingSQL = `SELECT count(*) FROM bus_messages WHERE to_user=$1 AND state IN ('queued','held','claimed') AND to_session IS NULL AND expires_at>$2
		AND (state<>'held' OR from_user=$3)`
)

// check applies reply_to and the loop and volume limits (plan §3). A
// refusal is returned, not failed: the message is still stored, as refused.
func (s *Store) check(ctx context.Context, tx pgx.Tx, c busproto.Caller, m *message, replyTo string, now time.Time) (*busproto.Error, error) {
	if replyTo != "" {
		var thread, intent, fromUser, toUser string
		err := tx.QueryRow(ctx, ReplyToSQL, replyTo).Scan(&thread, &intent, &fromUser, &toUser)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && fromUser != c.UserID && toUser != c.UserID) {
			return nil, fail(http.StatusNotFound, busproto.CodeNotFound, "reply_to %s: no such message sent or received by you", replyTo)
		}
		if err != nil {
			return nil, err
		}
		m.thread, m.replyTo = thread, replyTo
		// The thread's lock comes after the sender and recipient locks in
		// every transaction (one thread each), so the order stays acyclic.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "bus:thread:"+thread); err != nil {
			return nil, err
		}
		if busproto.Intent(intent) == busproto.IntentDone {
			return fail(http.StatusConflict, busproto.CodeReplyToDone, "%s closed its thread (intent done); it must not be answered", replyTo), nil
		}
	}
	var dup bool
	if err := tx.QueryRow(ctx, DuplicateSQL, m.from.id, m.sha, m.toUser, m.addressed, m.toSession, now.Add(-busproto.DuplicateWindow)).Scan(&dup); err != nil {
		return nil, err
	}
	if dup {
		return fail(http.StatusConflict, busproto.CodeDuplicate, "dropped: this session sent the same text to the same recipient in the last %s", busproto.DuplicateWindow), nil
	}
	var n int
	if err := tx.QueryRow(ctx, SessionSendsSQL, m.from.id, now.Add(-time.Hour)).Scan(&n); err != nil {
		return nil, err
	}
	if n >= busproto.SessionPerHour {
		return fail(http.StatusTooManyRequests, busproto.CodeSessionRate, "this session sent %d messages in the last hour; the limit is %d", n, busproto.SessionPerHour), nil
	}
	if m.replyTo != "" {
		if err := tx.QueryRow(ctx, ThreadSendsSQL, m.thread, now.Add(-time.Hour)).Scan(&n); err != nil {
			return nil, err
		}
		if n >= busproto.ThreadPerHour {
			return fail(http.StatusTooManyRequests, busproto.CodeThreadRate, "thread %s had %d messages in the last hour; the limit is %d", m.thread, n, busproto.ThreadPerHour), nil
		}
	}
	if m.toSession != "" {
		err := tx.QueryRow(ctx, SessionPendingSQL, m.toSession, now, c.UserID).Scan(&n)
		if err != nil {
			return nil, err
		}
	} else if err := tx.QueryRow(ctx, UserPendingSQL, m.toUser, now, c.UserID).Scan(&n); err != nil {
		return nil, err
	}
	if n >= busproto.MaxUndelivered {
		return fail(http.StatusConflict, busproto.CodeRecipientFull, "the recipient has %d undelivered messages; the limit is %d", n, busproto.MaxUndelivered), nil
	}
	return nil, nil
}
