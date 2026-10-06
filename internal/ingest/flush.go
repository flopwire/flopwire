// Package ingest is the server side of the sync protocol (spec §7.1):
// finalized chunks to object storage, manifests and provisional tails to
// Postgres, then a durable parse queue that turns the new bytes into
// message rows.
//
// A flush follows internal/syncproto's rules in order; internal/syncproto/
// synctest is the in-memory reference and its conformance suite runs
// against this implementation. Chunk bodies are verified and put to object
// storage as they stream in, each under the chunk advisory lock and an
// 'uploading' reservation (internal/store/chunks.go); the manifest
// transaction then commits the reservations it references. A request that
// fails half way leaves only reservations, which the orphan reconciler
// removes.
package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/flopwire/flopwire/internal/store"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Error is a request the server refuses; Status and Code go on the wire.
type Error struct {
	Status int
	Code   string
	Msg    string
}

func (e *Error) Error() string { return e.Code + ": " + e.Msg }

func badRequest(format string, args ...any) *Error {
	return &Error{http.StatusBadRequest, "bad_request", fmt.Sprintf(format, args...)}
}

// reservationGrace is how long an uncommitted chunk reservation is kept
// before the orphan reconciler may delete its object.
const reservationGrace = 15 * time.Minute

// storageKinds are the sources.storage_kind values a device may send.
var storageKinds = []string{"cass_export", "jsonl_append", "json_doc", "sqlite", "dir", "markdown", string(transcript.StorageCompanion)}

// Server applies sync requests.
type Server struct {
	Pool    *pgxpool.Pool
	Objects Objects
	Log     *slog.Logger
	// Queue, when set, is told about sources with new bytes to parse.
	Queue Notifier

	flushing sync.Map // device id -> struct{}: the device's flush in progress
}

// Notifier is the parse queue as the flush path sees it: it is told which
// sources have new bytes, and it may refuse flushes while parsing is
// behind. The durable request is the source_parse_state row the manifest
// transaction bumps; Notify only makes it prompt.
type Notifier interface {
	Notify(sourceID string)
	Overloaded() *Error
}

// Has answers which of the hashes the device's user does not hold. A chunk
// counts as held only when it is committed and the user already possesses
// it (one of their devices uploaded it, or one of their sources references
// it): the answer never reveals what other users have stored, and a hash
// alone never grants its bytes (proof of possession, D10).
func (s *Server) Has(ctx context.Context, deviceID string, hashes []syncproto.Hash) ([]syncproto.Hash, error) {
	if len(hashes) > syncproto.MaxHasHashes {
		return nil, badRequest("at most %d hashes", syncproto.MaxHasHashes)
	}
	have, err := owned(ctx, s.Pool, deviceID, hashes)
	if err != nil {
		return nil, err
	}
	// A chunk a message redaction rewrote is never stored again: the flush
	// that names it references the rewritten chunk instead. The user must
	// still prove possession: unless they hold the rewritten chunk, the old
	// hash is missing and its body (verified, then dropped) is the proof.
	moved, err := redirects(ctx, s.Pool, hashes)
	if err != nil {
		return nil, err
	}
	var targets []syncproto.Hash
	for _, nh := range moved {
		targets = append(targets, nh)
	}
	haveNew, err := owned(ctx, s.Pool, deviceID, targets)
	if err != nil {
		return nil, err
	}
	missing := []syncproto.Hash{}
	for _, h := range hashes {
		nh, ok := moved[h]
		if ok && !haveNew[nh] || !ok && !have[h] {
			missing = append(missing, h)
		}
	}
	return missing, nil
}

// owned reports which of the hashes are committed chunks that the device's
// user possesses: uploaded by one of their devices, or referenced by one
// of their sources.
func owned(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, deviceID string, hashes []syncproto.Hash) (map[syncproto.Hash]bool, error) {
	have := map[syncproto.Hash]bool{}
	if len(hashes) == 0 {
		return have, nil
	}
	raw := make([][]byte, len(hashes))
	for i := range hashes {
		raw[i] = hashes[i][:]
	}
	rows, err := q.Query(ctx, `WITH mine AS (SELECT id FROM devices WHERE user_id=(SELECT user_id FROM devices WHERE id=$2))
		SELECT c.hash FROM chunks c WHERE c.hash=ANY($1) AND c.state='committed'
			AND (c.uploaded_by_device IN (SELECT id FROM mine)
				OR EXISTS (SELECT 1 FROM manifest_entries m JOIN sources s ON s.id=m.source_id
					WHERE m.chunk_hash=c.hash AND s.device_id IN (SELECT id FROM mine)))`, raw, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		have[syncproto.Hash(b)] = true
	}
	return have, rows.Err()
}

