package store

import (
	"context"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/perfguard"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Performance guards for conversation deletion (notes/perf-guards.md,
// known violation #3): the foreign keys that point at messages and sources
// and the source lookups the deletion runs must each be served by an index.

// perfFixture is a migrated perfguard database holding one user and device,
// the conversation to delete with size messages on its own source (every
// other message superseded by the next, as reparses leave them), and others
// further conversations of eight messages, each on its own source.
func perfFixture(t testing.TB, size, others int) (*pgxpool.Pool, *perfguard.Counter, *Postgres, string, string) {
	t.Helper()
	ctx := context.Background()
	pool, counter := perfguard.NewPool(t)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	user, device, conv, source := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now().UTC()
	run := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	run(`INSERT INTO users(id,email,name,role,identity_type,created_at) VALUES($1,'a@example.test','n','admin','human',$2)`, user, now)
	run(`INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$2,'mac','darwin',$3)`, device, user, now)
	run(`INSERT INTO sources(id,device_id,agent,path,file_id,storage_kind,parser,first_seen_at)
		SELECT md5('s:'||i)::uuid,$1,'codex','/s'||i||'.jsonl','1:'||i,'jsonl_append','codex@1',$2 FROM generate_series(1,$3) i`, device, now, others)
	run(`INSERT INTO sources(id,device_id,agent,path,file_id,storage_kind,parser,first_seen_at) VALUES($1,$2,'codex','/d.jsonl','2:1','jsonl_append','codex@1',$3)`, source, device, now)
	run(`INSERT INTO conversations(id,source_id,agent,session_id,device_id,user_id)
		SELECT md5('c:'||i)::uuid,md5('s:'||i)::uuid,'codex','other-'||i,$1,$2 FROM generate_series(1,$3) i`, device, user, others)
	run(`INSERT INTO conversations(id,source_id,agent,session_id,device_id,user_id) VALUES($1,$2,'codex','doomed',$3,$4)`, conv, source, device, user)
	// messages(conv, source, count): message i is superseded by i+1 when
	// i is even; the pair shares one native id.
	messages := `INSERT INTO messages(id,conversation_id,source_id,native_id,ordinal,kind,text,text_len,content_sha,superseded,superseded_by,source_generation,parser)
		SELECT md5(c.id||':'||i)::uuid,c.id,c.source_id,'n'||(i/2),i,'assistant','text '||i,length('text '||i),sha256(('text '||i)::bytea),
			i%2=0 AND i+1<$2,CASE WHEN i%2=0 AND i+1<$2 THEN md5(c.id||':'||(i+1))::uuid END,0,'codex@1'
		FROM conversations c, generate_series(0,$2-1) i WHERE `
	run(messages+`c.id=$1`, conv, size)
	run(messages+`c.id<>$1`, conv, 8)
	run(`ANALYZE`)
	return pool, counter, NewPostgres(pool, nil, ""), conv, user
}

// deleteCost measures requesting the deletion of the fixture's conversation.
func deleteCost(t testing.TB, size, others int) perfguard.Cost {
	pool, counter, p, conv, user := perfFixture(t, size, others)
	return perfguard.Measure(t, pool, counter, func() {
		if _, err := p.RequestConversationDeletion(context.Background(), conv, user, "", false); err != nil {
			t.Fatal(err)
		}
	})
}

// Deleting a conversation of n messages is linear in n. Without an index
// on messages.superseded_by, each deleted row's ON DELETE SET NULL check
// scans every message. Those checks run after the cascade, so the
// conversation's own rows are dead tuples to them: seq_tup_read does not
// count them, and the rows metric grows only linearly. The heap blocks of
// messages show the n² scan, so this test bounds them too.
func TestPerfDeleteConversationLinearInSize(t *testing.T) {
	const n, k = 500, 8
	costs := map[int]perfguard.Cost{}
	perfguard.AssertScaling(t, perfguard.Linear, n, k, func(t testing.TB, n int) perfguard.Cost {
		costs[n] = deleteCost(t, n, 4)
		return costs[n]
	})
	blocks := func(size int) int64 {
		return costs[size].Tables["public.messages"].HeapBlks - costs[1].Tables["public.messages"].HeapBlks
	}
	small, large := blocks(n), blocks(k*n)
	if bound := perfguard.Linear.Bound(k); float64(large) > bound*float64(max(small, 16)) {
		t.Errorf("messages heap blocks %d → %d less baseline (×%.1f), bound ×%.1f",
			small, large, float64(large)/float64(max(small, 16)), bound)
	}
}

// Deleting one conversation costs the same however many other
// conversations and sources the server holds.
func TestPerfDeleteConversationConstantInOthers(t *testing.T) {
	perfguard.AssertScaling(t, perfguard.Constant, 50, 8, func(t testing.TB, n int) perfguard.Cost {
		return deleteCost(t, 16, n)
	})
}

// The foreign-key actions on message and source deletion, as the
// referential-integrity triggers run them, and the deletion's source
// lookups, use an index.
func TestPerfDeletionPlansUseIndexes(t *testing.T) {
	pool, _, _, _, _ := perfFixture(t, 16, 16)
	id := uuid.NewString()
	ids := []string{id, uuid.NewString()}
	for _, c := range []struct {
		name, sql string
		args      []any
	}{
		{"messages.superseded_by ON DELETE SET NULL", `UPDATE ONLY messages SET superseded_by=NULL WHERE $1=superseded_by`, []any{id}},
		{"messages.source_id ON DELETE SET NULL", `UPDATE ONLY messages SET source_id=NULL WHERE $1=source_id`, []any{id}},
		{"conversations.source_id ON DELETE SET NULL", `UPDATE ONLY conversations SET source_id=NULL WHERE $1=source_id`, []any{id}},
		{"request: tombstone unreferenced sources", tombstoneSourcesSQL, []any{ids, time.Now()}},
		{"purge: orphan sources", orphanSourcesSQL, []any{ids}},
	} {
		t.Run(c.name, func(t *testing.T) {
			perfguard.AssertIndexedPlan(t, pool, c.sql, c.args...)
		})
	}
}
