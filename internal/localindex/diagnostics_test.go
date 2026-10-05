package localindex

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"sync"
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

// ExtractionSummary runs three statements. Off the writer they must still
// read one snapshot: a commit between them made the totals disagree with
// the per-code counts and the affected list.
func TestExtractionSummaryOneSnapshot(t *testing.T) {
	s := openTest(t, DetailFull)
	st := source(t, s, transcript.AgentClaude, "/a.jsonl")
	m := msg("session", "m", 0, transcript.KindToolResult, "preview")
	apply(t, s, Batch{SourceID: st.ID, Generation: 1, NewGeneration: &transcript.Generation{Generation: 1}, Watermark: &transcript.Watermark{Offset: 10, LineNo: 1}, Extraction: checkpointReport(t, 1, 10, 1, nil), AppliedParser: "claude@2", Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "session"}}, Messages: []*transcript.Message{m}})
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	var report string
	if err := s.rdb.QueryRowContext(ctx, `SELECT extraction_report FROM sources WHERE id=?`, st.ID).Scan(&report); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := s.write(ctx, func(w *writeTx) error {
				_, err := w.exec(`UPDATE sources SET extraction_report=CASE WHEN extraction_report IS NULL THEN ? ELSE NULL END WHERE id=?`, report, st.ID)
				return err
			}); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	defer func() { close(stop); <-done }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		sum, err := s.ExtractionSummary(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if a, l, c := sum.AffectedSources > 0, len(sum.Affected) > 0, len(sum.Counts) > 0; a != l || a != c {
			t.Fatalf("mixed snapshots: affected=%d list=%d counts=%v", sum.AffectedSources, len(sum.Affected), sum.Counts)
		}
	}
}

// One summary scan at a time: a status asked while one runs waits without
// a read connection, so two status calls cannot hold the agent's whole
// read pool (two connections) and stall hook reads.
func TestExtractionSummaryOneScanAtATime(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "index.db"), Options{ReadConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	source(t, s, transcript.AgentClaude, "/a.jsonl")
	inScan, release := make(chan struct{}, 2), make(chan struct{})
	testHookSummaryScan = func(context.Context) {
		inScan <- struct{}{}
		<-release
	}
	defer func() { testHookSummaryScan = nil }()
	var wg sync.WaitGroup
	defer wg.Wait()
	defer close(release)
	for range 2 {
		wg.Go(func() {
			if _, err := s.ExtractionSummary(ctx); err != nil {
				t.Error(err)
			}
		})
	}
	<-inScan
	time.Sleep(50 * time.Millisecond) // let the second caller reach the pool
	if n := s.rdb.Stats().InUse; n != 1 {
		t.Fatalf("%d read connections held by summaries, want 1", n)
	}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var one int
	if err := s.rdb.QueryRowContext(rctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("a hook read on the pool: %v", err)
	}
}

// The caller's context reaches the queries: a status whose client left
// stops its scan.
func TestExtractionSummaryCancelled(t *testing.T) {
	s := openTest(t, DetailFull)
	source(t, s, transcript.AgentClaude, "/a.jsonl")
	cctx, cancel := context.WithCancel(ctx)
	testHookSummaryScan = func(context.Context) { cancel() }
	defer func() { testHookSummaryScan = nil }()
	if _, err := s.ExtractionSummary(cctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled", err)
	}
	testHookSummaryScan = nil
	if _, err := s.ExtractionSummary(ctx); err != nil {
		t.Fatalf("after a cancelled scan: %v", err)
	}
}
