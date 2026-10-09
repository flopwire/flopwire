package devicesync

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

func lifecycleSource(t *testing.T, e *env, spec SourceSpec) *sourceRow {
	t.Helper()
	src, err := e.store.source(t.Context(), spec.Path, &spec)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func lifecycleGeneration(t *testing.T, e *env, src *sourceRow) *genRow {
	t.Helper()
	g, err := e.store.gen(t.Context(), src.ID, src.Gen)
	if err != nil || g == nil {
		t.Fatalf("generation: %v, %v", g, err)
	}
	return g
}

func lifecycleTail(t *testing.T, e *env, sid, gen int64, tail syncproto.Tail, want []byte, present bool) {
	t.Helper()
	body, ok, err := e.spool.TailVersion(sid, gen, tail.Hash)
	if err != nil || ok != present || present && !bytes.Equal(body, want) {
		t.Fatalf("tail found=%v error=%v bytes=%q, want present=%v bytes=%q", ok, err, body, present, want)
	}
}

func lifecycleExport(body, state []byte) ExportFunc {
	return func(context.Context, []byte) (Export, error) { return Export{Data: body, State: state}, nil }
}

// The guard runs inside the real capture transaction, after publication and
// before Commit. Its failure proves that cleanup cannot precede durability.
func TestTailLifecycleIdleSealRetainsTailUntilCommit(t *testing.T) {
	now := time.Unix(1800000000, 0)
	e := newEnv(t, Config{SealAfter: time.Minute, Now: func() time.Time { return now }}, 4096)
	spec := e.spec("seal-export", transcript.StorageSQLite)
	spec.Export = true
	src := lifecycleSource(t, e, spec)
	body := []byte("synthetic complete record\n")
	export := lifecycleExport(body, []byte("state"))
	if err := e.sy.ordinaryOperation().capture(t.Context(), src, export, -1, nil); err != nil {
		t.Fatal(err)
	}
	before := lifecycleGeneration(t, e, src)
	if before.Tail.Size != int64(len(body)) || before.Entries != 0 {
		t.Fatalf("initial generation: %+v", before)
	}
	now = now.Add(2 * time.Minute)
	rejected := errors.New("reject seal transaction")
	guardCommit := func(ctx context.Context, src *sourceRow, g *genRow, add []syncproto.Entry, wm *transcript.Watermark, state []byte, guards ...func() error) error {
		if g.Tail.Size != 0 || len(add) != 1 {
			t.Fatalf("seal capture: %+v, %+v", g, add)
		}
		chunk, ok, err := e.spool.Chunk(before.Tail.Hash)
		if err != nil || !ok || !bytes.Equal(chunk, body) {
			t.Fatalf("seal chunk not published before commit: %v %v", ok, err)
		}
		lifecycleTail(t, e, src.ID, before.Gen, before.Tail, body, true)
		guards = append(guards, func() error { return rejected })
		return e.store.saveCapture(ctx, src, g, add, wm, state, guards...)
	}
	if err := e.sy.ordinaryOperation().captureWithCommit(t.Context(), src, export, -1, nil, guardCommit); !errors.Is(err, rejected) {
		t.Fatalf("guard error: %v", err)
	}
	committed := lifecycleGeneration(t, e, src)
	if committed.Tail != before.Tail || committed.Entries != before.Entries {
		t.Fatalf("failed seal changed ledger: %+v", committed)
	}
	lifecycleTail(t, e, src.ID, before.Gen, before.Tail, body, true)
	if err := e.sy.ordinaryOperation().capture(t.Context(), src, export, -1, nil); err != nil {
		t.Fatal(err)
	}
	committed = lifecycleGeneration(t, e, src)
	if committed.Tail.Size != 0 || committed.Entries != 1 {
		t.Fatalf("seal did not commit: %+v", committed)
	}
	lifecycleTail(t, e, src.ID, before.Gen, before.Tail, nil, false)
}

func TestTailLifecycleAcknowledgedExportRewriteRetainsOldUntilCommit(t *testing.T) {
	e := newEnv(t, Config{SealAfter: -1}, 4096)
	spec := e.spec("rewrite-export", transcript.StorageSQLite)
	spec.Export = true
	src := lifecycleSource(t, e, spec)
	old := []byte("old synthetic export\n")
	next := []byte("replacement synthetic export\n")
	if err := e.sy.ordinaryOperation().capture(t.Context(), src, lifecycleExport(old, []byte("old state")), -1, nil); err != nil {
		t.Fatal(err)
	}
	g := lifecycleGeneration(t, e, src)
	g.TailAcked = true
	if err := e.store.updateGen(t.Context(), g, nil); err != nil {
		t.Fatal(err)
	}
	e.sy.release(t.Context(), src, g, nil)
	lifecycleTail(t, e, src.ID, g.Gen, g.Tail, old, true)
	rejected := errors.New("reject rewrite transaction")
	commit := func(ctx context.Context, src *sourceRow, nextGen *genRow, add []syncproto.Entry, wm *transcript.Watermark, state []byte, guards ...func() error) error {
		if nextGen.Gen != g.Gen+1 {
			t.Fatalf("rewrite generation: %d", nextGen.Gen)
		}
		lifecycleTail(t, e, src.ID, g.Gen, g.Tail, old, true)
		lifecycleTail(t, e, src.ID, nextGen.Gen, nextGen.Tail, next, true)
		return e.store.saveCapture(ctx, src, nextGen, add, wm, state, append(guards, func() error { return rejected })...)
	}
	export := lifecycleExport(next, []byte("next state"))
	if err := e.sy.ordinaryOperation().captureWithCommit(t.Context(), src, export, -1, nil, commit); !errors.Is(err, rejected) {
		t.Fatalf("rewrite error: %v", err)
	}
	src = lifecycleSource(t, e, spec)
	if src.Gen != g.Gen || !bytes.Equal(src.ExportState, []byte("old state")) {
		t.Fatalf("failed rewrite changed source: %+v", src)
	}
	lifecycleTail(t, e, src.ID, g.Gen, g.Tail, old, true)
	if err := e.sy.ordinaryOperation().capture(t.Context(), src, export, -1, nil); err != nil {
		t.Fatal(err)
	}
	committed := lifecycleGeneration(t, e, src)
	if committed.Gen != g.Gen+1 || committed.Tail.Hash != syncproto.Sum(next) {
		t.Fatalf("rewrite ledger: %+v", committed)
	}
	lifecycleTail(t, e, src.ID, g.Gen, g.Tail, nil, false)
	lifecycleTail(t, e, src.ID, committed.Gen, committed.Tail, next, true)
}

func TestTailLifecycleCutFailurePreservesOldCommittedTail(t *testing.T) {
	e := newEnv(t, Config{SealAfter: -1}, 4096)
	spec := e.spec("cut-export", transcript.StorageSQLite)
	spec.Export = true
	src := lifecycleSource(t, e, spec)
	body := []byte("pending bytes to cut\n")
	if err := e.sy.ordinaryOperation().capture(t.Context(), src, lifecycleExport(body, []byte("state")), -1, nil); err != nil {
		t.Fatal(err)
	}
	before := lifecycleGeneration(t, e, src)
	if _, err := e.store.db.Exec(`CREATE TRIGGER reject_cut BEFORE UPDATE ON devsync_gens BEGIN SELECT RAISE(ABORT, 'reject cut'); END`); err != nil {
		t.Fatal(err)
	}
	if err := e.sy.cut(t.Context(), src, lifecycleGeneration(t, e, src), 0); err == nil {
		t.Fatal("cut unexpectedly committed")
	}
	committed := lifecycleGeneration(t, e, src)
	if committed.Tail != before.Tail || committed.TailAcked != before.TailAcked {
		t.Fatalf("failed cut changed ledger: %+v", committed)
	}
	lifecycleTail(t, e, src.ID, before.Gen, before.Tail, body, true)
	if _, err := e.store.db.Exec(`DROP TRIGGER reject_cut`); err != nil {
		t.Fatal(err)
	}
	if err := e.sy.cut(t.Context(), src, committed, 0); err != nil {
		t.Fatal(err)
	}
	committed = lifecycleGeneration(t, e, src)
	if committed.Tail.Size != 0 || !committed.TailAcked {
		t.Fatalf("cut not committed: %+v", committed)
	}
	lifecycleTail(t, e, src.ID, before.Gen, before.Tail, nil, false)
}

func TestTailLifecycleAcknowledgementRetentionUsesDurableReference(t *testing.T) {
	for _, export := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "incremental-export"}[export], func(t *testing.T) {
			e := newEnv(t, Config{SealAfter: -1}, 4096)
			spec := e.spec("ack-source", transcript.StorageSQLite)
			spec.Export = export
			src := lifecycleSource(t, e, spec)
			body := []byte("durable acknowledgement fixture\n")
			tail := syncproto.Tail{Size: int64(len(body)), Hash: syncproto.Sum(body)}
			g := &genRow{SourceID: src.ID, Gen: 0, FileID: "synthetic-file", Size: tail.Size, Tail: tail}
			var state []byte
			if export {
				state = []byte("incremental state")
			}
			if err := e.spool.PutTailVersion(src.ID, 0, tail.Hash, body); err != nil {
				t.Fatal(err)
			}
			if err := e.store.saveCapture(t.Context(), src, g, nil, &transcript.Watermark{Offset: tail.Size}, state); err != nil {
				t.Fatal(err)
			}
			// A mutated in-memory ACK must not release a still-pending durable copy.
			g.TailAcked = true
			e.sy.release(t.Context(), src, g, nil)
			lifecycleTail(t, e, src.ID, 0, tail, body, true)
			if err := e.store.updateGen(t.Context(), g, nil); err != nil {
				t.Fatal(err)
			}
			e.sy.release(t.Context(), src, g, nil)
			lifecycleTail(t, e, src.ID, 0, tail, body, export)
		})
	}
}

