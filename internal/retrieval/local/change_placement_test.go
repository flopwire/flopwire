package local

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/transcript"
)

func TestChangePlacementCursorAndParentVisibility(t *testing.T) {
	s, err := localindex.Open(filepath.Join(t.TempDir(), "index.db"), localindex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	src, err := s.EnsureSource(ctx, transcript.Source{Agent: transcript.AgentClaude, Path: "/missing/placement.jsonl", StorageKind: transcript.StorageJSONLAppend, Parser: "claude@1"})
	if err != nil {
		t.Fatal(err)
	}
	hash := "abcdef0" + strings.Repeat("1", 33)
	var messages []*transcript.Message
	for _, session := range []string{"parent", "child"} {
		call := &transcript.Message{SessionID: session, NativeID: "call", Ordinal: 1, Kind: transcript.KindToolCall, ToolName: "Bash", ToolCallID: "tool"}
		call.SetText(`{"command":"git -C /literal commit -m fix"}`, transcript.CapConfig{})
		result := &transcript.Message{SessionID: session, NativeID: "result", Ordinal: 2, Kind: transcript.KindToolResult, ToolName: "Bash", ToolCallID: "tool"}
		result.SetText("[main abcdef0] fix", transcript.CapConfig{})
		messages = append(messages, call, result)
	}
	if _, err = s.ApplyBatch(ctx, localindex.Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "parent", Cwd: "/transcript-repo"}, {Agent: transcript.AgentClaude, SessionID: "child", Cwd: "/transcript-repo", ParentSessionID: "parent", Depth: 1}}, Messages: messages}); err != nil {
		t.Fatal(err)
	}
	place := localindex.Placement{Agent: transcript.AgentClaude, SessionID: "parent", How: localindex.PlacedByWorktree, Placement: pathpolicy.Placement{Main: "/main/a", Remote: "host/org/a", Worktree: "/work/a"}}
	if err = s.SavePlacement(ctx, place); err != nil {
		t.Fatal(err)
	}
	assertVisible := func(main, remote string, want int) {
		t.Helper()
		for _, filter := range []localindex.Filter{{RepoMains: []string{main}}, {RepoRemotes: []string{remote}}} {
			rows, _, _, err := s.ChangeRecords(ctx, []string{hash}, filter, -1, 0)
			if err != nil || len(rows) != want {
				t.Fatalf("visibility %+v rows=%d want=%d err=%v", filter, len(rows), want, err)
			}
			if want == 2 {
				sessions := map[string]bool{}
				for _, r := range rows {
					var session string
					if err := s.DB().QueryRow(`SELECT session_id FROM conversations WHERE id=?`, r.ConversationID).Scan(&session); err != nil {
						t.Fatal(err)
					}
					sessions[session] = true
				}
				if !sessions["parent"] || !sessions["child"] {
					t.Fatalf("parent placement lost child visibility: %v", sessions)
				}
			}
		}
	}
	assertVisible("/main/a", "host/org/a", 2)
	backend := &Backend{Store: s}
	// This stable transcript scope does not change with placement resolution;
	// the old cursor must restart because the persisted revision changed.
	filter := format.Filters{RepoRoots: []string{"/transcript-repo"}}
	query := format.ChangeQuery{Commits: []string{hash}, Limit: 1}
	page, err := backend.Change(ctx, query, filter)
	if err != nil || !page.HasMore || page.NextCursor == "" {
		t.Fatalf("initial page %+v %v", page, err)
	}
	query.Cursor = page.NextCursor
	before, err := s.ChangeState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	place.Main = "/main/b"
	place.Remote = "host/org/b"
	place.Worktree = "/work/b"
	if err = s.SavePlacement(ctx, place); err != nil {
		t.Fatal(err)
	}
	after, err := s.ChangeState(ctx)
	if err != nil || after.Revision <= before.Revision {
		t.Fatalf("placement did not invalidate cursor: %+v %+v %v", before, after, err)
	}
	page, err = backend.Change(ctx, query, filter)
	if err != nil || !page.RestartRequired || len(page.Matches) != 0 || page.NextCursor != "" {
		t.Fatalf("old cursor did not restart: %+v %v", page, err)
	}
	assertVisible("/main/a", "host/org/a", 0)
	assertVisible("/main/b", "host/org/b", 2)
	page, err = backend.Change(ctx, format.ChangeQuery{Commits: []string{hash}, Limit: 10}, filter)
	if err != nil || len(page.Matches) != 2 {
		t.Fatalf("fresh page %+v %v", page, err)
	}
	foundParent := false
	for _, m := range page.Matches {
		if m.CommandDirectory != "/literal" || !m.DirectoryKnown {
			t.Fatalf("placement changed message-derived evidence: %+v", m)
		}
		if m.Session.SessionID == "parent" {
			foundParent = true
			if m.Session.RepoIdentity != "host/org/b" || m.Session.Worktree != "/work/b" {
				t.Fatalf("stale placement metadata: %+v", m.Session)
			}
		}
	}
	if !foundParent {
		t.Fatal("fresh page omitted parent")
	}
}
