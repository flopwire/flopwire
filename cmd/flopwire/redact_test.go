package main

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/localindex"
)

// Without a server or a running agent, `flopwire redact` masks the message
// in the local index itself.
func TestRedactCommandLocal(t *testing.T) {
	oracleIndex(t)
	db := os.Getenv("FLOPWIRE_INDEX")
	s, err := localindex.Open(db, localindex.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	hits, err := s.Find(t.Context(), "setTimeout", localindex.FindOptions{})
	s.Close()
	if err != nil || len(hits) == 0 {
		t.Fatalf("find: %v", err)
	}
	h := hits[0]
	addr := h.SessionID + "/" + itoa64(h.Ordinal)
	if err := redactCmd(t.Context(), []string{"--all-copies", addr}); err != nil {
		t.Fatal(err)
	}
	s, err = localindex.Open(db, localindex.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	after, err := s.Find(t.Context(), "setTimeout", localindex.FindOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range after {
		if a.SessionID == h.SessionID && a.Ordinal == h.Ordinal {
			t.Fatalf("message still found after redaction: %+v", a.Row.Kind)
		}
	}
	if _, err := os.Stat(db + ".redactions.jsonl"); err != nil {
		t.Fatalf("no tombstone sidecar: %v", err)
	}
	if err := redactCmd(t.Context(), []string{"--admin", addr}); err == nil || !strings.Contains(err.Error(), "needs a server") {
		t.Fatalf("--admin without a server: %v", err)
	}
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }
