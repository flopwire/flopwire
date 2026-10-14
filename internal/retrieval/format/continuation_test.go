package format

import (
	"bytes"
	"net/url"
	"strings"
	"testing"
)

func TestGrepContinuationTransport(t *testing.T) {
	q := GrepQuery{Pattern: "needle", Continue: true, Cursor: "opaque", Authority: "private"}
	v := q.Values(url.Values{})
	got, err := ParseGrepQuery(v)
	if err != nil || !got.Continue || got.Cursor != q.Cursor || got.Pattern != q.Pattern {
		t.Fatalf("roundtrip: %+v %v", got, err)
	}
	if v.Get("authority") != "" {
		t.Fatal("authority exposed")
	}
	if _, err := ParseGrepQuery(url.Values{"continue": {"broken"}}); err == nil {
		t.Fatal("accepted invalid boolean")
	}
	got, err = ParseGrepQuery(url.Values{"cursor": {"opaque"}})
	if err != nil || !got.Continue {
		t.Fatalf("cursor did not imply continuation: %+v %v", got, err)
	}
}

func TestGrepScanRenderingKeepsMessages(t *testing.T) {
	p := &Page{Scan: &GrepScan{Status: "more", NextCursor: "next", Checked: 10}, Hits: []Hit{
		{Address: "session/1", SessionID: "session", Lines: []Line{{N: 1, Text: strings.Repeat("a", 100), Match: true}}},
		{Address: "session/2", SessionID: "session", Lines: []Line{{N: 1, Text: strings.Repeat("b", 100), Match: true}}},
	}}
	var b bytes.Buffer
	if err := WriteGrep(&b, p, ModeContent, Style{Flat: true, MCP: true, Budget: 10}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"session/1", "session/2", "cursor=next"} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("missing %s: %s", want, b.String())
		}
	}
	if strings.Contains(b.String(), "offset=") {
		t.Fatal("snapshot used offset")
	}
	b.Reset()
	p.Hits = nil
	p.Scan.Status = "complete"
	p.Scan.NextCursor = ""
	if err := WriteGrep(&b, p, ModeContent, Style{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "no matches") || !strings.Contains(b.String(), "scan: complete") {
		t.Fatalf("misleading terminal page: %s", b.String())
	}
}

func TestBoundGrepScanShortensExcerpts(t *testing.T) {
	p := &Page{Scan: &GrepScan{Status: "more", NextCursor: "next"}, Next: 9}
	for i := 0; i < 20; i++ {
		p.Hits = append(p.Hits, Hit{Address: "session/1", MessageID: "id", SessionID: "session", Kind: "assistant", Title: strings.Repeat("title", 1000), Lines: []Line{{N: 1, Text: strings.Repeat("long excerpt", 1000), Match: true}}})
	}
	got := BoundGrepScan(p, MaxOutput)
	if len(got.Hits) != 20 || got.Next != 0 || got.Hits[0].Address != "session/1" || len(got.Hits[0].Lines[0].Text) > 104 || got.Hits[0].Title != "" {
		t.Fatalf("unbounded scan: %+v", got.Hits[0])
	}
	if p.Hits[0].Title == "" || p.Next != 9 {
		t.Fatal("mutated source page")
	}
}

func TestBoundGrepScanPreservesMatchLine(t *testing.T) {
	p := &Page{Scan: &GrepScan{Status: "more"}, Hits: []Hit{{Address: "session/1", Lines: []Line{{N: 499, Text: "context"}, {N: 500, Text: "actual match", Match: true}, {N: 501, Text: "next match", Match: true}}}}}
	got := BoundGrepScan(p, 1)
	if len(got.Hits[0].Lines) != 1 || got.Hits[0].Lines[0].N != 500 || !got.Hits[0].Lines[0].Match || got.Hits[0].MoreLines != 1 {
		t.Fatalf("invented match line: %+v", got.Hits[0])
	}
}
