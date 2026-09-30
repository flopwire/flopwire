package devin

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

const seedPath = "../../../testdata/oracle/seeds/devin.sql"

// buildDB builds the synthetic store from the oracle seed in a temp dir and
// returns its path and a writable handle for mutating it between parses.
func buildDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	seed, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "sessions.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(string(seed)); err != nil {
		t.Fatal(err)
	}
	return path, db
}

func exec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func parse(t *testing.T, path string, cur transcript.Cursor) (*transcript.Collector, transcript.Cursor) {
	t.Helper()
	c := &transcript.Collector{}
	p := &Parser{}
	src := &transcript.Source{Agent: transcript.AgentDevin, Path: path, StorageKind: transcript.StorageSQLite, Parser: p.Name()}
	next, err := p.Parse(context.Background(), transcript.Input{Source: src}, cur, c)
	if err != nil {
		t.Fatal(err)
	}
	return c, next
}

// store is a minimal upserting index: the latest emission per row key wins,
// SupersedeSession marks rows superseded, and a re-emission revives them.
type store map[string]*transcript.Message

// rowKey is the index's row identity: (native id, part), else (locator,
// part).
func rowKey(m *transcript.Message) string {
	if m.NativeID != "" {
		if m.Part != 0 {
			return fmt.Sprintf("%s|%s|%d", m.SessionID, m.NativeID, m.Part)
		}
		return m.SessionID + "|" + m.NativeID
	}
	return fmt.Sprintf("%s|@%s|%d", m.SessionID, m.Locator, m.Part)
}

func (s store) apply(c *transcript.Collector) {
	// Collector applies SupersedeSession to what it holds, in call order;
	// replay the same order here: supersede first, then upsert.
	for _, id := range c.SupersededSessions {
		for _, m := range s {
			if m.SessionID == id {
				m.Superseded = true
			}
		}
	}
	for _, m := range c.Messages {
		cp := *m
		cp.Superseded = false
		s[rowKey(&cp)] = &cp
	}
}

func (s store) get(t *testing.T, session, native string) *transcript.Message {
	t.Helper()
	m := s[session+"|"+native]
	if m == nil {
		t.Fatalf("no row %s|%s", session, native)
	}
	return m
}

func onPath(m *transcript.Message) string {
	if m.OnActivePath == nil {
		return "nil"
	}
	if *m.OnActivePath {
		return "true"
	}
	return "false"
}

