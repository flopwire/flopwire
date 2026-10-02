package ingest

import (
	"errors"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/store"
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

func advisoryHeld(e *env) int {
	return e.count(`SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND database=(SELECT oid FROM pg_database WHERE datname=current_database())`)
}

// A flush holds its chunks' advisory locks on a pooled connection until
// the request ends, and an archive rewrite holds the shared purge lock and
// its replacement chunks' locks until Close. When unlocking fails and the
// connection cannot reach the server, both still return with every lock
// gone.
func TestFlushAndArchiveRewriteFreeLocksWhenUnlockFails(t *testing.T) {
	e := newEnv(t)
	defer store.SetReleaseFaults(store.ReleaseFaults{Unlock: true, UnlockAll: true, Abandon: true})()
	sync1(t, e.syncer(devicesync.Config{SealAfter: -1}), bulkSpec(t, 40))
	if n := advisoryHeld(e); n != 0 {
		t.Fatalf("a flush returned with %d advisory locks held", n)
	}
	plan, err := PrepareArchiveRewrite(e.ctx, e.pool, e.objects, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := advisoryHeld(e); n != 1 {
		t.Fatalf("%d advisory locks held by a prepared rewrite, want the purge lock", n)
	}
	plan.Close()
	if n := advisoryHeld(e); n != 0 {
		t.Fatalf("an archive rewrite closed with %d advisory locks held", n)
	}
}

// Hiding a conversation hides its tree (its subagents, its copies on other
// devices) in one UPDATE, and restoring it unhides the tree in another; a
// recount or checkpoint of the same conversations locks them in session
// order, so both must take them in that order too. Here the subagent's
// session sorts first while its id and row position sort last.
func TestHideAndRestoreAndConcurrentRecountDoNotDeadlock(t *testing.T) {
	e := newEnv(t)
	main, sub := "00000000-0000-4000-8000-000000000001", "ffffffff-ffff-4fff-bfff-ffffffffffff"
	e.exec(`INSERT INTO conversations(id,agent,session_id,device_id,user_id) VALUES($1,'claude','z-main',$2,$3)`, main, e.deviceID, e.userID)
	e.exec(`INSERT INTO conversations(id,agent,session_id,device_id,user_id,parent_conversation_id) VALUES($1,'claude','a-sub',$2,$3,$4)`, sub, e.deviceID, e.userID, main)
	c := convRow{id: main, user: e.userID, device: e.deviceID}
	for _, step := range []struct {
		name string
		run  func() error
		want int
	}{
		{"hide", func() error {
			_, err := e.queue.hide(e.ctx, serverRules{}, c, pathpolicy.Decision{Mode: pathpolicy.Deny})
			return err
		}, 2},
		{"restore", func() error { return e.queue.restore(e.ctx, serverRules{}, c, "rules_changed") }, 0},
	} {
		recount, err := e.pool.Begin(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := recount.Exec(e.ctx, `SELECT 1 FROM conversations WHERE id=$1 FOR UPDATE`, sub); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- step.run() }()
		deadline := time.Now().Add(10 * time.Second)
		for e.count(`SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'`) == 0 {
			if time.Now().After(deadline) {
				t.Fatalf("the %s never waited on the recount", step.name)
			}
			time.Sleep(10 * time.Millisecond)
		}
		_, lockErr := recount.Exec(e.ctx, `SELECT 1 FROM conversations WHERE id=$1 FOR UPDATE`, main)
		if lockErr == nil {
			lockErr = recount.Commit(e.ctx)
		}
		_ = recount.Rollback(e.ctx)
		stepErr := <-done
		for _, err := range []error{lockErr, stepErr} {
			var pe *pgconn.PgError
			if errors.As(err, &pe) && pe.Code == "40P01" {
				t.Fatalf("%s deadlocked with a recount: recount %v, %s %v", step.name, lockErr, step.name, stepErr)
			}
		}
		if lockErr != nil || stepErr != nil {
			t.Fatalf("recount %v, %s %v", lockErr, step.name, stepErr)
		}
		if n := e.count(`SELECT count(*) FROM conversations WHERE hidden_at IS NOT NULL`); n != step.want {
			t.Fatalf("after %s: %d hidden conversations, want %d", step.name, n, step.want)
		}
	}
}
