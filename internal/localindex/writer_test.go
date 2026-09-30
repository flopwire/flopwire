package localindex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

// L1/D12: one writer per index. A second writing Open fails fast and
// names the holder; readers open alongside; the lock can be handed over
// (an agent's re-exec) without a gap.
func TestSecondWriterLocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Open(path, Options{})
	var locked *LockedError
	if !errors.As(err, &locked) || locked.PID != os.Getpid() {
		t.Fatalf("second writer: %v", err)
	}
	r, err := Open(path, Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("reader beside the writer: %v", err)
	}
	r.Close()
	// Hand-over: the lock outlives Close and is adopted by the next Open.
	lock := s.LockFile()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, Options{}); !errors.As(err, &locked) {
		t.Fatalf("lock released by Close after LockFile: %v", err)
	}
	s2, err := Open(path, Options{LockFile: lock})
	if err != nil {
		t.Fatalf("adopt lock: %v", err)
	}
	s2.Close()
	s3, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("after Close: %v", err)
	}
	s3.Close()
}

// L3: a read-only Open writes nothing: it needs an existing index, refuses
// writes, and leaves an index of another schema version alone.
func TestReadOnlyOpen(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(filepath.Join(dir, "none.db"), Options{ReadOnly: true}); err == nil {
		t.Fatal("read-only open of a missing index succeeded")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("read-only open created %v", ents)
	}
	path := filepath.Join(dir, "index.db")
	w, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	src := source(t, w, transcript.AgentClaude, "/h/ro.jsonl")
	apply(t, w, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{msg("ro", "r1", 0, transcript.KindUser, "read only wombat")}})
	r, err := Open(path, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if ids := findIDs(t, r, "wombat", FindOptions{}); len(ids) != 1 {
		t.Fatalf("reader find: %v", ids)
	}
	if st, err := r.Source(ctx, src.ID); err != nil || st.Source.Path != "/h/ro.jsonl" {
		t.Fatalf("reader source: %+v %v", st, err)
	}
	if _, err := r.EnsureSource(ctx, transcript.Source{Path: "/x"}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("write through reader: %v", err)
	}
	r.Close()
	w.Close()
	// An index of another version is the agent's to rebuild, not the reader's.
	w2, _ := Open(path, Options{})
	w2.write(ctx, func(w *writeTx) error { _, err := w.tx.Exec(`PRAGMA user_version = 3`); return err })
	w2.Close()
	if _, err := Open(path, Options{ReadOnly: true}); err == nil || !strings.Contains(err.Error(), "schema version 3") {
		t.Fatalf("reader on an old index: %v", err)
	}
	// The writer drops the old layout and starts over.
	w3, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("writer on an old index: %v", err)
	}
	defer w3.Close()
	var n int
	w3.DB().QueryRow(`SELECT count(*) FROM messages`).Scan(&n)
	if n != 0 || len(findIDs(t, w3, "wombat", FindOptions{})) != 0 {
		t.Fatalf("old rows kept: %d", n)
	}
}

