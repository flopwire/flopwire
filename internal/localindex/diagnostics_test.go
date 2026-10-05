package localindex

import (
	"strconv"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

func TestExtractionInspectionScopeAndCompanionRepair(t *testing.T) {
	s := openTest(t, DetailFull)
	st := source(t, s, transcript.AgentClaude, "/a.jsonl")
	source(t, s, transcript.AgentCodex, "/unassessed.jsonl")
	source(t, s, transcript.AgentDevin, "/devin.db")
	cp := checkpointReport(t, 1, 10, 1, nil)
	m := msg("session", "m", 0, transcript.KindToolResult, "preview")
	m.Enrichment = map[string]any{"persisted_output_missing": "tool-results/a.txt", "persisted_output_truncated": true}
	batch := Batch{SourceID: st.ID, Generation: 1, NewGeneration: &transcript.Generation{Generation: 1}, Watermark: &transcript.Watermark{Offset: 10, LineNo: 1}, Extraction: cp, AppliedParser: "claude@2", Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "session"}}, Messages: []*transcript.Message{m}}
	apply(t, s, batch)
	summary, err := s.ExtractionSummary(ctx)
	if err != nil || summary.AssessedSources != 1 || summary.UnassessedSources != 1 || summary.WarningSources != 1 || summary.Counts[transcript.MalformedRecord] != 1 || len(summary.Affected) != 1 {
		t.Fatalf("summary %+v %v", summary, err)
	}
	if err := s.write(ctx, func(w *writeTx) error {
		_, err := w.exec("UPDATE sources SET device_id=? WHERE id=?", "stored-device", st.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(st.ID, 10)
	detail, err := s.SourceDiagnostics(ctx, id)
	if err != nil || detail.MissingCompanions != 1 || detail.TruncatedCompanions != 1 || detail.DeviceID != "stored-device" {
		t.Fatalf("detail %+v %v", detail, err)
	}
	m.Enrichment = map[string]any{"persisted_output": "tool-results/a.txt"}
	batch.NewGeneration = nil
	apply(t, s, batch)
	detail, err = s.SourceDiagnostics(ctx, id)
	if err != nil || detail.MissingCompanions != 0 || detail.TruncatedCompanions != 0 {
		t.Fatalf("repair %+v %v", detail, err)
	}
}

// The agent's status answer calls ExtractionSummary, a scan of every
// source. It must not queue on the writer: a busy writer (a long batch, a
// Devin re-parse) made `flopwire agent status` and `setup --check` time
// out, and each abandoned status still ran its scan on the writer, ahead of
// hook flushes and index writes.
func TestExtractionSummaryDoesNotWaitForWriter(t *testing.T) {
	s := openTest(t, DetailFull)
	source(t, s, transcript.AgentClaude, "/a.jsonl")
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	busy, release := make(chan struct{}), make(chan struct{})
	go s.write(ctx, func(*writeTx) error {
		close(busy)
		<-release
		return nil
	})
	<-busy
	defer close(release)
	got := make(chan error, 1)
	go func() {
		summary, err := s.ExtractionSummary(ctx)
		if err == nil && summary.UnassessedSources != 1 {
			t.Errorf("summary %+v", summary)
		}
		got <- err
	}()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ExtractionSummary waited for the busy writer")
	}
}
