package devicesync

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/redact"
	"github.com/flopwire/flopwire/internal/redact/redacttest"
	"github.com/flopwire/flopwire/internal/transcript"
)

// Synthetic reproduction: a token begins in an unfinished JSONL record,
// the file idles, and the remaining token bytes arrive after idle sealing.
func TestSecretSplitAcrossIdleSeal(t *testing.T) {
	now := time.Now()
	e := newEnv(t, Config{Now: func() time.Time { return now }, SealAfter: time.Minute}, 1<<20)
	sp := e.spec("idle-secret.jsonl", transcript.StorageJSONLAppend)
	token := redacttest.Needles()["GITHUB_PAT"]
	first := []byte("{\"type\":\"x\",\"text\":\"ok\"}\n")
	partial := []byte(`{"type":"x","text":"token ` + token[:20])
	appendFile(t, sp.Path, append(append([]byte{}, first...), partial...))
	e.sync(sp)
	id := fileIDOf(t, sp.Path)
	active, err := e.srv.Reconstruct(sp.Path, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(active, first) {
		t.Fatalf("active record not withheld: %q", active)
	}
	t.Logf("synthetic token: %q", token)
	t.Logf("before idle: %q", active)
	now = now.Add(time.Hour)
	e.sync(sp)
	idle, err := e.srv.Reconstruct(sp.Path, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("after idle: %q", idle)
	if !bytes.Equal(idle, first) {
		t.Fatalf("unfinished record uploaded at idle: %q", idle)
	}
	entries, tail := e.srv.Manifest(sp.Path, id, 0)
	if tail != nil || len(entries) == 0 || entries[len(entries)-1].End() != int64(len(first)) {
		t.Fatal("idle did not seal the complete-record prefix")
	}
	if e.sy.provisional(context.Background(), sp.Path) {
		t.Fatal("unfinished suffix rearmed the idle check")
	}
	requests := e.srv.FlushRequests
	e.sync(sp)
	if e.srv.FlushRequests != requests {
		t.Fatal("unchanged unfinished suffix caused another upload")
	}
	appendFile(t, sp.Path, []byte(token[20:]+` end"}`+"\n"))
	e.sync(sp)
	final, err := e.srv.Reconstruct(sp.Path, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(sp.Path)
	if err != nil {
		t.Fatal(err)
	}
	want := redactAll(t, raw, redact.Lines)
	t.Logf("after completion: %q", final)
	t.Logf("one-shot redacted: %q", want)
	t.Logf("full credential persisted: %v; first 20 bytes persisted: idle=%v final=%v", bytes.Contains(final, []byte(token)), bytes.Contains(idle, []byte(token[:20])), bytes.Contains(final, []byte(token[:20])))
	if bytes.Contains(idle, []byte(token[:20])) || !bytes.Equal(final, want) {
		t.Fatal("idle seal persisted a token fragment and final archive differs from one-shot redaction")
	}
}
