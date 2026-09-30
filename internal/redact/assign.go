package redact

import (
	"bytes"
)

// The keyword-assignment rule: a key whose name ends in a secret word,
// a separator, and a value (env dumps, .env files, JSON, YAML, CLI
// flags):
//
//	GITHUB_TOKEN=abc123...   "client_secret": "..."   password: s3cr3t!x
//	--api-key=...            apiKey = '...'           DB_PASSWORD := ...
//
// It is procedural rather than one regex: "token" and "key" occur on most
// transcript lines, so the matcher finds keyword occurrences in a
// lowercased copy and checks the few bytes around each one, which keeps
// the whole rule close to the cost of a substring search.

const assignRule = "assignment"

// assignWords end a secret key name. A key must end with one of them: the
// byte after it is not alphanumeric, so "max_tokens", "tokenizer" and
// "password_hash" do not qualify.
var assignWords = [][]byte{
	[]byte("secret"), []byte("token"), []byte("password"), []byte("passwd"),
	[]byte("apikey"), []byte("api_key"), []byte("api-key"), []byte("accesskey"), []byte("access_key"), []byte("access-key"),
	[]byte("privatekey"), []byte("private_key"), []byte("private-key"), []byte("secretkey"), []byte("secret_key"), []byte("secret-key"),
	[]byte("signing_key"), []byte("signingkey"), []byte("encryption_key"), []byte("master_key"), []byte("auth_key"),
	[]byte("credential"), []byte("credentials"),
}

// assignAt checks an assignment word found at b[i:j] (any case) and
// returns the value it assigns, if that looks like a secret.
func assignAt(b []byte, i, j int) (Match, bool) {
	if j < len(b) && isAlnum(b[j]) {
		return Match{}, false // the key goes on: "tokens", "secretName"
	}
	ks := keyStart(b, i)
	if containsFold(b[ks:i], "public") {
		return Match{}, false // SSH_PUBLIC_KEY, publicKey
	}
	if bytes.HasSuffix(b[:ks], []byte("[REDACTED:")) {
		return Match{}, false // a marker's rule name: [REDACTED:github-token:1a2b3c4d]
	}
	s, e, ok := assignValue(b, j)
	if !ok {
		return Match{}, false
	}
	// Never hashed: a value here may be a human-chosen password, which a
	// 32-bit hash would let anyone confirm by guessing (character entropy
	// does not tell a random key from "Winter2024!Jackson#Blue").
	return Match{Start: s, End: e, Rule: assignRule, prio: len(rules)}, true
}

func containsFold(b []byte, lower string) bool {
	for i := 0; i+len(lower) <= len(b); i++ {
		k := 0
		for k < len(lower) && fold(b[i+k]) == lower[k] {
			k++
		}
		if k == len(lower) {
			return true
		}
	}
	return false
}

// keyStart walks back over the key name that ends in the word at i.
func keyStart(b []byte, i int) int {
	k := i
	for k > 0 && i-k < 64 && keyByte(b[k-1]) {
		k--
	}
	return k
}

func keyByte(c byte) bool {
	return isAlnum(c) || c == '_' || c == '-' || c == '.'
}

// assignValue parses the separator and value after a key that ends at j,
// and returns the value's span if it looks like a secret.
func assignValue(b []byte, j int) (int, int, bool) {
	k := j
	k = skipQuote(b, k)
	k = skipSpace(b, k)
	switch {
	case k < len(b) && b[k] == '=' && (k+1 >= len(b) || b[k+1] != '=' && b[k+1] != '~'):
		k++
		if k < len(b) && b[k] == '>' {
			k++
		}
	case k+1 < len(b) && b[k] == ':' && b[k+1] == '=':
		k += 2
	case k < len(b) && b[k] == ':' && (k+1 >= len(b) || b[k+1] != ':'):
		k++
	default:
		return -1, -1, false
	}
	k = skipSpace(b, k)
	q, esc := byte(0), 0
	if qk := skipQuote(b, k); qk > k {
		q, esc = b[qk-1], qk-1-k
		k = qk
	}
	s := k
	if q != 0 {
		k = quotedEnd(b, k, q, esc)
	} else {
		for k < len(b) && k-s < 256 && !valueEnd(b[k]) {
			k++
		}
	}
	if k-s >= 256 {
		return -1, -1, false // a blob, not a credential
	}
	if k < len(b) && b[k] == '(' {
		return -1, -1, false // a call: getenv(...)
	}
	if !assignValueOK(b[s:k]) {
		return -1, -1, false
	}
	return s, k, true
}

