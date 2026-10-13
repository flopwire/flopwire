package local

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/transcript"
)

func TestChangeCursorRestartsAfterActualIndexRebuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := localindex.Open(path, localindex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	src, err := s.EnsureSource(ctx, transcript.Source{Agent: transcript.AgentClaude, Path: "/missing/cursor.jsonl", FileID: transcript.FileID{Dev: 1, Ino: 1}, StorageKind: transcript.StorageJSONLAppend, Parser: "claude@1"})
	if err != nil {
		t.Fatal(err)
	}
	hash := "abcdef0" + strings.Repeat("1", 33)
	var msgs []*transcript.Message
	for i := 0; i < 2; i++ {
		call := &transcript.Message{SessionID: "cursor-session", NativeID: fmt.Sprintf("c%d", i), Ordinal: int64(i * 2), Kind: transcript.KindToolCall, ToolName: "Bash", ToolCallID: fmt.Sprintf("t%d", i), ByteOffset: int64(i * 100)}
		call.SetText(`{"command":"git commit -m fix"}`, transcript.CapConfig{})
		result := &transcript.Message{SessionID: "cursor-session", NativeID: fmt.Sprintf("r%d", i), Ordinal: int64(i*2 + 1), Kind: transcript.KindToolResult, ToolName: "Bash", ToolCallID: call.ToolCallID, ByteOffset: int64(i*100 + 50)}
		result.SetText("[main abcdef0] fix", transcript.CapConfig{})
		msgs = append(msgs, call, result)
	}
	if _, err := s.ApplyBatch(ctx, localindex.Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "cursor-session", Cwd: "/repo"}}, Messages: msgs}); err != nil {
		t.Fatal(err)
	}
	b := &Backend{Store: s}
	q := format.ChangeQuery{Commits: []string{hash}, Limit: 1}
	f := format.Filters{Repo: "/repo"}
	page, err := b.Change(ctx, q, f)
	if err != nil || !page.HasMore || page.NextCursor == "" {
		t.Fatalf("initial pagination %+v %v", page, err)
	}
	q.Cursor = page.NextCursor
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := localindex.Open(path, localindex.Options{RebuildIndex: true})
	if err != nil {
		t.Fatal(err)
	}
	defer rebuilt.Close()
	page, err = (&Backend{Store: rebuilt}).Change(ctx, q, f)
	if err != nil || !page.RestartRequired || len(page.Matches) != 0 || page.NextCursor != "" {
		t.Fatalf("old cursor did not restart after secret rotation %+v %v", page, err)
	}
}
