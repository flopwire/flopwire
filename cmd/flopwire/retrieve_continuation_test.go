package main

import (
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"testing"
)

func TestGrepContinuationMCPOptions(t *testing.T) {
	o, _, err := mcpOpts("flopwire_grep", map[string]any{"pattern": "x", "continue": true, "cursor": "opaque"})
	if err != nil || !o.on["continue"] || o.vals["cursor"] != "opaque" {
		t.Fatalf("options: %+v %v", o, err)
	}
}

func TestBoundScanPageKeepsMessages(t *testing.T) {
	p := &format.Page{Scan: &format.GrepScan{Status: "more", NextCursor: "next"}, Hits: []format.Hit{{Address: "session/1"}, {Address: "session/2"}}}
	got := boundPage("grep", p, format.ModeContent, 1, true)
	if len(got.Hits) != 2 || got.Scan.NextCursor != "next" || got.Next != 0 {
		t.Fatalf("lost scan results: %+v", got)
	}
}
