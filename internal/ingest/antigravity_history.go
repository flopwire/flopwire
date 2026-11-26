package ingest

import (
	"errors"

	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// writeAntigravityHistory attaches late records to the established replacement
// conversation, leaving its lineage, visibility, digest, and live rows alone.
// If the replacement has not been indexed, ordinary indexing can still
// publish the precursor; the replacement will retire it.
func (s *sink) writeAntigravityHistory(tx pgx.Tx, session string, replacedIn int64) (bool, error) {
	var conv string
	var hidden, deleted bool
	err := tx.QueryRow(s.ctx, `SELECT c.id::text,c.hidden_at IS NOT NULL,
		EXISTS (SELECT 1 FROM conversation_tombstones t WHERE t.user_id=c.user_id AND t.agent=c.agent
			AND t.session_id=c.session_id AND (t.scope='user' OR t.device_id=c.device_id))
		FROM conversations c WHERE c.device_id=$1 AND c.agent=$2 AND c.session_id=$3 AND c.source_id<>$4
		FOR NO KEY UPDATE`, s.src.deviceID, s.src.agent, session, s.src.id).Scan(&conv, &hidden, &deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if deleted || len(s.msgs) == 0 || !hidden && s.gate != nil && s.gate.hide[session].Mode != pathpolicy.Allow {
		return true, nil
	}
	batch := &pgx.Batch{}
	for _, m := range s.msgs {
		args := s.messageInsertArgs(uuid.NewString(), conv, m, clean(m.Text), enrichmentJSON(m.Enrichment), 1)
		batch.Queue(insertMessageColumns+`SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,
			COALESCE((SELECT max(version)+1 FROM messages WHERE conversation_id=$2 AND native_id=$4 AND part=$6),$17),
			$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,true,$28,
			(SELECT id FROM messages WHERE conversation_id=$2 AND native_id=$4 AND part=$6 AND NOT superseded)
			WHERE NOT EXISTS (SELECT 1 FROM messages WHERE source_id=$3 AND native_id=$4 AND part=$6 AND content_sha=$16)`,
			append(args, replacedIn)...)
	}
	return true, tx.SendBatch(s.ctx, batch).Close()
}
