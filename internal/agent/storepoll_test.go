package agent

import (
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript/devin"
)

// addDevinNode appends node n to a session's chain and moves its tip.
func addDevinNode(t *testing.T, db *sql.DB, session string, n int) {
	t.Helper()
	var parent any
	if n > 1 {
		parent = n - 1
	}
	if _, err := db.Exec(`INSERT INTO message_nodes (session_id, node_id, parent_node_id, chat_message, created_at) VALUES (?, ?, ?, ?, ?)`,
		session, n, parent, fmt.Sprintf(`{"message_id":"%s-%d","role":"assistant","content":"step %d"}`, session, n, n), 1790160000+n); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE sessions SET main_chain_id = ?, last_activity_at = ? WHERE id = ?`, n, 1790160000+n, session); err != nil {
		t.Fatal(err)
	}
}

// The device agent polls Devin's store every second while a session is
// live. An unchanged store must cost no parse; a new row must cost one
// parse that reads only that row and hands only its session to sync. The
// agent re-read a growing 30k-node session on every poll and ran at
// 120-180% CPU.
func TestDevinPollWorkIsBoundedByChange(t *testing.T) {
	const big = 200
	path, db := buildDevin(t)
	if _, err := db.Exec(`INSERT INTO sessions (id, working_directory, backend_type, model, agent_mode, created_at, last_activity_at) VALUES ('big', '/tmp/big', 'cli', 'swe-1-7', 'normal', 1790160000, 1790160000)`); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= big; n++ {
		addDevinNode(t, db, "big", n)
	}
	f := newFixture(t, path)
	f.once()
	poll := func() {
		f.a.devin.last = time.Time{} // past minStorePoll
		f.a.pollDevin(ctx, false, false)
	}

	polls := f.a.stats.DevinPolls.Load()
	for range 10 {
		poll()
	}
	if n := f.a.stats.DevinPolls.Load() - polls; n != 0 {
		t.Fatalf("10 polls of an unchanged store parsed it %d times, want 0", n)
	}

	addDevinNode(t, db, "big", big+1) // the first change may read the session once
	poll()
	for i := range 3 {
		f.rec = newRecorder()
		f.a.cfg.Sync = f.rec
		addDevinNode(t, db, "big", big+2+i)
		p, ok := f.a.devin.parser.(*devin.Parser)
		if !ok {
			t.Fatalf("store parser %T not kept across polls", f.a.devin.parser)
		}
		polls := f.a.stats.DevinPolls.Load()
		rows, _ := p.RowsRead()
		poll()
		if n := f.a.stats.DevinPolls.Load() - polls; n != 1 {
			t.Errorf("change %d: %d parses, want 1", i, n)
		}
		if p2, _ := f.a.devin.parser.(*devin.Parser); p2 != p {
			t.Fatalf("change %d: a new parser read the store", i)
		} else if now, _ := p.RowsRead(); now-rows != 1 {
			t.Errorf("change %d: one new node read %d graph rows, want 1", i, now-rows)
		}
		if got, want := slices.Sorted(maps.Keys(f.rec.exports)), []string{devin.ExportPath(path, "big")}; !slices.Equal(got, want) {
			t.Errorf("change %d: handed %v to sync, want %v", i, got, want)
		}
	}
}
