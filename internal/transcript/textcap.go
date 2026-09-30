package transcript

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// CapConfig bounds the text stored for one message kind (spec §4.2): keep
// Head bytes, Tail bytes, and from the middle every line matching one of
// Keep, up to MiddleBudget bytes. Head == 0 and Tail == 0 means uncapped.
type CapConfig struct {
	Head, Tail   int
	MiddleBudget int              // total bytes of kept middle lines
	MaxLineLen   int              // a kept middle line is truncated to this
	Keep         []*regexp.Regexp // middle lines worth keeping
}

// Uncapped stores text whole.
var Uncapped = CapConfig{}

// MiddlePatterns match lines worth keeping from the elided middle of tool
// text: errors, exceptions, path:line references and exit codes.
var MiddlePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(error|errors|exception|traceback|panic|fatal|failed|failure)\b`),
	regexp.MustCompile(`[\w.\-/]+\.\w+:\d+`),
	regexp.MustCompile(`(?i)\bexit(ed)?( with)? (code|status)[ :=]*-?\d+`),
}

// ToolCap is the default cap for tool calls and tool results. Decision D3
// indexes tool text whole, so this is only a safety bound of about 1MB per
// row: on the real corpus 99.99% of tool rows are under 280KB, and the few
// larger ones are multi-megabyte command dumps (base64, logs, JSON).
var ToolCap = CapConfig{Head: 768 << 10, Tail: 192 << 10, MiddleBudget: 64 << 10, MaxLineLen: 240, Keep: MiddlePatterns}

// DefaultCaps is the per-kind default. Kinds not listed are uncapped.
var DefaultCaps = map[Kind]CapConfig{
	KindToolCall:   ToolCap,
	KindToolResult: ToolCap,
}

// CapFor returns the cap for k from caps, falling back to Uncapped.
func CapFor(caps map[Kind]CapConfig, k Kind) CapConfig {
	if c, ok := caps[k]; ok {
		return c
	}
	return Uncapped
}

// Cap applies cfg to text and reports whether anything was elided. Cuts
// fall on UTF-8 boundaries. Elided spans are replaced by a marker line
// "[… N bytes elided …]".
func Cap(text string, cfg CapConfig) (string, bool) {
	if cfg.Head <= 0 && cfg.Tail <= 0 {
		return text, false
	}
	if len(text) <= cfg.Head+cfg.Tail+cfg.MiddleBudget {
		return text, false
	}
	headEnd := runeFloor(text, cfg.Head)
	tailStart := runeCeil(text, len(text)-cfg.Tail)
	middle := text[headEnd:tailStart]

	var b strings.Builder
	b.Grow(cfg.Head + cfg.Tail + cfg.MiddleBudget + 128)
	b.WriteString(text[:headEnd])
	elided, budget := 0, cfg.MiddleBudget
	for len(middle) > 0 {
		line := middle
		if i := strings.IndexByte(middle, '\n'); i >= 0 {
			line, middle = middle[:i+1], middle[i+1:]
		} else {
			middle = ""
		}
		kept := strings.TrimRight(line, "\r\n")
		if cfg.MaxLineLen > 0 && len(kept) > cfg.MaxLineLen {
			kept = kept[:runeFloor(kept, cfg.MaxLineLen)]
		}
		if budget < len(kept)+1 || !matchesAny(cfg.Keep, line) {
			elided += len(line)
			continue
		}
		writeElided(&b, elided)
		elided = 0
		b.WriteString(kept)
		b.WriteByte('\n')
		budget -= len(kept) + 1
	}
	writeElided(&b, elided)
	b.WriteString(text[tailStart:])
	return b.String(), true
}

func writeElided(b *strings.Builder, n int) {
	if n > 0 {
		fmt.Fprintf(b, "\n[… %d bytes elided …]\n", n)
	}
}

func matchesAny(res []*regexp.Regexp, s string) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// runeFloor returns the largest rune boundary <= i.
func runeFloor(s string, i int) int {
	if i >= len(s) {
		return len(s)
	}
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

// runeCeil returns the smallest rune boundary >= i.
func runeCeil(s string, i int) int {
	if i <= 0 {
		return 0
	}
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}
