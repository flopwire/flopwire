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
