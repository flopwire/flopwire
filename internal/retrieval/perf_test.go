package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/perfguard"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/grep"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// perfCorpus is a fresh migrated database on a perfguard pool with ten
// users on forty devices, sessions conversations (each with its source) of msgs messages each (kinds
// cycling user, assistant, tool_call, tool_result), last activity in
// groups of three equal times (ties for the keyset) a minute and some
// microseconds apart, every seventh undated. Rows are written in bulk SQL
// so thousands of sessions take well under a second.
func perfCorpus(t testing.TB, sessions, msgs int) (*Store, *perfguard.Counter) {
	t.Helper()
	pool, counter := perfguard.NewPool(t)
	ctx := context.Background()
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id,email,name,role,identity_type,created_at) SELECT md5('u'||i)::uuid,'perf'||i||'@example.test','Perf','member','human',now()
		 FROM generate_series(0,9) i`, nil},
		{`INSERT INTO devices(id,user_id,name,platform,created_at) SELECT md5('d'||i)::uuid,md5('u'||(i%10))::uuid,'laptop'||i,'darwin',now()
		 FROM generate_series(0,39) i`, nil},
		{`INSERT INTO sources(id,device_id,agent,path,file_id,storage_kind,parser,first_seen_at)
		 SELECT md5('f'||i)::uuid,md5('d'||(i%40))::uuid,'claude','/p/'||i||'.jsonl','f'||i,'jsonl_append','test',now()
		 FROM generate_series(1,$1) i`, []any{sessions}},
		{`INSERT INTO conversations(id,source_id,agent,session_id,device_id,user_id,repo_root,title,started_at)
		 SELECT md5('c'||i)::uuid,md5('f'||i)::uuid,'claude',md5('s'||i),md5('d'||(i%40))::uuid,md5('u'||(i%40%10))::uuid,
		        '/src/repo'||(i%5),'session '||i,'2026-09-01'::timestamptz+i*interval '1 minute'
		 FROM generate_series(1,$1) i`, []any{sessions}},
		{`UPDATE conversation_activity a SET last_activity_at=x.t FROM (SELECT md5('c'||i)::uuid id,
		        CASE WHEN i%7=0 THEN NULL ELSE '2026-09-01'::timestamptz+(i/3)*interval '1 minute'+(i/3%5)*interval '7 microseconds' END t
		 FROM generate_series(1,$1) i) x WHERE a.conversation_id=x.id`, []any{sessions}},
		{`INSERT INTO messages(id,conversation_id,source_id,ordinal,kind,ts,text,text_len,content_sha,source_generation,parser)
		 SELECT md5('m'||i||'/'||j)::uuid,md5('c'||i)::uuid,md5('f'||i)::uuid,j,(ARRAY['user','assistant','tool_call','tool_result'])[1+j%4],
		        '2026-09-01'::timestamptz+i*interval '1 minute'+j*interval '1 millisecond','step '||j||' of session '||i,20,
		        sha256(convert_to(i||'/'||j,'UTF8')),0,'test'
		 FROM generate_series(1,$1) i, generate_series(0,$2-1) j`, []any{sessions, msgs}},
		// Digests carry the per-kind counts ingest maintains; the read
		// header's message count comes from them.
		{`UPDATE conversation_activity a SET digest=(SELECT jsonb_build_object('messages',jsonb_object_agg(kind,k))
		 FROM (SELECT kind,count(*) k FROM messages m WHERE m.conversation_id=a.conversation_id GROUP BY kind) x)`, nil},
		// The activity rows were written twice after their insert: the
		// dead versions go, as autovacuum would remove them.
		{`VACUUM ANALYZE`, nil},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	return &Store{Pool: pool}, counter
}

// sessionAt is the conversation id of the i-th session (1-based) of a
// perfCorpus.
func sessionAt(t testing.TB, pool *pgxpool.Pool, i int) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `SELECT md5('c'||$1::int)::uuid::text`, i).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// sessionsCursor is the cursor after the i-th session (0-based) of the
// sessions list in its default order.
func sessionsCursor(t testing.TB, pool *pgxpool.Pool, i int) string {
	t.Helper()
	var c format.ConversationInfo
	if err := pool.QueryRow(context.Background(), `SELECT c.id::text,a.last_activity_at FROM conversations c JOIN conversation_activity a ON a.conversation_id=c.id
		ORDER BY a.last_activity_at DESC NULLS LAST,c.id DESC OFFSET $1 LIMIT 1`, i).Scan(&c.ID, &c.LastActivityAt); err != nil {
		t.Fatal(err)
	}
	return format.SessionCursor(c)
}

// outlineCursor is the cursor after the i-th outline entry (0-based) of
// conversation conv.
func outlineCursor(t testing.TB, pool *pgxpool.Pool, conv string, i int) string {
	t.Helper()
	var e format.OutlineEntry
	if err := pool.QueryRow(context.Background(), `SELECT ordinal,id::text FROM messages WHERE conversation_id=$1 AND kind IN ('user','tool_call')
		ORDER BY ordinal,id OFFSET $2 LIMIT 1`, conv, i).Scan(&e.Ordinal, &e.ID); err != nil {
		t.Fatal(err)
	}
	return format.OutlineCursor(e)
}

// One page of sessions costs the same whatever the corpus size: the first
// page, one deep among the dated sessions, and one among the undated ones
// at the end.
func TestSessionsPageScalingConstant(t *testing.T) {
	for _, at := range []string{"first", "deep", "undated"} {
		t.Run(at, func(t *testing.T) {
			perfguard.AssertScaling(t, perfguard.Constant, 500, 8, func(t testing.TB, n int) perfguard.Cost {
				s, counter := perfCorpus(t, n, 4)
				cursor := ""
				switch {
				case n < 20: // the baseline: one page of everything
				case at == "deep":
					cursor = sessionsCursor(t, s.Pool, n/2)
				case at == "undated":
					cursor = sessionsCursor(t, s.Pool, n-n/14)
				}
				return perfguard.Measure(t, s.Pool, counter, func() {
					out, err := s.Sessions(context.Background(), "", cursor, format.Filters{Limit: 20})
					if err != nil {
						t.Fatal(err)
					}
					if len(out.Sessions) == 0 {
						t.Fatalf("empty page at n=%d", n)
					}
				})
			})
		})
	}
}

// One outline page costs the same whatever the session's size, deep in
// the session as at its start. The read header is guarded by
// TestReadPageScalingConstant.
func TestOutlinePageScalingConstant(t *testing.T) {
	for _, deep := range []bool{false, true} {
		t.Run(map[bool]string{false: "first", true: "deep"}[deep], func(t *testing.T) {
			perfguard.AssertScaling(t, perfguard.Constant, 500, 8, func(t testing.TB, n int) perfguard.Cost {
				s, counter := perfCorpus(t, 3, n)
				conv := sessionAt(t, s.Pool, 2)
				rq := format.ReadQuery{Outline: true, Limit: 20}
				if deep && n > 40 {
					rq.Cursor = outlineCursor(t, s.Pool, conv, n/4)
				}
				return perfguard.Measure(t, s.Pool, counter, func() {
					page, _, err := s.outlinePage(context.Background(), conv, "", rq)
					if err != nil {
						t.Fatal(err)
					}
					if len(page) == 0 {
						t.Fatalf("empty outline page at n=%d", n)
					}
				})
			})
		})
	}
}

// The page queries walk an index in sort order, both ways, from the
// start and from a cursor among dated and among undated sessions. Users
// and devices join to the page's rows only; with a few of them the planner
// hashes the whole table, a per-team size that the corpus does not grow.
func TestSessionsAndOutlinePlansIndexed(t *testing.T) {
	s, _ := perfCorpus(t, 200, 8)
	dated, err := format.ParseSessionCursor(sessionsCursor(t, s.Pool, 50))
	if err != nil {
		t.Fatal(err)
	}
	undated, err := format.ParseSessionCursor(sessionsCursor(t, s.Pool, 195))
	if err != nil || !undated.Undated {
		t.Fatalf("undated cursor: %+v %v", undated, err)
	}
	for _, oldest := range []bool{false, true} {
		for _, after := range []*format.SessionKey{nil, &dated, &undated} {
			sql, args := sessionsPage("", format.Filters{}, oldest, after, 21)
			perfguard.AssertIndexedPlanExcept(t, s.Pool, []string{"users", "devices"}, sql, args...)
		}
	}
	conv := sessionAt(t, s.Pool, 2)
	for _, after := range []*outlineKey{nil, {ordinal: 3, id: "00000000-0000-4000-8000-000000000000"}} {
		sql, args := outlinePageQuery(conv, after, 21)
		perfguard.AssertIndexedPlan(t, s.Pool, sql, args...)
	}
}

// Walking the sessions list by cursor returns every session once, in
// order, both ways: ties on last activity, microsecond times, undated
// sessions last, filters applied on every page.
func TestSessionsCursorWalk(t *testing.T) {
	s, _ := perfCorpus(t, 103, 1)
	ctx := context.Background()
	want := func(order string, where string) []string {
		rows, err := s.Pool.Query(ctx, `SELECT id::text FROM conversations c JOIN conversation_activity a ON a.conversation_id=c.id WHERE `+where+` ORDER BY `+order)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		return ids
	}
	for _, tc := range []struct {
		sort, order, where string
		f                  format.Filters
	}{
		{format.SortNewest, "last_activity_at DESC NULLS LAST,id DESC", "true", format.Filters{}},
		{format.SortOldest, "last_activity_at NULLS LAST,id", "true", format.Filters{}},
		{format.SortNewest, "last_activity_at DESC NULLS LAST,id DESC", "repo_root='/src/repo3'", format.Filters{Repo: "/src/repo3"}},
	} {
		for _, limit := range []int{1, 7, 500} {
			t.Run(fmt.Sprintf("%s/%s/%d", tc.sort, tc.where, limit), func(t *testing.T) {
				f := tc.f
				f.Sort, f.Limit = tc.sort, limit
				var got []string
				cursor := ""
				for range 200 {
					out, err := s.Sessions(ctx, "", cursor, f)
					if err != nil {
						t.Fatal(err)
					}
					if len(out.Sessions) > limit || out.HasMore != (out.Next != "") || out.HasMore && len(out.Sessions) < limit {
						t.Fatalf("page of %d (limit %d) more=%v next=%q", len(out.Sessions), limit, out.HasMore, out.Next)
					}
					for _, c := range out.Sessions {
						got = append(got, c.ID)
					}
					if !out.HasMore {
						break
					}
					cursor = out.Next
				}
				if w := want(tc.order, tc.where); !slices.Equal(got, w) {
					t.Fatalf("walk returned %d sessions, want %d in order\ngot  %v\nwant %v", len(got), len(w), got, w)
				}
			})
		}
	}
	for _, bad := range []string{"x", "12.not-a-uuid", "abc.00000000-0000-4000-8000-000000000000"} {
		if _, err := s.Sessions(ctx, "", bad, format.Filters{}); !errors.Is(err, ErrBadRequest) {
			t.Errorf("cursor %q: %v, want a bad request", bad, err)
		}
	}
}

// Walking an outline by cursor returns every entry once, in order, with
// ties on the ordinal (parts of one record) broken by id.
func TestOutlineCursorWalk(t *testing.T) {
	s, _ := perfCorpus(t, 2, 41)
	ctx := context.Background()
	conv := sessionAt(t, s.Pool, 1)
	// Second parts of some records: the same ordinal, another id.
	if _, err := s.Pool.Exec(ctx, `INSERT INTO messages(id,conversation_id,ordinal,part,kind,ts,text,text_len,content_sha,source_generation,parser)
		SELECT md5('p'||j)::uuid,$1,j,1,'tool_call',now(),'part two of '||j,14,sha256(convert_to('p'||j,'UTF8')),0,'test'
		FROM generate_series(0,40,3) j`, conv); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Pool.Query(ctx, `SELECT id::text FROM messages WHERE conversation_id=$1 AND kind IN ('user','tool_call') ORDER BY ordinal,id`, conv)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		want = append(want, id)
	}
	for _, limit := range []int{1, 4, 2000} {
		var got []string
		rq := format.ReadQuery{Outline: true, Limit: limit}
		for range 100 {
			cx, err := s.outline(ctx, conv, rq)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range cx.Outline {
				got = append(got, e.ID)
			}
			if !cx.OutlineMore {
				break
			}
			rq.Cursor = cx.OutlineNext
		}
		if !slices.Equal(got, want) {
			t.Fatalf("limit %d: walk returned %v\nwant %v", limit, got, want)
		}
	}
	if _, err := s.outline(ctx, conv, format.ReadQuery{Outline: true, Cursor: "3.nope"}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("bad cursor: %v", err)
	}
}

// oldMatches marks the messages of the five oldest sessions of a
// perfCorpus with a rare word.
func oldMatches(t testing.TB, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE messages SET text=text||' zebracorn'
		WHERE conversation_id IN (SELECT md5('c'||i)::uuid FROM generate_series(1,5) i)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `VACUUM ANALYZE messages`); err != nil {
		t.Fatal(err)
	}
}

// Grep reads its trigram candidates, not the messages table: a pattern
// whose few matches are all old reads the same messages however many
// newer ones there are. A messages(ts) index would break this (see
// grepCandidates). Only messages are gated: joining the candidates to
// conversations is the planner's choice of hashing them all or looking
// each up, which flips with their count.
func TestGrepOldMatchesScalingConstant(t *testing.T) {
	perfguard.AssertScaling(t, perfguard.Constant, 500, 8, func(t testing.TB, n int) perfguard.Cost {
		s, counter := perfCorpus(t, max(n, 5), 4)
		oldMatches(t, s.Pool)
		cost := perfguard.Measure(t, s.Pool, counter, func() {
			page, err := s.Grep(context.Background(), format.GrepQuery{Pattern: "zebracorn", Fixed: true}, format.Filters{})
			if err != nil || len(page.Hits) == 0 {
				t.Fatalf("grep: %v", err)
			}
		})
		cost.Tables = map[string]perfguard.TableCost{"public.messages": cost.Tables["public.messages"]}
		return cost
	})
}

// The grep candidate query selects through the trigram index.
func TestGrepCandidatesPlanIndexed(t *testing.T) {
	s, _ := perfCorpus(t, 200, 8)
	oldMatches(t, s.Pool)
	for _, oldest := range []bool{false, true} {
		q := &query{}
		q.where("m.text ILIKE " + q.arg("%zebracorn%"))
		if err := hitFilters(q, format.Filters{}); err != nil {
			t.Fatal(err)
		}
		perfguard.AssertIndexedPlanExcept(t, s.Pool, []string{"users", "devices"}, grepCandidates(q, oldest), q.args...)
	}
}

// rareRepo places the five oldest sessions of a perfCorpus in a checkout
// of one remote; no other session has a remote. With 250 or more
// sessions they hold at most 2% of the messages.
const rareRemote = "github.com/perf/rare"

func rareRepo(t testing.TB, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE sources SET remote=$1,checkout='/src/rare'
		WHERE id IN (SELECT md5('f'||i)::uuid FROM generate_series(1,5) i)`, rareRemote); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `VACUUM ANALYZE`); err != nil {
		t.Fatal(err)
	}
}

