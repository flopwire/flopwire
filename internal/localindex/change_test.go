package localindex

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/transcript"
)

func changePair(session string, i int, hash string) []*transcript.Message {
	call := msg(session, fmt.Sprintf("call-%d", i), int64(i*2+1), transcript.KindToolCall, `{"command":"git -C /repo commit -m fix"}`)
	call.ToolName = "Bash"
	call.ToolCallID = fmt.Sprintf("tool-%d", i)
	res := msg(session, fmt.Sprintf("result-%d", i), int64(i*2+2), transcript.KindToolResult, "[main "+hash+"] fix")
	res.ToolName = "Bash"
	res.ToolCallID = call.ToolCallID
	return []*transcript.Message{call, res}
}
func TestChangeProjectionAppendReplaceAndPurge(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/p/change.jsonl")
	h1 := "abcdef0" + strings.Repeat("1", 33)
	h2 := "1234567" + strings.Repeat("2", 33)
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "change", Cwd: "/repo"}}, Messages: changePair("change", 0, h1[:7])})
	rows, state, _, err := s.ChangeRecords(ctx, []string{h1, h2}, Filter{}, -1, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("initial %v %+v", err, rows)
	}
	first := rows[0].ID
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: changePair("change", 1, h2[:7])})
	rows, next, _, err := s.ChangeRecords(ctx, []string{h1, h2}, Filter{}, state.Cutoff, 0)
	if err != nil || len(rows) != 1 || rows[0].ID != first || state.Revision != next.Revision {
		t.Fatalf("append altered snapshot %v %+v %+v", err, rows, next)
	}
	rows, next, _, err = s.ChangeRecords(ctx, []string{h1, h2}, Filter{}, -1, 0)
	if err != nil || len(rows) != 2 {
		t.Fatalf("fresh snapshot %v %+v", err, rows)
	}
	// Same identity with a changed hash removes old evidence atomically.
	apply(t, s, Batch{SourceID: src.ID, Generation: 2, Messages: changePair("change", 0, h2[:7])})
	rows, replaced, _, err := s.ChangeRecords(ctx, []string{h1}, Filter{}, -1, 0)
	if err != nil || len(rows) != 0 || replaced.Revision <= next.Revision {
		t.Fatalf("replacement %v %+v %+v", err, rows, replaced)
	}
	if err := s.PurgeConversation(ctx, transcript.AgentClaude, "change"); err != nil {
		t.Fatal(err)
	}
	rows, purged, _, err := s.ChangeRecords(ctx, []string{h1, h2}, Filter{}, -1, 0)
	if err != nil || len(rows) != 0 || purged.Revision <= replaced.Revision {
		t.Fatalf("purge %v %+v %+v", err, rows, purged)
	}
}
func TestChangeProjectionNoSummaryCapAndHashIndex(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/p/many.jsonl")
	var msgs []*transcript.Message
	var commits []string
	for i := 0; i < 80; i++ {
		h := fmt.Sprintf("%040x", i+1)
		commits = append(commits, h)
		msgs = append(msgs, changePair("many", i, h)...)
	}
	start := time.Now()
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "many", Cwd: "/repo"}}, Messages: msgs})
	t.Logf("80 events ingest: %s", time.Since(start))
	rows, _, _, err := s.ChangeRecords(ctx, commits, Filter{}, -1, 0)
	if err != nil || len(rows) != 80 {
		t.Fatalf("lost observations %v count=%d", err, len(rows))
	}
	q, err := s.DB().Query(`EXPLAIN QUERY PLAN SELECT id FROM change_events WHERE observed_hash IN (SELECT value FROM json_each(?)) AND id>? AND id<=? ORDER BY id LIMIT 21`, jsonArray(commits), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	indexed := false
	for q.Next() {
		var id, parent, unused int
		var detail string
		if err := q.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		t.Log(detail)
		indexed = indexed || strings.Contains(detail, "change_events_hash")
	}
	if !indexed {
		t.Fatal("hash lookup does not use hash index")
	}
}
func TestChangeProjectionReplaysDelayedResultAndSupersede(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/p/delayed.jsonl")
	h := "abcdef0" + strings.Repeat("1", 33)
	pair := changePair("delayed", 0, h[:7])
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "delayed", Cwd: "/repo"}}, Messages: pair[:1]})
	rows, _, _, err := s.ChangeRecords(ctx, []string{h}, Filter{}, -1, 0)
	if err != nil || len(rows) != 0 {
		t.Fatalf("unresolved call %+v %v", rows, err)
	}
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: pair[1:]})
	rows, state, _, err := s.ChangeRecords(ctx, []string{h}, Filter{}, -1, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("delayed result %+v %v", rows, err)
	}
	if _, err := s.SupersedeSession(ctx, transcript.AgentClaude, "delayed", 2); err != nil {
		t.Fatal(err)
	}
	rows, next, _, err := s.ChangeRecords(ctx, []string{h}, Filter{}, -1, 0)
	if err != nil || len(rows) != 0 || next.Revision <= state.Revision {
		t.Fatalf("supersede %+v %v", rows, err)
	}
}
func TestChangeProjectionIrrelevantAppendKeepsEvents(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/p/large.jsonl")
	h := "abcdef0" + strings.Repeat("1", 33)
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "large", Cwd: "/repo"}}, Messages: changePair("large", 0, h[:7])})
	var msgs []*transcript.Message
	for i := 0; i < 1000; i++ {
		m := msg("large", fmt.Sprintf("large-%d", i), int64(i+10), transcript.KindToolResult, strings.Repeat("ordinary output line\n", 512))
		m.ToolCallID = fmt.Sprintf("ordinary-%d", i)
		msgs = append(msgs, m)
	}
	start := time.Now()
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: msgs})
	elapsed := time.Since(start)
	t.Logf("1000 tool results (~10MB), append without commit evidence: %s", elapsed)
	rows, _, _, err := s.ChangeRecords(ctx, []string{h}, Filter{}, -1, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("event lost %+v %v", rows, err)
	}
}

