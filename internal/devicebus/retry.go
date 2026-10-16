package devicebus

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"

	"github.com/flopwire/flopwire/internal/busproto"
)

// localRetry validates identity and immutable content before looking up the
// accepted successor. The mapping is inserted in the send transaction only
// after policy checks pass, so refusals leave the source eligible.
func (b *Bus) localRetry(ctx context.Context, tx *sql.Tx, id string, candidate busproto.Envelope, in sendInput) (busproto.Envelope, *busproto.SendResponse, error) {
	var source busproto.Envelope
	var raw, state, reason string
	err := tx.QueryRowContext(ctx, `SELECT envelope,state,reason FROM devbus_messages WHERE id=? AND origin='local'`, id).Scan(&raw, &state, &reason)
	if isNoRows(err) {
		return source, nil, fail(http.StatusNotFound, busproto.CodeNotFound, "retry source not found")
	}
	if err != nil {
		return source, nil, err
	}
	if err = json.Unmarshal([]byte(raw), &source); err != nil {
		return source, nil, err
	}
	if source.From != candidate.From || source.FromAgent != candidate.FromAgent || source.UserID != candidate.UserID {
		return source, nil, fail(http.StatusNotFound, busproto.CodeNotFound, "retry source not found")
	}
	if source.Addressed != "session" || candidate.Addressed != "session" || source.ToSession != candidate.ToSession || source.ToAgent != candidate.ToAgent || source.ToUserID != candidate.ToUserID || source.Body != candidate.Body || source.Intent != candidate.Intent || !slices.Equal(source.Refs, candidate.Refs) || source.ReplyTo != in.replyTo {
		return source, nil, badRequest("retry must preserve the source sender, session recipient, body, intent, refs and reply_to")
	}
	if state != string(busproto.StateUndelivered) || reason != busproto.ReasonSessionEnded {
		return source, nil, badRequest("retry source must be undelivered with reason session_ended")
	}
	var successor string
	err = tx.QueryRowContext(ctx, `SELECT successor_id FROM devbus_retries WHERE source_id=?`, id).Scan(&successor)
	if isNoRows(err) {
		return source, nil, nil
	}
	if err != nil {
		return source, nil, err
	}
	err = tx.QueryRowContext(ctx, `SELECT envelope,state,reason FROM devbus_messages WHERE id=? AND origin='local'`, successor).Scan(&raw, &state, &reason)
	if isNoRows(err) {
		return source, nil, fail(http.StatusNotFound, busproto.CodeNotFound, "retry successor is no longer retained")
	}
	if err != nil {
		return source, nil, err
	}
	var e busproto.Envelope
	if err = json.Unmarshal([]byte(raw), &e); err != nil {
		return source, nil, err
	}
	return source, &busproto.SendResponse{ID: e.ID, ThreadID: e.ThreadID, State: publicMessageState(state), Reason: reason, RetryOf: id, Admission: e.Admission, Replayed: true, Sender: e.Sender, Intent: e.Intent, Sent: e.Sent, ExpiresAt: e.ExpiresAt, To: busproto.Recipient{Session: e.ToSession, Agent: e.ToAgent, User: e.ToUser, UserID: e.ToUserID}}, nil
}
