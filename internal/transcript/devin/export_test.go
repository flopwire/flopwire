package devin

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

// parseExport loads an export into a throwaway store and parses it with cur.
func parseExport(t *testing.T, data []byte, session string, cur transcript.Cursor) (*transcript.Collector, transcript.Cursor) {
	t.Helper()
	path, err := LoadExport(context.Background(), bytes.NewReader(data), session, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return parse(t, path, cur)
}

// Every session's export parses to exactly the rows the store itself gives.
func TestExportRoundTripMatchesStore(t *testing.T) {
	ctx := context.Background()
	path, _ := buildDB(t)
	direct, _ := parse(t, path, transcript.Cursor{})
	ids, err := ListSessions(ctx, path)
	if err != nil || len(ids) < 2 {
		t.Fatalf("sessions %v %v", ids, err)
	}
	var total int
	for _, id := range ids {
		data, err := Export(ctx, path, id)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := parseExport(t, data, id, transcript.Cursor{})
		want := direct.MessagesFor(id)
		if len(got.Messages) != len(want) {
			t.Fatalf("%s: %d rows from export, %d from store", id, len(got.Messages), len(want))
		}
		for i := range want {
			if !reflect.DeepEqual(got.Messages[i], want[i]) {
				t.Fatalf("%s row %d:\nexport %+v\nstore  %+v", id, i, got.Messages[i], want[i])
			}
		}
		total += len(want)
	}
	if total != len(direct.Messages) {
		t.Fatalf("exports cover %d of %d rows", total, len(direct.Messages))
	}
	if SessionOfExport(ExportPath(path, "s1")) != "s1" {
		t.Fatal("export path round trip")
	}
}

// A cursor carries over from one export to the next; the export of a
// vanished session supersedes it.
func TestExportCursorCarriesAcrossGenerations(t *testing.T) {
	ctx := context.Background()
	path, db := buildDB(t)
	const id = "devin-oracle-002"
	first, _ := Export(ctx, path, id)
	c1, cur := parseExport(t, first, id, transcript.Cursor{})
	exec(t, db, `INSERT INTO message_nodes (session_id, node_id, parent_node_id, chat_message, created_at) VALUES
		('devin-oracle-002', 3, 2, '{"message_id":"dm-103","role":"user","content":"and the seeds","metadata":{"is_user_input":true}}', 1790158003)`)
	exec(t, db, `UPDATE sessions SET main_chain_id = 3 WHERE id = 'devin-oracle-002'`)
	second, _ := Export(ctx, path, id)
	c2, cur := parseExport(t, second, id, cur)
	if len(c1.Messages) != 2 || len(c2.Messages) != 1 || c2.Messages[0].NativeID != "dm-103" {
		t.Fatalf("first %d rows, second %v", len(c1.Messages), c2.Messages)
	}
	exec(t, db, `DELETE FROM message_nodes WHERE session_id = 'devin-oracle-002'`)
	exec(t, db, `DELETE FROM sessions WHERE id = 'devin-oracle-002'`)
	gone, _ := Export(ctx, path, id)
	if len(gone) == 0 {
		t.Fatal("a vanished session must export a record")
	}
	c3, _ := parseExport(t, gone, id, cur)
	if len(c3.SupersededSessions) != 1 || c3.SupersededSessions[0] != id {
		t.Fatalf("empty export: %+v", c3)
	}
}

// An export kept up to date by appends loads to the same store, and
// parses to the same rows, as a whole export of the session taken at the
// end; each append carries only what changed.
func TestExportAppendsMatchWholeExport(t *testing.T) {
	ctx := context.Background()
	path, db := buildDB(t)
	const id = "big"
	growSession(t, db, id, 200)
	log, appended, state, err := ExportFrom(ctx, path, id, nil)
	if err != nil || appended {
		t.Fatalf("first export: appended %v, %v", appended, err)
	}
	step := func(name string, wantAppend bool, maxBytes int) {
		t.Helper()
		data, appended, next, err := ExportFrom(ctx, path, id, state)
		if err != nil {
			t.Fatal(err)
		}
		if appended != wantAppend {
			t.Fatalf("%s: appended %v, want %v", name, appended, wantAppend)
		}
		if len(data) > maxBytes {
			t.Fatalf("%s: %d bytes, want at most %d:\n%s", name, len(data), maxBytes, data)
		}
		if appended {
			log = append(log, data...)
		} else {
			log = data
		}
		state = next
		whole, err := Export(ctx, path, id)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := parseExport(t, log, id, transcript.Cursor{})
		want, _ := parseExport(t, whole, id, transcript.Cursor{})
		direct := snapshotStore(t, path)
		gs, ws := store{}, store{}
		gs.apply(got)
		ws.apply(want)
		assertSame(t, gs, ws)
		for k, m := range direct {
			if m.SessionID == id && gs[k] == nil {
				t.Errorf("%s: %s missing from the appended export", name, k)
			}
		}
		if t.Failed() {
			t.Fatalf("after %s", name)
		}
	}

	step("no change", true, 0)
	addNode(t, db, id, 201, `{"message_id":"big-201","role":"assistant","content":"","tool_calls":[{"id":"call_a","name":"exec","arguments":{"command":"ls"}}]}`)
	exec(t, db, `INSERT INTO tool_call_state VALUES (?, 'call_a', '{"kind":"execute","rawInput":{"command":"ls"}}', NULL)`, id)
	step("node with a pending tool call", true, 1024)
	step("pending, unchanged", true, 0)
	addNode(t, db, id, 202, `{"message_id":"big-202","role":"tool","tool_call_id":"call_a","content":"a b"}`)
	exec(t, db, `UPDATE tool_call_state SET tool_call_update_json = '{"status":"failed"}' WHERE tool_call_id = 'call_a'`)
	step("tool settled in place", true, 1024)
	exec(t, db, `INSERT INTO tool_call_state VALUES (?, 'call_b', '{"kind":"read"}', NULL)`, id)
	step("pending tool row", true, 512)
	exec(t, db, `INSERT OR REPLACE INTO tool_call_state VALUES (?, 'call_b', '{"kind":"read"}', '{"status":"completed"}')`, id)
	step("tool settled by replace", true, 512)
	exec(t, db, `UPDATE sessions SET title = 'renamed', main_chain_id = 100 WHERE id = ?`, id)
	step("main chain moved", true, 512)
	exec(t, db, `DELETE FROM message_nodes WHERE session_id = ? AND node_id = 150`, id)
	step("deleted node", false, 1<<20)
	addNode(t, db, id, 203, `{"message_id":"big-203","role":"assistant","content":"after delete"}`)
	step("node after the restart", true, 1024)
	exec(t, db, `DELETE FROM message_nodes WHERE session_id = ?`, id)
	exec(t, db, `DELETE FROM sessions WHERE id = ?`, id)
	step("vanished", false, 256)
	step("still gone", true, 0)
}
