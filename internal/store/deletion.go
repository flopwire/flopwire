package store

// Conversation deletion (spec §10), ported from the CASS-era deletion jobs
// (#3: 8a3fc34, 4120fba, 9c39cfd) and the deletion-owned states of the
// raw-object ledger (#6: d4eb6a1, 4c3ee87), remapped from segments to
// conversations, sources, and content-addressed chunks.
//
// Request (one transaction): take the conversation's natural-key lock, write
// the tombstone and a queued job, delete the conversation (messages cascade),
// audit. Search and context stop seeing it at commit.
//
// The request also marks the candidate sources (and their companion files)
// that nothing else references as tombstoned, so the ingest path
// acknowledges and discards later uploads to them from then on.
//
// Purge (worker, under the exclusive purge lock that backup shares):
//  1. claim the job; for the candidate sources that no conversation or
//     message still references, and their companions, keep the source row
//     as a tombstone and delete its generations (manifests and tails
//     cascade); move each chunk that lost its last manifest reference from
//     committed to purging_delete, owned by the job
//  2. delete those objects from S3
//  3. delete the chunk rows, complete the job, audit
//
// A failed attempt returns the job's chunks to deletion_pending (still owned)
// and schedules a retry; after five attempts the job fails and waits for an
// administrator's retry.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
)

// purgeLockID is held exclusively by a purge pass and shared by backup, so a
// backup never copies a manifest whose chunks are being deleted.
const purgeLockID int64 = 0x5445414d454d // "FLOPWIRE"

// PurgeLockID is held exclusively by anything that drops raw evidence (a
// purge pass, parse-time tombstoning) and shared by backup, so a backup
// never copies a manifest whose chunks are being deleted.
const PurgeLockID = purgeLockID

// sourceUnreferenced holds for a source s that no conversation or message
// names as itself or as its parent transcript. The lookups include
// superseded messages, so they need the unfiltered source_id indexes.
const sourceUnreferenced = `NOT EXISTS (SELECT 1 FROM conversations c WHERE c.source_id IN (s.id,s.parent_source_id))
	AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.source_id IN (s.id,s.parent_source_id))`

// tombstoneSourcesSQL marks the deleted conversations' sources ($1) that
// nothing references any more as tombstoned at $2.
const tombstoneSourcesSQL = `UPDATE sources s SET tombstoned_at=COALESCE(tombstoned_at,$2) WHERE s.id=ANY($1) AND ` + sourceUnreferenced

// orphanSourcesSQL returns the candidate sources ($1) and their companions
// that nothing references any more.
const orphanSourcesSQL = `SELECT COALESCE(array_agg(s.id),'{}') FROM sources s WHERE (s.id=ANY($1) OR s.parent_source_id=ANY($1)) AND ` + sourceUnreferenced

// ConversationLockKey names the advisory lock that serializes every write to
// one session of one user, whichever device it comes from. Ingest must take
// pg_advisory_xact_lock(hashtextextended(key, 0)) and then check
// conversation_tombstones before it inserts or updates the conversation, so
// a deletion and a re-upload of the same session cannot interleave.
func ConversationLockKey(userID, agent, sessionID string) string {
	return fmt.Sprintf("conversation:%d:%s:%d:%s:%s", len(userID), userID, len(agent), agent, sessionID)
}

// LockConversationsSQL locks the conversation rows $1 FOR UPDATE in the one
// order every transaction that writes several conversations, or a
// conversation and its message rows, takes them: session id bytewise, then
// id. A parse flush upserts its conversations in session order (Go sorts
// bytewise; the database default collation does not, hence COLLATE "C"),
// and the digest recount and checkpoint lock them in the same order. A
// writer locks its conversations this way before it touches any of their
// message rows: a flush holds its conversation from the upsert and then
// writes the messages, so the reverse order deadlocks with it. A writer
// that also locks a subagent's parent (its subagent count, its link, the
// ON DELETE SET NULL of its link) takes the parent in this order too,
// with the subagents, never after them.
const LockConversationsSQL = `SELECT id::text FROM conversations WHERE id=ANY($1::uuid[]) ORDER BY session_id COLLATE "C",id FOR UPDATE`

