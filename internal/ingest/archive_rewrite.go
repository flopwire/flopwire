package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/flopwire/flopwire/internal/redact"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ArchiveMask is a set of absolute byte spans against one generation snapshot.
type ArchiveMask struct {
	Generation *Generation
	Spans      []redact.Span
}

type rewriteChunk struct {
	old, new syncproto.Hash
	stored   int
}
type rewriteTail struct {
	source   string
	gen, off int64
	old, new []byte
}

// ArchiveRewrite prepares object writes outside the database transaction, then
// atomically installs their redirects and hands old objects to the purge worker.
type ArchiveRewrite struct {
	chunks []rewriteChunk
	tails  []rewriteTail
	conn   *pgxpool.Conn
	pool   *pgxpool.Pool
	locked []string
	// reserved is the replacements this plan put as 'uploading'.
	reserved [][]byte
}

type ArchiveRewriteResult struct {
	Chunks, Tails int
	JobID         string
}

// ErrArchiveChanged means the snapshots lost a race; reload and prepare again.
var ErrArchiveChanged error = archiveChangedError{}

type archiveChangedError struct{}

func (archiveChangedError) Error() string   { return "ingest: archive changed during rewrite; retry" }
func (archiveChangedError) Retryable() bool { return true }

func PrepareArchiveRewrite(ctx context.Context, pool *pgxpool.Pool, objects Objects, masks []ArchiveMask) (*ArchiveRewrite, error) {
	type fix struct {
		chunk Chunk
		spans []redact.Span
	}
	fixes := map[syncproto.Hash]*fix{}
	plan := &ArchiveRewrite{pool: pool}
	for _, m := range masks {
		g := m.Generation
		for _, c := range g.Entries {
			sp := archiveClip(m.Spans, c.Offset, c.Offset+c.Size)
			if len(sp) == 0 {
				continue
			}
			f := fixes[c.Hash]
			if f == nil {
				f = &fix{chunk: c}
				fixes[c.Hash] = f
			}
			f.spans = append(f.spans, sp...)
		}
		if g.Tail != nil {
			sp := archiveClip(m.Spans, g.TailOffset, g.TailOffset+int64(len(g.Tail)))
			if len(sp) > 0 {
				out := bytes.Clone(g.Tail)
				redact.FillSpans(out, sp, redact.MessageRule)
				if !bytes.Equal(out, g.Tail) {
					plan.tails = append(plan.tails, rewriteTail{g.SourceID, g.Generation, g.TailOffset, g.Tail, out})
				}
			}
		}
	}
	{
		conn, err := pool.Acquire(ctx)
		if err != nil {
			return nil, err
		}
		plan.conn = conn
		if err := store.PinBackend(ctx, conn); err != nil {
			conn.Release()
			return nil, err
		}
		var got bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock_shared($1)`, store.PurgeLockID).Scan(&got); err != nil {
			conn.Release()
			return nil, err
		}
		if !got {
			conn.Release()
			return nil, ErrArchiveChanged
		}
	}
	complete := false
	defer func() {
		if !complete {
			plan.Close()
		}
	}()
	for h, f := range fixes {
		data, err := GetChunk(ctx, objects, f.chunk)
		if err != nil {
			return nil, err
		}
		redact.FillSpans(data, f.spans, redact.MessageRule)
		nh := syncproto.Sum(data)
		if nh == h {
			continue
		}
		z := syncproto.Compress(nil, data)
		target, stored, err := plan.reserveReplacement(ctx, objects, h, nh, z)
		if err != nil {
			return nil, err
		}
		plan.chunks = append(plan.chunks, rewriteChunk{old: h, new: target, stored: stored})
	}
	slices.SortFunc(plan.chunks, func(a, b rewriteChunk) int { return bytes.Compare(a.old[:], b.old[:]) })
	complete = true
	return plan, nil
}

// Counts reports the prepared replacements for the request's audit record.
func (p *ArchiveRewrite) Counts() (chunks, tails int) { return len(p.chunks), len(p.tails) }

// Apply requires an existing message_redactions row. The replacement keeps the
// old chunk's quota owner. All references move together; no request body or
// object-storage operation runs while row locks are held.
func (p *ArchiveRewrite) Apply(ctx context.Context, tx pgx.Tx, redactionID, requestedBy string) (ArchiveRewriteResult, error) {
	out := ArchiveRewriteResult{Chunks: len(p.chunks), Tails: len(p.tails)}
	olds := make([][]byte, 0, len(p.chunks))
	locks := make([][]byte, 0, 2*len(p.chunks))
	for _, c := range p.chunks {
		olds = append(olds, c.old[:])
		locks = append(locks, c.old[:], c.new[:])
	}
	if len(locks) > 0 {
		// Both sides use one order. NOWAIT avoids waiting on an overlapping
		// flush or another rewrite while already holding part of the set.
		rows, err := tx.Query(ctx, `SELECT hash,state FROM chunks WHERE hash=ANY($1) ORDER BY hash FOR UPDATE NOWAIT`, locks)
		if err != nil {
			return out, archiveLockError(err)
		}
		states := map[syncproto.Hash]string{}
		for rows.Next() {
			var h []byte
			var state string
			if err := rows.Scan(&h, &state); err != nil {
				rows.Close()
				return out, err
			}
			states[syncproto.Hash(h)] = state
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return out, archiveLockError(err)
		}
		for _, c := range p.chunks {
			if states[c.old] != "committed" || states[c.new] == "purging_delete" || states[c.new] == "purging" || states[c.new] == "uploading" && !slices.Contains(p.locked, store.ChunkLockKey(c.new[:])) {
				return out, ErrArchiveChanged
			}
		}
		named := hashes(p.chunks)
		for _, c := range p.chunks {
			named = append(named, c.new)
		}
		moved, err := redirects(ctx, tx, named)
		if err != nil {
			return out, err
		}
		if len(moved) > 0 {
			return out, ErrArchiveChanged
		}
		for _, c := range p.chunks {
			if _, err := tx.Exec(ctx, `INSERT INTO chunks(hash,size,stored_size,encoding,object_key,state,uploaded_by_device) SELECT $1,size,$4,$5,$2,'committed',uploaded_by_device FROM chunks WHERE hash=$3
    ON CONFLICT (hash) DO UPDATE SET state='committed',deletion_job_id=NULL,stored_size=EXCLUDED.stored_size,updated_at=now() WHERE chunks.state<>'committed'`, c.new[:], ChunkKey(c.new), c.old[:], c.stored, syncproto.Encoding); err != nil {
				return out, err
			}
		}
		targets := map[syncproto.Hash]syncproto.Hash{}
		for _, c := range p.chunks {
			targets[c.old] = c.new
		}
		for _, c := range p.chunks {
			target := c.new
			for steps := 0; ; steps++ {
				next, ok := targets[target]
				if !ok {
					break
				}
				if steps >= len(targets) {
					return out, fmt.Errorf("ingest: cyclic archive rewrite")
				}
				target = next
			}
			if _, err := tx.Exec(ctx, `UPDATE manifest_entries SET chunk_hash=$2 WHERE chunk_hash=$1`, c.old[:], target[:]); err != nil {
				return out, err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO chunk_redirects(old_hash,new_hash,redaction_id) VALUES($1,$2,$3) ON CONFLICT (old_hash) DO UPDATE SET new_hash=EXCLUDED.new_hash,redaction_id=EXCLUDED.redaction_id`, c.old[:], target[:], redactionID); err != nil {
				return out, err
			}
			if _, err := tx.Exec(ctx, `UPDATE chunk_redirects SET new_hash=$2 WHERE new_hash=$1`, c.old[:], target[:]); err != nil {
				return out, err
			}
		}
	}
	for _, t := range p.tails {
		tag, err := tx.Exec(ctx, `UPDATE provisional_tails SET bytes=$4,message_redaction_id=$6,updated_at=now() WHERE source_id=$1 AND generation=$2 AND byte_offset=$3 AND bytes=$5`, t.source, t.gen, t.off, t.new, t.old, redactionID)
		if err != nil {
			return out, err
		}
		if tag.RowsAffected() != 1 {
			return out, ErrArchiveChanged
		}
	}
	if len(olds) > 0 {
		out.JobID = uuid.NewString()
		if _, err := tx.Exec(ctx, `INSERT INTO deletion_jobs(id,redaction_id,requested_by,state,requested_at) VALUES($1,$2,$3,'queued',$4)`, out.JobID, redactionID, requestedBy, time.Now().UTC()); err != nil {
			return out, err
		}
		if _, err := tx.Exec(ctx, `UPDATE chunks SET state='deletion_pending',deletion_job_id=$2,updated_at=now() WHERE hash=ANY($1) AND state='committed' AND NOT EXISTS(SELECT 1 FROM manifest_entries m WHERE m.chunk_hash=chunks.hash)`, olds, out.JobID); err != nil {
			return out, err
		}
	}
	return out, nil
}

