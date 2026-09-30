package redact

import (
	"bytes"
	"strings"
	"testing"
)

func TestLineCatalogChainedPartialMasks(t *testing.T) {
	raw := []byte(`{"id":"stable","content":"secret-A and secret-B"}`)
	a := bytes.Index(raw, []byte("secret-A"))
	b := bytes.Index(raw, []byte("secret-B"))
	first := []Span{{a, a + 8}}
	both := []Span{{a, a + 8}, {b, b + 8}}
	middle := bytes.Clone(raw)
	FillSpans(middle, first, MessageRule)
	catalog := NewLineCatalog([]LineMask{
		{SHA: LineSum(raw), Spans: first, RedactionID: "a", Proof: NewLineProof(raw, first)},
		{SHA: LineSum(middle), Spans: both, RedactionID: "b", Proof: NewLineProof(middle, both)},
	})
	hybrid := bytes.Clone(raw)
	copy(hybrid[a:a+3], "***")
	for _, input := range [][]byte{raw, middle, hybrid} {
		out := bytes.Clone(input)
		FillSpans(out, catalog.MatchBytes(input), MessageRule)
		if strings.Contains(string(out), "secret") {
			t.Fatalf("chained or partial mask left secret: %s", out)
		}
		if !bytes.Contains(out, []byte(`"id":"stable"`)) {
			t.Fatal("structural bytes changed")
		}
	}
	unrelated := bytes.ReplaceAll(raw, []byte("stable"), []byte("other!"))
	if len(catalog.MatchBytes(unrelated)) != 0 {
		t.Fatal("normalized proof matched a different record")
	}
}

func TestLineProofTrailingNewlineSpan(t *testing.T) {
	raw := []byte("secret\r\n")
	spans := []Span{{0, len(raw)}}
	proof := NewLineProof(raw, spans)
	catalog := NewLineCatalog([]LineMask{{SHA: LineSum(raw), Spans: spans, RedactionID: "r", Proof: proof}})
	if proof.Size != 6 || len(catalog.MatchBytes([]byte("**cret\r\n"))) != 1 {
		t.Fatal("whole-record span must support trimmed terminators")
	}
}
