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
	placeSidecar(t, path, path2, side)
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
	placeSidecar(t, path, path2, side)
	s2, err := Open(path2, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	index(s2)
	check(s2, "rebuilt")
}

// ftsHits counts the rows each FTS table returns for a phrase, read from
// the shards directly (find and search verify against stored text).
func ftsHits(t *testing.T, s *Store, phrase string) int {
	t.Helper()
	var n int
	for _, sh := range s.shards {
		var c int
		q := `SELECT count(*) FROM ` + sh.schema + `.` + sh.table + ` WHERE ` + sh.table + ` MATCH ?`
		if err := s.DB().QueryRow(q, `"`+phrase+`"`).Scan(&c); err != nil {
			t.Fatal(err)
		}
		n += c
	}
	return n
}

// A flush that computed its masks before a local redaction recorded its
// tombstone, and writes after the redaction committed, still stores the
// copy masked, in the row and in both FTS tables: the masks are applied on
// the writer, with the tombstones the writer has seen.
func TestLocalRedactionRacesFlush(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "defer"}[deferred], func(t *testing.T) {
			s, err := Open(filepath.Join(t.TempDir(), "index.db"), Options{DeferCommit: deferred})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			text := "first\ncodename BLUEFALCON-7731\nlast"
			src := source(t, s, transcript.AgentClaude, "/h/s1.jsonl")
			sinkMsgs(t, s, src.ID, 1, &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "sess-1"},
				msg("sess-1", "u1", 0, transcript.KindUser, text))

			// The copy's flush prepares its rows, then the redaction runs
			// to completion, then the flush writes.
			testHookSinkPrepared = func() {
				testHookSinkPrepared = nil
				if n, err := s.RedactMessage(ctx, LocalRedaction{Session: "sess-1", Ordinal: transcript.OrdinalAt(0, 0), From: 2, To: 2, AllCopies: true}); err != nil || n != 1 {
					t.Errorf("redacted %d: %v", n, err)
				}
			}
			defer func() { testHookSinkPrepared = nil }()
			src2 := source(t, s, transcript.AgentClaude, "/h/s2.jsonl")
			sinkMsgs(t, s, src2.ID, 1, &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "sess-2"},
				msg("sess-2", "u9", 0, transcript.KindUser, text))
			if err := s.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			if testHookSinkPrepared != nil {
				t.Fatal("hook did not run")
			}

			for _, id := range []string{"u1", "u9"} {
				r := rowsOf(t, s, id)
				if len(r) != 1 || strings.Contains(r[0].text, "BLUEFALCON") {
					t.Errorf("%s stored unmasked: %+v", id, r)
				}
			}
			if n := ftsHits(t, s, "BLUEFALCON"); n != 0 {
				t.Errorf("FTS still matches the redacted line in %d rows", n)
			}
			eq(t, "search", searchIDs(t, s, "BLUEFALCON", SearchOptions{}), nil)
		})
	}
}

// Rows written through ApplyBatch directly (not a Sink) are masked too.
func TestLocalRedactionAppliesToDirectBatch(t *testing.T) {
	s := openTest(t, DetailFull)
	text := "codename BLUEFALCON-7731 please fix the build"
	src := source(t, s, transcript.AgentClaude, "/h/s1.jsonl")
	sinkMsgs(t, s, src.ID, 1, &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "sess-1", Title: text},
		msg("sess-1", "u1", 0, transcript.KindUser, text))
	if _, err := s.RedactMessage(ctx, LocalRedaction{Session: "sess-1", Ordinal: transcript.OrdinalAt(0, 0), AllCopies: true}); err != nil {
		t.Fatal(err)
	}
	src2 := source(t, s, transcript.AgentClaude, "/h/s2.jsonl")
	apply(t, s, Batch{SourceID: src2.ID, Generation: 1,
		Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "sess-2", Title: text}},
		Messages:      []*transcript.Message{msg("sess-2", "u9", 0, transcript.KindUser, text)}})
	if r := rowsOf(t, s, "u9"); len(r) != 1 || strings.Contains(r[0].text, "BLUEFALCON") {
		t.Errorf("u9 stored unmasked: %+v", r)
	}
	var leaks int
	if err := s.DB().QueryRow(`SELECT count(*) FROM conversations WHERE instr(ifnull(title, ''), 'BLUEFALCON') > 0`).Scan(&leaks); err != nil || leaks != 0 {
		t.Errorf("%d titles hold the redacted text (%v)", leaks, err)
	}
	if n := ftsHits(t, s, "BLUEFALCON"); n != 0 {
		t.Errorf("FTS still matches the redacted text in %d rows", n)
	}
}
