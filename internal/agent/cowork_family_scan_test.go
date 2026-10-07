package agent

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/transcript"
)

// Captured child evidence restricts its verified family without tainting a
// separate metadata-only parent included in the same discovery batch.
func TestCoworkCapturedFamilyBatchDoesNotTaintUnrelatedParent(t *testing.T) {
	f := newCoworkFixture(t)
	otherParent := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	path := filepath.Join(f.cfg.ClaudeProjects, "project", coworkNativeID, "subagents", "agent-cafe.jsonl")
	src, err := f.store.EnsureSource(ctx, transcript.Source{
		Agent: transcript.AgentClaude, Path: path, SessionKey: "agent-cafe",
		StorageKind: transcript.StorageJSONLAppend, Parser: "claude@1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.SaveWatermark(ctx, src.ID, transcript.Watermark{Offset: 9}, nil); err != nil {
		t.Fatal(err)
	}
	coworkMetadata(t, f, []string{"/host/first"}, nil, nil)
	md, err := json.Marshal(map[string]any{
		"sessionId": "second", "cliSessionId": otherParent,
		"userSelectedFolders": []string{"/host/second"},
	})
	if err != nil {
		t.Fatal(err)
	}
	coworkWrite(t, filepath.Join(f.cfg.CoworkRoot, "account", "workspace", "second.json"), string(md))
	for attempt := range 2 {
		f.a.refreshCowork(ctx)
		for _, id := range []string{coworkNativeID, "agent-cafe"} {
			p, _ := f.a.storedPlace(placeKey{transcript.AgentClaude, id})
			if p.how != localindex.PlacedByCoworkUnknown {
				t.Fatalf("attempt %d: captured family member %s lost its historical floor: %+v", attempt, id, p)
			}
		}
		p, _ := f.a.storedPlace(placeKey{transcript.AgentClaude, otherParent})
		if p.how != localindex.PlacedByCowork {
			t.Fatalf("attempt %d: unrelated fresh parent inherited historical scope: %+v", attempt, p)
		}
		f.restart()
	}
}
