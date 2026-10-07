package ingest

import (
	"context"

	"github.com/flopwire/flopwire/internal/coverage"
)

// DeviceParseCoverage reads one statement snapshot of a device's durable parse
// queue. It exposes no source identity, transcript text, or other devices.
func (q *Queue) DeviceParseCoverage(ctx context.Context, deviceID string) (coverage.ParseSnapshot, error) {
	out := coverage.ParseSnapshot{DeviceID: deviceID}
	err := q.Pool.QueryRow(ctx, `SELECT statement_timestamp(),
 count(*) FILTER(WHERE p.requested_seq>p.parsed_seq AND p.quarantined_at IS NULL),
 count(*) FILTER(WHERE p.requested_seq>p.parsed_seq AND p.quarantined_at IS NULL AND p.attempts>0),
 count(*) FILTER(WHERE p.quarantined_at IS NOT NULL),
 min(p.requested_at) FILTER(WHERE p.requested_seq>p.parsed_seq AND p.quarantined_at IS NULL)
 FROM source_parse_state p JOIN sources s ON s.id=p.source_id WHERE s.device_id=$1`, deviceID).
		Scan(&out.ObservedAt, &out.Pending, &out.Failing, &out.Quarantined, &out.OldestPending)
	if err != nil {
		return coverage.ParseSnapshot{}, err
	}
	out.ObservedAt = out.ObservedAt.UTC()
	if out.OldestPending != nil {
		v := out.OldestPending.UTC()
		out.OldestPending = &v
	}
	return out, nil
}
