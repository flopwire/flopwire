package localindex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
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

func TestSessionEvidenceNativeCaptureWithoutSessionKey(t *testing.T) {
	const nativeID = "12345678-abcd-4321-9876-123456789abc"
	const otherID = "12345678-abcd-4321-9876-123456789abd"
	transcriptPath := "/synthetic/project/" + nativeID + ".jsonl"
	type captureCase struct {
		name             string
		mutate           func(map[string]any)
		rowPath, session string
		older            bool
		want             bool
	}
	cases := []captureCase{
		{name: "current transcript", want: true},
		{name: "acked earlier transcript", older: true, want: true},
		{name: "basename at root", rowPath: nativeID + ".jsonl", want: true},
		{name: "normalized canonical case", rowPath: strings.ToUpper(transcriptPath), session: strings.ToUpper(nativeID), want: true},
		{name: "companion parent", mutate: func(m map[string]any) { m["StorageKind"] = transcript.StorageCompanion; m["Parent"] = transcriptPath }, rowPath: "/synthetic/result.txt", want: true},
		{name: "acked companion parent", mutate: func(m map[string]any) { m["StorageKind"] = transcript.StorageCompanion; m["Parent"] = transcriptPath }, rowPath: "/synthetic/result.txt", older: true, want: true},
		{name: "missing key field", mutate: func(m map[string]any) { delete(m, "SessionKey") }, want: true},
		{name: "conflicting explicit key", mutate: func(m map[string]any) { m["SessionKey"] = otherID }},
		{name: "non UUID request", session: "arbitrary"},
		{name: "noncanonical UUID request", session: "urn:uuid:" + nativeID},
		{name: "other agent", mutate: func(m map[string]any) { m["Agent"] = transcript.AgentCodex }},
		{name: "CASS parser", mutate: func(m map[string]any) { m["Parser"] = "cass@1" }},
		{name: "missing native parser", mutate: func(m map[string]any) { delete(m, "Parser") }},
		{name: "unversioned native parser", mutate: func(m map[string]any) { m["Parser"] = "claude@" }},
		{name: "export", mutate: func(m map[string]any) { m["Export"] = true }},
		{name: "wrong storage", mutate: func(m map[string]any) { m["StorageKind"] = transcript.StorageJSONDoc }},
		{name: "other transcript basename", rowPath: "/synthetic/project/" + otherID + ".jsonl"},
		{name: "UUID suffix", rowPath: "/synthetic/prefix-" + nativeID + ".jsonl"},
		{name: "filename suffix", rowPath: transcriptPath + "-extra"},
		{name: "spec path cannot override row", rowPath: "/synthetic/unrelated.jsonl"},
		{name: "mismatched companion parent", mutate: func(m map[string]any) {
			m["StorageKind"] = transcript.StorageCompanion
			m["Parent"] = "/synthetic/" + otherID + ".jsonl"
		}, rowPath: "/synthetic/result.txt"},
		{name: "arbitrary companion parent", mutate: func(m map[string]any) {
			m["StorageKind"] = transcript.StorageCompanion
			m["Parent"] = "/synthetic/arbitrary.jsonl"
		}, rowPath: "/synthetic/result.txt"},
		{name: "missing companion parent", mutate: func(m map[string]any) { m["StorageKind"] = transcript.StorageCompanion }, rowPath: "/synthetic/result.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openEvidenceTest(t)
			spec := map[string]any{"Agent": transcript.AgentClaude, "SessionKey": "", "Parser": "claude@1", "StorageKind": transcript.StorageJSONLAppend, "Path": transcriptPath}
			if tc.mutate != nil {
				tc.mutate(spec)
			}
			encoded, err := json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			rowPath := tc.rowPath
			if rowPath == "" {
				rowPath = transcriptPath
			}
			session := tc.session
			if session == "" {
				session = nativeID
			}
			gen, closed, acked := 3, 0, 0
			if tc.older {
				gen, closed, acked = 1, 1, 1
			}
			if err = s.write(ctx, func(w *writeTx) error {
				for _, stmt := range []string{`CREATE TABLE devsync_sources(id INTEGER PRIMARY KEY,path TEXT NOT NULL,spec TEXT NOT NULL,generation INTEGER NOT NULL)`, `CREATE TABLE devsync_gens(source_id INTEGER,generation INTEGER,size INTEGER,closed INTEGER,acked INTEGER)`} {
					if _, err := w.exec(stmt); err != nil {
						return err
					}
				}
				if _, err := w.exec(`INSERT INTO devsync_sources VALUES(1,?,?,3)`, rowPath, string(encoded)); err != nil {
					return err
				}
				_, err := w.exec(`INSERT INTO devsync_gens VALUES(1,?,7,?,?)`, gen, closed, acked)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			requireSessionEvidence(t, s, transcript.AgentClaude, session, tc.want)
			requireSessionEvidence(t, s, transcript.AgentClaude, "12345678-abcd-4321-9876-123456789abe", false)
			requireSessionEvidence(t, s, transcript.AgentCodex, session, false)
			if tc.want {
				if err = s.Sync(ctx); err != nil {
					t.Fatal(err)
				}
				r, err := Open(s.Path(), Options{DeviceID: "local-device", ReadOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
				requireSessionEvidence(t, r, transcript.AgentClaude, session, true)
			}
		})
	}
}

func TestCapturedClaudeChildrenVerifiedStoredAncestry(t *testing.T) {
	const parent = "12345678-abcd-4321-9876-123456789abc"
	root := "/synthetic/projects"
	path := root + "/project/" + parent + "/subagents/workflows/run/agent-cafe.jsonl"
	for _, tc := range []struct {
		name, path, key, parser string
		foreign, want           bool
	}{
		{"native", path, "agent-cafe", "claude@1", false, true},
		{"empty key", path, "", "claude@1", false, true},
		{"other root", "/other/projects/project/" + parent + "/subagents/agent-cafe.jsonl", "agent-cafe", "claude@1", false, false},
		{"other parent", strings.Replace(path, parent, "12345678-abcd-4321-9876-123456789abd", 1), "agent-cafe", "claude@1", false, false},
		{"conflicting key", path, "agent-other", "claude@1", false, false},
		{"non-native parser", path, "agent-cafe", "cass@1", false, false},
		{"foreign device", path, "agent-cafe", "claude@1", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openEvidenceTest(t)
			src, err := s.EnsureSource(ctx, transcript.Source{Agent: transcript.AgentClaude, Path: tc.path, SessionKey: tc.key, StorageKind: transcript.StorageJSONLAppend, Parser: tc.parser})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.SaveWatermark(ctx, src.ID, transcript.Watermark{Offset: 9}, nil); err != nil {
				t.Fatal(err)
			}
			if tc.foreign {
				if err = s.write(ctx, func(w *writeTx) error {
					_, err := w.exec(`UPDATE sources SET device_id='foreign' WHERE id=?`, src.ID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			children, err := s.CapturedClaudeChildren(ctx, []string{root}, parent)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want {
				if !reflect.DeepEqual(children, []string{"agent-cafe"}) {
					t.Fatal(children)
				}
			} else if len(children) != 0 {
				t.Fatal("unverified relation", children)
			}
		})
	}
}

func TestCapturedClaudeChildrenSchedulerHistory(t *testing.T) {
	const parent = "12345678-abcd-4321-9876-123456789abc"
	for _, older := range []bool{false, true} {
		t.Run(fmt.Sprint(older), func(t *testing.T) {
			s := openEvidenceTest(t)
			root := "/synthetic/projects"
			path := root + "/project/" + parent + "/subagents/agent-cafe.jsonl"
			spec := `{"Agent":"claude","StorageKind":"jsonl_append","Parser":"claude@1","SessionKey":""}`
			gen, closed, acked := 3, 0, 0
			if older {
				gen, closed, acked = 1, 1, 1
			}
			if err := s.write(ctx, func(w *writeTx) error {
				for _, stmt := range []string{`CREATE TABLE devsync_sources(id INTEGER PRIMARY KEY,path TEXT,spec TEXT,generation INTEGER)`, `CREATE TABLE devsync_gens(source_id INTEGER,generation INTEGER,size INTEGER,closed INTEGER,acked INTEGER)`} {
					if _, err := w.exec(stmt); err != nil {
						return err
					}
				}
				if _, err := w.exec(`INSERT INTO devsync_sources VALUES(1,?,?,3)`, path, spec); err != nil {
					return err
				}
				_, err := w.exec(`INSERT INTO devsync_gens VALUES(1,?,9,?,?)`, gen, closed, acked)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			children, err := s.CapturedClaudeChildren(ctx, []string{root}, parent)
			if err != nil || !reflect.DeepEqual(children, []string{"agent-cafe"}) {
				t.Fatalf("scheduler relation %v %v", children, err)
			}
			if err = s.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			r, err := Open(s.Path(), Options{DeviceID: "local-device", ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			children, err = r.CapturedClaudeChildren(ctx, []string{root}, parent)
			if err != nil || !reflect.DeepEqual(children, []string{"agent-cafe"}) {
				t.Fatalf("reopened relation %v %v", children, err)
			}
		})
	}
}

func TestCapturedClaudeChildrenCompanionOnly(t *testing.T) {
	const parent = "12345678-abcd-4321-9876-123456789abc"
	root := "/synthetic/projects"
	childPath := root + "/project/" + parent + "/subagents/agent-cafe.jsonl"
	for _, tc := range []struct {
		name, key, parent, parser string
		want                      bool
	}{
		{"empty key", "", childPath, "claude@1", true},
		{"matching key", "agent-cafe", childPath, "claude@1", true},
		{"conflicting key", "agent-other", childPath, "claude@1", false},
		{"wrong ancestry", "", root + "/project/12345678-abcd-4321-9876-123456789abd/subagents/agent-cafe.jsonl", "claude@1", false},
		{"non-native", "", childPath, "cass@1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openEvidenceTest(t)
			spec, err := json.Marshal(map[string]any{"Agent": "claude", "StorageKind": "companion", "Parser": tc.parser, "Parent": tc.parent, "SessionKey": tc.key})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.write(ctx, func(w *writeTx) error {
				for _, stmt := range []string{`CREATE TABLE devsync_sources(id INTEGER PRIMARY KEY,path TEXT,spec TEXT,generation INTEGER)`, `CREATE TABLE devsync_gens(source_id INTEGER,generation INTEGER,size INTEGER,closed INTEGER,acked INTEGER)`} {
					if _, err := w.exec(stmt); err != nil {
						return err
					}
				}
				if _, err := w.exec(`INSERT INTO devsync_sources VALUES(1,'/synthetic/result.txt',?,3)`, string(spec)); err != nil {
					return err
				}
				_, err := w.exec(`INSERT INTO devsync_gens VALUES(1,1,9,1,1)`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			children, err := s.CapturedClaudeChildren(ctx, []string{root}, parent)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want {
				if !reflect.DeepEqual(children, []string{"agent-cafe"}) {
					t.Fatal(children)
				}
			} else if len(children) != 0 {
				t.Fatal(children)
			}
		})
	}
	// Local companion bytes can be the only evidence for an empty transcript.
	for _, conversationOnly := range []bool{false, true} {
		t.Run(fmt.Sprint("local conversation-only=", conversationOnly), func(t *testing.T) {
			s := openEvidenceTest(t)
			src, err := s.EnsureSource(ctx, transcript.Source{Agent: transcript.AgentClaude, Path: childPath, SessionKey: "agent-cafe", StorageKind: transcript.StorageJSONLAppend, Parser: "claude@1"})
			if err != nil {
				t.Fatal(err)
			}
			companion := Companion{Agent: transcript.AgentClaude, SessionID: "agent-cafe", SourceID: src.ID, Path: "/synthetic/result.txt", Size: 9}
			if conversationOnly {
				apply(t, s, Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "agent-cafe"}}})
				companion.SourceID = 0
			}
			if err = s.UpsertCompanion(ctx, companion); err != nil {
				t.Fatal(err)
			}
			children, err := s.CapturedClaudeChildren(ctx, []string{root}, parent)
			if err != nil || !reflect.DeepEqual(children, []string{"agent-cafe"}) {
				t.Fatalf("local companion relation %v %v", children, err)
			}
		})
	}
}

func TestSessionEvidenceCapturedGenerationSurvivesNoRowsOrWatermark(t *testing.T) {
	const parent = "12345678-abcd-4321-9876-123456789abc"
	for _, size := range []int64{0, 9} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			s := openEvidenceTest(t)
			root := "/synthetic/projects"
			path := root + "/project/" + parent + "/subagents/agent-cafe.jsonl"
			src, err := s.EnsureSource(ctx, transcript.Source{Agent: transcript.AgentClaude, Path: path, SessionKey: "agent-cafe", StorageKind: transcript.StorageJSONLAppend, Parser: "claude@1"})
			if err != nil {
				t.Fatal(err)
			}
			// Generation creation can precede parsing. Nonempty byte capture is
			// conservative historical proof even when extraction did not commit rows.
			if err = s.StartGeneration(ctx, src.ID, transcript.Generation{Generation: 1, Size: size, Complete: false}, "synthetic capture before parse"); err != nil {
				t.Fatal(err)
			}
			requireSessionEvidence(t, s, transcript.AgentClaude, "agent-cafe", size > 0)
			children, err := s.CapturedClaudeChildren(ctx, []string{root}, parent)
			if err != nil {
				t.Fatal(err)
			}
			if (len(children) > 0) != (size > 0) {
				t.Fatalf("generation children %v", children)
			}
			if err = s.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			r, err := Open(s.Path(), Options{DeviceID: "local-device", ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			requireSessionEvidence(t, r, transcript.AgentClaude, "agent-cafe", size > 0)
		})
	}
}
