package devicesync

import (
	"context"
	"errors"
	"os"

	"github.com/flopwire/flopwire/internal/syncproto"
)

type requiredTailFunc func(sid, gen int64) (syncproto.Tail, bool, error)

// planRequiredTails separates file inventory and SQL reference reads. The
// reference owner spans both phases and later deletion; the file mutex does not.
// The returned facts exist only for this operation, never as a shared cache.
func (s *Spool) planRequiredTails(ctx context.Context, sid int64, required requiredTailFunc) (requiredTailFunc, error) {
	keys, err := s.tailKeys(ctx, sid)
	if err != nil {
		return nil, err
	}
	type reference struct {
		tail   syncproto.Tail
		needed bool
	}
	facts := make(map[tailKey]reference, len(keys))
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ref, needed, err := required(key.sid, key.gen)
		if err != nil {
			return nil, err
		}
		facts[key] = reference{ref, needed}
	}
	return func(sid, gen int64) (syncproto.Tail, bool, error) {
		ref, found := facts[tailKey{sid, gen}]
		if !found {
			return syncproto.Tail{}, false, errors.New("devicesync: tail inventory changed during owned cleanup")
		}
		return ref.tail, ref.needed, nil
	}, nil
}

func (s *Spool) tailKeys(ctx context.Context, sid int64) ([]tailKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := openTailRoot(s.dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	files, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return nil, err
	}
	var keys []tailKey
	seen := map[tailKey]bool{}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name, valid := parseTailName(file.Name())
		if !valid || sid != 0 && name.sid != sid {
			continue
		}
		key := tailKey{name.sid, name.gen}
		if !seen[key] {
			keys = append(keys, key)
			seen[key] = true
		}
	}
	return keys, nil
}

// reconcileTailVersions removes only this source's verified tail files
// that no committed hash needs. Call it with the reference owner and before
// any current capture publication. Database uncertainty preserves charged bytes.
func (s *Spool) reconcileTailVersions(ctx context.Context, sid int64, required requiredTailFunc) error {
	required, err := s.planRequiredTails(ctx, sid, required)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	root, err := openTailRoot(s.dir)
	if err != nil {
		return err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	files, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return err
	}
	type obsolete struct {
		name string
		size int64
	}
	var remove []obsolete
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		name, valid := parseTailName(file.Name())
		if !valid || name.sid != sid {
			continue
		}
		ref, needed, err := required(name.sid, name.gen)
		if err != nil {
			return err
		}
		body, found, err := readTailCandidate(root, file.Name())
		hash := syncproto.Sum(body)
		if err != nil || !found || name.version && hash != name.hash {
			return errors.New("devicesync: cannot reconcile an invalid tail")
		}
		if needed && ref.Hash == hash {
			if int64(len(body)) != ref.Size {
				return errors.New("devicesync: required tail size mismatch")
			}
			continue
		}
		remove = append(remove, obsolete{file.Name(), int64(len(body))})
	}
	// Every decision succeeded before the first removal. No chunk or unrelated
	// source participates in this source-local retry reclamation.
	for _, file := range remove {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := root.Remove(file.name); err != nil {
			return err
		}
		s.used -= file.size
	}
	return nil
}

// sweepTails chooses exact committed versions. Legacy canonical bodies need
// their own hash/size check; a matching immutable sibling cannot validate them.
func (s *Spool) sweepTails(ctx context.Context, required requiredTailFunc) error {
	required, err := s.planRequiredTails(ctx, 0, required)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	root, err := openTailRoot(s.dir)
	if err != nil {
		return err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	files, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return err
	}
	type deletion struct {
		name string
		size int64
	}
	var remove []deletion
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		name, valid := parseTailName(file.Name())
		// Unrecognized names and partial publication files have no reference;
		// do not follow symlinks or remove directories while sweeping.
		if !valid {
			info, err := root.Lstat(file.Name())
			if err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				remove = append(remove, deletion{file.Name(), info.Size()})
			}
			continue
		}
		ref, needed, err := required(name.sid, name.gen)
		if err != nil {
			return err
		}
		body, found, err := readTailCandidate(root, file.Name())
		if err != nil || !found {
			return errors.New("devicesync: cannot inspect a tail during startup sweep")
		}
		hash := syncproto.Sum(body)
		if name.version && hash != name.hash {
			return errors.New("devicesync: immutable tail corrupted during startup sweep")
		}
		if needed && hash == ref.Hash {
			if int64(len(body)) != ref.Size {
				return errors.New("devicesync: committed tail size mismatch during startup sweep")
			}
			continue
		}
		remove = append(remove, deletion{file.Name(), int64(len(body))})
	}
	for _, file := range remove {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := root.Remove(file.name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		s.used -= file.size
	}
	return nil
}
