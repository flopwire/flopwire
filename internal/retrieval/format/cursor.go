package format

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Sessions and read --outline page by keyset cursor, not offset: the next
// page starts after the sort key of the last entry shown, so a page costs
// the same however deep in the list it is, and nothing counts the whole
// list. A cursor is opaque to callers; both backends read the same form.
//
//   - sessions: the last session's last activity (Unix microseconds, or
//     "-" when it has none, which sorts after every dated session) and its
//     conversation id, "1788220800000000.ID".
//   - outline: the last entry's ordinal and message id, "42.ID".

// SessionCursor is the cursor of the sessions page after c.
func SessionCursor(c ConversationInfo) string {
	at := "-"
	if c.LastActivityAt != nil {
		at = strconv.FormatInt(c.LastActivityAt.UnixMicro(), 10)
	}
	return at + "." + c.ID
}

// SessionKey is a parsed sessions cursor.
type SessionKey struct {
	// At is the last activity in Unix microseconds; Undated marks a
	// session with none.
	At      int64
	Undated bool
	ID      string
}

// Time is At as a time.
func (k SessionKey) Time() time.Time { return time.UnixMicro(k.At).UTC() }

// ParseSessionCursor reads a SessionCursor.
func ParseSessionCursor(s string) (SessionKey, error) {
	at, id, ok := strings.Cut(s, ".")
	if !ok || id == "" {
		return SessionKey{}, fmt.Errorf("%w: cursor %q: not a sessions cursor", ErrBadRequest, s)
	}
	if at == "-" {
		return SessionKey{Undated: true, ID: id}, nil
	}
	n, err := strconv.ParseInt(at, 10, 64)
	if err != nil {
		return SessionKey{}, fmt.Errorf("%w: cursor %q: not a sessions cursor", ErrBadRequest, s)
	}
	return SessionKey{At: n, ID: id}, nil
}

// OutlineCursor is the cursor of the outline page after e.
func OutlineCursor(e OutlineEntry) string {
	return strconv.FormatInt(e.Ordinal, 10) + "." + e.ID
}

// ParseOutlineCursor reads an OutlineCursor: the ordinal and message id.
func ParseOutlineCursor(s string) (int64, string, error) {
	ord, id, ok := strings.Cut(s, ".")
	n, err := strconv.ParseInt(ord, 10, 64)
	if !ok || id == "" || err != nil {
		return 0, "", fmt.Errorf("%w: cursor %q: not an outline cursor", ErrBadRequest, s)
	}
	return n, id, nil
}
