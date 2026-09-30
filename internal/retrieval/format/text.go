package format

import (
	"strings"
	"unicode/utf8"
)

// Clean makes transcript text safe to print on a terminal (decision D17):
// C0 control characters other than tab and newline become their Unicode
// control pictures (ESC is ␛), DEL is ␡, C1 controls and the bidi control
// characters (isBidiControl) become U+FFFD, a CR before a newline is dropped, and invalid
// UTF-8 is replaced. JSON output never goes through Clean; it is exact.
func Clean(s string) string {
	clean := true
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 && c != '\t' && c != '\n' || c == 0x7f || c >= 0x80 {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	s = strings.ToValidUTF8(s, "�")
	var b strings.Builder
	b.Grow(len(s))
	for i, r := range s {
		switch {
		case r == '\t' || r == '\n':
			b.WriteRune(r)
		case r == '\r' && i+1 < len(s) && s[i+1] == '\n':
		case r < 0x20:
			b.WriteRune(0x2400 + r)
		case r == 0x7f:
			b.WriteRune('␡')
		case r >= 0x80 && r < 0xa0, isBidiControl(r):
			b.WriteRune('�')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isBidiControl reports the Unicode Bidi_Control characters: the marks
// ALM, LRM and RLM, the embeddings and overrides, and the isolates. Each
// can reorder how a terminal shows the text around it.
func isBidiControl(r rune) bool {
	return r == 0x061c || r == 0x200e || r == 0x200f || r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069
}

// Excerpt is the part of a text read shows.
type Excerpt struct {
	Text     string
	From, To int  // lines shown, 1-based, inclusive
	Lines    int  // lines in the text
	Clipped  bool // lines or characters were left out
}

// Cut returns the lines of text from line from (1-based; 0 means 1) that
// fit in maxChars bytes; a first line longer than that is cut inside, at
// a rune boundary. maxChars <= 0 means no limit.
func Cut(text string, from, maxChars int) Excerpt {
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	e := Excerpt{Lines: len(lines), From: min(max(from, 1), len(lines))}
	var b strings.Builder
	e.To = e.From - 1
	for i := e.From - 1; i < len(lines); i++ {
		l := lines[i]
		first := i == e.From-1
		need := len(l)
		if !first {
			need++
		}
		if maxChars > 0 && b.Len()+need > maxChars {
			if first {
				b.WriteString(clip(l, maxChars))
				e.To = i + 1
			}
			e.Clipped = true
			break
		}
		if !first {
			b.WriteByte('\n')
		}
		b.WriteString(l)
		e.To = i + 1
	}
	e.Text = b.String()
	if e.From > 1 || e.To < e.Lines {
		e.Clipped = true
	}
	return e
}

// clip cuts s to at most n bytes at a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// ClipAround cuts a line to about n bytes around byte col, marking cut
// ends with "…".
func ClipAround(line string, col, n int) string {
	if len(line) <= n {
		return line
	}
	from := max(0, min(col-n/3, len(line)-n))
	for from > 0 && !utf8.RuneStart(line[from]) {
		from--
	}
	to := min(len(line), from+n)
	for to < len(line) && !utf8.RuneStart(line[to]) {
		to++
	}
	out := line[from:to]
	if from > 0 {
		out = "…" + out
	}
	if to < len(line) {
		out += "…"
	}
	return out
}
