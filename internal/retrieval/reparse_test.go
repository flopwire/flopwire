package retrieval_test

import (
	"context"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/retrieval/format"
)

func TestReadPrioritizesStaleSessionAfterAuthorization(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	specs, dv, export := s.writeRedactFixtures()
	s.syncRedact(s.sy, specs, dv, export)
	var id, source string
	if err := s.pool.QueryRow(ctx, `SELECT m.id::text,m.source_id::text FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE c.agent='claude' LIMIT 1`).Scan(&id, &source); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE source_parse_state SET applied_redaction_rules='old-rules' WHERE source_id=$1`, source); err != nil {
		t.Fatal(err)
	}
	// Hidden sessions must not acquire priority through an unauthorized read.
	s.setRules("/w/redact")
	if _, err := s.client.Read(ctx, format.ReadQuery{Address: id}, format.Filters{}); err == nil {
		t.Fatal("hidden read succeeded")
	}
	if s.count(`SELECT count(*) FROM source_parse_state WHERE refresh_requested_at IS NOT NULL`) != 0 {
		t.Fatal("unauthorized read scheduled refresh")
	}
	s.setRules("")
	var before int
	if err := s.pool.QueryRow(ctx, `SELECT parse_attempt FROM messages WHERE id=$1`, id).Scan(&before); err != nil {
		t.Fatal(err)
	}
	out, err := s.client.Read(ctx, format.ReadQuery{Address: id}, format.Filters{})
	if err != nil || out.Focus != id {
		t.Fatalf("immediate read: %+v %v", out, err)
	}
	if s.count(`SELECT count(*) FROM source_parse_state WHERE source_id=$1 AND refresh_requested_at IS NOT NULL`, source) != 1 {
		t.Fatal("authorized read did not prioritize refresh outside read transaction")
	}
	if s.count(`SELECT count(*) FROM messages WHERE id=$1 AND parse_attempt=$2`, id, before) != 1 {
		t.Fatal("read waited for or performed extraction")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE source_parse_state SET refresh_requested_at=NULL WHERE source_id=$1`, source); err != nil {
		t.Fatal(err)
	}
	if _, err := s.client.Read(ctx, format.ReadQuery{Address: id, Outline: true}, format.Filters{}); err != nil {
		t.Fatal(err)
	}
	if s.count(`SELECT count(*) FROM source_parse_state WHERE source_id=$1 AND refresh_requested_at IS NOT NULL`, source) != 1 {
		t.Fatal("outline did not prioritize refresh")
	}
	// A busy priority write must time out without failing the existing read.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT source_id FROM source_parse_state WHERE source_id=$1 FOR UPDATE`, source); err != nil {
		t.Fatal(err)
	}
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := s.client.Read(readCtx, format.ReadQuery{Address: id}, format.Filters{}); err != nil {
		t.Fatalf("priority contention failed read: %v", err)
	}

}
