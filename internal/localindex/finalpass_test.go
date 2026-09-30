package localindex

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

// Local redaction x title x digest x rebuild: a Claude session's title is
// its first prompt, so redacting that prompt's first line must mask the
// title and the digest, now and after the index is rebuilt from the
// transcript beside the same sidecar.
func TestLocalRedactionMasksTitleAcrossRebuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	prompt := "deploy with key HUNTER2-SECRET-99\nthen run tests"
	index := func(s *Store) {
		src := source(t, s, transcript.AgentClaude, "/h/t1.jsonl")
		sinkMsgs(t, s, src.ID, 1, &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "sess-t", Title: prompt},
			msg("sess-t", "u1", 0, transcript.KindUser, prompt), msg("sess-t", "a1", 1, transcript.KindAssistant, "ok"))
	}
	index(s)
	if _, err := s.RedactMessage(ctx, LocalRedaction{Session: "sess-t", Ordinal: transcript.OrdinalAt(0, 0), From: 1, To: 1}); err != nil {
		t.Fatal(err)
	}
	check := func(s *Store, when string) {
		t.Helper()
		var title, dig int
		s.DB().QueryRow(`SELECT count(*) FROM conversations WHERE instr(ifnull(title,''), 'HUNTER2') > 0`).Scan(&title)
		s.DB().QueryRow(`SELECT count(*) FROM conversations WHERE instr(ifnull(digest,''), 'HUNTER2') > 0`).Scan(&dig)
		if title != 0 || dig != 0 {
			t.Fatalf("%s: %d titles and %d digests hold the redacted line", when, title, dig)
		}
	}
	check(s, "after redaction")
	index(s)
	check(s, "after re-index")
	s.Close()
	side, err := os.ReadFile(path + ".redactions.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	path2 := filepath.Join(t.TempDir(), "index.db")
	os.WriteFile(path2+".redactions.jsonl", side, 0o600)
	s2, err := Open(path2, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	index(s2)
	check(s2, "after rebuild")
}
