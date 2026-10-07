package devicesync

import (
	"github.com/flopwire/flopwire/internal/transcript"
	"testing"
)

// cut retains old manifest rows for gap provenance. They are not pending
// uploads once the recoverable prefix has been shortened.
func TestCoveragePendingBytesExcludeManifestBeyondActualCut(t *testing.T) {
	e := newEnv(t, Config{MaxRequestBytes: 16 << 10, SealAfter: -1}, 1<<20)
	sp := e.spec("cut.jsonl", transcript.StorageJSONLAppend)
	appendFile(t, sp.Path, jsonlLines(222, 800, 100))
	outcome, err := e.sy.syncTurn(t.Context(), sp, nil, -1, nil, captureSource)
	if err != nil || outcome != uploadPending {
		t.Fatalf("outcome=%v err=%v", outcome, err)
	}
	src, err := e.store.source(t.Context(), sp.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	g, err := e.store.gen(t.Context(), src.ID, src.Gen)
	if err != nil {
		t.Fatal(err)
	}
	before := g.Entries
	keep := g.Acked + 1
	if keep >= before {
		t.Fatal("fixture needs multiple unacknowledged entries")
	}
	want, err := e.store.entries(t.Context(), src.ID, g.Gen, g.Acked, 1)
	if err != nil || len(want) != 1 {
		t.Fatalf("entries=%v err=%v", want, err)
	}
	if err := e.sy.cut(t.Context(), src, g, keep); err != nil {
		t.Fatal(err)
	}
	var retained int64
	if err := e.store.db.QueryRow(`SELECT count(*) FROM devsync_manifest WHERE source_id=? AND generation=?`, src.ID, g.Gen).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained != before {
		t.Fatal("real cut no longer retains trailing provenance rows")
	}
	facts, err := e.store.capturedPendingContext(t.Context())
	if err != nil || facts.PendingManifestEntries != 1 || facts.PendingManifestBytes != want[0].Size || facts.PendingTailBytes != 0 || facts.TruncatedGenerations != 1 || facts.LostGenerations != 0 {
		t.Fatalf("cut facts=%+v want bytes=%d err=%v", facts, want[0].Size, err)
	}
}
