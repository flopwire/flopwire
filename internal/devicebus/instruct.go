package devicebus

// The standing instruction (busrender.StandingInstruction) is delivered
// like a message (#101): a hook takes it on lease with the session's
// messages, prints it before them, and confirms it. A session is owed it
// until a hook confirms it, so a SessionStart hook that is killed or late
// leaves it to the session's next UserPromptSubmit or PostToolUse hook.
// While another hook holds the instruction's lease, a hook gets neither
// the instruction nor messages: no message reaches a session before the
// instruction does. A confirmed instruction is owed again after a
// SessionStart with a source other than startup (resume, compact, clear)
// that started after the confirmation: a compaction summarizes it away, and
// whether a resumed context keeps hook output is not something Flopwire
// can see.

import (
	"context"
	"database/sql"
	"time"
)

// Instruction asks TakeWith for the standing instruction too.
type Instruction struct {
	// Source is a SessionStart hook's source (startup, resume, compact,
	// clear); "" for other events.
	Source string
	// HookStart is when the asking hook started.
	HookStart time.Time
	// Bytes is the instruction's cost against Limit.Bytes, its separator
	// included.
	Bytes int
}

// renews reports whether a SessionStart of this source makes a confirmed
// instruction owed again.
func (in *Instruction) renews() bool { return in.Source != "" && in.Source != "startup" }

// ConfirmInstruction records that a hook of the session printed the
// standing instruction. A confirmation after the lease ended counts.
func (b *Bus) ConfirmInstruction(ctx context.Context, session string) error {
	now := ms(b.cfg.Now())
	_, err := b.st.db.ExecContext(ctx, `UPDATE devbus_instruct SET confirmed_at=?, lease_until=NULL, updated_at=? WHERE session_id=? AND attempts>0`,
		now, now, session)
	return err
}

// ReturnInstruction undoes taking the instruction for a hook that never
// received it (Requeue): it is owed as before, and the lease does not
// count as an attempt.
func (b *Bus) ReturnInstruction(ctx context.Context, session string) error {
	_, err := b.st.db.ExecContext(ctx, `UPDATE devbus_instruct SET lease_until=NULL, attempts=max(attempts-1,0), updated_at=?
		WHERE session_id=? AND lease_until IS NOT NULL AND confirmed_at IS NULL`, ms(b.cfg.Now()), session)
	return err
}

// takeInstruction decides, inside take's transaction, whether this hook
// gets the instruction (leased to it now) and whether it may take
// messages: not while the instruction is owed and leased to another hook.
// After maxAttempts unconfirmed leases the instruction is taken as
// delivered (each may have printed with its confirmation lost), so that a
// hook that can never confirm does not print it on every event.
func takeInstruction(ctx context.Context, tx *sql.Tx, session string, in *Instruction, now time.Time, lease time.Duration, maxAttempts int) (instruct, blocked bool, err error) {
	var confirmed, until sql.NullInt64
	var attempts int
	err = tx.QueryRowContext(ctx, `SELECT confirmed_at,lease_until,attempts FROM devbus_instruct WHERE session_id=?`, session).Scan(&confirmed, &until, &attempts)
	if err != nil && !isNoRows(err) {
		return false, false, err
	}
	owed := !confirmed.Valid
	if confirmed.Valid && in.renews() && confirmed.Int64 < ms(in.HookStart) {
		owed, attempts = true, 0
	}
	if !owed {
		return false, false, nil
	}
	// A lease ends at its time, or when it ends further ahead than a
	// clock step back allows (as for messages, expireLeases).
	if until.Valid && until.Int64 > ms(now) && until.Int64 <= ms(now.Add(2*lease)) {
		return false, true, nil
	}
	if attempts >= maxAttempts {
		_, err = tx.ExecContext(ctx, `UPDATE devbus_instruct SET confirmed_at=?, lease_until=NULL, updated_at=? WHERE session_id=?`, ms(now), ms(now), session)
		return false, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO devbus_instruct(session_id,confirmed_at,lease_until,attempts,updated_at) VALUES(?,NULL,?,?,?)
		ON CONFLICT(session_id) DO UPDATE SET confirmed_at=NULL, lease_until=excluded.lease_until, attempts=excluded.attempts, updated_at=excluded.updated_at`,
		session, ms(now.Add(lease)), attempts+1, ms(now))
	return err == nil, false, err
}
