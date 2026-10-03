package devicebus

// Ended sessions (issues #67 and #82). A session ends when its harness
// says so: a SessionEnd hook (End), or its registry entry no longer names
// a running process (Observe). Idleness never ends a session, nor does
// the hook busy cap. An ended session drops out of presence at once, and
// the messages it held (queued for it, or claimed for it from @user) are
// undelivered with reason session_ended, reported to the server like
// unconfirmed ones. They are never given to another session.
//
// A message that reaches the inbox after its session ended is marked the
// same way only when it was sent while the session could still look live
// to its sender. A later one was sent to a session the sender was told is
// not running ("only_if_resumed"): it stays queued for a resume, as the
// receipt said. Without a server the receipt came from this device's own
// presence, at most presenceFresh old: a message is marked when it was
// sent within presenceFresh of the last live sighting, which matches the
// receipt exactly. With a server the receipt came from the server's copy
// of presence, which lags by a presence tick and a poll, on another
// clock: EndGrace.

import (
	"context"
	"database/sql"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
)

// Ref names one session of one harness.
type Ref struct {
	Agent   string
	Session string
}

// Holder is the process a registry names as holding a session open.
type Holder struct {
	// ID tells one holding process from another: a pid with its start
	// time, or "lock" where only a lock is known.
	ID string
	// Start is when the process started; zero when unknown.
	Start time.Time
}

// Registry is what the harness registries say now.
type Registry struct {
	// Held: sessions a running process holds open, with that process.
	Held map[Ref]Holder
	// Gone: sessions whose registry entry names a process that is no
	// longer running (a dead or reused pid, an unlocked writer lock).
	Gone []Ref
	// Unknown: entries that could not be read this time; their sessions
	// keep what was last observed.
	Unknown []Ref
	// Read: the harnesses whose registry was read. A session of another
	// harness that was held before is not taken as ended for its absence.
	Read map[string]bool
}

const (
	// EndDebounce: a session held before whose registry entry is missing
	// (not dead: missing) ends only when it is still missing at a read at
	// least this long after the first. A harness that rewrites its entry
	// must not end the session.
	EndDebounce = time.Second
	// EndGrace: a message from the server sent up to this long after the
	// last time the device saw its session live is marked session_ended
	// when the session ended. The server lists a session as live until
	// the device's next poll after the end (a presence tick, about 2 s),
	// and the server's clock may differ from the device's. A message sent
	// in the rest of this window was told only_if_resumed and is marked
	// anyway: undelivered, visibly, rather than waiting for a resume.
	EndGrace = 10 * time.Second
	// heldRefresh: how stale held_at may get before a read of the same
	// holder writes it again (it only dates the row for purge). Presence is
	// read every 2 s; most reads then write nothing.
	heldRefresh = time.Minute
)

// Observe records what the harness registries say (see Registry) and
// returns the sessions that are ended now. A held session that was ended
// is live again (a resume) when the registry had shown it gone since, or
// when the process holding it now started after the end (or, its start
// unknown, is not the one that held it at the end). Messages of sessions
// that ended are marked undelivered and their reports queued.
func (b *Bus) Observe(ctx context.Context, r Registry) (map[Ref]bool, error) {
	ended, n, err := b.st.observe(ctx, r, b.cfg.Now(), EndDebounce)
	if err != nil {
		return nil, err
	}
	b.endedMarked(n)
	return ended, nil
}

// End records that the session ended at at: its SessionEnd hook started
// then. An empty Agent ends the session id of any harness.
func (b *Bus) End(ctx context.Context, ref Ref, at time.Time) error {
	if ref.Session == "" {
		return nil
	}
	n, err := b.st.end(ctx, ref, at, b.cfg.Now())
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.presence = presenceCache{} // the next presence leaves it out
	b.mu.Unlock()
	b.endedMarked(n)
	return nil
}

// Revive records that a hook of the session that started at at ran
// (SessionStart, a prompt, a tool call, a Stop): an end recorded before
// at was followed by a resume. An end at or after at stands: that hook's
// event came before the end and arrived late.
func (b *Bus) Revive(ctx context.Context, session string, at time.Time) error {
	if session == "" {
		return nil
	}
	_, err := b.st.db.ExecContext(ctx, `UPDATE devbus_sessions SET ended_at=NULL, ended_by='', ended_holder=''
		WHERE session_id=? AND ended_at IS NOT NULL AND ended_at<?`, session, ms(at))
	return err
}

