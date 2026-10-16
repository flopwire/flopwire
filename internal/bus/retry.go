package bus

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/jackc/pgx/v5"
)

// prepareRetry runs under the ordinary send locks, then locks its source.
// All request identity and content are validated before replaying a receipt.
func prepareRetry(ctx context.Context, tx pgx.Tx, c busproto.Caller, req busproto.SendRequest, m *message, out *busproto.SendResponse) (bool, error) {
	var fromUser, fromDevice, fromAgent, fromSession, toUser, toAgent, toSession, addressed, body, intent, reply, thread, state, reason string
	var refs []string
	err := tx.QueryRow(ctx, `SELECT from_user::text,COALESCE(from_device::text,''),from_agent,from_session,to_user::text,COALESCE(to_agent,''),COALESCE(to_session,''),addressed,body,intent,refs,COALESCE(reply_to,''),thread_id,state,reason FROM bus_messages WHERE id=$1 FOR UPDATE`, req.RetryOf).Scan(&fromUser, &fromDevice, &fromAgent, &fromSession, &toUser, &toAgent, &toSession, &addressed, &body, &intent, &refs, &reply, &thread, &state, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, fail(http.StatusNotFound, busproto.CodeNotFound, "retry_of: source not found")
	}
	if err != nil {
		return false, err
	}
	if fromUser != c.UserID {
		return false, fail(http.StatusNotFound, busproto.CodeNotFound, "retry_of: source not found")
	}
	if fromDevice != c.DeviceID || fromAgent != m.from.agent || fromSession != m.from.id || addressed != "session" || toUser != m.toUser || toAgent != m.toAgent || toSession != m.toSession || body != m.body || intent != string(m.intent) || !slices.Equal(refs, m.refs) || reply != strings.TrimSpace(req.ReplyTo) {
		return false, badRequest("retry_of: sender, recipient and message content must match the source")
	}
	if state != string(busproto.StateUndelivered) || reason != busproto.ReasonSessionEnded {
		return false, badRequest("retry_of: source must be undelivered with reason session_ended")
	}
	var successor string
	err = tx.QueryRow(ctx, `SELECT successor_id FROM bus_retries WHERE source_id=$1`, req.RetryOf).Scan(&successor)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if successor != "" {
		var e busproto.Envelope
		var successorState busproto.State
		var successorReason string
		e, err = scanEnvelope(tx.QueryRow(ctx, `SELECT `+envCols+`,m.state,m.reason FROM `+envFrom+` WHERE m.id=$1`, successor), &successorState, &successorReason)
		if err == nil {
			out.ID, out.ThreadID, out.State, out.Sender, out.Intent, out.Sent, out.ExpiresAt = e.ID, e.ThreadID, successorState, e.Sender, e.Intent, e.Sent, e.ExpiresAt
			out.RetryOf, out.Replayed, out.Reason, out.Admission = req.RetryOf, true, successorReason, e.Admission
			return true, nil
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return false, fail(http.StatusNotFound, busproto.CodeNotFound, "retry_of: successor no longer available")
		}
		return false, err
	}
	m.retryOf, m.thread = req.RetryOf, thread
	// Only validated failed ancestry is exempt from ordinary duplicate matching.
	rows, err := tx.Query(ctx, `WITH RECURSIVE ancestry AS (SELECT id,retry_of FROM bus_messages WHERE id=$1 AND state='undelivered' AND reason='session_ended' UNION SELECT p.id,p.retry_of FROM bus_messages p JOIN ancestry a ON p.id=a.retry_of WHERE p.state='undelivered' AND p.reason='session_ended') SELECT id FROM ancestry`, req.RetryOf)
	if err != nil {
		return false, err
	}
	m.retryAncestors, err = pgx.CollectRows(rows, pgx.RowTo[string])
	return false, err
}
