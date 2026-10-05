package format

import (
	"bytes"
	"strings"
	"testing"
)

func TestReadContinuationsUseSelectedCommand(t *testing.T) {
	st := Style{CLIReadCommand: "flopwire read --local --index '/tmp/archive.db'"}
	for _, pair := range [][2]string{
		{st.More("session/1", "after", 10), "flopwire read --local --index '/tmp/archive.db' session/1 --messages-after 10"},
		{st.readAt("session/1", 4), "flopwire read --local --index '/tmp/archive.db' session/1 --line-offset 4"},
		{st.outlineAt("session", "cursor"), "flopwire read --local --index '/tmp/archive.db' session --outline --cursor cursor"},
	} {
		if pair[0] != pair[1] {
			t.Fatalf("got %q, want %q", pair[0], pair[1])
		}
	}
	st.MCP = true
	for _, pair := range [][2]string{
		{st.More("session/1", "after", 10), "flopwire_read address=session/1 messages_after=10"},
		{st.readAt("session/1", 4), "flopwire_read address=session/1 line_offset=4"},
		{st.outlineAt("session", "cursor"), "flopwire_read address=session outline=true cursor=cursor"},
	} {
		if pair[0] != pair[1] {
			t.Fatalf("MCP got %q, want %q", pair[0], pair[1])
		}
	}
}

func TestGroupedGrepMoreLinesKeepsReadScope(t *testing.T) {
	p := &Page{Hits: []Hit{{Address: "session/1:2", SessionID: "session", Agent: "claude", Kind: "user", Lines: []Line{{N: 2, Text: "match", Match: true}}, MoreLines: 3}}, Total: 1, Exact: true}
	for _, command := range []string{
		"flopwire read --local",
		"flopwire read --index '/tmp/archive.db'",
		"flopwire read --local --index '/tmp/archive.db'",
	} {
		var out bytes.Buffer
		if err := WriteGrep(&out, p, "", Style{CLIReadCommand: command}); err != nil {
			t.Fatal(err)
		}
		want := "[+3 more matching lines; " + command + " session/1:2]"
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing scoped grouped continuation %q: %s", want, out.String())
		}
	}
	var out bytes.Buffer
	if err := WriteGrep(&out, p, "", Style{MCP: true, CLIReadCommand: "flopwire read --local"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "[+3 more matching lines; flopwire_read address=session/1:2]") {
		t.Fatalf("MCP grouped continuation used CLI scope: %s", out.String())
	}
}
