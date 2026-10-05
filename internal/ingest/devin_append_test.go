package ingest

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/transcript/devin"
)

// devinExportFunc is the device agent's exporter for one session: appends
// to the export the saved state describes (devin-export@2).
func devinExportFunc(db, session string) devicesync.ExportFunc {
	return func(ctx context.Context, prev []byte) (devicesync.Export, error) {
		data, appended, state, err := devin.ExportFrom(ctx, db, session, prev)
		return devicesync.Export{Data: data, Append: appended, State: state}, err
	}
}

func devinOnly(m map[string][]string) map[string][]string {
	out := map[string][]string{}
	for k, v := range m {
		if strings.HasPrefix(k, "devin|") {
			out[k] = v
		}
	}
	return out
}

// A Devin session synced by appended exports, one change at a time, ends
// with exactly the rows a full parse of the store gives, and every append
// extends the source's generation; a deleted node starts a new one.
func TestDevinAppendedExportIngest(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	f := newFixture(t)
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	sync := func() {
		t.Helper()
		for _, id := range f.devinSessions {
			if err := sy.SyncExportFunc(ctx, devinSpec(f.devinDB, id), devinExportFunc(f.devinDB, id)); err != nil {
				t.Fatalf("sync %s: %v", id, err)
			}
		}
		e.drain()
	}
	const id = "devin-oracle-001"
	generation := func() int {
		return e.count(`SELECT max(g.generation) FROM generations g JOIN sources s ON s.id=g.source_id WHERE s.path=$1`, devin.ExportPath(f.devinDB, id))
	}
	sync()
	sameRows(t, devinOnly(e.live()), devinOnly(f.want(t)))
	gen := generation()

	db, err := sql.Open("sqlite", "file:"+f.devinDB)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, step := range []struct {
		name   string
		sql    []string
		newGen bool
	}{
		{"node with a pending tool call", []string{
			`INSERT INTO message_nodes (session_id, node_id, parent_node_id, chat_message, created_at) VALUES ('devin-oracle-001', 8, 7, '{"message_id":"dm-008","role":"assistant","content":"Running the tests.","tool_calls":[{"id":"call_t8","type":"function","function":{"name":"exec","arguments":"{\"command\":\"go test ./...\"}"}}]}', 1790157607)`,
			`INSERT INTO tool_call_state VALUES ('devin-oracle-001', 'call_t8', '{"toolCallId":"call_t8","kind":"execute","rawInput":{"command":"go test ./..."}}', NULL)`,
			`UPDATE sessions SET main_chain_id=8, last_activity_at=1790157707 WHERE id='devin-oracle-001'`}, false},
		{"tool settles, result node", []string{
			`UPDATE tool_call_state SET tool_call_update_json='{"status":"failed","_meta":{"terminal_exit":{"exit_code":1}}}' WHERE tool_call_id='call_t8'`,
			`INSERT INTO message_nodes (session_id, node_id, parent_node_id, chat_message, created_at) VALUES ('devin-oracle-001', 9, 8, '{"message_id":"dm-009","role":"tool","tool_call_id":"call_t8","content":"FAIL auth_test.go"}', 1790157608)`,
			`UPDATE sessions SET main_chain_id=9, last_activity_at=1790157708 WHERE id='devin-oracle-001'`}, false},
		{"renamed", []string{`UPDATE sessions SET title='Fix login bug and test it' WHERE id='devin-oracle-001'`}, false},
		{"main chain back to the abandoned branch", []string{`UPDATE sessions SET main_chain_id=4 WHERE id='devin-oracle-001'`}, false},
		{"node deleted", []string{`DELETE FROM message_nodes WHERE session_id='devin-oracle-001' AND node_id=9`,
			`UPDATE sessions SET main_chain_id=8 WHERE id='devin-oracle-001'`}, true},
		{"node after the new generation", []string{
			`INSERT INTO message_nodes (session_id, node_id, parent_node_id, chat_message, created_at) VALUES ('devin-oracle-001', 10, 8, '{"message_id":"dm-010","role":"assistant","content":"Tests pass now."}', 1790157610)`,
			`UPDATE sessions SET main_chain_id=10 WHERE id='devin-oracle-001'`}, false},
	} {
		for _, q := range step.sql {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("%s: %s: %v", step.name, q, err)
			}
		}
		sync()
		sameRows(t, devinOnly(e.live()), devinOnly(f.want(t)))
		g := generation()
		if step.newGen != (g != gen) {
			t.Errorf("%s: generation %d -> %d", step.name, gen, g)
		}
		gen = g
		if t.Failed() {
			t.Fatalf("after %s", step.name)
		}
	}
}