// WithTx applies the plan on its fenced connection, requiring only one pool slot.
func (p *ArchiveRewrite) WithTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, p.conn, fn)
}

// Close releases the object-write fences even after an abandoned plan.
// Reserved objects then remain in the existing orphan ledger for cleanup.
// A replacement that was never installed may hold a line a newer catalog
// redacts (the plan was derived from an older one), so it is due for
// cleanup at once rather than after the upload grace; the chunk lock held
// here means no upload owns the reservation meanwhile. Installed ones are
// 'committed' and stay.
func (p *ArchiveRewrite) Close() {
	if p.conn == nil {
		return
	}
	if len(p.reserved) > 0 {
		// Best effort: on failure the reconciler still finds them after
		// the upload grace.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = p.conn.Exec(ctx, `UPDATE chunks SET cleanup_after=now(),updated_at=now() WHERE hash=ANY($1) AND state='uploading'`, p.reserved)
		cancel()
	}
	locks := []store.AdvisoryLock{{ID: store.PurgeLockID, Shared: true}}
	for _, key := range p.locked {
		locks = append(locks, store.AdvisoryLock{Key: key})
	}
	_ = store.ReleaseAdvisoryLocks(p.conn, p.pool, locks...)
	p.conn.Release()
	p.conn = nil
}

