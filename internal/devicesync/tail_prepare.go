package devicesync

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

// LegacyTailPreparation counts format work, not uploaded or recovered history.
type LegacyTailPreparation struct {
	Needed          int `json:"needed"`
	Converted       int `json:"converted"`
	Canonical       int `json:"canonical"`
	Unspooled       int `json:"unspooled"`
	RemovedVersions int `json:"removed_versions"`
}

type tailKey struct{ sid, gen int64 }

type committedTail struct {
	hash           syncproto.Hash
	size           int64
	allowUnspooled bool
}

// PrepareLegacyTails converts verified committed immutable tails to the legacy
// canonical layout. The caller must hold exclusive index ownership and open an
// existing database; this function performs no SQLite writes, schema setup,
// source reads, chunk cleanup or startup sweep. It prepares only tail format,
// not compatibility of an older binary's schema or policy enforcement.
func PrepareLegacyTails(ctx context.Context, db *sql.DB, spoolDir string) (LegacyTailPreparation, error) {
	return prepareLegacyTails(ctx, db, spoolDir, nil)
}

// afterRename is an explicit, unexported test operation for proving interruption
// after an actual atomic rename. Normal preparation has no fault endpoint/hook.
func prepareLegacyTails(ctx context.Context, db *sql.DB, spoolDir string, afterRename func() error) (LegacyTailPreparation, error) {
	var result LegacyTailPreparation
	refs, err := neededTailReferences(ctx, db)
	if err != nil {
		return result, err
	}
	root, err := openTailRoot(spoolDir)
	if err != nil {
		return result, err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return result, errors.New("devicesync: cannot enumerate tail directory")
	}
	files, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return result, errors.New("devicesync: cannot enumerate tail directory")
	}
	// Verify all recognized immutable versions before any conversion or cleanup.
	// Unrecognized files (including temporaries) and chunks remain untouched.
	versions := make(map[string]tailName)
	versionKeys := make(map[tailKey]bool)
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		name, valid := parseTailName(file.Name())
		if !valid || !name.version {
			continue
		}
		body, found, err := readTailCandidate(root, file.Name())
		if err != nil || !found || syncproto.Sum(body) != name.hash {
			return result, errors.New("devicesync: immutable tail validation failed; preparation stopped")
		}
		versions[file.Name()] = name
		versionKeys[tailKey{name.sid, name.gen}] = true
	}
	type conversion struct {
		version, canonical string
		ref                committedTail
	}
	var conversions []conversion
	keys := make([]tailKey, 0, len(refs))
	for key := range refs {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].sid < keys[j].sid || keys[i].sid == keys[j].sid && keys[i].gen < keys[j].gen
	})
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		ref := refs[key]
		result.Needed++
		version, err := tailVersionName(key.sid, key.gen, ref.hash)
		if err != nil {
			return result, err
		}
		canonical := fmt.Sprintf("%d-%d", key.sid, key.gen)
		v, hasVersion, err := readTailCandidate(root, version)
		if err != nil {
			return result, err
		}
		if hasVersion && !matchesTail(v, ref) {
			return result, errors.New("devicesync: committed immutable tail mismatch; preparation stopped")
		}
		legacy, hasLegacy, err := readTailCandidate(root, canonical)
		if err != nil {
			return result, err
		}
		if hasLegacy && matchesTail(legacy, ref) {
			result.Canonical++
			continue
		}
		if hasVersion {
			// A verified committed version may replace a regular stale legacy
			// tail. Its publication and bytes have already been validated.
			conversions = append(conversions, conversion{version, canonical, ref})
			continue
		}
		if !hasLegacy && !versionKeys[key] && ref.allowUnspooled {
			result.Unspooled++
			continue
		}
		return result, errors.New("devicesync: needed tail missing or mismatching; preparation stopped")
	}
	// No needed candidate failed validation. Each rename leaves a canonical
	// committed-hash-valid file, so cancellation or process death is resumable.
	for _, change := range conversions {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		body, found, err := readTailCandidate(root, change.version)
		if err != nil || !found || !matchesTail(body, change.ref) {
			return result, errors.New("devicesync: committed tail changed before conversion")
		}
		// Do not replace a nonregular canonical introduced after validation.
		if _, _, err := readTailCandidate(root, change.canonical); err != nil {
			return result, err
		}
		if err := root.Rename(change.version, change.canonical); err != nil {
			return result, errors.New("devicesync: tail conversion failed")
		}
		delete(versions, change.version)
		result.Converted++
		if afterRename != nil {
			if err := afterRename(); err != nil {
				return result, err
			}
		}
	}
	// Obsolete immutable versions have no legacy purpose. Revalidate before
	// deletion and preserve malformed or unrelated files rather than guessing.
	names := make([]string, 0, len(versions))
	for name := range versions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		body, found, err := readTailCandidate(root, name)
		if err != nil || !found || syncproto.Sum(body) != versions[name].hash {
			return result, errors.New("devicesync: obsolete tail changed before cleanup")
		}
		if err := root.Remove(name); err != nil {
			return result, errors.New("devicesync: obsolete tail cleanup failed")
		}
		result.RemovedVersions++
	}
	return result, nil
}