// redirects maps the hashes that message redactions rewrote to the chunks
// that replaced them (notes/redaction.md).
func redirects(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, hashes []syncproto.Hash) (map[syncproto.Hash]syncproto.Hash, error) {
	out := map[syncproto.Hash]syncproto.Hash{}
	if len(hashes) == 0 {
		return out, nil
	}
	raw := make([][]byte, len(hashes))
	for i := range hashes {
		raw[i] = hashes[i][:]
	}
	rows, err := q.Query(ctx, `SELECT old_hash,new_hash FROM chunk_redirects WHERE old_hash=ANY($1)`, raw)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var o, n []byte
		if err := rows.Scan(&o, &n); err != nil {
			return nil, err
		}
		out[syncproto.Hash(o)] = syncproto.Hash(n)
	}
	return out, rows.Err()
}

// flush is the state of one flush request.
type flush struct {
	s        *Server
	deviceID string
	h        *syncproto.FlushHeader
	conn     *pgxpool.Conn
	locked   []string                // chunk advisory locks held
	reserved map[syncproto.Hash]bool // chunks this request put and reserved
	sizes    map[syncproto.Hash]int64
	moved    map[syncproto.Hash]syncproto.Hash // chunk redirects of redacted messages
}

// Flush applies one flush request of the device. Parse work it creates is
// queued after the commit.
func (s *Server) Flush(ctx context.Context, deviceID string, h *syncproto.FlushHeader, pr *syncproto.PayloadReader) (*syncproto.FlushResponse, error) {
	if !slices.Contains(storageKinds, h.Source.StorageKind) {
		return nil, badRequest("unknown storage_kind %q", h.Source.StorageKind)
	}
	if s.Queue != nil {
		if e := s.Queue.Overloaded(); e != nil {
			return nil, e
		}
	}
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	if err := store.PinBackend(ctx, conn); err != nil {
		return nil, err
	}
	f := &flush{s: s, deviceID: deviceID, h: h, conn: conn, reserved: map[syncproto.Hash]bool{}, sizes: map[syncproto.Hash]int64{}}
	defer f.unlock()

	if err := recordDeviceDirs(ctx, conn, deviceID, h.Device); err != nil {
		return nil, err
	}
	if err := recordLive(ctx, conn, deviceID, h.Live); err != nil {
		return nil, err
	}
	if _, err := checkFlushPolicy(ctx, conn, deviceID, h.Source); err != nil {
		return nil, err
	}
	var tombstoned bool
	var refused *string
	err = conn.QueryRow(ctx, `SELECT tombstoned_at IS NOT NULL,refused_rule FROM sources WHERE device_id=$1 AND path=$2 AND file_id=$3`,
		deviceID, h.Source.Path, h.Source.FileID).Scan(&tombstoned, &refused)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if tombstoned {
		resp := swallow(h)
		resp.Refused = deref(refused)
		return resp, nil
	}
	// A device that still holds a chunk a message redaction rewrote names
	// the rewritten chunk instead, and its copy of the old bytes is dropped.
	var named []syncproto.Hash
	for _, e := range h.Entries {
		named = append(named, e.Hash)
	}
	for _, b := range h.Bodies {
		named = append(named, b.Hash)
	}
	if f.moved, err = redirects(ctx, conn, named); err != nil {
		return nil, err
	}
	for i := range h.Entries {
		if nh, ok := f.moved[h.Entries[i].Hash]; ok {
			h.Entries[i].Hash = nh
		}
	}
	// Step 1: every body is verified before anything references it; each
	// is stored as it arrives so memory holds one chunk at a time.
	// A body is decoded and verified, then its compressed frame is stored
	// as the object (syncproto codec.go).
	var zbuf, buf []byte
	for {
		b, data, z, err := pr.NextBody(zbuf, buf)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, badRequest("%v", err)
		}
		zbuf, buf = z[:0], data[:0]
		if err := f.store(ctx, b, z); err != nil {
			return nil, err
		}
	}
	tailData, err := pr.ReadTail(nil)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	var resp *syncproto.FlushResponse
	var parse []string
	err = pgx.BeginTxFunc(ctx, conn, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var err error
		resp, parse, err = f.commit(ctx, tx, tailData)
		return err
	})
	if err != nil {
		return nil, err
	}
	if s.Queue != nil {
		for _, id := range parse {
			s.Queue.Notify(id)
		}
	}
	return resp, nil
}

// maxDirLen bounds a directory a device reports.
const maxDirLen = 4096

