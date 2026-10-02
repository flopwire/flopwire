package localindex

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"strings"
)

// schemaVersion is PRAGMA user_version after migrate. The index is derived
// data: an incompatible change bumps the version, and the next writing
// Open drops the old tables and the indexer rebuilds from the transcripts.
// 4: whole-text trigram index (no cap), messages_meta and messages_ts.
// 5: placements. 6: placements.checked_at and candidates.
// 7: conversations_session, for address lookups by session id prefix, and
// the token table at detail=full so bm25 ranks. 8: placements.other_cwds
// and withhold. 9: conversations.branches and conversations.digest.
// 10: sources.extraction_report.
// 11: messages_tool_call and conversations_unspawned, for link resolution.
// 12: messages_sha, for local redactions (now built on first use: shaIndexSQL).
// Placements are carried across a rebuild (carryPlacements): a session
// whose worktree is gone cannot be placed again from its transcript.
const schemaVersion = 12

// Column types follow spec §4 with SQLite equivalents: integer row ids
// (FTS5 keys on the integer rowid), times as unix milliseconds, booleans as
// 0/1, hashes as blobs, jsonb as JSON text.
const schemaSQL = `
CREATE TABLE sources (
  id             INTEGER PRIMARY KEY,
  device_id      TEXT NOT NULL,
  agent          TEXT NOT NULL,
  path           TEXT NOT NULL,
  file_id        TEXT NOT NULL,
  session_key    TEXT,
  storage_kind   TEXT NOT NULL,
  parser         TEXT NOT NULL,
  first_seen_at  INTEGER NOT NULL,
  generation     INTEGER NOT NULL DEFAULT 0,  -- current generation
  -- watermark (spec §6.2): identity tuple sampled before the parse
  wm_dev         INTEGER,
  wm_ino         INTEGER,
  wm_size        INTEGER,
  wm_ctime       INTEGER,
  wm_sampled_at  INTEGER,
  wm_offset      INTEGER,                     -- indexed byte offset (or rowid watermark)
  wm_line_no     INTEGER,
  wm_head_len    INTEGER,
  wm_head_hash   BLOB,
  wm_anchor_len  INTEGER,
  wm_anchor_hash BLOB,
  extraction_report TEXT,                     -- scoped extraction checkpoint JSON
  cursor_state   BLOB,                        -- opaque parser cursor state
  UNIQUE (device_id, path, file_id)
);

CREATE TABLE generations (
  source_id   INTEGER NOT NULL,
  generation  INTEGER NOT NULL,
  size        INTEGER NOT NULL,
  change_time INTEGER,
  captured_at INTEGER NOT NULL,
  complete    INTEGER NOT NULL,
  reason      TEXT,
  PRIMARY KEY (source_id, generation)
) WITHOUT ROWID;

CREATE TABLE conversations (
  id                      INTEGER PRIMARY KEY,
  source_id               INTEGER,
  agent                   TEXT NOT NULL,
  session_id              TEXT NOT NULL,
  device_id               TEXT NOT NULL,
  cwd                     TEXT,
  repo_root               TEXT,
  title                   TEXT,
  started_at              INTEGER,
  last_activity_at        INTEGER,
  parent_session_id       TEXT,     -- native parent id, kept until resolved
  parent_conversation_id  INTEGER,
  spawned_by_tool_call_id TEXT,     -- native id of the parent's spawning tool call
  spawned_by_message_id   INTEGER,
  depth                   INTEGER NOT NULL DEFAULT 0,
  extra                   TEXT,     -- JSON
  branches                TEXT,     -- JSON array of git branches, first seen first
  digest                  TEXT,     -- JSON, internal/digest
  deleted_in_generation   INTEGER,  -- tombstone: the session vanished from its source
  UNIQUE (device_id, agent, session_id)
);
CREATE INDEX conversations_parent ON conversations (agent, parent_session_id) WHERE parent_session_id IS NOT NULL;
CREATE INDEX conversations_activity ON conversations (last_activity_at);
CREATE INDEX conversations_session ON conversations (session_id);
-- Children whose spawning tool call is not found yet (resolveLinks).
CREATE INDEX conversations_unspawned ON conversations (parent_conversation_id)
  WHERE spawned_by_message_id IS NULL AND spawned_by_tool_call_id IS NOT NULL;

CREATE TABLE messages (
  id                       INTEGER PRIMARY KEY,  -- FTS rowid
  conversation_id          INTEGER NOT NULL,
  source_id                INTEGER NOT NULL,
  native_id                TEXT,
  part                     INTEGER NOT NULL DEFAULT 0,  -- message index within its physical record
  parent_native_id         TEXT,
  ordinal                  INTEGER NOT NULL,
  kind                     TEXT NOT NULL,
  role                     TEXT,
  tool_name                TEXT,
  tool_call_id             TEXT,
  is_error                 INTEGER,
  ts                       INTEGER,
  text_len                 INTEGER NOT NULL,     -- bytes of the capped text
  full_len                 INTEGER NOT NULL,     -- bytes of the uncapped text
  content_sha              BLOB NOT NULL,        -- sha256 of the uncapped text
  version                  INTEGER NOT NULL DEFAULT 1,
  superseded               INTEGER NOT NULL DEFAULT 0,
  superseded_by            INTEGER,
  superseded_in_generation INTEGER,
  on_active_path           INTEGER,              -- NULL unless the harness records a pointer
  enrichment               TEXT,                 -- JSON (commands, changed_paths, ...)
  source_generation        INTEGER NOT NULL,
  line_no                  INTEGER,
  byte_offset              INTEGER,
  byte_len                 INTEGER,
  locator                  TEXT,
  parser                   TEXT NOT NULL,
  -- Last, so reading the filter columns never walks the blob's overflow pages.
  text                     BLOB NOT NULL         -- zstd of the capped extracted text
);
-- Identity: one head version per (conversation, native_id, part), else per
-- (source, locator, part). Older versions carry superseded_by.
CREATE UNIQUE INDEX messages_native_head ON messages (conversation_id, native_id, part)
  WHERE native_id IS NOT NULL AND superseded_by IS NULL;
CREATE UNIQUE INDEX messages_locator_head ON messages (source_id, ifnull(locator, byte_offset), part)
  WHERE native_id IS NULL AND superseded_by IS NULL;
CREATE INDEX messages_conv_ordinal ON messages (conversation_id, ordinal);
-- Default search filter (spec §4.2).
CREATE INDEX messages_default ON messages (conversation_id, ordinal)
  WHERE superseded = 0 AND on_active_path IS NOT 0;
CREATE INDEX messages_source_gen ON messages (source_id, source_generation);
-- Failed tool calls, for the digest's count on append.
CREATE INDEX messages_failed ON messages (conversation_id, tool_call_id) WHERE is_error = 1;
-- A subagent's spawning tool call in its parent (resolveLinks).
CREATE INDEX messages_tool_call ON messages (conversation_id, tool_call_id)
  WHERE kind = 'tool_call' AND superseded_by IS NULL AND tool_call_id IS NOT NULL;
-- Candidate ordering by message time (decision D7). Find and the
-- ranked-search cap order FTS candidates either by looking up each one's
-- time (messages_meta, keyed by id) or, for many candidates, by walking
-- messages_ts newest first. Both cover the filter columns on messages, so
-- neither reads message rows, which hold the text inline (about 1KB a row;
-- 78k lookups there took 5s cold, against 0.06s in a covering index).
-- Queries name them with INDEXED BY.
CREATE INDEX messages_meta ON messages (id, ts, conversation_id, superseded, on_active_path, kind);
CREATE INDEX messages_ts ON messages (ts, id, superseded, on_active_path, kind, conversation_id);

-- Claude subagent meta.json and tool-results/ files (decision 10).
CREATE TABLE companions (
  id              INTEGER PRIMARY KEY,
  conversation_id INTEGER,
  source_id       INTEGER,
  path            TEXT NOT NULL,
  kind            TEXT NOT NULL,     -- subagent_meta | tool_result | ...
  size            INTEGER NOT NULL,
  content_sha     BLOB,
  indexed_text    INTEGER NOT NULL DEFAULT 0,  -- 1 when its text replaced a row's preview
  message_id      INTEGER,
  seen_at         INTEGER NOT NULL,
  UNIQUE (path)
);
CREATE INDEX companions_conversation ON companions (conversation_id);

-- Where each session ran, for path rules (D18): resolved once, when the
-- agent first sees the session, and kept when its rows are purged, its
-- worktree is deleted or the index is rebuilt, so rules still apply (and
-- re-apply when they change) after the directory is gone. Keyed like
-- conversations.
CREATE TABLE placements (
  agent         TEXT NOT NULL,
  session_id    TEXT NOT NULL,
  cwd           TEXT,
  worktree_root TEXT,
  main_root     TEXT,
  remote        TEXT,              -- origin, normalized host/owner/name
  how           TEXT NOT NULL,     -- localindex.PlacedBy*
  resolved_at   INTEGER NOT NULL,
  checked_at    INTEGER,           -- last search for a deleted worktree's main checkout
  candidates    TEXT,              -- an ambiguous search's repositories: "main<TAB>remote" lines
  other_cwds    TEXT,              -- the other directories the session named: "cwd<TAB>worktree<TAB>main<TAB>remote" lines
  withhold      TEXT,              -- a server deletion owed (a later directory tightened the verdict): "mode<TAB>rule"
  PRIMARY KEY (agent, session_id)
) WITHOUT ROWID;

-- FTS changes not yet applied by every FTS shard (fts.go).
CREATE TABLE fts_queue (
  seq    INTEGER PRIMARY KEY AUTOINCREMENT,
  msg_id INTEGER NOT NULL,
  op     INTEGER NOT NULL   -- 1 delete, 2 index current text
);
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID;
`

