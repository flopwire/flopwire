package localindex

import (
	"crypto/sha256"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/transcript"
)

func resultEligibility(t *testing.T, s *Store, id string) int {
	t.Helper()
	var got int
	if err := s.DB().QueryRow(`SELECT change_result_eligible FROM messages WHERE native_id=? AND superseded_by IS NULL`, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestChangeEligibilityStoredTextLifecycle(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/p/eligibility.jsonl")
	pair := changePair("refresh", 0, "abcdef0")
	// Classify calls too: a later metadata-only kind change must use stored text.
	m := msg("refresh", "lifecycle", 3, transcript.KindAssistant, "ordinary output")
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "refresh", Cwd: "/repo"}}, Messages: []*transcript.Message{m}})
	assertChangeReplay(t, s, 0)
	if got := resultEligibility(t, s, m.NativeID); got != 0 {
		t.Fatal(got)
	}
	// Same SHA keeps the old stored text even if the incoming extraction differs.
	m.Kind = transcript.KindToolResult
	m.Text = "abcdef0"
	m.ToolCallID = "lifecycle"
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{m}})
	assertChangeReplay(t, s, 0)
	if got := resultEligibility(t, s, m.NativeID); got != 0 {
		t.Fatal("touch classified incoming rather than stored text")
	}
	// A real text rewrite updates eligibility; a prefix growth must do so too.
	m.Text = "ordinary output\nabcdef0"
	m.ContentSHA = sha256.Sum256([]byte(m.Text))
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{m}})
	assertChangeReplay(t, s, 0)
	if got := resultEligibility(t, s, m.NativeID); got != 1 {
		t.Fatal("growth lost eligibility")
	}
	m.Text = "safe replacement"
	m.ContentSHA = sha256.Sum256([]byte(m.Text))
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{m}})
	assertChangeReplay(t, s, 0)
	if got := resultEligibility(t, s, m.NativeID); got != 0 {
		t.Fatal("version kept old eligibility")
	}
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: pair})
	assertChangeReplay(t, s, 1)
	if _, err := s.RedactMessage(ctx, LocalRedaction{Session: "refresh", Ordinal: pair[1].Ordinal}); err != nil {
		t.Fatal(err)
	}
	assertChangeReplay(t, s, 0)
	if got := resultEligibility(t, s, pair[1].NativeID); got != 0 {
		t.Fatal("redaction retained eligibility")
	}
}

func TestChangeEligibilityUnknownAndRollback(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/p/unknown.jsonl")
	pair := changePair("refresh", 0, "abcdef0")
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "refresh", Cwd: "/repo"}}, Messages: pair})
	assertChangeReplay(t, s, 1)
	// Mimic a retained old-schema result: unknown must force replay and classify
	// stored output, even if there are no existing events.
	if err := s.write(ctx, func(w *writeTx) error {
		_, err := w.exec(`DELETE FROM change_events`)
		if err != nil {
			return err
		}
		_, err = w.exec(`UPDATE messages SET change_result_eligible=NULL`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("rollback")
	if err := s.write(ctx, func(w *writeTx) error {
		var conv int64
		if err := w.tx.QueryRow(`SELECT id FROM conversations WHERE session_id='refresh'`).Scan(&conv); err != nil {
			return err
		}
		if err := w.refreshChangeEvents(conv); err != nil {
			return err
		}
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	var unknown int
	if err := s.DB().QueryRow(`SELECT count(*) FROM messages WHERE change_result_eligible IS NULL`).Scan(&unknown); err != nil || unknown != 2 {
		t.Fatalf("classification escaped rollback: %d %v", unknown, err)
	}
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: pair[:1]})
	assertChangeReplay(t, s, 1)
	if got := resultEligibility(t, s, pair[1].NativeID); got != 1 {
		t.Fatal("unknown result was not classified")
	}
}

func TestChangeEligibilityGuardUsesIndex(t *testing.T) {
	s := openTest(t, DetailColumn)
	rows, err := s.DB().Query(`EXPLAIN QUERY PLAN SELECT EXISTS(SELECT 1 FROM messages WHERE conversation_id=? AND kind='tool_result' AND superseded=0 AND on_active_path IS NOT 0 AND change_result_eligible IS NOT 0)`, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var a, b, c int
		var detail string
		if err := rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range details {
		found = found || strings.Contains(d, "messages_change_results")
	}
	if !found {
		t.Fatalf("guard index absent: %v", details)
	}
}

func TestChangeSchema14UpgradePreservesRetainedRows(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/missing/schema14.jsonl")
	pair := changePair("refresh", 0, "abcdef0")
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "refresh", Cwd: "/repo"}}, Messages: pair})
	if err := s.SavePlacement(ctx, Placement{Agent: transcript.AgentClaude, SessionID: "refresh", Placement: pathpolicy.Placement{Main: "/missing/main", Remote: "github.com/org/repo"}}); err != nil {
		t.Fatal(err)
	}
	var placement string
	if err := s.DB().QueryRow(`SELECT json_array(agent,session_id,main_root,remote) FROM placements WHERE session_id='refresh'`).Scan(&placement); err != nil {
		t.Fatal(err)
	}
	before, err := s.ChangeState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	rows, err := s.DB().Query(`SELECT id FROM messages ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	// Build a true old-schema file and stale projection, as produced by issue330.
	if err := s.write(ctx, func(w *writeTx) error {
		for _, q := range []string{`DROP INDEX messages_change_results`, `ALTER TABLE messages DROP COLUMN change_result_eligible`, `UPDATE messages SET line_no=99 WHERE kind='tool_call'`, `PRAGMA user_version=14`} {
			if _, err := w.exec(q); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	path := s.path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	assertChangeReplay(t, upgraded, 1)
	var retainedPlacement string
	if err := upgraded.DB().QueryRow(`SELECT json_array(agent,session_id,main_root,remote) FROM placements WHERE session_id='refresh'`).Scan(&retainedPlacement); err != nil {
		t.Fatal(err)
	}
	if placement != retainedPlacement {
		t.Fatal("placement lost on additive migration")
	}

	after, err := upgraded.ChangeState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.IndexID != after.IndexID || after.Revision <= before.Revision {
		t.Fatalf("upgrade rebuilt or failed to repair stale events: %+v %+v", before, after)
	}
	var gotIDs []int64
	rows, err = upgraded.DB().Query(`SELECT id FROM messages ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		gotIDs = append(gotIDs, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if !reflect.DeepEqual(ids, gotIDs) {
		t.Fatal("retained row IDs changed")
	}
	if got := resultEligibility(t, upgraded, pair[1].NativeID); got != 1 {
		t.Fatal("migration left old result unknown")
	}
}
