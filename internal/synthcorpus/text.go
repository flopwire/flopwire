package synthcorpus

import (
	"math"
	"math/rand/v2"
	"strconv"
	"time"
)

// techWords are common words of real transcripts. None holds a q, x, z or
// j, so a needle that holds one of those letters never occurs by chance.
var techWords = []string{
	"error", "retry", "upload", "index", "cursor", "digest", "parser", "bucket", "schema", "token",
	"window", "sync", "chunk", "ledger", "branch", "kernel", "harbor", "test", "build", "commit",
	"merge", "deploy", "config", "server", "client", "request", "response", "handler", "module", "package",
	"function", "return", "value", "string", "struct", "pointer", "slice", "channel", "context", "timeout",
	"cache", "query", "insert", "update", "delete", "select", "table", "column", "migration", "worker",
	"agent", "session", "message", "stream", "socket", "listen", "connect", "failed", "passed", "warning",
	"debug", "trace", "metric", "latency", "memory", "goroutine", "mutex", "lock", "atomic", "file",
	"path", "directory", "read", "write", "open", "close", "flush", "offset", "length", "header",
	"payload", "encode", "decode", "parse", "format", "print", "log", "output", "input", "result",
}

// vocab is techWords plus pronounceable filler words built from a fixed
// syllable set (no q, x, z, j). It is the same for every seed.
var vocab = func() []string {
	cons := "bcdfghklmnprstvw"
	vows := "aeiou"
	out := append([]string(nil), techWords...)
	r := rand.New(rand.NewPCG(1, 2))
	for len(out) < 1500 {
		n := 2 + r.IntN(3)
		var b []byte
		for range n {
			b = append(b, cons[r.IntN(len(cons))], vows[r.IntN(len(vows))])
		}
		out = append(out, string(b))
	}
	return out
}()

// b64 is a fixed block of base64 characters; image data and encrypted
// blobs are slices of it.
var b64 = func() []byte {
	const alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	r := rand.New(rand.NewPCG(3, 4))
	b := make([]byte, 1<<20)
	for i := range b {
		b[i] = alpha[r.IntN(len(alpha))]
	}
	return b
}()

// rng wraps the per-file generator with the helpers the writers use.
type rng struct{ *rand.Rand }

func newRNG(seed, stream uint64) rng {
	return rng{rand.New(rand.NewPCG(seed, stream*0x9e3779b97f4a7c15+0x632be59bd9b4e019))}
}

// word returns a vocabulary word, skewed towards the front of the list so
// tech words are common and filler words rare, like real text.
func (r rng) word() string {
	u := r.Float64()
	return vocab[int(u*u*u*float64(len(vocab)))]
}

// prose appends about n bytes of space-separated words (no JSON escaping
// needed).
func (r rng) prose(dst []byte, n int) []byte {
	end := len(dst) + n
	first := true
	for len(dst) < end {
		if !first {
			dst = append(dst, ' ')
		}
		first = false
		dst = append(dst, r.word()...)
	}
	return dst
}

// output appends about n bytes of tool output: file:line: words, one per
// line. Unless raw, newlines are written as \n (a JSON string body).
func (r rng) output(dst []byte, n int, raw bool) []byte {
	end := len(dst) + n
	for len(dst) < end {
		dst = append(dst, "src/"...)
		dst = append(dst, r.word()...)
		dst = append(dst, '/')
		dst = append(dst, r.word()...)
		dst = append(dst, ".go:"...)
		dst = strconv.AppendInt(dst, int64(1+r.IntN(900)), 10)
		dst = append(dst, ": "...)
		dst = r.prose(dst, 20+r.IntN(80))
		if raw {
			dst = append(dst, '\n')
		} else {
			dst = append(dst, `\n`...)
		}
	}
	return dst
}

// blob appends n base64 characters.
func (r rng) blob(dst []byte, n int) []byte {
	for n > 0 {
		off := r.IntN(len(b64) / 2)
		k := min(n, len(b64)-off)
		dst = append(dst, b64[off:off+k]...)
		n -= k
	}
	return dst
}

// lognormal draws exp(N(ln median, sigma)).
func (r rng) lognormal(median, sigma float64) float64 {
	return median * math.Exp(sigma*r.NormFloat64())
}

// logUniform draws from [lo, hi) with a uniform logarithm.
func (r rng) logUniform(lo, hi float64) float64 {
	return lo * math.Exp(r.Float64()*math.Log(hi/lo))
}

// uuid4 is a random-looking v4 uuid.
func (r rng) uuid4() string {
	return hex(r.Uint64(), 8) + "-" + hex(r.Uint64(), 4) + "-4" + hex(r.Uint64(), 3) + "-8" + hex(r.Uint64(), 3) + "-" + hex(r.Uint64(), 12)
}

// uuid7 is a v7-shaped uuid like Codex session ids.
func (r rng) uuid7() string {
	return "019a" + hex(r.Uint64(), 4) + "-" + hex(r.Uint64(), 4) + "-7" + hex(r.Uint64(), 3) + "-8" + hex(r.Uint64(), 3) + "-" + hex(r.Uint64(), 12)
}

func hex(v uint64, n int) string {
	const digits = "0123456789abcdef"
	b := make([]byte, n)
	for i := n - 1; i >= 0; i-- {
		b[i] = digits[v&15]
		v >>= 4
	}
	return string(b)
}

// appendJSON appends s as a JSON string literal.
func appendJSON(dst []byte, s string) []byte {
	dst = append(dst, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			dst = append(dst, '\\', c)
		case c == '\n':
			dst = append(dst, '\\', 'n')
		case c == '\t':
			dst = append(dst, '\\', 't')
		case c < 0x20:
			dst = append(dst, `\u00`...)
			dst = append(dst, "0123456789abcdef"[c>>4], "0123456789abcdef"[c&15])
		default:
			dst = append(dst, c)
		}
	}
	return append(dst, '"')
}

func appendTS(dst []byte, t time.Time) []byte {
	return t.UTC().AppendFormat(dst, `"2006-01-02T15:04:05.000Z"`)
}
