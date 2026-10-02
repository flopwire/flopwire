package synthcorpus

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	_ "modernc.org/sqlite" // pure Go SQLite driver
)

// devinSession is one planned session of the Devin store.
type devinSession struct {
	id     string
	cwd    string
	start  int64 // epoch seconds
	bytes  int64 // target chat_message bytes
	plants []string
}

func planDevin(r rng, total int64) []*devinSession {
	var out []*devinSession
	median, most := float64(min(64<<10, total/6)), total/3
	for total > 0 {
		s := min(total, most, max(8<<10, int64(r.lognormal(median, 1.2))))
		out = append(out, &devinSession{
			id:    r.uuid4(),
			cwd:   cwdOf(repos[r.IntN(len(repos))]),
			start: windowStartUnix + r.Int64N(windowDays*86400-12*3600),
			bytes: s,
		})
		total -= s
	}
	return out
}

// devinSchema is the store's schema as of 2026-09
// (testdata/oracle/seeds/devin.sql).
const devinSchema = `
CREATE TABLE sessions (
  id TEXT PRIMARY KEY, working_directory TEXT NOT NULL, backend_type TEXT NOT NULL, model TEXT NOT NULL,
  agent_mode TEXT NOT NULL, created_at INTEGER NOT NULL, last_activity_at INTEGER NOT NULL, title TEXT,
  main_chain_id INTEGER, shell_last_seen_index INTEGER DEFAULT 0, cogs_json TEXT, workspace_dirs TEXT,
  hidden INTEGER NOT NULL DEFAULT 0, metadata TEXT);
CREATE TABLE message_nodes (
  row_id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, node_id INTEGER NOT NULL,
  parent_node_id INTEGER, chat_message TEXT NOT NULL, created_at INTEGER NOT NULL, metadata TEXT,
  FOREIGN KEY (session_id) REFERENCES sessions(id), UNIQUE(session_id, node_id));
CREATE TABLE tool_call_state (
  session_id TEXT NOT NULL, tool_call_id TEXT NOT NULL, tool_call_json TEXT, tool_call_update_json TEXT,
  PRIMARY KEY (session_id, tool_call_id), FOREIGN KEY (session_id) REFERENCES sessions(id));
CREATE TABLE subagent_heads (
  session_id TEXT NOT NULL, agent_id TEXT NOT NULL, chain_node_id INTEGER NOT NULL, updated_at INTEGER NOT NULL,
  PRIMARY KEY (session_id, agent_id), FOREIGN KEY (session_id) REFERENCES sessions(id));
`

// writeDevin builds the store: per session a linear chain of system,
// user, assistant (with a tool call), tool and assistant nodes.
func writeDevin(seed uint64, path string, sessions []*devinSession) (fileResult, error) {
	res := fileResult{planted: map[string]int{}}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return res, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(DELETE)")
	if err != nil {
		return res, err
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, devinSchema); err != nil {
		return res, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	insNode, err := tx.PrepareContext(ctx, `INSERT INTO message_nodes (session_id, node_id, parent_node_id, chat_message, created_at) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return res, err
	}
	insTool, err := tx.PrepareContext(ctx, `INSERT INTO tool_call_state (session_id, tool_call_id, tool_call_json, tool_call_update_json) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return res, err
	}
	for si, s := range sessions {
		r := newRNG(seed, 1<<40+uint64(si))
		node, at := int64(0), s.start
		var written int64
		msg := func(role string, content []byte, extra string) error {
			node++
			at += 1 + r.Int64N(20)
			var b []byte
			b = append(b, `{"message_id":"dm-`...)
			b = append(b, hex(r.Uint64(), 12)...)
			b = append(b, `","role":"`...)
			b = append(b, role...)
			b = append(b, `","content":"`...)
			b = append(b, content...)
			b = append(b, '"')
			b = append(b, extra...)
			b = append(b, '}')
			written += int64(len(b))
			var parent any
			if node > 1 {
				parent = node - 1
			}
			_, err := insNode.ExecContext(ctx, s.id, node, parent, string(b), at)
			return err
		}
		if err := msg("system", []byte("You are Devin."), `,"metadata":{"is_user_input":null}`); err != nil {
			return res, err
		}
		plants := s.plants
		for turn := 0; written < s.bytes || len(plants) > 0; turn++ {
			if err := msg("user", r.prose(nil, 40+r.IntN(300)), `,"metadata":{"is_user_input":true}`); err != nil {
				return res, err
			}
			call := "exec:" + strconv.Itoa(turn) + "#" + hex(r.Uint64(), 6)
			cmd := r.prose(nil, 10+r.IntN(30))
			if err := msg("assistant", r.prose(nil, 20+r.IntN(200)),
				`,"tool_calls":[{"id":"`+call+`","index":0,"kind":"function","name":"exec","arguments":{"command":"`+string(cmd)+`"}}],"metadata":{}`); err != nil {
				return res, err
			}
			if err := msg("tool", r.output(nil, int(min(64<<10, r.lognormal(800, 1.3))), false), `,"tool_call_id":"`+call+`","metadata":{}`); err != nil {
				return res, err
			}
			if _, err := insTool.ExecContext(ctx, s.id, call,
				`{"toolCallId":"`+call+`","title":"Ran command","kind":"execute","rawInput":{"command":"`+string(cmd)+`"}}`,
				`{"toolCallId":"`+call+`","status":"completed"}`); err != nil {
				return res, err
			}
			var text []byte
			if len(plants) > 0 {
				text = append(text, plants[0]...)
				text = append(text, ' ')
				res.planted[plants[0]]++
				plants = plants[1:]
			}
			if err := msg("assistant", r.prose(text, 40+r.IntN(400)), `,"metadata":{}`); err != nil {
				return res, err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (id, working_directory, backend_type, model, agent_mode, created_at, last_activity_at, title, main_chain_id, hidden) VALUES (?, ?, 'cli', 'swe-1-7', 'normal', ?, ?, ?, ?, 0)`,
			s.id, s.cwd, s.start, at, fmt.Sprintf("synthetic session %d", si), node); err != nil {
			return res, err
		}
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	if err := db.Close(); err != nil {
		return res, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return res, err
	}
	res.bytes = fi.Size()
	return res, nil
}
