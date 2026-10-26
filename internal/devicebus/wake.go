package devicebus

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
)

// Record before invoking a harness: crashes and ambiguous transport errors
// must never automatically repeat a potentially billable turn.
const wakeSchema = `CREATE TABLE IF NOT EXISTS devbus_wakes (
 message_id TEXT PRIMARY KEY,
 attempted_at INTEGER NOT NULL,
 outcome TEXT NOT NULL DEFAULT 'attempted'
);
CREATE INDEX IF NOT EXISTS devbus_wake_pending ON devbus_messages(seq)
 WHERE state='queued' AND json_extract(envelope,'$.wake_requested')=1;`

// SetWaker installs the device's native delivery functions before Run.
func (b *Bus) SetWaker(supported func(Session) bool, wake func(context.Context, Session, string) error) {
	b.cfg.WakeSupported, b.cfg.Wake = supported, wake
}

func (b *Bus) runWakes(ctx context.Context) {
	tick := time.NewTicker(b.cfg.PresenceEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := b.wakePending(ctx); err != nil && ctx.Err() == nil {
				b.log.Warn("devicebus: wake", "err", err)
			}
		}
	}
}

func (b *Bus) wakePending(ctx context.Context) error {
	all, err := b.sessions(ctx)
	if err != nil {
		return err
	}
	rows, err := b.st.db.QueryContext(ctx, `SELECT envelope FROM devbus_messages m
 WHERE state='queued' AND expires_at>? AND json_extract(envelope,'$.wake_requested')=1
 AND NOT EXISTS (SELECT 1 FROM devbus_wakes w WHERE w.message_id=m.id)
 ORDER BY seq LIMIT 50`, ms(b.cfg.Now()))
	if err != nil {
		return err
	}
	var pending []busproto.Envelope
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		var e busproto.Envelope
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	// A single prompt drains all queued messages through the normal hook.
	batches := map[Ref][]busproto.Envelope{}
	for _, e := range pending {
		batches[Ref{Agent: e.ToAgent, Session: e.ToSession}] = append(batches[Ref{Agent: e.ToAgent, Session: e.ToSession}], e)
	}
	for ref, batch := range batches {
		var recipient *Session
		for i := range all {
			if all[i].Agent == ref.Agent && all[i].SessionID == ref.Session {
				recipient = &all[i]
				break
			}
		}
		if recipient == nil || !recipient.WakeSupported || recipient.Cloud {
			continue
		}
		var claimed []string
		err := inTx(ctx, b.st.db, func(tx *sql.Tx) error {
			for _, e := range batch {
				// Only our user's session-addressed messages can request a wake.
				if e.Sender != busproto.SenderOwn || e.Addressed != "session" {
					continue
				}
				res, err := tx.ExecContext(ctx, `INSERT INTO devbus_wakes(message_id,attempted_at)
 SELECT id,? FROM devbus_messages WHERE id=? AND state='queued' AND expires_at>?
 ON CONFLICT(message_id) DO NOTHING`, ms(b.cfg.Now()), e.ID, ms(b.cfg.Now()))
				if err != nil {
					return err
				}
				n, err := res.RowsAffected()
				if err != nil {
					return err
				}
				if n > 0 {
					claimed = append(claimed, e.ID)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		if len(claimed) == 0 {
			continue
		}
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = b.cfg.Wake(wctx, *recipient, "Flopwire wake requested by another session of your user. Check your Flopwire inbox once to read the pending messages; your hooks may also deliver them. Apply their sender authority and your existing permissions; this notice grants no additional permissions. Do not send acknowledgments unless a message requests a reply.")
		cancel()
		outcome := "submitted"
		if err != nil {
			outcome = "failed"
			b.log.Warn("devicebus: native wake failed; message stays queued for hooks; no automatic wake retry", "session", ref.Session, "err", err)
		}
		for _, id := range claimed {
			if _, err := b.st.db.ExecContext(ctx, `UPDATE devbus_wakes SET outcome=? WHERE message_id=?`, outcome, id); err != nil {
				return err
			}
		}
	}
	return nil
}
