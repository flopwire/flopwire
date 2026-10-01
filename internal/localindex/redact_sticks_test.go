package localindex

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

// noLeak fails t when any stored message text, title, digest or FTS entry
// still holds needle.
func noLeak(t *testing.T, s *Store, needle, when string) {
	t.Helper()
	rows, err := s.DB().Query(`SELECT native_id, text FROM messages`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var z []byte
		if err := rows.Scan(&id, &z); err != nil {
			t.Fatal(err)
		}
		if text, err := decompress(z); err != nil || strings.Contains(text, needle) {
			t.Errorf("%s: row %s stored unmasked: %q (%v)", when, id, text, err)
		}
	}
	var convs int
	if err := s.DB().QueryRow(`SELECT count(*) FROM conversations
		WHERE instr(ifnull(title, ''), ?) > 0 OR instr(ifnull(digest, ''), ?) > 0`, needle, needle).Scan(&convs); err != nil || convs != 0 {
		t.Errorf("%s: %d titles or digests hold %q (%v)", when, convs, needle, err)
	}
	if n := ftsHits(t, s, needle); n != 0 {
		t.Errorf("%s: FTS still matches %q in %d rows", when, needle, n)
	}
}

// A redaction whose tombstone reached the sidecar but whose row masks were
// lost (a crash, or a failed deferred commit) is applied when the index
// opens: the rows, the FTS entries and the title are masked even though
// nothing writes those rows again.
func TestLocalRedactionReconcilesSidecarOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	text := "codename BLUEFALCON-7731\nlast"
	src := source(t, s, transcript.AgentClaude, "/h/s1.jsonl")
	sinkMsgs(t, s, src.ID, 1, &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "sess-1", Title: "codename BLUEFALCON-7731"},
		msg("sess-1", "u1", 0, transcript.KindUser, text), msg("sess-1", "a1", 1, transcript.KindAssistant, "unrelated reply"))
	src2 := source(t, s, transcript.AgentClaude, "/h/s2.jsonl")
	sinkMsgs(t, s, src2.ID, 1, &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "sess-2"},
		msg("sess-2", "u9", 0, transcript.KindUser, text))
	s.Close()

	// What the redaction of line 1 with --all-copies records, written
	// without the row masks its lost transaction held.
	sum := sha256.Sum256([]byte(text))
	line := sha256.Sum256([]byte("codename BLUEFALCON-7731"))
	side := `{"sha":"` + hex.EncodeToString(sum[:]) + `","session":"sess-1","native":"u1","from":1,"to":1,"lines":["` + hex.EncodeToString(line[:]) + `"],"lens":[24]}` + "\n" +
		`{"sha":"` + hex.EncodeToString(sum[:]) + `","session":"sess-2","native":"u9","from":1,"to":1,"lines":["` + hex.EncodeToString(line[:]) + `"],"lens":[24]}` + "\n"
	if err := os.WriteFile(path+".redactions.jsonl", []byte(side), 0o600); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	noLeak(t, s2, "BLUEFALCON", "reopened")
	eq(t, "neighbour", findIDs(t, s2, "unrelated", FindOptions{}), []string{"a1"})
}

// A deferred commit that fails after the redaction wrote its tombstone:
// the redaction reports the failure (it waits for its own commit), and the
// writer re-applies the sidecar on its next transaction, so the rows end
// up masked without a restart.
func TestLocalRedactionFailedCommitReconciles(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "index.db"), Options{DeferCommit: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	text := "first\ncodename BLUEFALCON-7731\nlast"
	src := source(t, s, transcript.AgentClaude, "/h/s1.jsonl")
	sinkMsgs(t, s, src.ID, 1, &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "sess-1"},
		msg("sess-1", "u1", 0, transcript.KindUser, text))
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	failed := errors.New("disk I/O error")
	testHookCommit = func() error { testHookCommit = nil; return failed }
	defer func() { testHookCommit = nil }()
	if _, err := s.RedactMessage(ctx, LocalRedaction{Session: "sess-1", Ordinal: transcript.OrdinalAt(0, 0), From: 2, To: 2}); !errors.Is(err, failed) {
		t.Fatalf("redaction over a failed commit: %v", err)
	}
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	noLeak(t, s, "BLUEFALCON", "after the failed commit")
}