func TestChangeSchema13UpgradeBackfillsRetainedMissingSources(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/missing/retained.jsonl")
	h := "abcdef0" + strings.Repeat("1", 33)
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "retained", Cwd: "/repo"}}, Messages: changePair("retained", 0, h[:7])})
	rows, before, _, err := s.ChangeRecords(ctx, []string{h}, Filter{}, -1, 0)
	if err != nil || len(rows) != 1 {
		t.Fatal(err)
	}
	commandID := rows[0].CommandID
	path := s.path
	// Reconstruct a version-13 file without deleting any retained transcript rows.
	err = s.write(ctx, func(w *writeTx) error {
		for _, stmt := range []string{`DROP TRIGGER change_conversation_visibility`, `DROP TRIGGER change_message_generation`, `DROP TRIGGER change_placement_update`, `DROP TRIGGER change_placement_delete`, `DROP TRIGGER change_placement_insert`, `DROP TABLE change_events`, `DELETE FROM meta WHERE key IN ('change_cursor_secret','change_revision')`, `PRAGMA user_version=13`} {
			if _, err := w.exec(stmt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	rows, after, _, err := upgraded.ChangeRecords(ctx, []string{h}, Filter{}, -1, 0)
	if err != nil || len(rows) != 1 || rows[0].CommandID != commandID || before.IndexID != after.IndexID {
		t.Fatalf("retained evidence was lost or rebuilt: %v %+v %+v", err, rows, after)
	}
	if row, err := upgraded.Messages(ctx, []int64{commandID}); err != nil || len(row) != 1 || !strings.Contains(row[0].Text, "git -C") {
		t.Fatalf("retained command unavailable %+v %v", row, err)
	}
}
func TestChangePlacementAndConversationVisibilityInvalidates(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/p/moved.jsonl")
	h := "abcdef0" + strings.Repeat("1", 33)
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "moved", Cwd: "/repo/a"}}, Messages: changePair("moved", 0, h[:7])})
	before, err := s.ChangeState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "moved", Cwd: "/repo/b"}}})
	after, err := s.ChangeState(ctx)
	if err != nil || after.Revision <= before.Revision {
		t.Fatalf("conversation placement did not invalidate %+v %+v %v", before, after, err)
	}
	if err := s.SavePlacement(ctx, Placement{Agent: transcript.AgentClaude, SessionID: "moved", Placement: pathpolicy.Placement{Main: "/repo/main", Remote: "github.com/org/repo"}}); err != nil {
		t.Fatal(err)
	}
	placed, err := s.ChangeState(ctx)
	if err != nil || placed.Revision <= after.Revision {
		t.Fatalf("saved placement did not invalidate %+v %+v %v", after, placed, err)
	}
}

func TestChangeRedactionRemovesEvidenceAndCannotRevive(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/p/redacted-change.jsonl")
	h := "abcdef0" + strings.Repeat("1", 33)
	pair := changePair("redacted-change", 0, h[:7])
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "redacted-change", Cwd: "/repo"}}, Messages: pair})
	before, err := s.ChangeState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedactMessage(ctx, LocalRedaction{Session: "redacted-change", Ordinal: pair[1].Ordinal}); err != nil {
		t.Fatal(err)
	}
	rows, after, _, err := s.ChangeRecords(ctx, []string{h}, Filter{}, -1, 0)
	if err != nil || len(rows) != 0 || after.Revision <= before.Revision {
		t.Fatalf("redacted evidence leaked: %+v %v", rows, err)
	}
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: pair})
	rows, _, _, err = s.ChangeRecords(ctx, []string{h}, Filter{}, -1, 0)
	if err != nil || len(rows) != 0 {
		t.Fatalf("reingest revived redacted evidence: %+v %v", rows, err)
	}
}
func TestChangeDoesNotPairAcrossSourceGenerations(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/p/generations.jsonl")
	h := "abcdef0" + strings.Repeat("1", 33)
	pair := changePair("generations", 0, h[:7])
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "generations", Cwd: "/repo"}}, Messages: pair[:1]})
	apply(t, s, Batch{SourceID: src.ID, Generation: 2, Messages: pair[1:]})
	rows, _, _, err := s.ChangeRecords(ctx, []string{h}, Filter{}, -1, 0)
	if err != nil || len(rows) != 0 {
		t.Fatalf("mixed generation fabricated evidence %+v %v", rows, err)
	}
	apply(t, s, Batch{SourceID: src.ID, Generation: 2, Messages: pair})
	rows, _, _, err = s.ChangeRecords(ctx, []string{h}, Filter{}, -1, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("same generation evidence absent %+v %v", rows, err)
	}
}
