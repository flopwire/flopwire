package localindex

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

func columnLifecycleOpen(t *testing.T, path string, detail Detail, deferred bool) *Store {
	t.Helper()
	s, err := Open(path, Options{TokDetail: DetailFull, TriDetail: detail, TriParts: 2, DeferCommit: deferred})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func columnLifecycleHits(t *testing.T, s *Store, text string, want int) {
	t.Helper()
	hits, err := s.Find(ctx, text, FindOptions{})
	if err != nil || len(hits) != want {
		t.Fatalf("Find(%q): %d, %v; want %d", text, len(hits), err, want)
	}
	got, err := s.Search(ctx, text, SearchOptions{})
	if err != nil || len(got) != want {
		t.Fatalf("Search(%q): %d, %v; want %d", text, len(got), err, want)
	}
}

func columnLifecycleMode(t *testing.T, s *Store, detail Detail) {
	t.Helper()
	var tok, tri string
	var parts int
	if err := s.DB().QueryRow(`SELECT (SELECT value FROM meta WHERE key='tok_detail'),(SELECT value FROM meta WHERE key='tri_detail'),(SELECT value FROM meta WHERE key='tri_parts')`).Scan(&tok, &tri, &parts); err != nil {
		t.Fatal(err)
	}
	if tok != "full" || tri != string(detail) || parts != 2 {
		t.Fatalf("persisted mode %s/%s/%d", tok, tri, parts)
	}
}

// These mutations exercise actual queue inserts/deletes and physical redaction
// compaction, with the positional index as the same-operation control.
func TestTrigramColumnLifecycleMutations(t *testing.T) {
	for _, detail := range []Detail{DetailFull, DetailColumn} {
		t.Run(string(detail), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "index.db")
			s := columnLifecycleOpen(t, path, detail, false)
			src := source(t, s, transcript.AgentClaude, "/synthetic/lifecycle.jsonl")
			put := func(native string, ordinal int64, text string) {
				apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{msg("column-life", native, ordinal, transcript.KindAssistant, text)}})
			}
			put("grow", 0, "growprefix")
			put("grow", 0, "growprefix appendedneedle")
			columnLifecycleHits(t, s, "appendedneedle", 1)
			put("grow", 0, "replacementneedle")
			columnLifecycleHits(t, s, "appendedneedle", 0)
			columnLifecycleHits(t, s, "replacementneedle", 1)
			put("secret", 1, "neighbour\nzebracorn 4417\nremaining")
			columnLifecycleHits(t, s, "zebracorn", 1)
			if !columnLifecycleStoredToken(t, path, "zebracorn") || !columnLifecycleStoredToken(t, path, "4417") {
				t.Fatal("physical scrub fixture tokens absent before redaction")
			}
			if n, err := s.RedactMessage(ctx, LocalRedaction{Session: "column-life", Ordinal: transcript.OrdinalAt(1, 0), From: 2, To: 2}); err != nil || n != 1 {
				t.Fatalf("redact %d: %v", n, err)
			}
			columnLifecycleHits(t, s, "zebracorn", 0)
			columnLifecycleHits(t, s, "remaining", 1)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			// Close completes scrub; inspect database and WAL bytes, excluding the
			// deliberately persistent redaction sidecar.
			files, err := filepath.Glob(path + "*")
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range files {
				if strings.Contains(file, "redactions") {
					continue
				}
				b, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(bytes.ToLower(b), []byte("zebracorn")) || bytes.Contains(b, []byte("4417")) {
					t.Fatalf("redacted token retained in %s", filepath.Base(file))
				}
			}
			s = columnLifecycleOpen(t, path, detail, false)
			defer s.Close()
			columnLifecycleMode(t, s, detail)
			columnLifecycleHits(t, s, "replacementneedle", 1)
			columnLifecycleHits(t, s, "zebracorn", 0)
			if n, err := s.SupersedeSession(ctx, transcript.AgentClaude, "column-life", 2); err != nil || n == 0 {
				t.Fatalf("supersede %d: %v", n, err)
			}
			columnLifecycleHits(t, s, "replacementneedle", 0)
		})
	}
}

