package devicebus

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
)

const failureSchema = `CREATE TABLE IF NOT EXISTS devbus_failures (
 id TEXT PRIMARY KEY, origin TEXT NOT NULL, session_id TEXT NOT NULL, agent TEXT NOT NULL,
 state TEXT NOT NULL, reason TEXT NOT NULL, created_at INTEGER NOT NULL,
 server_token TEXT NOT NULL DEFAULT '', valid_until INTEGER,
 lease_id TEXT NOT NULL DEFAULT '', lease_until INTEGER, attempts INTEGER NOT NULL DEFAULT 0,
 confirmed_at INTEGER, ack TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS devbus_failure_sender ON devbus_failures(session_id,agent,confirmed_at,created_at);
CREATE INDEX IF NOT EXISTS devbus_failure_ack ON devbus_failures(ack) WHERE ack='owed';`

// Capture local failures before their envelopes can be purged. Server
// failures arrive as metadata in Poll; recipient envelopes never create
// notices on the wrong device. INSERT OR IGNORE also retains acknowledgement.
func captureLocalFailures(ctx context.Context, x execer, now time.Time) error {
	_, err := x.ExecContext(ctx, `INSERT OR IGNORE INTO devbus_failures(id,origin,session_id,agent,state,reason,created_at)
 SELECT id,'local',from_session,from_agent,
 CASE WHEN state='undelivered' THEN state ELSE 'expired' END,
 CASE WHEN state='undelivered' THEN reason ELSE 'expired' END,created_at
 FROM devbus_messages WHERE origin='local' AND
 (state='undelivered' OR (state IN ('queued','leased') AND expires_at<=? AND (state<>'leased' OR lease_until<=?)))`, ms(now), ms(now))
	return err
}

// TakeFailures leases a bounded batch to one normal hook. No network and
// no wake; an idle sender's statuses wait durably until it runs again.
func (b *Bus) TakeFailures(ctx context.Context, session, agent string) ([]busproto.DeliveryFailure, error) {
	if session == "" {
		return nil, fmt.Errorf("failure notice: session required")
	}
	now := b.cfg.Now()
	var out []busproto.DeliveryFailure
	err := inTx(ctx, b.st.db, func(tx *sql.Tx) error {
		if err := captureLocalFailures(ctx, tx, now); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT id,session_id,agent,state,reason,attempts FROM devbus_failures
   WHERE session_id=? AND (?='' OR agent=?) AND confirmed_at IS NULL
   AND (lease_until IS NULL OR lease_until<=?)
   AND (origin='local' OR valid_until>?) ORDER BY created_at,id LIMIT 10`, session, agent, agent, ms(now), ms(now.Add(b.cfg.Lease)))
		if err != nil {
			return err
		}
		for rows.Next() {
			var n busproto.DeliveryFailure
			if err := rows.Scan(&n.ID, &n.Session, &n.Agent, &n.State, &n.Reason, &n.Attempt); err != nil {
				rows.Close()
				return err
			}
			out = append(out, n)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for i := range out {
			token := newID()
			n := &out[i]
			n.LeaseID = "status:" + token
			n.Attempt++
			if _, err := tx.ExecContext(ctx, `UPDATE devbus_failures SET lease_id=?,lease_until=?,attempts=? WHERE id=?`, n.LeaseID, ms(now.Add(b.cfg.Lease)), n.Attempt, n.ID); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// ConfirmFailures accepts only the current hook token of this sender.
// A late confirmation cannot acknowledge a newer lease. Confirmed notices
// never print again locally, including while their server ack is retrying.
func (b *Bus) ConfirmFailures(ctx context.Context, session string, ids []string) error {
	err := inTx(ctx, b.st.db, func(tx *sql.Tx) error {
		for _, id := range ids {
			if !strings.HasPrefix(id, "status:") {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE devbus_failures SET confirmed_at=?,ack=CASE WHEN origin='server' THEN 'owed' ELSE 'done' END
    WHERE session_id=? AND lease_id=? AND confirmed_at IS NULL AND lease_until>?`, ms(b.cfg.Now()), session, id, ms(b.cfg.Now())); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil && !b.Local() {
		b.kickAcks()
	}
	return err
}

func (s *store) importFailures(ctx context.Context, in []busproto.DeliveryFailure, now time.Time) error {
	return inTx(ctx, s.db, func(tx *sql.Tx) error {
		for _, n := range in {
			if n.ID == "" || len(n.ID) > 64 || n.Token == "" || len(n.Token) > 64 || n.Session == "" || len(n.Session) > 256 || n.Agent == "" || len(n.Agent) > 64 || n.ValidUntil.IsZero() ||
				(n.State != busproto.StateUndelivered && n.State != busproto.StateExpired) || len(n.Reason) > 64 {
				return fmt.Errorf("invalid delivery status")
			}
			valid := ms(n.ValidUntil)
			_, err := tx.ExecContext(ctx, `INSERT INTO devbus_failures(id,origin,session_id,agent,state,reason,created_at,server_token,valid_until)
    VALUES(?,'server',?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET
    server_token=excluded.server_token,valid_until=excluded.valid_until,
    ack=CASE WHEN confirmed_at IS NOT NULL AND (server_token<>excluded.server_token OR ack NOT IN ('done','rejected')) THEN 'owed' ELSE ack END`, n.ID, n.Session, n.Agent, n.State, n.Reason, ms(now), n.Token, valid)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *store) failureAcks(ctx context.Context, limit int) ([]busproto.FailureAck, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,server_token FROM devbus_failures WHERE ack='owed' ORDER BY created_at,id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []busproto.FailureAck
	for rows.Next() {
		var n busproto.FailureAck
		if err := rows.Scan(&n.ID, &n.Token); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *store) failureAcked(ctx context.Context, ids []string, sent []busproto.FailureAck) error {
	return inTx(ctx, s.db, func(tx *sql.Tx) error {
		for _, id := range ids {
			for _, n := range sent {
				if id == n.ID {
					if _, err := tx.ExecContext(ctx, `UPDATE devbus_failures SET ack='done' WHERE id=? AND server_token=?`, id, n.Token); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
}

func (s *store) failureRejected(ctx context.Context, ids []string, sent []busproto.FailureAck) error {
	return inTx(ctx, s.db, func(tx *sql.Tx) error {
		for _, id := range ids {
			for _, n := range sent {
				if id == n.ID {
					if _, err := tx.ExecContext(ctx, `UPDATE devbus_failures SET ack='rejected' WHERE id=? AND server_token=?`, id, n.Token); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
}