func skipSpace(b []byte, k int) int {
	for n := 0; k < len(b) && n < 3 && (b[k] == ' ' || b[k] == '\t'); n++ {
		k++
	}
	return k
}

// skipQuote skips one quote, escaped by up to three backslashes (a JSON
// string inside a JSON string).
func skipQuote(b []byte, k int) int {
	i := k
	for n := 0; i < len(b) && n < 3 && b[i] == '\\'; n++ {
		i++
	}
	if i < len(b) && (b[i] == '"' || b[i] == '\'' || b[i] == '`') {
		return i + 1
	}
	return k
}

// quotedEnd returns the end of a quoted value that starts at k: the
// backslash run of its closing quote. esc is the number of backslashes
// before the opening quote (0 in plain JSON, 1 in a JSON string inside a
// JSON string, 3 one level deeper), so an escaped quote or backslash in
// the value (\", \\) stays part of it, as do the separators an unquoted
// value stops at. Whitespace and control bytes still end it (prose, not a
// credential); the 256-byte cap applies as for unquoted values.
func quotedEnd(b []byte, k int, q byte, esc int) int {
	s, unit := k, esc+1 // at nesting depth d, esc = 2^d-1
	for k < len(b) && k-s < 256 {
		c := b[k]
		if c == ' ' || c == '\t' || c < 0x20 {
			return k
		}
		if c != '\\' && c != q {
			k++
			continue
		}
		r := 0
		for k+r < len(b) && b[k+r] == '\\' {
			r++
		}
		if k+r >= len(b) {
			return k
		}
		if b[k+r] != q {
			k += r + 1 // an escape inside the value
			continue
		}
		// r backslashes then the quote: at this depth that is r0 escaped
		// backslashes then either an escaped quote (r0 odd, part of the
		// value) or the closing quote (r0 even). Anything else ends the
		// string at a shallower depth. The value stops before the run.
		if (r+1)%unit != 0 || ((r+1)/unit-1)%2 == 0 {
			return k
		}
		k += r + 1
	}
	return k
}

func valueEnd(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n', '"', '\'', '`', '\\', ',', ';', '(', ')', '{', '}', '[', ']', '<', '>', '&', '|':
		return true
	}
	return c < 0x20
}

func assignValueOK(v []byte) bool {
	if len(v) < 8 {
		return false
	}
	switch v[0] {
	case '$', '%', '/', '~', '.', '#', '@', '*', '-', '=', ':', '!', '?':
		return false
	}
	if isMarker(v) || isPlaceholder(v) || allowlisted(v) || bytes.Contains(v, []byte("://")) {
		return false
	}
	if codeExpr(v) {
		return false // self.token, req.body?.token, os.environ
	}
	letters, digits, other := 0, 0, 0
	for _, c := range v {
		if c >= 0x80 {
			return false // real credentials are ASCII; "sk-ant-…" is a display
		}
		switch {
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			letters++
		case c >= '0' && c <= '9':
			digits++
		case c == '_' || c == '.' || c == '-':
		default:
			other++
		}
	}
	if digits == len(v) {
		return false // a number
	}
	h := entropy(v)
	if digits == 0 && other == 0 {
		// Letters and _.- only: an identifier unless long and random.
		return len(v) >= 32 && h >= 4.0
	}
	return h >= 3.0
}

// codeExpr: a code expression rather than a literal: identifiers joined
// by '.' (with TypeScript's ?. and !.), or an arrow or call.
func codeExpr(v []byte) bool {
	if bytes.Contains(v, []byte("=>")) || bytes.Contains(v, []byte("()")) {
		return true
	}
	if bytes.IndexByte(v, '?') >= 0 || bytes.IndexByte(v, '!') >= 0 {
		v = bytes.Map(func(r rune) rune {
			if r == '?' || r == '!' {
				return -1
			}
			return r
		}, v)
	}
	return dottedIdent(v)
}

// dottedIdent: identifier segments joined by '.' ("a.b", "x.y_z").
func dottedIdent(v []byte) bool {
	if bytes.IndexByte(v, '.') < 0 {
		return false
	}
	for _, seg := range bytes.Split(v, []byte(".")) {
		if len(seg) == 0 || seg[0] >= '0' && seg[0] <= '9' {
			return false
		}
		for _, c := range seg {
			if !isAlnum(c) && c != '_' {
				return false
			}
		}
	}
	return true
}
