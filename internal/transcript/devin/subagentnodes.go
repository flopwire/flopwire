package devin

import (
	"context"
	"database/sql"
	"fmt"
)

// SubagentNodesContain reports whether a node outside the session's own
// thread holds needle. A run_subagent subagent writes its nodes on roots of
// their own (parent_node_id NULL) when it finishes (hookthread.go); the
// session's own thread is the tree of its newest node. `flopwire probe`
// uses it to check that a message never entered a subagent.
func SubagentNodesContain(ctx context.Context, dbPath, session, needle string) (bool, error) {
	db, err := openForHook(dbPath)
	if err != nil {
		return false, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT node_id, parent_node_id, instr(chat_message, ?) > 0 FROM message_nodes WHERE session_id = ? ORDER BY row_id`, needle, session)
	if err != nil {
		return false, fmt.Errorf("devin: subagent nodes: %w", err)
	}
	defer rows.Close()
	parent := map[int64]int64{}
	var holds []int64
	last, any := int64(0), false
	for rows.Next() {
		var id int64
		var p sql.NullInt64
		var has bool
		if err := rows.Scan(&id, &p, &has); err != nil {
			return false, fmt.Errorf("devin: subagent nodes: %w", err)
		}
		if p.Valid {
			parent[id] = p.Int64
		}
		if has {
			holds = append(holds, id)
		}
		last, any = id, true
	}
	if err := rows.Err(); err != nil || !any {
		return false, err
	}
	root := func(n int64) int64 {
		for range len(parent) + 1 {
			p, ok := parent[n]
			if !ok {
				return n
			}
			n = p
		}
		return n // a cycle: the store is damaged; n is as good a root as any
	}
	own := root(last)
	for _, n := range holds {
		if root(n) != own {
			return true, nil
		}
	}
	return false, nil
}