func TestFullParse(t *testing.T) {
	path, _ := buildDB(t)
	c, cur := parse(t, path, transcript.Cursor{})
	s := store{}
	s.apply(c)

	// Every emitted native id is unique: compaction copies and twins
	// collapse to one row.
	seen := map[string]bool{}
	for _, m := range c.Messages {
		if seen[rowKey(m)] {
			t.Errorf("row %s emitted twice in one parse", rowKey(m))
		}
		seen[rowKey(m)] = true
	}

	var convs []string
	for _, cv := range c.Conversations {
		convs = append(convs, cv.SessionID)
	}
	if want := []string{"devin-oracle-001", "devin-oracle-002", "devin-oracle-003", "devin-oracle-004", "devin-oracle-005"}; !slices.Equal(convs, want) {
		t.Fatalf("conversations %v, want %v", convs, want)
	}
	if h := c.Conversations[3].Extra["hidden"]; h != true {
		t.Errorf("hidden session Extra[hidden] = %v", h)
	}
	if cv := c.Conversations[0]; cv.Cwd != "/tmp/oracle-alpha" || cv.Title != "Fix login bug" || cv.StartedAt.Unix() != 1790157600 {
		t.Errorf("conversation 1 = %+v", cv)
	}

	const s1, s3 = "devin-oracle-001", "devin-oracle-003"
	cases := []struct {
		session, native string
		kind            transcript.Kind
		onPath          string
		text            string
	}{
		{s1, "dm-001", transcript.KindSystem, "true", "You are Devin."},
		{s1, "dm-002", transcript.KindUser, "true", "fix the login bug"},
		{s1, "dm-003#thinking", transcript.KindThinking, "true", "Start with the auth module."},
		{s1, "dm-003", transcript.KindAssistant, "true", "Reading auth.ts."},
		{s1, "dm-003#call:call_d1", transcript.KindToolCall, "true", "read\n{\"path\":\"auth.ts\"}"},
		{s1, "dm-004", transcript.KindAssistant, "false", "(abandoned retry)"},
		{s1, "dm-005", transcript.KindToolResult, "true", "export function login() { return fetch(url) }"},
		// Compaction: the on-chain copy's text wins; the original is folded in.
		{s3, "dm-307", transcript.KindAssistant, "true", "Swapping the map for a redis client (resumed)."},
		{s3, "dm-308", transcript.KindToolResult, "true", "edited src/cache.go"},
		{s3, "dm-312", transcript.KindSystem, "true", "You are continuing work from a previous conversation. Summary: the user asked to port the cache to redis."},
		// Compacted-away history and the summarizer tree stay, off path.
		{s3, "dm-303#call:exec:0#aaa", transcript.KindToolCall, "false", "exec\n{\"command\":\"grep -rn cache src\"}"},
		{s3, "dm-305", transcript.KindAssistant, "false", "Found the cache type."},
		{s3, "dm-306", transcript.KindSystem, "false", "continue"}, // keepalive
		{s3, "dm-310", transcript.KindSystem, "false", "Conversation to summarize: port the cache to redis"},
		{s3, "dm-311", transcript.KindAssistant, "false", "Summary: the user asked to port the cache to redis."},
		{"devin-oracle-004", "dm-401", transcript.KindUser, "true", "a prompt in a hidden session"},
		{"devin-oracle-005", "dm-501", transcript.KindUser, "nil", "a prompt whose session row is gone"},
	}
	for _, tc := range cases {
		m := s.get(t, tc.session, tc.native)
		if m.Kind != tc.kind || onPath(m) != tc.onPath || m.Text != tc.text {
			t.Errorf("%s: kind %v on_path %s text %q; want %v %s %q", tc.native, m.Kind, onPath(m), m.Text, tc.kind, tc.onPath, tc.text)
		}
		if m.Parser != Name || m.Superseded {
			t.Errorf("%s: parser %q superseded %v", tc.native, m.Parser, m.Superseded)
		}
	}

	// Collapsed rows keep the first copy's position and time; the locator
	// names the copy whose content won.
	m := s.get(t, s3, "dm-307")
	if m.Locator != "devin-oracle-003/14/"+rowIDOf(t, path, s3, 14) || m.TS.Unix() != 1790160007 {
		t.Errorf("dm-307 locator %q ts %d", m.Locator, m.TS.Unix())
	}
	if m.Ordinal != transcript.OrdinalAt(mustInt(rowIDOf(t, path, s3, 8)), 1) { // text slot
		t.Errorf("dm-307 ordinal %d not at first sight", m.Ordinal)
	}
	if m.ParentNativeID != "dm-312" {
		t.Errorf("dm-307 parent %q, want the on-chain parent dm-312", m.ParentNativeID)
	}

	// tool_call_state enrichment.
	call := s.get(t, s3, "dm-303#call:exec:0#aaa")
	if !call.IsError || call.ToolName != "exec" || call.ToolCallID != "exec:0#aaa" {
		t.Errorf("exec call: %+v", call)
	}
	cmds, _ := call.Enrichment["commands"].([]map[string]any)
	if len(cmds) != 1 || cmds[0]["cmd"] != "grep -rn cache src" || cmds[0]["exit_code"] != int64(1) || cmds[0]["cwd"] != "/tmp/oracle-gamma" {
		t.Errorf("exec enrichment %v", call.Enrichment)
	}
	if r := s.get(t, s3, "dm-304"); !r.IsError || r.ToolName != "exec" {
		t.Errorf("exec result: is_error %v tool %q", r.IsError, r.ToolName)
	}
	edit := s.get(t, s3, "dm-307#call:edit:0#bbb")
	if p, _ := edit.Enrichment["changed_paths"].([]string); len(p) != 1 || p[0] != "/tmp/oracle-gamma/src/cache.go" || edit.IsError {
		t.Errorf("edit enrichment %v is_error %v", edit.Enrichment, edit.IsError)
	}

	if cur.Offset != int64(28) {
		t.Errorf("cursor offset %d, want max row_id 28", cur.Offset)
	}
	if len(c.SupersededSessions) != 0 {
		t.Errorf("superseded %v on a first parse", c.SupersededSessions)
	}
}

