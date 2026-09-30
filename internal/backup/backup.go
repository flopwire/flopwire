// Package backup creates, verifies, and restores a coordinated backup of the
// durable state: one Postgres snapshot plus every chunk object that snapshot
// references (spec §7.2, §10).
//
// Ported from the CASS-era recovery hardening (#3: 8a3fc34, 1f6d8fc, 4120fba,
// 9c39cfd): an explicit encrypted-destination acknowledgement, a shared purge
// lock so deletion cannot remove objects mid-backup, one exported snapshot for
// both the inventory and pg_dump, synced files, a state file, and
// manifest.json as the durable commit point. Restore refuses non-empty
// targets and cross-checks the restored inventory. The inventory now comes
// from chunks, and every object is verified against its BLAKE3 content
// address on backup and on restore.
package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"
	"github.com/zeebo/blake3"
)

// purgeLockID matches store.purgeLockID: backup holds it shared, the deletion
// worker exclusively.
const purgeLockID int64 = 0x5445414d454d

// manifestVersion 4 is the chunk-based format with compressed objects.
// Earlier backups are not restorable into the fresh schema.
const manifestVersion = 4

// Entry is one chunk object: a zstd frame of the chunk (syncproto codec).
// Size and SHA256 cover the object as stored (the copied file, for
// backup-verify); BLAKE3 is the chunk's content address (hex) as recorded
// in Postgres, of the RawSize bytes the object decodes to.
type Entry struct {
	Key     string `json:"key"`
	Size    int64  `json:"size"`
	RawSize int64  `json:"raw_size"`
	BLAKE3  string `json:"blake3"`
	SHA256  string `json:"sha256"`
}

// inventorySQL lists the chunk objects a snapshot owns: committed chunks
// that some live source still references (or that nothing references yet).
// Left out:
//   - orphan-owned states (uploading, cleanup_pending, purging): uncommitted
//     upload debris; the reconciler removes their rows after a restore;
//   - deletion-owned states (deletion_pending, purging_delete): their
//     objects may already be gone, and a restored job's delete of a missing
//     key is a no-op;
//   - chunks referenced only by tombstoned sources: evidence of deleted
//     conversations, which a backup must not keep (D9); the restored
//     deletion job drops those manifests.
const inventorySQL = `SELECT object_key,stored_size::bigint,size::bigint,encode(hash,'hex') FROM chunks c
	WHERE state='committed' AND (
		NOT EXISTS (SELECT 1 FROM manifest_entries m WHERE m.chunk_hash=c.hash)
		OR EXISTS (SELECT 1 FROM manifest_entries m JOIN sources s ON s.id=m.source_id WHERE m.chunk_hash=c.hash AND s.tombstoned_at IS NULL))
	ORDER BY object_key`

