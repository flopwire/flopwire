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
  state         TEXT NOT NULL,              -- queued, delivered, refused (local)
  refuse_reason TEXT NOT NULL DEFAULT '',
  created_at    INTEGER NOT NULL,           -- unix ms
  expires_at    INTEGER NOT NULL,
  delivered_at  INTEGER,
  ack           TEXT NOT NULL DEFAULT '',   -- server: '' none owed, owed, done, rejected
  listed        INTEGER NOT NULL DEFAULT 1  -- server: in the last poll's set
);
CREATE INDEX IF NOT EXISTS devbus_to ON devbus_messages (to_session, state);
CREATE INDEX IF NOT EXISTS devbus_ack ON devbus_messages (ack) WHERE ack = 'owed';
CREATE INDEX IF NOT EXISTS devbus_from ON devbus_messages (from_session, created_at);
CREATE INDEX IF NOT EXISTS devbus_thread ON devbus_messages (thread_id, created_at);
CREATE INDEX IF NOT EXISTS devbus_expires ON devbus_messages (expires_at);
`

func openStore(path string) (*store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// One connection: every write is serialized, so a statement that moves
	// a message from queued to delivered runs alone (Pending).
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
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

// take marks the session's undelivered, unexpired messages delivered and
// returns them in delivery order. One statement does both, on the store's
// single connection, so of several callers for one session each message
// goes to exactly one. A message from the server owes an ack.
func (s *store) take(ctx context.Context, session, agent string, now time.Time) ([]busproto.Envelope, error) {
	rows, err := s.db.QueryContext(ctx, `UPDATE devbus_messages SET state='delivered', delivered_at=?,
			ack=CASE WHEN origin='server' THEN 'owed' ELSE '' END
		WHERE to_session=? AND (?='' OR to_agent=?) AND state='queued' AND expires_at>?
		RETURNING envelope`, ms(now), session, agent, agent, ms(now))
	if err != nil {
		return nil, err
	}
	out, err := decodeEnvelopes(rows)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b busproto.Envelope) int {
		if c := a.Sent.Compare(b.Sent); c != 0 {
			return c
		}
		return int(a.Seq - b.Seq)
	})
	return out, nil
}

// untake puts delivered messages back in the queue (Requeue). A receipt
// still owed is withdrawn. One the server already took (the receipt
// batch went out first, after AckDelay) cannot be: the server stops
// listing the message and the next poll drops it, so it is lost as
// before.
func (s *store) untake(ctx context.Context, ids []string) error {
	return inTx(ctx, s.db, func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `UPDATE devbus_messages SET state='queued', delivered_at=NULL,
					ack=CASE WHEN ack='owed' THEN '' ELSE ack END
				WHERE id=? AND state='delivered'`, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// reconcile folds one poll's deliverable set into the inbox. A message in
// the set is added (queued) or kept as it is. A queued message missing
// from it is dropped: it was delivered elsewhere, expired, or held again.
// A delivered one stays until it expires, so a message the server lists
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
// inbox holds as listed. A delivered message listed again was never
// acknowledged as far as the server knows: its ack is owed again.
func upsertServer(ctx context.Context, x execer, e busproto.Envelope) error {
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = x.ExecContext(ctx, `INSERT INTO devbus_messages(id,origin,seq,to_session,to_agent,from_session,from_agent,thread_id,envelope,state,created_at,expires_at)
		VALUES(?,'server',?,?,?,?,?,?,?,'queued',?,?)
		ON CONFLICT(id) DO UPDATE SET listed=1, envelope=excluded.envelope, to_session=excluded.to_session, to_agent=excluded.to_agent,
			expires_at=excluded.expires_at, ack=CASE WHEN state='delivered' AND ack<>'owed' THEN 'owed' ELSE ack END
		WHERE origin='server'`,
		e.ID, e.Seq, e.ToSession, e.ToAgent, e.From, e.FromAgent, e.ThreadID, string(raw), ms(e.Sent), ms(e.ExpiresAt))
	return err
}

// owed returns up to n message ids whose delivery the server has not
// acknowledged yet.
func (s *store) owed(ctx context.Context, n int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM devbus_messages WHERE ack='owed' ORDER BY delivered_at, id LIMIT ?`, n)
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
// Both stay as delivered until they expire (see reconcile).
func (s *store) acked(ctx context.Context, acked, rejected []string) error {
	return inTx(ctx, s.db, func(tx *sql.Tx) error {
		for _, l := range []struct {
			ids []string
			to  string
		}{{acked, "done"}, {rejected, "rejected"}} {
			for _, id := range l.ids {
				if _, err := tx.ExecContext(ctx, `UPDATE devbus_messages SET ack=? WHERE id=? AND ack='owed'`, l.to, id); err != nil {
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
	_, err := s.db.ExecContext(ctx, `DELETE FROM devbus_messages WHERE (origin='server' AND expires_at<? AND ack<>'owed') OR (origin='local' AND expires_at<?)`,
		ms(now.Add(-serverKeep)), ms(now.Add(-localKeep)))
	return err
}

// counts is the inbox at a glance, for status.
type counts struct {
	pending, owed int
}

func (s *store) counts(ctx context.Context, now time.Time) (counts, error) {
	var c counts
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM devbus_messages WHERE state='queued' AND expires_at>?),
		(SELECT count(*) FROM devbus_messages WHERE ack='owed')`, ms(now)).Scan(&c.pending, &c.owed)
	return c, err
}

var errNoRows = sql.ErrNoRows

func isNoRows(err error) bool { return errors.Is(err, errNoRows) }