// broadPattern is an alternation of two literals every perfCorpus message
// holds ("step j of session i"): its trigram condition admits every
// message in every repo.
const broadPattern = "step|session"

// A grep filtered to a repo reads that repo's messages, not every message
// the pattern's trigrams admit: a broad pattern over a repo holding a
// fixed few sessions costs the same however many other sessions there
// are. Only messages are gated, as in TestGrepOldMatchesScalingConstant.
func TestGrepRepoShareScalingConstant(t *testing.T) {
	perfguard.AssertScaling(t, perfguard.Constant, 500, 8, func(t testing.TB, n int) perfguard.Cost {
		s, counter := perfCorpus(t, max(n, 5), 4)
		rareRepo(t, s.Pool)
		rare := map[string]bool{}
		for i := 1; i <= 5; i++ {
			rare[sessionAt(t, s.Pool, i)] = true
		}
		cost := perfguard.Measure(t, s.Pool, counter, func() {
			page, err := s.Grep(context.Background(), format.GrepQuery{Pattern: broadPattern, Limit: 500}, format.Filters{RepoRemotes: []string{rareRemote}})
			if err != nil || page.Truncated || page.Total != 20 {
				t.Fatalf("grep: %v (page %+v)", err, page)
			}
			for _, h := range page.Hits {
				if !rare[h.ConversationID] {
					t.Fatalf("hit outside the repo: %+v", h)
				}
			}
		})
		cost.Tables = map[string]perfguard.TableCost{"public.messages": cost.Tables["public.messages"]}
		return cost
	})
}

