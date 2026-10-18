package local

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/transcript"
)

func TestSearchMatchingSynthetic(t *testing.T) {
	b, _ := oracle(t)
	addRows(t, b, "topic", &transcript.Message{Kind: transcript.KindAssistant, Text: "autoreview review only café can't"})
	addRows(t, b, "noise",
		&transcript.Message{Kind: transcript.KindAssistant, Text: "UX review automatic edits"},
		&transcript.Message{Kind: transcript.KindAssistant, Text: "UX review off always"},
		&transcript.Message{Kind: transcript.KindToolResult, Text: "UX review only"})
	query := "autoreview automatic edits off always review only"
	search := func(q, mode string, offset int, f format.Filters) *format.Page {
		t.Helper()
		p, err := b.Search(ctx, format.SearchQuery{Query: q, Matching: mode, Offset: offset, Limit: 1}, f)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := search(query, "auto", 0, format.Filters{})
	if len(p.Hits) != 1 || p.Hits[0].SessionID != "topic" || len(p.Notes) != 1 || !strings.Contains(p.Notes[0], "rarest matching term: autoreview") {
		t.Fatalf("anchor: %+v", p)
	}
	later := search(query, "auto", 100, format.Filters{})
	if len(later.Hits) != 0 || strings.Join(later.Notes, "") == "" || strings.Join(later.Notes, "") != strings.Join(p.Notes, "") {
		t.Fatalf("offset changed strategy: %+v", later)
	}
	if p = search(query, "all", 0, format.Filters{}); len(p.Hits) != 0 || len(p.Notes) != 0 {
		t.Fatalf("all: %+v", p)
	}
	p = search(query, "any", 0, format.Filters{Session: "noise"})
	if len(p.Hits) != 1 || p.Hits[0].SessionID != "noise" {
		t.Fatalf("any: %+v", p)
	}
	for _, q := range []string{`autoreview missing "review only"`, "autoreview autoreview, vanished?", "café can't vanished"} {
		p = search(q, "auto", 0, format.Filters{Session: "topic"})
		if len(p.Hits) != 1 || p.Hits[0].SessionID != "topic" {
			t.Fatalf("%q: %+v", q, p)
		}
	}
	if p = search(`autoreview missing "UX review"`, "auto", 0, format.Filters{}); len(p.Hits) != 0 {
		t.Fatalf("lost phrase or anchor: %+v", p)
	}
	if p = search(`autoreview "review only"`, "any", 0, format.Filters{Session: "noise"}); len(p.Hits) != 0 {
		t.Fatalf("any lost word requirement: %+v", p)
	}
	for _, q := range []string{"neverindexed notindexed", "how did we"} {
		if p = search(q, "auto", 0, format.Filters{Session: "topic"}); len(p.Hits) != 0 || len(p.Notes) != 0 {
			t.Fatalf("unknown/stopword: %+v", p)
		}
	}
	for _, f := range []format.Filters{{Kinds: []string{"assistant"}}, {Repo: "/tmp/big"}, {Session: "topic"}, {Sort: format.SortOldest}} {
		p = search(query, "auto", 0, f)
		if len(p.Hits) != 1 || p.Hits[0].SessionID != "topic" {
			t.Fatalf("filters %+v: %+v", f, p)
		}
	}
	deadline, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancel()
	p, err := b.Search(deadline, format.SearchQuery{Query: query}, format.Filters{})
	// Filter resolution itself may return the expired context; neither path
	// may return broader hits after incomplete evidence.
	if err == nil && (!p.Truncated || len(p.Hits) != 0 || len(p.Notes) != 0) {
		t.Fatalf("deadline fallback: %+v", p)
	}
}

func TestSearchFilteredRarityAndLongPhrase(t *testing.T) {
	b, _ := oracle(t)
	addRows(t, b, "rarity-scope",
		&transcript.Message{Kind: transcript.KindAssistant, Text: "alphatopic sharedtopic"},
		&transcript.Message{Kind: transcript.KindAssistant, Text: "alphatopic extra"},
		&transcript.Message{Kind: transcript.KindAssistant, Text: "alphatopic other"})
	addRows(t, b, "rarity-global",
		&transcript.Message{Kind: transcript.KindAssistant, Text: "sharedtopic one"},
		&transcript.Message{Kind: transcript.KindAssistant, Text: "sharedtopic two"},
		&transcript.Message{Kind: transcript.KindAssistant, Text: "sharedtopic three"},
		&transcript.Message{Kind: transcript.KindAssistant, Text: "sharedtopic four"})
	for _, tc := range []struct {
		f      format.Filters
		anchor string
	}{{format.Filters{}, "alphatopic"}, {format.Filters{Session: "rarity-scope"}, "sharedtopic"}} {
		p, err := b.Search(ctx, format.SearchQuery{Query: "alphatopic sharedtopic absentword"}, tc.f)
		if err != nil || len(p.Hits) != 1 || len(p.Notes) != 1 || !strings.Contains(p.Notes[0], "rarest matching term: "+tc.anchor) {
			t.Fatalf("filtered rarity %+v: %+v %v", tc, p, err)
		}
	}
	addRows(t, b, "long-phrase",
		&transcript.Message{Kind: transcript.KindAssistant, Text: "before review only after"},
		&transcript.Message{Kind: transcript.KindAssistant, Text: "before review only between after"})
	for _, mode := range []string{"auto", "any", "all"} {
		p, err := b.Search(ctx, format.SearchQuery{Query: `"before review only after"`, Matching: mode}, format.Filters{Session: "long-phrase"})
		if err != nil || len(p.Hits) != 1 || p.Hits[0].Snippet != "before review only after" {
			t.Fatalf("long phrase %s: %+v %v", mode, p, err)
		}
	}
}