func readInventory(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) ([]Entry, error) {
	rows, err := q.Query(ctx, inventorySQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		if err = rows.Scan(&e.Key, &e.Size, &e.RawSize, &e.BLAKE3); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return canonicalInventory(out)
}

type Manifest struct {
	Version        int       `json:"version"`
	CreatedAt      time.Time `json:"created_at"`
	DatabaseFile   string    `json:"database_file"`
	DatabaseSize   int64     `json:"database_size"`
	DatabaseSHA256 string    `json:"database_sha256"`
	Objects        []Entry   `json:"objects"`
}
type State struct {
	Operation string    `json:"operation"`
	Status    string    `json:"status"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	Error     string    `json:"error,omitempty"`
}

func Create(ctx context.Context, databaseURL string, objects *minio.Client, bucket, output string, encryptedDestination bool) (manifest Manifest, err error) {
	if !encryptedDestination {
		return manifest, errors.New("production backup requires --encrypted-destination acknowledgement")
	}
	info, statErr := os.Stat(output)
	if statErr == nil && (!info.IsDir() || directoryNotEmpty(output)) {
		return manifest, fmt.Errorf("backup output must be a new or empty directory: %s", output)
	}
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return manifest, statErr
	}
	if err = os.MkdirAll(filepath.Join(output, "objects"), 0700); err != nil {
		return manifest, err
	}
	if err = syncDir(output); err != nil {
		return manifest, err
	}
	state := State{Operation: "backup", Status: "incomplete", StartedAt: time.Now().UTC()}
	if err = writeState(output, state); err != nil {
		return manifest, err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(filepath.Join(output, "manifest.json"))
			_ = syncDir(output)
			state.EndedAt = time.Now().UTC()
			state.Status, state.Error = "failed", errorClass(err)
			if stateErr := writeState(output, state); stateErr != nil {
				err = errors.Join(err, fmt.Errorf("write failed backup state: %w", stateErr))
			}
		}
	}()

	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return manifest, fmt.Errorf("connect postgres: %w", err)
	}
	defer conn.Close(context.Background())
	if _, err = conn.Exec(ctx, `SELECT pg_advisory_lock_shared($1)`, purgeLockID); err != nil {
		return manifest, err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock_shared($1)`, purgeLockID)
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return manifest, err
	}
	defer tx.Rollback(context.Background())
	var snapshot string
	if err = tx.QueryRow(ctx, `SELECT pg_export_snapshot()`).Scan(&snapshot); err != nil {
		return manifest, err
	}
	if manifest.Objects, err = readInventory(ctx, tx); err != nil {
		return manifest, err
	}

	dbPath := filepath.Join(output, "postgres.dump")
	cmd := exec.CommandContext(ctx, "pg_dump", "--dbname="+databaseURL, "--format=custom", "--snapshot="+snapshot, "--file", dbPath)
	if out, dumpErr := cmd.CombinedOutput(); dumpErr != nil {
		return manifest, fmt.Errorf("pg_dump: %w: %s", dumpErr, string(out))
	}
	if err = tx.Commit(ctx); err != nil {
		return manifest, err
	}
	if err = syncFileAndDir(dbPath); err != nil {
		return manifest, fmt.Errorf("sync database dump: %w", err)
	}
	dbSum, dbSize, err := fileHash(dbPath)
	if err != nil {
		return manifest, err
	}
	manifest.Version, manifest.CreatedAt = manifestVersion, time.Now().UTC()
	manifest.DatabaseFile, manifest.DatabaseSize, manifest.DatabaseSHA256 = "postgres.dump", dbSize, dbSum
	for i, entry := range manifest.Objects {
		if !safeKey(entry.Key) {
			return manifest, fmt.Errorf("unsafe object key in database: %q", entry.Key)
		}
		target := filepath.Join(output, "objects", filepath.FromSlash(entry.Key))
		if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return manifest, err
		}
		if err = objects.FGetObject(ctx, bucket, entry.Key, target, minio.GetObjectOptions{}); err != nil {
			return manifest, fmt.Errorf("copy object %s: %w", entry.Key, err)
		}
		sums, hashErr := fileSums(target, entry.RawSize)
		if hashErr != nil {
			return manifest, hashErr
		}
		if sums.blake3 != entry.BLAKE3 || sums.size != entry.Size {
			return manifest, fmt.Errorf("snapshot object mismatch: %s", entry.Key)
		}
		manifest.Objects[i].SHA256 = sums.sha256
		if err = syncFileAndDir(target); err != nil {
			return manifest, fmt.Errorf("sync object %s: %w", entry.Key, err)
		}
	}
	raw, marshalErr := json.MarshalIndent(manifest, "", "  ")
	if marshalErr != nil {
		return manifest, marshalErr
	}
	state.EndedAt, state.Status, state.Error = time.Now().UTC(), "complete", ""
	if err = writeState(output, state); err != nil {
		return manifest, fmt.Errorf("write complete backup state: %w", err)
	}
	if err = publishManifest(output, append(raw, '\n'), nil); err != nil {
		return manifest, err
	}
	return manifest, nil
}

