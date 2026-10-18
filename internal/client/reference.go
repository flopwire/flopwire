package client

import (
	"context"
	"strings"

	"github.com/flopwire/flopwire/internal/busrender"
	"github.com/flopwire/flopwire/internal/retrieval/format"
)

// ReadReference uses the receiving device's normal, authorized read route.
// It never resolves a reference from the sender's payload or local files.
func (b Bus) ReadReference(ctx context.Context, ref string) (string, error) {
	addr, err := format.ParseAddress(ref)
	if err != nil || addr.Kind != format.AddrMessage {
		return "", err
	}
	c := HTTP{Server: b.Server, Token: b.Token, Client: b.HTTP}
	out, err := c.Read(ctx, format.ReadQuery{Address: ref, LineOffset: addr.Line, MaxChars: 4 * busrender.MaxExcerptBytes}, format.Filters{})
	if err != nil {
		return "", err
	}
	for _, m := range out.Messages {
		if m.ID != out.Focus || m.Text == "" {
			continue
		}
		text := m.Text
		if addr.Line > 0 {
			// Read already sliced at LineOffset. Taking the first line
			// preserves the local excerpt's addressed-line semantics.
			if m.LineFrom != addr.Line {
				return "", nil
			}
			text, _, _ = strings.Cut(text, "\n")
		}
		return busrender.ReferenceText(text, m.Role), nil
	}
	return "", nil
}