// settleEnded marks the messages of ended sessions (the loop's tick: a
// message may arrive, or a lease end, after its session ended).
func (b *Bus) settleEnded(ctx context.Context) {
	n, err := settleEnded(ctx, b.st.db, b.cfg.Now())
	if err != nil {
		if ctx.Err() == nil {
			b.log.Warn("devicebus: ended sessions", "err", err)
		}
		return
	}
	b.endedMarked(n)
}

func (b *Bus) endedMarked(n int64) {
	if n == 0 {
		return
	}
	b.log.Info("devicebus: messages undelivered: their session ended before a hook delivered them", "count", n)
	if !b.Local() {
		b.kickAcks()
	}
}

func (s *store) observe(ctx context.Context, r Registry, now time.Time, debounce time.Duration) (map[Ref]bool, int64, error) {
	ended := map[Ref]bool{}
	var marked int64
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		for ref, h := range r.Held {
			var endedAt sql.NullInt64
			var endedBy, endedHolder string
			err := tx.QueryRowContext(ctx, `SELECT ended_at,ended_by,ended_holder FROM devbus_sessions WHERE agent=? AND session_id=?`,
				ref.Agent, ref.Session).Scan(&endedAt, &endedBy, &endedHolder)
			if err != nil && !isNoRows(err) {
				return err
			}
			revive := endedAt.Valid && (endedBy == "registry" ||
				!h.Start.IsZero() && ms(h.Start) > endedAt.Int64 ||
				h.Start.IsZero() && endedHolder != "" && endedHolder != h.ID)
			if _, err := tx.ExecContext(ctx, `INSERT INTO devbus_sessions(agent,session_id,holder,held_at) VALUES(?,?,?,?)
				ON CONFLICT(agent,session_id) DO UPDATE SET holder=excluded.holder, held_at=excluded.held_at, missing_since=NULL,
					ended_at=CASE WHEN ? THEN NULL ELSE ended_at END, ended_by=CASE WHEN ? THEN '' ELSE ended_by END,
					ended_holder=CASE WHEN ? THEN '' ELSE ended_holder END
				WHERE ? OR holder<>excluded.holder OR missing_since IS NOT NULL OR COALESCE(held_at,0)<?`,
				ref.Agent, ref.Session, h.ID, ms(now), revive, revive, revive, revive, ms(now.Add(-heldRefresh))); err != nil {
				return err
			}
		}
		skip := map[Ref]bool{}
		for _, ref := range r.Unknown {
			skip[ref] = true
		}
		gone := map[Ref]bool{}
		for _, ref := range r.Gone {
			if _, held := r.Held[ref]; held || skip[ref] {
				continue
			}
			gone[ref] = true
			// A dead entry ends the session at once, held before or not
			// (an entry left by a process killed while the agent was not
			// running).
			if _, err := tx.ExecContext(ctx, `INSERT INTO devbus_sessions(agent,session_id,ended_at,ended_by) VALUES(?,?,?,'registry')
				ON CONFLICT(agent,session_id) DO UPDATE SET holder='', missing_since=NULL, ended_holder='',
					ended_at=COALESCE(ended_at,excluded.ended_at), ended_by='registry'
				WHERE holder<>'' OR missing_since IS NOT NULL OR ended_at IS NULL OR ended_by<>'registry' OR ended_holder<>''`,
				ref.Agent, ref.Session, ms(now)); err != nil {
				return err
			}
		}
		// Held before and missing now, in a registry that was read.
		rows, err := tx.QueryContext(ctx, `SELECT agent,session_id,missing_since FROM devbus_sessions WHERE holder<>''`)
		if err != nil {
			return err
		}
		type miss struct {
			ref   Ref
			since sql.NullInt64
		}
		var missing []miss
		for rows.Next() {
			var m miss
			if err := rows.Scan(&m.ref.Agent, &m.ref.Session, &m.since); err != nil {
				rows.Close()
				return err
			}
			if _, held := r.Held[m.ref]; held || skip[m.ref] || gone[m.ref] || !r.Read[m.ref.Agent] {
				continue
			}
			missing = append(missing, m)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, m := range missing {
			q, args := `UPDATE devbus_sessions SET missing_since=? WHERE agent=? AND session_id=?`, []any{ms(now), m.ref.Agent, m.ref.Session}
			if m.since.Valid && now.Sub(time.UnixMilli(m.since.Int64)) >= debounce {
				q, args = `UPDATE devbus_sessions SET holder='', missing_since=NULL, ended_holder='',
					ended_at=COALESCE(ended_at,?), ended_by='registry'
					WHERE agent=? AND session_id=?`, []any{ms(now), m.ref.Agent, m.ref.Session}
			} else if m.since.Valid {
				continue
			}
			if _, err := tx.ExecContext(ctx, q, args...); err != nil {
				return err
			}
		}
		n, err := settleEnded(ctx, tx, now)
		if err != nil {
			return err
		}
		marked = n
		rows, err = tx.QueryContext(ctx, `SELECT agent,session_id FROM devbus_sessions WHERE ended_at IS NOT NULL`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ref Ref
			if err := rows.Scan(&ref.Agent, &ref.Session); err != nil {
				return err
			}
			ended[ref] = true
		}
		return rows.Err()
	})
	return ended, marked, err
}