func rowIDOf(t *testing.T, path, session string, node int64) string {
	t.Helper()
	db, err := openReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var r string
	if err := db.QueryRow(`SELECT row_id FROM message_nodes WHERE session_id = ? AND node_id = ?`, session, node).Scan(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

func mustInt(s string) int64 {
	var n int64
	for _, c := range s {
		n = n*10 + int64(c-'0')
	}
	return n
}

func TestUnchangedStoreEmitsNothing(t *testing.T) {
	path, _ := buildDB(t)
	_, cur := parse(t, path, transcript.Cursor{})
	c, next := parse(t, path, cur)
	if len(c.Messages)+len(c.Conversations)+len(c.SupersededSessions) != 0 {
		t.Fatalf("second parse emitted %d messages, %d conversations, %v superseded", len(c.Messages), len(c.Conversations), c.SupersededSessions)
	}
	// The second parse cools the sessions (one more tool_call_state check
	// after activity); from then on the cursor is stable.
	c, again := parse(t, path, next)
	if len(c.Messages)+len(c.Conversations) != 0 || again.Offset != cur.Offset || string(again.State) != string(next.State) {
		t.Fatalf("cursor moved without changes")
	}
}

// snapshotStore parses from scratch and applies it to a fresh store.
func snapshotStore(t *testing.T, path string) store {
	c, _ := parse(t, path, transcript.Cursor{})
	s := store{}
	s.apply(c)
	return s
}

// assertSame compares the live rows of two stores field by field.
func assertSame(t *testing.T, got, want store) {
	t.Helper()
	for k, w := range want {
		g := got[k]
		if g == nil {
			t.Errorf("%s missing from incremental store", k)
			continue
		}
		if g.Text != w.Text || g.Kind != w.Kind || onPath(g) != onPath(w) || g.Ordinal != w.Ordinal ||
			g.Locator != w.Locator || g.ParentNativeID != w.ParentNativeID || g.IsError != w.IsError || g.Superseded != w.Superseded {
			t.Errorf("%s differs:\n  incremental %+v\n  full        %+v", k, g, w)
		}
	}
	for k, g := range got {
		if want[k] == nil && !g.Superseded {
			t.Errorf("%s live in incremental store, absent from full parse", k)
		}
	}
}

func TestIncrementalAppendMatchesFullParse(t *testing.T) {
	path, db := buildDB(t)
	s := store{}
	c, cur := parse(t, path, transcript.Cursor{})
	s.apply(c)

	// Session 2 grows: a tool call whose state is pending, and its result.
	exec(t, db, `INSERT INTO message_nodes (session_id, node_id, parent_node_id, chat_message, created_at) VALUES
		('devin-oracle-002', 3, 2, '{"message_id":"dm-103","role":"assistant","content":"","tool_calls":[{"id":"exec:0#ccc","name":"exec","arguments":{"command":"ls migrations"}}]}', 1790158003),
		('devin-oracle-002', 4, 3, '{"message_id":"dm-104","role":"tool","tool_call_id":"exec:0#ccc","content":"001.sql 002.sql 003.sql"}', 1790158004)`)
	exec(t, db, `INSERT INTO tool_call_state VALUES ('devin-oracle-002', 'exec:0#ccc', '{"kind":"execute","rawInput":{"command":"ls migrations"}}', NULL)`)
	exec(t, db, `UPDATE sessions SET main_chain_id = 4, last_activity_at = 1790158004 WHERE id = 'devin-oracle-002'`)
	c, cur = parse(t, path, cur)
	s.apply(c)
	if n := len(c.Messages); n != 2 {
		t.Errorf("append emitted %d messages, want 2 (call, result)", n)
	}
	if st := s.get(t, "devin-oracle-002", "dm-103#call:exec:0#ccc").Enrichment["status"]; st != "pending" {
		t.Errorf("pending call status %v", st)
	}
	assertSame(t, s, snapshotStore(t, path))

	// The tool finishes with a nonzero exit: only the rows referencing it
	// are re-emitted, as new versions with is_error set.
	exec(t, db, `UPDATE tool_call_state SET tool_call_update_json = '{"status":"completed","_meta":{"terminal_exit":{"exit_code":2}}}' WHERE tool_call_id = 'exec:0#ccc'`)
	c, cur = parse(t, path, cur)
	s.apply(c)
	var got []string
	for _, m := range c.Messages {
		got = append(got, m.NativeID)
	}
	if !slices.Equal(got, []string{"dm-103#call:exec:0#ccc", "dm-104"}) {
		t.Errorf("tool update re-emitted %v", got)
	}
	if !s.get(t, "devin-oracle-002", "dm-104").IsError {
		t.Error("tool result not marked is_error after the update")
	}
	assertSame(t, s, snapshotStore(t, path))

	// Nothing else changed.
	c, _ = parse(t, path, cur)
	if len(c.Messages) != 0 {
		t.Errorf("idle parse emitted %d messages", len(c.Messages))
	}
}

func TestMainChainChangeRecomputesActivePath(t *testing.T) {
	path, db := buildDB(t)
	s := store{}
	c, cur := parse(t, path, transcript.Cursor{})
	s.apply(c)

	// Revert: the CLI moves main_chain_id back to the abandoned retry
	// (node 4) without writing rows.
	exec(t, db, `UPDATE sessions SET main_chain_id = 4 WHERE id = 'devin-oracle-001'`)
	c, _ = parse(t, path, cur)
	s.apply(c)
	var got []string
	for _, m := range c.Messages {
		got = append(got, m.NativeID+"="+onPath(m))
	}
	slices.Sort(got)
	want := []string{"dm-004=true", "dm-005=false", "dm-006=false", "dm-007=false"}
	if !slices.Equal(got, want) {
		t.Errorf("chain change emitted %v, want %v", got, want)
	}
	if len(c.Conversations) != 1 {
		t.Errorf("conversation not re-emitted after main_chain_id change")
	}
	assertSame(t, s, snapshotStore(t, path))
}

func TestDeletionsSupersede(t *testing.T) {
	path, db := buildDB(t)
	s := store{}
	c, cur := parse(t, path, transcript.Cursor{})
	s.apply(c)

	// Whole session deleted: every row superseded, none dropped.
	exec(t, db, `DELETE FROM message_nodes WHERE session_id = 'devin-oracle-002'`)
	exec(t, db, `DELETE FROM sessions WHERE id = 'devin-oracle-002'`)
	// Rows deleted inside a live session: the session is reset.
	exec(t, db, `DELETE FROM message_nodes WHERE session_id = 'devin-oracle-001' AND node_id = 4`)
	c, cur = parse(t, path, cur)
	s.apply(c)
	if !slices.Equal(c.SupersededSessions, []string{"devin-oracle-002", "devin-oracle-001"}) {
		t.Errorf("superseded %v", c.SupersededSessions)
	}
	for _, id := range []string{"dm-101", "dm-102"} {
		if m := s.get(t, "devin-oracle-002", id); !m.Superseded {
			t.Errorf("%s of the deleted session not superseded", id)
		}
	}
	if m := s.get(t, "devin-oracle-001", "dm-004"); !m.Superseded {
		t.Error("deleted node dm-004 not superseded")
	}
	if m := s.get(t, "devin-oracle-001", "dm-002"); m.Superseded {
		t.Error("surviving node dm-002 left superseded after the reset")
	}
	assertSame(t, s, snapshotStore(t, path))

	// The deletion is recorded once.
	c, _ = parse(t, path, cur)
	if len(c.SupersededSessions)+len(c.Messages) != 0 {
		t.Errorf("repeat parse: superseded %v, %d messages", c.SupersededSessions, len(c.Messages))
	}
}

func TestParseNeverWritesTheStore(t *testing.T) {
	path, db := buildDB(t)
	exec(t, db, `PRAGMA wal_checkpoint(TRUNCATE)`)
	// Leave committed frames in the WAL, as the live CLI does.
	exec(t, db, `UPDATE sessions SET title = 'renamed' WHERE id = 'devin-oracle-002'`)
	sum := func() string {
		h := sha256.New()
		for _, suffix := range []string{"", "-wal"} {
			b, err := os.ReadFile(path + suffix)
			if err != nil {
				t.Fatal(err)
			}
			h.Write(b)
		}
		return string(h.Sum(nil))
	}
	before := sum()
	c, _ := parse(t, path, transcript.Cursor{})
	if after := sum(); after != before {
		t.Fatal("parse changed sessions.db or its WAL")
	}
	for _, cv := range c.Conversations {
		if cv.SessionID == "devin-oracle-002" && cv.Title != "renamed" {
			t.Errorf("WAL frames not read: title %q", cv.Title)
		}
	}
}

func TestRequiresSupersedingSink(t *testing.T) {
	path, _ := buildDB(t)
	p := &Parser{}
	_, err := p.Parse(context.Background(), transcript.Input{Source: &transcript.Source{Path: path}}, transcript.Cursor{}, plainSink{})
	if err == nil || !strings.Contains(err.Error(), "SessionSuperseder") {
		t.Fatalf("err = %v", err)
	}
}

type plainSink struct{}

func (plainSink) Conversation(*transcript.Conversation) error { return nil }
func (plainSink) Message(*transcript.Message) error           { return nil }

func TestDefaultPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "devin", "cli"), 0o755); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "devin", "cli", "sessions.db")
	if err := os.WriteFile(db, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvDB, "")
	t.Setenv(EnvDataRoot, "")
	if got := DefaultPath("/home/u"); got != "/home/u/.local/share/devin/cli/sessions.db" {
		t.Errorf("default %s", got)
	}
	for _, root := range []string{db, filepath.Join(dir, "devin", "cli"), filepath.Join(dir, "devin")} {
		t.Setenv(EnvDataRoot, root)
		if got := DefaultPath("/home/u"); got != db {
			t.Errorf("%s=%s → %s", EnvDataRoot, root, got)
		}
	}
	t.Setenv(EnvDB, "/x/y.db")
	if got := DefaultPath("/home/u"); got != "/x/y.db" {
		t.Errorf("%s → %s", EnvDB, got)
	}
}

