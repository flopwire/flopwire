package devicebus

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	_ "modernc.org/sqlite"

	"github.com/flopwire/flopwire/internal/busproto"
)

// store is the local inbox: every message this device should deliver, from
// the server's poll or routed on the device, with its delivery state. It
// is a SQLite file of its own (bus.db beside the client config), not a
// table in the index: the index writer keeps a transaction open for up
// to a second (localindex DeferCommit), and a hook asking for its messages
// must not wait for it.
//
// A row holds the whole envelope as the server set it (or as local routing
// built it with the same type), so a hook gets one shape whichever way a
// message came.
type store struct {
	db *sql.DB
}

// schemaVersion is the inbox's PRAGMA user_version. An inbox with another
// version is from an earlier build (pre-release: no migration) and is
// recreated empty: messages from a server come back with the next poll.
const schemaVersion = 2

const schema = `
CREATE TABLE IF NOT EXISTS devbus_messages (
  id            TEXT PRIMARY KEY,
  origin        TEXT NOT NULL,              -- server or local
  seq           INTEGER NOT NULL,           -- delivery order (the server's seq, or local)
  to_session    TEXT NOT NULL,              -- '' while a local @user message is unclaimed
  to_agent      TEXT NOT NULL,
  from_session  TEXT NOT NULL,
  from_agent    TEXT NOT NULL,
  thread_id     TEXT NOT NULL,
  to_key        TEXT NOT NULL DEFAULT '',   -- local: the recipient, for the limits
  body_sha      BLOB,                       -- local: the duplicate check
  envelope      TEXT NOT NULL,              -- busproto.Envelope JSON
  -- queued, leased (a hook took it and has not confirmed printing it),
  -- delivered (confirmed), undelivered (MaxAttempts leases, none
  -- confirmed), refused (local)
  state         TEXT NOT NULL,
  reason        TEXT NOT NULL DEFAULT '',   -- refused: the limit's code; undelivered: busproto.ReasonUnconfirmed
  attempts      INTEGER NOT NULL DEFAULT 0, -- leases handed to hooks
  lease_until   INTEGER,                    -- leased: unix ms when the lease ends
  created_at    INTEGER NOT NULL,           -- unix ms
  expires_at    INTEGER NOT NULL,
  delivered_at  INTEGER,
  -- server: '' none owed, owed (a delivery receipt), report (an undelivered
  -- report), done, rejected
  ack           TEXT NOT NULL DEFAULT '',
  listed        INTEGER NOT NULL DEFAULT 1  -- server: in the last poll's set
);
CREATE INDEX IF NOT EXISTS devbus_to ON devbus_messages (to_session, state);
CREATE INDEX IF NOT EXISTS devbus_ack ON devbus_messages (ack) WHERE ack IN ('owed', 'report');
CREATE INDEX IF NOT EXISTS devbus_lease ON devbus_messages (lease_until) WHERE state = 'leased';
CREATE INDEX IF NOT EXISTS devbus_from ON devbus_messages (from_session, created_at);
CREATE INDEX IF NOT EXISTS devbus_thread ON devbus_messages (thread_id, created_at);
CREATE INDEX IF NOT EXISTS devbus_expires ON devbus_messages (expires_at);
-- The held-message notice (Bus.HeldNotice): when the user was last told
-- about each sender's held messages.
CREATE TABLE IF NOT EXISTS devbus_notices (
  sender_user TEXT PRIMARY KEY,
  noticed_at  INTEGER NOT NULL              -- unix ms
);
`

