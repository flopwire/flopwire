package ingest

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/perfguard"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/google/uuid"
)

func workerCommit(t *testing.T, e *env) string {
	t.Helper()
	a := &appendSession{e: e, sy: e.syncer(devicesync.Config{SealAfter: -1}), session: perfguard.ClaudeSession{SessionID: uuid.NewString()}, next: 8}
	path := filepath.Join(t.TempDir(), a.session.SessionID+".jsonl")
	if err := os.WriteFile(path, a.session.Lines(0, 8), 0600); err != nil {
		t.Fatal(err)
	}
	a.spec = devicesync.SourceSpec{Path: path, Agent: transcript.AgentClaude, StorageKind: transcript.StorageJSONLAppend, Parser: "claude@1"}
	if err := a.sy.Sync(e.ctx, a.spec); err != nil {
		t.Fatal(err)
	}
	e.drain()
	if err := e.pool.QueryRow(e.ctx, `SELECT id::text FROM sources WHERE path=$1`, path).Scan(&a.id); err != nil {
		t.Fatal(err)
	}
	lines := a.toolCalls(a.next, 1, 0)
	lines = bytes.ReplaceAll(lines, []byte(`"command":"false"`), []byte(`"command":"git commit -m synthetic"`))
	lines = bytes.ReplaceAll(lines, []byte("exit status 1"), []byte("[main abcdef0] synthetic"))
	a.add(t, lines)
	var conv string
	if err := e.pool.QueryRow(e.ctx, `SELECT id::text FROM conversations WHERE session_id=$1`, a.session.SessionID).Scan(&conv); err != nil {
		t.Fatal(err)
	}
	return conv
}

func TestCommitEvidenceWorkerAutonomousAndCancellation(t *testing.T) {
	e := newEnv(t)
	conv := workerCommit(t, e)
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan struct{})
	go func() { defer close(done); (&CommitEvidenceWorker{Pool: e.pool}).Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(5 * time.Second)
	for e.count(`SELECT count(*) FROM commit_evidence WHERE conversation_id=$1`, conv) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("background worker never published evidence")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not join after cancellation")
	}
}

func TestCommitEvidenceWorkerTimeoutDefersAndProgresses(t *testing.T) {
	e := newEnv(t)
	first, second := workerCommit(t, e), workerCommit(t, e)
	if first > second {
		first, second = second, first
	}
	e.exec(`CREATE FUNCTION slow_evidence() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.conversation_id::text='` + first + `' THEN PERFORM pg_sleep(2); END IF; RETURN NEW; END $$`)
	e.exec(`CREATE TRIGGER slow_evidence BEFORE INSERT ON commit_evidence FOR EACH ROW EXECUTE FUNCTION slow_evidence()`)
	w := &CommitEvidenceWorker{Pool: e.pool, Timeout: 100 * time.Millisecond}
	if worked, err := w.step(e.ctx, false); !worked || err == nil {
		t.Fatalf("timeout: worked=%v err=%v", worked, err)
	}
	if e.count(`SELECT count(*) FROM commit_evidence_dirty WHERE conversation_id=$1 AND retry_after>now()`, first) != 1 {
		t.Fatal("timed-out conversation was not deferred")
	}
	if e.count(`SELECT count(*) FROM commit_evidence WHERE conversation_id=$1`, first) != 0 {
		t.Fatal("partial projection published")
	}
	w.Timeout = 2 * time.Second
	if worked, err := w.step(e.ctx, false); !worked || err != nil {
		t.Fatalf("progress: worked=%v err=%v", worked, err)
	}
	if e.count(`SELECT count(*) FROM commit_evidence WHERE conversation_id=$1`, second) != 1 {
		t.Fatal("deferred first conversation starved second")
	}
}

func TestCommitEvidenceWorkerSkipsLockedConversation(t *testing.T) {
	e := newEnv(t)
	first, second := workerCommit(t, e), workerCommit(t, e)
	if first > second {
		first, second = second, first
	}
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(e.ctx)
	if _, err = tx.Exec(e.ctx, `SELECT id FROM conversations WHERE id=$1 FOR UPDATE`, first); err != nil {
		t.Fatal(err)
	}
	if worked, err := (&CommitEvidenceWorker{Pool: e.pool}).step(e.ctx, false); !worked || err != nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if e.count(`SELECT count(*) FROM commit_evidence WHERE conversation_id=$1`, second) != 1 {
		t.Fatal("locked conversation blocked independent work")
	}
}

func TestCommitEvidenceWorkerRecentAndFairSelections(t *testing.T) {
	e := newEnv(t)
	old, recent := workerCommit(t, e), workerCommit(t, e)
	if old > recent {
		old, recent = recent, old
	}
	e.exec(`UPDATE source_parse_state SET parsed_at=now()-interval '1 day' WHERE source_id=(SELECT source_id FROM conversations WHERE id=$1)`, old)
	e.exec(`UPDATE source_parse_state SET parsed_at=now() WHERE source_id=(SELECT source_id FROM conversations WHERE id=$1)`, recent)
	w := &CommitEvidenceWorker{Pool: e.pool}
	if _, err := w.step(e.ctx, true); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT count(*) FROM commit_evidence WHERE conversation_id=$1`, recent) != 1 {
		t.Fatal("recent work was not prioritized")
	}
	// A hot source becomes dirty again before the next turn.
	e.exec(`UPDATE messages SET text=text||' ' WHERE conversation_id=$1 AND kind='tool_result'`, recent)
	if _, err := w.step(e.ctx, false); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT count(*) FROM commit_evidence WHERE conversation_id=$1`, old) != 1 {
		t.Fatal("hot recent source starved historical work")
	}
}