// P1: when the chosen copy of a message moves to a sibling with other
// blocks, the incremental result equals a full parse: one text row (its
// part does not depend on the copy), and the call only the old copy has
// stays indexed, off the active path.
func TestCopySwitchKeepsRowsStable(t *testing.T) {
	path, db := buildDB(t)
	exec(t, db, `INSERT INTO sessions (id, working_directory, backend_type, model, agent_mode, created_at, last_activity_at, title, main_chain_id, hidden)
		VALUES ('devin-p1', '/tmp/p1', 'cli', 'swe-1-7', 'normal', 1790170000, 1790170010, 'copy switch', 2, 0)`)
	exec(t, db, `INSERT INTO message_nodes (session_id, node_id, parent_node_id, chat_message, created_at) VALUES
		('devin-p1', 1, NULL, '{"message_id":"P","role":"user","content":"go","metadata":{"is_user_input":true}}', 1790170000),
		('devin-p1', 2, 1, '{"message_id":"A","role":"assistant","content":"working","tool_calls":[{"id":"c1","name":"exec","arguments":{"command":"ls"}}]}', 1790170001)`)
	s := store{}
	c, cur := parse(t, path, transcript.Cursor{})
	s.apply(c)

	// An interrupt: a sibling copy of A with thinking and text, no call,
	// becomes the chain tip.
	exec(t, db, `INSERT INTO message_nodes (session_id, node_id, parent_node_id, chat_message, created_at) VALUES
		('devin-p1', 3, 1, '{"message_id":"A","role":"assistant","content":"working","thinking":{"thinking":"hmm"}}', 1790170002)`)
	exec(t, db, `UPDATE sessions SET main_chain_id = 3 WHERE id = 'devin-p1'`)
	c, _ = parse(t, path, cur)
	s.apply(c)

	full := snapshotStore(t, path)
	assertSame(t, s, full)
	for _, id := range []string{"A", "A#thinking"} {
		if m := s.get(t, "devin-p1", id); onPath(m) != "true" {
			t.Errorf("%s on_path %s", id, onPath(m))
		}
	}
	if m := s.get(t, "devin-p1", "A#call:c1"); onPath(m) != "false" || m.Superseded {
		t.Errorf("old copy's call: on_path %s superseded %v", onPath(m), m.Superseded)
	}
	ords := map[int64]string{}
	for k, m := range full {
		if m.SessionID != "devin-p1" {
			continue
		}
		if m.Part != 0 {
			t.Errorf("%s has part %d", k, m.Part)
		}
		if o, dup := ords[m.Ordinal]; dup {
			t.Errorf("%s and %s share ordinal %d", k, o, m.Ordinal)
		}
		ords[m.Ordinal] = k
	}
	if len(ords) != 4 { // P, A, A#thinking, A#call:c1
		t.Errorf("%d live rows, want 4", len(ords))
	}
}
