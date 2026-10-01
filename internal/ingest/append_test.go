package ingest

import (
	"bytes"
	"io"
	"os"
	"testing"
)

// A generation loaded from an offset reads the bytes before it too: the
// reader loads the earlier manifest entries when a read reaches them.
func TestGenerationFromOffsetReadsWholeFile(t *testing.T) {
	e := newEnv(t)
	a := newAppendSession(t, e, 400)
	want, err := os.ReadFile(a.spec.Path)
	if err != nil {
		t.Fatal(err)
	}
	full, err := LoadGeneration(e.ctx, e.pool, a.id, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Entries) < 8 {
		t.Fatalf("fixture has %d manifest entries, want several", len(full.Entries))
	}
	from := full.Entries[len(full.Entries)-3].Offset + 5
	g, err := LoadGenerationFrom(e.ctx, e.pool, a.id, -1, from)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Entries) != 3 {
		t.Fatalf("loaded %d entries from offset %d, want the 3 that cover it on", len(g.Entries), from)
	}
	r := NewReader(e.ctx, e.objects, g)
	if r.Size() != int64(len(want)) {
		t.Fatalf("size %d, want %d", r.Size(), len(want))
	}
	// A read in the window, then one that reaches back to the start.
	tail := make([]byte, int64(len(want))-from)
	if _, err := r.ReadAt(tail, from); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tail, want[from:]) || len(g.Entries) != 3 {
		t.Fatalf("window read differs or loaded more entries (%d)", len(g.Entries))
	}
	got, err := io.ReadAll(io.NewSectionReader(r, 0, r.Size()))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("reconstruction from an offset differs from the file")
	}
	if len(g.Entries) != len(full.Entries) {
		t.Fatalf("after reading from 0: %d entries, want %d", len(g.Entries), len(full.Entries))
	}
}
