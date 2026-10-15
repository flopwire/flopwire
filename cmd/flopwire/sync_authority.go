package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"

	"github.com/flopwire/flopwire/internal/client"
)

// syncStateReader is the SQL read surface shared by a connection and a
// transaction. Validation performs no schema changes or ownership assignment.
type syncStateReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func syncAuthorityOwner(cfg client.Config) (string, error) {
	server, err := client.NormalizeServer(cfg.Server)
	if err != nil {
		return "", err
	}
	identity := "server\x00" + server + "\x00device\x00" + cfg.DeviceID
	if cfg.DeviceID == "" {
		fingerprint := sha256.Sum256([]byte(cfg.Token))
		identity = "server\x00" + server + "\x00token\x00" + hex.EncodeToString(fingerprint[:])
	}
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:]), nil
}

func validateSpoolKey(key string) error {
	if raw, err := hex.DecodeString(key); err != nil || len(raw) != 16 {
		return errors.New("sync spool identity is invalid; saved state is preserved")
	}
	return nil
}

// readSyncAuthority runs under the index writer lock during agent startup,
// before retention/schema migration. Empty new state can be bound later.
func readSyncAuthority(ctx context.Context, db syncStateReader, cfg client.Config) (string, error) {
	owner, err := syncAuthorityOwner(cfg)
	if err != nil {
		return "", err
	}
	var exists int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='devsync_authority'`).Scan(&exists); err != nil {
		return "", err
	}
	if exists != 0 {
		var saved, key string
		err := db.QueryRowContext(ctx, `SELECT owner,spool_key FROM devsync_authority WHERE id=1`).Scan(&saved, &key)
		if err == nil {
			if saved != owner {
				return "", errors.New("sync state belongs to another server or device; its queued captures and acknowledgments are preserved. Restore that enrollment or use a separate --db path for this enrollment")
			}
			if err := validateSpoolKey(key); err != nil {
				return "", err
			}
			return key, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	// Even fully acknowledged generations and known hashes belong to a server.
	// An empty/missing binding row cannot establish the owner of older state.
	for _, table := range []string{"devsync_sources", "devsync_gens", "devsync_manifest", "devsync_known", "devsync_schedule_hints"} {
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&exists); err != nil {
			return "", err
		}
		if exists == 0 {
			continue
		}
		var nonempty int
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+table+` LIMIT 1)`).Scan(&nonempty); err != nil {
			return "", err
		}
		if nonempty != 0 {
			return "", errors.New("sync state has no recorded server/device owner; its queued captures and acknowledgments are preserved and uploads are paused. Use a separate --db path for a fresh queue; do not rebuild this index to assign its old uploads")
		}
	}
	return "", nil
}

// bindSyncAuthority binds watermarks, acknowledgments and queued captures to
// their destination before NewStore or spool cleanup can mutate saved state.
func bindSyncAuthority(ctx context.Context, db *sql.DB, cfg client.Config) (string, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	key, err := readSyncAuthority(ctx, tx, cfg)
	if err != nil {
		return "", err
	}
	if key != "" {
		return key, nil
	}
	owner, err := syncAuthorityOwner(cfg)
	if err != nil {
		return "", err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	key = hex.EncodeToString(nonce[:])
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS devsync_authority (id INTEGER PRIMARY KEY CHECK(id=1), owner TEXT NOT NULL, spool_key TEXT NOT NULL)`); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO devsync_authority(id,owner,spool_key) VALUES(1,?,?)`, owner, key); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return key, nil
}

// savedSyncSpoolKey resolves maintenance against existing state without binding
// ownerless data to whichever credential happens to be installed now.
func savedSyncSpoolKey(ctx context.Context, db *sql.DB) (string, error) {
	var exists int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='devsync_authority'`).Scan(&exists); err != nil {
		return "", err
	}
	if exists == 0 {
		return "", nil
	}
	var key string
	if err := db.QueryRowContext(ctx, `SELECT spool_key FROM devsync_authority WHERE id=1`).Scan(&key); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	if err := validateSpoolKey(key); err != nil {
		return "", err
	}
	return key, nil
}
