package localindex

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

func generationCheckpointRows(w *writeTx) ([]string, error) {
	rows, e := w.tx.Query(`SELECT json_array(source_id,generation,size,change_time,captured_at,complete,reason) FROM generations ORDER BY source_id,generation`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if e = rows.Scan(&s); e != nil {
			return nil, e
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func TestStartGenerationCheckpointDifferential(t *testing.T) {
	fast, full := openTest(t, DetailColumn), openTest(t, DetailColumn)
	a, b := source(t, fast, transcript.AgentClaude, "/p/generation.jsonl"), source(t, full, transcript.AgentClaude, "/p/generation.jsonl")
	other, otherFull := source(t, fast, transcript.AgentClaude, "/p/other.jsonl"), source(t, full, transcript.AgentClaude, "/p/other.jsonl")
	if a.ID != b.ID || other.ID != otherFull.ID {
		t.Fatal("source IDs differ")
	}
	err := fast.write(ctx, func(fw *writeTx) error {
		return full.write(ctx, func(bw *writeTx) error {
			ft, bt := txRun{w: fw}, txRun{w: bw}
			step := func(name string, build func() writeReq, wantErr string) error {
				for i, tt := range []*txRun{&ft, &bt} {
					req := build()
					if i == 1 {
						req.projection = projectionInvalidate
					}
					n := len(tt.waiting)
					tt.runOne(req)
					if len(tt.waiting) == n {
						e := <-req.done
						if wantErr == "" || e == nil || !strings.Contains(e.Error(), wantErr) {
							return fmt.Errorf("%s unexpected failure: %v", name, e)
						}
					} else if wantErr != "" {
						return fmt.Errorf("%s expected failure", name)
					}
				}
				x, e := projectionSnapshot(fw)
				if e != nil {
					return e
				}
				y, e := projectionSnapshot(bw)
				if e != nil {
					return e
				}
				if !reflect.DeepEqual(x, y) {
					return fmt.Errorf("%s projection differs: %v / %v", name, x, y)
				}
				x, e = generationCheckpointRows(fw)
				if e != nil {
					return e
				}
				y, e = generationCheckpointRows(bw)
				if e != nil {
					return e
				}
				if !reflect.DeepEqual(x, y) {
					return fmt.Errorf("%s generation metadata differs", name)
				}
				xst, e := loadSource(ctx, fw.tx, `WHERE id=?`, a.ID)
				if e != nil {
					return e
				}
				yst, e := loadSource(ctx, bw.tx, `WHERE id=?`, a.ID)
				if e != nil {
					return e
				}
				if !reflect.DeepEqual(xst, yst) {
					return fmt.Errorf("%s source states differ", name)
				}
				return nil
			}
			appendBatch := func(b Batch) error {
				return step("append", func() writeReq {
					return writeReq{ctx: ctx, done: make(chan error, 1), projection: projectionAppend, fn: func(w *writeTx) error { var r BatchResult; return w.applyBatch(&b, &r) }}
				}, "")
			}
			generation := func(id, gen, size int64, reason string, wantErr string) error {
				return step("StartGeneration", func() writeReq {
					return startGenerationRequest(ctx, id, transcript.Generation{Generation: gen, Size: size, CapturedAt: t0, ChangeTime: t0, Complete: true}, reason)
				}, wantErr)
			}
			plain := func(sql string) error {
				return step("default write", func() writeReq {
					return writeReq{ctx: ctx, done: make(chan error, 1), fn: func(w *writeTx) error { _, e := w.exec(sql); return e }}
				}, "")
			}
			if e := generation(a.ID, 1, 10, "first", ""); e != nil {
				return e
			}
			if fw.changeCache != nil || fw.changeAppend {
				return fmt.Errorf("generation seeded or advanced checkpoint")
			}
			pending := changePair("s", 1, "1234567")
			if e := appendBatch(Batch{SourceID: a.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "s", Cwd: "/repo"}}, Messages: append(changePair("s", 0, "abcdef0"), pending[0])}); e != nil {
				return e
			}
			var conv int64
			if e := fw.tx.QueryRow(`SELECT id FROM conversations WHERE session_id='s'`).Scan(&conv); e != nil {
				return e
			}
			checkpoint := fw.changeCache[conv]
			if checkpoint == nil {
				return fmt.Errorf("missing checkpoint")
			}
			for _, v := range []struct {
				id, gen, size int64
				reason        string
			}{{a.ID, 2, 20, "new"}, {a.ID, 2, 30, "ignored"}, {a.ID, 1, 40, "ignored-old"}, {other.ID, 8, 50, "unrelated"}} {
				if e := generation(v.id, v.gen, v.size, v.reason, ""); e != nil {
					return e
				}
				if fw.changeCache[conv] != checkpoint || fw.changeAppend {
					return fmt.Errorf("public generation discarded or advanced checkpoint")
				}
			}
			var gen, size int64
			var reason string
			if e := fw.tx.QueryRow(`SELECT generation FROM sources WHERE id=?`, a.ID).Scan(&gen); e != nil {
				return e
			}
			if gen != 2 {
				return fmt.Errorf("current generation regressed: %d", gen)
			}
			if e := fw.tx.QueryRow(`SELECT size,reason FROM generations WHERE source_id=? AND generation=2`, a.ID).Scan(&size, &reason); e != nil {
				return e
			}
			if size != 30 || reason != "new" {
				return fmt.Errorf("conflict semantics changed: %d/%s", size, reason)
			}
			// Same tool-call ID in generation2 cannot consume the generation1 pending call.
			different := *pending[1]
			different.NativeID = "generation2-result"
			if e := appendBatch(Batch{SourceID: a.ID, Generation: 2, Messages: []*transcript.Message{&different}}); e != nil {
				return e
			}
			var count int
			if e := fw.tx.QueryRow(`SELECT count(*) FROM change_events WHERE observed_hash='1234567'`).Scan(&count); e != nil {
				return e
			}
			if count != 0 {
				return fmt.Errorf("paired across generations")
			}
			if fw.changeCache[conv] != checkpoint {
				return fmt.Errorf("new-generation suffix failed to reuse checkpoint")
			}
			if e := appendBatch(Batch{SourceID: a.ID, Generation: 1, Messages: pending[1:]}); e != nil {
				return e
			}
			if fw.changeCache[conv] != checkpoint {
				return fmt.Errorf("old pending completion did not hit checkpoint")
			}
			var commandID, physicalID int64
			if e := fw.tx.QueryRow(`SELECT command_id FROM change_events WHERE observed_hash='1234567'`).Scan(&commandID); e != nil {
				return e
			}
			if e := fw.tx.QueryRow(`SELECT id FROM messages WHERE native_id=?`, pending[0].NativeID).Scan(&physicalID); e != nil {
				return e
			}
			if commandID != physicalID {
				return fmt.Errorf("pending physical ID changed")
			}
			// Moving an existing command into another generation changes retained input;
			// it must use full replay and stop pairing with the old result.
			if e := appendBatch(Batch{SourceID: a.ID, Generation: 2, Messages: pending[:1]}); e != nil {
				return e
			}
			if fw.changeCache[conv] == checkpoint {
				return fmt.Errorf("existing row generation migration reused stale checkpoint")
			}
			// ApplyBatch.NewGeneration retains its conservative clear, even without rows.
			if e := appendBatch(Batch{SourceID: a.ID, NewGeneration: &transcript.Generation{Generation: 3, CapturedAt: t0}}); e != nil {
				return e
			}
			if fw.changeCache != nil || fw.changeAppend {
				return fmt.Errorf("ApplyBatch.NewGeneration policy changed")
			}
			if e := appendBatch(Batch{SourceID: a.ID, Generation: 3, Messages: changePair("s", 3, "7654321")}); e != nil {
				return e
			}
			// A generation INSERT succeeds before the source UPDATE fails. This request
			// must roll back the catalog insert, discard checkpoints and retain prior events.
			if e := plain(`CREATE TEMP TRIGGER reject_generation BEFORE UPDATE OF generation ON sources WHEN NEW.generation=99 BEGIN SELECT RAISE(ABORT,'generation failure'); END`); e != nil {
				return e
			}
			if e := appendBatch(Batch{SourceID: a.ID, Generation: 3, Messages: changePair("s", 4, "fedcba0")}); e != nil {
				return e
			}
			before, e := projectionSnapshot(fw)
			if e != nil {
				return e
			}
			catalog, e := generationCheckpointRows(fw)
			if e != nil {
				return e
			}
			if e := generation(a.ID, 99, 99, "rollback", "generation failure"); e != nil {
				return e
			}
			if fw.changeCache != nil {
				return fmt.Errorf("failed generation retained cache")
			}
			after, e := projectionSnapshot(fw)
			if e != nil {
				return e
			}
			catalogAfter, e := generationCheckpointRows(fw)
			if e != nil {
				return e
			}
			if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(catalog, catalogAfter) {
				return fmt.Errorf("generation failure escaped rollback")
			}
			if e := plain(`DROP TRIGGER reject_generation`); e != nil {
				return e
			}
			if e := generation(999999, 1, 1, "missing", "source"); e != nil {
				return e
			}
			catalogAfter, e = generationCheckpointRows(fw)
			if e != nil {
				return e
			}
			if !reflect.DeepEqual(catalog, catalogAfter) {
				return fmt.Errorf("missing source left orphan generation")
			}
			if e := generation(a.ID, 4, 40, "no-seed", ""); e != nil {
				return e
			}
			if fw.changeCache != nil {
				return fmt.Errorf("metadata write seeded checkpoint")
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStartGenerationCheckpointCommitBoundary(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/p/commit.jsonl")
	tx, e := s.wdb.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	w := &writeTx{s: s, tx: tx, ctx: ctx}
	tt := txRun{w: w}
	b := Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "s", Cwd: "/repo"}}, Messages: changePair("s", 0, "abcdef0")}
	appendReq := writeReq{ctx: ctx, done: make(chan error, 1), projection: projectionAppend, fn: func(w *writeTx) error { var r BatchResult; return w.applyBatch(&b, &r) }}
	tt.runOne(appendReq)
	if len(w.changeCache) == 0 {
		t.Fatal("missing checkpoint")
	}
	req := startGenerationRequest(ctx, src.ID, transcript.Generation{Generation: 2, CapturedAt: t0}, "next")
	tt.runOne(req)
	if len(w.changeCache) == 0 {
		t.Fatal("generation discarded checkpoint")
	}
	sync := writeReq{ctx: ctx, done: make(chan error, 1)}
	tt.runOne(sync)
	if !tt.barrier {
		t.Fatal("Sync did not request commit")
	}
	tt.commit()
	for _, r := range []writeReq{appendReq, req, sync} {
		if e := <-r.done; e != nil {
			t.Fatal(e)
		}
	}
	if w.changeCache != nil {
		t.Fatal("checkpoint survived commit")
	}
	// A subsequent actual public request starts with no state from the old tx.
	if e := s.StartGeneration(ctx, src.ID, transcript.Generation{Generation: 3, CapturedAt: t0}, "after commit"); e != nil {
		t.Fatal(e)
	}
	if e := s.write(ctx, func(w *writeTx) error {
		if w.changeCache != nil {
			return fmt.Errorf("checkpoint crossed transaction boundary")
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
}
