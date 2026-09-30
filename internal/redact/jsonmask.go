package redact

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Message redaction after the fact (notes/redaction.md, "Redacting a
// message after the fact"): given a record's raw bytes and what to hide,
// find the raw spans to mask so that the record stays valid JSON at every
// nesting depth and keeps its structure (types, ids, timestamps), and fill
// them with a length-preserving marker.

// Span is a byte range [Start, End).
type Span struct {
	Start int `json:"s"`
	End   int `json:"e"`
}

// MessageRule names the marker of a redacted message.
const MessageRule = "message"

// structural keys keep their values in a whole-record mask: they identify
// and order records, and never carry conversation text.
var structural = map[string]bool{
	"type": true, "t": true, "uuid": true, "parentUuid": true, "sessionId": true, "session_id": true, "timestamp": true,
	"id": true, "role": true, "message_id": true, "call_id": true, "tool_use_id": true, "tool_call_id": true, "toolCallId": true,
	"name": true, "kind": true, "status": true, "model": true, "requestId": true, "agentId": true, "userType": true,
	"version": true, "created_at": true, "last_activity_at": true, "node_id": true, "parent_node_id": true,
}

// MaskRecord returns the raw spans of rec to mask. whole masks every
// string value except structural ones; otherwise every occurrence of each
// needle inside a decoded string value is masked. JSON held inside a
// string (a Devin chat_message, Codex call arguments) is walked the same
// way, so it stays valid too. A record that is not JSON (a tool output
// file) is searched as plain text, or masked entirely when whole. found
// reports whether every needle was found at least once.
func MaskRecord(rec []byte, needles []string, whole bool) (spans []Span, found bool) {
	t := bytes.TrimSpace(rec)
	if len(t) == 0 || (t[0] != '{' && t[0] != '[') || !json.Valid(t) {
		if whole {
			return []Span{{0, len(rec)}}, true
		}
		seen := make([]bool, len(needles))
		for i, n := range needles {
			for off := 0; n != ""; {
				j := bytes.Index(rec[off:], []byte(n))
				if j < 0 {
					break
				}
				spans = append(spans, Span{off + j, off + j + len(n)})
				seen[i] = true
				off += j + len(n)
			}
		}
		return mergeSpans(spans), allTrue(seen)
	}
	seen := make([]bool, len(needles))
	identity := func(i int) int { return i }
	walkJSON(rec, identity, func(key string, dec []byte, pos func(int) int) {
		if whole {
			if !structural[key] && len(dec) > 0 {
				spans = append(spans, Span{pos(0), pos(len(dec))})
			}
			return
		}
		for i, n := range needles {
			for off := 0; n != ""; {
				j := bytes.Index(dec[off:], []byte(n))
				if j < 0 {
					break
				}
				spans = append(spans, Span{pos(off + j), pos(off + j + len(n))})
				seen[i] = true
				off += j + len(n)
			}
		}
	})
	return mergeSpans(spans), allTrue(seen)
}

// walkJSON calls visit for every string value in b with its key, its
// decoded bytes and a map from decoded offsets to offsets in the
// outermost buffer (through outer, which maps b's offsets). A value that
// is itself JSON is walked instead of visited.
func walkJSON(b []byte, outer func(int) int, visit func(key string, dec []byte, pos func(int) int)) {
	key := ""
	for i := 0; i < len(b); i++ {
		if b[i] != '"' {
			continue
		}
		j := i + 1
		for j < len(b) && b[j] != '"' {
			if b[j] == '\\' {
				j++
			}
			j++
		}
		if j >= len(b) {
			return
		}
		raw := b[i+1 : j]
		k := j + 1
		for k < len(b) && (b[k] == ' ' || b[k] == '\t' || b[k] == '\n' || b[k] == '\r') {
			k++
		}
		dec, at := unescape(raw)
		start := i + 1
		pos := func(d int) int {
			if d >= len(at) {
				return outer(start + len(raw))
			}
			return outer(start + at[d])
		}
		if k < len(b) && b[k] == ':' {
			key = string(dec)
		} else if t := bytes.TrimSpace(dec); len(t) > 1 && (t[0] == '{' || t[0] == '[') && json.Valid(t) {
			walkJSON(dec, pos, visit)
		} else {
			visit(key, dec, pos)
		}
		i = j
	}
}

// unescape decodes a JSON string body and returns, for each decoded byte,
// the offset in raw where the escape or character that produced it
// starts.
func unescape(raw []byte) ([]byte, []int) {
	dec := make([]byte, 0, len(raw))
	at := make([]int, 0, len(raw))
	for i := 0; i < len(raw); {
		start := i
		c := raw[i]
		if c != '\\' || i+1 >= len(raw) {
			_, n := utf8.DecodeRune(raw[i:])
			for k := 0; k < n; k++ {
				dec = append(dec, raw[i+k])
				at = append(at, start)
			}
			i += n
			continue
		}
		var r rune
		switch e := raw[i+1]; e {
		case 'u':
			if i+6 > len(raw) {
				r, i = utf8.RuneError, len(raw)
				break
			}
			r = rune(hexVal(raw[i+2 : i+6]))
			i += 6
			if r >= 0xD800 && r < 0xDC00 && i+6 <= len(raw) && raw[i] == '\\' && raw[i+1] == 'u' {
				lo := rune(hexVal(raw[i+2 : i+6]))
				if lo >= 0xDC00 && lo < 0xE000 {
					r = (r-0xD800)<<10 + (lo - 0xDC00) + 0x10000
					i += 6
				}
			}
		case 'n':
			r, i = '\n', i+2
		case 't':
			r, i = '\t', i+2
		case 'r':
			r, i = '\r', i+2
		case 'b':
			r, i = '\b', i+2
		case 'f':
			r, i = '\f', i+2
		default:
			r, i = rune(e), i+2
		}
		var buf [4]byte
		n := utf8.EncodeRune(buf[:], r)
		for k := 0; k < n; k++ {
			dec = append(dec, buf[k])
			at = append(at, start)
		}
	}
	return dec, at
}

func hexVal(h []byte) int {
	v := 0
	for _, c := range h {
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v |= int(c - '0')
		case c >= 'a' && c <= 'f':
			v |= int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v |= int(c-'A') + 10
		}
	}
	return v
}