func openStore(path string) (*store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// One connection: every write is serialized, so a statement that moves
	// a message from queued to delivered runs alone (Pending).
	db.SetMaxOpenConns(1)
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		db.Close()
		return nil, fmt.Errorf("devicebus: schema: %w", err)
	}
	if version != schemaVersion {
		if _, err := db.Exec(`DROP TABLE IF EXISTS devbus_messages; DROP TABLE IF EXISTS devbus_notices;`); err != nil {
			db.Close()
			return nil, fmt.Errorf("devicebus: schema: %w", err)
		}
	}
	if _, err := db.Exec(schema + fmt.Sprintf("PRAGMA user_version = %d;", schemaVersion)); err != nil {
		db.Close()
		return nil, fmt.Errorf("devicebus: schema: %w", err)
	}
	return &store{db: db}, nil
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func inTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func decodeEnvelopes(rows *sql.Rows) ([]busproto.Envelope, error) {
	defer rows.Close()
	var out []busproto.Envelope
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var e busproto.Envelope
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// take leases the session's queued, unexpired messages to one hook and
// returns them in delivery order, as many as fit in lim (always at least
// one when any is queued), each with its Attempt. The read and the update
// run in one transaction on the store's single connection, so of several
// callers for one session each message goes to exactly one.
//
// While a lease of the session is outstanding, take returns nothing: a
// message newer than a leased one must not reach the session before it,
// should that lease expire and the leased message be offered again.
// Expired leases are settled first (expireLeases).
func (s *store) take(ctx context.Context, session, agent string, now time.Time, lim Limit, lease time.Duration, maxAttempts int) ([]busproto.Envelope, error) {
	var out []busproto.Envelope
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := expireLeases(ctx, tx, now, maxAttempts); err != nil {
			return err
		}
		var leased int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM devbus_messages WHERE to_session=? AND (?='' OR to_agent=?) AND state='leased'`,
			session, agent, agent).Scan(&leased); err != nil {
			return err
		}
		if leased > 0 {
			return nil
		}
		rows, err := tx.QueryContext(ctx, `SELECT envelope,attempts FROM devbus_messages
			WHERE to_session=? AND (?='' OR to_agent=?) AND state='queued' AND expires_at>?`, session, agent, agent, ms(now))
		if err != nil {
			return err
		}
		all, err := decodeLeasable(rows)
		if err != nil {
			return err
		}
		slices.SortFunc(all, func(a, b busproto.Envelope) int {
			if c := a.Sent.Compare(b.Sent); c != 0 {
				return c
			}
			return int(a.Seq - b.Seq)
		})
		out = fit(all, lim)
		for _, e := range out {
			if _, err := tx.ExecContext(ctx, `UPDATE devbus_messages SET state='leased', attempts=attempts+1, lease_until=?
				WHERE id=? AND state='queued'`, ms(now.Add(lease)), e.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// decodeLeasable decodes (envelope, attempts) rows; each envelope's
// Attempt is the lease it is about to get.
func decodeLeasable(rows *sql.Rows) ([]busproto.Envelope, error) {
	defer rows.Close()
	var out []busproto.Envelope
	for rows.Next() {
		var raw string
		var attempts int
		if err := rows.Scan(&raw, &attempts); err != nil {
			return nil, err
		}
		var e busproto.Envelope
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			return nil, err
		}
		e.Attempt = attempts + 1
		out = append(out, e)
	}
	return out, rows.Err()
}

// expireLeases settles the leases that ended by now without a
// confirmation: a message leased fewer than maxAttempts times is queued
// again (the next hook gets it, marked as a redelivery); one leased
// maxAttempts times becomes undelivered (ReasonUnconfirmed), and one from
// the server owes the server that report. It returns how many became
// undelivered.
func expireLeases(ctx context.Context, x execer, now time.Time, maxAttempts int) (int64, error) {
	res, err := x.ExecContext(ctx, `UPDATE devbus_messages SET state='undelivered', reason=?, lease_until=NULL,
			ack=CASE WHEN origin='server' THEN 'report' ELSE '' END
		WHERE state='leased' AND lease_until<=? AND attempts>=?`, busproto.ReasonUnconfirmed, ms(now), maxAttempts)
	if err != nil {
		return 0, err
	}
	gone, _ := res.RowsAffected()
	_, err = x.ExecContext(ctx, `UPDATE devbus_messages SET state='queued', lease_until=NULL WHERE state='leased' AND lease_until<=?`, ms(now))
	return gone, err
}

// confirm records that a hook of the session printed these messages:
// leased, they are delivered, and one from the server owes a receipt. A
// confirmation that arrives after its lease ended (the message is queued
// again and no hook took it yet) still counts: the print happened. It
// returns how many it confirmed.
func (s *store) confirm(ctx context.Context, session string, ids []string, now time.Time) (int, error) {
	n := 0
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		for _, id := range ids {
			res, err := tx.ExecContext(ctx, `UPDATE devbus_messages SET state='delivered', delivered_at=?, lease_until=NULL,
					ack=CASE WHEN origin='server' THEN 'owed' ELSE '' END
				WHERE id=? AND to_session=? AND attempts>0 AND state IN ('leased','queued')`, ms(now), id, session)
			if err != nil {
				return err
			}
			if c, _ := res.RowsAffected(); c > 0 {
				n++
			}
		}
		return nil
	})
	return n, err
}

// fit is the longest prefix of msgs within lim, and never empty when msgs
// is not.
func fit(msgs []busproto.Envelope, lim Limit) []busproto.Envelope {
	size := lim.Size
	if size == nil {
		size = func(e busproto.Envelope) int {
			n := len(e.Body)
			for _, r := range e.Refs {
				n += len(r)
			}
			return n
		}
	}
	used := 0
	for i, e := range msgs {
		if lim.Count > 0 && i == lim.Count {
			return msgs[:i]
		}
		if lim.Bytes > 0 {
			n := size(e)
			if i > 0 {
				n += lim.Sep
			}
			if i > 0 && used+n > lim.Bytes {
				return msgs[:i]
			}
			used += n
		}
	}
	return msgs
}

// untake gives leased messages back unused (Requeue): their caller never
// received them, so they are queued again and the lease does not count as
// an attempt.
func (s *store) untake(ctx context.Context, ids []string) error {
	return inTx(ctx, s.db, func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `UPDATE devbus_messages SET state='queued', lease_until=NULL, attempts=max(attempts-1,0)
				WHERE id=? AND state='leased'`, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// reconcile folds one poll's deliverable set into the inbox. A message in
// the set is added (queued) or kept as it is. A queued message missing
// from it is dropped: it was delivered elsewhere, expired, or held again.
// A leased one stays (a hook may be printing it; if its lease ends it is
// queued and the next poll drops it). A delivered or undelivered one stays
// until it expires, so a message the server lists
// again later (held, then released) is acknowledged again rather than
// delivered twice. keep names claimable ids, which stay too.
func (s *store) reconcile(ctx context.Context, set []busproto.Envelope, keep []string, now time.Time) error {
	return inTx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE devbus_messages SET listed=0 WHERE origin='server'`); err != nil {
			return err
		}
		for _, e := range set {
			if err := upsertServer(ctx, tx, e); err != nil {
				return err
			}
		}
		for _, id := range keep {
			if _, err := tx.ExecContext(ctx, `UPDATE devbus_messages SET listed=1 WHERE id=? AND origin='server'`, id); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM devbus_messages WHERE origin='server' AND listed=0 AND state='queued'`)
		return err
	})
}

// has reports whether the inbox holds a message.
func (s *store) has(ctx context.Context, id string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM devbus_messages WHERE id=?`, id).Scan(&n)
	return n > 0, err
}

