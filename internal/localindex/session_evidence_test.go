package localindex

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

func openEvidenceTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "index.db"), Options{DeviceID: "local-device", DeferCommit: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func requireSessionEvidence(t *testing.T, s *Store, agent transcript.Agent, session string, want bool) {
	t.Helper()
	have, err := s.SessionHasEvidence(ctx, agent, session)
	if err != nil || have != want {
		t.Fatalf("SessionHasEvidence(%s,%s)=%v,%v; want %v", agent, session, have, err, want)
	}
}

func TestSessionEvidenceMessagesBeforeWatermark(t *testing.T) {
	s := openEvidenceTest(t)
	// Native identity comes from the conversation when source session_key is absent.
	src := source(t, s, transcript.AgentClaude, "/synthetic/session.jsonl")
	requireSessionEvidence(t, s, transcript.AgentClaude, "native", false)
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{msg("native", "message", 0, transcript.KindUser, "captured bytes")}})
	st, err := s.Source(ctx, src.ID)
	if err != nil || st.Watermark != nil {
		t.Fatalf("expected batch before watermark: %+v %v", st, err)
	}
	requireSessionEvidence(t, s, transcript.AgentClaude, "native", true)
	requireSessionEvidence(t, s, transcript.AgentClaude, "other", false)
	requireSessionEvidence(t, s, transcript.AgentCodex, "native", false)
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	r, err := Open(s.Path(), Options{DeviceID: "local-device", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	requireSessionEvidence(t, r, transcript.AgentClaude, "native", true)
}

func TestSessionEvidenceOrphanAndUnlinkedCompanions(t *testing.T) {
	for _, linked := range []bool{false, true} {
		t.Run(map[bool]string{false: "source identity", true: "conversation identity"}[linked], func(t *testing.T) {
			s := openEvidenceTest(t)
			src, err := s.EnsureSource(ctx, transcript.Source{Agent: transcript.AgentClaude, Path: "/synthetic/main.jsonl", SessionKey: "native", StorageKind: transcript.StorageJSONLAppend, Parser: "claude@1"})
			if err != nil {
				t.Fatal(err)
			}
			if linked {
				apply(t, s, Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "native"}}})
			}
			c := Companion{Agent: transcript.AgentClaude, SessionID: "native", Path: "/synthetic/result.txt", Kind: "tool_result", Size: 0}
			if !linked {
				c.SourceID = src.ID
			}
			if err := s.UpsertCompanion(ctx, c); err != nil {
				t.Fatal(err)
			}
			requireSessionEvidence(t, s, transcript.AgentClaude, "native", false)
			c.Size = 5
			if err := s.UpsertCompanion(ctx, c); err != nil {
				t.Fatal(err)
			}
			requireSessionEvidence(t, s, transcript.AgentClaude, "native", true)
			requireSessionEvidence(t, s, transcript.AgentCodex, "native", false)
			requireSessionEvidence(t, s, transcript.AgentClaude, "other", false)
		})
	}
}

