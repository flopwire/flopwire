package ingest

import (
	"context"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/redact"
)

func TestArchiveScanLongLines(t *testing.T) {
	// Just over ReaderAt's old segment limit, including trailing CRs across
	// read-buffer boundaries. Hashing and mask positions remain whole-line.
	line := strings.Repeat("x", redact.SegMax+3) + "secret" + strings.Repeat("\r", 65537)
	prefix := "ignore\n"
	spans := []redact.Span{{Start: redact.SegMax + 3, End: redact.SegMax + 9}}
	masks := redact.NewLineCatalog([]redact.LineMask{{SHA: redact.LineSum([]byte(line)), Spans: spans, RedactionID: "r"}})
	found := map[string][]redact.Span{}
	next, done, err := scanArchiveMasks(context.Background(), strings.NewReader(prefix+line+"\n"), 0, int64(len(prefix)+len(line)+1), masks, found)
	if err != nil || !done || next != int64(len(prefix)+len(line)+1) {
		t.Fatalf("scan: %d %v %v", next, done, err)
	}
	got := found["r"]
	if len(got) != 1 || got[0].Start != len(prefix)+spans[0].Start || got[0].End != len(prefix)+spans[0].End {
		t.Fatalf("whole-line mask: %+v", got)
	}
}

func TestArchiveScanBatchResume(t *testing.T) {
	masks := redact.NewLineCatalog([]redact.LineMask{{SHA: redact.LineSum([]byte("secret")), Spans: []redact.Span{{Start: 0, End: 6}}, RedactionID: "r"}})
	data := strings.Repeat("secret\n", 257)
	found := map[string][]redact.Span{}
	next, done, err := scanArchiveMasks(context.Background(), strings.NewReader(data), 0, int64(len(data)), masks, found)
	if err != nil || done || len(found["r"]) != 256 {
		t.Fatalf("first batch: %d %v %v", len(found["r"]), done, err)
	}
	remaining := map[string][]redact.Span{}
	end, done, err := scanArchiveMasks(context.Background(), strings.NewReader(data), next, int64(len(data))-next, masks, remaining)
	if err != nil || !done || end != int64(len(data)) || len(remaining["r"]) != 1 || remaining["r"][0].Start != int(next) {
		t.Fatalf("resume: %d %v %+v %v", end, done, remaining, err)
	}
}

func TestArchiveScanTerminatorSpan(t *testing.T) {
	raw := []byte("secret\r\n")
	spans := []redact.Span{{Start: 0, End: len(raw)}}
	catalog := redact.NewLineCatalog([]redact.LineMask{{SHA: redact.LineSum(raw), Spans: spans, RedactionID: "r", Proof: redact.NewLineProof(raw, spans)}})
	found := map[string][]redact.Span{}
	_, done, err := scanArchiveMasks(context.Background(), strings.NewReader("**cret\r\n"), 0, 8, catalog, found)
	if err != nil || !done || len(found["r"]) != 1 || found["r"][0].End != 6 {
		t.Fatalf("terminator span: %+v %v", found, err)
	}
}
