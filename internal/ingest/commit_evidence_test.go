package ingest

import (
	"testing"

	"github.com/flopwire/flopwire/internal/digest"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/jackc/pgx/v5"
)

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
}