// InsertAudit writes an audit event inside the caller's transaction.
func InsertAudit(ctx context.Context, tx pgx.Tx, a domain.AuditEvent) error {
	return insertAudit(a)(ctx, tx)
}

// ErrForbidden: the caller may not act on the target.
var ErrForbidden = errors.New("forbidden")

// DeletionStore is implemented by Postgres only: the memory store holds no
// conversations.
type DeletionStore interface {
	// RequestConversationDeletion deletes the conversation, every
	// conversation of the same user and session on the user's other
	// devices, and every subagent conversation below them, and tombstones
	// their sessions for good. With ownerOnly, a conversation of another
	// user is ErrForbidden.
	RequestConversationDeletion(ctx context.Context, conversationID, actorID, deviceID string, ownerOnly bool) (domain.DeletionJob, error)
	DeletionJobByID(context.Context, string) (domain.DeletionJob, error)
	RetryDeletionJob(ctx context.Context, id, actorID, deviceID string) (domain.DeletionJob, error)
	ProcessDeletionJobs(context.Context) (int, error)
	// WithholdSession deletes a session of userID's, addressed by agent and
	// native session id, for the device that withheld it (a directory the
	// session named later moved it to local or deny after some of it was
	// uploaded). It returns the deletion job and conversation, or a zero job
	// when no parsed conversation exists. Acceptance always records a durable
	// tombstone, including before the first upload.
	WithholdSession(ctx context.Context, userID, deviceID, agent, session string) (job domain.DeletionJob, conversationID string, err error)
	WithholdDeviceSession(ctx context.Context, userID, deviceID, agent, session string) (job domain.DeletionJob, conversationID string, err error)
}

// Deletion scope is internal: an upload credential cannot select user scope.
type deletionScope string

const (
	deletionUser   deletionScope = "user"
	deletionDevice deletionScope = "device"
)

var _ DeletionStore = (*Postgres)(nil)

// A job without a tombstone belongs to a message redaction
// (notes/redaction.md): it only purges the old chunks the redaction
// replaced, and reads with an empty conversation.
const deletionSelect = `SELECT j.id::text,COALESCE(t.conversation_id::text,''),COALESCE(t.device_id::text,''),COALESCE(t.agent,''),COALESCE(t.session_id,''),
	j.requested_by::text,j.state,j.attempts,j.next_attempt_at,j.last_error,j.requested_at,j.completed_at
	FROM deletion_jobs j LEFT JOIN conversation_tombstones t ON t.id=j.tombstone_id`

// jobAudit names a job's audit event and target: a conversation deletion,
// or the purge of a message redaction.
func jobAudit(job domain.DeletionJob, step string) (action, targetType, targetID string) {
	if job.ConversationID == "" {
		return "message.redaction." + step, "deletion_job", job.ID
	}
	return "conversation.deletion." + step, "conversation", job.ConversationID
}

func scanDeletionJob(row pgx.Row) (domain.DeletionJob, error) {
	var job domain.DeletionJob
	var next, completed *time.Time
	err := row.Scan(&job.ID, &job.ConversationID, &job.DeviceID, &job.Agent, &job.SessionID, &job.RequestedBy, &job.State, &job.Attempts, &next, &job.LastError, &job.RequestedAt, &completed)
	if errors.Is(err, pgx.ErrNoRows) {
		return job, ErrNotFound
	}
	if next != nil {
		job.NextAttemptAt = *next
	}
	if completed != nil {
		job.CompletedAt = *completed
	}
	return job, err
}

func (p *Postgres) RequestConversationDeletion(ctx context.Context, conversationID, actorID, deviceID string, ownerOnly bool) (domain.DeletionJob, error) {
	return p.requestConversationDeletion(ctx, conversationID, actorID, deviceID, ownerOnly, deletionUser)
}