// recordDeviceDirs stores the home and harness directories the device
// reported, when they changed. The server expands "~" in admin path rules
// with this home (D18); a device reports only its own.
func recordDeviceDirs(ctx context.Context, conn *pgxpool.Conn, deviceID string, d *syncproto.DeviceDirs) error {
	if d == nil {
		return nil
	}
	if len(d.Home) > maxDirLen || len(d.ClaudeProjects) > maxDirLen || len(d.CodexHome) > maxDirLen {
		return badRequest("device directories longer than %d bytes", maxDirLen)
	}
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, policyDeviceLockKey(deviceID)); err != nil {
			return err
		}
		var ledger bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM session_policy_placements WHERE device_id=$1)`, deviceID).Scan(&ledger); err != nil {
			return err
		}
		if ledger {
			if err := validatePolicyDeviceDirs(d); err != nil {
				return err
			}
			return recordPolicyDeviceDirs(ctx, tx, deviceID, d)
		}
		_, err := tx.Exec(ctx, `UPDATE devices SET home=NULLIF($2,''),claude_projects=NULLIF($3,''),codex_home=NULLIF($4,'')
			WHERE id=$1 AND (home,claude_projects,codex_home) IS DISTINCT FROM (NULLIF($2,''),NULLIF($3,''),NULLIF($4,''))`, deviceID, d.Home, d.ClaudeProjects, d.CodexHome)
		return err
	})
}

// recordLive stores the sessions the device reports its harnesses hold
// open (nil: no report). It writes when the list changed or the last
// report is a minute old, so retrieval can tell a fresh report from a
// stale one without a write per flush.
func recordLive(ctx context.Context, conn *pgxpool.Conn, deviceID string, live []string) error {
	if live == nil {
		return nil
	}
	if len(live) > syncproto.MaxLive {
		return badRequest("more than %d live sessions", syncproto.MaxLive)
	}
	for _, id := range live {
		if len(id) > 256 {
			return badRequest("live session id longer than 256 bytes")
		}
	}
	_, err := conn.Exec(ctx, `UPDATE devices SET live_sessions=$2,live_at=now()
		WHERE id=$1 AND (live_sessions IS DISTINCT FROM $2 OR live_at IS NULL OR live_at<now()-interval '1 minute')`, deviceID, live)
	return err
}

// swallow acknowledges a flush to a tombstoned source without storing it:
// the conversation was deleted, and the device must stop re-sending. The
// entry count saturates; the device clamps it to what it holds.
func swallow(h *syncproto.FlushHeader) *syncproto.FlushResponse {
	resp := &syncproto.FlushResponse{Version: syncproto.Version, Status: syncproto.StatusOK, Generation: h.Generation,
		AckedEntries: 1 << 53, TailAcked: true}
	if n := len(h.Entries); n > 0 {
		resp.AckedOffset = h.Entries[n-1].End()
	}
	if t := h.Tail; t != nil {
		resp.AckedOffset, resp.TailOffset, resp.TailSize = t.Offset, t.Offset, t.Size
	}
	return resp
}

// store reserves a chunk and puts its object, the verified zstd frame z,
// unless it is committed already. It holds the chunk's advisory lock until the request ends.
//
// The lock is only tried, never waited for: bodies arrive in the device's
// order, so two requests carrying the same chunks in opposite orders
// would each hold one lock and wait for the other (a deadlock, 40P01).
// When another request (or the orphan reconciler) holds the chunk, this
// one skips it; the commit then lists it in Missing unless the other
// request committed it first, and the device sends it again.
func (f *flush) store(ctx context.Context, b syncproto.Body, z []byte) error {
	if nh, ok := f.moved[b.Hash]; ok {
		// The verified old bytes prove possession of the rewritten chunk,
		// which the entries now name; the old bytes are not stored.
		f.sizes[nh] = b.Size
		return nil
	}
	if f.reserved[b.Hash] {
		return nil
	}
	f.sizes[b.Hash] = b.Size
	key := store.ChunkLockKey(b.Hash[:])
	if !slices.Contains(f.locked, key) {
		var got bool
		if err := f.conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,0))`, key).Scan(&got); err != nil {
			return err
		}
		if !got {
			return nil
		}
		f.locked = append(f.locked, key)
	}
	skip := false
	err := pgx.BeginFunc(ctx, f.conn, func(tx pgx.Tx) error {
		var state string
		err := tx.QueryRow(ctx, `SELECT state FROM chunks WHERE hash=$1 FOR UPDATE`, b.Hash[:]).Scan(&state)

		// Redaction locks the old chunk before installing its redirect. Check
		// after taking that same row lock so an upload cannot take a rewritten
		// chunk back from its purge job using an earlier redirect snapshot.
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		moved, lookupErr := redirects(ctx, tx, []syncproto.Hash{b.Hash})
		if lookupErr != nil {
			return lookupErr
		}
		if len(moved) > 0 {
			return redirectChanged()
		}
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			_, err = tx.Exec(ctx, `INSERT INTO chunks(hash,size,stored_size,encoding,object_key,state,uploaded_by_device,cleanup_after)
			VALUES($1,$2,$3,$4,$5,'uploading',$6,now()+$7::interval) ON CONFLICT (hash) DO NOTHING`,
				b.Hash[:], b.Size, len(z), syncproto.Encoding, ChunkKey(b.Hash), f.deviceID, reservationGrace.String())
		case err != nil:
		case state == "committed":
			skip = true
			return nil
		case state == "purging_delete":
			// A purge pass is deleting the object right now; retry after it.
			return &Error{http.StatusServiceUnavailable, "chunk_purging", "chunk is being purged; retry later"}
		case state == "deletion_pending":
			// A deletion job that failed or waits to retry owns the row; its
			// object may be gone already. This upload takes the chunk back, so a
			// stuck job never blocks a device: the job purges only the rows it
			// still owns.
			var tag pgconn.CommandTag
			tag, err = tx.Exec(ctx, `UPDATE chunks SET state='uploading',deletion_job_id=NULL,uploaded_by_device=$2,cleanup_after=now()+$3::interval,stored_size=$4,updated_at=now()
			WHERE hash=$1 AND state='deletion_pending'`, b.Hash[:], f.deviceID, reservationGrace.String(), len(z))
			if err == nil && tag.RowsAffected() == 0 {
				return &Error{http.StatusServiceUnavailable, "chunk_purging", "chunk is being purged; retry later"}
			}
		default: // an abandoned reservation or cleanup: take it over
			_, err = tx.Exec(ctx, `UPDATE chunks SET state='uploading',uploaded_by_device=$2,cleanup_after=now()+$3::interval,stored_size=$4,updated_at=now() WHERE hash=$1`,
				b.Hash[:], f.deviceID, reservationGrace.String(), len(z))
		}
		return err
	})
	if err != nil {
		return err
	}
	if skip {
		return nil
	}
	if err := f.s.Objects.Put(ctx, ChunkKey(b.Hash), z); err != nil {
		return &Error{http.StatusServiceUnavailable, "object_store_unavailable", err.Error()}
	}
	f.reserved[b.Hash] = true
	return nil
}

