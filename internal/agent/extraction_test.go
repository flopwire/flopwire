package agent

import (
	"os"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

func extractionCount(cp *transcript.ExtractionCheckpoint, code transcript.DiagnosticCode) uint64 {
	if cp == nil {
		return 0
	}
	for _, issue := range cp.Report.Issues {
		if issue.Code == code {
			return issue.Count
		}
	}
	return 0
}
func TestAgentExtractionAppendRewriteAndLegacyAssessment(t *testing.T) {
	f := newFixture(t, "-")
	f.once()
	path := f.path(alphaRel)
	rows, err := f.store.SourcesByPath(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	st := rows[0]
	if st.Extraction == nil {
		t.Fatal("initial extraction not persisted")
	}
	original := extractionCount(st.Extraction, transcript.MalformedRecord)
	appendFile(t, path, "{broken\n")
	if _, err := f.a.FlushPath(ctx, path, ""); err != nil {
		t.Fatal(err)
	}
	st, err = f.store.Source(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if extractionCount(st.Extraction, transcript.MalformedRecord) != original+1 {
		t.Fatal("append count missing")
	}
	if _, err := f.a.FlushPath(ctx, path, ""); err != nil {
		t.Fatal(err)
	}
	same, err := f.store.Source(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if extractionCount(same.Extraction, transcript.MalformedRecord) != original+1 {
		t.Fatal("unchanged parse double counted")
	}
	// Complete records count; a live incomplete tail does not.
	appendFile(t, path, "{broken")
	if _, err := f.a.FlushPath(ctx, path, ""); err != nil {
		t.Fatal(err)
	}
	tail, err := f.store.Source(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if extractionCount(tail.Extraction, transcript.MalformedRecord) != original+1 {
		t.Fatal("incomplete tail warned")
	}
	appendFile(t, path, "\n")
	if _, err := f.a.FlushPath(ctx, path, ""); err != nil {
		t.Fatal(err)
	}
	tail, err = f.store.Source(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if extractionCount(tail.Extraction, transcript.MalformedRecord) != original+2 {
		t.Fatal("completed bad tail not counted")
	}
	// Invalid saved parser state forces a full replacement within an append.
	f.exec("UPDATE sources SET cursor_state=? WHERE id=?", []byte("invalid"), st.ID)
	f.exec("UPDATE messages SET native_id='old-state-native-id' WHERE id=(SELECT id FROM messages WHERE source_id=? AND NOT superseded LIMIT 1)", st.ID)
	appendFile(t, path, claudeUser("state-reset", "after invalid cursor"))
	if _, err := f.a.FlushPath(ctx, path, ""); err != nil {
		t.Fatal(err)
	}
	reset, err := f.store.Source(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reset.Generation <= st.Generation || extractionCount(reset.Extraction, transcript.MalformedRecord) != original+2 {
		t.Fatal("state reset did not replace extraction generation")
	}
	var live int
	if err := f.store.DB().QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE source_id=? AND native_id='old-state-native-id' AND NOT superseded", st.ID).Scan(&live); err != nil || live != 0 {
		t.Fatalf("state reset retained old row: %d %v", live, err)
	}
	// A legacy report is absent, not issue-free. The existing gate queues a
	// full assessment even when the file tuple did not change.
	f.exec("UPDATE sources SET extraction_report=NULL WHERE id=?", st.ID)
	f.restart()
	f.once()
	assessed, err := f.store.Source(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if assessed.Extraction == nil || extractionCount(assessed.Extraction, transcript.MalformedRecord) != original+2 {
		t.Fatal("legacy checkpoint not assessed")
	}
	if err := os.WriteFile(path, []byte(claudeUser("repaired", "repaired transcript")), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.FlushPath(ctx, path, ""); err != nil {
		t.Fatal(err)
	}
	repaired, err := f.store.Source(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if extractionCount(repaired.Extraction, transcript.MalformedRecord) != 0 || repaired.Extraction.Generation <= st.Extraction.Generation {
		t.Fatal("rewrite retained old evidence")
	}
}