func TestTailLifecycleSameHashReleasePreservesLiveVersion(t *testing.T) {
	e := newEnv(t, Config{SealAfter: -1}, 4096)
	spec := e.spec("same-hash-export", transcript.StorageSQLite)
	spec.Export = true
	src := lifecycleSource(t, e, spec)
	body := []byte("same committed bytes\n")
	if err := e.sy.ordinaryOperation().capture(t.Context(), src, lifecycleExport(body, []byte("state")), -1, nil); err != nil {
		t.Fatal(err)
	}
	g := lifecycleGeneration(t, e, src)
	e.sy.releaseTail(t.Context(), src.ID, g.Gen, g.Tail)
	lifecycleTail(t, e, src.ID, g.Gen, g.Tail, body, true)
	if e.spool.Used() != int64(len(body)) {
		t.Fatalf("live same-hash accounting: %d", e.spool.Used())
	}
}

func TestTailLifecycleSalvageRequiresExactPendingTail(t *testing.T) {
	for _, matching := range []bool{false, true} {
		t.Run(map[bool]string{false: "mismatching-presence", true: "exact-pending-version"}[matching], func(t *testing.T) {
			e := newEnv(t, Config{SealAfter: -1}, 4096)
			spec := e.spec("vanished-jsonl", transcript.StorageJSONLAppend)
			src := lifecycleSource(t, e, spec)
			body := []byte("pending native bytes\n")
			tail := syncproto.Tail{Size: int64(len(body)), Hash: syncproto.Sum(body)}
			g := &genRow{SourceID: src.ID, Gen: 0, FileID: "vanished-inode", Size: tail.Size, Tail: tail}
			if err := e.store.saveCapture(t.Context(), src, g, nil, &transcript.Watermark{Offset: tail.Size}, nil); err != nil {
				t.Fatal(err)
			}
			stored := []byte("different native bytes\n")
			if matching {
				stored = body
			}
			if err := e.spool.PutTailVersion(src.ID, 0, syncproto.Sum(stored), stored); err != nil {
				t.Fatal(err)
			}
			e.sy.ordinaryOperation().salvage(t.Context(), src, g, "synthetic source unavailable")
			committed := lifecycleGeneration(t, e, src)
			if matching {
				if committed.Tail != tail || committed.TailAcked {
					t.Fatalf("exact pending bytes were cut: %+v", committed)
				}
				lifecycleTail(t, e, src.ID, 0, tail, body, true)
			} else if committed.Tail.Size != 0 || !committed.TailAcked {
				t.Fatalf("wrong bytes accepted as pending tail: %+v", committed)
			}
		})
	}
}
