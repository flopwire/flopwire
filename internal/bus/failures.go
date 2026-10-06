package bus

import (
	"context"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// A poll leases status to one device. Repeated polls renew its token;
// another device resuming this sender can take it after the lease ends.
// Latest trusted presence routes resumes, including open idle senders.
// Absent senders keep their durable notices without occupying a batch.
// Unoffered notices precede retries; retries rotate by their last offer,
// independently of the ownership deadline, so an unprinted batch cannot
// monopolize every poll. Only print confirmation acknowledges a notice.
const failureLease = busproto.FailureLease
const leaseFailuresSQL = `WITH candidates AS (
 SELECT n.message_id FROM bus_delivery_failures n
 WHERE n.from_user=$1 AND n.acked_at IS NULL
 AND (SELECT p.device_id FROM bus_presence p JOIN devices d ON d.id=p.device_id
   WHERE p.user_id=n.from_user AND p.agent=n.from_agent AND p.session_id=n.from_session
   AND NOT p.cloud AND p.seen_at>$5 AND d.revoked_at IS NULL
   ORDER BY p.seen_at DESC,p.device_id LIMIT 1)=$2
 AND (n.lease_device=$2 OR n.lease_until IS NULL OR n.lease_until<=$3)
 ORDER BY n.last_offered_at NULLS FIRST,n.created_at,n.message_id LIMIT 50 FOR UPDATE OF n SKIP LOCKED
) UPDATE bus_delivery_failures n SET lease_device=$2,
 lease_token=CASE WHEN n.lease_device=$2 AND n.lease_until>$3 THEN n.lease_token ELSE $6 END,
 lease_until=$4,last_offered_at=clock_timestamp() FROM candidates c WHERE n.message_id=c.message_id
 RETURNING n.message_id,n.from_session,n.from_agent,n.state,n.reason,n.lease_token,n.lease_until`

func (s *Store) leaseFailures(ctx context.Context, c busproto.Caller, now time.Time) ([]busproto.DeliveryFailure, error) {
	rows, err := s.Pool.Query(ctx, leaseFailuresSQL, c.UserID, c.DeviceID, now, now.Add(failureLease), now.Add(-busproto.PresenceTTL), uuid.NewString())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (busproto.DeliveryFailure, error) {
		var n busproto.DeliveryFailure
		err := r.Scan(&n.ID, &n.Session, &n.Agent, &n.State, &n.Reason, &n.Token, &n.ValidUntil)
		return n, err
	})
}

func ackFailures(ctx context.Context, tx pgx.Tx, c busproto.Caller, in []busproto.FailureAck, now time.Time) ([]string, []string, error) {
	var out, rejected []string
	for _, n := range in {
		var id string
		err := tx.QueryRow(ctx, `UPDATE bus_delivery_failures SET acked_at=COALESCE(acked_at,$5)
    WHERE message_id=$1 AND from_user=$2 AND lease_device=$3 AND lease_token=$4
    RETURNING message_id`, n.ID, c.UserID, c.DeviceID, n.Token, now).Scan(&id)
		if err == pgx.ErrNoRows {
			rejected = append(rejected, n.ID)
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		out = append(out, id)
	}
	return out, rejected, nil
}
