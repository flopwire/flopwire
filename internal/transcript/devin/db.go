// Package devin parses the Devin CLI session store,
// ~/.local/share/devin/cli/sessions.db (spec §5.3, §5.4).
//
// The store is one SQLite database holding every local session:
//
//   - sessions: id, working_directory, model, agent_mode, created_at and
//     last_activity_at (epoch seconds), title, main_chain_id (the tip of the
//     live conversation), hidden.
//   - message_nodes: a forest per session. row_id is AUTOINCREMENT;
//     (session_id, node_id) is unique; parent_node_id links a node to its
//     parent; chat_message is JSON {message_id, role, content, tool_calls,
//     tool_call_id, thinking, images, metadata}; created_at is epoch seconds.
//     Compaction copies nodes into a new tree under the same message_id.
//   - tool_call_state: one row per (session_id, tool_call_id), updated in
//     place when the tool finishes.
//
// Format knowledge comes from franken-agent-detection 0.3.1
// src/connectors/devin.rs (MIT, git c06d1cb; main-chain walk, content and
// image handling, the CASS_DEVIN_DATA_ROOT override), kenn-io/agentsview
// internal/parser/devin.go (MIT, 563023de1d7b7f5af50ad5967c101341a44a2bfc)
// and gbasin/agentboard src/server/devinSync.ts (MIT,
// 4b2f9c88c4f92b333f7758fd3481082eb899a69f; internal user rows carry
// is_user_input null). No code is copied; see third_party/fad.
package devin

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
	// EnvDB names the sessions.db file to read, overriding every default.
	EnvDB = "FLOPWIRE_DEVIN_DB"
	// EnvDataRoot is FAD's override: the sessions.db file, the cli
	// directory holding it, or the devin data root above that.
	EnvDataRoot = "CASS_DEVIN_DATA_ROOT"
)

// DefaultPath returns the store to read: $FLOPWIRE_DEVIN_DB, then
// $CASS_DEVIN_DATA_ROOT resolved like FAD does, then
// <home>/.local/share/devin/cli/sessions.db. The CLI uses that path on
// every platform, including macOS.
func DefaultPath(home string) string {
	if p := os.Getenv(EnvDB); p != "" {
		return p
	}
	if root := os.Getenv(EnvDataRoot); root != "" {
		return pathFromRoot(root)
	}
	return filepath.Join(home, ".local", "share", "devin", "cli", "sessions.db")
}

// pathFromRoot accepts a .db file, a directory holding sessions.db, or the
// devin data root with cli/sessions.db below it.
func pathFromRoot(root string) string {
	if filepath.Ext(root) == ".db" {
		return root
	}
	direct := filepath.Join(root, "sessions.db")
	if fi, err := fsprobe.Stat(direct); err == nil && fi.Mode().IsRegular() {
		return direct
	}
	nested := filepath.Join(root, "cli", "sessions.db")
	if fi, err := fsprobe.Stat(nested); err == nil && fi.Mode().IsRegular() {
		return nested
	}
	return direct
}

// openReadOnly opens the store for reading only. mode=ro means SQLite never
// writes the database or its WAL, so it never checkpoints; query_only is a
// second guard. The live CLI may hold a write lock, hence the busy timeout.
// The file must exist: SQLite would otherwise report an empty database.
func openReadOnly(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if _, err := fsprobe.Stat(abs); err != nil {
		return nil, fmt.Errorf("devin: %w", err)
	}
	fsprobe.Note(fsprobe.OpOpen, abs) // SQLite opens it below
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "query_only(1)")
	dsn := (&url.URL{Scheme: "file", Path: abs, RawQuery: q.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("devin: open %s: %w", abs, err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// snapshot starts a read transaction so every query of one Parse sees the
// same WAL snapshot.
func snapshot(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("devin: begin read: %w", err)
	}
	return tx, nil
}