// The two contentless FTS5 tables, keyed by messages.id, live in shard
// files of their own (fts.go). fts_tok: unicode61 with '_-./' as token
// characters, so paths, flags and dotted identifiers stay whole. fts_tri:
// trigram, case-insensitive.
const (
	tokTokenize = `"unicode61 remove_diacritics 2 tokenchars '_-./'"`
	triTokenize = `"trigram case_sensitive 0"`
)

// Index modes, stored in meta under 'mode'. A full index holds message
// rows and the FTS shards; a sync-only index holds only what the agent
// needs to hand sources to sync (sources, watermarks, generations,
// conversations, companions, placements) and has no shard files.
const (
	ModeFull     = "full"
	ModeSyncOnly = "sync-only"
)

// ErrSyncOnly is returned by a read-only Open of a sync-only index: there
// is nothing local to query.
var ErrSyncOnly = errors.New("this device is sync-only; use --server")

// migrate creates the schema on an empty database and returns the token
// tables' detail levels. An index built in the other mode (mode) is
// dropped and created again, as for an older layout, so a switch to full
// builds the message rows from the transcripts and a switch to sync-only
// drops them; rebuilt reports that. force rebuilds an index of the current
// layout and mode the same way (Options.RebuildIndex).
func migrate(db *sql.DB, want Details, mode string, force bool) (got Details, rebuilt bool, err error) {
	for _, d := range []Detail{want.Tok, want.Tri} {
		if d != DetailColumn && d != DetailFull {
			return Details{}, false, fmt.Errorf("unknown detail %q", d)
		}
	}
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return Details{}, false, err
	}
	if v == schemaVersion {
		var have string

		err := db.QueryRow(`SELECT (SELECT value FROM meta WHERE key='tok_detail'), (SELECT value FROM meta WHERE key='tri_detail'),
			(SELECT CAST(value AS INTEGER) FROM meta WHERE key='tri_parts'), ifnull((SELECT value FROM meta WHERE key='mode'), '')`).Scan(&got.Tok, &got.Tri, &got.TriParts, &have)
		if err != nil || have == mode && !force {
			return got, false, err
		}
		if have != mode {
			slog.Info("localindex: index mode changed; rebuilding", "from", have, "to", mode)
		} else {
			slog.Info("localindex: rebuilding the index on request")
		}
	}
	tx, err := db.Begin()
	if err != nil {
		return Details{}, false, err
	}
	defer tx.Rollback()
	keep := false
	if v != 0 {
		// An older layout or another mode: drop it (the caller holds the
		// index lock). Other tables in the file (devicesync's) are left
		// alone. A new index_id makes the FTS shards start over. Placements
		// are set aside first.
		if keep, err = setAsidePlacements(tx); err != nil {
			return Details{}, false, err
		}
		for _, t := range ownTables {
			if _, err := tx.Exec(`DROP TABLE IF EXISTS ` + t); err != nil {
				return Details{}, false, err
			}
		}
	}
	for _, stmt := range splitSQL(schemaSQL) {
		if _, err := tx.Exec(stmt); err != nil {
			return Details{}, false, fmt.Errorf("%w in %s", err, stmt)
		}
	}
	if keep {
		if err := carryPlacements(tx); err != nil {
			return Details{}, false, err
		}
	}
	// index_id ties the FTS shard files to this database: a shard left from
	// an index that was deleted is rebuilt, not trusted.
	if _, err := tx.Exec(`INSERT INTO meta VALUES ('tok_detail', ?), ('tri_detail', ?), ('tri_parts', ?), ('index_id', ?), ('mode', ?)`,
		string(want.Tok), string(want.Tri), strconv.Itoa(want.TriParts), strconv.FormatInt(rand.Int64(), 10), mode); err != nil {
		return Details{}, false, err
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return Details{}, false, err
	}
	return want, v != 0, tx.Commit()
}

