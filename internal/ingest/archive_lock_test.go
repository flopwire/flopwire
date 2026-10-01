package ingest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// A flush decides whether its upload needs an at-rest repair under the
// redacted-lines lock. An upload whose commit straddles the first
// redaction's commit either sees the redacted line (and records repair
// work) or commits before the redaction takes the lock (and the
// redaction's own repair finds it). Without the lock, the flush read "no
// redacted lines" while the redaction was uncommitted and then committed
// after it: neither side repaired the upload.
func TestFlushRecordsRepairUnderRedactionLock(t *testing.T) {
	e := newEnv(t)
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(e.ctx)
	if err := LockRedactedLines(e.ctx, tx); err != nil {
		t.Fatal(err)
	}
	redaction := uuid.NewString()
	if _, err := tx.Exec(e.ctx, `INSERT INTO message_redactions(id,requested_by,message_id,all_copies,by_admin,messages,chunks,tails,created_at)
		VALUES($1,$2,gen_random_uuid(),false,false,1,0,0,now())`, redaction, e.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(e.ctx, `INSERT INTO redacted_lines(line_sha,spans,redaction_id) VALUES(sha256('x'::bytea),'[{"start":0,"end":1}]',$1)`, redaction); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := tryFlush(e, "/lock.jsonl", []byte("{\"a\":\"racing upload\"}\n"))
		done <- err
	}()
	// Commit once the flush waits for the lock, or at once if it finished
	// without waiting.
	deadline := time.Now().Add(10 * time.Second)
	for waited := false; !waited; {
		select {
		case err := <-done:
			done <- err
			waited = true
		default:
			var n int
			if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n > 0 {
				waited = true
			} else if time.Now().After(deadline) {
				t.Fatal("flush neither finished nor waited")
			} else {
				time.Sleep(5 * time.Millisecond)
			}
		}
	}
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM archive_redaction_work w JOIN sources s ON s.id=w.source_id WHERE s.path='/lock.jsonl'`); n != 1 {
		t.Fatalf("%d repair records for an upload that committed after the redaction", n)
	}
}

// A redaction that bumps a work row after the worker checked it, but
// before the worker's batch commits, must not have its work cleared: the
// worker scanned with the older catalog. lockArchiveWork does not lock the
// work row, and QueueArchiveRepair does not take the source lock, so the
// batch's DELETE must itself require the revision it checked.
func TestRepairBatchKeepsWorkBumpedAfterCheck(t *testing.T) {
	e := newEnv(t)
	redaction := uuid.NewString()
	e.exec(`INSERT INTO message_redactions(id,requested_by,message_id,all_copies,by_admin,messages,chunks,tails,created_at)
		VALUES($1,$2,gen_random_uuid(),false,false,1,0,0,now())`, redaction, e.userID)
	e.exec(`INSERT INTO redacted_lines(line_sha,spans,redaction_id) VALUES(sha256('x'::bytea),'[{"start":0,"end":1}]',$1)`, redaction)
	flushOne(t, e, "/bump.jsonl", []byte("{\"a\":\"uploaded after a redaction\"}\n"))
	var source string
	if err := e.pool.QueryRow(e.ctx, `SELECT source_id::text FROM archive_redaction_work w JOIN sources s ON s.id=w.source_id WHERE s.path='/bump.jsonl'`).Scan(&source); err != nil {
		t.Fatal(err)
	}
	afterArchiveWorkLock = func() {
		afterArchiveWorkLock = nil
		ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
		defer cancel()
		if err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
			if err := LockRedactedLines(ctx, tx); err != nil {
				return err
			}
			return QueueArchiveRepair(ctx, tx, []string{source})
		}); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { afterArchiveWorkLock = nil })
	err := e.queue.repairUploadedArchive(e.ctx, source)
	if n := e.count(`SELECT count(*) FROM archive_redaction_work WHERE source_id=$1`, source); n != 1 {
		t.Fatalf("work bumped by a redaction during the batch was cleared (err %v)", err)
	}
	if !errors.Is(err, ErrArchiveChanged) {
		t.Fatalf("stale batch: %v", err)
	}
	// The retry scans with the current catalog and clears it.
	if err := e.queue.repairUploadedArchive(e.ctx, source); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM archive_redaction_work WHERE source_id=$1`, source); n != 0 {
		t.Fatalf("%d work rows after the retry", n)
	}
}
