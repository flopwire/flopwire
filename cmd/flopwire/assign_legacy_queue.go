package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/syncproto"
)

type legacyAssignment struct {
	Owner string
	Key   string
}

func agentAssignLegacyQueue(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("agent assign-legacy-queue", flag.ContinueOnError)
	path := fs.String("db", "", "existing collector index")
	server := fs.String("server", "", "explicit intended enrolled server")
	device := fs.String("device-id", "", "explicit intended enrolled device ID")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *path == "" || *server == "" || *device == "" {
		return errors.New("usage: flopwire agent assign-legacy-queue --db PATH --server URL --device-id ID")
	}
	// Own the writer lock before config-lock creation or any SQLite/spool mutation.
	lock, err := localindex.AcquireMaintenanceLock(*path)
	if err != nil {
		return fmt.Errorf("assignment requires a stopped collector: %w", err)
	}
	defer lock.Close()
	return client.WithConfigLock(ctx, func() error {
		cfg, err := client.Load()
		if err != nil {
			return err
		}
		intended, err := syncAuthorityOwner(client.Config{Server: *server, DeviceID: *device})
		if err != nil {
			return err
		}
		enrolled, err := syncAuthorityOwner(cfg)
		if err != nil {
			return err
		}
		if cfg.DeviceID == "" || enrolled != intended {
			return errors.New("explicit server/device does not match current enrollment; saved queue is preserved")
		}
		abs, err := filepath.Abs(*path)
		if err != nil {
			return err
		}
		abs, err = filepath.EvalSymlinks(abs)
		if err != nil {
			return err
		}
		u := url.URL{Scheme: "file", Path: abs}
		q := u.Query()
		q.Set("mode", "ro")
		u.RawQuery = q.Encode()
		preflight, err := sql.Open("sqlite", u.String())
		if err != nil {
			return err
		}
		err = legacyAssignmentCompatible(ctx, preflight)
		closeErr := preflight.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		q.Set("mode", "rw")
		q.Set("_pragma", "synchronous(FULL)")
		u.RawQuery = q.Encode()
		db, err := sql.Open("sqlite", u.String())
		if err != nil {
			return err
		}
		defer db.Close()
		db.SetMaxOpenConns(1)
		dir, err := configDir()
		if err != nil {
			return err
		}
		key, err := assignLegacyQueue(ctx, db, filepath.Join(dir, "spool"), abs, intended)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "queue assigned to enrolled server/device; spool scope %s; legacy captures retained\n", key)
		return err
	})
}

// Only authority metadata is written. Never open the local index or sync store:
// those constructors can migrate retention, queues, or capture representations.
func legacyAssignmentCompatible(ctx context.Context, db *sql.DB) error {
	var exists int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='devsync_gens'`).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return nil
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('devsync_gens') WHERE name='redactions'`).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return errors.New("legacy queue schema lacks redactions support; assignment is refused and saved state is preserved; retain this snapshot for a compatible recovery procedure")
	}
	return nil
}