// setAsidePlacements renames an older layout's placements table so the
// rebuild keeps it. It reports whether there was one.
func setAsidePlacements(tx *sql.Tx) (bool, error) {
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'placements'`).Scan(&n); err != nil || n == 0 {
		return false, err
	}
	if _, err := tx.Exec(`DROP TABLE IF EXISTS placements_old`); err != nil {
		return false, err
	}
	_, err := tx.Exec(`ALTER TABLE placements RENAME TO placements_old`)
	return err == nil, err
}

// carryPlacements copies the set-aside placements into the new table,
// column by column where the names match, and drops the old table. Rows
// the new layout cannot take are dropped.
func carryPlacements(tx *sql.Tx) error {
	cols := func(table string) (map[string]bool, []string, error) {
		rows, err := tx.Query(`SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			return nil, nil, err
		}
		defer rows.Close()
		set, list := map[string]bool{}, []string(nil)
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				return nil, nil, err
			}
			set[c] = true
			list = append(list, c)
		}
		return set, list, rows.Err()
	}
	old, _, err := cols("placements_old")
	if err != nil {
		return err
	}
	_, cur, err := cols("placements")
	if err != nil {
		return err
	}
	var common []string
	for _, c := range cur {
		if old[c] {
			common = append(common, `"`+c+`"`)
		}
	}
	if len(common) > 0 {
		list := strings.Join(common, ", ")
		// A failing copy (a column the new layout requires is missing)
		// leaves the new table empty: the placements are re-derived.
		if _, err := tx.Exec(`INSERT OR IGNORE INTO placements (` + list + `) SELECT ` + list + ` FROM placements_old`); err != nil {
			if _, err := tx.Exec(`DELETE FROM placements`); err != nil {
				return err
			}
		}
	}
	_, err = tx.Exec(`DROP TABLE placements_old`)
	return err
}

// ownTables are the tables schemaSQL creates.
var ownTables = []string{"sources", "generations", "conversations", "messages", "companions", "placements", "fts_queue", "meta"}

// splitSQL splits on statement-ending semicolons; the schema has none inside
// literals.
func splitSQL(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ";\n") {
		if strings.TrimSpace(stripComments(part)) != "" {
			out = append(out, part)
		}
	}
	return out
}

func stripComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
	}
	return b.String()
}
