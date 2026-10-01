package ingest

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"slices"
	"time"

	"github.com/flopwire/flopwire/internal/domain"
	"github.com/flopwire/flopwire/internal/redact"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type archiveWork struct{ gen, from, revision int64 }

// repairUploadedArchive uses the durable parse request's retry/restart path.
// Work is recorded by flush, never by a historical-archive sweep.
func (q *Queue) repairUploadedArchive(ctx context.Context, source string) error {
	rows, err := q.Pool.Query(ctx, `SELECT generation,from_offset,revision FROM archive_redaction_work WHERE source_id=$1 ORDER BY generation`, source)
	if err != nil {
		return err
	}
	work, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (archiveWork, error) {
		var w archiveWork
		err := r.Scan(&w.gen, &w.from, &w.revision)
		return w, err
	})
	if err != nil || len(work) == 0 {
		return err
	}
	masks, err := loadLineMasks(ctx, q.Pool)
	if err != nil {
		return err
	}
	for _, w := range work {
		for {
			g, err := LoadGenerationFrom(ctx, q.Pool, source, w.gen, w.from-1)
			if err != nil {
				return err
			}
			r := NewReader(ctx, q.Objects, g)
			start, err := archiveLineStart(r, min(w.from, r.Size()))
			if err != nil {
				return err
			}
			byRedaction := map[string][]redact.Span{}
			next, finished, err := scanArchiveMasks(ctx, r, start, r.Size()-start, masks, byRedaction)
			if err != nil {
				return err
			}
			ids := make([]string, 0, len(byRedaction))
			for id := range byRedaction {
				ids = append(ids, id)
			}
			slices.Sort(ids)
			var spans []redact.Span
			for _, id := range ids {
				spans = append(spans, byRedaction[id]...)
			}
			err = func() error {
				var plan *ArchiveRewrite
				if len(spans) > 0 {
					var err error
					plan, err = PrepareArchiveRewrite(ctx, q.Pool, q.Objects, []ArchiveMask{{Generation: g, Spans: spans}})
					if err != nil {
						return err
					}
					defer plan.Close()
				}
				apply := func(tx pgx.Tx) error {
					if err := lockArchiveWork(ctx, tx, source, w); err != nil {
						return err
					}
					if plan != nil {
						var owner string
						if err := tx.QueryRow(ctx, `SELECT requested_by::text FROM message_redactions WHERE id=$1`, ids[0]).Scan(&owner); err != nil {
							return err
						}
						result, err := plan.Apply(ctx, tx, ids[0], owner)
						if err != nil {
							return err
						}
						if result.Chunks+result.Tails > 0 {
							if err := store.InsertAudit(ctx, tx, domain.AuditEvent{ID: uuid.NewString(), Action: "archive.redaction.rewritten", TargetType: "source", TargetID: source, Metadata: map[string]any{"generation": w.gen, "redaction_ids": ids, "chunks": result.Chunks, "tails": result.Tails, "job_id": result.JobID}, CreatedAt: time.Now().UTC()}); err != nil {
								return err
							}
						}
					}
					if finished {
						_, err := tx.Exec(ctx, `DELETE FROM archive_redaction_work WHERE source_id=$1 AND generation=$2`, source, w.gen)
						return err
					}
					_, err := tx.Exec(ctx, `UPDATE archive_redaction_work SET from_offset=$3,revision=revision+1 WHERE source_id=$1 AND generation=$2`, source, w.gen, next)
					return err
				}
				if plan != nil {
					return plan.WithTx(ctx, apply)
				}
				return pgx.BeginFunc(ctx, q.Pool, apply)
			}()
			if err != nil {
				return err
			}

			if finished {
				break
			}
			w.from = next
			w.revision++
		}
	}
	return nil
}

// Flush takes the source lock before changing work. Check both the revision
// and offset under that lock; stale plans cannot clear an append or a batch.
func lockArchiveWork(ctx context.Context, tx pgx.Tx, source string, w archiveWork) error {
	var tombstoned bool
	err := tx.QueryRow(ctx, `SELECT tombstoned_at IS NOT NULL FROM sources WHERE id=$1 FOR UPDATE`, source).Scan(&tombstoned)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && tombstoned {
		return ErrArchiveChanged
	}
	if err != nil {
		return err
	}
	var from, revision int64
	err = tx.QueryRow(ctx, `SELECT from_offset,revision FROM archive_redaction_work WHERE source_id=$1 AND generation=$2`, source, w.gen).Scan(&from, &revision)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (from != w.from || revision != w.revision) {
		return ErrArchiveChanged
	}
	return err
}

func archiveLineStart(r io.ReaderAt, pos int64) (int64, error) {
	buf := make([]byte, 64<<10)
	for pos > 0 {
		from := max(0, pos-int64(len(buf)))
		b := buf[:pos-from]
		n, err := r.ReadAt(b, from)
		if err != nil && err != io.EOF {
			return 0, err
		}
		if i := bytes.LastIndexByte(b[:n], '\n'); i >= 0 {
			return from + int64(i) + 1, nil
		}
		pos = from
	}
	return 0, nil
}

// scanArchiveMasks hashes complete lines across arbitrary chunk boundaries.
// It holds only a read buffer and at most 256 matched lines per batch. Long
// records are streamed, including trailing CRs split across buffer reads.
func scanArchiveMasks(ctx context.Context, r io.ReaderAt, off, size int64, masks *redact.LineCatalog, into map[string][]redact.Span) (next int64, finished bool, err error) {
	br := bufio.NewReaderSize(io.NewSectionReader(r, off, size), 64<<10)
	hash := sha256.New()
	start := off
	pendingCR := 0
	matches := 0
	var prefix []byte
	crs := bytes.Repeat([]byte{'\r'}, 1024)
	for {
		if err := ctx.Err(); err != nil {
			return off, false, err
		}
		part, err := br.ReadSlice('\n')
		end := off + int64(len(part))
		if len(prefix) < 64 {
			prefix = append(prefix, part[:min(len(part), 64-len(prefix))]...)
		}
		if err != nil && err != bufio.ErrBufferFull && err != io.EOF {
			return off, false, err
		}
		data := bytes.TrimSuffix(part, []byte{'\n'})
		trim := bytes.TrimRight(data, "\r")
		if len(trim) > 0 {
			for pendingCR > 0 {
				n := min(pendingCR, len(crs))
				hash.Write(crs[:n])
				pendingCR -= n
			}
			hash.Write(trim)
		}
		pendingCR += len(data) - len(trim)
		off = end
		if err == bufio.ErrBufferFull {
			continue
		}
		if end > start {
			var sum [32]byte
			copy(sum[:], hash.Sum(nil))
			lineSize := end - start - int64(pendingCR)
			if len(part) > 0 && part[len(part)-1] == '\n' {
				lineSize--
			}
			found, matchErr := masks.Match(r, start, lineSize, sum, prefix[:min(int64(len(prefix)), lineSize)])
			if matchErr != nil {
				return end, false, matchErr
			}
			for _, m := range found {
				for _, sp := range m.Spans {
					sp.End = int(min(int64(sp.End), lineSize))
					if sp.Start >= 0 && sp.Start < sp.End {
						into[m.RedactionID] = append(into[m.RedactionID], redact.Span{Start: int(start) + sp.Start, End: int(start) + sp.End})
					}
				}
			}
			if len(found) > 0 {
				matches++
			}
		}
		if err == io.EOF {
			return end, true, nil
		}
		if matches >= 256 {
			return end, false, nil
		}
		start = end
		pendingCR = 0
		hash.Reset()
		prefix = nil
	}
}
