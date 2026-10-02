package ingest

import (
	"context"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/perfguard"
	"github.com/flopwire/flopwire/internal/redact"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// rulesFixture is a database under old redaction rules: one source of n
// messages over n/8 conversations, then n/50 sources of 50 messages each.
// It returns the sources, the large one first. Half of every source's
// messages are superseded. Half of its conversations name the source; the
// rest reach it only through their messages.
func rulesFixture(t testing.TB, n int) (*pgxpool.Pool, *perfguard.Counter, []string) {
	t.Helper()
	ctx := context.Background()
	pool, counter := perfguard.NewPool(t)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	user, device := uuid.NewString(), uuid.NewString()
	exec(`INSERT INTO users(id,email,name,role,identity_type,created_at) VALUES($1::uuid,$1::text||'@example.test','Perf','member','human',now())`, user)
	exec(`INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$2,'mac','darwin',now())`, device, user)
	secret := "ghp_" + strings.Repeat("aB9x", 9)
	sizes := []int{n}
	for range n / 50 {
		sizes = append(sizes, 50)
	}
	var sources []string
	for _, size := range sizes {
		src := uuid.NewString()
		sources = append(sources, src)
		convs := max(1, size/8)
		exec(`INSERT INTO sources(id,device_id,agent,path,file_id,storage_kind,parser,first_seen_at)
 VALUES($1::uuid,$2,'claude','/perf/'||$1::text,'f','jsonl_append','claude',now())`, src, device)
		// Conversation ids are random, so the two kinds interleave in id order.
		exec(`INSERT INTO conversations(id,source_id,agent,session_id,device_id,user_id,title)
 SELECT gen_random_uuid(), CASE WHEN g%2=0 THEN $1::uuid END, 'claude', $1::text||'-'||g, $2, $3, 'title '||g||' '||$5::text
 FROM generate_series(1,$4) g`, src, device, user, convs, secret)
		exec(`UPDATE conversation_activity a SET digest=jsonb_build_object('intent', 'step '||split_part(c.session_id,'-',6)||' '||$2::text)
 FROM conversations c WHERE c.id=a.conversation_id AND c.session_id LIKE $1::text||'-%'`, src, secret)
		exec(`WITH c AS (SELECT id, row_number() OVER (ORDER BY session_id) - 1 AS k FROM conversations WHERE session_id LIKE $1::text||'-%')
 INSERT INTO messages(id,conversation_id,source_id,ordinal,kind,text,text_len,content_sha,superseded,source_generation,parser,enrichment,redaction_rules)
 SELECT gen_random_uuid(), c.id, $1::uuid, g, 'user', 'message '||g||' '||$4::text, 60, sha256(convert_to('message '||g, 'UTF8')),
        g%2=0, 0, 'claude@3', jsonb_build_object('n', g), 'old-rules'
 FROM generate_series(0,$2-1) g JOIN c ON c.k = g % $3`, src, size, convs, secret)
	}
	exec(`ANALYZE`)
	return pool, counter, sources
}

// Every query of the rules upgrade must be served by an index: the
// historical versions it rewrites include superseded rows, and
// conversations reach the source either directly or through their
// messages.
func TestRuleUpgradeBatchPlans(t *testing.T) {
	pool, _, sources := rulesFixture(t, 200)
	source := sources[0]
	ctx := context.Background()
	list := func(query string, want int) []string {
		t.Helper()
		args := []any{source}
		if query == staleVersions {
			args = append(args, redact.RulesVersion, staleVersionsPage)
		}
		perfguard.AssertIndexedPlan(t, pool, query, args...)
		rows, err := pool.Query(ctx, query, args...)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil || len(ids) != want {
			t.Fatalf("%s: %d ids, err %v; want %d", query, len(ids), err, want)
		}
		return ids[:min(len(ids), 64)]
	}
	perfguard.AssertIndexedPlan(t, pool, staleVersionsBatch, list(staleVersions, 200), redact.RulesVersion)
	perfguard.AssertIndexedPlan(t, pool, sourceConversationsBatch, list(sourceConversations, 25))
}

// A full rules upgrade, every source in turn, is linear in the rows and
// conversations stored: not one scan of messages or conversations per
// 64-row batch, nor per source.
func TestRuleUpgradeScalesLinearly(t *testing.T) {
	perfguard.AssertScaling(t, perfguard.Linear, 500, 8, func(t testing.TB, n int) perfguard.Cost {
		pool, counter, sources := rulesFixture(t, n)
		q := &Queue{Pool: pool}
		cost := perfguard.Measure(t, pool, counter, func() {
			for _, source := range sources {
				if err := q.maskStoredVersions(context.Background(), source); err != nil {
					t.Fatal(err)
				}
			}
		})
		var stale, secrets int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE redaction_rules IS DISTINCT FROM $1),
 count(*) FILTER (WHERE strpos(text,'ghp_')>0) FROM messages`, redact.RulesVersion).Scan(&stale, &secrets); err != nil || stale != 0 || secrets != 0 {
			t.Fatalf("after upgrade: %d stale rows, %d with the secret, err %v", stale, secrets, err)
		}
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM conversations c JOIN conversation_activity a ON a.conversation_id=c.id WHERE strpos(c.title,'ghp_')>0 OR strpos(a.digest::text,'ghp_')>0`).Scan(&secrets); err != nil || secrets != 0 {
			t.Fatalf("after upgrade: %d conversations keep the secret, err %v", secrets, err)
		}
		return cost
	})
}

// A source with more stale rows than one listing page is masked in full,
// through a pool of one connection: each page is read to the end before
// its batches run. 250 rows over pages of 100 lists 100, 100, 50; 200 rows
// ends on an empty page.
func TestRuleUpgradePagesStaleVersions(t *testing.T) {
	defer func(page int) { staleVersionsPage = page }(staleVersionsPage)
	staleVersionsPage = 100
	for _, n := range []int{250, 200} {
		pool, _, sources := rulesFixture(t, n)
		cfg := pool.Config()
		cfg.MaxConns = 1
		one, err := pgxpool.NewWithConfig(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := (&Queue{Pool: one}).maskStoredVersions(context.Background(), sources[0]); err != nil {
			t.Fatal(err)
		}
		var stale, masked int
		if err := one.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE redaction_rules IS DISTINCT FROM $2),
 count(*) FILTER (WHERE redaction_rules = $2 AND strpos(text,'ghp_')=0) FROM messages WHERE source_id=$1`, sources[0], redact.RulesVersion).Scan(&stale, &masked); err != nil {
			t.Fatal(err)
		}
		one.Close()
		if stale != 0 || masked != n {
			t.Fatalf("n=%d: %d stale, %d masked", n, stale, masked)
		}
	}
}
