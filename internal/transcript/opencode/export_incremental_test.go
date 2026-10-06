package opencode

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript/opencode/opencodetest"
)

// Compare the actual reconstructed stores, including rows a parser ignores,
// rather than only rendered text. State round trips on every step.
func TestExportAppendsMatchWholeExport(t *testing.T) {
	ctx := context.Background()
	s := opencodetest.New(t, "")
	const id = "ses_export"
	s.Session(id, "", "/work/demo", "Export tests", opencodetest.T0)
	for i := range 600 {
		s.Prompt(id, opencodetest.T0+int64(i)*3000, strings.Repeat("old text ", 128))
	}
	log, appended, state, err := ExportFrom(ctx, s.Path, id, nil)
	if err != nil || appended {
		t.Fatalf("initial export: %v, %v", appended, err)
	}
	step := func(name string, wantAppend bool, maxBytes int) {
		t.Helper()
		data, appended, next, err := ExportFrom(ctx, s.Path, id, state)
		if err != nil {
			t.Fatal(err)
		}
		if appended != wantAppend || len(data) > maxBytes {
			t.Fatalf("%s: appended %v, %d bytes (limit %d)", name, appended, len(data), maxBytes)
		}
		if len(data) != 0 && data[len(data)-1] != '\n' {
			t.Fatal("export ends mid-line")
		}
		if appended {
			log = append(log, data...)
		} else {
			log = data
		}
		state = next
		whole, err := Export(ctx, s.Path, id)
		if err != nil {
			t.Fatal(err)
		}
		got := exportStoreRows(t, log, id)
		want := exportStoreRows(t, whole, id)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: appended store differs from whole export", name)
		}
	}
	step("unchanged", true, 0)
	at := opencodetest.T0 + 600*3000
	part := s.Prompt(id, at, "stream one")
	step("new message and part", true, 1024)
	s.UpdatePart(part, at+1, `{"type":"text","text":"stream two"}`)
	step("stream update", true, 512)
	s.UpdatePart(part, at+1, `{"type":"text","text":"stream six"}`)
	step("same millisecond and length", true, 512)
	// Old updates are found from metadata even outside the overlap window.
	oldPart := opencodetest.ID("prt", opencodetest.T0, 2)
	s.UpdatePart(oldPart, opencodetest.T0+1, `{"type":"text","text":"edited old"}`)
	step("old part timestamp changed", true, 512)
	s.Exec(`UPDATE message SET data='{"role":"assistant"}', time_updated=? WHERE id=?`, at+2, opencodetest.ID("msg", at, 1))
	step("message rewritten", true, 512)
	s.Exec(`UPDATE session SET title='Title <with> symbols & punctuation', time_archived=? WHERE id=?`, at, id)
	step("session renamed and archived", true, 1024)
	step("metadata stable", true, 0)
	// Deletion with equal row count cannot hide behind an aggregate.
	s.Exec(`DELETE FROM part WHERE id=?`, oldPart)
	s.Part("prt_replacement", opencodetest.ID("msg", at, 1), id, at+3, `{"type":"text","text":"replacement"}`)
	step("delete and insert", false, 2<<20)
	s.UpdatePart(part, at+4, `{"type":"text","text":"after reset"}`)
	step("append after reset", true, 512)
	s.Exec(`DELETE FROM session WHERE id=?`, id)
	step("session metadata vanished", false, 2<<20)
	s.Exec(`DELETE FROM message WHERE session_id=?`, id)
	s.Exec(`DELETE FROM part WHERE session_id=?`, id)
	step("gone", false, 128)
	step("still gone", true, 0)
	s.Session(id, "", "/work/demo", "Recreated", at+5)
	s.Prompt(id, at+5, "back again")
	step("recreated", true, 1024)
	state = []byte(`{"v":999}`)
	step("unknown state version", false, 1024)
	state = []byte("broken")
	step("invalid state", false, 1024)
}

func exportStoreRows(t *testing.T, data []byte, session string) []string {
	t.Helper()
	path, err := LoadExport(context.Background(), bytes.NewReader(data), session, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var out []string
	for _, table := range []string{"session", "message", "part"} {
		rows, err := db.Query(`SELECT * FROM ` + table + ` ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprintf("%s:%v", table, values))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func TestExportAppendUpdatesAncestorMetadata(t *testing.T) {
	s := opencodetest.New(t, "")
	s.Session("grand", "", "/work/demo", "Grandparent", opencodetest.T0)
	s.Session("parent", "", "/work/demo", "Parent", opencodetest.T0)
	s.Session("child", "parent", "/work/demo", "Child", opencodetest.T0)
	s.Prompt("child", opencodetest.T0, "child work")
	ctx := context.Background()
	log, _, state, err := ExportFrom(ctx, s.Path, "child", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The parent records its spawning task after the child's first export.
	s.Message("msg_task", "parent", opencodetest.T0, `{"role":"assistant"}`)
	s.Part("prt_task", "msg_task", "parent", opencodetest.T0, `{"type":"tool","tool":"task","state":{"metadata":{"sessionId":"child"}}}`)
	for _, ancestorChange := range []string{"", `UPDATE session SET parent_id='grand' WHERE id='parent'`, `DELETE FROM session WHERE id='parent'`} {
		if ancestorChange != "" {
			s.Exec(ancestorChange)
		}
		data, appended, next, err := ExportFrom(ctx, s.Path, "child", state)
		if err != nil || !appended || len(data) == 0 || len(data) > 1024 {
			t.Fatalf("ancestor update: append %v, %d bytes, %v", appended, len(data), err)
		}
		log = append(log, data...)
		state = next
		whole, err := Export(ctx, s.Path, "child")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(exportStoreRows(t, log, "child"), exportStoreRows(t, whole, "child")) {
			t.Fatal("ancestor metadata differs")
		}
	}
}

func BenchmarkLiveSessionExport(b *testing.B) {
	s := opencodetest.New(b, "")
	const id = "ses_bench"
	s.Session(id, "", "/work/demo", "Benchmark", opencodetest.T0)
	body := strings.Repeat("old text ", 512)
	for i := range 1000 {
		s.Prompt(id, opencodetest.T0+int64(i)*3000, body)
	}
	at := opencodetest.T0 + 1000*3000
	part := s.Prompt(id, at, "streaming")
	_, _, state, err := ExportFrom(context.Background(), s.Path, id, nil)
	if err != nil {
		b.Fatal(err)
	}
	s.UpdatePart(part, at+1, `{"type":"text","text":"streamed update"}`)
	for _, mode := range []string{"whole", "incremental"} {
		b.Run(mode, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var data []byte
				var err error
				if mode == "incremental" {
					data, _, _, err = ExportFrom(context.Background(), s.Path, id, state)
				} else {
					data, err = Export(context.Background(), s.Path, id)
				}
				if err != nil {
					b.Fatal(err)
				}
				b.ReportMetric(float64(len(data)), "export-bytes/op")
			}
		})
	}
}
