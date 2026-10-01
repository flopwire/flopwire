package retrieval_test

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/codex"
)

// Bytes in a provisional tail are not fixed: a whole tail (From 0) of the
// same generation replaces them without a prefix check. Bob plants a
// Codex user line without an id (keyed by its offset) before Gary
// uploads, with text that is a prefix of Gary's and the same length. After
// Gary uploads, Bob replaces his tail with Gary's bytes, and the next full
// reparse grows his row in place at the same byte range. It must not keep
// its earlier first_seen_at, or Bob counts as the first uploader of
// Gary's record and his redaction rewrites Gary's row.
func TestRedactPlantedTailSwapIsNotFirst(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	meta := jline(map[string]any{"timestamp": "2026-09-30T12:00:00Z", "type": "session_meta", "payload": map[string]any{"id": "30000000-0000-4000-8000-0000000000d1",
		"timestamp": "2026-09-30T12:00:00Z", "cwd": "/w/swap", "originator": "fixture", "cli_version": "1.0.0", "source": "cli"}})
	user := func(text, pad string) string {
		return jline(map[string]any{"timestamp": "2026-09-30T12:00:01Z", "type": "response_item", "payload": map[string]any{"type": "message", "role": "user",
			"pad": pad, "content": []any{map[string]any{"type": "input_text", "text": text}}}})
	}
	real := user(hidden, "")
	planted := user("first line", "")
	planted = user("first line", strings.Repeat("x", len(real)-len(planted)))
	if len(planted) != len(real) {
		t.Fatalf("fixture: lengths %d and %d", len(planted), len(real))
	}

	bob := s.member("bob@example.test")
	src := syncproto.Source{Path: "/w/bob/swap.jsonl", FileID: "copy:swap", Agent: "codex", StorageKind: "jsonl_append", Parser: codex.Name}
	c := &syncproto.Client{Server: bob.Server, Token: bob.Token, HTTP: http.DefaultClient}
	flushTail := func(data []byte) {
		t.Helper()
		h := syncproto.FlushHeader{Version: syncproto.Version, CapturedAt: time.Now().UTC(), Source: src,
			Chunker: syncproto.ChunkerParams{Algorithm: devicesync.Algorithm, Min: 1024, Avg: 4096, Max: 16 << 10},
			Tail:    &syncproto.Tail{Offset: 0, Size: int64(len(data)), Hash: syncproto.Sum(data)}}
		res, err := c.Flush(ctx, &syncproto.FlushRequest{Header: h, Payload: bytes.NewReader(data)})
		if err != nil || res.Status != syncproto.StatusOK {
			t.Fatalf("tail flush: %+v %v", res, err)
		}
		if err := s.queue.Drain(ctx); err != nil {
			t.Fatal(err)
		}
	}
	flushTail([]byte(meta + planted))
	var bobRow string
	var bobFirst time.Time
	if err := s.pool.QueryRow(ctx, `SELECT m.id::text,m.first_seen_at FROM messages m JOIN sources src ON src.id=m.source_id
		WHERE src.path=$1 AND m.role='user'`, src.Path).Scan(&bobRow, &bobFirst); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)

	// Gary uploads the real record.
	cx := filepath.Join(s.home, ".codex", "sessions", "swap.jsonl")
	os.MkdirAll(filepath.Dir(cx), 0o700)
	if err := os.WriteFile(cx, []byte(meta+real), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.sy.Sync(ctx, devicesync.SourceSpec{Path: cx, Agent: transcript.AgentCodex, StorageKind: transcript.StorageJSONLAppend, Parser: codex.Name}); err != nil {
		t.Fatal(err)
	}
	if err := s.queue.Drain(ctx); err != nil {
		t.Fatal(err)
	}

	// Bob swaps his tail for Gary's bytes in the same generation.
	flushTail([]byte(meta + real))
	// A full reparse (a parser or rules upgrade, a quarantine release)
	// reads the swapped bytes.
	if _, err := s.pool.Exec(ctx, `UPDATE source_parse_state SET reparse=true,requested_seq=requested_seq+1,requested_at=now()
		WHERE source_id=(SELECT id FROM sources WHERE path=$1)`, src.Path); err != nil {
		t.Fatal(err)
	}
	if err := s.queue.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	var id string
	var first time.Time
	if err := s.pool.QueryRow(ctx, `SELECT m.id::text,m.first_seen_at FROM messages m JOIN sources src ON src.id=m.source_id
		WHERE src.path=$1 AND strpos(m.text,'BLUEFALCON')>0 AND NOT m.superseded`, src.Path).Scan(&id, &first); err != nil {
		t.Fatal(err)
	}
	if id == bobRow && first.Equal(bobFirst) {
		t.Errorf("Bob's row kept its planted first_seen_at %v after its bytes changed", bobFirst)
	}
	if _, err := s.redact(bob, "/v1/redactions", format.RedactRequest{Address: id + ":2-2"}); err != nil {
		t.Fatal(err)
	}
	s.drainRepairs(s.queue)
	if s.rowsAt(cx, "BLUEFALCON") != 1 {
		t.Error("Bob's redaction rewrote Gary's row")
	}
}
