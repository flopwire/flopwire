package ingest

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func isDeadlock(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "40P01"
}

// waitForLockWait waits until some backend of the test database waits on a
// lock.
func waitForLockWait(t *testing.T, e *env, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for e.count(`SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'`) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the %s never waited on the hide", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A subagent's digest refresh updates its parent's subagent count, and a
// hide or deletion of the parent's tree locks the parent and its
// subagents in store.LockConversationsSQL's order. Every writer that
// touches a subagent and then its parent (the parse flush, the digest
// recount, the checkpoint, the late supersede, the link resolution) must
// lock the parent in that order too, not after the subagent. Here the
// parent's session sorts first; the "hide" holds the parent and then asks
// for the subagent, as the hide of the parent's tree does.
func TestSubagentWritersLockParentInSessionOrder(t *testing.T) {
	for _, op := range []struct {
		name string
		run  func(e *env, src source, parent, sub string) error
	}{
		{"recount", func(e *env, _ source, _, sub string) error {
			return pgx.BeginFunc(e.ctx, e.pool, func(tx pgx.Tx) error { return recountDigests(e.ctx, tx, []string{sub}) })
		}},
		{"checkpoint", func(e *env, src source, _, _ string) error {
			return pgx.BeginFunc(e.ctx, e.pool, func(tx pgx.Tx) error {
				return checkpointDigests(e.ctx, tx, &job{src: source{id: uuid.NewString()}, previous: &src.id}, 2, true)
			})
		}},
		{"late supersede", func(e *env, src source, _, _ string) error {
			return pgx.BeginFunc(e.ctx, e.pool, func(tx pgx.Tx) error { return retireLateSource(e.ctx, tx, src.id, 2) })
		}},
		{"flush", func(e *env, src source, _, _ string) error {
			s := newSink(e.ctx, e.pool, src)
			s.convs["z-sub"] = &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "z-sub", ParentSessionID: "a-main"}
			m := &transcript.Message{SessionID: "z-sub", NativeID: "sublock-1", Kind: transcript.KindUser, Role: "user", Ordinal: 99, Parser: "claude@1", Text: "more"}
			m.FullLen = len(m.Text)
			s.msgs = append(s.msgs, m)
			return s.flush()
		}},
		{"flush (stored parent)", func(e *env, src source, _, _ string) error {
			s := newSink(e.ctx, e.pool, src)
			m := &transcript.Message{SessionID: "z-sub", NativeID: "sublock-2", Kind: transcript.KindUser, Role: "user", Ordinal: 98, Parser: "claude@1", Text: "more"}
			m.FullLen = len(m.Text)
			s.msgs = append(s.msgs, m)
			return s.flush()
		}},
		{"resolve links", func(e *env, src source, _, sub string) error {
			return resolveLinks(e.ctx, e.pool, src.deviceID, []string{sub})
		}},
	} {
		t.Run(op.name, func(t *testing.T) {
			e := newEnv(t)
			id := refreshedSource(t, e)
			var src source
			var sub string
			if err := e.pool.QueryRow(e.ctx, `SELECT s.id::text,s.device_id::text,d.user_id::text,s.agent,c.id::text
				FROM sources s JOIN devices d ON d.id=s.device_id JOIN conversations c ON c.source_id=s.id WHERE s.id=$1`, id).
				Scan(&src.id, &src.deviceID, &src.userID, &src.agent, &sub); err != nil {
				t.Fatal(err)
			}
			src.generation, src.parseAttempt = 1, 1000
			parent := "ffffffff-ffff-4fff-bfff-ffffffffffff"
			e.exec(`UPDATE conversations SET session_id='z-sub',parent_native_session_id='a-main' WHERE id=$1`, sub)
			e.exec(`INSERT INTO conversations(id,agent,session_id,device_id,user_id) VALUES($1,$2,'a-main',$3,$4)`, parent, src.agent, src.deviceID, src.userID)
			e.exec(`UPDATE conversation_activity SET digest='{}' WHERE digest IS NULL`)

			hide, err := e.pool.Begin(e.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer hide.Rollback(e.ctx)
			if _, err := hide.Exec(e.ctx, `SELECT 1 FROM conversations WHERE id=$1 FOR UPDATE`, parent); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- op.run(e, src, parent, sub) }()
			waitForLockWait(t, e, op.name)
			_, hideErr := hide.Exec(e.ctx, `SELECT 1 FROM conversations WHERE id=$1 FOR UPDATE`, sub)
			if hideErr == nil {
				hideErr = hide.Commit(e.ctx)
			}
			_ = hide.Rollback(e.ctx)
			opErr := <-done
			if isDeadlock(hideErr) || isDeadlock(opErr) {
				t.Fatalf("%s deadlocked with a hide of the parent's tree: hide %v, %s %v", op.name, hideErr, op.name, opErr)
			}
			if hideErr != nil || opErr != nil {
				t.Fatalf("hide %v, %s %v", hideErr, op.name, opErr)
			}
		})
	}
}