func Verify(output string) (Manifest, error) {
	var manifest Manifest
	var state State
	stateRaw, err := os.ReadFile(filepath.Join(output, "state.json"))
	if err != nil {
		return manifest, fmt.Errorf("read backup state: %w", err)
	}
	if err = json.Unmarshal(stateRaw, &state); err != nil {
		return manifest, fmt.Errorf("decode backup state: %w", err)
	}
	if state.Operation != "backup" || state.Status != "complete" || state.StartedAt.IsZero() || state.EndedAt.IsZero() || state.Error != "" {
		return manifest, fmt.Errorf("backup state is not complete")
	}
	raw, err := os.ReadFile(filepath.Join(output, "manifest.json"))
	if err != nil {
		return manifest, err
	}
	if err = json.Unmarshal(raw, &manifest); err != nil {
		return manifest, err
	}
	if manifest.Version != manifestVersion {
		return manifest, fmt.Errorf("unsupported backup version %d", manifest.Version)
	}
	if manifest.DatabaseFile != "postgres.dump" {
		return manifest, fmt.Errorf("invalid database dump path: %q", manifest.DatabaseFile)
	}
	dbSum, dbSize, err := fileHash(filepath.Join(output, manifest.DatabaseFile))
	if err != nil {
		return manifest, err
	}
	if dbSum != manifest.DatabaseSHA256 || dbSize != manifest.DatabaseSize {
		return manifest, errors.New("database dump verification failed")
	}
	seen := map[string]bool{}
	for _, entry := range manifest.Objects {
		if !safeKey(entry.Key) || seen[entry.Key] {
			return manifest, fmt.Errorf("invalid object manifest key: %q", entry.Key)
		}
		seen[entry.Key] = true
		sums, hashErr := fileSums(filepath.Join(output, "objects", filepath.FromSlash(entry.Key)), entry.RawSize)
		if hashErr != nil {
			return manifest, hashErr
		}
		if sums.sha256 != entry.SHA256 || sums.blake3 != entry.BLAKE3 || sums.size != entry.Size {
			return manifest, fmt.Errorf("object verification failed: %s", entry.Key)
		}
	}
	return manifest, nil
}

func Restore(ctx context.Context, databaseURL string, objects *minio.Client, bucket, input, stateOutput string) (err error) {
	state := State{Operation: "restore", Status: "incomplete", StartedAt: time.Now().UTC()}
	if err = writeStateFile(stateOutput, state); err != nil {
		return fmt.Errorf("write restore state: %w", err)
	}
	defer func() {
		state.EndedAt = time.Now().UTC()
		if err != nil {
			state.Status, state.Error = "failed", restoreErrorClass(err)
		} else {
			state.Status = "complete"
		}
		if stateErr := writeStateFile(stateOutput, state); err == nil && stateErr != nil {
			err = stateErr
		}
	}()
	manifest, err := Verify(input)
	if err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connect restore target: %w", err)
	}
	defer conn.Close(context.Background())
	empty, emptyErr := restoreTargetDatabaseEmpty(ctx, conn)
	if emptyErr != nil {
		return emptyErr
	}
	if !empty {
		return errors.New("restore target database is not empty")
	}
	for item := range objects.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true, MaxKeys: 1}) {
		if item.Err != nil {
			return item.Err
		}
		return errors.New("restore target bucket is not empty")
	}
	for _, entry := range manifest.Objects {
		source := filepath.Join(input, "objects", filepath.FromSlash(entry.Key))
		if _, err = objects.FPutObject(ctx, bucket, entry.Key, source, minio.PutObjectOptions{}); err != nil {
			return fmt.Errorf("restore object %s: %w", entry.Key, err)
		}
		obj, getErr := objects.GetObject(ctx, bucket, entry.Key, minio.GetObjectOptions{})
		if getErr != nil {
			return getErr
		}
		sums, hashErr := readerSums(obj, entry.RawSize)
		closeErr := obj.Close()
		if hashErr != nil {
			return hashErr
		}
		if closeErr != nil {
			return closeErr
		}
		if sums.sha256 != entry.SHA256 || sums.blake3 != entry.BLAKE3 || sums.size != entry.Size {
			return fmt.Errorf("restored object verification failed: %s", entry.Key)
		}
	}
	cmd := exec.CommandContext(ctx, "pg_restore", "--dbname="+databaseURL, "--no-owner", filepath.Join(input, manifest.DatabaseFile))
	if out, restoreErr := cmd.CombinedOutput(); restoreErr != nil {
		return fmt.Errorf("pg_restore: %w: %s", restoreErr, string(out))
	}
	restored, err := readInventory(ctx, conn)
	if err != nil {
		return err
	}
	if len(restored) != len(manifest.Objects) {
		return fmt.Errorf("restored database inventory mismatch: database=%d manifest=%d", len(restored), len(manifest.Objects))
	}
	for i := range restored {
		if restored[i] != (Entry{Key: manifest.Objects[i].Key, Size: manifest.Objects[i].Size, RawSize: manifest.Objects[i].RawSize, BLAKE3: manifest.Objects[i].BLAKE3}) {
			return fmt.Errorf("restored database inventory mismatch at object %d", i)
		}
	}
	return nil
}