func (p *ArchiveRewrite) reserveReplacement(ctx context.Context, objects Objects, old, nu syncproto.Hash, z []byte) (syncproto.Hash, int, error) {
	original := nu
	seen := map[syncproto.Hash]bool{}
	for {
		if seen[nu] {
			return nu, 0, fmt.Errorf("ingest: cyclic chunk redirect")
		}
		seen[nu] = true
		moved, err := redirects(ctx, p.conn, []syncproto.Hash{nu})
		if err != nil {
			return nu, 0, err
		}
		next, ok := moved[nu]
		if !ok {
			break
		}
		nu = next
	}
	key := store.ChunkLockKey(nu[:])
	if !slices.Contains(p.locked, key) {
		var got bool
		if err := p.conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,0))`, key).Scan(&got); err != nil {
			return nu, 0, err
		}
		if !got {
			return nu, 0, ErrArchiveChanged
		}
		p.locked = append(p.locked, key)
	}
	var state string
	var stored int
	err := p.conn.QueryRow(ctx, `SELECT state,stored_size FROM chunks WHERE hash=$1`, nu[:]).Scan(&state, &stored)
	if err == nil && state == "committed" {
		return nu, stored, nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nu, 0, err
	}
	// A redirected target is reusable only when its bytes are durable. The
	// next scan resolves all line masks before deriving another replacement.
	if nu != original {
		return nu, 0, ErrArchiveChanged
	}
	tag, err := p.conn.Exec(ctx, `INSERT INTO chunks(hash,size,stored_size,encoding,object_key,state,uploaded_by_device,cleanup_after)
 SELECT $1,size,$3,$4,$5,'uploading',uploaded_by_device,now()+$6::interval FROM chunks WHERE hash=$2 AND state='committed'
 ON CONFLICT(hash) DO UPDATE SET state='uploading',deletion_job_id=NULL,stored_size=EXCLUDED.stored_size,uploaded_by_device=EXCLUDED.uploaded_by_device,cleanup_after=EXCLUDED.cleanup_after,updated_at=now()`, nu[:], old[:], len(z), syncproto.Encoding, ChunkKey(nu), reservationGrace.String())
	if err != nil {
		return nu, 0, err
	}
	if tag.RowsAffected() != 1 {
		return nu, 0, ErrArchiveChanged
	}
	p.reserved = append(p.reserved, bytes.Clone(nu[:]))
	if err := objects.Put(ctx, ChunkKey(nu), z); err != nil {
		return nu, 0, err
	}
	return nu, len(z), nil
}

func hashes(chunks []rewriteChunk) []syncproto.Hash {
	out := make([]syncproto.Hash, 0, len(chunks))
	for _, c := range chunks {
		out = append(out, c.old)
	}
	return out
}
func archiveLockError(err error) error {
	var e *pgconn.PgError
	if errors.As(err, &e) && e.Code == "55P03" {
		return ErrArchiveChanged
	}
	return err
}
func archiveClip(spans []redact.Span, lo, hi int64) []redact.Span {
	var out []redact.Span
	for _, sp := range spans {
		s, e := max(int64(sp.Start), lo), min(int64(sp.End), hi)
		if s < e {
			out = append(out, redact.Span{Start: int(s - lo), End: int(e - lo)})
		}
	}
	return out
}
