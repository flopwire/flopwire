package localindex

import (
	"bytes"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

func TestCompanionBeforeOwnerSurvivesRestartAndPurges(t *testing.T) {
	s := openEvidenceTest(t)
	c := Companion{Agent: transcript.AgentClaude, SessionID: "owner", Path: "/synthetic/result.txt", Size: 12}
	if err := s.UpsertCompanion(ctx, c); err != nil {
		t.Fatal(err)
	}
	requireSessionEvidence(t, s, transcript.AgentClaude, "owner", true)
	src := source(t, s, transcript.AgentClaude, "/synthetic/owner.jsonl")
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "owner", Cwd: "/work", Title: "owner title"}}})
	// A later companion must not replace the transcript's source or metadata.
	if err := s.UpsertCompanion(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	var id int64
	var cwd, title string
	if err := s.DB().QueryRow(`SELECT source_id,cwd,title FROM conversations WHERE session_id='owner'`).Scan(&id, &cwd, &title); err != nil {
		t.Fatal(err)
	}
	if id != src.ID || cwd != "/work" || title != "owner title" {
		t.Fatalf("owner overwritten: %d %q %q", id, cwd, title)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(s.Path(), Options{DeviceID: "local-device", DeferCommit: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for i := 0; i < 2; i++ {
		if err := reopened.PurgeConversation(ctx, transcript.AgentClaude, "owner"); err != nil {
			t.Fatal(err)
		}
	}
	if err := reopened.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := reopened.DB().QueryRow(`SELECT count(*) FROM companions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("companions after purge=%d", n)
	}
}

func TestPurgeUnlinkedCompanionPreservesLinkedAndUnrelatedEvidence(t *testing.T) {
	s := openEvidenceTest(t)
	for _, c := range []Companion{
		{Path: "/synthetic/orphan.txt", Size: 12},
		{Path: "/synthetic/other-orphan.txt", Size: 9},
		{Agent: transcript.AgentClaude, SessionID: "owner", Path: "/synthetic/linked.txt", Size: 7, ContentSHA: bytes.Repeat([]byte{7}, 32)},
	} {
		if err := s.UpsertCompanion(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	sizes, err := s.CompanionSizes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sizes) != 1 || sizes["/synthetic/linked.txt"] != 7 {
		t.Fatalf("startup sizes=%v", sizes)
	}
	for _, path := range []string{"/synthetic/orphan.txt", "/synthetic/orphan.txt", "/synthetic/linked.txt", "/synthetic/missing.txt"} {
		if err := s.PurgeUnlinkedCompanion(ctx, path); err != nil {
			t.Fatal(err)
		}
	}
	digest, err := s.CompanionDigest(ctx, "/synthetic/linked.txt")
	if err != nil || !bytes.Equal(digest, bytes.Repeat([]byte{7}, 32)) {
		t.Fatalf("linked evidence changed: %x %v", digest, err)
	}
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM companions WHERE path='/synthetic/other-orphan.txt'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("unrelated orphan was deleted")
	}
	if err := s.DB().QueryRow(`SELECT count(*) FROM companions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("remaining companions=%d", n)
	}
}
