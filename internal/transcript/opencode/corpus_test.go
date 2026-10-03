package opencode

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

// TestCorpus parses a copy of this machine's opencode store
// (FLOPWIRE_CORPUS=1): every session row yields a conversation, row keys
// are unique, a second parse emits nothing, and every session's export
// parses to the same rows. It reports counts only, never content.
func TestCorpus(t *testing.T) {
	if os.Getenv("FLOPWIRE_CORPUS") != "1" {
		t.Skip("set FLOPWIRE_CORPUS=1 to parse the local opencode store")
	}
	home, _ := os.UserHomeDir()
	src := DefaultPath(home)
	if _, err := os.Stat(src); err != nil {
		t.Skipf("no opencode store: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.db")
	for _, sfx := range []string{"", "-wal", "-shm"} {
		in, err := os.Open(src + sfx)
		if os.IsNotExist(err) && sfx != "" {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		out, err := os.Create(path + sfx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(out, in); err != nil {
			t.Fatal(err)
		}
		in.Close()
		out.Close()
	}
	c, cur := parse(t, path, transcript.Cursor{})
	byKind := map[transcript.Kind]int{}
	keys := map[string]bool{}
	for _, m := range c.Messages {
		byKind[m.Kind]++
		k := m.SessionID + "|" + m.NativeID + "|" + string(rune('0'+m.Part))
		if keys[k] {
			t.Errorf("duplicate row key %s", k)
		}
		keys[k] = true
	}
	ctx := context.Background()
	ids, err := ListSessions(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Conversations) != len(ids) {
		t.Errorf("conversations %d, sessions %d", len(c.Conversations), len(ids))
	}
	again, _ := parse(t, path, cur)
	if len(again.Messages)+len(again.Conversations) != 0 {
		t.Errorf("second parse emitted %d rows, %d conversations", len(again.Messages), len(again.Conversations))
	}
	subagents := 0
	for _, id := range ids {
		b, err := Export(ctx, path, id)
		if err != nil {
			t.Fatal(err)
		}
		db, err := LoadExport(ctx, bytes.NewReader(b), id, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		got, _ := parse(t, db, transcript.Cursor{})
		if !slices.Equal(rows(got.Messages), rows(c.MessagesFor(id))) {
			t.Errorf("session %d: export rows differ (%d vs %d)", slices.Index(ids, id), len(got.Messages), len(c.MessagesFor(id)))
		}
		if cv := conv(c, id); cv != nil && cv.ParentSessionID != "" {
			subagents++
		}
	}
	t.Logf("opencode corpus: %d sessions (%d subagents), %d rows: user %d, assistant %d, thinking %d, tool_call %d, tool_result %d, injected %d, system %d",
		len(ids), subagents, len(c.Messages), byKind[transcript.KindUser], byKind[transcript.KindAssistant], byKind[transcript.KindThinking],
		byKind[transcript.KindToolCall], byKind[transcript.KindToolResult], byKind[transcript.KindInjected], byKind[transcript.KindSystem])
}