// addClaimed stores an @user message this device claimed for one of its
// sessions.
func (s *store) addClaimed(ctx context.Context, e busproto.Envelope) error {
	return upsertServer(ctx, s.db, e)
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// upsertServer adds a message from the server as queued, or marks one the
// inbox holds as listed. A delivered (or undelivered) message listed again
// was never acknowledged (or reported) as far as the server knows: its ack
// (or report) is owed again.
func upsertServer(ctx context.Context, x execer, e busproto.Envelope) error {
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = x.ExecContext(ctx, `INSERT INTO devbus_messages(id,origin,seq,to_session,to_agent,from_session,from_agent,thread_id,envelope,state,created_at,expires_at)
		VALUES(?,'server',?,?,?,?,?,?,?,'queued',?,?)
		ON CONFLICT(id) DO UPDATE SET listed=1, envelope=excluded.envelope, to_session=excluded.to_session, to_agent=excluded.to_agent,
			expires_at=excluded.expires_at, ack=CASE WHEN state='delivered' THEN 'owed' WHEN state='undelivered' THEN 'report' ELSE ack END
		WHERE origin='server'`,
		e.ID, e.Seq, e.ToSession, e.ToAgent, e.From, e.FromAgent, e.ThreadID, string(raw), ms(e.Sent), ms(e.ExpiresAt))
	return err
}

// owed returns up to n message ids whose delivery (ack 'owed') or
// undelivered report (ack 'report') the server has not taken yet.
func (s *store) owed(ctx context.Context, ack string, n int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM devbus_messages WHERE ack=? ORDER BY delivered_at, id LIMIT ?`, ack, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// acked records the server's answer to an ack: acked ids are settled,
// rejected ones are not deliverable by this device and are not sent again.
// Both keep their state until they expire (see reconcile).
func (s *store) acked(ctx context.Context, acked, rejected []string) error {
	return inTx(ctx, s.db, func(tx *sql.Tx) error {
		for _, l := range []struct {
			ids []string
			to  string
		}{{acked, "done"}, {rejected, "rejected"}} {
			for _, id := range l.ids {
				if _, err := tx.ExecContext(ctx, `UPDATE devbus_messages SET ack=? WHERE id=? AND ack IN ('owed','report')`, l.to, id); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// Retention past expiry: a server message is kept an hour (a tombstone
// against delivering it twice), a local one a week (the local inbox).
const (
	serverKeep = time.Hour
	localKeep  = 7 * 24 * time.Hour
)

// purge drops messages past their retention.
func (s *store) purge(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM devbus_messages WHERE (origin='server' AND expires_at<? AND ack NOT IN ('owed','report')) OR (origin='local' AND expires_at<?)`,
		ms(now.Add(-serverKeep)), ms(now.Add(-localKeep)))
	return err
}

