package local

import (
	"context"
	"errors"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"testing"
)

func TestLocalGrepRejectsContinuation(t *testing.T) {
	b := &Backend{}
	for _, q := range []format.GrepQuery{{Continue: true}, {Cursor: "opaque"}} {
		if _, err := b.Grep(context.Background(), q, format.Filters{}); !errors.Is(err, format.ErrBadRequest) {
			t.Fatalf("got %v", err)
		}
	}
}
