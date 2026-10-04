// Package opencode parses the opencode session store,
// ~/.local/share/opencode/opencode.db (issue #62).
//
// The store is one SQLite database (Drizzle schema) holding every local
// session. The parser reads three tables:
//
//   - session: id, parent_id (a subagent's parent session), directory,
//     title, version, agent, model, time_created and time_updated (epoch
//     milliseconds), time_archived.
//   - message: id, session_id, time_created, time_updated, data. data is
//     JSON {role, time, agent, model | modelID, providerID, path {cwd,
//     root}, error, finish, ...}; the id and session id are columns, not
//     fields of data.
//   - part: id, message_id, session_id, time_created, time_updated, data.
//     data is JSON with a type: text {text, synthetic, ignored, metadata},
//     reasoning {text}, tool {tool, callID, state {status, input, output,
//     error, metadata}}, file {mime, filename, url}, agent, subtask,
//     compaction, retry, step-start, step-finish, snapshot, patch.
//
// Parts are updated in place while a turn streams (time_updated moves) and
// deleted when the user reverts a turn. Ids are opencode's ascending ids:
// "prt_" or "msg_", then 12 hex digits holding the low 48 bits of
// (milliseconds * 4096 + a per-millisecond counter), then random base62.
//
// Format knowledge comes from the opencode 1.18.30 store on this machine
// (read-only) and the probes in notes/message-bus/probes-2026-10-01.md.
package opencode

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/flopwire/flopwire/internal/fsprobe"

	_ "modernc.org/sqlite" // registers the "sqlite" driver; pure Go, no cgo
)

const (
	// EnvDB names the opencode.db file to read, overriding every default.
	EnvDB = "FLOPWIRE_OPENCODE_DB"
	// EnvOpencodeDB is opencode's own override of its database path.
	EnvOpencodeDB = "OPENCODE_DB"
)

// DefaultPath returns the store to read: $FLOPWIRE_OPENCODE_DB, then
// opencode's $OPENCODE_DB when it names a file, then
// $XDG_DATA_HOME/opencode/opencode.db, then
// <home>/.local/share/opencode/opencode.db (`opencode db path` prints the
// same; opencode uses the XDG path on macOS too).
func DefaultPath(home string) string {
	return PathFrom(os.Getenv, home)
}

// PathFrom is DefaultPath with the environment read through getenv.
func PathFrom(getenv func(string) string, home string) string {
	if p := getenv(EnvDB); p != "" {
		return p
	}
	if p := getenv(EnvOpencodeDB); p != "" && p != ":memory:" && filepath.IsAbs(p) {
		return p
	}
	if x := getenv("XDG_DATA_HOME"); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, "opencode", "opencode.db")
	}
	return filepath.Join(home, ".local", "share", "opencode", "opencode.db")
}

// openReadOnly opens the store for reading only. mode=ro means SQLite never
// writes the database or its WAL, so it never checkpoints; query_only is a
// second guard. A running opencode may hold a write lock, hence the busy
// timeout. The file must exist: SQLite would otherwise report an empty
// database.
func openReadOnly(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if _, err := fsprobe.Stat(abs); err != nil {
		return nil, fmt.Errorf("opencode: %w", err)
	}
	fsprobe.Note(fsprobe.OpOpen, abs) // SQLite opens it below
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "query_only(1)")
	dsn := (&url.URL{Scheme: "file", Path: abs, RawQuery: q.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opencode: open %s: %w", abs, err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// snapshot starts a read transaction so every query of one Parse sees the
// same WAL snapshot.
func snapshot(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("opencode: begin read: %w", err)
	}
	return tx, nil
}

// Cwds returns every session's working directory, read-only.
func Cwds(ctx context.Context, path string) (map[string]string, error) {
	db, err := openReadOnly(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT id, ifnull(directory, '') FROM session`)
	if err != nil {
		return nil, fmt.Errorf("opencode: session directories: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, cwd string
		if err := rows.Scan(&id, &cwd); err != nil {
			return nil, err
		}
		out[id] = cwd
	}
	return out, rows.Err()
}
