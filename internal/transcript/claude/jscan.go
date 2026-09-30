package claude

import (
	"bytes"
	"encoding/json"
	"errors"
)

// A minimal JSON walker. encoding/json validates a whole line before
// decoding it and validates again for every nested Unmarshal, so a line
// with a large toolUseResult mirror (a third of the corpus bytes) is
// scanned three or more times. The walker visits an object's members once,
// hands each value back as a borrowed slice, and skips unwanted values by
// bracket counting. It checks structure (balanced brackets, terminated
// strings, member syntax), not the grammar inside skipped values; string
// values that are kept are unquoted with encoding/json whenever they
// contain an escape.

var errSyntax = errors.New("claude: malformed JSON")

func skipWS(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// skipString returns the index just past the string starting at b[i] == '"'.
func skipString(b []byte, i int) (int, error) {
	start := i
	i++
	for {
		j := bytes.IndexByte(b[i:], '"')
		if j < 0 {
			return 0, errSyntax
		}
		k := i + j
		n := 0
		for p := k - 1; p > start && b[p] == '\\'; p-- {
			n++
		}
		if n%2 == 0 {
			return k + 1, nil
		}
		i = k + 1
	}
}

// skipValue returns the index just past the value starting at or after b[i].
func skipValue(b []byte, i int) (int, error) {
	i = skipWS(b, i)
	if i >= len(b) {
		return 0, errSyntax
	}
	switch b[i] {
	case '"':
		return skipString(b, i)
	case '{', '[':
		depth := 0
		for i < len(b) {
			switch b[i] {
			case '"':
				j, err := skipString(b, i)
				if err != nil {
					return 0, err
				}
				i = j
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1, nil
				}
			}
			i++
		}
		return 0, errSyntax
	case '}', ']', ',', ':':
		return 0, errSyntax
	default: // number, true, false, null
		j := i
		for j < len(b) {
			switch b[j] {
			case ',', '}', ']', ' ', '\t', '\r', '\n':
				return j, nil
			}
			j++
		}
		return j, nil
	}
}

// eachField calls fn for every member of the object b. Keys are raw (the
// keys the parser looks for never contain escapes).
func eachField(b []byte, fn func(key, val []byte)) error {
	i := skipWS(b, 0)
	if i >= len(b) || b[i] != '{' {
		return errSyntax
	}
	i = skipWS(b, i+1)
	if i < len(b) && b[i] == '}' {
		return nil
	}
	for {
		if i >= len(b) || b[i] != '"' {
			return errSyntax
		}
		ke, err := skipString(b, i)
		if err != nil {
			return err
		}
		key := b[i+1 : ke-1]
		i = skipWS(b, ke)
		if i >= len(b) || b[i] != ':' {
			return errSyntax
		}
		vs := skipWS(b, i+1)
		ve, err := skipValue(b, vs)
		if err != nil {
			return err
		}
		fn(key, b[vs:ve])
		i = skipWS(b, ve)
		if i >= len(b) {
			return errSyntax
		}
		switch b[i] {
		case ',':
			i = skipWS(b, i+1)
		case '}':
			return nil
		default:
			return errSyntax
		}
	}
}

// eachElem calls fn for every element of the array b.
func eachElem(b []byte, fn func(val []byte)) error {
	i := skipWS(b, 0)
	if i >= len(b) || b[i] != '[' {
		return errSyntax
	}
	i = skipWS(b, i+1)
	if i < len(b) && b[i] == ']' {
		return nil
	}
	for {
		vs := skipWS(b, i)
		ve, err := skipValue(b, vs)
		if err != nil {
			return err
		}
		fn(b[vs:ve])
		i = skipWS(b, ve)
		if i >= len(b) {
			return errSyntax
		}
		switch b[i] {
		case ',':
			i++
		case ']':
			return nil
		default:
			return errSyntax
		}
	}
}

// unquote decodes a JSON string value; ok is false for any other value.
func unquote(v []byte) (string, bool) {
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return "", false
	}
	inner := v[1 : len(v)-1]
	if bytes.IndexByte(inner, '\\') < 0 {
		return string(inner), true
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		return "", false
	}
	return s, true
}

func str(v []byte) string {
	s, _ := unquote(v)
	return s
}

func isTrue(v []byte) bool { return string(v) == "true" }