// activityFromMessages sets each conversation's last activity to its
// newest message's time, as ingest keeps it (perfCorpus staggers them
// for the keyset tests).
func activityFromMessages(t testing.TB, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE conversation_activity a SET last_activity_at=(SELECT max(m.ts) FROM messages m WHERE m.conversation_id=a.conversation_id)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `VACUUM ANALYZE`); err != nil {
		t.Fatal(err)
	}
}

// A grep since a time reads the messages of the sessions active since
// then, through conversation_activity_idx, not every message the
// pattern's trigrams admit: a broad pattern since the five newest sessions
// costs the same however many older sessions there are. Without the
// conversation step the trigram bitmap covers the whole table and m.ts,
// unindexed by design (TestNoMessagesTSIndex), filters it afterwards.
func TestGrepSinceScalingConstant(t *testing.T) {
	perfguard.AssertScaling(t, perfguard.Constant, 500, 8, func(t testing.TB, n int) perfguard.Cost {
		n = max(n, 5)
		s, counter := perfCorpus(t, n, 4)
		activityFromMessages(t, s.Pool)
		since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(n-4) * time.Minute)
		cost := perfguard.Measure(t, s.Pool, counter, func() {
			page, err := s.Grep(context.Background(), format.GrepQuery{Pattern: broadPattern, Limit: 500}, format.Filters{Since: since})
			if err != nil || page.Truncated || page.Total != 20 {
				t.Fatalf("grep: %v (page %+v)", err, page)
			}
		})
		cost.Tables = map[string]perfguard.TableCost{"public.messages": cost.Tables["public.messages"]}
		return cost
	})
}

