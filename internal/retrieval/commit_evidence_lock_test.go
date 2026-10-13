package retrieval

import (
	"context"
	"errors"
	"github.com/flopwire/flopwire/internal/ingest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"strings"
	"testing"
	"time"
)

func TestCommitEvidenceDirtyDoesNotLockConversation(t *testing.T) {
	f := newChangeFixture(t)
	msg, _ := f.commit("abcdef1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	writer, err := f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(ctx)
	if _, err = writer.Exec(ctx, `SELECT 1 FROM messages WHERE id=$1 FOR UPDATE`, msg); err != nil {
		t.Fatal(err)
	}
	redactor, err := f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer redactor.Rollback(ctx)
	if _, err = redactor.Exec(ctx, `SELECT 1 FROM conversations WHERE id=$1 FOR UPDATE`, f.conv); err != nil {
		t.Fatal(err)
	}
	// The writer must complete while the conversation remains locked. A dirty
	// queue foreign key would block here, before the redactor can lock messages.
	if _, err = writer.Exec(ctx, `SET LOCAL lock_timeout='1s'`); err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Exec(ctx, `UPDATE messages SET text=text||' changed' WHERE id=$1`, msg); err != nil {
		t.Fatal(err)
	}
	if err = writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = redactor.Exec(ctx, `SELECT 1 FROM messages WHERE id=$1 FOR UPDATE`, msg); err != nil {
		t.Fatal(err)
	}
	if err = redactor.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Pool.Exec(ctx, `DELETE FROM conversations WHERE id=$1`, f.conv); err != nil {
		t.Fatal(err)
	}
	var dirty bool
	if err = f.s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM commit_evidence_dirty WHERE conversation_id=$1)`, f.conv).Scan(&dirty); err != nil {
		t.Fatal(err)
	}
	if dirty {
		t.Fatal("deleted conversation remains in dirty queue")
	}
}

func TestCommitEvidencePublicationKeepsConcurrentRewrite(t *testing.T) {
	f := newChangeFixture(t)
	oldHash := strings.Repeat("a", 40)
	newHash := strings.Repeat("b", 40)
	_, result := f.commit(oldHash)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	writer, err := f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(ctx)
	var writerPID int
	if err = writer.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&writerPID); err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Exec(ctx, `SELECT 1 FROM messages WHERE id=$1 FOR UPDATE`, result); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- ingest.BackfillCommitEvidenceFor(ctx, f.s.Pool, []string{f.conv}, 1) }()
	// Wait until projection reads have reached this locked message. The writer
	// then changes the exact bytes the projection would otherwise publish.
	for {
		var waiting bool
		if err = f.s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))`, writerPID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err = <-done:
			t.Fatalf("projection completed before locking retained message: %v", err)
		default:
		}
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err = writer.Exec(ctx, `UPDATE messages SET text=replace(text,$2,$3) WHERE id=$1`, result, oldHash, newHash); err != nil {
		t.Fatal(err)
	}
	if err = writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	var stale int
	if err = f.s.Pool.QueryRow(ctx, `SELECT count(*) FROM commit_evidence WHERE conversation_id=$1 AND observed_hash=$2`, f.conv, oldHash).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale != 0 {
		t.Fatal("projection published text from before concurrent rewrite")
	}
	if got := f.lookup([]string{newHash}, 10, ""); len(got.Matches) != 1 {
		t.Fatalf("rewritten evidence missing: %+v", got)
	}
	if got := f.lookup([]string{oldHash}, 10, ""); len(got.Matches) != 0 {
		t.Fatalf("stale evidence retained: %+v", got)
	}
}

type commitEvidencePublicationTx struct {
	pgx.Tx
	beforeInsert func() error
}

func (tx *commitEvidencePublicationTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if tx.beforeInsert != nil && strings.Contains(sql, "INSERT INTO commit_evidence(") {
		hook := tx.beforeInsert
		tx.beforeInsert = nil
		if err := hook(); err != nil {
			return pgconn.CommandTag{}, err
		}
	}
	return tx.Tx.Exec(ctx, sql, args...)
}
func TestCommitEvidencePublicationRetriesOmittedRowRevival(t *testing.T) {
	f := newChangeFixture(t)
	hash := strings.Repeat("c", 40)
	f.commit(hash)
	_, result := f.commit(hash)
	ctx := context.Background()
	if _, err := f.s.Pool.Exec(ctx, `UPDATE messages SET on_active_path=false WHERE id=$1`, result); err != nil {
		t.Fatal(err)
	}
	tx, err := f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT 1 FROM conversations WHERE id=$1 FOR UPDATE`, f.conv); err != nil {
		t.Fatal(err)
	}
	fired := false
	hooked := &commitEvidencePublicationTx{Tx: tx, beforeInsert: func() error {
		fired = true
		_, err := f.s.Pool.Exec(ctx, `UPDATE messages SET on_active_path=true WHERE id=$1`, result)
		return err
	}}
	if err = ingest.RefreshCommitEvidence(ctx, hooked, f.conv); !errors.Is(err, ingest.ErrCommitEvidenceMoved) {
		t.Fatalf("publication error: %v", err)
	}
	if !fired {
		t.Fatal("publication hook did not run")
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var dirty bool
	if err = f.s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM commit_evidence_dirty WHERE conversation_id=$1)`, f.conv).Scan(&dirty); err != nil {
		t.Fatal(err)
	}
	if !dirty {
		t.Fatal("concurrent revival lost dirty marker")
	}
	var count int
	if err = f.s.Pool.QueryRow(ctx, `SELECT count(*) FROM commit_evidence WHERE conversation_id=$1`, f.conv).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("partial projection survived rollback")
	}
	if got := f.lookup([]string{hash}, 10, ""); len(got.Matches) != 2 {
		t.Fatalf("revived evidence missing: %+v", got)
	}
}
