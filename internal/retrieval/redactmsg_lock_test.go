package retrieval_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/jackc/pgx/v5/pgconn"
)

// A redaction rewrites message rows and their conversations' titles and
// digests. A parse flush of the same conversation holds the conversation
// from its upsert and then writes the message rows; the redaction must
// take the conversation first too, or the two deadlock.
func TestRedactionAndConcurrentFlushDoNotDeadlock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s := newServer(t)
	specs, dv, export := s.writeRedactFixtures()
	s.syncRedact(s.sy, specs, dv, export)
	var msg, conv string
	if err := s.pool.QueryRow(ctx, `SELECT m.id::text,m.conversation_id::text FROM messages m JOIN conversations c ON c.id=m.conversation_id
		WHERE c.agent='claude' AND strpos(m.text,'BLUEFALCON')>0`).Scan(&msg, &conv); err != nil {
		t.Fatal(err)
	}
	// The flush: it holds the conversation (its upsert).
	flush, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer flush.Rollback(ctx)
	if _, err := flush.Exec(ctx, `UPDATE conversations SET last_activity_at=last_activity_at WHERE id=$1`, conv); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.redact(s.client, "/v1/redactions", format.RedactRequest{Address: msg + ":2-2"})
		done <- err
	}()
	deadline := time.Now().Add(20 * time.Second)
	for s.count(`SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'`) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the redaction never waited on the flush")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The flush then writes the conversation's message rows and commits.
	_, flushErr := flush.Exec(ctx, `UPDATE messages SET enrichment=enrichment WHERE id=$1`, msg)
	if flushErr == nil {
		flushErr = flush.Commit(ctx)
	}
	redactErr := <-done
	var pe *pgconn.PgError
	if errors.As(flushErr, &pe) && pe.Code == "40P01" {
		t.Fatalf("the flush deadlocked with the redaction: %v", flushErr)
	}
	if flushErr != nil || redactErr != nil {
		t.Fatalf("flush %v, redaction %v", flushErr, redactErr)
	}
	if n := s.rowsWith("BLUEFALCON"); n != 2 {
		t.Fatalf("%d rows hold the codename, want the two other copies", n)
	}
}