// end records a SessionEnd at at. The process that holds the session now
// is the one ending; only another one revives it (observe).
func (s *store) end(ctx context.Context, ref Ref, at, now time.Time) (int64, error) {
	var marked int64
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		if ref.Agent != "" {
			if _, err := tx.ExecContext(ctx, `INSERT INTO devbus_sessions(agent,session_id) VALUES(?,?) ON CONFLICT DO NOTHING`, ref.Agent, ref.Session); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE devbus_sessions SET ended_at=?, ended_by='hook', ended_holder=holder
			WHERE session_id=? AND (?='' OR agent=?) AND (ended_at IS NULL OR ended_at>?)`,
			ms(at), ref.Session, ref.Agent, ref.Agent, ms(at)); err != nil {
			return err
		}
		n, err := settleEnded(ctx, tx, now)
		marked = n
		return err
	})
	return marked, err
}

// noteLive records that the sessions are live now (in presence).
func (s *store) noteLive(ctx context.Context, all []Session, now time.Time) error {
	if len(all) == 0 {
		return nil
	}
	return inTx(ctx, s.db, func(tx *sql.Tx) error {
		for _, v := range all {
			if _, err := tx.ExecContext(ctx, `INSERT INTO devbus_sessions(agent,session_id,live_at) VALUES(?,?,?)
				ON CONFLICT(agent,session_id) DO UPDATE SET live_at=excluded.live_at`, v.Agent, v.SessionID, ms(now)); err != nil {
				return err
			}
		}
		return nil
	})
}

// settleEnded marks undelivered (ReasonSessionEnded) the queued, unexpired
// messages of ended sessions that were sent no later than EndGrace after
// the device last saw the session live (see the file comment), and queues
// the reports of those from the server. A leased message is left to its
// lease: the hook may be printing it, and a confirmation still counts. It
// returns how many it marked.
func settleEnded(ctx context.Context, x execer, now time.Time) (int64, error) {
	res, err := x.ExecContext(ctx, `UPDATE devbus_messages SET state='undelivered', reason=?, lease_until=NULL,
			ack=CASE WHEN origin='server' THEN 'report' ELSE '' END
		WHERE state='queued' AND to_session<>'' AND expires_at>? AND EXISTS(SELECT 1 FROM devbus_sessions s
			WHERE s.agent=devbus_messages.to_agent AND s.session_id=devbus_messages.to_session AND s.ended_at IS NOT NULL
				AND s.live_at IS NOT NULL AND devbus_messages.created_at<=s.live_at+CASE WHEN devbus_messages.origin='local' THEN ? ELSE ? END)`,
		busproto.ReasonSessionEnded, ms(now), presenceFresh.Milliseconds(), EndGrace.Milliseconds())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// reports returns up to n message ids whose undelivered report the server
// has not taken, split by reason.
func (s *store) reports(ctx context.Context, n int) (unconfirmed, ended []string, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,reason FROM devbus_messages WHERE ack='report' ORDER BY id LIMIT ?`, n)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, reason string
		if err := rows.Scan(&id, &reason); err != nil {
			return nil, nil, err
		}
		if reason == busproto.ReasonSessionEnded {
			ended = append(ended, id)
		} else {
			unconfirmed = append(unconfirmed, id)
		}
	}
	return unconfirmed, ended, rows.Err()
}
