package ingest

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Refusing a source deletes the conversations of its sessions on the
// device: several, in one statement. A checkpoint or recount of them
// locks them in session order, so the refusal must lock them in that
// order too. Here the subagent's session sorts first while its id and row
// position sort last.
func TestRefuseSourceAndConcurrentRecountDoNotDeadlock(t *testing.T) {
	e := newEnv(t)
	id := refreshedSource(t, e)
	var src source
	if err := e.pool.QueryRow(e.ctx, `SELECT s.id::text,s.device_id::text,d.user_id::text,s.agent FROM sources s JOIN devices d ON d.id=s.device_id WHERE s.id=$1`, id).
		Scan(&src.id, &src.deviceID, &src.userID, &src.agent); err != nil {
		t.Fatal(err)
	}
	main, sub := "00000000-0000-4000-8000-000000000001", "ffffffff-ffff-4fff-bfff-ffffffffffff"
	e.exec(`INSERT INTO conversations(id,agent,session_id,device_id,user_id) VALUES($1,$2,'z-main',$3,$4)`, main, src.agent, src.deviceID, src.userID)
	e.exec(`INSERT INTO conversations(id,agent,session_id,device_id,user_id) VALUES($1,$2,'a-sub',$3,$4)`, sub, src.agent, src.deviceID, src.userID)

	recount, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer recount.Rollback(e.ctx)
	if _, err := recount.Exec(e.ctx, `SELECT 1 FROM conversations WHERE id=$1 FOR UPDATE`, sub); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- refuseSource(e.ctx, e.pool, src, "/refused.jsonl", []string{"z-main", "a-sub"}, "deny:/x", nil)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for e.count(`SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'`) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the refusal never waited on the recount")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, lockErr := recount.Exec(e.ctx, `SELECT 1 FROM conversations WHERE id=$1 FOR UPDATE`, main)
	if lockErr == nil {
		lockErr = recount.Commit(e.ctx)
	}
	refuseErr := <-done
	for _, err := range []error{lockErr, refuseErr} {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "40P01" {
			t.Fatalf("refusal deadlocked with a recount: recount %v, refusal %v", lockErr, refuseErr)
		}
	}
	if lockErr != nil || refuseErr != nil {
		t.Fatalf("recount %v, refusal %v", lockErr, refuseErr)
	}
	if n := e.count(`SELECT count(*) FROM conversations WHERE id=$1 OR id=$2`, main, sub); n != 0 {
		t.Fatalf("%d refused conversations left", n)
	}
}
