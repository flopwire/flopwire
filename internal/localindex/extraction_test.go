package localindex

import (
	"database/sql"
	"reflect"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

func checkpointReport(t *testing.T, gen, offset, line int64, previous *transcript.ExtractionCheckpoint) *transcript.ExtractionCheckpoint {
	t.Helper()
	from := int64(0)
	if previous != nil {
		from = previous.Offset
	}
	d := transcript.Diagnostics{FromOffset: from}
	d.Record(transcript.MalformedRecord, line, from)
	report := d.Report()
	cp, err := transcript.FinalizeExtraction(previous, gen, "claude@2/test", transcript.ParseResult{FromOffset: from, Cursor: transcript.Cursor{Offset: offset, LineNo: line}, Report: &report})
	if err != nil {
		t.Fatal(err)
	}
	return cp
}
func TestExtractionCheckpointAtomicFailureAndReplay(t *testing.T) {
	s := openTest(t, DetailFull)
	st := source(t, s, transcript.AgentClaude, "/poc.jsonl")
	first := checkpointReport(t, 1, 10, 1, nil)
	apply(t, s, Batch{SourceID: st.ID, Generation: 1, NewGeneration: &transcript.Generation{Generation: 1},
		Watermark: &transcript.Watermark{Offset: 10, LineNo: 1}, AppliedParser: "claude@2", Extraction: first})
	before, err := s.Source(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Extraction == nil || before.Source.Parser != "claude@2" {
		t.Fatal("missing applied extraction metadata")
	}
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", s.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TRIGGER fail_report BEFORE UPDATE OF extraction_report ON sources BEGIN SELECT RAISE(ABORT,'blocked diagnostic commit'); END`)
	if err != nil {
		t.Fatal(err)
	}
	second := checkpointReport(t, 1, 20, 2, first)
	batch := Batch{SourceID: st.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "s"}},
		Messages:  []*transcript.Message{msg("s", "m", 0, transcript.KindUser, "checkpoint atomic")},
		Watermark: &transcript.Watermark{Offset: 20, LineNo: 2}, CursorState: []byte("new-state"), AppliedParser: "claude@2", Extraction: second}
	if _, err := s.ApplyBatch(ctx, batch); err == nil || !strings.Contains(err.Error(), "blocked diagnostic") {
		t.Fatalf("unexpected failure %v", err)
	}
	after, err := s.Source(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed report commit changed checkpoint")
	}
	var n int
	if err := s.DB().QueryRow("SELECT count(*) FROM messages").Scan(&n); err != nil || n != 0 {
		t.Fatal("final batch survived rollback")
	}
	if _, err := db.Exec("DROP TRIGGER fail_report"); err != nil {
		t.Fatal(err)
	}
	apply(t, s, batch)
	apply(t, s, batch) // replay the same completed checkpoint, not a new delta
	saved, err := s.Source(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Watermark.Offset != 20 || saved.Extraction.Report.Issues[0].Count != 2 {
		t.Fatal("replay double counted or lost cursor")
	}
}
