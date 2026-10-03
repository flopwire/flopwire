package opencode

import (
	"bytes"
	"context"
	"slices"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

// A subagent whose parent has no task call naming it, or whose parent
// session is gone, parses without a link; a task call naming another
// child does not link it.
func TestSubagentWithoutParentCall(t *testing.T) {
	f := newFixture(t)
	seed(f)
	const sesC, sesOrphan = "ses_synthetic0000000000000C", "ses_synthetic0000000000000O"
	f.part(oid("prt", t0+29, 1), msgA1, sesA, t0+29, `{"type":"tool","tool":"task","callID":"call_t","state":{"status":"running","input":{},"metadata":{"sessionId":"`+sesC+`"}}}`)
	f.session(sesB, sesA, cwd, "child without a call", t0+30)
	f.Prompt(sesB, t0+31, "explore widget.go")
	f.session(sesOrphan, "ses_missing_parent", cwd, "orphan", t0+32)
	f.Prompt(sesOrphan, t0+33, "orphan prompt")
	c, cur := parse(t, f.path, transcript.Cursor{})
	for _, id := range []string{sesB, sesOrphan} {
		v := conv(c, id)
		if v == nil || v.SpawnedByToolCallID != "" || v.Depth != 1 || v.ParentSessionID == "" {
			t.Fatalf("%s = %+v", id, v)
		}
	}
	f.Prompt(sesB, t0+40, "again")
	f.Prompt(sesOrphan, t0+41, "again")
	c, _ = parse(t, f.path, cur)
	for _, id := range []string{sesB, sesOrphan} {
		if v := conv(c, id); v != nil && v.SpawnedByToolCallID != "" {
			t.Fatalf("%s linked to %q", id, v.SpawnedByToolCallID)
		}
	}
	for _, id := range []string{sesB, sesOrphan} {
		b, err := Export(context.Background(), f.path, id)
		if err != nil {
			t.Fatal(err)
		}
		db, err := LoadExport(context.Background(), bytes.NewReader(b), id, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		got, _ := parse(t, db, transcript.Cursor{})
		if v := conv(got, id); v == nil || v.SpawnedByToolCallID != "" {
			t.Fatalf("server %s = %+v", id, v)
		}
	}
}

// A session deleted and created again under the same id between two
// parses: its old rows are superseded and only the new ones come back.
func TestRecreatedSessionDoesNotResurrect(t *testing.T) {
	f := newFixture(t)
	seed(f)
	_, cur := parse(t, f.path, transcript.Cursor{})
	f.exec(`DELETE FROM part`)
	f.exec(`DELETE FROM message`)
	f.exec(`DELETE FROM session`)
	f.session(sesA, "", cwd, "Fix the flaky test", t0+90000)
	newPart := f.Prompt(sesA, t0+90000, "a fresh start")
	c, _ := parse(t, f.path, cur)
	if !slices.Equal(c.SupersededSessions, []string{sesA}) {
		t.Fatalf("superseded = %v", c.SupersededSessions)
	}
	var natives []string
	for _, m := range c.Messages {
		natives = append(natives, m.NativeID)
	}
	if !slices.Equal(natives, []string{newPart}) {
		t.Fatalf("emitted %v, want only %s", natives, newPart)
	}
}

// A row that appears below the saved highest id and more than lagMS
// before the watermark (the clock stepped back, or another writer's
// older id committed late) is still emitted.
func TestRowBelowWatermarkIsEmitted(t *testing.T) {
	f := newFixture(t)
	seed(f)
	_, cur := parse(t, f.path, transcript.Cursor{})
	// The clock stepped back 10 s: the new part's id and stamps are older
	// than the session's highest id and watermark.
	at := t0 + 30000
	late := oid("prt", at, 9)
	f.part(late, msgA1, sesA, at, `{"type":"text","text":"written after the clock stepped back"}`)
	c, _ := parse(t, f.path, cur)
	for _, m := range c.Messages {
		if m.NativeID == late {
			return
		}
	}
	t.Fatalf("row %s below the watermark was never emitted (%d rows)", late, len(c.Messages))
}