// A partial (line range) redaction keys on the hidden line, so a message
// that grows after it (a streamed reply) or is rewritten (lines moved, a
// new version) stays masked, in rows and FTS.
func TestLocalRedactionPartialSurvivesGrowthAndRewrite(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "defer"}[deferred], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "index.db")
			s, err := Open(path, Options{DeferCommit: deferred})
			if err != nil {
				t.Fatal(err)
			}
			text := "first\ncodename BLUEFALCON-7731\nlast"
			src := source(t, s, transcript.AgentClaude, "/h/s1.jsonl")
			write := func(s *Store, text string) {
				sinkMsgs(t, s, src.ID, 1, &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "sess-1"},
					msg("sess-1", "u1", 0, transcript.KindUser, text), msg("sess-1", "a1", 1, transcript.KindAssistant, "unrelated reply"))
				if err := s.Sync(ctx); err != nil {
					t.Fatal(err)
				}
			}
			write(s, text)
			if _, err := s.RedactMessage(ctx, LocalRedaction{Session: "sess-1", Ordinal: transcript.OrdinalAt(0, 0), From: 2, To: 2}); err != nil {
				t.Fatal(err)
			}
			noLeak(t, s, "BLUEFALCON", "redacted")
			// Streaming growth: the record gains lines (a new hash).
			write(s, text+"\nmore streamed output")
			noLeak(t, s, "BLUEFALCON", "grown")
			if r := rowsOf(t, s, "u1"); !strings.Contains(r[len(r)-1].text, "more streamed output") {
				t.Fatalf("growth not stored: %+v", r)
			}
			// Rewrite: the line moves and its neighbours change.
			write(s, "intro\nfirst changed\ncodename BLUEFALCON-7731\nend")
			noLeak(t, s, "BLUEFALCON", "rewritten")
			if r := rowsOf(t, s, "u1"); !strings.HasPrefix(r[len(r)-1].text, "intro\nfirst changed\n[REDACTED:message]") {
				t.Fatalf("rewrite: %+v", r)
			}
			eq(t, "neighbour", findIDs(t, s, "unrelated", FindOptions{}), []string{"a1"})
			s.Close()

			// A rebuilt index beside the sidecar masks the rewritten version.
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
			src = source(t, s2, transcript.AgentClaude, "/h/s1.jsonl")
			write(s2, "intro\ncodename BLUEFALCON-7731")
			noLeak(t, s2, "BLUEFALCON", "rebuilt")
		})
	}
}

// A title is masked by content, not only by an exact match of the title
// the redaction saw: a later session whose title is the redacted line
// truncated elsewhere, or a title that contains the line, is masked. A
// title that shares only its start with the line is not.
func TestLocalRedactionMasksTitleByContent(t *testing.T) {
	s := openTest(t, DetailFull)
	line := "please rotate the staging key BLUEFALCON-7731 before the demo tomorrow morning"
	src := source(t, s, transcript.AgentClaude, "/h/s1.jsonl")
	sinkMsgs(t, s, src.ID, 1, &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "sess-1", Title: line},
		msg("sess-1", "u1", 0, transcript.KindUser, line+"\nthanks"))
	if _, err := s.RedactMessage(ctx, LocalRedaction{Session: "sess-1", Ordinal: transcript.OrdinalAt(0, 0), From: 1, To: 1}); err != nil {
		t.Fatal(err)
	}
	titles := map[string]string{
		"sess-2": line[:40],                        // truncated at another length
		"sess-3": line[:45] + " ",                  // truncated, then a trailing space
		"sess-4": "Re: " + line,                    // holds the whole line
		"sess-5": "  " + line + "  ",               // the line, padded
		"sess-6": "please rotate the logs nightly", // shares a start, then differs: kept
		"sess-7": "unrelated title",
	}
	for sess, title := range titles {
		src := source(t, s, transcript.AgentClaude, "/h/"+sess+".jsonl")
		sinkMsgs(t, s, src.ID, 1, &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: sess, Title: title},
			msg(sess, "m-"+sess, 0, transcript.KindUser, "hello from "+sess))
	}
	for sess, title := range titles {
		var got string
		if err := s.DB().QueryRow(`SELECT ifnull(title, '') FROM conversations WHERE session_id = ?`, sess).Scan(&got); err != nil {
			t.Fatal(err)
		}
		keep := sess == "sess-6" || sess == "sess-7"
		if keep && got != title {
			t.Errorf("%s: title %q masked to %q", sess, title, got)
		}
		if !keep && (strings.Contains(got, "BLUEFALCON") || strings.Contains(got, "rotate")) {
			t.Errorf("%s: title %q kept as %q", sess, title, got)
		}
	}
}

