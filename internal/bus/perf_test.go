package bus

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/perfguard"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Performance guards (notes/perf-guards.md): every bus lookup is served by
// an index, and a device's poll costs the same however many messages and
// sessions the rest of the organization has.

// perfFixture holds one person (me) with a device and a live session,
// and others people, each with a device, a live session, an uploaded
// session and eight messages in every state.
func perfFixture(t testing.TB, others int) (*pgxpool.Pool, *perfguard.Counter, busproto.Caller, time.Time) {
	t.Helper()
	ctx := context.Background()
	pool, counter := perfguard.NewPool(t)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	run := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	me := busproto.Caller{UserID: uuid.NewString(), DeviceID: uuid.NewString()}
	run(`INSERT INTO users(id,email,name,role,identity_type,created_at) VALUES($1,'me@example.test','Me','member','human',$2)`, me.UserID, now)
	run(`INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$2,'mac','darwin',$3)`, me.DeviceID, me.UserID, now)
	run(`INSERT INTO bus_presence(device_id,user_id,agent,session_id,repo,busy,seen_at) VALUES($1,$2,'claude','me-session','/x/api',true,$3)`, me.DeviceID, me.UserID, now)
	run(`INSERT INTO users(id,email,name,role,identity_type,created_at)
		SELECT md5('u'||i)::uuid,'u'||i||'@example.test','U'||i,'member','human',$1 FROM generate_series(1,$2) i`, now, others)
	run(`INSERT INTO devices(id,user_id,name,platform,created_at) SELECT md5('d'||i)::uuid,md5('u'||i)::uuid,'d','linux',$1 FROM generate_series(1,$2) i`, now, others)
	run(`INSERT INTO bus_presence(device_id,user_id,agent,session_id,repo,busy,seen_at)
		SELECT md5('d'||i)::uuid,md5('u'||i)::uuid,'codex','live-'||lpad(i::text,6,'0'),'/y/api',false,$1 FROM generate_series(1,$2) i`, now, others)
	run(`INSERT INTO conversations(id,agent,session_id,device_id,user_id)
		SELECT md5('c'||i)::uuid,'codex','up-'||lpad(i::text,6,'0'),md5('d'||i)::uuid,md5('u'||i)::uuid FROM generate_series(1,$1) i`, others)
	run(`INSERT INTO bus_messages(id,thread_id,from_user,from_device,from_agent,from_session,to_user,to_agent,to_session,addressed,sender,intent,body,body_sha,state,created_at,expires_at)
		SELECT 'm'||i||'-'||k,'m'||i||'-0',md5('u'||i)::uuid,md5('d'||i)::uuid,'codex','up-'||lpad(i::text,6,'0'),md5('u'||i)::uuid,'codex','live-'||lpad(i::text,6,'0'),'session','own','inform','x',sha256('x'::bytea),
			(ARRAY['queued','held','claimed','delivered','read','expired','refused','queued'])[k+1],$1,$2
		FROM generate_series(1,$3) i, generate_series(0,7) k`, now, now.Add(time.Hour), others)
	// A few messages for me: one queued to my session and one @user.
	run(`INSERT INTO bus_messages(id,thread_id,from_user,from_agent,from_session,to_user,to_agent,to_session,addressed,sender,intent,body,body_sha,state,created_at,expires_at)
		VALUES('mine-1','mine-1',md5('u1')::uuid,'codex','up-000001',$1,'claude','me-session','session','teammate','inform','x',sha256('x'::bytea),'queued',$2,$3)`, me.UserID, now, now.Add(time.Hour))
	run(`INSERT INTO bus_messages(id,thread_id,from_user,from_agent,from_session,to_user,addressed,sender,intent,body,body_sha,state,created_at,expires_at)
		VALUES('mine-2','mine-2',md5('u1')::uuid,'codex','up-000001',$1,'user','teammate','inform','x',sha256('x'::bytea),'held',$2,$3)`, me.UserID, now, now.Add(time.Hour))
	run(`INSERT INTO bus_messages(id,thread_id,from_user,from_agent,from_session,to_user,to_agent,to_session,addressed,sender,intent,body,body_sha,state,created_at,expires_at,delivered_at)
		VALUES('mine-3','mine-3',md5('u1')::uuid,'codex','up-000001',$1,'claude','me-session','session','teammate','inform','x',sha256('x'::bytea),'delivered',$2,$3,$2)`, me.UserID, now, now.Add(time.Hour))
	run(`ANALYZE`)
	return pool, counter, me, now
}