func mergeSpans(s []Span) []Span {
	if len(s) == 0 {
		return nil
	}
	out := []Span{}
	for _, x := range sortSpans(s) {
		if n := len(out); n > 0 && x.Start <= out[n-1].End {
			out[n-1].End = max(out[n-1].End, x.End)
			continue
		}
		out = append(out, x)
	}
	return out
}

func sortSpans(s []Span) []Span {
	out := append([]Span(nil), s...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Start < out[j-1].Start; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func allTrue(b []bool) bool {
	for _, v := range b {
		if !v {
			return false
		}
	}
	return true
}

// FillSpans writes the message marker over each span of b (in place).
// Every span must lie within b.
func FillSpans(b []byte, spans []Span, rule string) {
	for _, s := range spans {
		marker(b[s.Start:s.End], nil, rule, false)
	}
}

// MaskText masks lines [from, to] (1-based, as read numbers them) of
// text, or all of it when from is 0, with the message marker, keeping
// every line's length. It returns the masked text and the original lines
// it hid, the needles for MaskRecord.
func MaskText(text string, from, to int) (string, []string) {
	if from == 0 {
		b := []byte(text)
		marker(b, nil, MessageRule, false)
		return string(b), nil
	}
	lines := strings.Split(text, "\n")
	var hidden []string
	for i := from - 1; i < to && i < len(lines); i++ {
		if lines[i] == "" {
			continue
		}
		hidden = append(hidden, lines[i])
		b := []byte(lines[i])
		marker(b, nil, MessageRule, false)
		lines[i] = string(b)
	}
	return strings.Join(lines, "\n"), hidden
}