func matchesTail(body []byte, ref committedTail) bool {
	return int64(len(body)) == ref.size && syncproto.Sum(body) == ref.hash
}

func neededTailReferences(ctx context.Context, db *sql.DB) (map[tailKey]committedTail, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT g.source_id,g.generation,g.size,g.tail_offset,g.tail_size,g.tail_hash,g.tail_acked,g.closed,g.lost,
	 s.generation,s.spec,s.watermark FROM devsync_gens g LEFT JOIN devsync_sources s ON s.id=g.source_id
	 WHERE g.tail_size>0 ORDER BY g.source_id,g.generation`)
	if err != nil {
		return nil, errors.Join(errors.New("devicesync: cannot read existing committed tail state"), ctx.Err())
	}
	defer rows.Close()
	refs := make(map[tailKey]committedTail)
	for rows.Next() {
		var key tailKey
		var captured, offset, size int64
		var hash []byte
		var acked, closed, lost bool
		var current sql.NullInt64
		var specJSON, wmJSON sql.NullString
		if err := rows.Scan(&key.sid, &key.gen, &captured, &offset, &size, &hash, &acked, &closed, &lost, &current, &specJSON, &wmJSON); err != nil {
			return nil, errors.New("devicesync: cannot decode committed tail state")
		}
		if !current.Valid || !specJSON.Valid {
			return nil, errors.New("devicesync: committed tail lacks source state")
		}
		var spec SourceSpec
		var wm storedWatermark
		if json.Unmarshal([]byte(specJSON.String), &spec) != nil || wmJSON.Valid && json.Unmarshal([]byte(wmJSON.String), &wm) != nil {
			return nil, errors.New("devicesync: invalid committed source metadata")
		}
		if lost || acked && !(current.Int64 == key.gen && wm.Export != nil) {
			continue
		}
		if key.sid <= 0 || key.gen < 0 || offset < 0 || size > syncproto.MaxPartBytes || captured < size || offset != captured-size || len(hash) != len(syncproto.Hash{}) {
			return nil, errors.New("devicesync: invalid needed tail metadata")
		}
		var ref committedTail
		copy(ref.hash[:], hash)
		ref.size = size
		ref.allowUnspooled = !spec.Export && wm.Export == nil && wmJSON.Valid && wm.Offset == captured && !closed && current.Int64 == key.gen &&
			(spec.StorageKind == transcript.StorageJSONLAppend || spec.StorageKind == transcript.StorageCompanion)
		refs[key] = ref
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Join(errors.New("devicesync: committed tail query interrupted"), ctx.Err())
	}
	return refs, ctx.Err()
}