// redirectChanged asks the device to retry with a fresh redirect snapshot.
func redirectChanged() *Error {
	return &Error{http.StatusServiceUnavailable, "chunk_redirect_changed", "chunk was redacted during upload; retry"}
}

func (f *flush) unlock() {
	locks := make([]store.AdvisoryLock, 0, len(f.locked))
	for _, key := range f.locked {
		locks = append(locks, store.AdvisoryLock{Key: key})
	}
	// Never return a connection holding a lock to the pool.
	_ = store.ReleaseAdvisoryLocks(f.conn, f.s.Pool, locks...)
}

// commit applies steps 2 to 5 in the manifest transaction and returns the
// sources to parse.
func (f *flush) commit(ctx context.Context, tx pgx.Tx, tailData []byte) (*syncproto.FlushResponse, []string, error) {
	h := f.h
	src := h.Source
	// A protected companion may establish ownership of its as-yet absent
	// parent. Serialize that decision against every same-device manifest,
	// before checking identity or acquiring source and session locks.
	gateSQL := `SELECT pg_advisory_xact_lock_shared(hashtextextended($1,0))`
	if src.Parent != nil {
		gateSQL = `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`
	}
	if _, err := tx.Exec(ctx, gateSQL, policyDeviceLockKey(f.deviceID)); err != nil {
		return nil, nil, err
	}
	policyIDs, err := checkFlushPolicy(ctx, tx, f.deviceID, src)
	if err != nil {
		return nil, nil, err
	}
	var policySchema bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('source_policy_identity') IS NOT NULL`).Scan(&policySchema); err != nil {
		return nil, nil, err
	}
	if policySchema {
		if len(policyIDs) > 0 && src.Agent == "claude" && src.Parent == nil && src.StorageKind != "cass_export" {
			owner, err := policySourceSession(ctx, tx, f.deviceID, src)
			if err != nil {
				return nil, nil, err
			}
			if validPolicySession(owner) {
				if err := bindPolicySourceOwner(ctx, tx, f.deviceID, src.Agent, owner, syncproto.PolicySource{Path: src.Path, FileID: src.FileID, Generation: h.Generation}, false); err != nil {
					return nil, nil, err
				}
			}
		}
		if err := bindPolicyCaptureIdentity(ctx, tx, f.deviceID, src); err != nil {
			return nil, nil, err
		}
	}
	if len(policyIDs) > 0 && (len(h.Entries) > 0 || h.Tail != nil && h.Tail.Size > 0) {
		ownSession, err := policySourceSession(ctx, tx, f.deviceID, src)
		if err != nil {
			return nil, nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE session_policy_placements SET evidence_scope=CASE WHEN evidence_scope='none' THEN 'mapped' ELSE evidence_scope END WHERE device_id=$1 AND agent=$2 AND session_id=$3 AND current_mapping_known AND jsonb_array_length(placements)>0`, f.deviceID, src.Agent, ownSession); err != nil {
			return nil, nil, err
		}
	}
	var parentPath, parentFileID *string
	if src.Parent != nil {
		parentPath, parentFileID = &src.Parent.Path, &src.Parent.FileID
	}
	var prevPath, prevFileID *string
	if src.Previous != nil {
		prevPath, prevFileID = &src.Previous.Path, &src.Previous.FileID
	}
	kind := src.StorageKind
	if src.Parent != nil {
		kind = string(transcript.StorageCompanion)
	}
	var sourceID string
	var tombstoned bool
	var refused *string
	// The first statement shares the redacted-lines lock until commit, in
	// the same round trip as the source upsert. Whether this upload needs
	// an at-rest repair (archive_redaction_work below) is then decided
	// either after a message redaction committed (its lines are visible)
	// or before it takes the lock (its QueueArchiveRepair sees this
	// upload's pending parse). Taking it first keeps the order every
	// redacted-lines writer uses: the lock before any row lock.
	b := &pgx.Batch{}
	b.Queue(`SELECT pg_advisory_xact_lock_shared(hashtextextended($1,0))`, redactedLinesLock)
	b.Queue(`INSERT INTO sources(id,device_id,agent,path,file_id,session_key,storage_kind,parser,first_seen_at,parent_path,parent_file_id,previous_path,previous_file_id,checkout,remote)
		VALUES($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,now(),$9,$10,$11,$12,NULLIF($13,''),NULLIF($14,''))
		ON CONFLICT (device_id,path,file_id) DO UPDATE SET agent=excluded.agent,session_key=excluded.session_key,
			storage_kind=excluded.storage_kind,parser=excluded.parser,parent_path=excluded.parent_path,parent_file_id=excluded.parent_file_id,
			previous_path=COALESCE(excluded.previous_path,sources.previous_path),previous_file_id=COALESCE(excluded.previous_file_id,sources.previous_file_id),
			checkout=excluded.checkout,remote=excluded.remote
		RETURNING id::text, tombstoned_at IS NOT NULL, refused_rule`,
		uuid.NewString(), f.deviceID, src.Agent, src.Path, src.FileID, src.SessionKey, kind, src.Parser, parentPath, parentFileID, prevPath, prevFileID, src.Checkout, src.Remote)
	br := tx.SendBatch(ctx, b)
	if _, err := br.Exec(); err != nil {
		br.Close()
		return nil, nil, err
	}
	if err := br.QueryRow().Scan(&sourceID, &tombstoned, &refused); err != nil {
		br.Close()
		return nil, nil, err
	}
	if err := br.Close(); err != nil {
		return nil, nil, err
	}
	if tombstoned { // deleted or refused since the pre-check
		resp := swallow(h)
		resp.Refused = deref(refused)
		return resp, nil, nil
	}
	for _, policyID := range policyIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO source_policy_placements(device_id,agent,session_id,path,file_id,generation) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, f.deviceID, src.Agent, policyID, src.Path, src.FileID, h.Generation); err != nil {
			return nil, nil, err
		}
	}
	if err := f.link(ctx, tx, sourceID); err != nil {
		return nil, nil, err
	}

	resp := &syncproto.FlushResponse{Version: syncproto.Version, Status: syncproto.StatusOK}
	// Step 2: generation.
	var latest int64 = -1
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(generation),-1) FROM generations WHERE source_id=$1`, sourceID).Scan(&latest); err != nil {
		return nil, nil, err
	}
	if h.Generation < latest {
		resp.Status, resp.Generation = syncproto.StatusStaleGeneration, latest
		return resp, nil, nil
	}
	changed := false
	if h.Generation > latest {
		if _, err := tx.Exec(ctx, `INSERT INTO generations(source_id,generation,size,change_time,captured_at,complete) VALUES($1,$2,0,$3,$4,true)`,
			sourceID, h.Generation, nullTime(h.ChangeTime), h.CapturedAt); err != nil {
			return nil, nil, err
		}
		latest, changed = h.Generation, true
	}
	resp.Generation = latest

	// Step 3: entries.
	n, end, err := manifestEnd(ctx, tx, sourceID, h.Generation)
	if err != nil {
		return nil, nil, err
	}
	// Serialize both existing and new entry hashes against redaction.
	// Never acknowledge or compare a manifest using an obsolete redirect;
	// the next request can resolve the new target and prove possession anew.
	states, sizes, err := lockChunks(ctx, tx, h.Entries, 0)
	if err != nil {
		return nil, nil, err
	}
	named := make([]syncproto.Hash, 0, len(h.Entries))
	for _, e := range h.Entries {
		named = append(named, e.Hash)
	}
	moved, err := redirects(ctx, tx, named)
	if err != nil {
		return nil, nil, err
	}
	if len(moved) > 0 {
		return nil, nil, redirectChanged()
	}
	repairFrom := end
	all := true
	var add []syncproto.Entry
	var committed []syncproto.Hash
	var check []int64
	for _, e := range h.Entries {
		if e.Ordinal < n {
			check = append(check, e.Ordinal)
		}
	}
	if len(check) > 0 {
		ok, err := matchCommitted(ctx, tx, sourceID, h.Generation, h.Entries, check, n)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			resp.Status, all = syncproto.StatusNewGeneration, false
		}
	}
	if all {
		// A committed chunk is referenced without its body only when the
		// user already possesses it; otherwise the body must come along.
		var fresh []syncproto.Hash
		for _, e := range h.Entries {
			if e.Ordinal >= n && states[e.Hash] == "committed" && f.sizes[e.Hash] == 0 {
				fresh = append(fresh, e.Hash)
			}
		}
		mine, err := owned(ctx, tx, f.deviceID, fresh)
		if err != nil {
			return nil, nil, err
		}
		next := n
		for _, e := range h.Entries {
			if e.Ordinal < n {
				continue
			}
			if e.Ordinal > next {
				all = false // a gap: the device re-sends from AckedEntries
				break
			}
			st := states[e.Hash]
			held := st == "committed" && (f.sizes[e.Hash] > 0 || mine[e.Hash])
			if !(held || st == "uploading" && f.reserved[e.Hash]) {
				resp.Missing = append(resp.Missing, e.Hash)
				all = false
				break
			}
			if e.Offset != end {
				return nil, nil, badRequest("entry offset does not continue the manifest")
			}
			if sizes[e.Hash] != e.Size {
				// The manifest's byte arithmetic trusts entry sizes; a lie
				// would misplace every later offset.
				return nil, nil, badRequest("entry %d: size %d does not match the chunk's %d bytes", e.Ordinal, e.Size, sizes[e.Hash])
			}
			if st == "uploading" {
				committed = append(committed, e.Hash)
				states[e.Hash] = "committed"
			}
			add = append(add, e)
			end, next = e.End(), next+1
		}
		if err := appendEntries(ctx, tx, sourceID, h.Generation, add, committed); err != nil {
			return nil, nil, err
		}
		n = next
		changed = changed || len(add) > 0
	}

	// Step 4: tail.
	var tailOff, tailSize int64 = -1, 0
	var stored []byte
	var tailRedaction *string
	forceWholeTail := false
	err = tx.QueryRow(ctx, `SELECT byte_offset,bytes,message_redaction_id::text FROM provisional_tails WHERE source_id=$1 AND generation=$2 FOR UPDATE`, sourceID, h.Generation).Scan(&tailOff, &stored, &tailRedaction)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, err
	}
	if tailOff >= 0 && tailOff < end {
		if _, err := tx.Exec(ctx, `DELETE FROM provisional_tails WHERE source_id=$1 AND generation=$2`, sourceID, h.Generation); err != nil {
			return nil, nil, err
		}
		tailOff, stored, tailRedaction = -1, nil, nil
	}
	if all {
		switch t := h.Tail; {
		case t == nil:
			resp.TailAcked = true
		case t.Offset == end:
			base := stored
			if tailOff != t.Offset {
				base = nil
			}
			whole, err := t.Apply(base, tailData)
			if errors.Is(err, syncproto.ErrTailMismatch) {
				if tailRedaction != nil && t.From > 0 {
					// Its prefix was masked, not lost. Ask for a verified
					// full tail in this generation; keep the stored masked
					// bytes available until that retry arrives.
					forceWholeTail = true
					break
				}
				resp.Status = syncproto.StatusNewGeneration
				break
			}
			if err != nil {
				break // base missing: TailOffset/TailSize say what to resend
			}
			if tailOff != t.Offset || int64(len(stored)) <= t.Size {
				if _, err := tx.Exec(ctx, `INSERT INTO provisional_tails(source_id,generation,byte_offset,bytes,updated_at) VALUES($1,$2,$3,$4,now())
					ON CONFLICT (source_id,generation) DO UPDATE SET byte_offset=excluded.byte_offset,bytes=excluded.bytes,message_redaction_id=NULL,updated_at=now()`,
					sourceID, h.Generation, t.Offset, whole); err != nil {
					return nil, nil, err
				}
				if !bytes.Equal(stored, whole) || tailOff != t.Offset {
					changed = true
					repairFrom = min(repairFrom, t.Offset)
				}
				tailOff, stored = t.Offset, whole
			}
			resp.TailAcked = true
		}
	}
	if tailOff >= 0 {
		tailSize = int64(len(stored))
		resp.TailOffset, resp.TailSize = tailOff, tailSize
		if forceWholeTail {
			resp.TailOffset, resp.TailSize = -1, 0
		}
	}
	if resp.Status == syncproto.StatusOK && !(all && resp.TailAcked) {
		resp.Status = syncproto.StatusPartial
	}
	resp.AckedEntries, resp.AckedOffset = n, end

	if r := h.Redaction; r != nil {
		// The device's cumulative record for the generation: last one wins.
		counts := r.Counts
		if counts == nil {
			counts = map[string]int64{}
		}
		if _, err := tx.Exec(ctx, `UPDATE generations SET redaction_rules=$3,redactions=$4 WHERE source_id=$1 AND generation=$2`,
			sourceID, h.Generation, r.Rules, counts); err != nil {
			return nil, nil, err
		}
	}
	if !changed {
		return resp, nil, nil
	}
	size := end
	if tailOff >= 0 {
		size = tailOff + tailSize
	}
	if _, err := tx.Exec(ctx, `UPDATE generations SET size=$3,complete=$4,captured_at=$5,change_time=COALESCE($6,change_time) WHERE source_id=$1 AND generation=$2`,
		sourceID, h.Generation, size, tailOff < 0, h.CapturedAt, nullTime(h.ChangeTime)); err != nil {
		return nil, nil, err
	}
	if err := checkQuota(ctx, tx, f.deviceID); err != nil {
		return nil, nil, err
	}
	// Under the shared redacted-lines lock (first statement): a redaction
	// that committed before is visible here; one that commits later queues
	// this source itself (QueueArchiveRepair).
	if _, err := tx.Exec(ctx, `INSERT INTO archive_redaction_work(source_id,generation,from_offset)
 SELECT $1,$2,$3 WHERE EXISTS(SELECT 1 FROM redacted_lines)
 ON CONFLICT(source_id,generation) DO UPDATE SET from_offset=LEAST(archive_redaction_work.from_offset,EXCLUDED.from_offset),revision=archive_redaction_work.revision+1`, sourceID, h.Generation, repairFrom); err != nil {
		return nil, nil, err
	}
	return resp, []string{sourceID}, requestParse(ctx, tx, sourceID, false)
}

// link records the companion and previous-source links of a source and
// resolves companions that arrived before it.
func (f *flush) link(ctx context.Context, tx pgx.Tx, sourceID string) error {
	src := f.h.Source
	if p := src.Parent; p != nil {
		if _, err := tx.Exec(ctx, `UPDATE sources SET parent_source_id=(SELECT id FROM sources
			WHERE device_id=$2 AND path=$3 AND ($4='' OR file_id=$4) AND id<>$1 ORDER BY first_seen_at DESC LIMIT 1)
			WHERE id=$1 AND parent_source_id IS NULL`, sourceID, f.deviceID, p.Path, p.FileID); err != nil {
			return err
		}
	}
	if p := src.Previous; p != nil {
		if _, err := tx.Exec(ctx, `UPDATE sources SET previous_source_id=(SELECT id FROM sources WHERE device_id=$2 AND path=$3 AND file_id=$4 AND id<>$1)
			WHERE id=$1 AND previous_source_id IS NULL`, sourceID, f.deviceID, p.Path, p.FileID); err != nil {
			return err
		}
	}
	// D20: a source that named this one as its previous before this one
	// arrived links now; this source's parse then supersedes its rows.
	if _, err := tx.Exec(ctx, `UPDATE sources SET previous_source_id=$1 WHERE device_id=$2 AND previous_path=$3 AND previous_file_id=$4
		AND previous_source_id IS NULL AND id<>$1`, sourceID, f.deviceID, src.Path, src.FileID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE sources SET parent_source_id=$1 WHERE device_id=$2 AND parent_path=$3
		AND parent_source_id IS NULL AND (COALESCE(parent_file_id,'')='' OR parent_file_id=$4) AND id<>$1`,
		sourceID, f.deviceID, src.Path, src.FileID); err != nil {
		return err
	}
	// A Claude subagent transcript adopts its agent-<id>.meta.json sidecar,
	// whatever parent the device named when it uploaded the sidecar first.
	if strings.HasSuffix(src.Path, ".jsonl") {
		if _, err := tx.Exec(ctx, `UPDATE sources SET parent_source_id=$1 WHERE device_id=$2 AND path=$3 AND storage_kind='companion' AND id<>$1`,
			sourceID, f.deviceID, strings.TrimSuffix(src.Path, ".jsonl")+".meta.json"); err != nil {
			return err
		}
	}
	return nil
}

