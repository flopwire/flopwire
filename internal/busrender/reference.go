package busrender

import (
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"strings"
)

// ReferenceText applies the same bounded source window to local and shared
// excerpts. Blank windows produce no excerpt, including no role-only label.
func ReferenceText(text, role string) string {
	text = format.Cut(text, 1, 4*MaxExcerptBytes).Text
	text = strings.Join(strings.Fields(format.Clean(text)), " ")
	if text == "" {
		return ""
	}
	if role != "" {
		text = role + ": " + text
	}
	return format.Cut(text, 1, 4*MaxExcerptBytes).Text
}
