package localindex

import (
	"context"
	"fmt"
	"strings"
)

// CoworkHistoricalUnknownFacts lists device-bound facts that bytes belonging
// to these native identities preceded complete host-scope proof. This table is
// deliberately outside rebuild-owned index tables: deleting rows, generations,
// or source paths must not erase historical provenance.
func (s *Store) CoworkHistoricalUnknownFacts(ctx context.Context) ([]string, error) {
	var facts []string
	err := s.readSources(ctx, func(ctx context.Context, q dbtx) error {
		var exists bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='cowork_history')`).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return nil
		}
		rows, err := q.QueryContext(ctx, `SELECT session_id FROM cowork_history WHERE device_id=? ORDER BY session_id`, s.opts.DeviceID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			facts = append(facts, id)
		}
		return rows.Err()
	})
	return facts, err
}

func (s *Store) CoworkHistoricalUnknown(ctx context.Context, session string) (bool, error) {
	var found bool
	err := s.readSources(ctx, func(ctx context.Context, q dbtx) error {
		var exists bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='cowork_history')`).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return nil
		}
		return q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM cowork_history WHERE device_id=? AND session_id=?)`, s.opts.DeviceID, session).Scan(&found)
	})
	return found, err
}

// MarkCoworkHistoricalUnknown atomically commits a whole verified family.
// Callers must supply actual unknown historical capture/index evidence or
// earlier historical provenance, never current readiness or metadata alone.
// The method returns only after commit, before any last proof can be purged.
func (s *Store) MarkCoworkHistoricalUnknown(ctx context.Context, sessions []string) error {
	if len(sessions) == 0 {
		return nil
	}
	for _, session := range sessions {
		if session == "" || strings.ContainsAny(session, "/\\\t\r\n\x00") {
			return fmt.Errorf("localindex: invalid Cowork historical native identity")
		}
	}
	return s.writeWait(ctx, func(w *writeTx) error {
		if _, err := w.exec(`CREATE TABLE IF NOT EXISTS cowork_history(device_id TEXT NOT NULL,session_id TEXT NOT NULL,PRIMARY KEY(device_id,session_id)) WITHOUT ROWID`); err != nil {
			return err
		}
		for _, session := range sessions {
			if _, err := w.exec(`INSERT OR IGNORE INTO cowork_history(device_id,session_id) VALUES(?,?)`, s.opts.DeviceID, session); err != nil {
				return err
			}
		}
		return nil
	})
}