func TestTrigramColumnSyncWaitsForQueuedShard(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once, released sync.Once
	shardFault = func(sh *ftsShard) error {
		if sh.schema == "tri0" {
			once.Do(func() { close(entered) })
			<-release
		}
		return nil
	}
	defer func() { shardFault = nil }()
	s := columnLifecycleOpen(t, filepath.Join(t.TempDir(), "index.db"), DetailColumn, true)
	defer s.Close()
	defer released.Do(func() { close(release) })
	src := source(t, s, transcript.AgentClaude, "/synthetic/queued.jsonl")
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{msg("queue", "a", 0, transcript.KindUser, "queuedneedle first"), msg("queue", "b", 1, transcript.KindAssistant, "queuedneedle second")}})
	done := make(chan error, 1)
	go func() { done <- s.Sync(ctx) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("shard never entered apply")
	}
	var pending int
	if err := s.DB().QueryRow(`SELECT count(*) FROM fts_queue`).Scan(&pending); err != nil || pending == 0 {
		t.Fatalf("committed queue %d: %v", pending, err)
	}
	select {
	case err := <-done:
		t.Fatalf("Sync acknowledged blocked shard: %v", err)
	default:
	}
	released.Do(func() { close(release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Sync did not acknowledge released shard")
	}
	columnLifecycleHits(t, s, "queuedneedle", 2)
}

func TestTrigramColumnCrashQueueRecovery(t *testing.T) {
	if path := os.Getenv("FLOPWIRE_COLUMN_CRASH_INDEX"); path != "" {
		entered := make(chan struct{})
		var once sync.Once
		shardFault = func(sh *ftsShard) error {
			if sh.schema == "tri0" {
				once.Do(func() { close(entered) })
				select {}
			}
			return nil
		}
		s := columnLifecycleOpen(t, path, DetailColumn, true)
		src := source(t, s, transcript.AgentClaude, "/synthetic/crash.jsonl")
		apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{msg("crash", "a", 0, transcript.KindUser, "crashreplayneedle first"), msg("crash", "b", 1, transcript.KindAssistant, "crashreplayneedle second")}})
		go s.Sync(ctx)
		<-entered
		var pending, rows int
		if err := s.DB().QueryRow(`SELECT count(*) FROM fts_queue`).Scan(&pending); err != nil || pending == 0 {
			t.Fatal("missing committed queue", err)
		}
		if err := s.DB().QueryRow(`SELECT count(*) FROM messages`).Scan(&rows); err != nil || rows != 2 {
			t.Fatal("missing committed rows", err)
		}
		os.Exit(73) // No Close, checkpoint, or shard acknowledgement.
	}
	path := filepath.Join(t.TempDir(), "index.db")
	c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(c, os.Args[0], "-test.run=^TestTrigramColumnCrashQueueRecovery$")
	cmd.Env = append(os.Environ(), "FLOPWIRE_COLUMN_CRASH_INDEX="+path)
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 73 {
		t.Fatalf("child did not reach committed crash point: %v\n%s", err, output)
	}
	s := columnLifecycleOpen(t, path, DetailColumn, false)
	defer s.Close()
	columnLifecycleMode(t, s, DetailColumn)
	columnLifecycleHits(t, s, "crashreplayneedle", 2)
	var top, applied int64
	if err := s.DB().QueryRow(`SELECT seq FROM sqlite_sequence WHERE name='fts_queue'`).Scan(&top); err != nil {
		t.Fatal(err)
	}
	for _, sh := range s.shards {
		if err := sh.db.QueryRow(`SELECT value FROM fts_meta WHERE key='applied'`).Scan(&applied); err != nil || top <= 0 || applied != top {
			t.Fatalf("%s applied %d != %d: %v", sh.schema, applied, top, err)
		}
	}
}

func TestTrigramColumnInterruptedRebuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	s := columnLifecycleOpen(t, path, DetailColumn, false)
	src := source(t, s, transcript.AgentClaude, "/synthetic/rebuild.jsonl")
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{msg("rebuild", "a", 0, transcript.KindUser, "restoredneedle first"), msg("rebuild", "b", 1, transcript.KindAssistant, "restoredneedle second")}})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"-tri0")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`INSERT INTO fts_meta VALUES('rebuilding',1)`, `DELETE FROM fts_tri`, `INSERT INTO fts_tri(rowid,text) VALUES(999,'phantomneedle')`} {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s = columnLifecycleOpen(t, path, DetailColumn, false)
	columnLifecycleHits(t, s, "restoredneedle", 2)
	columnLifecycleHits(t, s, "phantomneedle", 0)
	var phantoms int
	if err := s.DB().QueryRow(`SELECT count(*) FROM tri0.fts_tri WHERE fts_tri MATCH 'pha'`).Scan(&phantoms); err != nil || phantoms != 0 {
		t.Fatalf("physical phantom entries %d: %v", phantoms, err)
	}
	columnLifecycleMode(t, s, DetailColumn)
	var markers int
	if err := s.DB().QueryRow(`SELECT count(*) FROM tri0.fts_meta WHERE key='rebuilding'`).Scan(&markers); err != nil || markers != 0 {
		t.Fatalf("rebuild marker %d: %v", markers, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + "-tri1" + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	s = columnLifecycleOpen(t, path, DetailColumn, false)
	defer s.Close()
	columnLifecycleHits(t, s, "restoredneedle", 2)
	columnLifecycleMode(t, s, DetailColumn)
}

// Inspect physical bytes only; the redaction sidecar intentionally persists.
func columnLifecycleStoredToken(t *testing.T, path, token string) bool {
	t.Helper()
	files, err := filepath.Glob(path + "*")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.Contains(file, "redactions") {
			continue
		}
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(bytes.ToLower(b), []byte(token)) {
			return true
		}
	}
	return false
}