// requestParse bumps a source's parse request inside the caller's
// transaction, so the work is durable with the evidence. It leaves the
// failure count and backoff alone: a live session flushes every few
// seconds, and a source whose parse fails must still back off.
func requestParse(ctx context.Context, tx pgx.Tx, sourceID string, reparse bool) error {
	_, err := tx.Exec(ctx, `INSERT INTO source_parse_state(source_id,reparse) VALUES($1,$2)
		ON CONFLICT (source_id) DO UPDATE SET requested_seq=source_parse_state.requested_seq+1,
			reparse=source_parse_state.reparse OR excluded.reparse,requested_at=now()`, sourceID, reparse)
	return err
}

// manifestEnd is the number of a generation's manifest entries and the
// byte where they end. Ordinals run from 0 without gaps (appendEntries
// adds only the next ones), so the last entry tells both.
func manifestEnd(ctx context.Context, tx pgx.Tx, sourceID string, gen int64) (n, end int64, err error) {
	err = tx.QueryRow(ctx, `SELECT m.ordinal+1,m.byte_offset+(SELECT size FROM chunks WHERE hash=m.chunk_hash) FROM manifest_entries m
		WHERE m.source_id=$1 AND m.generation=$2 ORDER BY m.ordinal DESC LIMIT 1`, sourceID, gen).Scan(&n, &end)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, nil
	}
	return n, end, err
}

