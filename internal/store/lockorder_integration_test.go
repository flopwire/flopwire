package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// waitForLockWait waits until a backend of the test database waits on a
// lock.
func waitForLockWait(t *testing.T, f deletionFixture) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for f.count(t, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'`) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("nothing ever waited on a lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func isDeadlock(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "40P01"
}

// A deletion removes a session and its subagents: several conversations.
// A checkpoint or recount of the same conversations (ingest.recountDigests,
// lockCheckpointSQL) locks them in session order. The deletion must lock
// them in that order too, or the two deadlock: here the subagent's session
// sorts first while its id (and row position) sorts last.
func TestDeletionLocksConversationsInSessionOrder(t *testing.T) {
	ctx := context.Background()
	f := newDeletionFixture(t)
	child := "ffffffff-ffff-4fff-bfff-ffffffffffff"
	exec(t, f.pool, `INSERT INTO conversations(id,agent,session_id,device_id,user_id,parent_conversation_id,parent_native_session_id,depth)
		VALUES($1,'codex','a-child',$2,$3,$4,'sess-1',1)`, child, f.device, f.user, f.convA)

	// The recount: the first conversation in session order is held.
	recount, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer recount.Rollback(ctx)
	if _, err := recount.Exec(ctx, `SELECT 1 FROM conversations WHERE id=$1 FOR UPDATE`, child); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.p.RequestConversationDeletion(ctx, f.convA, f.user, f.device, true)
		done <- err
	}()
	waitForLockWait(t, f)
	// The recount takes the next one in order, then commits.
	_, lockErr := recount.Exec(ctx, `SELECT 1 FROM conversations WHERE id=$1 FOR UPDATE`, f.convA)
	if lockErr == nil {
		lockErr = recount.Commit(ctx)
	} else {
		_ = recount.Rollback(ctx)
	}
	delErr := <-done
	if isDeadlock(lockErr) || isDeadlock(delErr) {
		t.Fatalf("deletion deadlocked with a recount: recount %v, deletion %v", lockErr, delErr)
	}
	if lockErr != nil || delErr != nil {
		t.Fatalf("recount %v, deletion %v", lockErr, delErr)
	}
	if n := f.count(t, `SELECT count(*) FROM conversations WHERE id=$1 OR id=$2`, f.convA, child); n != 0 {
		t.Fatalf("%d doomed conversations left", n)
	}
}
