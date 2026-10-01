package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/perfguard"
	"github.com/flopwire/flopwire/internal/retrieval/format"
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
		{`INSERT INTO conversations(id,source_id,agent,session_id,device_id,user_id,repo_root,title,started_at,last_activity_at)
		 SELECT md5('c'||i)::uuid,md5('f'||i)::uuid,'claude',md5('s'||i),md5('d'||(i%40))::uuid,md5('u'||(i%40%10))::uuid,
		        '/src/repo'||(i%5),'session '||i,'2026-09-01'::timestamptz+i*interval '1 minute',
		        CASE WHEN i%7=0 THEN NULL ELSE '2026-09-01'::timestamptz+(i/3)*interval '1 minute'+(i/3%5)*interval '7 microseconds' END
		 FROM generate_series(1,$1) i`, []any{sessions}},
		{`INSERT INTO messages(id,conversation_id,source_id,ordinal,kind,ts,text,text_len,content_sha,source_generation,parser)
		 SELECT md5('m'||i||'/'||j)::uuid,md5('c'||i)::uuid,md5('f'||i)::uuid,j,(ARRAY['user','assistant','tool_call','tool_result'])[1+j%4],
		        '2026-09-01'::timestamptz+i*interval '1 minute'+j*interval '1 millisecond','step '||j||' of session '||i,20,
		        sha256(convert_to(i||'/'||j,'UTF8')),0,'test'
		 FROM generate_series(1,$1) i, generate_series(0,$2-1) j`, []any{sessions, msgs}},
		// Digests carry the per-kind counts ingest maintains; the read
		// header's message count comes from them.
		{`UPDATE conversations c SET digest=(SELECT jsonb_build_object('messages',jsonb_object_agg(kind,k))
		 FROM (SELECT kind,count(*) k FROM messages m WHERE m.conversation_id=c.id GROUP BY kind) x)`, nil},
		{`ANALYZE`, nil},
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
	if err := pool.QueryRow(context.Background(), `SELECT id::text,last_activity_at FROM conversations
		ORDER BY last_activity_at DESC NULLS LAST,id DESC OFFSET $1 LIMIT 1`, i).Scan(&c.ID, &c.LastActivityAt); err != nil {
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
		rows, err := s.Pool.Query(ctx, `SELECT id::text FROM conversations WHERE `+where+` ORDER BY `+order)
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
