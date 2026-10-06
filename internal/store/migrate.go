package store

// The transactional migration ledger is ported from the CASS-era launch
// stack (#8: 7bcbe33, 91e1724, b9e6387). Kept: one transaction for the whole
// pending set, an advisory transaction lock that serializes every caller,
// per-file checksums, a contiguous-prefix check, and binding to one explicit
// target schema. Dropped: recognition of pre-ledger CASS-era schemas and the
// search-server lease. A database created by the old migrations is refused.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/flopwire/flopwire/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	migrationLockClass = int32(0x54454d4d) // "TEMM"
	migrationLockID    = int32(0x4d494752) // "MIGR"

	// 661a48b shipped this authentic 009 before refusal coalescing and bus
	// retention were edited into 009 on main. Only this exact historical
	// checksum is recognized, and only with the exact canonical 009 and
	// additive 013 below. Never rewrite an applied historical ledger row.
	busMigrationName     = "009_bus.sql"
	busLegacyChecksum    = "fd7ce62752862b6b566472e248247a849f07a6a39ae7f892237aa4af8ae927d7"
	busCanonicalChecksum = "16150bac5be12b63ec029bcec5fc927956b7716c08a9fce292899b9c34db57f2"
	busUpgradeName       = "013_bus_retention_upgrade.sql"
	busUpgradeChecksum   = "bb979ebc3509eb893c50c02f97c01e6e6a4c0e4a0939b144b9bb4362cbc1668f"
)

// ErrLegacySchema reports a database that holds application tables but no
// migration ledger: a CASS-era deployment. It must be backed up and replaced
// by an empty database.
var ErrLegacySchema = errors.New("database has application tables but no migration ledger (a pre-reset schema); restore into or start from an empty database")

type migrationFile struct{ name, sql, checksum string }

// Migrate applies every pending migration in one transaction. Concurrent
// callers serialize on an advisory lock; the loser sees the winner's ledger
// and applies nothing. A failure rolls back the schema changes and the ledger
// rows together.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	files, err := migrationFiles(migrations.Files)
	if err != nil {
		return err
	}
	return migrate(ctx, pool, files)
}

func migrate(ctx context.Context, pool *pgxpool.Pool, files []migrationFile) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var target string
	if err = tx.QueryRow(ctx, `SELECT current_schema()`).Scan(&target); err != nil || target == "" {
		return fmt.Errorf("resolve migration target schema: search_path names no existing schema (%v)", err)
	}
	// Freeze one explicit target so objects are created in, and evidence is
	// read from, the first search_path schema only.
	if _, err = tx.Exec(ctx, `SET LOCAL search_path TO `+pgx.Identifier{target}.Sanitize()+`, pg_catalog`); err != nil {
		return fmt.Errorf("set migration target schema: %w", err)
	}
	// Schema changes may outlast the pool's statement backstop.
	if _, err = tx.Exec(ctx, `SET LOCAL statement_timeout = 0`); err != nil {
		return fmt.Errorf("lift statement timeout: %w", err)
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1,$2)`, migrationLockClass, migrationLockID); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	var ledgerExists, legacy bool
	if err = tx.QueryRow(ctx, `SELECT
		to_regclass(format('%I.flopwire_schema_migrations', current_schema())) IS NOT NULL,
		to_regclass(format('%I.users', current_schema())) IS NOT NULL`).Scan(&ledgerExists, &legacy); err != nil {
		return fmt.Errorf("inspect schema: %w", err)
	}
	if !ledgerExists && legacy {
		return ErrLegacySchema
	}
	if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS flopwire_schema_migrations(
		name text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}
	applied, err := readAppliedMigrations(ctx, tx)
	if err != nil {
		return err
	}
	if err = validateAppliedPrefix(files, applied); err != nil {
		return err
	}
	for _, file := range files {
		if _, ok := applied[file.name]; ok {
			continue
		}
		if _, err = tx.Exec(ctx, file.sql); err != nil {
			return fmt.Errorf("migration %s: %w", file.name, err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO flopwire_schema_migrations(name,checksum) VALUES($1,$2)`, file.name, file.checksum); err != nil {
			return fmt.Errorf("record migration %s: %w", file.name, err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migrations: %w", err)
	}
	return nil
}

// validateAppliedPrefix requires the ledger to be an unmodified, contiguous
// prefix of the embedded files: no unknown, reordered, or edited migration,
// except the exact deployed 009 whose delta is supplied by the pinned 013.
func validateAppliedPrefix(files []migrationFile, applied map[string]string) error {
	if len(applied) > len(files) {
		return errors.New("migration ledger contains entries this binary does not know; refusing to run an older binary against a newer schema")
	}
	var busUpgradePresent bool
	for _, file := range files {
		if file.name == busUpgradeName && file.checksum == busUpgradeChecksum {
			busUpgradePresent = true
		}
	}
	for i, file := range files {
		checksum, ok := applied[file.name]
		if i < len(applied) && !ok {
			return fmt.Errorf("migration ledger is not a contiguous prefix at %s", file.name)
		}
		if i >= len(applied) && ok {
			return fmt.Errorf("migration ledger contains out-of-order entry %s", file.name)
		}
		if ok && checksum != file.checksum {
			if file.name == busMigrationName && file.checksum == busCanonicalChecksum &&
				checksum == busLegacyChecksum && busUpgradePresent {
				continue
			}
			return fmt.Errorf("migration %s checksum mismatch: an applied migration was edited", file.name)
		}
	}
	return nil
}

func migrationFiles(fsys fs.FS) ([]migrationFile, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	files := make([]migrationFile, 0, len(names))
	for _, name := range names {
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(raw)
		files = append(files, migrationFile{name, string(raw), hex.EncodeToString(sum[:])})
	}
	return files, nil
}

func readAppliedMigrations(ctx context.Context, tx pgx.Tx) (map[string]string, error) {
	rows, err := tx.Query(ctx, `SELECT name,checksum FROM flopwire_schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read migration ledger: %w", err)
	}
	defer rows.Close()
	applied := map[string]string{}
	for rows.Next() {
		var name, checksum string
		if err = rows.Scan(&name, &checksum); err != nil {
			return nil, err
		}
		applied[name] = checksum
	}
	return applied, rows.Err()
}
