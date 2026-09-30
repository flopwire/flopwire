package localindex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

func sinkMsgs(t *testing.T, s *Store, src int64, gen int64, conv *transcript.Conversation, msgs ...*transcript.Message) {
	t.Helper()
	k := s.NewSink(ctx, src, gen)
	if conv != nil {
		k.Conversation(conv)
	}
	for _, m := range msgs {
		if err := k.Message(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := k.Flush(nil, nil); err != nil {
		t.Fatal(err)
	}
}

// The owner's redaction masks the row and its identical copies in the
// local index (FTS too), and stays applied when the transcript is indexed
// again, even after the index is rebuilt from scratch.
func TestLocalRedaction(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.db")
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	text := "first\ncodename BLUEFALCON-7731\nlast"
	index := func(s *Store) {
		src := source(t, s, transcript.AgentClaude, "/h/s1.jsonl")
		sinkMsgs(t, s, src.ID, 1, &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "sess-1"},
			msg("sess-1", "u1", 0, transcript.KindUser, text), msg("sess-1", "a1", 1, transcript.KindAssistant, "unrelated reply"))
		src2 := source(t, s, transcript.AgentClaude, "/h/s2.jsonl")
		sinkMsgs(t, s, src2.ID, 1, &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "sess-2"},
			msg("sess-2", "u9", 0, transcript.KindUser, text))
	}
	index(s)
	n, err := s.RedactMessage(ctx, LocalRedaction{Session: "sess-1", Ordinal: transcript.OrdinalAt(0, 0), From: 2, To: 2, AllCopies: true})
	if err != nil || n != 2 {
		t.Fatalf("redacted %d: %v", n, err)
	}
	check := func(s *Store) {
		t.Helper()
		for _, id := range []string{"u1", "u9"} {
			r := rowsOf(t, s, id)
			if len(r) != 1 || strings.Contains(r[0].text, "BLUEFALCON") || !strings.HasPrefix(r[0].text, "first\n[REDACTED:message]") {
				t.Fatalf("%s: %+v", id, r)
			}
		}
		eq(t, "find", findIDs(t, s, "BLUEFALCON", FindOptions{}), nil)
		eq(t, "search", searchIDs(t, s, "BLUEFALCON", SearchOptions{}), nil)
		eq(t, "neighbour", findIDs(t, s, "unrelated", FindOptions{}), []string{"a1"})
		var leaks int
		if err := s.DB().QueryRow(`SELECT count(*) FROM conversations WHERE instr(ifnull(digest, ''), 'BLUEFALCON') > 0`).Scan(&leaks); err != nil || leaks != 0 {
			t.Fatalf("%d digests hold the redacted line (%v)", leaks, err)
		}
	}
	check(s)
	// Indexing the transcript again (same text) keeps it masked.
	index(s)
	check(s)
	s.Close()

	// A rebuilt index (a new database beside the same sidecar) re-applies
	// the tombstones.
	side, err := os.ReadFile(path + ".redactions.jsonl")
	if err != nil || strings.Contains(string(side), "BLUEFALCON") {
		t.Fatalf("sidecar: %v", err)
	}
	path2 := filepath.Join(t.TempDir(), "index.db")
	os.WriteFile(path2+".redactions.jsonl", side, 0o600)
	s2, err := Open(path2, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	index(s2)
	check(s2)
}

// A redaction of the first prompt's first line masks the conversation
// title (the parsers' title is that line), and the title stays masked when
// the transcript is indexed again, so the digest's intent, which falls back
// to the title once the masked prompt reads as weak, never brings it back.
func TestLocalRedactionMasksTitle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.db")
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	title := "codename BLUEFALCON-7731 please fix the build"
	index := func(s *Store) {
		src := source(t, s, transcript.AgentClaude, "/h/s1.jsonl")
		sinkMsgs(t, s, src.ID, 1, &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "sess-1", Title: title},
			msg("sess-1", "u1", 0, transcript.KindUser, title+"\nmore"), msg("sess-1", "a1", 1, transcript.KindAssistant, "reply"))
	}
	check := func(s *Store, when string) {
		t.Helper()
		var leaks int
		if err := s.DB().QueryRow(`SELECT count(*) FROM conversations
			WHERE instr(ifnull(title, ''), 'BLUEFALCON') > 0 OR instr(ifnull(digest, ''), 'BLUEFALCON') > 0`).Scan(&leaks); err != nil || leaks != 0 {
			t.Fatalf("%s: %d conversations hold the redacted line in title or digest (%v)", when, leaks, err)
		}
	}
	index(s)
	if _, err := s.RedactMessage(ctx, LocalRedaction{Session: "sess-1", Ordinal: transcript.OrdinalAt(0, 0), From: 1, To: 1}); err != nil {
		t.Fatal(err)
	}
	check(s, "redacted")
	index(s)
	check(s, "indexed again")
	s.Close()

	side, err := os.ReadFile(path + ".redactions.jsonl")
	if err != nil || strings.Contains(string(side), "BLUEFALCON") {
		t.Fatalf("sidecar: %v", err)
	}
	path2 := filepath.Join(t.TempDir(), "index.db")
	os.WriteFile(path2+".redactions.jsonl", side, 0o600)
	s2, err := Open(path2, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	index(s2)
	check(s2, "rebuilt")
}
