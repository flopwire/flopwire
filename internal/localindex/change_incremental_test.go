package localindex

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/digest"
	"github.com/flopwire/flopwire/internal/transcript"
)

func projectionSnapshot(w *writeTx) ([]string, error) {
	rows, err := w.tx.Query(`SELECT json_array(id,source_id,source_generation,conversation_id,event_key,observed_hash,command_id,result_id,reveal_command_id,reveal_result_id,evidence) FROM change_events ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, s)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	var revision, sequence int64
	if err = w.tx.QueryRow(`SELECT ifnull((SELECT CAST(value AS INTEGER) FROM meta WHERE key='change_revision'),0),ifnull((SELECT seq FROM sqlite_sequence WHERE name='change_events'),0)`).Scan(&revision, &sequence); err != nil {
		return nil, err
	}
	return append(out, fmt.Sprintf("revision:%d sequence:%d", revision, sequence)), nil
}

// Hold two real writer transactions and compare after every request, including
// savepoint failures. The control never retains incremental checkpoints.
func TestChangeIncrementalRequestDifferential(t *testing.T) {
	fast, full := openTest(t, DetailColumn), openTest(t, DetailColumn)
	a := source(t, fast, transcript.AgentClaude, "/p/incremental.jsonl")
	b := source(t, full, transcript.AgentClaude, "/p/incremental.jsonl")
	if a.ID != b.ID {
		t.Fatal("source IDs differ")
	}
	err := fast.write(ctx, func(fw *writeTx) error {
		return full.write(ctx, func(bw *writeTx) error {
			ft, bt := txRun{w: fw}, txRun{w: bw}
			run := func(fn func(*writeTx) error, policy projectionPolicy) error {
				for i, tt := range []*txRun{&ft, &bt} {
					done := make(chan error, 1)
					before := len(tt.waiting)
					requestPolicy := projectionInvalidate
					if i == 0 {
						requestPolicy = policy
					}
					tt.runOne(writeReq{ctx: ctx, fn: fn, done: done, projection: requestPolicy})
					if len(tt.waiting) == before {
						if got := <-done; got != nil && !errors.Is(got, errIncrementalInjected) {
							return got
						}
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
					return fmt.Errorf("projection mismatch\nfast %v\nfull %v", x, y)
				}
				return nil
			}
			applyOne := func(batch Batch) error {
				return run(func(w *writeTx) error { var r BatchResult; return w.applyBatch(&batch, &r) }, projectionAppend)
			}
			if e := applyOne(Batch{SourceID: a.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "refresh", Cwd: "/repo"}}, Messages: changePair("refresh", 0, "abcdef0")}); e != nil {
				return e
			}
			if len(fw.changeCache) != 1 {
				return errors.New("initial replay did not seed")
			}
			c := fw.changeCache[1]
			// Event keys are unique within a conversation. An identical key in an
			// unrelated conversation must neither scan history nor force fallback.
			if e := applyOne(Batch{SourceID: a.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "other"}}, Messages: changePair("other", 1, "1234567")}); e != nil {
				return e
			}
			// Pending old commands resolve to exact old physical IDs on a later suffix.
			readSource := func(w *writeTx) error {
				if w.changeAppend {
					return errors.New("source read enabled append or seeding")
				}
				_, err := loadSource(w.ctx, w.tx, `WHERE id = ?`, a.ID)
				return err
			}
			if e := run(readSource, projectionRead); e != nil {
				return e
			}
			if fw.changeCache[1] != c {
				return errors.New("successful source read discarded checkpoint")
			}
			pending := changePair("refresh", 1, "1234567")
			if e := applyOne(Batch{SourceID: a.ID, Generation: 1, Messages: pending[:1]}); e != nil {
				return e
			}
			if fw.changeCache[1] != c {
				return errors.New("append unexpectedly replayed")
			}
			if e := applyOne(Batch{SourceID: a.ID, Generation: 1, Messages: pending[1:]}); e != nil {
				return e
			}
			if fw.changeCache[1] != c {
				return errors.New("completion unexpectedly replayed")
			}
			// Failed reads discard tentative state. A subsequent successful read
			// cannot seed checkpoints, and an unclassified write still invalidates.
			if e := run(func(w *writeTx) error {
				if e := readSource(w); e != nil {
					return e
				}
				return errIncrementalInjected
			}, projectionRead); e != nil {
				return e
			}
			if fw.changeCache != nil {
				return errors.New("failed source read retained checkpoint")
			}
			if e := run(readSource, projectionRead); e != nil {
				return e
			}
			if fw.changeCache != nil {
				return errors.New("source read seeded checkpoint")
			}
			if e := applyOne(Batch{SourceID: a.ID, Generation: 1, Messages: pending[:1]}); e != nil {
				return e
			}
			if fw.changeCache[1] == nil {
				return errors.New("append failed to reseed after read failure")
			}
			if e := run(func(w *writeTx) error { _, e := w.exec(`UPDATE sources SET path=path WHERE id=?`, a.ID); return e }, projectionInvalidate); e != nil {
				return e
			}
			if fw.changeCache != nil {
				return errors.New("generic write retained checkpoint")
			}
			if e := applyOne(Batch{SourceID: a.ID, Generation: 1, Messages: pending[:1]}); e != nil {
				return e
			}
			c = fw.changeCache[1]
			// Call enrichment alone and incoming text with unchanged SHA do not alter
			// the stored extractor input. Both must preserve a real checkpoint hit.
			pending[0].Enrichment = map[string]any{"commands": []string{"git reset --hard"}}
			pending[0].Text = "incoming ignored text"
			if e := applyOne(Batch{SourceID: a.ID, Generation: 1, Messages: pending[:1]}); e != nil {
				return e
			}
			if fw.changeCache[1] != c {
				return errors.New("irrelevant effective call touch replayed")
			}
			// A changed full-content SHA with identical extracted text still takes
			// refresh(nil), preserving the exact stored text and checkpoint.
			pending[0].Text = changePair("refresh", 1, "1234567")[0].Text
			pending[0].ContentSHA = sha256.Sum256([]byte("different full content, identical extracted text"))
			if e := applyOne(Batch{SourceID: a.ID, Generation: 1, Messages: pending[:1]}); e != nil {
				return e
			}
			if fw.changeCache[1] != c {
				return errors.New("identical stored text unexpectedly replayed")
			}
			// Prefix growth writes text in place and must discard the old prefix.
			pending[1].Text += "\nadditional output"
			pending[1].ContentSHA = sha256.Sum256([]byte(pending[1].Text))
			if e := applyOne(Batch{SourceID: a.ID, Generation: 1, Messages: pending[1:]}); e != nil {
				return e
			}
			if fw.changeCache[1] == c {
				return errors.New("stored text growth retained checkpoint")
			}
			c = fw.changeCache[1]
			// Replacement versions retire an old physical reference before inserting
			// a new row, even if all metadata fields would otherwise compare equal.
			pending[1].Text = "[main 1234567] rewritten output"
			pending[1].ContentSHA = sha256.Sum256([]byte(pending[1].Text))
			if e := applyOne(Batch{SourceID: a.ID, Generation: 1, Messages: pending[1:]}); e != nil {
				return e
			}
			if fw.changeCache[1] == c {
				return errors.New("replacement retained checkpoint")
			}
			c = fw.changeCache[1]
			// Relevant result enrichment invalidates rather than reusing the prefix.
			pending[1].Enrichment = map[string]any{"exit_code": 1}
			if e := applyOne(Batch{SourceID: a.ID, Generation: 1, Messages: pending[1:]}); e != nil {
				return e
			}
			if fw.changeCache[1] == c {
				return errors.New("result enrichment retained stale checkpoint")
			}
			// A projection failure remains inside this request's savepoint. The first
			// successful requests survive, and the tentative checkpoint is discarded.
			if e := run(func(w *writeTx) error {
				_, e := w.exec(`CREATE TEMP TRIGGER fail_incremental BEFORE INSERT ON change_events WHEN NEW.observed_hash='badcafe' BEGIN SELECT RAISE(ABORT,'injected'); END`)
				return e
			}, projectionInvalidate); e != nil {
				return e
			}
			if e := applyOne(Batch{SourceID: a.ID, Generation: 1, Messages: changePair("refresh", 0, "abcdef0")[:1]}); e != nil {
				return e
			}
			if len(fw.changeCache) == 0 {
				return errors.New("failure control did not seed cache")
			}
			failed := changePair("refresh", 2, "badcafe")
			if e := run(func(w *writeTx) error {
				var r BatchResult
				e := w.applyBatch(&Batch{SourceID: a.ID, Generation: 1, Messages: failed}, &r)
				if e == nil {
					return errors.New("injection did not fail")
				}
				return errIncrementalInjected
			}, projectionAppend); e != nil {
				return e
			}
			if fw.changeCache != nil {
				return errors.New("failed request retained cache")
			}
			if e := run(func(w *writeTx) error { _, e := w.exec(`DROP TRIGGER fail_incremental`); return e }, projectionInvalidate); e != nil {
				return e
			}
			if e := applyOne(Batch{SourceID: a.ID, Generation: 1, Messages: changePair("refresh", 3, "7654321")}); e != nil {
				return e
			}
			// RELEASE can fail after the callback changed state. Drop the tentative
			// cache even when the callback itself succeeded and SQLite retained rows.
			for i, tt := range []*txRun{&ft, &bt} {
				done := make(chan error, 1)
				policy := projectionInvalidate
				if i == 0 {
					policy = projectionAppend
				}
				tt.runOne(writeReq{ctx: ctx, projection: policy, done: done, fn: func(w *writeTx) error {
					var r BatchResult
					if e := w.applyBatch(&Batch{SourceID: a.ID, Generation: 1, Messages: changePair("refresh", 30, "c0ffee0")}, &r); e != nil {
						return e
					}
					_, e := w.tx.Exec("RELEASE r" + itoa(int64(tt.n)))
					return e
				}})
				if e := <-done; e == nil || !strings.Contains(e.Error(), "no such savepoint") {
					return fmt.Errorf("unexpected release control error: %v", e)
				}
			}
			if fw.changeCache != nil {
				return errors.New("RELEASE failure retained cache")
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
				return errors.New("release failure changed projection")
			}
			// Late earlier-ordinal calls must replay. Their arrival can unlock evidence
			// in an older result, which an append-only state cannot discover.
			late := changePair("refresh", 4, "fedcba0")
			if e := applyOne(Batch{SourceID: a.ID, Generation: 1, Messages: late[1:]}); e != nil {
				return e
			}
			c = fw.changeCache[1]
			if e := applyOne(Batch{SourceID: a.ID, Generation: 1, Messages: late[:1]}); e != nil {
				return e
			}
			if fw.changeCache[1] == c {
				return errors.New("late call did not replay")
			}
			// Physical evidence references and generation stamps are effective inputs.
			late[0].LineNo = 91
			late[0].ByteOffset = 123
			if e := applyOne(Batch{SourceID: a.ID, Generation: 1, Messages: late[:1]}); e != nil {
				return e
			}
			if e := applyOne(Batch{SourceID: a.ID, Generation: 2, Messages: late}); e != nil {
				return e
			}
			// Every unclassified write path drops cache before it can alter rows.
			if e := run(func(w *writeTx) error {
				_, e := w.exec(`UPDATE messages SET on_active_path=0 WHERE native_id=?`, late[1].NativeID)
				if e != nil {
					return e
				}
				return w.refreshChangeEvents(1)
			}, projectionInvalidate); e != nil {
				return e
			}
			if fw.changeCache != nil {
				return errors.New("non-Apply request retained cache")
			}
			// The fallback identities include part, for both locators and byte offsets.
			for j, locator := range []string{"same-locator", ""} {
				pair := changePair("refresh", 10+j, "aabbccd")
				for k, m := range pair {
					m.NativeID = ""
					m.Locator = locator
					m.ByteOffset = 900
					m.Part = k + 1
				}
				if e := applyOne(Batch{SourceID: a.ID, Generation: 2, Messages: pair[:1]}); e != nil {
					return e
				}
				if e := applyOne(Batch{SourceID: a.ID, Generation: 2, Messages: pair[1:]}); e != nil {
					return e
				}
			}
			// A deleted conversation must not seed an empty checkpoint. Message-only
			// batches can revive rows while hidden; clearing the deletion must replay.
			if e := run(func(w *writeTx) error {
				_, e := w.exec(`UPDATE conversations SET deleted_in_generation=3 WHERE id=1`)
				if e != nil {
					return e
				}
				_, e = w.exec(`UPDATE messages SET superseded=1 WHERE conversation_id=1`)
				if e != nil {
					return e
				}
				return w.refreshChangeEvents(1)
			}, projectionInvalidate); e != nil {
				return e
			}
			revive := changePair("refresh", 15, "ddeeff0")
			if e := applyOne(Batch{SourceID: a.ID, Generation: 3, Messages: revive}); e != nil {
				return e
			}
			if fw.changeCache != nil {
				return errors.New("deleted conversation seeded cache")
			}
			if e := applyOne(Batch{SourceID: a.ID, Generation: 3, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "refresh"}}, Messages: changePair("refresh", 16, "feedabc")}); e != nil {
				return e
			}

			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

var errIncrementalInjected = errors.New("injected projection request failure")

func TestChangeIncrementalBudgetFallsBack(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/p/budget.jsonl")
	err := s.write(ctx, func(w *writeTx) error {
		w.changeAppend = true
		var res BatchResult
		if err := w.applyBatch(&Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "refresh"}}, Messages: changePair("refresh", 0, "abcdef0")}, &res); err != nil {
			return err
		}
		if len(w.changeCache) == 0 {
			return errors.New("no seed")
		}
		// Oversized compact pending identity/directory state cannot be retained.
		huge := changePair("refresh", 1, "1234567")[0]
		huge.ToolCallID = strings.Repeat("x", int(changeCacheBudget))
		if err := w.applyBatch(&Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{huge}}, &res); err != nil {
			return err
		}
		if w.changeCache != nil {
			return errors.New("oversized state retained")
		}
		before, err := projectionSnapshot(w)
		if err != nil {
			return err
		}
		w.changeAppend = false
		if err = w.replayChangeEvents(1); err != nil {
			return err
		}
		after, err := projectionSnapshot(w)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(before, after) {
			return errors.New("budget fallback differs from full replay")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestChangeRefLookupUsesUniqueHeadIndexes(t *testing.T) {
	s := openTest(t, DetailColumn)
	for _, ref := range []digest.EvidenceRef{{NativeID: "native", Part: 2}, {Locator: "locator", Part: 2}, {ByteOffset: 12, Part: 2}} {
		pred, args := changeRefPredicate(ref)
		args = append([]any{int64(1), int64(1), int64(1)}, args...)
		rows, err := s.DB().Query(`EXPLAIN QUERY PLAN SELECT id FROM messages INDEXED BY `+changeRefIndex(ref)+` WHERE conversation_id=? AND source_id=? AND source_generation=? AND superseded=0 AND on_active_path IS NOT 0 AND kind IN ('tool_call','tool_result') AND `+pred+` ORDER BY ordinal DESC,id DESC LIMIT 1`, args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var a, b, c int
			var detail string
			if err = rows.Scan(&a, &b, &c, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(plan, "\n")
		if !strings.Contains(joined, "SEARCH messages USING INDEX "+changeRefIndex(ref)) || strings.Contains(joined, "SCAN messages") {
			t.Fatalf("unbounded identity plan: %s", joined)
		}
		t.Logf("%s: %s", changeRefIndex(ref), joined)
	}
}

func TestChangeIncrementalLegacyRowsUseReplay(t *testing.T) {
	for _, mutation := range []string{`native_id=''`, `locator=''`, `superseded_by=999`, `native_id=NULL,locator=NULL,byte_offset=NULL`} {
		t.Run(mutation, func(t *testing.T) {
			s := openTest(t, DetailColumn)
			src := source(t, s, transcript.AgentClaude, "/p/legacy.jsonl")
			if err := s.write(ctx, func(w *writeTx) error {
				w.changeAppend = true
				var res BatchResult
				if err := w.applyBatch(&Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "refresh"}}, Messages: changePair("refresh", 0, "abcdef0")}, &res); err != nil {
					return err
				}
				if _, err := w.exec(`UPDATE messages SET ` + mutation + ` WHERE kind='tool_result'`); err != nil {
					return err
				}
				if err := w.replayChangeEvents(1); err != nil {
					return err
				}
				if len(w.changeCache) != 0 {
					return errors.New("noncanonical legacy row seeded cache")
				}
				before, err := projectionSnapshot(w)
				if err != nil {
					return err
				}
				w.changeAppend = false
				if err = w.replayChangeEvents(1); err != nil {
					return err
				}
				after, err := projectionSnapshot(w)
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(before, after) {
					return errors.New("legacy fallback differs from retained replay")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestChangeEventKeyLookupUsesConversationIndex(t *testing.T) {
	s := openTest(t, DetailColumn)
	rows, err := s.DB().Query(`EXPLAIN QUERY PLAN SELECT EXISTS(SELECT 1 FROM change_events WHERE conversation_id=? AND event_key=?)`, 1, "key")
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var a, b, c int
		var detail string
		if err = rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "SEARCH change_events USING COVERING INDEX") || !strings.Contains(joined, "conversation_id=? AND event_key=?") || strings.Contains(joined, "SCAN change_events") {
		t.Fatalf("unbounded event identity lookup: %s", joined)
	}
	t.Log(joined)
}

func TestFreshMessageIdentityAddressIsCanonical(t *testing.T) {
	for _, m := range []transcript.Message{{}, {ByteOffset: 17}, {Parser: "sqlite"}, {Parser: "jsonl", ByteOffset: 0}} {
		if got := jsonlInt(&m, m.ByteOffset); got == nil {
			t.Fatalf("missing locator stored NULL byte identity: %+v", m)
		}
	}
	m := transcript.Message{Locator: "rowid:1"}
	if got := jsonlInt(&m, 0); got != nil {
		t.Fatalf("locator-only row expected NULL byte address: %v", got)
	}
}

func TestChangeFreshRowsBatchUsesPrimaryKeys(t *testing.T) {
	s := openTest(t, DetailColumn)
	rows, err := s.DB().Query(`EXPLAIN QUERY PLAN `+changeFreshRowsSQL, int64JSON([]int64{3, 1, 9}), 1)
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var a, b, c int
		var detail string
		if err = rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "SEARCH messages USING INTEGER PRIMARY KEY (rowid=?)") || strings.Contains(joined, "SCAN messages") {
		t.Fatalf("fresh-ID batch scans retained history: %s", joined)
	}
	t.Log(joined)
}

func TestChangeFreshTrackingIncludesOnlyToolRows(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/p/fresh-tools.jsonl")
	if err := s.write(ctx, func(w *writeTx) error {
		w.changeAppend = true
		var res BatchResult
		if err := w.applyBatch(&Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "refresh"}}, Messages: changePair("refresh", 0, "abcdef0")}, &res); err != nil {
			return err
		}
		if w.changeCache[1] == nil {
			return errors.New("no checkpoint")
		}
		batch := Batch{SourceID: src.ID, Generation: 1}
		ordinary := msg("refresh", "ordinary", 99, transcript.KindAssistant, "ordinary text")
		pair := changePair("refresh", 1, "1234567")
		for _, m := range []*transcript.Message{ordinary, pair[0], pair[1]} {
			prepared := prepareAll([]*transcript.Message{m})
			if _, err := w.insertMessage(&batch, 1, m, &prepared[0], 1); err != nil {
				return err
			}
		}
		if len(w.changeAdded[1]) != 2 {
			return fmt.Errorf("tracked %d IDs, want only two tool rows", len(w.changeAdded[1]))
		}
		rows, err := w.readFreshChangeRows(1, w.changeAdded[1])
		if err != nil {
			return err
		}
		if len(rows) != 2 {
			return fmt.Errorf("batch read %d rows, want two", len(rows))
		}
		for _, r := range rows {
			if r.m.Kind != transcript.KindToolCall && r.m.Kind != transcript.KindToolResult {
				return errors.New("ordinary row entered suffix")
			}
		}
		checkpoint := w.changeCache[1]
		if err := w.refreshChangeEvents(1); err != nil {
			return err
		}
		if w.changeCache[1] != checkpoint {
			return errors.New("tool suffix unexpectedly replayed")
		}
		before, err := projectionSnapshot(w)
		if err != nil {
			return err
		}
		w.changeAppend = false
		if err := w.replayChangeEvents(1); err != nil {
			return err
		}
		after, err := projectionSnapshot(w)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(before, after) {
			return errors.New("batched suffix differs from full replay")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
