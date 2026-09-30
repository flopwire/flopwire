package format

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Addresses. Every hit, message and session prints one, and read accepts
// all of them:
//
//	SESSION/ORDINAL[:LINE]  a message, and a line of its text (1-based)
//	SESSION                 a session, read from its start
//	MESSAGE_ID              a message by its store id
//	/path/to/file.jsonl:N   the message(s) recorded at a transcript line
//
// SESSION is any prefix of the harness session id that names one session
// (as with git's short hashes); printed addresses use the shortest unique
// prefix of at least MinPrefix characters.

// MinPrefix is the shortest session prefix an address prints.
const MinPrefix = 8

// Address kinds.
const (
	AddrMessage = iota + 1 // SESSION/ORDINAL[:LINE]
	AddrBare               // a message id or a session prefix
	AddrPath               // path:line
)

// Addr is a parsed address.
type Addr struct {
	Kind    int
	Session string // AddrMessage: the session prefix
	Ordinal int64  // AddrMessage
	Line    int    // AddrMessage: the addressed line, 0 when none
	Token   string // AddrBare
	Path    string // AddrPath
	PathNo  int64  // AddrPath: the transcript line
}

// MessageAddress renders SESSION/ORDINAL.
func MessageAddress(session string, ordinal int64) string {
	return session + "/" + strconv.FormatInt(ordinal, 10)
}

// ParseAddress parses an address; see the package comment on addresses.
func ParseAddress(s string) (Addr, error) {
	s = strings.TrimSpace(s)
	bad := func(why string) (Addr, error) {
		return Addr{}, fmt.Errorf("%w: address %q %s; want SESSION/ORDINAL[:LINE], SESSION, a message id, or /path/file.jsonl:LINE", ErrBadRequest, s, why)
	}
	switch {
	case s == "":
		return bad("is empty")
	case strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~"):
		i := strings.LastIndexByte(s, ':')
		if i < 0 {
			return bad("has no :LINE")
		}
		n, err := strconv.ParseInt(s[i+1:], 10, 64)
		if err != nil || n <= 0 {
			return bad("has a bad line number")
		}
		return Addr{Kind: AddrPath, Path: s[:i], PathNo: n}, nil
	case strings.Contains(s, "/"):
		i := strings.IndexByte(s, '/')
		sess, rest := s[:i], s[i+1:]
		a := Addr{Kind: AddrMessage, Session: sess}
		if j := strings.IndexByte(rest, ':'); j >= 0 {
			n, err := strconv.Atoi(rest[j+1:])
			if err != nil || n <= 0 {
				return bad("has a bad line number")
			}
			a.Line, rest = n, rest[:j]
		}
		n, err := strconv.ParseInt(rest, 10, 64)
		if err != nil || n < 0 || sess == "" {
			return bad("has a bad ordinal")
		}
		a.Ordinal = n
		return a, nil
	}
	return Addr{Kind: AddrBare, Token: s}, nil
}

// ShortPrefix is the shortest prefix of id, at least MinPrefix long, that
// no id in others starts with. others may include id itself.
func ShortPrefix(id string, others []string) string {
	n := min(MinPrefix, len(id))
	for _, o := range others {
		if o == id {
			continue
		}
		c := 0
		for c < len(id) && c < len(o) && id[c] == o[c] {
			c++
		}
		n = max(n, c+1)
	}
	return id[:min(n, len(id))]
}

// AmbiguousError lists the sessions an address prefix could name.
func AmbiguousError(prefix string, ids []string) error {
	if len(ids) > 5 {
		ids = append(ids[:5:5], "…")
	}
	return fmt.Errorf("%w: session prefix %q is ambiguous: %s; use more characters", ErrBadRequest, prefix, strings.Join(ids, ", "))
}

var lineRange = regexp.MustCompile(`^(.*):(\d+)-(\d+)$`)

// SplitLineRange splits "ADDRESS:L1-L2" (flopwire redact) into the address
// and the line range; an address without a range means the whole message
// (0, 0).
func SplitLineRange(s string) (string, int, int, error) {
	m := lineRange.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return strings.TrimSpace(s), 0, 0, nil
	}
	from, _ := strconv.Atoi(m[2])
	to, _ := strconv.Atoi(m[3])
	if from < 1 || to < from {
		return "", 0, 0, fmt.Errorf("%w: bad line range %s-%s", ErrBadRequest, m[2], m[3])
	}
	return m[1], from, to, nil
}
