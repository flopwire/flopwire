package claude

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

// Every cwd a session's records name after the first is listed once in
// OtherCwds, across incremental parses.
func TestOtherCwds(t *testing.T) {
	line := func(n int, cwd string) string {
		return fmt.Sprintf(`{"type":"user","cwd":%q,"sessionId":"s-cwds","uuid":"%s","timestamp":"2026-09-23T10:00:%02dZ","message":{"role":"user","content":"hi %d"}}`, cwd, u(n), n, n)
	}
	p := &Parser{}
	b := []byte(strings.Join([]string{line(1, "/work/app"), line(2, "/work/app/sub"), line(3, "/work/app")}, "\n") + "\n")
	c, cur := parseBytes(t, p, "/p/s-cwds.jsonl", b, transcript.Cursor{})
	conv := lastConv(t, c)
	if conv.Cwd != "/work/app" || !reflect.DeepEqual(conv.OtherCwds, []string{"/work/app/sub"}) {
		t.Fatalf("cwd %q other %v", conv.Cwd, conv.OtherCwds)
	}
	b2 := append(b, []byte(line(4, "/work/secret")+"\n"+line(5, "/work/app/sub")+"\n")...)
	c, _ = parseBytes(t, p, "/p/s-cwds.jsonl", b2, cur)
	if got := lastConv(t, c).OtherCwds; !reflect.DeepEqual(got, []string{"/work/app/sub", "/work/secret"}) {
		t.Fatalf("after append: %v", got)
	}
}
