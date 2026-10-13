package ingest

import (
	"bytes"
	"testing"

	"github.com/flopwire/flopwire/internal/digest"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/jackc/pgx/v5"
)

func TestCommitEvidencePublicationUsesBoundedBackfill(t *testing.T) {
	e := newEnv(t)
	a := newAppendSession(t, e, 8)
	lines := a.toolCalls(a.next, 1, 0)
	lines = bytes.ReplaceAll(lines, []byte(`"command":"false"`), []byte(`"command":"git commit -m synthetic"`))
	lines = bytes.ReplaceAll(lines, []byte("exit status 1"), []byte("[main abcdef0] synthetic"))
	a.add(t, lines)
	var conv string
	if err := e.pool.QueryRow(e.ctx, `SELECT id::text FROM conversations WHERE session_id=$1`, a.session.SessionID).Scan(&conv); err != nil {
		t.Fatal(err)
	}
	if got := e.count(`SELECT count(*) FROM commit_evidence`); got != 0 {
		t.Fatal("parse checkpoint eagerly replayed commit history")
	}
	if got := e.count(`SELECT count(*) FROM commit_evidence_dirty WHERE conversation_id=$1`, conv); got != 1 {
		t.Fatal("completed parse lost indexing backlog")
	}
	if err := BackfillCommitEvidenceFor(e.ctx, e.pool, []string{conv}, 1); err != nil {
		t.Fatal(err)
	}
	if got := e.count(`SELECT count(*) FROM commit_evidence WHERE conversation_id=$1`, conv); got != 1 {
		t.Fatal("bounded backfill did not publish captured commit")
	}
	e.exec(`UPDATE messages SET text='[redacted]' WHERE conversation_id=$1 AND kind='tool_result'`, conv)
	if got := e.count(`SELECT count(*) FROM commit_evidence WHERE conversation_id=$1`, conv); got != 0 {
		t.Fatal("destructive update retained stale evidence until backfill")
	}
}

func TestCommitCompletionPreservesServerSummaryCounts(t *testing.T) {
	s := batchTestSink(t)
	ordinary := batchMessage("ordinary", 1, "synthetic prompt")
	if err := s.Message(ordinary); err != nil {
		t.Fatal(err)
	}
	if err := s.flush(); err != nil {
		t.Fatal(err)
	}
	completion := batchMessage("completion", 2, "synthetic output")
	completion.Kind = transcript.KindToolResult
	completion.ToolName = "shell"
	completion.ToolCallID = "completion"
	completion.IsError = true
	completion.Enrichment = map[string]any{"command_completion": true, "commands": []any{map[string]any{"cmd": "git commit -m synthetic", "exit_code": 1}}}
	if err := s.Message(completion); err != nil {
		t.Fatal(err)
	}
	if err := s.flush(); err != nil {
		t.Fatal(err)
	}
	conv := s.convIDs["batch-test"]
	assert := func() {
		t.Helper()
		var raw []byte
		if err := s.pool.QueryRow(s.ctx, `SELECT digest FROM conversation_activity WHERE conversation_id=$1`, conv).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		out := digest.Parse(raw)
		if out.Messages["user"] != 1 || out.Messages["tool_result"] != 0 || out.Failed != 0 {
			t.Fatalf("completion changed summary: %+v", out)
		}
	}
	assert()
	if err := pgx.BeginFunc(s.ctx, s.pool, func(tx pgx.Tx) error { return recountDigests(s.ctx, tx, []string{conv}) }); err != nil {
		t.Fatal(err)
	}
	assert()
	var rows int
	if err := s.pool.QueryRow(s.ctx, `SELECT count(*) FROM messages WHERE conversation_id=$1 AND NOT superseded`, conv).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatal("evidence-only completion was not retained")
	}
	// A completion that was already a summary row before evidence extraction
	// keeps its legacy message/tool/failure counts, on append and recount.
	legacy := batchMessage("legacy-completion", 3, "synthetic legacy output")
	legacy.Kind = transcript.KindToolResult
	legacy.ToolName = "shell"
	legacy.ToolCallID = "legacy"
	legacy.IsError = true
	legacy.Enrichment = map[string]any{"command_completion": true, "completion_legacy": true}
	if err := s.Message(legacy); err != nil {
		t.Fatal(err)
	}
	if err := s.flush(); err != nil {
		t.Fatal(err)
	}
	checkLegacy := func() {
		t.Helper()
		var raw []byte
		if err := s.pool.QueryRow(s.ctx, `SELECT digest FROM conversation_activity WHERE conversation_id=$1`, conv).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		out := digest.Parse(raw)
		if out.Messages["user"] != 1 || out.Messages["tool_result"] != 1 || out.Failed != 1 {
			t.Fatalf("legacy completion lost summary counts: %+v", out)
		}
	}
	checkLegacy()
	if err := pgx.BeginFunc(s.ctx, s.pool, func(tx pgx.Tx) error { return recountDigests(s.ctx, tx, []string{conv}) }); err != nil {
		t.Fatal(err)
	}
	checkLegacy()
}