func (p *Postgres) requestConversationDeletion(ctx context.Context, conversationID, actorID, deviceID string, ownerOnly bool, scope deletionScope) (domain.DeletionJob, error) {
	var job domain.DeletionJob
	var scopedDevice any
	if scope == deletionDevice {
		scopedDevice = deviceID
	}
	if _, err := uuid.Parse(conversationID); err != nil {
		return job, ErrNotFound
	}
	err := p.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		var owner string
		// requested finds the job of an earlier request for this
		// conversation: idempotent, for whoever may see it. A narrower
		// device request cannot acknowledge a user-wide deletion; the
		// withhold caller retries against any remaining copies.
		requested := func() error {
			var err error
			if job, err = scanDeletionJob(tx.QueryRow(ctx, deletionSelect+`
				WHERE j.id=(SELECT job_id FROM conversation_tombstones WHERE conversation_id=$1
					AND ($2='device' OR scope='user') AND ($2<>'device' OR device_id=$3))`, conversationID, scope, scopedDevice)); err != nil {
				return err
			}
			if err = tx.QueryRow(ctx, `SELECT user_id::text FROM conversation_tombstones WHERE conversation_id=$1`, conversationID).Scan(&owner); err != nil {
				return err
			}
			if ownerOnly && owner != actorID {
				return ErrForbidden
			}
			return nil
		}
		if err = requested(); !errors.Is(err, ErrNotFound) {
			return err
		}
		var device, agent, session string
		if err = tx.QueryRow(ctx, `SELECT user_id::text,device_id::text,agent,session_id FROM conversations WHERE id=$1`, conversationID).Scan(&owner, &device, &agent, &session); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if (ownerOnly && owner != actorID) || (scope == deletionDevice && (owner != actorID || device != deviceID)) {
			return ErrForbidden
		}
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, ConversationLockKey(owner, agent, session)); err != nil {
			return err
		}
		// A concurrent request for the same session may have committed while
		// this one waited for the lock: answer its job, not a conflict.
		var live bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM conversations WHERE id=$1)`, conversationID).Scan(&live); err != nil {
			return err
		}
		if !live {
			return requested()
		}
		// The doomed set: the session on every device of its user, and every
		// subagent conversation below, linked or still waiting for its
		// parent's id. parent is the session it was reached from.
		type doomed struct{ id, device, agent, session, parent string }
		var set []doomed
		rows, err := tx.Query(ctx, `WITH RECURSIVE d AS (
				SELECT id,device_id,agent,session_id,''::text AS parent FROM conversations WHERE user_id=$1 AND agent=$2 AND session_id=$3 AND ($4<>'device' OR device_id=$5)
				UNION
				SELECT c.id,c.device_id,c.agent,c.session_id,d.session_id FROM conversations c JOIN d ON c.parent_conversation_id=d.id
					OR (c.parent_conversation_id IS NULL AND c.user_id=$1 AND c.agent=d.agent AND c.parent_native_session_id=d.session_id)
				WHERE c.user_id=$1 AND ($4<>'device' OR c.device_id=$5))
			SELECT id::text,device_id::text,agent,session_id,parent FROM d`, owner, agent, session, scope, scopedDevice)
		if err != nil {
			return err
		}
		for rows.Next() {
			var d doomed
			if err := rows.Scan(&d.id, &d.device, &d.agent, &d.session, &d.parent); err != nil {
				rows.Close()
				return err
			}
			set = append(set, d)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return err
		}
		// A parse flush of a subagent takes its parent's session lock and
		// then its own, so the session locks go parent first at every
		// depth, then in (agent, session) order.
		type sessKey struct{ agent, session string }
		parentOf := map[sessKey]string{}
		for _, d := range set {
			if d.parent != "" {
				parentOf[sessKey{d.agent, d.session}] = d.parent
			}
		}
		depth := func(d doomed) int {
			n, k := 0, sessKey{d.agent, d.session}
			for k.agent != agent || k.session != session {
				p, ok := parentOf[k]
				if !ok || n > len(set) { // not reached from a parent, or a cycle
					break
				}
				k, n = sessKey{k.agent, p}, n+1
			}
			return n
		}
		depths := make(map[string]int, len(set))
		for _, d := range set {
			depths[d.id] = depth(d)
		}
		slices.SortFunc(set, func(a, b doomed) int {
			return cmp.Or(cmp.Compare(depths[a.id], depths[b.id]), cmp.Compare(a.agent, b.agent), cmp.Compare(a.session, b.session), cmp.Compare(a.id, b.id))
		})
		set = slices.CompactFunc(set, func(a, b doomed) bool { return a.id == b.id })
		ids := make([]string, 0, len(set))
		locked := map[sessKey]bool{{agent, session}: true}
		for _, d := range set {
			ids = append(ids, d.id)
			if k := (sessKey{d.agent, d.session}); !locked[k] {
				locked[k] = true
				if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, ConversationLockKey(owner, d.agent, d.session)); err != nil {
					return err
				}
			}
		}
		var sources []uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT COALESCE(array_agg(DISTINCT s),'{}') FROM (
			SELECT source_id FROM conversations WHERE id=ANY($1::uuid[])
			UNION SELECT source_id FROM messages WHERE conversation_id=ANY($1::uuid[])) x(s) WHERE s IS NOT NULL`, ids).Scan(&sources); err != nil {
			return err
		}
		if err = tx.QueryRow(ctx, `SELECT COALESCE(array_agg(id),'{}') FROM sources WHERE id=ANY($1) OR parent_source_id=ANY($1)`, sources).Scan(&sources); err != nil {
			return err
		}
		// The doomed set spans sessions. The DELETE below would lock its
		// rows in plan order, while a checkpoint or recount of some of them
		// (which takes no session lock) locks them in session order.
		if _, err = tx.Exec(ctx, LockConversationsSQL, ids); err != nil {
			return err
		}
		now := time.Now().UTC()
		root := uuid.NewString()
		job = domain.DeletionJob{ID: uuid.NewString(), ConversationID: conversationID, DeviceID: device, Agent: agent, SessionID: session, RequestedBy: actorID, State: domain.DeletionQueued, RequestedAt: now}
		if _, err = tx.Exec(ctx, `INSERT INTO conversation_tombstones(id,user_id,device_id,agent,session_id,conversation_id,requested_by,requested_at,scope) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			root, owner, device, agent, session, conversationID, actorID, now, scope); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO deletion_jobs(id,tombstone_id,source_ids,requested_by,state,requested_at) VALUES($1,$2,$3,$4,$5,$6)`,
			job.ID, root, sources, actorID, job.State, now); err != nil {
			return err
		}
		for _, d := range set {
			if _, err = tx.Exec(ctx, `INSERT INTO conversation_tombstones(id,user_id,device_id,agent,session_id,conversation_id,requested_by,requested_at,scope)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT DO NOTHING`, uuid.NewString(), owner, d.device, d.agent, d.session, d.id, actorID, now, scope); err != nil {
				return err
			}
		}
		for _, stmt := range []struct {
			sql  string
			args []any
		}{
			{`UPDATE conversation_tombstones SET job_id=$3 WHERE (id=$1 OR conversation_id=ANY($2::uuid[])) AND job_id IS NULL`, []any{root, ids, job.ID}},
			{`DELETE FROM conversations WHERE id=ANY($1::uuid[])`, []any{ids}},
			{tombstoneSourcesSQL, []any{sources, now}},
			// A tail is never in object storage; drop it now so no later
			// backup copies the deleted bytes.
			{`DELETE FROM provisional_tails WHERE source_id IN (SELECT id FROM sources WHERE id=ANY($1) AND tombstoned_at IS NOT NULL)`, []any{sources}},
		} {
			if _, err = tx.Exec(ctx, stmt.sql, stmt.args...); err != nil {
				return err
			}
		}
		return insertAudit(domain.AuditEvent{ID: uuid.NewString(), ActorID: actorID, DeviceID: deviceID, Action: "conversation.deletion.requested", TargetType: "conversation", TargetID: conversationID,
			Metadata: map[string]any{"job_id": job.ID, "agent": agent, "session_id": session, "owner": owner, "conversations": len(ids), "sources": len(sources)}, CreatedAt: now})(ctx, tx)
	})
	return job, err
}

