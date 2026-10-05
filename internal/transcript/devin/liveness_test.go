package devin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// livenessStore is a sessions.db with the tables the liveness reads use.
type livenessStore struct {
	t    *testing.T
	path string
	db   *sql.DB
}

func newLivenessStore(t *testing.T) *livenessStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sessions.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, last_activity_at INTEGER NOT NULL DEFAULT 0, main_chain_id INTEGER);
		CREATE TABLE message_nodes (row_id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, node_id INTEGER NOT NULL,
		  parent_node_id INTEGER, chat_message TEXT NOT NULL, created_at INTEGER NOT NULL, metadata TEXT, UNIQUE(session_id, node_id));`); err != nil {
		t.Fatal(err)
	}
	return &livenessStore{t: t, path: path, db: db}
}

// node writes a node; parent < 0 is a root.
func (s *livenessStore) node(session string, id, parent int64, msg map[string]any) {
	s.t.Helper()
	b, _ := json.Marshal(msg)
	var p any
	if parent >= 0 {
		p = parent
	}
	if _, err := s.db.Exec(`INSERT INTO message_nodes (session_id, node_id, parent_node_id, chat_message, created_at) VALUES (?,?,?,?,1791144707)`,
		session, id, p, string(b)); err != nil {
		s.t.Fatal(err)
	}
}

func meta(at time.Time, ext map[string]any) map[string]any {
	m := map[string]any{"created_at": at.UTC().Format(time.RFC3339Nano)}
	if ext != nil {
		m["extensions"] = ext
	}
	return m
}

// Devin fires no Stop for an interrupted turn; the store's marker node
// (shapes from the TUI probe, devin 3000.11.1) says the turn ended, and
// when.
func TestInterruptedSince(t *testing.T) {
	s := newLivenessStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 10, 4, 20, 10, 0, 0, time.UTC)
	got := func(session string, since time.Time) bool {
		t.Helper()
		ok, err := InterruptedSince(ctx, s.path, session, since)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	// A turn running a tool: no marker.
	s.node("tool", 1, -1, map[string]any{"role": "user", "content": "run sleep 90", "metadata": meta(t0, nil)})
	s.node("tool", 2, 1, map[string]any{"role": "assistant", "content": "", "tool_calls": []map[string]any{{"id": "get_output_0#1", "name": "get_output"}}})
	if got("tool", t0) {
		t.Fatal("a running turn reads as interrupted")
	}
	// Esc twice while the tool ran.
	s.node("tool", 3, 2, map[string]any{"role": "tool", "content": "Canceled due to user interrupt", "tool_call_id": "get_output_0#1",
		"metadata": meta(t0.Add(28*time.Second), map[string]any{"chisel/tool_failure": map[string]any{"reason": "Canceled"}})})
	if !got("tool", t0.Add(19*time.Second)) {
		t.Fatal("a tool interrupt after the last busy hook not seen")
	}
	// A prompt after the interrupt: that turn was not interrupted.
	if got("tool", t0.Add(40*time.Second)) {
		t.Fatal("an interrupt before the turn's start counted")
	}
	// Esc while the model wrote its answer.
	s.node("text", 1, -1, map[string]any{"role": "user", "content": "write an essay"})
	s.node("text", 2, 1, map[string]any{"role": "assistant", "content": "# Bridges\n\nFew structures..."})
	s.node("text", 3, 2, map[string]any{"role": "system", "content": "[Response interrupted by user]", "metadata": meta(t0.Add(5*time.Second), nil)})
	if !got("text", t0) {
		t.Fatal("a response interrupt not seen")
	}
	// Text that only mentions an interrupt, and a marker without its time.
	s.node("quote", 1, -1, map[string]any{"role": "user", "content": "[Response interrupted by user] is what Devin writes", "metadata": meta(t0.Add(time.Second), nil)})
	s.node("quote", 2, 1, map[string]any{"role": "tool", "content": "Canceled due to user interrupt"})
	if got("quote", t0) {
		t.Fatal("a user's text or an undated marker counted")
	}
	// Only the newest nodes are read.
	for i := int64(4); i < 4+InterruptRecentNodes; i++ {
		s.node("tool", i, i-1, map[string]any{"role": "assistant", "content": "later"})
	}
	if got("tool", t0) {
		t.Fatal("an interrupt older than the newest nodes counted")
	}
	if got("missing", t0) {
		t.Fatal("an unknown session reads as interrupted")
	}
}

func TestSessions(t *testing.T) {
	s := newLivenessStore(t)
	if _, err := s.db.Exec(`INSERT INTO sessions (id) VALUES ('running'), ('other')`); err != nil {
		t.Fatal(err)
	}
	got, err := Sessions(context.Background(), s.path, []string{"running", "deleted"})
	if err != nil || !got["running"] || got["deleted"] || got["other"] {
		t.Fatalf("Sessions: %v %v", got, err)
	}
	if _, err := Sessions(context.Background(), filepath.Join(t.TempDir(), "none.db"), []string{"x"}); err == nil {
		t.Fatal("a missing store read without error")
	}
}

// A run_subagent call whose process died has no result. `devin -r` goes on
// from the call's parent: the next prompt is the call's sibling (probe,
// devin 3000.11.1). The call is abandoned, so the session's Stop is its
// own. Devin's second copy of an assistant node (a sibling with the same
// message_id) is no fork.
func TestSubagentRunningIgnoresAbandonedCall(t *testing.T) {
	s := newLivenessStore(t)
	ctx := context.Background()
	running := func() bool {
		t.Helper()
		ok, err := SubagentRunning(ctx, s.path, "s")
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	call := map[string]any{"message_id": "m-call", "role": "assistant", "content": "",
		"tool_calls": []map[string]any{{"id": "run_subagent_0_7b22#52ba", "name": "run_subagent"}}}
	s.node("s", 28, -1, map[string]any{"message_id": "m-sys", "role": "system", "content": "instructions"})
	s.node("s", 29, 28, map[string]any{"message_id": "m-u1", "role": "user", "content": "start a subagent"})
	s.node("s", 30, 29, call)
	s.node("s", 31, 29, call) // Devin's second copy
	if !running() {
		t.Fatal("an unanswered run_subagent call is not running")
	}
	// The process was killed; the resumed session's next prompt.
	s.node("s", 32, 29, map[string]any{"message_id": "m-u2", "role": "user", "content": "reply gamma"})
	if running() {
		t.Fatal("an abandoned run_subagent call still counts as running")
	}
	// A new run_subagent in the resumed turn runs.
	s.node("s", 33, 32, map[string]any{"message_id": "m-call2", "role": "assistant", "content": "",
		"tool_calls": []map[string]any{{"id": "run_subagent_0_9c21#1111", "name": "run_subagent"}}})
	if !running() {
		t.Fatal("the resumed turn's run_subagent call is not running")
	}
}

// A background subagent (run_subagent in the background, or one moved
// there by Esc) writes its nodes into its session as it runs, on a root of
// its own, and its hooks carry the session's id (probes, devin 3000.11.1,
// 2026-10-04). Its tool call is in the store, but not on the session's
// main chain: sessions.main_chain_id names a node of that chain, which may
// lag behind the newest node or already be past the call (#107, #109).
func TestOwnToolCallIsOnTheMainChain(t *testing.T) {
	s := newLivenessStore(t)
	ctx := context.Background()
	own := func(id string) bool {
		t.Helper()
		ok, err := OwnToolCall(ctx, s.path, "s", id)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	setMain := func(n any) {
		t.Helper()
		if _, err := s.db.Exec(`UPDATE sessions SET main_chain_id = ? WHERE id = 's'`, n); err != nil {
			t.Fatal(err)
		}
	}
	call := func(mid, id, name string) map[string]any {
		return map[string]any{"message_id": mid, "role": "assistant", "content": "",
			"tool_calls": []map[string]any{{"id": id, "name": name}}}
	}
	if _, err := s.db.Exec(`INSERT INTO sessions (id) VALUES ('s')`); err != nil {
		t.Fatal(err)
	}
	s.node("s", 15, -1, map[string]any{"message_id": "m-sys", "role": "system", "content": "You are Devin"})
	s.node("s", 21, 15, map[string]any{"message_id": "m-u", "role": "user", "content": "start a background subagent"})
	s.node("s", 22, 21, call("m-run", "run_subagent_0#1", "run_subagent"))
	s.node("s", 28, -1, map[string]any{"message_id": "m-sub", "role": "system", "content": "You are a subagent"})
	s.node("s", 29, 28, map[string]any{"message_id": "m-task", "role": "user", "content": "sleep 4"})
	s.node("s", 30, 21, call("m-run", "run_subagent_0#1", "run_subagent")) // Devin's second copy
	s.node("s", 31, 30, map[string]any{"message_id": "m-r", "role": "tool", "content": "Background subagent started", "tool_call_id": "run_subagent_0#1"})
	setMain(31)
	s.node("s", 32, 29, call("m-s1", "exec_sub#1", "exec"))  // the subagent's
	s.node("s", 33, 31, call("m-p1", "exec_main#1", "exec")) // the session's, past main_chain_id
	if !own("exec_main#1") {
		t.Fatal("the session's tool call, written after main_chain_id moved, is not its own")
	}
	if !own("run_subagent_0#1") {
		t.Fatal("the run_subagent call, behind main_chain_id, is not its own")
	}
	if own("exec_sub#1") {
		t.Fatal("a background subagent's tool call counts as the session's own")
	}
	// main_chain_id moves past the call (its result): still its own.
	s.node("s", 34, 31, call("m-p1", "exec_main#1", "exec"))
	s.node("s", 35, 34, map[string]any{"message_id": "m-r2", "role": "tool", "content": "ok", "tool_call_id": "exec_main#1"})
	setMain(35)
	if !own("exec_main#1") || own("exec_sub#1") {
		t.Fatal("main_chain_id past the call")
	}
	// Twenty parallel calls: their results chain below the call (devin
	// 3000.11.1), and main_chain_id moves to the last one.
	calls := make([]map[string]any, 20)
	for i := range calls {
		calls[i] = map[string]any{"id": fmt.Sprintf("par#%d", i), "name": "read"}
	}
	par := map[string]any{"message_id": "m-par", "role": "assistant", "content": "", "tool_calls": calls}
	s.node("s", 40, 35, par)
	s.node("s", 41, 35, par)
	parent := int64(41)
	for i := range calls {
		s.node("s", 42+int64(i), parent, map[string]any{"message_id": fmt.Sprintf("m-pr%d", i), "role": "tool", "content": "ok", "tool_call_id": fmt.Sprintf("par#%d", i)})
		parent = 42 + int64(i)
	}
	setMain(parent)
	if !own("par#0") || own("exec_sub#1") {
		t.Fatal("main_chain_id past twenty parallel results")
	}
	setMain(35) // before the call: its PreToolUse
	if !own("par#19") {
		t.Fatal("main_chain_id before the parallel call")
	}
	// No main chain recorded (a store that does not set it): any recent
	// call counts, as before.
	setMain(nil)
	if !own("exec_sub#1") {
		t.Fatal("without main_chain_id a recent call is not taken as the session's")
	}
}

// A long session: 200 nodes on the main chain, and a background subagent
// with 200 nodes of its own. The walk is bounded at HookRecentNodes; the
// session's newest call is a few nodes from main_chain_id however deep the
// chain is, and a long subagent's call still never reaches it.
func TestOwnToolCallDeepChains(t *testing.T) {
	s := newLivenessStore(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO sessions (id) VALUES ('s')`); err != nil {
		t.Fatal(err)
	}
	call := func(mid, id string) map[string]any {
		return map[string]any{"message_id": mid, "role": "assistant", "content": "",
			"tool_calls": []map[string]any{{"id": id, "name": "exec"}}}
	}
	s.node("s", 1, -1, map[string]any{"message_id": "m-sys", "role": "system", "content": "You are Devin"})
	s.node("s", 10000, -1, map[string]any{"message_id": "m-sub", "role": "system", "content": "You are a subagent"})
	mainTip, subTip := int64(1), int64(10000)
	for i := int64(1); i <= 200; i++ { // interleaved: both chains grow
		s.node("s", 1+i, mainTip, map[string]any{"message_id": fmt.Sprintf("m%d", i), "role": "user", "content": "x"})
		mainTip = 1 + i
		s.node("s", 10000+i, subTip, map[string]any{"message_id": fmt.Sprintf("s%d", i), "role": "user", "content": "x"})
		subTip = 10000 + i
	}
	if _, err := s.db.Exec(`UPDATE sessions SET main_chain_id = ? WHERE id = 's'`, mainTip); err != nil {
		t.Fatal(err)
	}
	s.node("s", 500, mainTip, call("m-own", "own#1"))
	s.node("s", 10500, subTip, call("m-sc", "sub#1"))
	for _, c := range []struct {
		id   string
		want bool
	}{{"own#1", true}, {"sub#1", false}} {
		got, err := OwnToolCall(ctx, s.path, "s", c.id)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Fatalf("%s: own = %v, want %v", c.id, got, c.want)
		}
	}
	// main_chain_id at the call's result, the walk from main goes 1 step.
	s.node("s", 501, 500, map[string]any{"message_id": "m-r", "role": "tool", "content": "ok", "tool_call_id": "own#1"})
	if _, err := s.db.Exec(`UPDATE sessions SET main_chain_id = 501 WHERE id = 's'`); err != nil {
		t.Fatal(err)
	}
	if ok, err := OwnToolCall(ctx, s.path, "s", "own#1"); err != nil || !ok {
		t.Fatalf("own call behind main_chain_id on a 200-node chain: %v %v", ok, err)
	}
}
