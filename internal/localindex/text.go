package localindex

import (
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"

	"github.com/flopwire/flopwire/internal/transcript"
)

var (
	// Rows are small and compressed on every parse worker at once: one
	// encoder per concurrent caller, each with a small window (the default
	// 8MB window allocates a 16MB history per encoder). The encoders sit in
	// a sync.Pool, so an idle agent's GC drops them (about 1.4MB each).
	zencPool = sync.Pool{New: func() any {
		e, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1),
			zstd.WithWindowSize(256<<10), zstd.WithLowerEncoderMem(true), zstd.WithZeroFrames(true))
		return e
	}}
	zdec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0), zstd.WithDecoderLowmem(true))
)

func compress(s string) []byte {
	enc := zencPool.Get().(*zstd.Encoder)
	defer zencPool.Put(enc)
	// EncodeAll sizes its output for the input; rows are kept until their
	// batch commits, so hold only what the frame needs.
	z := enc.EncodeAll([]byte(s), nil)
	if cap(z) > len(z)+len(z)/4+64 {
		z = append([]byte(nil), z...)
	}
	return z
}

// prepared is a message's text in the forms the writer stores. The Sink
// computes it on the parse worker, so the single writer goroutine only
// runs SQL. Both FTS tables index the whole stored text (decision D3: no
// trigram cap); the fts_tok shard applies tokText itself.
type prepared struct {
	z    []byte // zstd of the stored text
	text string
}

func prepareAll(msgs []*transcript.Message) []prepared {
	out := make([]prepared, len(msgs))
	for i, m := range msgs {
		out[i] = prepared{z: compress(m.Text), text: m.Text}
	}
	return out
}

func decompress(b []byte) (string, error) {
	out, err := zdec.DecodeAll(b, nil)
	return string(out), err
}

// isTokChar mirrors fts_tok's tokenizer: unicode61 token characters
// (letters, numbers, private use) plus the configured tokenchars '_-./'.
func isTokChar(r rune) bool {
	switch r {
	case '_', '-', '.', '/':
		return true
	}
	return unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.Is(unicode.Co, r)
}

// trimLead and trimTrail are stripped from each token before indexing and
// querying, so sentence punctuation and relative-path prefixes do not glue
// onto words: "done." -> "done", "./scripts/x.sh" -> "scripts/x.sh",
// "internal/" -> "internal". A leading '-' is kept so flags stay whole.
const (
	trimLead  = "./"
	trimTrail = ".-/"
)

// tokenize splits s the way fts_tok does and applies the trim rules.
func tokenize(s string, fn func(tok string)) {
	start := -1
	for i, r := range s {
		if isTokChar(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			if t := trimTok(s[start:i]); t != "" {
				fn(t)
			}
			start = -1
		}
	}
	if start >= 0 {
		if t := trimTok(s[start:]); t != "" {
			fn(t)
		}
	}
}

func trimTok(t string) string {
	return strings.TrimRight(strings.TrimLeft(t, trimLead), trimTrail)
}

// tokText is the text fed to fts_tok: the capped text with the trim rules
// applied (trimmed characters become spaces, so the tokenizer sees the same
// tokens tokenize reports).
func tokText(s string) string {
	b := []byte(s)
	changed := false
	blank := func(from, to int) {
		for j := from; j < to; j++ {
			b[j] = ' '
		}
		changed = true
	}
	start := -1
	flush := func(end int) {
		t := s[start:end]
		lead := len(t) - len(strings.TrimLeft(t, trimLead))
		if lead == len(t) {
			blank(start, end)
			return
		}
		trail := len(t) - len(strings.TrimRight(t, trimTrail))
		if lead > 0 {
			blank(start, start+lead)
		}
		if trail > 0 {
			blank(end-trail, end)
		}
	}
	for i, r := range s {
		if isTokChar(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			flush(i)
			start = -1
		}
	}
	if start >= 0 {
		flush(len(s))
	}
	if !changed {
		return s
	}
	return string(b)
}

// ftsString quotes s as an FTS5 string literal.
func ftsString(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// tokQuery turns free text into an fts_tok MATCH expression: every token
// quoted and joined with AND (or OR when any is set). A token written with
// a trailing '*' becomes a prefix query. Returns "" when the text has no
// tokens. Phrases are not used, so the expression works at either detail.
func tokQuery(text string, any bool) (expr string, terms []string) {
	var parts []string
	for _, field := range strings.Fields(text) {
		prefix := strings.HasSuffix(field, "*")
		field = strings.TrimSuffix(field, "*")
		var toks []string
		tokenize(field, func(t string) { toks = append(toks, t) })
		for i, t := range toks {
			terms = append(terms, t)
			p := ftsString(t)
			if prefix && i == len(toks)-1 {
				p += " *"
			}
			parts = append(parts, p)
		}
	}
	sep := " AND "
	if any {
		sep = " OR "
	}
	return strings.Join(parts, sep), terms
}

// runeLen counts runes; trigram matching needs at least three.
func runeLen(s string) int { return utf8.RuneCountInString(s) }