func (p *Postgres) DeletionJobByID(ctx context.Context, id string) (domain.DeletionJob, error) {
	if _, err := uuid.Parse(id); err != nil {
		return domain.DeletionJob{}, ErrNotFound
	}
	return scanDeletionJob(p.pool.QueryRow(ctx, deletionSelect+` WHERE j.id=$1`, id))
}

func (p *Postgres) RetryDeletionJob(ctx context.Context, id, actorID, deviceID string) (domain.DeletionJob, error) {
	var job domain.DeletionJob
	if _, err := uuid.Parse(id); err != nil {
		return job, ErrNotFound
	}
	err := p.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE deletion_jobs SET state='queued',last_error='',next_attempt_at=NULL,locked_at=NULL WHERE id=$1 AND state='failed'`, id)
		if err != nil {
			return err
		}
		if job, err = scanDeletionJob(tx.QueryRow(ctx, deletionSelect+` WHERE j.id=$1`, id)); err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrConflict // exists but is not failed
		}
		return insertAudit(domain.AuditEvent{ID: uuid.NewString(), ActorID: actorID, DeviceID: deviceID, Action: "conversation.deletion.retried", TargetType: "conversation", TargetID: job.ConversationID, Metadata: map[string]any{"job_id": job.ID}, CreatedAt: time.Now().UTC()})(ctx, tx)
	})
	return job, err
}

// ProcessDeletionJobs runs at most one job and reports whether it completed
// one. A job left in purging by a crashed worker is reclaimed after five
// minutes.
func (p *Postgres) ProcessDeletionJobs(ctx context.Context) (int, error) {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()
	if err = PinBackend(ctx, conn); err != nil {
		return 0, err
	}
	if _, err = conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, purgeLockID); err != nil {
		return 0, err
	}
	defer func() { _ = ReleaseAdvisoryLocks(conn, p.pool, AdvisoryLock{ID: purgeLockID}) }()

	var job domain.DeletionJob
	var sources, removed int
	err = inConnTx(ctx, conn, func(tx pgx.Tx) error {
		var id string
		var candidates []uuid.UUID
		err := tx.QueryRow(ctx, `UPDATE deletion_jobs SET state='purging',attempts=attempts+1,locked_at=now(),next_attempt_at=NULL
			WHERE id=(SELECT id FROM deletion_jobs
				WHERE state='queued' OR state='retry_wait' AND next_attempt_at<=now() OR state='purging' AND locked_at<now()-interval '5 minutes'
				ORDER BY requested_at FOR UPDATE SKIP LOCKED LIMIT 1)
			RETURNING id::text,source_ids`).Scan(&id, &candidates)
		if err != nil {
			return err
		}
		if job, err = scanDeletionJob(tx.QueryRow(ctx, deletionSelect+` WHERE j.id=$1`, id)); err != nil {
			return err
		}
		var orphans []uuid.UUID
		if err = tx.QueryRow(ctx, orphanSourcesSQL, candidates).Scan(&orphans); err != nil {
			return err
		}
		sources = len(orphans)
		var hashes [][]byte
		if err = tx.QueryRow(ctx, `SELECT COALESCE(array_agg(DISTINCT chunk_hash),'{}') FROM manifest_entries WHERE source_id=ANY($1)`, orphans).Scan(&hashes); err != nil {
			return err
		}
		// The source row stays as the tombstone that stops a re-upload.
		if _, err = tx.Exec(ctx, `UPDATE sources SET tombstoned_at=COALESCE(tombstoned_at,now()) WHERE id=ANY($1)`, orphans); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM generations WHERE source_id=ANY($1)`, orphans); err != nil {
			return err
		}
		// Lock first, then test for references in a later statement, so the
		// test sees every manifest committed by an ingest that held the row.
		if _, err = tx.Exec(ctx, `SELECT 1 FROM chunks WHERE hash=ANY($1) AND state='committed' ORDER BY hash FOR UPDATE`, hashes); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE chunks SET state='purging_delete',deletion_job_id=$2,updated_at=now()
			WHERE hash=ANY($1) AND state='committed' AND NOT EXISTS (SELECT 1 FROM manifest_entries m WHERE m.chunk_hash=chunks.hash)`, hashes, id); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE chunks SET state='purging_delete',updated_at=now() WHERE deletion_job_id=$1 AND state='deletion_pending'`, id)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	keys, err := collect[string](conn.Query(ctx, `SELECT object_key FROM chunks WHERE deletion_job_id=$1 AND state='purging_delete' ORDER BY object_key`, job.ID))(func(row pgx.Row) (string, error) {
		var key string
		return key, row.Scan(&key)
	})
	if err != nil {
		return 0, p.failDeletion(ctx, conn, job, err)
	}
	for _, key := range keys {
		if err = p.removeObject(ctx, key); err != nil {
			return 0, p.failDeletion(ctx, conn, job, fmt.Errorf("object_remove: %w", err))
		}
	}
	err = inConnTx(ctx, conn, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM chunks WHERE deletion_job_id=$1 AND state='purging_delete'`, job.ID)
		if err != nil {
			return err
		}
		removed = int(tag.RowsAffected())
		now := time.Now().UTC()
		if _, err = tx.Exec(ctx, `UPDATE deletion_jobs SET state='complete',completed_at=$2,locked_at=NULL,last_error='' WHERE id=$1`, job.ID, now); err != nil {
			return err
		}
		action, tt, target := jobAudit(job, "complete")
		return insertAudit(domain.AuditEvent{ID: uuid.NewString(), ActorID: job.RequestedBy, Action: action, TargetType: tt, TargetID: target, Metadata: map[string]any{"job_id": job.ID, "attempts": job.Attempts, "sources": sources, "chunks": removed}, CreatedAt: now})(ctx, tx)
	})
	if err != nil {
		return 0, p.failDeletion(ctx, conn, job, err)
	}
	return 1, nil
}

func (p *Postgres) removeObject(ctx context.Context, key string) error {
	if p.objects == nil {
		return errors.New("object client is not configured")
	}
	deleteCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return p.objects.RemoveObject(deleteCtx, p.bucket, key, minio.RemoveObjectOptions{})
}

// failDeletion reuses the worker's connection, which still holds the purge
// lock, so the bookkeeping commits before the lock is released and does not
// depend on pool capacity.
func (p *Postgres) failDeletion(ctx context.Context, conn *pgxpool.Conn, job domain.DeletionJob, cause error) error {
	state := domain.DeletionRetryWait
	if job.Attempts >= 5 {
		state = domain.DeletionFailed
	}
	next := time.Now().UTC().Add(time.Duration(1<<min(job.Attempts, 8)) * time.Second)
	errClass := "worker_error"
	if strings.Contains(cause.Error(), "object_remove") {
		errClass = "object_store_unavailable"
	}
	bookCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	err := inConnTx(bookCtx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(bookCtx, `UPDATE chunks SET state='deletion_pending',updated_at=now(),last_error_class=$2 WHERE deletion_job_id=$1 AND state='purging_delete'`, job.ID, errClass); err != nil {
			return err
		}
		if _, err := tx.Exec(bookCtx, `UPDATE deletion_jobs SET state=$2,next_attempt_at=$3,last_error=$4,locked_at=NULL WHERE id=$1`, job.ID, state, next, errClass); err != nil {
			return err
		}
		step := "retry_wait"
		if state == domain.DeletionFailed {
			step = "failed"
		}
		action, tt, target := jobAudit(job, step)
		return insertAudit(domain.AuditEvent{ID: uuid.NewString(), ActorID: job.RequestedBy, Action: action, TargetType: tt, TargetID: target, Metadata: map[string]any{"job_id": job.ID, "attempts": job.Attempts, "error_class": errClass}, CreatedAt: time.Now().UTC()})(bookCtx, tx)
	})
	return errors.Join(cause, err)
}

func inConnTx(ctx context.Context, conn *pgxpool.Conn, fn func(pgx.Tx) error) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// WithholdSession: when the server holds a conversation of the session
// (the device's own first), it is RequestConversationDeletion of it as its
// owner, which covers the session on every device of the user and every
// subagent below it. Without a conversation, it records a prospective
// tombstone: the ingest sink rejects and purges later uploads. A conversation
// created while this looked is found on the next try.
func (p *Postgres) WithholdSession(ctx context.Context, userID, deviceID, agent, session string) (domain.DeletionJob, string, error) {
	return p.withholdSession(ctx, userID, deviceID, agent, session, deletionUser)
}

// WithholdDeviceSession permanently withholds only the bound device's copy.
func (p *Postgres) WithholdDeviceSession(ctx context.Context, userID, deviceID, agent, session string) (domain.DeletionJob, string, error) {
	return p.withholdSession(ctx, userID, deviceID, agent, session, deletionDevice)
}

func (p *Postgres) withholdSession(ctx context.Context, userID, deviceID, agent, session string, scope deletionScope) (domain.DeletionJob, string, error) {
	var own bool
	if err := p.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM devices WHERE id=$1 AND user_id=$2)`, deviceID, userID).Scan(&own); err != nil {
		return domain.DeletionJob{}, "", err
	}
	if !own {
		return domain.DeletionJob{}, "", ErrForbidden
	}

	for range 3 {
		var convID string
		err := p.pool.QueryRow(ctx, `SELECT id::text FROM conversations WHERE user_id=$1 AND agent=$2 AND session_id=$3
			AND ($5<>'device' OR device_id=$4) ORDER BY device_id=$4 DESC, id LIMIT 1`, userID, agent, session, deviceID, scope).Scan(&convID)
		if err == nil {
			job, err := p.requestConversationDeletion(ctx, convID, userID, deviceID, true, scope)
			if errors.Is(err, ErrNotFound) {
				continue // gone meanwhile (superseded, or deleted without a tombstone)
			}
			return job, convID, err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return domain.DeletionJob{}, "", err
		}
		again := false
		err = p.inTx(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, ConversationLockKey(userID, agent, session)); err != nil {
				return err
			}
			var live, dead bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM conversations WHERE user_id=$1 AND agent=$2 AND session_id=$3 AND ($5<>'device' OR device_id=$4)),
				EXISTS(SELECT 1 FROM conversation_tombstones WHERE user_id=$1 AND agent=$2 AND session_id=$3 AND (scope='user' OR ($5='device' AND device_id=$4)))`, userID, agent, session, deviceID, scope).Scan(&live, &dead); err != nil {
				return err
			}
			switch {
			case live:
				again = true
				return nil
			case dead:
				return nil // deleted before: nothing comes back
			}
			now := time.Now().UTC()
			if _, err := tx.Exec(ctx, `INSERT INTO conversation_tombstones(id,user_id,device_id,agent,session_id,conversation_id,requested_by,requested_at,scope)
				VALUES($1,$2,$3,$4,$5,$6,$2,$7,$8)`, uuid.NewString(), userID, deviceID, agent, session, uuid.NewString(), now, scope); err != nil {
				return err
			}
			return insertAudit(domain.AuditEvent{ID: uuid.NewString(), ActorID: userID, DeviceID: deviceID, Action: "conversation.deletion.requested", TargetType: "conversation",
				Metadata: map[string]any{"agent": agent, "session_id": session, "owner": userID, "conversations": 0, "unparsed": true}, CreatedAt: now})(ctx, tx)
		})
		if !again {
			return domain.DeletionJob{}, "", err
		}
	}
	return domain.DeletionJob{}, "", ErrConflict
}