// L3: a shard applies each fts_queue entry once, however the queue is cut
// into work items. Re-applying an insert under an entry already covered
// would leave a stale FTS entry that the row's later delete cannot remove.
func TestShardAppliesEachEntryOnce(t *testing.T) {
	s := openTest(t, DetailColumn)
	sh := s.shards[0] // fts_tok
	if err := sh.apply([]*ftsWork{{seq: 1, ops: []ftsOp{{seq: 1, id: 7, text: "alpha"}}}}); err != nil {
		t.Fatal(err)
	}
	// A replay whose chunk starts below applied: entry 1 again, then new ones.
	if err := sh.apply([]*ftsWork{{seq: 3, ops: []ftsOp{{seq: 1, id: 7, text: "alpha"}, {seq: 2, id: 7, del: true}, {seq: 3, id: 7, text: "beta"}}}}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := sh.db.QueryRow(`SELECT count(*) FROM fts_tok WHERE fts_tok MATCH 'alpha'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("stale alpha entries: %d %v", n, err)
	}
}

// L2: a shard transaction that fails is retried, in order; applied never
// passes it, so its rows are indexed once the shard recovers, and the
// shard does not stay broken.
func TestShardRetriesFailedBatch(t *testing.T) {
	defer func(min time.Duration) { shardRetryMin = min }(shardRetryMin)
	shardRetryMin = time.Millisecond
	s := openTest(t, DetailColumn)
	fails := 2
	shardFault = func(sh *ftsShard) error {
		if sh.table == "fts_tok" && fails > 0 {
			fails--
			return errors.New("injected shard failure")
		}
		return nil
	}
	defer func() { shardFault = nil }()
	src := source(t, s, transcript.AgentClaude, "/h/retry.jsonl")
	// Writes report the shard's error while it fails (the rows are
	// committed in the main database and the shard keeps retrying).
	s.ApplyBatch(ctx, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{msg("r", "r1", 0, transcript.KindUser, "first aardvark")}})
	s.ApplyBatch(ctx, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{msg("r", "r2", 1, transcript.KindUser, "second aardvark")}})
	deadline := time.Now().Add(5 * time.Second)
	for err := s.Sync(ctx); err != nil; err = s.Sync(ctx) {
		if time.Now().After(deadline) {
			t.Fatalf("shard never recovered: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	eq(t, "search", searchIDs(t, s, "aardvark", SearchOptions{}), []string{"r2", "r1"})
}

// L4: a lost shard file, or one ahead of the main database, is rebuilt
// from the stored text at Open, even after the queue was pruned.
func TestLostShardRebuilt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	src := source(t, s, transcript.AgentClaude, "/h/lost.jsonl")
	for i, text := range []string{"lost platypus", "kept platypus", "third platypus"} {
		apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{msg("l", "l"+itoa(int64(i)), int64(i), transcript.KindUser, text)}})
	}
	s.Close()
	for _, sfx := range []string{"", "-wal", "-shm"} {
		os.Remove(shardPath(path, "tok") + sfx)
	}
	s, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "search after losing fts_tok", searchIDs(t, s, "platypus", SearchOptions{}), []string{"l2", "l1", "l0"})
	// A shard ahead of the main database (the main file lost commits).
	if _, err := s.tri[0].db.Exec(`UPDATE fts_meta SET value = 1000000 WHERE key = 'applied'`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := s.tri[0].appliedSeq(); got >= 1000000 {
		t.Fatalf("shard ahead not rebuilt: applied %d", got)
	}
	if ids := findIDs(t, s, "platypus", FindOptions{}); len(ids) != 3 {
		t.Fatalf("find after rebuild: %v", ids)
	}
}

// L6: cancelling a request's context mid-statement must not roll back the
// transaction it shares with requests already answered (DeferCommit).
func TestCancelledRequestKeepsTransaction(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "index.db"), Options{DeferCommit: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	src := source(t, s, transcript.AgentClaude, "/h/cancel.jsonl")
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{msg("c", "c1", 0, transcript.KindUser, "answered narwhal")}})
	cctx, cancel := context.WithCancel(ctx)
	s.write(cctx, func(w *writeTx) error {
		// SQLite rolls back the whole transaction when it interrupts a
		// write statement.
		time.AfterFunc(5*time.Millisecond, cancel)
		if _, err := w.tx.ExecContext(w.ctx, `INSERT INTO meta (key, value)
			WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 300000) SELECT 'k' || x, x FROM c`); err != nil {
			return err
		}
		return errors.New("discard this request") // its savepoint rolls back
	})
	if err := s.Sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if ids := findIDs(t, s, "narwhal", FindOptions{}); len(ids) != 1 {
		t.Fatalf("answered row lost with the cancelled request: %v", ids)
	}
}

// L5: the end of a re-parse (superseding absent rows, retiring replaced
// file identities) commits with the rows and the watermark.
func TestSupersedeInFinalBatch(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/h/final.jsonl")
	old, err := s.EnsureSource(ctx, transcript.Source{Agent: transcript.AgentClaude, Path: "/h/final.jsonl", FileID: transcript.FileID{Dev: 1, Ino: 41},
		StorageKind: transcript.StorageJSONLAppend, Parser: "claude@1"})
	if err != nil {
		t.Fatal(err)
	}
	apply(t, s, Batch{SourceID: old.ID, Generation: 1, Messages: []*transcript.Message{msg("o", "o1", 0, transcript.KindUser, "replaced file row")}})
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{
		msg("f", "f1", 0, transcript.KindUser, "kept row"), msg("f", "f2", 1, transcript.KindUser, "dropped row")}})
	wm := transcript.Watermark{Offset: 123}
	apply(t, s, Batch{SourceID: src.ID, NewGeneration: &transcript.Generation{Generation: 2, Complete: true},
		Messages:  []*transcript.Message{msg("f", "f1", 0, transcript.KindUser, "kept row")},
		Watermark: &wm, SupersedeAbsent: true, RetireSources: []int64{old.ID, src.ID}})
	for id, want := range map[string]bool{"f1": false, "f2": true, "o1": true} {
		if rs := rowsOf(t, s, id); len(rs) != 1 || rs[0].superseded != want {
			t.Errorf("%s: %+v, want superseded=%v", id, rs, want)
		}
	}
	if st, _ := s.Source(ctx, src.ID); st.Watermark == nil || st.Watermark.Offset != 123 {
		t.Errorf("watermark %+v", st.Watermark)
	}
}

// D3: the trigram table indexes the whole stored text, so find reaches a
// match in the middle of a long row.
func TestFindReachesMiddleOfLongText(t *testing.T) {
	for _, detail := range []Detail{DetailColumn, DetailFull} {
		s := openTest(t, detail)
		src := source(t, s, transcript.AgentClaude, "/h/long.jsonl")
		long := strings.Repeat("ordinary output line\n", 800) + "needle-in-the-middle 4242\n" + strings.Repeat("more ordinary output\n", 800)
		apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{msg("l", "l1", 0, transcript.KindToolResult, long)}})
		if ids := findIDs(t, s, "needle-in-the-middle", FindOptions{}); len(ids) != 1 {
			t.Errorf("%s: middle of a %d-byte row not found: %v", detail, len(long), ids)
		}
	}
}