// The conversation list a filtered grep resolves first walks indexes, and
// the candidate query it narrows probes the messages' conversation index
// (ANDed with the trigram bitmap, or alone with the text filtered).
func TestGrepFilteredCandidatesPlanIndexed(t *testing.T) {
	s, _ := perfCorpus(t, 200, 8)
	rareRepo(t, s.Pool)
	f := format.Filters{RepoRemotes: []string{rareRemote}}
	cq := &query{}
	grepConvWhere(cq, f)
	perfguard.AssertIndexedPlanExcept(t, s.Pool, []string{"users", "devices"}, grepConvQuery(cq, grepConvCap+1), cq.args...)
	tx, err := s.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids, narrowed, err := grepConversations(context.Background(), tx, f)
	_ = tx.Rollback(context.Background())
	if err != nil || !narrowed || len(ids) != 5 {
		t.Fatalf("grepConversations: %d ids, narrowed=%v, %v", len(ids), narrowed, err)
	}
	plan, err := grep.Compile(format.GrepQuery{Pattern: broadPattern})
	if err != nil {
		t.Fatal(err)
	}
	for _, oldest := range []bool{false, true} {
		q := &query{}
		q.where(trigramCond(q, plan.Query))
		if err := hitFilters(q, f); err != nil {
			t.Fatal(err)
		}
		q.where("m.conversation_id=ANY(" + q.arg(ids) + "::uuid[])")
		sql := grepCandidates(q, oldest)
		perfguard.AssertIndexedPlanExcept(t, s.Pool, []string{"users", "devices"}, sql, q.args...)
		explained, err := perfguard.Explain(s.Pool, sql, q.args...)
		if err != nil {
			t.Fatal(err)
		}
		probes := perfguard.IndexProbes(explained)
		if !slices.Contains(probes, "messages_conversation_ordinal_idx") && !slices.Contains(probes, "messages_default_filter_idx") {
			t.Errorf("oldest=%v: the narrowed candidate query does not probe the messages' conversation index; probes %v\nplan:\n%s", oldest, probes, explained)
		}
	}
}