// matchCommitted reports whether the request's entries at the given
// ordinals equal the committed ones (hash and offset).
func matchCommitted(ctx context.Context, tx pgx.Tx, sourceID string, gen int64, entries []syncproto.Entry, ordinals []int64, n int64) (bool, error) {
	rows, err := tx.Query(ctx, `SELECT ordinal,chunk_hash,byte_offset FROM manifest_entries WHERE source_id=$1 AND generation=$2 AND ordinal=ANY($3)`, sourceID, gen, ordinals)
	if err != nil {
		return false, err
	}
	got := map[int64]syncproto.Entry{}
	for rows.Next() {
		var e syncproto.Entry
		var h []byte
		if err := rows.Scan(&e.Ordinal, &h, &e.Offset); err != nil {
			rows.Close()
			return false, err
		}
		e.Hash = syncproto.Hash(h)
		got[e.Ordinal] = e
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, e := range entries {
		if c, ok := got[e.Ordinal]; e.Ordinal < n && (!ok || c.Hash != e.Hash || c.Offset != e.Offset) {
			return false, nil
		}
	}
	return true, nil
}

// lockChunks locks the chunk rows of the new entries (in hash order) and
// returns their states and sizes.
func lockChunks(ctx context.Context, tx pgx.Tx, entries []syncproto.Entry, n int64) (map[syncproto.Hash]string, map[syncproto.Hash]int64, error) {
	var hs [][]byte
	for _, e := range entries {
		if e.Ordinal >= n {
			hs = append(hs, e.Hash[:])
		}
	}
	states, sizes := map[syncproto.Hash]string{}, map[syncproto.Hash]int64{}
	if len(hs) == 0 {
		return states, sizes, nil
	}
	rows, err := tx.Query(ctx, `SELECT hash,state,size FROM chunks WHERE hash=ANY($1) ORDER BY hash FOR UPDATE`, hs)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var h []byte
		var st string
		var size int64
		if err := rows.Scan(&h, &st, &size); err != nil {
			return nil, nil, err
		}
		states[syncproto.Hash(h)], sizes[syncproto.Hash(h)] = st, size
	}
	return states, sizes, rows.Err()
}

