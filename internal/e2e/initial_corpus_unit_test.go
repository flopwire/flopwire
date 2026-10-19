package e2e

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCorpusSnapshotVerdictAndMissingRows(t *testing.T) {
	start := time.Now()
	clean := corpusSnapshot{pgAt: start.Add(301 * time.Second), localAt: start.Add(301 * time.Second)}
	if snapshotWithinDeadline(start, clean, 5*time.Minute) {
		t.Fatal("late clean observation cannot qualify the five-minute gate")
	}
	beforeStart := corpusSnapshot{pgAt: start.Add(-time.Second), localAt: start.Add(time.Second)}
	if snapshotWithinDeadline(start, beforeStart, 5*time.Minute) {
		t.Fatal("snapshot before agent start cannot qualify the gate")
	}
	within := corpusSnapshot{pgAt: start.Add(295 * time.Second), localAt: start.Add(296 * time.Second)}
	if !snapshotWithinDeadline(start, within, 5*time.Minute) {
		t.Fatal("bounded snapshot was rejected")
	}
	wantKeys := map[string]int{"source#0": 1}
	wantContent := map[string]int{"fingerprint": 1}
	problems := compareCorpusSession("synthetic", wantKeys, wantKeys, wantContent, nil, nil, nil)
	if len(problems) != 3 {
		t.Fatalf("missing local keys, server keys and server content must all be reported: %v", problems)
	}
	problems = compareCorpusSession("synthetic", wantKeys, wantKeys, wantContent, wantKeys, wantKeys, wantContent)
	if len(problems) != 0 {
		t.Fatalf("matched source and snapshots failed: %v", problems)
	}
}

func TestExpectedDevinKeysFromSource(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "sessions.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := os.ReadFile(filepath.Join("..", "..", "testdata", "oracle", "seeds", "devin.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(seed)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	keys, err := expectedDevinKeys(context.Background(), root, dbPath, "devin-oracle-001")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) == 0 {
		t.Fatal("source export yielded no Devin keys")
	}
}

func TestCorpusLocalKeysUsesOneSnapshotAcrossSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE conversations (id INTEGER PRIMARY KEY, agent TEXT, session_id TEXT);
		CREATE TABLE messages (conversation_id INTEGER, native_id TEXT, byte_offset INTEGER, part INTEGER, superseded INTEGER);
		INSERT INTO conversations VALUES (1, 'claude', 'A'), (2, 'claude', 'B')`); err != nil {
		t.Fatal(err)
	}
	reader, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	ctx := context.Background()
	readFresh := func(session string) map[string]int {
		t.Helper()
		tx, err := reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		keys, err := corpusLocalKeys(ctx, tx, "claude", session)
		if err != nil {
			t.Fatal(err)
		}
		return keys
	}
	// Separate observations can claim A is absent even though both sessions
	// are complete by the time the sweep reaches B.
	oldA := readFresh("A")
	if _, err := db.Exec(`INSERT INTO messages VALUES (1, 'a', 0, 0, 0), (2, 'b', 0, 0, 0)`); err != nil {
		t.Fatal(err)
	}
	oldB := readFresh("B")
	if len(oldA) != 0 || oldB["b#0"] != 1 || readFresh("A")["a#0"] != 1 {
		t.Fatalf("expected mixed-time sweep: A=%v B=%v", oldA, oldB)
	}
	// A fixed read transaction sees the same complete state even after the
	// writer removes both rows. The next transaction sees their removal.
	snapshot, err := reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Rollback()
	snapA, err := corpusLocalKeys(ctx, snapshot, "claude", "A")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM messages`); err != nil {
		t.Fatal(err)
	}
	snapB, err := corpusLocalKeys(ctx, snapshot, "claude", "B")
	if err != nil {
		t.Fatal(err)
	}
	if snapA["a#0"] != 1 || snapB["b#0"] != 1 {
		t.Fatalf("snapshot drifted across writer commit: A=%v B=%v", snapA, snapB)
	}
	if got := readFresh("B"); len(got) != 0 {
		t.Fatalf("fresh read missed writer deletion: %v", got)
	}
}