// Refusing a parent session deletes its conversation, and the delete
// clears its subagents' parent_conversation_id (ON DELETE SET NULL),
// locking them after the parent. A hide of the parent's tree locks them
// in session order. Here the subagent's session sorts first.
func TestRefuseParentAndConcurrentHideDoNotDeadlock(t *testing.T) {
	e := newEnv(t)
	parent, sub := "00000000-0000-4000-8000-000000000001", "ffffffff-ffff-4fff-bfff-ffffffffffff"
	e.exec(`INSERT INTO conversations(id,agent,session_id,device_id,user_id) VALUES($1,'claude','z-main',$2,$3)`, parent, e.deviceID, e.userID)
	e.exec(`INSERT INTO conversations(id,agent,session_id,device_id,user_id,parent_native_session_id,parent_conversation_id) VALUES($1,'claude','a-sub',$2,$3,'z-main',$4)`,
		sub, e.deviceID, e.userID, parent)
	hide, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hide.Rollback(e.ctx)
	if _, err := hide.Exec(e.ctx, `SELECT 1 FROM conversations WHERE id=$1 FOR UPDATE`, sub); err != nil {
		t.Fatal(err)
	}
	src := source{id: uuid.NewString(), deviceID: e.deviceID, userID: e.userID, agent: "claude"}
	done := make(chan error, 1)
	go func() { done <- refuseSource(e.ctx, e.pool, src, "/refused.jsonl", []string{"z-main"}, "deny:/x", nil) }()
	waitForLockWait(t, e, "refusal")
	_, hideErr := hide.Exec(e.ctx, `SELECT 1 FROM conversations WHERE id=$1 FOR UPDATE`, parent)
	if hideErr == nil {
		hideErr = hide.Commit(e.ctx)
	}
	_ = hide.Rollback(e.ctx)
	refuseErr := <-done
	if isDeadlock(hideErr) || isDeadlock(refuseErr) {
		t.Fatalf("refusal deadlocked with a hide: hide %v, refusal %v", hideErr, refuseErr)
	}
	if hideErr != nil || refuseErr != nil {
		t.Fatalf("hide %v, refusal %v", hideErr, refuseErr)
	}
	if n := e.count(`SELECT count(*) FROM conversations WHERE id=$1`, parent); n != 0 {
		t.Fatal("the refused parent is still stored")
	}
}

// A stress of the cycle above: hide and restore a 41-conversation tree
// (a parent and 40 subagents linked by native session id, the parent's
// session sorting in the middle) while 4 workers recount the digests of
// random subagents. No transaction may deadlock.
func TestHideRestoreAndSubagentRecountsStress(t *testing.T) {
	if testing.Short() {
		t.Skip("stress")
	}
	e := newEnv(t)
	parent := uuid.NewString()
	e.exec(`INSERT INTO conversations(id,agent,session_id,device_id,user_id) VALUES($1,'claude','m-root',$2,$3)`, parent, e.deviceID, e.userID)
	var subs []string
	for i := range 40 {
		id := uuid.NewString()
		subs = append(subs, id)
		e.exec(`INSERT INTO conversations(id,agent,session_id,device_id,user_id,parent_native_session_id) VALUES($1,'claude',$2,$3,$4,'m-root')`,
			id, fmt.Sprintf("%c-sub-%02d", 'a'+rune(i%26), i), e.deviceID, e.userID)
	}
	e.exec(`UPDATE conversation_activity SET digest='{}'`)
	var deadlocks, txs atomic.Int64
	count := func(err error) error {
		txs.Add(1)
		if isDeadlock(err) {
			deadlocks.Add(1)
			return nil
		}
		return err
	}
	ctx, stop := context.WithCancel(e.ctx)
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for w := range 4 {
		wg.Go(func() {
			r := rand.New(rand.NewPCG(uint64(w), 7))
			for ctx.Err() == nil {
				pick := slices.Clone(subs)
				r.Shuffle(len(pick), func(i, j int) { pick[i], pick[j] = pick[j], pick[i] })
				pick = pick[:1+r.IntN(8)]
				err := pgx.BeginFunc(e.ctx, e.pool, func(tx pgx.Tx) error { return recountDigests(e.ctx, tx, pick) })
				if err = count(err); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	c := convRow{id: parent, user: e.userID, device: e.deviceID}
	for range 40 {
		_, err := e.queue.hide(e.ctx, serverRules{}, c, pathpolicy.Decision{Mode: pathpolicy.Deny})
		if err = count(err); err != nil {
			errs <- err
			break
		}
		if err = count(e.queue.restore(e.ctx, serverRules{}, c, "rules_changed")); err != nil {
			errs <- err
			break
		}
	}
	stop()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if n := deadlocks.Load(); n > 0 {
		t.Fatalf("%d deadlocks in %d transactions", n, txs.Load())
	}
	t.Logf("%d transactions, no deadlock", txs.Load())
}

// A subagent's first flush sets its parent session while holding its
// row: a lock taken meanwhile sees the subagent without a parent and,
// once the flush commits, locks it alone. lockWithParents finds the
// parent missing and takes the locks again, the parent included.
func TestLockWithParentsSeesParentSetWhileWaiting(t *testing.T) {
	e := newEnv(t)
	parent, sub := uuid.NewString(), uuid.NewString()
	e.exec(`INSERT INTO conversations(id,agent,session_id,device_id,user_id) VALUES($1,'claude','a-main',$2,$3)`, parent, e.deviceID, e.userID)
	e.exec(`INSERT INTO conversations(id,agent,session_id,device_id,user_id) VALUES($1,'claude','z-sub',$2,$3)`, sub, e.deviceID, e.userID)
	flush, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer flush.Rollback(e.ctx)
	if _, err := flush.Exec(e.ctx, `UPDATE conversations SET parent_native_session_id='a-main' WHERE id=$1`, sub); err != nil {
		t.Fatal(err)
	}
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(e.ctx)
	type result struct {
		locked []string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		locked, err := lockWithParents(e.ctx, tx, []string{sub})
		done <- result{locked, err}
	}()
	waitForLockWait(t, e, "lock")
	if err := flush.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if want := []string{parent, sub}; !slices.Equal(r.locked, want) {
		t.Fatalf("locked %v, want %v", r.locked, want)
	}
	_, err = e.pool.Exec(e.ctx, `SELECT 1 FROM conversations WHERE id=$1 FOR UPDATE NOWAIT`, parent)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "55P03" {
		t.Fatalf("the parent is not locked: %v", err)
	}
}
