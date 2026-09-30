package transcript

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCapShortTextUnchanged(t *testing.T) {
	s := strings.Repeat("ok\n", 100)
	if got, cut := Cap(s, ToolCap); cut || got != s {
		t.Fatal("short text was capped")
	}
	long := strings.Repeat("y", 1<<20)
	if got, cut := Cap(long, Uncapped); cut || got != long {
		t.Fatal("Uncapped capped text")
	}
}

func TestCapKeepsHeadTailAndSignalLines(t *testing.T) {
	var b strings.Builder
	b.WriteString("HEAD-START\n")
	for i := 0; i < 5000; i++ {
		switch i {
		case 1000:
			b.WriteString("internal/store/db.go:42: undefined: Foo\n")
		case 2000:
			b.WriteString("panic: runtime error: index out of range\n")
		case 3000:
			b.WriteString("process exited with code 2\n")
		default:
			fmt.Fprintf(&b, "compiling package %d\n", i)
		}
	}
	b.WriteString("TAIL-END")
	s := b.String()
	cfg := CapConfig{Head: 3 << 10, Tail: 1 << 10, MiddleBudget: 2 << 10, MaxLineLen: 240, Keep: MiddlePatterns}
	got, cut := Cap(s, cfg)
	if !cut {
		t.Fatal("expected cap")
	}
	for _, want := range []string{"HEAD-START", "TAIL-END", "db.go:42: undefined", "panic: runtime error", "exited with code 2", "bytes elided"} {
		if !strings.Contains(got, want) {
			t.Errorf("capped text lacks %q", want)
		}
	}
	if strings.Contains(got, "compiling package 1500") {
		t.Error("unmatched middle line kept")
	}
	if max := cfg.Head + cfg.Tail + cfg.MiddleBudget + 200; len(got) > max {
		t.Errorf("capped length %d exceeds %d", len(got), max)
	}
}

// Decision D3: tool text is indexed whole, so find reaches the middle of a
// long tool output. ToolCap only bounds pathological rows at about 1MB.
func TestToolCapKeepsLongOutputWhole(t *testing.T) {
	var b strings.Builder
	for i := 0; b.Len() < 300<<10; i++ {
		fmt.Fprintf(&b, "line %d of a long listing\n", i)
	}
	s := b.String()
	if got, cut := Cap(s, ToolCap); cut || got != s {
		t.Fatalf("300KB tool output capped to %d bytes", len(got))
	}
	huge := strings.Repeat("0123456789abcdef\n", 8<<16) // 8.5MB
	got, cut := Cap(huge, ToolCap)
	if !cut || len(got) > 1<<20+200 {
		t.Fatalf("8MB tool output: cut=%v len=%d", cut, len(got))
	}
}

func TestCapRespectsUTF8AndBudget(t *testing.T) {
	s := strings.Repeat("日本語エラー error line\n", 2000)
	cfg := CapConfig{Head: 1001, Tail: 499, MiddleBudget: 300, MaxLineLen: 10, Keep: MiddlePatterns}
	got, cut := Cap(s, cfg)
	if !cut || !utf8.ValidString(got) {
		t.Fatalf("cut=%v valid=%v", cut, utf8.ValidString(got))
	}
	if len(got) > 1001+499+300+2*40 {
		t.Fatalf("budget exceeded: %d", len(got))
	}
}

func TestSetTextHashesUncapped(t *testing.T) {
	a := strings.Repeat("a", 10000) + "1"
	b := strings.Repeat("a", 10000) + "2"
	var ma, mb Message
	ma.SetText(a, CapConfig{Head: 10, Tail: 0})
	mb.SetText(b, CapConfig{Head: 10, Tail: 0})
	if ma.Text != mb.Text || ma.ContentSHA == mb.ContentSHA || ma.FullLen != len(a) {
		t.Fatal("ContentSHA must cover the uncapped text")
	}
}