func TestPerfBusPlansUseIndexes(t *testing.T) {
	pool, _, me, now := perfFixture(t, 16)
	live := now.Add(-busproto.PresenceTTL)
	sha := make([]byte, 32)
	ids := []string{"m1-0", "m2-0"}
	for _, c := range []struct {
		name, sql string
		args      []any
	}{
		{"session on device", SessionOnDeviceSQL, []any{me.DeviceID, "me-session", "", live}},
		{"session prefix", SessionPrefixSQL, []any{"live-000001%", live}},
		{"user live", UserLiveSQL, []any{me.UserID, live}},
		{"deliverable", DeliverableSQL, []any{me.UserID, now, me.DeviceID}},
		{"held", HeldSQL, []any{me.UserID, now}},
		{"held list", HeldListSQL, []any{me.UserID, now}},
		{"reply to", ReplyToSQL, []any{"m1-0"}},
		{"duplicate", DuplicateSQL, []any{"me-session", sha, me.UserID, "session", "live-000001", now}},
		{"session sends", SessionSendsSQL, []any{"me-session", now}},
		{"thread sends", ThreadSendsSQL, []any{"m1-0", now}},
		{"session pending", SessionPendingSQL, []any{"live-000001", now, me.UserID}},
		{"user pending", UserPendingSQL, []any{me.UserID, now, me.UserID}},
		{"foreign sessions", ForeignSessionsSQL, []any{[]string{"up-000001", "me-session"}, me.UserID}},
		{"clear presence", clearPresenceSQL, []any{me.DeviceID, []string{"claude"}, []string{"me-session"}}},
		{"ack", AckSQL, []any{ids, me.UserID, me.DeviceID, now}},
		{"acked before", ackedBeforeSQL, []any{ids, me.UserID, []string{}}},
		{"undelivered", UndeliveredSQL, []any{ids, me.UserID, me.DeviceID, busproto.ReasonUnconfirmed}},
		{"session ended", EndedSQL, []any{ids, me.UserID, me.DeviceID, busproto.ReasonSessionEnded}},
		{"undelivered before", undeliveredBeforeSQL, []any{ids, me.UserID, []string{}}},
		{"read", ReadSQL, []any{ids, me.UserID, me.DeviceID, now, []string{"me-session", "live-000002"}, []string{"claude", "codex"}, []time.Time{now, now}}},
		{"read before", readBeforeSQL, []any{ids, me.UserID, me.DeviceID, []string{"me-session", "live-000002"}, []string{"claude", "codex"}, []string{}}},
		{"inbox", InboxSQL, []any{"me-session", me.UserID, false, "", nil, "", 51}},
		{"inbox page", InboxSQL, []any{"me-session", me.UserID, false, "m1-0", now, "m1-0", 51}},
		{"peers", PeersSQL, []any{live}},
		{"release held", releaseHeldSQL, []any{me.UserID, me.UserID, now}},
		{"rehold", reholdSQL, []any{me.UserID, me.UserID}},
		{"expire", expireSQL, []any{now}},
		{"drop presence", dropPresenceSQL, []any{now}},
	} {
		t.Run(c.name, func(t *testing.T) {
			perfguard.AssertIndexedPlan(t, pool, c.sql, c.args...)
		})
	}
	assertSendCeilingIndexes(t, pool, me, now)
	// The users table is a handful of rows; its lookup is not indexed.
	perfguard.AssertIndexedPlanExcept(t, pool, []string{"users"}, UserLookupSQL, "alex")
}

