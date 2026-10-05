package devin

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

// growSession adds a session of n assistant nodes in one chain.
func growSession(t *testing.T, db *sql.DB, id string, n int) {
	t.Helper()
	exec(t, db, `INSERT INTO sessions (id, working_directory, backend_type, model, agent_mode, created_at, last_activity_at, main_chain_id) VALUES (?, '/tmp/big', 'cli', 'swe-1-7', 'normal', 1790160000, 1790160000, ?)`, id, n)
	for i := 1; i <= n; i++ {
		var parent any
		if i > 1 {
			parent = i - 1
		}
		exec(t, db, `INSERT INTO message_nodes (session_id, node_id, parent_node_id, chat_message, created_at) VALUES (?, ?, ?, ?, ?)`,
			id, i, parent, fmt.Sprintf(`{"message_id":"big-%d","role":"assistant","content":"step %d"}`, i, i), 1790160000+i)
	}
}

// addNode appends node n to a session's chain and moves its tip.
func addNode(t *testing.T, db *sql.DB, id string, n int, msg string) {
	t.Helper()
	exec(t, db, `INSERT INTO message_nodes (session_id, node_id, parent_node_id, chat_message, created_at) VALUES (?, ?, ?, ?, ?)`, id, n, n-1, msg, 1790160000+n)
	exec(t, db, `UPDATE sessions SET main_chain_id = ?, last_activity_at = ? WHERE id = ?`, n, 1790160000+n, id)
}

func parseWith(t *testing.T, p *Parser, path string, cur transcript.Cursor) (*transcript.Collector, transcript.Cursor) {
	t.Helper()
	c := &transcript.Collector{}
	src := &transcript.Source{Agent: transcript.AgentDevin, Path: path, StorageKind: transcript.StorageSQLite, Parser: p.Name()}
	next, err := p.Parse(context.Background(), transcript.Input{Source: src}, cur, c)
	if err != nil {
		t.Fatal(err)
	}
	return c, next
}

func graphRows(p *Parser) int64 { g, _ := p.RowsRead(); return g }

func sessionsOf(c *transcript.Collector) []string {
	var out []string
	for _, m := range c.Messages {
		if !slices.Contains(out, m.SessionID) {
			out = append(out, m.SessionID)
		}
	}
	slices.Sort(out)
	return out
}

// A live session gains a node every second or so. A parser kept across
// polls must read only the new rows of that session, not the whole
// session again: the agent re-parsed a 30k-node, 255MB session on every
// poll and ran at 120-180% CPU.
func TestLiveSessionParseReadsOnlyNewRows(t *testing.T) {
	const big = 300
	path, db := buildDB(t)
	growSession(t, db, "big", big)
	p := &Parser{}
	s := store{}
	c0, cur := parseWith(t, p, path, transcript.Cursor{})
	s.apply(c0)

	// Unchanged store: no session graph is read, nothing is emitted.
	before := graphRows(p)
	for range 5 {
		var c *transcript.Collector
		c, cur = parseWith(t, p, path, cur)
		s.apply(c)
		if len(c.Messages)+len(c.Conversations) != 0 {
			t.Fatalf("unchanged store emitted %d messages, %d conversations", len(c.Messages), len(c.Conversations))
		}
	}
	if n := graphRows(p) - before; n != 0 {
		t.Fatalf("unchanged store read %d graph rows, want 0", n)
	}

	// The first change after the initial parse may read the session once.
	addNode(t, db, "big", big+1, `{"message_id":"big-301","role":"assistant","content":"","tool_calls":[{"id":"call_x","name":"exec","arguments":{"command":"ls"}}]}`)
	exec(t, db, `INSERT INTO tool_call_state VALUES ('big', 'call_x', '{"kind":"execute","rawInput":{"command":"ls"}}', NULL)`)
	c1, cur := parseWith(t, p, path, cur)
	s.apply(c1)

	// From then on each change reads only its new rows.
	for i, step := range []struct {
		msg, tool string
		want      []string
	}{
		{`{"message_id":"big-302","role":"tool","tool_call_id":"call_x","content":"a b c"}`,
			`UPDATE tool_call_state SET tool_call_update_json = '{"status":"failed"}' WHERE tool_call_id = 'call_x'`,
			[]string{"big-301#call:call_x", "big-302"}},
		{`{"message_id":"big-303","role":"assistant","content":"done"}`, "", []string{"big-303"}},
	} {
		addNode(t, db, "big", big+2+i, step.msg)
		if step.tool != "" {
			exec(t, db, step.tool)
		}
		before := graphRows(p)
		c, next := parseWith(t, p, path, cur)
		cur = next
		s.apply(c)
		if n := graphRows(p) - before; n != 1 {
			t.Errorf("step %d: one new node read %d graph rows, want 1", i, n)
		}
		if got := sessionsOf(c); !slices.Equal(got, []string{"big"}) {
			t.Errorf("step %d: emitted sessions %v, want [big]", i, got)
		}
		var got []string
		for _, m := range c.Messages {
			got = append(got, m.NativeID)
		}
		slices.Sort(got)
		if !slices.Equal(got, step.want) {
			t.Errorf("step %d: emitted %v, want %v", i, got, step.want)
		}
	}
	assertSame(t, s, snapshotStore(t, path))

	// A deleted row invalidates the kept graph: the session is read again
	// and matches a fresh parse.
	exec(t, db, `DELETE FROM message_nodes WHERE session_id = 'big' AND node_id = 150`)
	addNode(t, db, "big", big+4, `{"message_id":"big-304","role":"assistant","content":"after delete"}`)
	before = graphRows(p)
	c, _ := parseWith(t, p, path, cur)
	s.apply(c)
	if n := graphRows(p) - before; n < big {
		t.Errorf("deletion read %d graph rows, want the whole session", n)
	}
	assertSame(t, s, snapshotStore(t, path))
}

// Devin moves a long session's main chain to another branch (compaction
// starts a new tree; the live chain of a 30k-node session was 390 nodes).
// The keys that flip on or off the path are re-emitted; their rows are
// read by row id, not by streaming the whole session.
func TestChainSwitchReadsOnlyFlippedRows(t *testing.T) {
	const big, fork = 1600, 1300
	path, db := buildDB(t)
	growSession(t, db, "big", big)
	p := &Parser{}
	s := store{}
	c, cur := parseWith(t, p, path, transcript.Cursor{})
	s.apply(c)

	// A branch off node fork: the nodes after it leave the path.
	exec(t, db, `INSERT INTO message_nodes (session_id, node_id, parent_node_id, chat_message, created_at) VALUES ('big', ?, ?, '{"message_id":"big-branch","role":"assistant","content":"other branch"}', 1790170000)`, big+1, fork)
	exec(t, db, `UPDATE sessions SET main_chain_id = ? WHERE id = 'big'`, big+1)
	_, before := p.RowsRead()
	c, _ = parseWith(t, p, path, cur)
	s.apply(c)
	_, after := p.RowsRead()
	if len(c.Messages) != big-fork+1 {
		t.Errorf("chain switch emitted %d messages, want %d", len(c.Messages), big-fork+1)
	}
	if n := after - before; n != big-fork+1 {
		t.Errorf("chain switch read %d rows with content, want the %d flipped and new ones", n, big-fork+1)
	}
	assertSame(t, s, snapshotStore(t, path))
}