func assignLegacyQueue(ctx context.Context, db *sql.DB, spool, dbPath, owner string) (string, error) {
	if err := legacyAssignmentCompatible(ctx, db); err != nil {
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
				return "", errors.New("queue already belongs to another server/device")
			}
			return key, validateSpoolKey(key)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := regularDirectory(spool, true); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(dbPath))
	journal := filepath.Join(spool, ".assign-"+hex.EncodeToString(sum[:])+".json")
	var plan legacyAssignment
	raw, err := os.ReadFile(journal)
	if err == nil {
		fi, e := os.Lstat(journal)
		if e != nil {
			return "", e
		}
		if !fi.Mode().IsRegular() {
			return "", errors.New("assignment journal is not regular")
		}
		if err = json.Unmarshal(raw, &plan); err != nil {
			return "", err
		}
		if plan.Owner != owner {
			return "", errors.New("unfinished assignment belongs to another server/device")
		}
		if err = validateSpoolKey(plan.Key); err != nil {
			return "", err
		}
	} else if errors.Is(err, os.ErrNotExist) {
		var nonce [16]byte
		if _, err = rand.Read(nonce[:]); err != nil {
			return "", err
		}
		plan = legacyAssignment{Owner: owner, Key: hex.EncodeToString(nonce[:])}
		raw, _ = json.Marshal(plan)
		if err = durableAssignmentFile(journal, raw); err != nil {
			return "", err
		}
	} else {
		return "", err
	}
	target := filepath.Join(spool, plan.Key)
	if err = regularDirectory(target, true); err != nil {
		return "", err
	}
	for _, name := range []string{"chunks", "tails"} {
		src := filepath.Join(spool, name)
		dst := filepath.Join(target, name)
		if err = regularDirectory(dst, true); err != nil {
			return "", err
		}
		if err = regularDirectory(src, false); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		// An older collector may have run between attempts. Never publish copied
		// captures that no longer exist in the legacy source.
		copied, e := os.ReadDir(dst)
		if e != nil {
			return "", e
		}
		for _, entry := range copied {
			if strings.HasPrefix(entry.Name(), ".assign-copy-") {
				continue
			}
			if _, e := os.Lstat(filepath.Join(src, entry.Name())); e != nil {
				return "", errors.New("assignment destination has a capture absent from legacy spool")
			}
		}
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		entries, e := os.ReadDir(src)
		if e != nil {
			return "", e
		}
		for _, entry := range entries {
			if err = ctx.Err(); err != nil {
				return "", err
			}
			p := filepath.Join(src, entry.Name())
			fi, e := os.Lstat(p)
			if e != nil {
				return "", e
			}
			if !fi.Mode().IsRegular() {
				return "", errors.New("legacy spool contains a nonregular capture")
			}
			data, e := os.ReadFile(p)
			if e != nil {
				return "", e
			}
			if name == "chunks" {
				var h syncproto.Hash
				if e := h.UnmarshalText([]byte(entry.Name())); e == nil && syncproto.Sum(data) != h {
					return "", errors.New("legacy chunk bytes do not match their hash")
				}
			}
			dest := filepath.Join(dst, entry.Name())
			if fi, e = os.Lstat(dest); e == nil {
				if !fi.Mode().IsRegular() {
					return "", errors.New("assignment destination is not regular")
				}
				prior, e := os.ReadFile(dest)
				if e != nil {
					return "", e
				}
				if !bytes.Equal(prior, data) {
					return "", errors.New("assignment destination differs from retained capture")
				}
				existing, e := os.OpenFile(dest, os.O_RDWR, 0)
				if e != nil {
					return "", e
				}
				e = existing.Sync()
				closeErr := existing.Close()
				if e != nil {
					return "", e
				}
				if closeErr != nil {
					return "", closeErr
				}
			} else if errors.Is(e, os.ErrNotExist) {
				if e = durableAssignmentFile(dest, data); e != nil {
					return "", e
				}
			} else {
				return "", e
			}
		}
		if err = syncAssignmentDir(dst); err != nil {
			return "", err
		}
	}
	if err = syncAssignmentDir(target); err != nil {
		return "", err
	}
	if err = syncAssignmentDir(spool); err != nil {
		return "", err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS devsync_authority (id INTEGER PRIMARY KEY CHECK(id=1), owner TEXT NOT NULL, spool_key TEXT NOT NULL)`); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO devsync_authority(id,owner,spool_key) VALUES(1,?,?)`, owner, plan.Key); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return plan.Key, nil
}

func regularDirectory(path string, create bool) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && create {
		if err = os.Mkdir(path, 0700); err != nil {
			return err
		}
		return syncAssignmentDir(filepath.Dir(path))
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return errors.New("assignment spool path is not a directory")
	}
	return nil
}
func syncAssignmentDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func durableAssignmentFile(path string, data []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".assign-copy-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncAssignmentDir(filepath.Dir(path))
}