func TestSessionEvidenceWatermarkedSourceAndDeviceScope(t *testing.T) {
	s := openEvidenceTest(t)
	src, err := s.EnsureSource(ctx, transcript.Source{Agent: transcript.AgentClaude, Path: "/synthetic/main.jsonl", SessionKey: "native", StorageKind: transcript.StorageJSONLAppend, Parser: "claude@1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveWatermark(ctx, src.ID, transcript.Watermark{Offset: 9}, nil); err != nil {
		t.Fatal(err)
	}
	requireSessionEvidence(t, s, transcript.AgentClaude, "native", true)
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{msg("native", "message", 0, transcript.KindUser, "foreign captured bytes")}})
	if err := s.UpsertCompanion(ctx, Companion{Agent: transcript.AgentClaude, SessionID: "native", SourceID: src.ID, Path: "/synthetic/result.txt", Size: 5}); err != nil {
		t.Fatal(err)
	}
	if err := s.write(ctx, func(w *writeTx) error {
		if _, err := w.exec(`UPDATE sources SET device_id='foreign-device' WHERE id=?`, src.ID); err != nil {
			return err
		}
		_, err := w.exec(`UPDATE conversations SET device_id='foreign-device' WHERE session_id='native'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	requireSessionEvidence(t, s, transcript.AgentClaude, "native", false)
}

func TestCompanionDigestSeesPendingWritesAndUpdates(t *testing.T) {
	s := openEvidenceTest(t)
	path := "/synthetic/result.txt"
	if digest, err := s.CompanionDigest(ctx, path); err != nil || digest != nil {
		t.Fatalf("missing digest=%x,%v", digest, err)
	}
	c := Companion{Path: path, Kind: "tool_result", Size: 5, ContentSHA: bytes.Repeat([]byte{1}, 32)}
	if err := s.UpsertCompanion(ctx, c); err != nil {
		t.Fatal(err)
	}
	if digest, err := s.CompanionDigest(ctx, path); err != nil || !bytes.Equal(digest, c.ContentSHA) {
		t.Fatalf("pending digest=%x,%v", digest, err)
	}
	c.ContentSHA = bytes.Repeat([]byte{2}, 32)
	if err := s.UpsertCompanion(ctx, c); err != nil {
		t.Fatal(err)
	}
	if digest, err := s.CompanionDigest(ctx, path); err != nil || !bytes.Equal(digest, c.ContentSHA) {
		t.Fatalf("updated digest=%x,%v", digest, err)
	}
	c.ContentSHA = nil
	if err := s.UpsertCompanion(ctx, c); err != nil {
		t.Fatal(err)
	}
	if digest, err := s.CompanionDigest(ctx, path); err != nil || digest != nil {
		t.Fatalf("absent digest=%x,%v", digest, err)
	}
}

func TestSessionEvidenceOptionalSyncTables(t *testing.T) {
	for _, tables := range []int{0, 1, 2} {
		t.Run(string(rune('0'+tables)), func(t *testing.T) {
			s := openEvidenceTest(t)
			if err := s.write(ctx, func(w *writeTx) error {
				if tables > 0 {
					if _, err := w.exec(`CREATE TABLE devsync_sources(id INTEGER PRIMARY KEY,spec TEXT NOT NULL,generation INTEGER NOT NULL)`); err != nil {
						return err
					}
				}
				if tables > 1 {
					if _, err := w.exec(`CREATE TABLE devsync_gens(source_id INTEGER,generation INTEGER,size INTEGER,closed INTEGER,lost INTEGER,acked INTEGER)`); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if tables == 1 {
				if _, err := s.SessionHasEvidence(ctx, transcript.AgentClaude, "native"); err == nil {
					t.Fatal("partial device sync evidence schema ignored")
				}
			} else {
				requireSessionEvidence(t, s, transcript.AgentClaude, "native", false)
			}
		})
	}
}

func TestSessionEvidenceSyncCapturesBeforeExtraction(t *testing.T) {
	cases := []struct {
		name                string
		size                int64
		addGen              bool
		generation          int
		closed, lost, acked int
		want                bool
	}{
		{"metadata only", 0, false, 3, 0, 0, 0, false},
		{"empty capture", 0, true, 3, 0, 0, 0, false},
		{"current pending", 7, true, 3, 0, 0, 0, true},
		{"earlier closed", 7, true, 1, 1, 0, 0, true},
		{"earlier acknowledged", 7, true, 1, 1, 0, 1, true},
		{"earlier lost", 7, true, 1, 1, 1, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openEvidenceTest(t)
			// SourceSpec fields have Go's default uppercase JSON names. These are
			// scheduler-only rows: no local-index source, conversation or watermark.
			spec, err := json.Marshal(struct {
				Agent      transcript.Agent
				SessionKey string
			}{transcript.AgentClaude, "native"})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.write(ctx, func(w *writeTx) error {
				for _, stmt := range []string{`CREATE TABLE devsync_sources(id INTEGER PRIMARY KEY,spec TEXT NOT NULL,generation INTEGER NOT NULL)`, `CREATE TABLE devsync_gens(source_id INTEGER,generation INTEGER,size INTEGER,closed INTEGER,lost INTEGER,acked INTEGER)`} {
					if _, err := w.exec(stmt); err != nil {
						return err
					}
				}
				if _, err := w.exec(`INSERT INTO devsync_sources VALUES(1,?,3)`, string(spec)); err != nil {
					return err
				}
				if tc.addGen {
					_, err := w.exec(`INSERT INTO devsync_gens VALUES(1,?,?,?,?,?)`, tc.generation, tc.size, tc.closed, tc.lost, tc.acked)
					return err
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			requireSessionEvidence(t, s, transcript.AgentClaude, "native", tc.want)
			requireSessionEvidence(t, s, transcript.AgentCodex, "native", false)
			requireSessionEvidence(t, s, transcript.AgentClaude, "other", false)
			if err = s.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			r, err := Open(s.Path(), Options{DeviceID: "local-device", ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			requireSessionEvidence(t, r, transcript.AgentClaude, "native", tc.want)
		})
	}
}

func TestSessionEvidenceSyncMissingOrMalformedIdentity(t *testing.T) {
	for _, spec := range []string{`{}`, `{"Agent":"claude"}`, `{"SessionKey":"native"}`, `{"agent":"claude","sessionKey":"native"}`, `not-json`} {
		t.Run(spec, func(t *testing.T) {
			s := openEvidenceTest(t)
			if err := s.write(ctx, func(w *writeTx) error {
				for _, stmt := range []string{`CREATE TABLE devsync_sources(id INTEGER PRIMARY KEY,spec TEXT NOT NULL)`, `CREATE TABLE devsync_gens(source_id INTEGER,size INTEGER)`} {
					if _, err := w.exec(stmt); err != nil {
						return err
					}
				}
				if _, err := w.exec(`INSERT INTO devsync_sources VALUES(1,?)`, spec); err != nil {
					return err
				}
				_, err := w.exec(`INSERT INTO devsync_gens VALUES(1,7)`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if spec == "not-json" {
				if _, err := s.SessionHasEvidence(ctx, transcript.AgentClaude, "native"); err == nil {
					t.Fatal("corrupt captured source identity ignored")
				}
			} else {
				requireSessionEvidence(t, s, transcript.AgentClaude, "native", false)
			}
		})
	}
}

func TestSessionEvidenceSyncQueryFailurePropagates(t *testing.T) {
	s := openEvidenceTest(t)
	if err := s.write(ctx, func(w *writeTx) error {
		if _, err := w.exec(`CREATE TABLE devsync_sources(id INTEGER PRIMARY KEY,spec TEXT NOT NULL)`); err != nil {
			return err
		}
		// An incomplete/corrupt optional schema must hold collection through an
		// error, rather than report that an unknown evidence store is empty.
		_, err := w.exec(`CREATE TABLE devsync_gens(source_id INTEGER)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionHasEvidence(ctx, transcript.AgentClaude, "native"); err == nil {
		t.Fatal("sync evidence query failure ignored")
	}
}