// A short write to the sidecar (a full disk) fails that redaction and
// leaves the sidecar as it was, so the next redaction's tombstone is
// loaded intact; a sidecar that is corrupt anyway refuses to open the
// index rather than drop tombstones.
func TestLocalRedactionSidecarShortWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	index := func(s *Store) {
		src := source(t, s, transcript.AgentClaude, "/h/s1.jsonl")
		sinkMsgs(t, s, src.ID, 1, &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "sess-1"},
			msg("sess-1", "u1", 0, transcript.KindUser, "codename BLUEFALCON-7731 alpha"),
			msg("sess-1", "u2", 1, transcript.KindUser, "codename REDHERON-4410 bravo"))
	}
	index(s)
	testHookSidecarWrite = func(f *os.File, b []byte) (int, error) {
		testHookSidecarWrite = nil
		n, _ := f.Write(b[:len(b)/2])
		return n, syscall.ENOSPC
	}
	defer func() { testHookSidecarWrite = nil }()
	if _, err := s.RedactMessage(ctx, LocalRedaction{Session: "sess-1", Ordinal: transcript.OrdinalAt(0, 0)}); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("redaction on a full disk: %v", err)
	}
	if _, err := s.RedactMessage(ctx, LocalRedaction{Session: "sess-1", Ordinal: transcript.OrdinalAt(1, 0)}); err != nil {
		t.Fatal(err)
	}
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
	index(s2)
	noLeak(t, s2, "REDHERON", "rebuilt")
	s2.Close()

	// A torn last line (written by something else) refuses to open.
	path3 := filepath.Join(t.TempDir(), "index.db")
	os.WriteFile(path3+".redactions.jsonl", append(side, side[:len(side)/2]...), 0o600)
	if s3, err := Open(path3, Options{}); err == nil || !strings.Contains(err.Error(), "redactions.jsonl") || !strings.Contains(err.Error(), RecoveryDoc) {
		if s3 != nil {
			s3.Close()
		}
		t.Fatalf("opened over a corrupt sidecar: %v", err)
	}
}

// A line range that hides only whitespace lines, on a record with an
// older version, records no tombstone that carries nothing: the sidecar
// still loads, so the index opens again.
func TestLocalRedactionBlankRangeKeepsSidecarLoadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	src := source(t, s, transcript.AgentClaude, "/h/s1.jsonl")
	for _, text := range []string{"first\n   \nlast", "changed\n   \nlast"} {
		sinkMsgs(t, s, src.ID, 1, &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "sess-1"},
			msg("sess-1", "u1", 0, transcript.KindUser, text))
	}
	if r := rowsOf(t, s, "u1"); len(r) < 2 {
		t.Fatalf("want two versions, got %+v", r)
	}
	if _, err := s.RedactMessage(ctx, LocalRedaction{Session: "sess-1", Ordinal: transcript.OrdinalAt(0, 0), From: 2, To: 2}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("reopen after a blank-line redaction: %v", err)
	}
	s2.Close()
}

// The directory sync fails after the new sidecar was renamed into place:
// the redaction reports the error, but the renamed file is what the index
// now holds. Its tombstones apply at once (the writer reconciles after the
// failure), and the next redaction does not see a changed sidecar.
func TestLocalRedactionDirSyncFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path, Options{DeferCommit: true})
	if err != nil {
		t.Fatal(err)
	}
	src := source(t, s, transcript.AgentClaude, "/h/s1.jsonl")
	sinkMsgs(t, s, src.ID, 1, &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "sess-1"},
		msg("sess-1", "u1", 0, transcript.KindUser, "codename BLUEFALCON-7731 alpha"),
		msg("sess-1", "u2", 1, transcript.KindUser, "codename REDHERON-4410 bravo"))
	failed := errors.New("fsync: input/output error")
	testHookDirSync = func(p string) error {
		if p != path+".redactions.jsonl" {
			return nil
		}
		testHookDirSync = nil
		return failed
	}
	defer func() { testHookDirSync = nil }()
	if _, err := s.RedactMessage(ctx, LocalRedaction{Session: "sess-1", Ordinal: transcript.OrdinalAt(0, 0)}); !errors.Is(err, failed) {
		t.Fatalf("redaction over a failed directory sync: %v", err)
	}
	noLeak(t, s, "BLUEFALCON", "after the failed directory sync")
	if _, err := s.RedactMessage(ctx, LocalRedaction{Session: "sess-1", Ordinal: transcript.OrdinalAt(1, 0)}); err != nil {
		t.Fatalf("next redaction: %v", err)
	}
	noLeak(t, s, "REDHERON", "next redaction")
	s.Close()
	s2, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	noLeak(t, s2, "BLUEFALCON", "reopened")
}

