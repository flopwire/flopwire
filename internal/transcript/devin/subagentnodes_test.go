package devin

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// The store as Devin 3000.11.1 writes a session that ran one run_subagent:
// the subagent's nodes hang off roots of their own (parent NULL), written
// when it finished, before the session's next nodes.
func TestSubagentNodesContain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE message_nodes (row_id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, node_id INTEGER NOT NULL,
		parent_node_id INTEGER, chat_message TEXT NOT NULL, created_at INTEGER NOT NULL, metadata TEXT, UNIQUE(session_id, node_id))`); err != nil {
		t.Fatal(err)
	}
	add := func(session string, id int64, parent any, text string) {
		if _, err := db.Exec(`INSERT INTO message_nodes (session_id, node_id, parent_node_id, chat_message, created_at) VALUES (?,?,?,?,0)`,
			session, id, parent, `{"role":"x","content":"`+text+`"}`); err != nil {
			t.Fatal(err)
		}
	}
	add("s", 1, nil, "user prompt")
	add("s", 2, 1, "run_subagent call")
	add("s", 3, nil, "subagent task")       // the subagent's own root
	add("s", 4, 3, "subagent saw SUB-MARK") // inside the subagent
	add("s", 5, 2, "Subagent result")
	add("s", 6, 5, "<flopwire-message> MAIN-MARK")
	add("s", 7, 6, "assistant quotes MAIN-MARK")
	add("other", 1, nil, "OTHER-MARK in another session's subagent")
	add("other", 2, nil, "other's main thread")
	ctx := context.Background()
	for needle, want := range map[string]bool{"SUB-MARK": true, "MAIN-MARK": false, "OTHER-MARK": false, "NOWHERE": false} {
		got, err := SubagentNodesContain(ctx, path, "s", needle)
		if err != nil || got != want {
			t.Errorf("%s: got %v %v, want %v", needle, got, err, want)
		}
	}
	if got, err := SubagentNodesContain(ctx, path, "missing", "SUB-MARK"); err != nil || got {
		t.Errorf("unknown session: %v %v", got, err)
	}
}