func appendEntries(ctx context.Context, tx pgx.Tx, sourceID string, gen int64, add []syncproto.Entry, committed []syncproto.Hash) error {
	if len(add) == 0 {
		return nil
	}
	ords, offs := make([]int64, len(add)), make([]int64, len(add))
	hs := make([][]byte, len(add))
	for i, e := range add {
		ords[i], offs[i], hs[i] = e.Ordinal, e.Offset, e.Hash[:]
	}
	if _, err := tx.Exec(ctx, `INSERT INTO manifest_entries(source_id,generation,ordinal,chunk_hash,byte_offset)
		SELECT $1,$2,o,h,b FROM unnest($3::bigint[],$4::bytea[],$5::bigint[]) AS x(o,h,b)`, sourceID, gen, ords, hs, offs); err != nil {
		return err
	}
	if len(committed) == 0 {
		return nil
	}
	cs := make([][]byte, len(committed))
	for i := range committed {
		cs[i] = committed[i][:]
	}
	// committed lists the chunks lockChunks found 'uploading' and still
	// holds locked. A state='uploading' filter would let the planner read
	// chunks_orphan_idx, past the dead entries of every chunk committed
	// since the last vacuum.
	_, err := tx.Exec(ctx, `UPDATE chunks SET state='committed',cleanup_after=NULL,last_error_class='',updated_at=now() WHERE hash=ANY($1)`, cs)
	return err
}

// checkQuota fails the flush when stored bytes exceed the collection
// policy. It reads the storage_usage counters that triggers keep on chunks
// and tails (reservations included), so it costs two small reads and takes
// no global lock; concurrent flushes can overshoot by what they have in
// flight. The device retries (507 is transient).
func checkQuota(ctx context.Context, tx pgx.Tx, deviceID string) error {
	var maxAll, maxUser int64
	if err := tx.QueryRow(ctx, `SELECT max_storage_bytes,max_user_bytes FROM collection_policy WHERE singleton`).Scan(&maxAll, &maxUser); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	if maxAll == 0 && maxUser == 0 {
		return nil
	}
	var total, user int64
	err := tx.QueryRow(ctx, `SELECT (SELECT COALESCE(sum(bytes),0) FROM storage_usage)::bigint,
		COALESCE((SELECT bytes FROM storage_usage WHERE owner=(SELECT user_id::text FROM devices WHERE id=$1)),0)`, deviceID).Scan(&total, &user)
	if err != nil {
		return err
	}
	if maxAll > 0 && total > maxAll || maxUser > 0 && user > maxUser {
		return &Error{http.StatusInsufficientStorage, "quota_exceeded", "storage quota exceeded; collection paused"}
	}
	return nil
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
