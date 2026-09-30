package codex

import (
	"reflect"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

// Every directory a session names after session_meta (a turn_context cwd,
// an <environment_context> <cwd> tag) is listed once in OtherCwds, across
// incremental parses; a tag in a tool output or a prompt is not one.
func TestOtherCwds(t *testing.T) {
	first := []string{
		rec(0, "session_meta", `{"id":"s-cwds","cwd":"/work/app","source":"cli"}`),
		rec(1, "turn_context", `{"turn_id":"t1","cwd":"/work/app"}`),
		rec(2, "response_item", `{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n<cwd>/work/app</cwd></environment_context>"}]}`),
		rec(3, "response_item", `{"type":"message","role":"user","content":[{"type":"input_text","text":"what is in <cwd>/not/a/dir</cwd>?"}]}`),
		rec(4, "response_item", `{"type":"function_call_output","call_id":"c1","output":"<environment_context><cwd>/not/either</cwd></environment_context>"}`),
		rec(5, "turn_context", `{"turn_id":"t2","cwd":"/work/secret"}`),
	}
	c, cur := parseLines(t, nil, "", first, transcript.Cursor{})
	conv := lastConv(c)
	if conv.Cwd != "/work/app" || !reflect.DeepEqual(conv.OtherCwds, []string{"/work/secret"}) {
		t.Fatalf("cwd %q other %v", conv.Cwd, conv.OtherCwds)
	}
	more := []string{
		rec(6, "response_item", `{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n<cwd>/work/other</cwd></environment_context>"}]}`),
		rec(7, "turn_context", `{"turn_id":"t3","cwd":"/work/secret"}`),
	}
	c, _ = parseLines(t, nil, "", append(first, more...), cur)
	if len(c.Conversations) == 0 {
		t.Fatal("no conversation re-emitted for a new directory")
	}
	if got := lastConv(c).OtherCwds; !reflect.DeepEqual(got, []string{"/work/secret", "/work/other"}) {
		t.Fatalf("after append: %v", got)
	}
}
