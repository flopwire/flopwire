package format

import "testing"

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