// A filter admitting more than grepConvCap conversations leaves the
// candidate query as it is; the hits are the same.
func TestGrepConversationCapFallsBack(t *testing.T) {
	s, _ := perfCorpus(t, grepConvCap+1, 1)
	f := format.Filters{Agent: "claude"}
	tx, err := s.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids, ok, err := grepConversations(context.Background(), tx, f)
	_ = tx.Rollback(context.Background())
	if err != nil || ok || ids != nil {
		t.Fatalf("grepConversations over the cap: %d ids, ok=%v, %v", len(ids), ok, err)
	}
	page, err := s.Grep(context.Background(), format.GrepQuery{Pattern: "step 0 of session " + strconv.Itoa(grepConvCap+1), Fixed: true}, f)
	if err != nil || page.Truncated || page.Total != 1 || len(page.Notes) != 0 {
		t.Fatalf("grep over the cap: %v (page %+v)", err, page)
	}
}

// No index leads with messages.ts. The planner, planning grep's cursor for
// its first rows, would walk it newest first instead of reading the
// trigram candidates, and a pattern whose matches are few and old would
// then read nearly every message (see grepCandidates). Plans and cost at
// test sizes do not show this reliably, so the schema is checked.
func TestNoMessagesTSIndex(t *testing.T) {
	// allowed names indexes that lead with messages.ts for a reason grep
	// cannot trip over, each with that reason: a partial index whose
	// predicate grep never matches (WHERE superseded, say), or one for
	// another job after grep's ORDER BY was made unindexable. Add one only
	// with a grep plan and scaling check that shows it is not walked.
	allowed := map[string]string{}
	s, _ := perfCorpus(t, 1, 1)
	var names []string
	rows, err := s.Pool.Query(context.Background(), `SELECT i.indexrelid::regclass::text FROM pg_index i
		JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=i.indkey[0]
		WHERE i.indrelid='messages'::regclass AND a.attname='ts'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		if _, ok := allowed[n]; !ok {
			names = append(names, n)
		}
	}
	if len(names) > 0 {
		t.Fatalf("indexes leading with messages.ts: %v. Grep plans its candidate query as a cursor (for the first rows), so the "+
			"planner may walk such an index newest first and filter every message by the pattern instead of reading the trigram "+
			"candidates; a pattern whose matches are few and old then reads nearly the whole table (see grepCandidates). "+
			"Drop the index, or, if it is needed for another job and grep cannot use it, add it to allowed in this test with the "+
			"reason and a grep check that proves it", names)
	}
}

// An outline page past the last entry is an empty outline, not a read
// with no messages, here and after the JSON the API sends.
func TestOutlinePastEnd(t *testing.T) {
	s, _ := perfCorpus(t, 2, 8)
	conv := sessionAt(t, s.Pool, 1)
	cx, err := s.outline(context.Background(), conv, format.ReadQuery{Outline: true, Cursor: outlineCursor(t, s.Pool, conv, 3)})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(cx)
	if err != nil {
		t.Fatal(err)
	}
	var wire format.Context
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	for _, c := range []*format.Context{cx, &wire} {
		var b strings.Builder
		if err := format.WriteRead(&b, c, format.Style{}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(b.String(), "[no prompts or tool calls]") {
			t.Fatalf("empty outline page renders as\n%s", b.String())
		}
	}
}

// A read of one page costs the same whatever the session's length: the
// header (its message count) and the page, for a message address in the
// middle of the session and for a bare session address (its first
// message).
func TestReadPageScalingConstant(t *testing.T) {
	for _, at := range []string{"message", "session"} {
		t.Run(at, func(t *testing.T) {
			perfguard.AssertScaling(t, perfguard.Constant, 500, 8, func(t testing.TB, n int) perfguard.Cost {
				s, counter := perfCorpus(t, 3, n)
				var addr string
				q := `SELECT CASE $1::text WHEN 'message' THEN md5('m'||2||'/'||$2::int)::uuid::text ELSE md5('s'||2) END`
				if err := s.Pool.QueryRow(context.Background(), q, at, n/2).Scan(&addr); err != nil {
					t.Fatal(err)
				}
				return perfguard.Measure(t, s.Pool, counter, func() {
					cx, err := s.Read(context.Background(), "", format.ReadQuery{Address: addr}, format.Filters{})
					if err != nil {
						t.Fatal(err)
					}
					if len(cx.Messages) == 0 || cx.Conversation.Messages != n {
						t.Fatalf("read at n=%d: %d messages, header count %d", n, len(cx.Messages), cx.Conversation.Messages)
					}
				})
			})
		})
	}
}

// globCorpus is a perfCorpus whose first session alone has a rare word
// in its title, and whose second alone has it in its repo's last path
// element.
func globCorpus(t testing.TB, sessions int) (*Store, *perfguard.Counter) {
	t.Helper()
	s, counter := perfCorpus(t, max(sessions, 2), 1)
	for _, q := range []string{
		`UPDATE conversations SET title='fix the zebracorn' WHERE id=md5('c'||1)::uuid`,
		`UPDATE conversations SET repo_root='/src/zebracorn-app' WHERE id=md5('c'||2)::uuid`,
		`VACUUM ANALYZE conversations`,
	} {
		if _, err := s.Pool.Exec(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	return s, counter
}

// A sessions glob that matches a few sessions reads those, not the list:
// its cost is the same whatever the corpus size.
//
// Sequential scans are off, as in the plan tests: with the hot columns in
// conversation_activity, a conversations row is narrow, and a scan of
// this corpus's 4000 rows (about 150 pages) costs the planner less than
// the four trigram index scans (crossover near 4500 rows), so it rightly
// scans a table this small. What the guard catches is an index path that
// walks the list and filters by the glob, which stays open.
func TestSessionsRareGlobScalingConstant(t *testing.T) {
	perfguard.AssertScaling(t, perfguard.Constant, 500, 8, func(t testing.TB, n int) perfguard.Cost {
		s, counter := globCorpus(t, n)
		if _, err := s.Pool.Exec(context.Background(), `DO $$ BEGIN EXECUTE format('ALTER DATABASE %I SET enable_seqscan=off', current_database()); END $$`); err != nil {
			t.Fatal(err)
		}
		s.Pool.Reset() // new connections take the setting
		return perfguard.Measure(t, s.Pool, counter, func() {
			out, err := s.Sessions(context.Background(), "*zebracorn*", "", format.Filters{Limit: 20})
			if err != nil {
				t.Fatal(err)
			}
			if len(out.Sessions) != 2 {
				t.Fatalf("glob matched %d sessions at n=%d, want 2", len(out.Sessions), n)
			}
		})
	})
}

// The sessions glob selects through the trigram indexes, both ways and
// from a cursor mid-list. The planner rightly walks the list and filters
// when few rows are left to walk (a small table, a cursor near the end),
// so the corpus is the scaling test's large size and the cursor leaves
// about half the list either way.
func TestSessionsGlobPlanIndexed(t *testing.T) {
	s, _ := globCorpus(t, 4000)
	dated, err := format.ParseSessionCursor(sessionsCursor(t, s.Pool, 1700))
	if err != nil {
		t.Fatal(err)
	}
	for _, glob := range []string{"*zebracorn*", "zebracorn", "zebra*corn"} {
		for _, oldest := range []bool{false, true} {
			for _, after := range []*format.SessionKey{nil, &dated} {
				sql, args := sessionsPage(glob, format.Filters{}, oldest, after, 21)
				perfguard.AssertIndexedPlanExcept(t, s.Pool, []string{"users", "devices"}, sql, args...)
				// An index walk in list order that filters by the glob
				// has an Index Cond (the activity bound) yet reads the
				// whole list for a rare glob: the glob must select rows
				// (an index condition), never filter them.
				plan, err := perfguard.Explain(s.Pool, sql, args...)
				if err != nil {
					t.Fatal(err)
				}
				if f := globFilters(t, plan); len(f) > 0 {
					t.Errorf("glob %q oldest=%v after=%v: applied as a filter: %v\nplan:\n%s", glob, oldest, after != nil, f, plan)
				}
			}
		}
	}
}

// globFilters returns the plan nodes' Filters that apply an ILIKE (~~*).
func globFilters(t testing.TB, plan string) []string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(plan), &v); err != nil {
		t.Fatal(err)
	}
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			if f, ok := x["Filter"].(string); ok && strings.Contains(f, "~~*") {
				out = append(out, fmt.Sprintf("%v on %v: %s", x["Node Type"], x["Relation Name"], f))
			}
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(v)
	return out
}

// The glob keeps its meaning: each of session id, title and repo (or
// cwd, or its last path element) matches, case-insensitively, and a
// near miss does not.
func TestSessionsGlobSemantics(t *testing.T) {
	s, _ := globCorpus(t, 20)
	ctx := context.Background()
	if _, err := s.Pool.Exec(ctx, `UPDATE conversations SET repo_root=NULL,cwd='/home/x/Zebracorn-Cwd' WHERE id=md5('c'||3)::uuid`); err != nil {
		t.Fatal(err)
	}
	var sid string
	if err := s.Pool.QueryRow(ctx, `SELECT session_id FROM conversations WHERE id=md5('c'||4)::uuid`).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		glob string
		want []int
	}{
		{"zebracorn", []int{1, 2, 3}},
		{"ZEBRACORN", []int{1, 2, 3}},
		{"zebracorn-*", []int{2, 3}},     // the repo's (or cwd's) last element
		{"/src/zebracorn-app", []int{2}}, // the whole repo
		{"fix the zebra?orn", []int{1}},  // the whole title
		{"*the*corn", []int{1}},
		{sid[:12] + "*", []int{4}}, // a session id prefix
		{"zebracornx", nil},
		{"fix the zebracorn!", nil},
	} {
		out, err := s.Sessions(ctx, tc.glob, "", format.Filters{Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, c := range out.Sessions {
			got = append(got, c.ID)
		}
		var want []string
		for _, i := range tc.want {
			want = append(want, sessionAt(t, s.Pool, i))
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("glob %q: got %v, want %v", tc.glob, got, want)
		}
	}
}

// A bare session address finds its first row through an index in
// ordinal order, for any address and for the caller's own session.
func TestFirstLivePlanIndexed(t *testing.T) {
	s, _ := perfCorpus(t, 20, 200)
	var sid, owner string
	if err := s.Pool.QueryRow(context.Background(), `SELECT session_id,user_id::text FROM conversations WHERE id=md5('c'||2)::uuid`).Scan(&sid, &owner); err != nil {
		t.Fatal(err)
	}
	perfguard.AssertIndexedPlan(t, s.Pool, firstLive(addressConversations+` c`, `c.session_id=$3`), "", false, sid)
	perfguard.AssertIndexedPlan(t, s.Pool, firstLive(visible+` c`, `c.session_id=$1 AND c.user_id=$2`), sid, owner)
}

// hideNewest hides the newest 80% of a perfCorpus's sessions (i > n/5),
// as an admin path rule over a busy repo would: every one of them is
// newer than the first page of visible sessions.
func hideNewest(t testing.TB, pool *pgxpool.Pool, n int) {
	t.Helper()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE conversations SET hidden_at=now(),hidden_rule='perf',hidden_root=id
		 WHERE id IN (SELECT md5('c'||i)::uuid FROM generate_series($1::int/5+1,$1::int) i)`, []any{n}},
		{`VACUUM ANALYZE`, nil},
	} {
		if _, err := pool.Exec(context.Background(), q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
}

// One page of sessions costs the same however many hidden sessions are
// newer than it: the keyset index leaves hidden sessions out, so the page
// never reads them. Rows and sequential pages are gated by AssertScaling;
// blocks (heap and index buffers) are gated here too, since a walk over
// hidden index entries shows in buffers first.
func TestSessionsPageHiddenScalingConstant(t *testing.T) {
	for _, oldest := range []bool{false, true} {
		t.Run(map[bool]string{false: "newest", true: "oldest"}[oldest], func(t *testing.T) {
			costs := map[int]perfguard.Cost{}
			var hidden []string
			perfguard.AssertScaling(t, perfguard.Constant, 500, 8, func(t testing.TB, n int) perfguard.Cost {
				s, counter := perfCorpus(t, n, 4)
				hideNewest(t, s.Pool, n)
				c := perfguard.Measure(t, s.Pool, counter, func() {
					out, err := s.Sessions(context.Background(), "", "", format.Filters{Limit: 20, Sort: map[bool]string{false: format.SortNewest, true: format.SortOldest}[oldest]})
					if err != nil {
						t.Fatal(err)
					}
					if n >= 100 && len(out.Sessions) != 20 {
						t.Fatalf("page of %d sessions at n=%d, want 20", len(out.Sessions), n)
					}
					hidden = hidden[:0]
					for _, c := range out.Sessions {
						hidden = append(hidden, c.ID)
					}
				})
				var listedHidden int
				if err := s.Pool.QueryRow(context.Background(), `SELECT count(*) FROM conversations WHERE id::text=ANY($1) AND hidden_at IS NOT NULL`, hidden).Scan(&listedHidden); err != nil || listedHidden > 0 {
					t.Fatalf("listed %d hidden sessions at n=%d (%v)", listedHidden, n, err)
				}
				costs[n] = c
				return c
			})
			small, large, base := costs[500].Total().Blocks(), costs[4000].Total().Blocks(), costs[1].Total().Blocks()
			if r := float64(large-base) / float64(max(small-base, 16)); r > perfguard.Constant.Bound(8) {
				t.Errorf("blocks grow %.2fx from n=500 (%d) to n=4000 (%d), base %d: want <= %.1fx", r, small, large, base, perfguard.Constant.Bound(8))
			}
			t.Logf("blocks: base %d, n=500 %d, n=4000 %d", base, small, large)
		})
	}
}

// With most sessions hidden, both keyset branches still walk the
// activity index, whose predicate leaves hidden sessions out.
func TestSessionsHiddenPlanIndexed(t *testing.T) {
	s, _ := perfCorpus(t, 200, 1)
	hideNewest(t, s.Pool, 200)
	for _, oldest := range []bool{false, true} {
		sql, args := sessionsPage("", format.Filters{}, oldest, nil, 21)
		perfguard.AssertIndexedPlanExcept(t, s.Pool, []string{"users", "devices"}, sql, args...)
		plan, err := perfguard.Explain(s.Pool, sql, args...)
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(plan, "conversation_activity_idx"); n < 2 {
			t.Errorf("oldest=%v: %d branches on conversation_activity_idx, want 2\nplan:\n%s", oldest, n, plan)
		}
	}
}