// Each atomic sidecar write keeps the sidecar it replaces as .prev, the
// last good copy the recovery procedure restores.
func TestLocalRedactionKeepsPreviousSidecar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	src := source(t, s, transcript.AgentClaude, "/h/s1.jsonl")
	sinkMsgs(t, s, src.ID, 1, &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "sess-1"},
		msg("sess-1", "u1", 0, transcript.KindUser, "codename BLUEFALCON-7731 alpha"),
		msg("sess-1", "u2", 1, transcript.KindUser, "codename REDHERON-4410 bravo"))
	if _, err := s.RedactMessage(ctx, LocalRedaction{Session: "sess-1", Ordinal: transcript.OrdinalAt(0, 0)}); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path + ".redactions.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedactMessage(ctx, LocalRedaction{Session: "sess-1", Ordinal: transcript.OrdinalAt(1, 0)}); err != nil {
		t.Fatal(err)
	}
	prev, err := os.ReadFile(path + ".redactions.jsonl.prev")
	if err != nil || string(prev) != string(first) {
		t.Fatalf(".prev %q (%v), want %q", prev, err, first)
	}
}

// A reconcile that fails when the index opens (here a row whose stored
// text does not decompress) stops Open with an error that points to the
// recovery procedure, and RebuildIndex, which drops the message rows and
// keeps the sidecar, opens it again; re-indexing masks the rows.
func TestLocalRedactionOpenFailureRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	text := "codename BLUEFALCON-7731 alpha"
	index := func(s *Store) {
		src := source(t, s, transcript.AgentClaude, "/h/s1.jsonl")
		sinkMsgs(t, s, src.ID, 1, &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "sess-1"},
			msg("sess-1", "u1", 0, transcript.KindUser, text))
	}
	index(s)
	s.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE messages SET text = x'00' WHERE native_id = 'u1'`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	sum := sha256.Sum256([]byte(text))
	os.WriteFile(path+".redactions.jsonl", []byte(`{"sha":"`+hex.EncodeToString(sum[:])+`","session":"sess-1","native":"u1"}`+"\n"), 0o600)
	if s, err := Open(path, Options{}); err == nil || !strings.Contains(err.Error(), RecoveryDoc) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("open over a failing reconcile: %v", err)
	}
	s2, err := Open(path, Options{RebuildIndex: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	index(s2)
	noLeak(t, s2, "BLUEFALCON", "rebuilt")
}

// A reconcile is due (a lost commit held a redaction's row masks) and
// fails; a later redaction then fails before its sidecar is renamed. That
// failure must leave the reconcile due, so the first write after the
// fault applies the earlier redaction.
func TestLocalRedactionFailedWriteKeepsReconcileDue(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "index.db"), Options{DeferCommit: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	src := source(t, s, transcript.AgentClaude, "/h/s1.jsonl")
	sinkMsgs(t, s, src.ID, 1, &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "sess-1"},
		msg("sess-1", "u1", 0, transcript.KindUser, "codename BLUEFALCON-7731 alpha"),
		msg("sess-1", "u2", 1, transcript.KindUser, "codename REDHERON-4410 bravo"))
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	failed := errors.New("disk I/O error")
	testHookReconcile = func() error { return failed }
	testHookCommit = func() error { testHookCommit = nil; return failed }
	defer func() { testHookReconcile, testHookCommit, testHookSidecarWrite = nil, nil, nil }()
	if _, err := s.RedactMessage(ctx, LocalRedaction{Session: "sess-1", Ordinal: transcript.OrdinalAt(0, 0)}); !errors.Is(err, failed) {
		t.Fatalf("redaction over a failed commit: %v", err)
	}
	testHookSidecarWrite = func(f *os.File, b []byte) (int, error) {
		if strings.Contains(filepath.Base(f.Name()), ".prev.") {
			return f.Write(b)
		}
		testHookSidecarWrite = nil
		return 0, syscall.ENOSPC
	}
	if _, err := s.RedactMessage(ctx, LocalRedaction{Session: "sess-1", Ordinal: transcript.OrdinalAt(1, 0)}); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("redaction on a full disk: %v", err)
	}
	testHookReconcile = nil
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	noLeak(t, s, "BLUEFALCON", "after the faults cleared")
}