// assertSendCeilingIndexes checks that the device and person ceilings
// probe their own indexes. Either count can otherwise be served by an
// Index Cond on created_at alone in another (x, created_at) index, which
// reads that whole index.
func assertSendCeilingIndexes(t testing.TB, pool *pgxpool.Pool, me busproto.Caller, now time.Time) {
	t.Helper()
	perfguard.AssertPlanUsesIndex(t, pool, "bus_messages_from_device_idx", DeviceSendsSQL, me.DeviceID, now)
	perfguard.AssertPlanUsesIndex(t, pool, "bus_messages_from_user_idx", UserSendsSQL, me.UserID, now)
}

// failures records a guard's failures instead of failing the test.
type failures struct {
	testing.TB
	mu  sync.Mutex
	got []string
}

func (f *failures) Helper() {}

func (f *failures) Errorf(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, fmt.Sprintf(format, args...))
}

// The ceiling guards fail when their indexes are gone (#70: they passed
// with both deleted, on a full scan of bus_messages_from_session_idx).
func TestPerfSendCeilingGuardsCatchAMissingIndex(t *testing.T) {
	pool, _, me, now := perfFixture(t, 16)
	for _, idx := range []string{"bus_messages_from_device_idx", "bus_messages_from_user_idx"} {
		if _, err := pool.Exec(context.Background(), `DROP INDEX `+idx); err != nil {
			t.Fatal(err)
		}
	}
	f := &failures{TB: t}
	assertSendCeilingIndexes(f, pool, me, now)
	all := strings.Join(f.got, "\n")
	for _, idx := range []string{"bus_messages_from_device_idx", "bus_messages_from_user_idx"} {
		if !strings.Contains(all, idx) {
			t.Errorf("no failure names %s: %q", idx, all)
		}
	}
}

// A device's poll reads its own person's messages and presence only.
func TestPerfPollConstantInOrganization(t *testing.T) {
	perfguard.AssertScaling(t, perfguard.Constant, 100, 8, func(t testing.TB, n int) perfguard.Cost {
		pool, counter, me, now := perfFixture(t, n)
		s := &Store{Pool: pool, Now: func() time.Time { return now }}
		return perfguard.Measure(t, pool, counter, func() {
			out, err := s.Poll(context.Background(), me, busproto.PollRequest{Sessions: []busproto.PresenceSession{{SessionID: "me-session", Agent: "claude", Repo: "/x/api", Busy: true}}})
			if err != nil || len(out.Messages) != 1 || len(out.Held) != 1 {
				t.Fatalf("poll: %+v %v", out, err)
			}
		})
	})
}

// A send's lookups and limit checks do not grow with the organization.
func TestPerfSendConstantInOrganization(t *testing.T) {
	perfguard.AssertScaling(t, perfguard.Constant, 100, 8, func(t testing.TB, n int) perfguard.Cost {
		pool, counter, me, now := perfFixture(t, n)
		s := &Store{Pool: pool, Now: func() time.Time { return now }}
		return perfguard.Measure(t, pool, counter, func() {
			if _, err := s.Send(context.Background(), me, busproto.SendRequest{FromSession: "me-session", To: "live-000001", Body: "hello"}); err != nil {
				t.Fatal(err)
			}
		})
	})
}

// A read receipt looks its message up by id: its cost does not grow with
// the organization's messages.
func TestPerfReadReceiptConstantInOrganization(t *testing.T) {
	perfguard.AssertScaling(t, perfguard.Constant, 100, 8, func(t testing.TB, n int) perfguard.Cost {
		pool, counter, me, now := perfFixture(t, n)
		s := &Store{Pool: pool, Now: func() time.Time { return now }}
		return perfguard.Measure(t, pool, counter, func() {
			out, err := s.Ack(context.Background(), me, busproto.AckRequest{Read: []busproto.ReadReceipt{{ID: "mine-3", Session: "me-session", Agent: "claude", At: now}}})
			if err != nil || len(out.Read) != 1 {
				t.Fatalf("read: %+v %v", out, err)
			}
		})
	})
}
