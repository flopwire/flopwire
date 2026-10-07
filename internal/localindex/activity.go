package localindex

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

// SessionActivities reads only conversation metadata, in bounded batches.
// Unknown sessions and undated conversations have zero activity. Multiple
// indexed copies of a session contribute their latest activity.
func (s *Store) SessionActivities(ctx context.Context, agent transcript.Agent, sessions []string) (map[string]time.Time, error) {
	out := make(map[string]time.Time, len(sessions))
	for _, session := range sessions {
		out[session] = time.Time{}
	}
	const activityBatch = 400
	for from := 0; from < len(sessions); from += activityBatch {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := min(from+activityBatch, len(sessions))
		raw, err := json.Marshal(sessions[from:end])
		if err != nil {
			return nil, err
		}
		rows, err := s.rdb.QueryContext(ctx, `SELECT session_id,max(last_activity_at) FROM conversations WHERE agent=? AND session_id IN (SELECT value FROM json_each(?)) GROUP BY session_id`, string(agent), string(raw))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var session string
			var at sql.NullInt64
			if err := rows.Scan(&session, &at); err != nil {
				rows.Close()
				return nil, err
			}
			if at.Valid {
				activity := time.UnixMilli(at.Int64)
				if activity.After(out[session]) {
					out[session] = activity
				}
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