// safeKey accepts an object key only if it stays inside the objects directory.
func safeKey(key string) bool {
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(key)))
	return key != "" && !filepath.IsAbs(filepath.FromSlash(key)) && clean != ".." && !strings.HasPrefix(clean, "../")
}

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func restoreTargetDatabaseEmpty(ctx context.Context, q rowQuerier) (bool, error) {
	var empty bool
	// Restore is allowed only into a database containing the built-ins created
	// by initdb/CREATE DATABASE. Reject both schema-local objects and every
	// database-wide user object class emitted by pg_dump; otherwise pg_restore
	// could merge with or overwrite operator-owned state.
	err := q.QueryRow(ctx, `SELECT NOT EXISTS (
		SELECT 1 FROM pg_namespace WHERE nspname NOT LIKE 'pg_%' AND nspname NOT IN ('information_schema','public')
		UNION ALL SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public'
		UNION ALL SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public'
		UNION ALL SELECT 1 FROM pg_type t JOIN pg_namespace n ON n.oid=t.typnamespace WHERE n.nspname='public'
		UNION ALL SELECT 1 FROM pg_collation x JOIN pg_namespace n ON n.oid=x.collnamespace WHERE n.nspname='public'
		UNION ALL SELECT 1 FROM pg_conversion x JOIN pg_namespace n ON n.oid=x.connamespace WHERE n.nspname='public'
		UNION ALL SELECT 1 FROM pg_operator x JOIN pg_namespace n ON n.oid=x.oprnamespace WHERE n.nspname='public'
		UNION ALL SELECT 1 FROM pg_opclass x JOIN pg_namespace n ON n.oid=x.opcnamespace WHERE n.nspname='public'
		UNION ALL SELECT 1 FROM pg_opfamily x JOIN pg_namespace n ON n.oid=x.opfnamespace WHERE n.nspname='public'
		UNION ALL SELECT 1 FROM pg_ts_config x JOIN pg_namespace n ON n.oid=x.cfgnamespace WHERE n.nspname='public'
		UNION ALL SELECT 1 FROM pg_ts_dict x JOIN pg_namespace n ON n.oid=x.dictnamespace WHERE n.nspname='public'
		UNION ALL SELECT 1 FROM pg_ts_parser x JOIN pg_namespace n ON n.oid=x.prsnamespace WHERE n.nspname='public'
		UNION ALL SELECT 1 FROM pg_ts_template x JOIN pg_namespace n ON n.oid=x.tmplnamespace WHERE n.nspname='public'
		UNION ALL SELECT 1 FROM pg_statistic_ext x JOIN pg_namespace n ON n.oid=x.stxnamespace WHERE n.nspname='public'
		UNION ALL SELECT 1 FROM pg_extension WHERE extname <> 'plpgsql'
		UNION ALL SELECT 1 FROM pg_language WHERE lanname NOT IN ('internal','c','sql','plpgsql')
		UNION ALL SELECT 1 FROM pg_cast WHERE oid >= 16384
		UNION ALL SELECT 1 FROM pg_transform WHERE oid >= 16384
		UNION ALL SELECT 1 FROM pg_event_trigger
		UNION ALL SELECT 1 FROM pg_foreign_data_wrapper WHERE oid >= 16384
		UNION ALL SELECT 1 FROM pg_foreign_server
		UNION ALL SELECT 1 FROM pg_user_mapping
		UNION ALL SELECT 1 FROM pg_publication
		UNION ALL SELECT 1 FROM pg_subscription
		UNION ALL SELECT 1 FROM pg_largeobject_metadata
		UNION ALL SELECT 1 FROM pg_default_acl
		UNION ALL SELECT 1 FROM pg_am WHERE oid >= 16384
	)`).Scan(&empty)
	return empty, err
}

