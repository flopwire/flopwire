package localindex

import (
	"context"
	"database/sql"
	"errors"

	"github.com/flopwire/flopwire/internal/transcript"
)

// UpdateAntigravityLineage applies authoritative catalog metadata to an
// existing transcript. The catalog and the transcript can change independently.
func (s *Store) UpdateAntigravityLineage(ctx context.Context, sourceID int64, session, parent string, depth int) error {
	return s.write(ctx, func(w *writeTx) error {
		row, err := w.queryRow(`SELECT id, parent_conversation_id FROM conversations
			WHERE device_id = ? AND agent = ? AND session_id = ? AND source_id = ? AND deleted_in_generation IS NULL
			  AND (parent_session_id IS NOT ? OR depth != ?)`,
			w.s.opts.DeviceID, transcript.AgentAntigravity, session, sourceID, nullStr(parent), depth)
		if err != nil {
			return err
		}
		var id int64
		var oldParent sql.NullInt64
		if err := row.Scan(&id, &oldParent); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if _, err := w.exec(`UPDATE conversations SET parent_session_id = ?, parent_conversation_id = NULL,
			spawned_by_message_id = NULL, depth = ? WHERE id = ?`, nullStr(parent), depth, id); err != nil {
			return err
		}
		if err := w.resolveLinks(id); err != nil {
			return err
		}
		if _, err := w.exec(parentSubagentsSQL, id); err != nil {
			return err
		}
		if oldParent.Valid {
			_, err = w.exec(`UPDATE conversations SET digest = json_set(digest, '$.subagents',
				(SELECT count(*) FROM conversations k WHERE k.device_id = conversations.device_id AND k.agent = conversations.agent
				   AND k.parent_session_id = conversations.session_id AND k.id <> conversations.id))
				WHERE digest IS NOT NULL AND id = ?`, oldParent.Int64)
		}
		return err
	})
}
