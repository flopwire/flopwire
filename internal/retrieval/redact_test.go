package retrieval_test

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/redact"
	"github.com/flopwire/flopwire/internal/redact/redacttest"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript/claude"
)

// Raw reads go through the server's redaction pass: bytes an old agent
// uploaded unredacted are served masked, at the same offsets.
func TestRawReadIsRedacted(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	tmpl, err := os.ReadFile("../../testdata/redaction/claude.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(redacttest.Fill(string(tmpl), 1))
	path := s.home + "/.claude/projects/-workspace-redaction/20000000-0000-4000-8000-000000000010.jsonl"
	h := syncproto.FlushHeader{Version: syncproto.Version, CapturedAt: time.Now().UTC(),
		Source:  syncproto.Source{Path: path, FileID: "1:1", Agent: "claude", StorageKind: "jsonl_append", Parser: claude.ParserName},
		Chunker: syncproto.ChunkerParams{Algorithm: devicesync.Algorithm, Min: 1 << 10, Avg: 4 << 10, Max: 16 << 10},
		Entries: []syncproto.Entry{{Hash: syncproto.Sum(data), Size: int64(len(data))}}}
	body, z := syncproto.EncodeBody(data)
	h.Bodies = []syncproto.Body{body}
	old := &syncproto.Client{Server: s.client.Server, Token: s.client.Token, HTTP: http.DefaultClient}
	if _, err := old.Flush(ctx, &syncproto.FlushRequest{Header: h, Payload: bytes.NewReader(z)}); err != nil {
		t.Fatal(err)
	}
	if err := s.queue.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	var src string
	if err := s.pool.QueryRow(ctx, `SELECT id::text FROM sources WHERE path=$1`, path).Scan(&src); err != nil {
		t.Fatal(err)
	}
	got, err := s.client.Raw(ctx, src, 0, 0, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	want := make([]byte, len(data))
	redact.NewReaderAt(bytes.NewReader(data), redact.Lines).ReadAt(want, 0)
	if !bytes.Equal(got, want) || bytes.Equal(got, data) {
		t.Fatalf("raw read is not the redacted bytes (%d vs %d)", len(got), len(want))
	}
	// A range that starts mid-line redacts the same way.
	mid, err := s.client.Raw(ctx, src, 0, 700, 900)
	if err != nil || !bytes.Equal(mid, want[700:1600]) {
		t.Fatalf("mid-line raw read differs: %v", err)
	}
	for name, needle := range redacttest.Needles() {
		if bytes.Contains(got, []byte(needle)) {
			t.Errorf("raw read serves planted %s", name)
		}
	}
}