type sums struct {
	sha256, blake3 string
	size           int64
}

// readerSums hashes an object as stored (SHA256, size) and, when rawSize
// is positive, the BLAKE3 of the chunk it decodes to ("" when it does not
// decode to rawSize bytes). A chunk object is at most one part.
func readerSums(r io.Reader, rawSize int64) (sums, error) {
	if rawSize <= 0 {
		s := sha256.New()
		size, err := io.Copy(s, r)
		return sums{sha256: hex.EncodeToString(s.Sum(nil)), size: size}, err
	}
	z, err := io.ReadAll(io.LimitReader(r, syncproto.MaxPartBytes+1<<20))
	if err != nil {
		return sums{}, err
	}
	s := sha256.Sum256(z)
	out := sums{sha256: hex.EncodeToString(s[:]), size: int64(len(z))}
	if data, err := syncproto.DecompressAny(z, rawSize); err == nil {
		b := blake3.Sum256(data)
		out.blake3 = hex.EncodeToString(b[:])
	}
	return out, nil
}
func fileSums(path string, rawSize int64) (sums, error) {
	f, err := os.Open(path)
	if err != nil {
		return sums{}, err
	}
	defer f.Close()
	return readerSums(f, rawSize)
}
func fileHash(path string) (string, int64, error) {
	s, err := fileSums(path, 0)
	return s.sha256, s.size, err
}
func directoryNotEmpty(path string) bool {
	entries, err := os.ReadDir(path)
	return err != nil || len(entries) > 0
}
func ValidateOutput(path string) error {
	clean := filepath.Clean(path)
	if clean == "." || clean == string(filepath.Separator) || strings.TrimSpace(path) == "" {
		return fmt.Errorf("refusing broad backup path %q", path)
	}
	return nil
}
func writeState(output string, state State) error {
	return writeStateFile(filepath.Join(output, "state.json"), state)
}
func writeStateFile(path string, state State) error {
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err = writeSyncedFile(tmp, append(raw, '\n')); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

type publishFault func(boundary string) error

func publishManifest(output string, raw []byte, fault publishFault) (err error) {
	tmp := filepath.Join(output, ".manifest.json.tmp")
	final := filepath.Join(output, "manifest.json")
	published := false
	defer func() {
		if err == nil {
			return
		}
		_ = os.Remove(tmp)
		if published {
			_ = os.Remove(final)
			_ = syncDir(output)
		}
	}()
	if err = writeSyncedFile(tmp, raw); err != nil {
		return err
	}
	if fault != nil {
		if err = fault("after_manifest_sync"); err != nil {
			return err
		}
	}
	if err = os.Rename(tmp, final); err != nil {
		return err
	}
	published = true
	if fault != nil {
		if err = fault("after_manifest_rename"); err != nil {
			return err
		}
	}
	if err = syncDir(output); err != nil {
		return err
	}
	return nil
}

func writeSyncedFile(path string, raw []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func syncFileAndDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	err = dir.Sync()
	closeErr := dir.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func restoreErrorClass(err error) string {
	text := err.Error()
	switch {
	case strings.Contains(text, "not empty"):
		return "target_not_empty"
	case strings.Contains(text, "verification"):
		return "verification_failed"
	case strings.Contains(text, "pg_restore"):
		return "database_restore_failed"
	case strings.Contains(text, "object"):
		return "object_restore_failed"
	default:
		return "restore_failed"
	}
}

func canonicalInventory(entries []Entry) ([]Entry, error) {
	byKey := make(map[string]Entry, len(entries))
	for _, entry := range entries {
		if previous, ok := byKey[entry.Key]; ok {
			if previous.Size != entry.Size || previous.BLAKE3 != entry.BLAKE3 {
				return nil, fmt.Errorf("inconsistent metadata for object %s", entry.Key)
			}
			continue
		}
		byKey[entry.Key] = entry
	}
	out := make([]Entry, 0, len(byKey))
	for _, entry := range byKey {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}
func errorClass(err error) string {
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	text := err.Error()
	if strings.Contains(text, "pg_dump") {
		return "database_dump_failed"
	}
	if strings.Contains(text, "object") {
		return "object_copy_failed"
	}
	return "backup_failed"
}
