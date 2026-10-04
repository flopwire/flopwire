// Package opencodetest builds synthetic opencode stores for tests: the
// opencode 1.18.30 DDL of the tables Flopwire reads, and helpers that
// write sessions, messages and parts.
package opencodetest

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Schema is the opencode 1.18.30 DDL of the session, message and part
// tables and their indexes.
const Schema = "CREATE TABLE `session` (`id` text PRIMARY KEY, `project_id` text NOT NULL, `workspace_id` text, `parent_id` text," +
	" `slug` text NOT NULL, `directory` text NOT NULL, `path` text, `title` text NOT NULL, `version` text NOT NULL, `share_url` text," +
	" `summary_additions` integer, `summary_deletions` integer, `summary_files` integer, `summary_diffs` text, `metadata` text," +
	" `cost` real DEFAULT 0 NOT NULL, `tokens_input` integer DEFAULT 0 NOT NULL, `tokens_output` integer DEFAULT 0 NOT NULL," +
	" `tokens_reasoning` integer DEFAULT 0 NOT NULL, `tokens_cache_read` integer DEFAULT 0 NOT NULL, `tokens_cache_write` integer DEFAULT 0 NOT NULL," +
	" `revert` text, `permission` text, `agent` text, `model` text, `time_created` integer NOT NULL, `time_updated` integer NOT NULL," +
	" `time_compacting` integer, `time_archived` integer);\n" +
	"CREATE TABLE `message` (`id` text PRIMARY KEY, `session_id` text NOT NULL, `time_created` integer NOT NULL, `time_updated` integer NOT NULL, `data` text NOT NULL);\n" +
	"CREATE TABLE `part` (`id` text PRIMARY KEY, `message_id` text NOT NULL, `session_id` text NOT NULL, `time_created` integer NOT NULL, `time_updated` integer NOT NULL, `data` text NOT NULL);\n" +
	"CREATE INDEX `message_session_time_created_id_idx` ON `message` (`session_id`,`time_created`,`id`);\n" +
	"CREATE INDEX `part_message_id_id_idx` ON `part` (`message_id`,`id`);\n" +
	"CREATE INDEX `part_session_idx` ON `part` (`session_id`);\n"

// T0 is a fixture clock: 2026-10-03, epoch milliseconds.
const T0 = int64(1791000000000)

// ID makes an opencode ascending id ("prt", "msg") for millisecond ms and
// per-millisecond counter c.
func ID(prefix string, ms int64, c int) string {
	v := uint64(ms*4096+int64(c)) & (1<<48 - 1)
	return fmt.Sprintf("%s_%012xSYNTHETICxxxxx", prefix, v)
}

// Store is a writable synthetic opencode.db.
type Store struct {
	t    testing.TB
	Path string
	DB   *sql.DB
}

// New creates opencode.db with Schema in dir (a temp dir when "").
func New(t testing.TB, dir string) *Store {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	path := filepath.Join(dir, "opencode.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	s := &Store{t: t, Path: path, DB: db}
	s.Exec(Schema)
	return s
}

// Exec runs one statement or fails the test.
func (s *Store) Exec(q string, args ...any) {
	s.t.Helper()
	if _, err := s.DB.Exec(q, args...); err != nil {
		s.t.Fatalf("%s: %v", q, err)
	}
}

// Session writes a session row; parent "" is a top-level session.
func (s *Store) Session(id, parent, dir, title string, at int64) {
	s.t.Helper()
	var p any
	if parent != "" {
		p = parent
	}
	s.Exec(`INSERT INTO session (id, project_id, parent_id, slug, directory, title, version, agent, model, time_created, time_updated)
		VALUES (?, 'prj_synthetic', ?, 'calm-otter', ?, ?, '1.18.30', 'build', '{"id":"big-pickle","providerID":"opencode","variant":"default"}', ?, ?)`,
		id, p, dir, title, at, at)
}

// Message writes a message row.
func (s *Store) Message(id, session string, at int64, data string) {
	s.t.Helper()
	s.Exec(`INSERT INTO message VALUES (?, ?, ?, ?, ?)`, id, session, at, at, data)
}

// Part writes a part row.
func (s *Store) Part(id, msg, session string, at int64, data string) {
	s.t.Helper()
	s.Exec(`INSERT INTO part VALUES (?, ?, ?, ?, ?, ?)`, id, msg, session, at, at, data)
}

// UpdatePart rewrites a part's data, as opencode does while a turn streams.
func (s *Store) UpdatePart(id string, at int64, data string) {
	s.t.Helper()
	s.Exec(`UPDATE part SET data = ?, time_updated = ? WHERE id = ?`, data, at, id)
}

// Prompt writes a user message with one text part at ms and returns the
// part id.
func (s *Store) Prompt(session string, ms int64, text string) string {
	s.t.Helper()
	msg, part := ID("msg", ms, 1), ID("prt", ms, 2)
	s.Message(msg, session, ms, `{"role":"user","time":{"created":1}}`)
	s.Part(part, msg, session, ms, fmt.Sprintf(`{"type":"text","text":%q}`, text))
	return part
}
