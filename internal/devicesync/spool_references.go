package devicesync

import (
	"context"
	"errors"
	"os"
	"slices"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

// spoolReferenceScope is lexical ownership of one actual Store/Spool pair.
// Do not retain it after withReferences returns or call withReferences from
// inside its callback. Nested publication/cleanup uses the supplied scope.
// This API is dormant; existing runtime operations do not yet join it.
type spoolReferenceScope struct {
	store *Store
	spool *Spool
}

// withReferences binds the first Store wrapper and serializes reference
// transitions. Lock order is referenceMu -> SQLite or Spool.mu. No SQLite
// transaction spans spool IO, and Spool.mu never spans a reference query.
// Neither reference ownership nor Spool.mu may span network transport.
func (s *Spool) withReferences(store *Store, fn func(*spoolReferenceScope) error) error {
	if store == nil || fn == nil {
		return errors.New("devicesync: reference ownership needs a store and callback")
	}
	s.referenceMu.Lock()
	defer s.referenceMu.Unlock()
	if s.referenceStore != nil && s.referenceStore != store {
		return errors.New("devicesync: spool reference owner is bound to another store")
	}
	s.referenceStore = store
	return fn(&spoolReferenceScope{store: store, spool: s})
}

func (r *spoolReferenceScope) putChunk(hash syncproto.Hash, body []byte) error {
	return r.spool.putOwnedChunk(hash, body)
}

func (s *Spool) putOwnedChunk(hash syncproto.Hash, body []byte) error {
	if len(body) == 0 || len(body) > syncproto.MaxPartBytes || syncproto.Sum(body) != hash {
		return errors.New("devicesync: invalid owned chunk publication")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	root, err := openChunkRoot(s.dir)
	if err != nil {
		return err
	}
	defer root.Close()
	old, found, err := readTailCandidate(root, hash.String())
	if err != nil {
		return err
	}
	if found {
		if syncproto.Sum(old) != hash {
			return errors.New("devicesync: owned existing chunk does not match its hash")
		}
		return nil
	}
	if err := s.checkSpaceLocked(int64(len(body))); err != nil {
		return err
	}
	retained, err := publishTailVersion(root, hash.String(), body)
	if err != nil {
		s.used += retained
		return err
	}
	s.used += int64(len(body))
	s.blocked = false
	return nil
}

func (r *spoolReferenceScope) putTailVersion(sid, gen int64, hash syncproto.Hash, body []byte) error {
	return r.spool.PutTailVersion(sid, gen, hash, body)
}

func (r *spoolReferenceScope) saveCapture(ctx context.Context, src *sourceRow, g *genRow, add []syncproto.Entry, wm *transcript.Watermark, state []byte, guards ...func() error) error {
	return r.store.saveCapture(ctx, src, g, add, wm, state, guards...)
}

// releaseChunks checks every requested hash before opening the file owner.
// A query failure/cancellation cannot authorize removal from a partial result.
func (r *spoolReferenceScope) releaseChunks(ctx context.Context, hashes []syncproto.Hash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	refs, err := r.requiredChunks(ctx, hashes)
	if err != nil {
		return err
	}
	var unused []syncproto.Hash
	seen := make(map[syncproto.Hash]bool, len(hashes))
	for _, hash := range hashes {
		if !refs[hash] && !seen[hash] {
			unused = append(unused, hash)
			seen[hash] = true
		}
	}
	return r.spool.dropVerifiedChunks(ctx, unused)
}

func (r *spoolReferenceScope) releaseTail(ctx context.Context, sid, gen int64, old syncproto.Tail) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if old.Size == 0 {
		return nil
	}
	ref, needed, err := r.store.requiredTail(ctx, sid, gen)
	if err != nil || needed && ref.Hash == old.Hash {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.spool.dropTailVersion(ctx, sid, gen, old.Hash)
}

// requiredChunks is deliberately stricter than the legacy referenced lookup.
// Only hashes supplied by the caller are queried, in bounded hash batches.
// Cut generations retain audit rows beyond entries; those are not pending.
func (r *spoolReferenceScope) requiredChunks(ctx context.Context, hashes []syncproto.Hash) (map[syncproto.Hash]bool, error) {
	refs := make(map[syncproto.Hash]bool)
	for part := range slices.Chunk(hashes, batchRows) {
		rows, err := r.store.db.QueryContext(ctx, `SELECT m.hash,m.ordinal,m.offset,m.size,g.source_id,g.entries,g.acked,g.lost FROM devsync_manifest m LEFT JOIN devsync_gens g ON g.source_id=m.source_id AND g.generation=m.generation WHERE m.hash IN (`+placeholders(len(part))+`)`, hashArgs(part)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var raw []byte
			var ordinal, offset, size, sid, entries, acked int64
			var lost bool
			if err := rows.Scan(&raw, &ordinal, &offset, &size, &sid, &entries, &acked, &lost); err != nil {
				rows.Close()
				return nil, err // NULL joined metadata includes orphan manifests
			}
			if len(raw) != len(syncproto.Hash{}) || sid <= 0 || entries < 0 || acked < 0 || acked > entries || ordinal < 0 || offset < 0 || size <= 0 || size > syncproto.MaxPartBytes {
				rows.Close()
				return nil, errors.New("devicesync: uncertain chunk reference metadata")
			}
			if !lost && ordinal >= acked && ordinal < entries {
				var hash syncproto.Hash
				copy(hash[:], raw)
				refs[hash] = true
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return refs, nil
}

// dropVerifiedChunks owns inspection, unlink and accounting. Database queries
// have already completed; all candidate bodies validate before any removal.
func (s *Spool) dropVerifiedChunks(ctx context.Context, hashes []syncproto.Hash) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(hashes) == 0 {
		return nil
	}
	chunks, err := openChunkRoot(s.dir)
	if err != nil {
		return err
	}
	defer chunks.Close()
	type deletion struct {
		name string
		size int64
	}
	var remove []deletion
	for _, hash := range hashes {
		if err := ctx.Err(); err != nil {
			return err
		}
		body, found, err := readTailCandidate(chunks, hash.String())
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		if syncproto.Sum(body) != hash {
			return errors.New("devicesync: owned chunk candidate does not match its hash")
		}
		remove = append(remove, deletion{hash.String(), int64(len(body))})
	}
	for _, file := range remove {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := chunks.Remove(file.name); err != nil {
			return err
		}
		s.used -= file.size
	}
	return nil
}

func openChunkRoot(dir string) (*os.Root, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, err := root.Lstat("chunks")
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("devicesync: owned chunk directory unavailable or nonregular")
	}
	return root.OpenRoot("chunks")
}
