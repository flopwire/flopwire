package ingest

import (
	"bytes"
	"io"
	"os"
	"testing"

	"github.com/flopwire/flopwire/internal/redact"
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

// A line redacted after the queue cached the catalog is masked in the
// next append that carries it: the cache reloads when the revision moves.
func TestAppendSeesLineRedactedSinceLastParse(t *testing.T) {
	e := newEnv(t)
	a := newAppendSession(t, e, 8)
	a.records(t, 4) // the queue has loaded the (empty) catalog
	line := a.session.Lines(a.next, a.next+1)
	secret := []byte("step 12:")
	i := bytes.Index(line, secret)
	if i < 0 {
		t.Fatalf("fixture line lacks %q: %s", secret, line)
	}
	e.exec(`INSERT INTO message_redactions(id,requested_by,message_id,all_copies,by_admin,messages,chunks,tails,created_at)
		VALUES('a99e0000-0000-4000-a000-000000000001',$1,gen_random_uuid(),false,false,1,0,0,now())`, e.userID)
	sum := redact.LineSum(line)
	e.exec(`INSERT INTO redacted_lines(line_sha,spans,redaction_id) VALUES($1,$2,'a99e0000-0000-4000-a000-000000000001')`,
		sum[:], []redact.Span{{Start: i, End: i + len(secret) + 20}})
	a.add(t, line)
	a.next++
	if n := e.count(`SELECT count(*) FROM messages WHERE NOT superseded AND strpos(text,'step 12:')>0`); n != 0 {
		t.Fatal("an append parsed a redacted line unmasked: the cached catalog was stale")
	}
	if n := e.count(`SELECT count(*) FROM messages WHERE NOT superseded AND strpos(text,'[REDACTED')>0`); n != 1 {
		t.Fatalf("%d masked rows, want 1", n)
	}
	sameDigest(t, e, a.session.SessionID)
}
