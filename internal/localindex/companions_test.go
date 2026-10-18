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

func TestLinkCompanionOwnerPreservesCapturedEvidence(t *testing.T) {
	s := openEvidenceTest(t)
	// Legacy row with no stored owner or source identity.
	c := Companion{Path: "/synthetic/result.txt", Size: 12, ContentSHA: bytes.Repeat([]byte{7}, 32)}
	if err := s.UpsertCompanion(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	sizes, err := s.CompanionSizes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sizes[c.Path]; ok {
		t.Fatal("unlinked row would skip discovery repair")
	}
	c.Agent, c.SessionID = transcript.AgentClaude, "owner"
	linked, err := s.LinkCompanionOwner(ctx, c)
	if err != nil || !linked {
		t.Fatalf("repair=%v %v", linked, err)
	}
	digest, err := s.CompanionDigest(ctx, c.Path)
	if err != nil || !bytes.Equal(digest, c.ContentSHA) {
		t.Fatalf("repair changed digest: %x %v", digest, err)
	}
	requireSessionEvidence(t, s, transcript.AgentClaude, "owner", true)
	// Existing ownership cannot be reassigned by a later caller.
	linked, err = s.LinkCompanionOwner(ctx, c)
	if err != nil || !linked {
		t.Fatalf("retry lost owner: %v %v", linked, err)
	}
	c.SessionID = "other"
	linked, err = s.LinkCompanionOwner(ctx, c)
	if err != nil || linked {
		t.Fatalf("reassigned linked row: %v %v", linked, err)
	}
	c.Path = "/synthetic/absent.txt"
	linked, err = s.LinkCompanionOwner(ctx, c)
	if err != nil || linked {
		t.Fatalf("created absent row: %v %v", linked, err)
	}
	requireSessionEvidence(t, s, transcript.AgentClaude, "other", false)
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	sizes, err = s.CompanionSizes(ctx)
	if err != nil || sizes["/synthetic/result.txt"] != 12 {
		t.Fatalf("linked sizes=%v %v", sizes, err)
	}
}
