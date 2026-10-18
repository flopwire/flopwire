package localindex

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/transcript"
)

func placementRows(w *writeTx, clock bool) ([]string, error) {
	columns := `agent,session_id,cwd,worktree_root,main_root,remote,how,checked_at,candidates,other_cwds,withhold`
	if clock {
		columns += `,resolved_at`
	}
	rows, err := w.tx.Query(`SELECT json_array(` + columns + `) FROM placements ORDER BY agent,session_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Use the exact request builder sent by public SavePlacement. The control
// invalidates checkpoints and performs full replay on every following append.
func TestPlacementCheckpointRequestDifferential(t *testing.T) {
	for _, failure := range []string{"none", "placement", "revision", "release"} {
		t.Run(failure, func(t *testing.T) {
			fast, full := openTest(t, DetailColumn), openTest(t, DetailColumn)
			a := source(t, fast, transcript.AgentClaude, "/p/placement.jsonl")
			b := source(t, full, transcript.AgentClaude, "/p/placement.jsonl")
			if a.ID != b.ID {
				t.Fatal("source IDs differ")
			}
			err := fast.write(ctx, func(fw *writeTx) error {
				return full.write(ctx, func(bw *writeTx) error {
					ft, bt := txRun{w: fw}, txRun{w: bw}
					step := func(name string, build func() writeReq, wantErr string) error {
						for i, tt := range []*txRun{&ft, &bt} {
							before, err := projectionSnapshot(tt.w)
							if err != nil {
								return err
							}
							placed, err := placementRows(tt.w, true)
							if err != nil {
								return err
							}
							req := build()
							if i == 1 {
								req.projection = projectionInvalidate
							}
							waiting := len(tt.waiting)
							tt.runOne(req)
							if len(tt.waiting) == waiting {
								got := <-req.done
								if wantErr == "" || got == nil || !strings.Contains(got.Error(), wantErr) {
									return fmt.Errorf("%s unexpected request error: %v", name, got)
								}
								// RELEASE failure follows existing SQLite semantics: the callback's
								// explicit RELEASE has already retained its rows. Other failures roll back.
								if failure != "release" {
									after, err := projectionSnapshot(tt.w)
									if err != nil {
										return err
									}
									afterPlacement, err := placementRows(tt.w, true)
									if err != nil {
										return err
									}
									if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(placed, afterPlacement) {
										return fmt.Errorf("%s escaped request rollback", name)
									}
								}
							} else if wantErr != "" {
								return fmt.Errorf("%s unexpectedly succeeded", name)
							}
						}
						x, err := projectionSnapshot(fw)
						if err != nil {
							return err
						}
						y, err := projectionSnapshot(bw)
						if err != nil {
							return err
						}
						xp, err := placementRows(fw, false)
						if err != nil {
							return err
						}
						yp, err := placementRows(bw, false)
						if err != nil {
							return err
						}
						if !reflect.DeepEqual(x, y) || !reflect.DeepEqual(xp, yp) {
							return fmt.Errorf("%s differs from full replay: fast=%v control=%v placements=%v/%v", name, x, y, xp, yp)
						}
						return nil
					}
					appendBatch := func(batch Batch) error {
						return step("append", func() writeReq {
							return writeReq{ctx: ctx, done: make(chan error, 1), projection: projectionAppend, fn: func(w *writeTx) error { var r BatchResult; return w.applyBatch(&batch, &r) }}
						}, "")
					}
					plain := func(sql string) error {
						return step("unclassified write", func() writeReq {
							return writeReq{ctx: ctx, done: make(chan error, 1), fn: func(w *writeTx) error { _, err := w.exec(sql); return err }}
						}, "")
					}
					if failure == "placement" {
						if e := plain(`CREATE TEMP TRIGGER fail_placement BEFORE UPDATE ON placements BEGIN SELECT RAISE(ABORT,'placement failure'); END`); e != nil {
							return e
						}
					}
					if failure == "revision" {
						if e := plain(`CREATE TEMP TRIGGER fail_revision BEFORE UPDATE ON meta WHEN NEW.key='change_revision' BEGIN SELECT RAISE(ABORT,'revision failure'); END`); e != nil {
							return e
						}
					}
					pending := changePair("child", 1, "1234567")
					seed := append(changePair("parent", 0, "abcdef0"), changePair("child", 0, "abcdef0")...)
					seed = append(seed, pending[0])
					if e := appendBatch(Batch{SourceID: a.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "parent", Cwd: "/repo"}, {Agent: transcript.AgentClaude, SessionID: "child", ParentSessionID: "parent", Cwd: "/repo"}}, Messages: seed}); e != nil {
						return e
					}
					var child int64
					if e := fw.tx.QueryRow(`SELECT id FROM conversations WHERE session_id='child'`).Scan(&child); e != nil {
						return e
					}
					checkpoint := fw.changeCache[child]
					if checkpoint == nil {
						return fmt.Errorf("seed missing child checkpoint")
					}
					place := Placement{Agent: transcript.AgentClaude, SessionID: "parent", Placement: pathpolicy.Placement{Cwd: "/repo", Main: "/repo/main-a", Remote: "host/org/a", Worktree: "/repo/work-a"}, How: PlacedByWorktree}
					save := func(p Placement, wantErr string) error {
						return step("SavePlacement", func() writeReq {
							req := savePlacementRequest(ctx, p)
							if failure == "release" && wantErr != "" {
								fn := req.fn
								req.fn = func(w *writeTx) error {
									if err := fn(w); err != nil {
										return err
									}
									_, err := w.tx.Exec("RELEASE r" + itoa(int64(ft.n)))
									return err
								}
							}
							return req
						}, wantErr)
					}
					if e := save(place, ""); e != nil {
						return e
					}
					if fw.changeAppend {
						return fmt.Errorf("placement write enabled extraction")
					}
					if fw.changeCache[child] != checkpoint {
						return fmt.Errorf("placement INSERT discarded checkpoint")
					}
					if failure == "none" {
						if e := appendBatch(Batch{SourceID: a.ID, Generation: 1, Messages: pending[1:]}); e != nil {
							return e
						}
						if fw.changeCache[child] != checkpoint {
							return fmt.Errorf("old pending completion did not hit checkpoint")
						}
						var oldCommand, storedCommand int64
						if e := fw.tx.QueryRow(`SELECT id FROM messages WHERE conversation_id=? AND native_id=?`, child, pending[0].NativeID).Scan(&oldCommand); e != nil {
							return e
						}
						if e := fw.tx.QueryRow(`SELECT command_id FROM change_events WHERE conversation_id=? AND observed_hash='1234567'`, child).Scan(&storedCommand); e != nil {
							return e
						}
						if oldCommand != storedCommand {
							return fmt.Errorf("pending command physical ID changed")
						}
						place.Main = "/repo/main-b"
						place.Remote = "host/org/b"
						place.Worktree = "/repo/work-b"
						if e := save(place, ""); e != nil {
							return e
						}
						if fw.changeCache[child] != checkpoint {
							return fmt.Errorf("placement UPDATE discarded checkpoint")
						}
						var revision int64
						if e := fw.tx.QueryRow(`SELECT value FROM meta WHERE key='change_revision'`).Scan(&revision); e != nil {
							return e
						}
						place.How = PlacedByRemote
						place.CheckedAt = 42
						place.Candidates = "candidate"
						place.OtherCwds = "other"
						place.Cwd = "/other"
						if e := save(place, ""); e != nil {
							return e
						}
						var unchanged int64
						if e := fw.tx.QueryRow(`SELECT value FROM meta WHERE key='change_revision'`).Scan(&unchanged); e != nil {
							return e
						}
						if unchanged != revision {
							return fmt.Errorf("unwatched placement fields changed revision")
						}
					} else {
						place.Main = "/repo/main-b"
						expected := failure + " failure"
						if failure == "release" {
							expected = "no such savepoint"
						}
						if e := save(place, expected); e != nil {
							return e
						}
						if fw.changeCache != nil {
							return fmt.Errorf("failed placement request retained checkpoints")
						}
						if failure == "placement" {
							if e := plain(`DROP TRIGGER fail_placement`); e != nil {
								return e
							}
						}
						if failure == "revision" {
							if e := plain(`DROP TRIGGER fail_revision`); e != nil {
								return e
							}
						}
						if e := appendBatch(Batch{SourceID: a.ID, Generation: 1, Messages: pending[1:]}); e != nil {
							return e
						}
						if fw.changeCache[child] == nil {
							return fmt.Errorf("append did not safely reseed")
						}
					}
					// Unknown/default callbacks still discard. A placement write with no
					// checkpoint must not seed one, even when it changes public revision.
					for _, policy := range []projectionPolicy{projectionInvalidate, projectionPolicy(255)} {
						if e := step("default scope", func() writeReq {
							return writeReq{ctx: ctx, done: make(chan error, 1), projection: policy, fn: func(w *writeTx) error { _, e := w.exec(`UPDATE placements SET withhold='local'`); return e }}
						}, ""); e != nil {
							return e
						}
						if fw.changeCache != nil {
							return fmt.Errorf("default/unknown policy preserved checkpoint")
						}
						if e := save(place, ""); e != nil {
							return e
						}
						if fw.changeCache != nil {
							return fmt.Errorf("placement request seeded checkpoint")
						}
						if e := appendBatch(Batch{SourceID: a.ID, Generation: 1, Messages: pending[1:]}); e != nil {
							return e
						}
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
