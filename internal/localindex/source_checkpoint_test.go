package localindex

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

// Both sides execute the actual public request builder. The control drops
// checkpoints before each request, independently retaining full-replay behavior.
func TestEnsureSourceCheckpointDifferential(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			fast, full := openTest(t, DetailColumn), openTest(t, DetailColumn)
			a, b := source(t, fast, transcript.AgentClaude, "/p/source.jsonl"), source(t, full, transcript.AgentClaude, "/p/source.jsonl")
			if a.ID != b.ID {
				t.Fatal("source IDs differ")
			}
			err := fast.write(ctx, func(fw *writeTx) error {
				return full.write(ctx, func(bw *writeTx) error {
					ft, bt := txRun{w: fw}, txRun{w: bw}
					step := func(build func(int) writeReq, wantFailure bool) error {
						for i, tt := range []*txRun{&ft, &bt} {
							req := build(i)
							if i == 1 {
								req.projection = projectionInvalidate
							}
							n := len(tt.waiting)
							tt.runOne(req)
							if len(tt.waiting) == n {
								e := <-req.done
								if !wantFailure || e == nil {
									return fmt.Errorf("unexpected request failure: %v", e)
								}
							} else if wantFailure {
								return fmt.Errorf("expected load failure")
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
							return fmt.Errorf("projection differs: %v / %v", x, y)
						}
						return nil
					}
					appendBatch := func(batch Batch) error {
						return step(func(int) writeReq {
							return writeReq{ctx: ctx, done: make(chan error, 1), projection: projectionAppend, fn: func(w *writeTx) error { var r BatchResult; return w.applyBatch(&batch, &r) }}
						}, false)
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
						return fmt.Errorf("missing seed")
					}
					src := a.Source
					ensure := func(spec transcript.Source) ([2]SourceState, error) {
						var states [2]SourceState
						e := step(func(i int) writeReq {
							stores := []*Store{fast, full}
							return stores[i].ensureSourceRequest(ctx, spec, &states[i])
						}, false)
						if e == nil && !reflect.DeepEqual(states[0], states[1]) {
							e = fmt.Errorf("source states differ")
						}
						if e == nil && (fw.changeCache[conv] != checkpoint || fw.changeAppend) {
							e = fmt.Errorf("EnsureSource discarded or advanced checkpoint")
						}
						return states, e
					}
					if _, e := ensure(src); e != nil {
						return e
					}
					src.SessionKey = "owner"
					src.Parser = "claude@next"
					states, e := ensure(src)
					if e != nil {
						return e
					}
					if states[0].ID != a.ID || states[0].Source.SessionKey != "owner" || states[0].Source.Parser != src.Parser {
						return fmt.Errorf("conflict values not stored")
					}
					src.SessionKey = ""
					src.Parser = ""
					states, e = ensure(src)
					if e != nil {
						return e
					}
					if states[0].Source.SessionKey != "owner" || states[0].Source.Parser != "" {
						return fmt.Errorf("coalesce/parser semantics changed")
					}
					src.FileID.Ino++
					states, e = ensure(src)
					if e != nil {
						return e
					}
					if states[0].ID == a.ID {
						return fmt.Errorf("new file identity reused source")
					}

					if fail {
						// Corrupt only the load result, without changing retained message inputs.
						// UPSERT succeeds, then loadSource's extraction JSON decode fails.
						for _, w := range []*writeTx{fw, bw} {
							if _, e := w.exec(`UPDATE sources SET extraction_report='{' WHERE id=?`, a.ID); e != nil {
								return e
							}
						}
						before, e := projectionSnapshot(fw)
						if e != nil {
							return e
						}
						spec := a.Source
						spec.SessionKey = "must-rollback"
						spec.Parser = "must-rollback"
						if e := step(func(i int) writeReq {
							stores := []*Store{fast, full}
							var st SourceState
							return stores[i].ensureSourceRequest(ctx, spec, &st)
						}, true); e != nil {
							return e
						}
						if fw.changeCache != nil {
							return fmt.Errorf("failed load retained cache")
						}
						for _, w := range []*writeTx{fw, bw} {
							var key, parser string
							if e := w.tx.QueryRow(`SELECT coalesce(session_key,''),parser FROM sources WHERE id=?`, a.ID).Scan(&key, &parser); e != nil {
								return e
							}
							if key != "owner" || parser != "" {
								return fmt.Errorf("failed load escaped rollback")
							}
							if _, e := w.exec(`UPDATE sources SET extraction_report=NULL WHERE id=?`, a.ID); e != nil {
								return e
							}
						}
						after, e := projectionSnapshot(fw)
						if e != nil {
							return e
						}
						if !reflect.DeepEqual(before, after) {
							return fmt.Errorf("prior requests lost")
						}
					}
					if e := appendBatch(Batch{SourceID: a.ID, Generation: 1, Messages: pending[1:]}); e != nil {
						return e
					}
					if !fail && fw.changeCache[conv] != checkpoint {
						return fmt.Errorf("pending completion did not reuse checkpoint")
					}
					// A result from the new source cannot consume another old-source call.
					other := changePair("s", 2, "7654321")
					if e := appendBatch(Batch{SourceID: a.ID, Generation: 1, Messages: other[:1]}); e != nil {
						return e
					}
					if e := appendBatch(Batch{SourceID: states[0].ID, Generation: 1, Messages: other[1:]}); e != nil {
						return e
					}
					// Ordinary source generation writes still use the default invalidation policy.
					if e := step(func(int) writeReq {
						return writeReq{ctx: ctx, done: make(chan error, 1), fn: func(w *writeTx) error {
							_, e := w.exec(`UPDATE sources SET generation=generation+1 WHERE id=?`, a.ID)
							return e
						}}
					}, false); e != nil {
						return e
					}
					if fw.changeCache != nil {
						return fmt.Errorf("default source mutation preserved cache")
					}
					if e := step(func(i int) writeReq {
						stores := []*Store{fast, full}
						var st SourceState
						return stores[i].ensureSourceRequest(ctx, src, &st)
					}, false); e != nil {
						return e
					}
					if fw.changeCache != nil {
						return fmt.Errorf("EnsureSource seeded empty cache")
					}
					return nil
				})
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