// counts is the inbox at a glance, for status: pending counts queued and
// leased messages (a lease is not a delivery yet), owed the receipts and
// reports the server has not taken.
type counts struct {
	pending, owed int
}

func (s *store) counts(ctx context.Context, now time.Time) (counts, error) {
	var c counts
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM devbus_messages WHERE state IN ('queued','leased') AND expires_at>?),
		(SELECT count(*) FROM devbus_messages WHERE ack IN ('owed','report'))`, ms(now)).Scan(&c.pending, &c.owed)
	return c, err
}

var errNoRows = sql.ErrNoRows

func isNoRows(err error) bool { return errors.Is(err, errNoRows) }

// notice returns which of held were not noticed since now-every and
// records them noticed now, in one transaction: of two hooks asking at
// once, one gets each sender.
func (s *store) notice(ctx context.Context, held []busproto.HeldSender, now time.Time, every time.Duration) ([]busproto.HeldSender, error) {
	var out []busproto.HeldSender
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		for _, h := range held {
			res, err := tx.ExecContext(ctx, `INSERT INTO devbus_notices(sender_user,noticed_at) VALUES(?,?)
				ON CONFLICT(sender_user) DO UPDATE SET noticed_at=excluded.noticed_at WHERE noticed_at<=?`, h.UserID, ms(now), ms(now.Add(-every)))
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				out = append(out, h)
			}
		}
		return nil
	})
	return out, err
}
