package retrieval

import (
	"context"
	"crypto/sha256"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// findFixture is a migrated database with one conversation whose messages
// the test writes directly.
type findFixture struct {
	t    *testing.T
	s    *Store
	conv string
	n    int
}

func newFindFixture(t *testing.T) *findFixture {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, pgtest.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	user, device, conv := uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now().UTC()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id,email,name,role,identity_type,created_at) VALUES($1,'gary@example.test','Gary','member','human',$2)`, []any{user, now}},
		{`INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$2,'laptop-a','darwin',$3)`, []any{device, user, now}},
		{`INSERT INTO conversations(id,agent,session_id,device_id,user_id,repo_root) VALUES($1,'claude','sess-1',$2,$3,'/src/flopwire')`, []any{conv, device, user}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	return &findFixture{t: t, s: &Store{Pool: pool}, conv: conv}
}

func (f *findFixture) add(kind, text string) {
	f.t.Helper()
	f.n++
	sum := sha256.Sum256([]byte(text))
	if _, err := f.s.Pool.Exec(context.Background(), `INSERT INTO messages(id,conversation_id,ordinal,kind,ts,text,text_len,content_sha,source_generation,parser)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,0,'test')`, uuid.NewString(), f.conv, f.n, kind, time.Now().Add(time.Duration(f.n)*time.Second), text, len(text), sum[:]); err != nil {
		f.t.Fatal(err)
	}
}

func (f *findFixture) find(pattern string, regex, cs bool) (*format.Page, error) {
	return f.s.Grep(context.Background(), format.GrepQuery{Pattern: pattern, Fixed: !regex, CaseSensitive: cs}, format.Filters{})
}

// snippet is a grep hit's first matching line.
func snip(h format.Hit) string {
	for _, l := range h.Lines {
		if l.Match {
			return l.Text
		}
	}
	return ""
}

func matching(h format.Hit) int {
	n := 0
	for _, l := range h.Lines {
		if l.Match {
			n++
		}
	}
	return n + h.MoreLines
}

// V1, V7: the server verifies regexes in Go with the local index's RE2
// semantics (multi-line ^ and $, \b word boundaries), and refuses a pattern
// whose trigram plan cannot narrow the scan.
func TestFindRE2SemanticsAndRefusesUnindexed(t *testing.T) {
	f := newFindFixture(t)
	f.add("tool_result", "building...\nretry succeeded after 3 attempts\ndone")
	f.add("assistant", "the retrying loop is fine")
	f.add("user", "RETRY THE UPLOAD")

	for _, tc := range []struct {
		pattern string
		cs      bool
		want    []string // snippets, newest first
	}{
		{`^retry\b`, false, []string{"RETRY THE UPLOAD", "retry succeeded after 3 attempts"}},
		{`^retry\b`, true, []string{"retry succeeded after 3 attempts"}},
		{`attempts$`, false, []string{"retry succeeded after 3 attempts"}},
		{`\bretrying\b`, false, []string{"the retrying loop is fine"}},
		{`upload|attempts`, false, []string{"RETRY THE UPLOAD", "retry succeeded after 3 attempts"}},
	} {
		page, err := f.find(tc.pattern, true, tc.cs)
		if err != nil {
			t.Fatalf("%s: %v", tc.pattern, err)
		}
		var got []string
		for _, h := range page.Hits {
			got = append(got, snip(h))
		}
		if strings.Join(got, "|") != strings.Join(tc.want, "|") || page.Truncated {
			t.Errorf("%s (cs=%v): %q, want %q", tc.pattern, tc.cs, got, tc.want)
		}
		if len(page.Hits) > 0 && page.Hits[0].Lines[0].N == 0 {
			t.Errorf("%s: no line number", tc.pattern)
		}
	}
	// Line numbers and the matching-line count come from the Go match.
	page, _ := f.find(`retry|done`, true, false)
	for _, h := range page.Hits {
		if strings.HasPrefix(snip(h), "retry succeeded") && (h.Lines[0].N != 2 || matching(h) != 2) {
			t.Errorf("multi-line hit: line %d, %d matches", h.Lines[0].N, matching(h))
		}
	}
	for _, p := range []string{`.`, `a.b`, `.*`, `\d+`, `^$`, `[a-z]{5}`, `ab|xyz`} {
		if _, err := f.find(p, true, false); !errors.Is(err, ErrBadRequest) || !strings.Contains(err.Error(), "scan every message") {
			t.Errorf("unindexed %q: %v", p, err)
		}
	}
	// A short substring has no trigram either.
	if _, err := f.find("re", false, false); !errors.Is(err, ErrBadRequest) {
		t.Errorf("2-char substring: %v", err)
	}
	// Substrings are literal, never regex syntax.
	f.add("tool_result", "cost is $5.00 (approx)")
	if page, err := f.find("$5.00 (approx", false, false); err != nil || len(page.Hits) != 1 {
		t.Errorf("literal substring: %+v %v", page, err)
	}
}

// D3: find reads every byte of a message, however long; nothing is capped
// to a head and a tail.
func TestFindUncappedText(t *testing.T) {
	f := newFindFixture(t)
	var b strings.Builder
	for i := 0; b.Len() < 1<<20; i++ {
		b.WriteString("ok upload chunk\n")
	}
	b.WriteString("panic: middle-of-the-log marker\n")
	for i := 0; b.Len() < 2<<20; i++ {
		b.WriteString("ok upload chunk\n")
	}
	f.add("tool_result", b.String())
	page, err := f.find("middle-of-the-log", false, false)
	if err != nil || len(page.Hits) != 1 || snip(page.Hits[0]) != "panic: middle-of-the-log marker" {
		t.Fatalf("find in the middle of a 2MB result: %+v %v", page, err)
	}
	if page.Hits[0].User != "gary@example.test" || page.Hits[0].Device != "laptop-a" || page.Hits[0].SessionID != "sess-1" || page.Hits[0].Repo != "/src/flopwire" {
		t.Fatalf("attribution: %+v", page.Hits[0])
	}
}

// V1: retrieval statements run under a Postgres statement_timeout equal to
// the query budget, so a slow query frees its pooled connection; a budget
// that runs out is a truncated page, not an error.
func TestQueryBudget(t *testing.T) {
	f := newFindFixture(t)
	start := time.Now()
	err := f.s.read(context.Background(), 300*time.Millisecond, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `SELECT pg_sleep(5)`)
		return err
	})
	if !timedOut(err) || time.Since(start) > 3*time.Second {
		t.Fatalf("statement timeout: %v after %s", err, time.Since(start))
	}
	f.add("user", "retry the upload")
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	page, err := f.s.Grep(ctx, format.GrepQuery{Pattern: "upload", Fixed: true}, format.Filters{})
	if err != nil || !page.Truncated || !strings.HasPrefix(page.Reason, "timed out after") {
		t.Fatalf("expired budget: %+v %v", page, err)
	}
	page, err = f.s.Search(ctx, format.SearchQuery{Query: "upload"}, format.Filters{})
	if err != nil || !page.Truncated {
		t.Fatalf("expired search budget: %+v %v", page, err)
	}
	if b := budget(context.Background()); b != DefaultBudget {
		t.Fatalf("default budget %s", b)
	}
	long, cancel2 := context.WithTimeout(context.Background(), time.Hour)
	defer cancel2()
	if b := budget(long); b != MaxBudget {
		t.Fatalf("budget above the maximum: %s", b)
	}
}

// A substring of short words ("go to") has no 3-letter run, but pg_trgm
// still narrows it with space-padded trigrams ("go ", " to"), and the
// local index serves it; the server must not refuse it. A substring whose
// words are all single letters stays refused.
func TestFindShortWordPhrase(t *testing.T) {
	f := newFindFixture(t)
	f.add("assistant", "let us go to the park")
	f.add("assistant", "gone today")
	page, err := f.find("go to", false, false)
	if err != nil {
		t.Fatalf("go to: %v", err)
	}
	if len(page.Hits) != 1 || snip(page.Hits[0]) != "let us go to the park" {
		t.Fatalf("go to: %+v", page.Hits)
	}
	page, err = f.find("GO TO", false, true)
	if err != nil || len(page.Hits) != 0 {
		t.Fatalf("case-sensitive GO TO: %v %+v", err, page)
	}
	if _, err := f.find("a b", false, false); !errors.Is(err, ErrBadRequest) {
		t.Errorf("single-letter words: %v", err)
	}
}

// D6: search and find leave out injected text (CLAUDE.md, AGENTS.md,
// system reminders) unless the filter names kinds, as the local index
// does. Context and conversation still show it.
func TestHitsHideInjectedByDefault(t *testing.T) {
	f := newFindFixture(t)
	f.add("injected", "always run gofmt before committing")
	f.add("user", "did you run gofmt on it")
	ctx := context.Background()
	for name, run := range map[string]func(format.Filters) (*format.Page, error){
		"search": func(fl format.Filters) (*format.Page, error) {
			return f.s.Search(ctx, format.SearchQuery{Query: "gofmt"}, fl)
		},
		"grep": func(fl format.Filters) (*format.Page, error) {
			return f.s.Grep(ctx, format.GrepQuery{Pattern: "gofmt", Fixed: true}, fl)
		},
	} {
		p, err := run(format.Filters{})
		if err != nil {
			t.Fatal(name, err)
		}
		if len(p.Hits) != 1 || p.Hits[0].Kind != "user" {
			t.Fatalf("%s: %d hits (%+v), want only the user row", name, len(p.Hits), p.Hits)
		}
		if p, err = run(format.Filters{Kinds: []string{"injected"}}); err != nil || len(p.Hits) != 1 || p.Hits[0].Kind != "injected" {
			t.Fatalf("%s --kind injected: %+v %v", name, p, err)
		}
	}
	c, err := f.s.Read(ctx, "", format.ReadQuery{Address: "sess-1"}, format.Filters{})
	if err != nil || len(c.Messages) != 2 {
		t.Fatalf("read session: %+v %v", c, err)
	}
}

// The server's grep, search, sessions and read answer with the local
// index's semantics: identical texts collapse (+N copies), pages walk by
// offset with totals, -l/-c count sessions, a search with no every-term
// match ranks any term, and every printed address reads back.
func TestServerToolsMatchLocalSemantics(t *testing.T) {
	f := newFindFixture(t)
	ctx := context.Background()
	f.add("tool_result", "retry once\nok\nretry twice") // 1
	f.add("assistant", "the upload retry is flaky")     // 2
	f.add("tool_result", "retry once\nok\nretry twice") // 3: a copy of 1
	f.add("user", "use exponential backoff for the retry")
	var other string
	firstConv := f.conv
	if err := f.s.Pool.QueryRow(ctx, `INSERT INTO conversations(id,agent,session_id,device_id,user_id,repo_root,title,last_activity_at)
		SELECT gen_random_uuid(),'codex','sess-2',device_id,user_id,'/src/web','web work',now() FROM conversations WHERE id=$1 RETURNING id::text`, f.conv).Scan(&other); err != nil {
		t.Fatal(err)
	}
	f.conv, f.n = other, 10
	f.add("tool_result", "retry in the web repo")
	f.conv = ""
	grepQ := func(q format.GrepQuery, fl format.Filters) *format.Page {
		t.Helper()
		p, err := f.s.Grep(ctx, q, fl)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := grepQ(format.GrepQuery{Pattern: "retry"}, format.Filters{})
	if len(p.Hits) != 4 || p.Total != 4 || p.TotalSessions != 2 || !p.Exact || p.Hits[0].SessionID != "sess-2" {
		t.Fatalf("grep: %+v", p)
	}
	var copied format.Hit
	for _, h := range p.Hits {
		if h.Copies == 1 {
			copied = h
		}
	}
	if len(copied.Lines) != 2 || copied.Lines[1].N != 3 || copied.Address != format.MessageAddress("sess-1", 3) {
		t.Fatalf("collapsed hit: %+v", copied)
	}
	if p = grepQ(format.GrepQuery{Pattern: "retry", Limit: 2, Offset: 1}, format.Filters{}); len(p.Hits) != 2 || p.Next != 3 || p.Total != 4 {
		t.Fatalf("page: %+v", p)
	}
	if p = grepQ(format.GrepQuery{Pattern: "retry", Mode: format.ModeCount}, format.Filters{}); len(p.Sessions) != 2 || p.Sessions[1].Hits != 3 || p.Sessions[1].Address != "sess-1" {
		t.Fatalf("count: %+v", p.Sessions)
	}
	if p = grepQ(format.GrepQuery{Pattern: "retry", Fixed: true}, format.Filters{Repo: "web"}); len(p.Hits) != 1 || p.Hits[0].Repo != "/src/web" {
		t.Fatalf("repo name: %+v", p.Hits)
	}
	if p = grepQ(format.GrepQuery{Pattern: "retry", Fixed: true}, format.Filters{ExcludeKinds: []string{"tool_result"}}); len(p.Hits) != 2 {
		t.Fatalf("exclude kind: %+v", p.Hits)
	}
	// Search: copies collapse; no every-term match ranks any term.
	sp, err := f.s.Search(ctx, format.SearchQuery{Query: "retry once"}, format.Filters{})
	if err != nil || len(sp.Hits) != 1 || sp.Hits[0].Copies != 1 || sp.Hits[0].Address == "" {
		t.Fatalf("search copies: %+v %v", sp, err)
	}
	sp, err = f.s.Search(ctx, format.SearchQuery{Query: "how did we handle exponential tokenizer"}, format.Filters{})
	if err != nil || len(sp.Hits) != 1 || len(sp.Notes) != 1 || !strings.Contains(sp.Notes[0], "any of: handle exponential tokenizer") {
		t.Fatalf("search any-term: %+v %v", sp, err)
	}
	// Prompts and replies outrank tool output that matches as well, even
	// newer tool output.
	f.conv = firstConv
	f.add("assistant", "ocelot fence ya")
	f.add("tool_result", "ocelot fence ok")
	if sp, err = f.s.Search(ctx, format.SearchQuery{Query: "ocelot fence"}, format.Filters{}); err != nil || len(sp.Hits) != 2 || sp.Hits[0].Kind != "assistant" {
		t.Fatalf("search kind boost: %+v %v", sp, err)
	}
	// Sessions: newest first, glob, totals.
	ss, err := f.s.Sessions(ctx, "", 0, format.Filters{})
	if err != nil || ss.Total != 2 || ss.Sessions[0].SessionID != "sess-2" || ss.Sessions[0].Address != "sess-2" || ss.Sessions[1].Messages != 6 {
		t.Fatalf("sessions: %+v %v", ss, err)
	}
	if ss, err = f.s.Sessions(ctx, "web*", 0, format.Filters{}); err != nil || ss.Total != 1 || ss.Sessions[0].SessionID != "sess-2" {
		t.Fatalf("sessions glob: %+v %v", ss, err)
	}
	// Read: the addresses grep printed; an ambiguous prefix says so.
	for _, h := range grepQ(format.GrepQuery{Pattern: "retry"}, format.Filters{}).Hits {
		cx, err := f.s.Read(ctx, "", format.ReadQuery{Address: h.Address + ":1"}, format.Filters{})
		if err != nil || cx.Focus != h.MessageID || cx.Line != 1 || cx.Messages[0].Address != h.Address {
			t.Fatalf("read %s: %+v %v", h.Address, cx, err)
		}
	}
	if _, err := f.s.Read(ctx, "", format.ReadQuery{Address: "sess/1"}, format.Filters{}); !errors.Is(err, ErrBadRequest) || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous: %v", err)
	}
	cx, err := f.s.Read(ctx, "", format.ReadQuery{Address: "sess-1/2", Before: 1, After: 1, MaxChars: 8}, format.Filters{})
	if err != nil || len(cx.Messages) != 3 || !cx.MoreAfter || cx.MoreBefore || cx.Messages[1].Text != "the uplo" || !cx.Messages[1].Clipped {
		t.Fatalf("read neighbours: %+v %v", cx, err)
	}
}

// The shared filters mean what they mean locally: --agent takes a comma
// list, and the excluded calling session takes its subagents (at every
// depth) with it, on grep, search and sessions alike.
func TestServerAgentListAndSelfExclusion(t *testing.T) {
	f := newFindFixture(t)
	ctx := context.Background()
	f.add("assistant", "pangolin in the parent")
	root := f.conv
	for _, c := range []struct{ agent, session, parent string }{
		{"claude", "agent-x", "sess-1"}, {"claude", "agent-y", "agent-x"}, {"codex", "other", ""},
	} {
		var id string
		if err := f.s.Pool.QueryRow(ctx, `INSERT INTO conversations(id,agent,session_id,device_id,user_id,parent_native_session_id,depth)
			SELECT gen_random_uuid(),$2,$3,device_id,user_id,NULLIF($4,''),CASE WHEN $4='' THEN 0 ELSE 1 END FROM conversations WHERE id=$1 RETURNING id::text`,
			root, c.agent, c.session, c.parent).Scan(&id); err != nil {
			t.Fatal(err)
		}
		f.conv = id
		f.add("assistant", "pangolin in "+c.session)
	}
	sessionsOf := func(hits []format.Hit) string {
		var out []string
		for _, h := range hits {
			out = append(out, h.SessionID)
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	p, err := f.s.Grep(ctx, format.GrepQuery{Pattern: "pangolin"}, format.Filters{Agent: "claude,codex"})
	if err != nil || len(p.Hits) != 4 {
		t.Fatalf("agent list: %+v %v", p, err)
	}
	ex := format.Filters{ExcludeSession: "sess-1"}
	if p, err = f.s.Grep(ctx, format.GrepQuery{Pattern: "pangolin"}, ex); err != nil || sessionsOf(p.Hits) != "other" {
		t.Fatalf("grep self-exclusion: %s %v", sessionsOf(p.Hits), err)
	}
	if p, err = f.s.Search(ctx, format.SearchQuery{Query: "pangolin"}, ex); err != nil || sessionsOf(p.Hits) != "other" {
		t.Fatalf("search self-exclusion: %s %v", sessionsOf(p.Hits), err)
	}
	ss, err := f.s.Sessions(ctx, "", 0, ex)
	if err != nil || ss.Total != 1 || ss.Sessions[0].SessionID != "other" {
		t.Fatalf("sessions self-exclusion: %+v %v", ss, err)
	}
	// session scopes grep and search to one session (by a unique prefix)
	// and its subagents; an unknown prefix is an error, not "no matches".
	in := format.Filters{Session: "sess-1"}
	if p, err = f.s.Grep(ctx, format.GrepQuery{Pattern: "pangolin"}, in); err != nil || sessionsOf(p.Hits) != "agent-x,agent-y,sess-1" {
		t.Fatalf("grep session: %s %v", sessionsOf(p.Hits), err)
	}
	if p, err = f.s.Search(ctx, format.SearchQuery{Query: "pangolin"}, format.Filters{Session: "oth"}); err != nil || sessionsOf(p.Hits) != "other" {
		t.Fatalf("search session: %s %v", sessionsOf(p.Hits), err)
	}
	if _, err = f.s.Grep(ctx, format.GrepQuery{Pattern: "pangolin"}, format.Filters{Session: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown session: %v", err)
	}
	// A failed tool call is marked on its hit.
	if _, err := f.s.Pool.Exec(ctx, `UPDATE messages SET is_error=true WHERE text='pangolin in other'`); err != nil {
		t.Fatal(err)
	}
	p, err = f.s.Grep(ctx, format.GrepQuery{Pattern: "pangolin"}, format.Filters{Session: "other"})
	if err != nil || len(p.Hits) != 1 || !p.Hits[0].IsError {
		t.Fatalf("is_error: %+v %v", p, err)
	}
}

// A grep whose budget runs out after it has verified hits returns them as
// a truncated page: the lookups that follow the scan (session infos,
// addresses) must not fail on the spent deadline.
func TestGrepBudgetEndsAfterHits(t *testing.T) {
	f := newFindFixture(t)
	ctx := context.Background()
	if _, err := f.s.Pool.Exec(ctx, `INSERT INTO messages(id,conversation_id,ordinal,kind,ts,text,text_len,content_sha,source_generation,parser)
		SELECT gen_random_uuid(),$1,g,'tool_result',now()-g*interval '1 second','upload number '||g,20,sha256(('upload number '||g)::bytea),0,'test'
		FROM generate_series(1,20000) g`, f.conv); err != nil {
		t.Fatal(err)
	}
	partial := 0
	for d := 20 * time.Millisecond; d < 5*time.Second; d = d * 5 / 4 {
		qctx, cancel := context.WithTimeout(ctx, d)
		p, err := f.s.Grep(qctx, format.GrepQuery{Pattern: "upload number", Fixed: true, Limit: 500}, format.Filters{})
		cancel()
		if err != nil {
			t.Fatalf("budget %s: %v", d, err)
		}
		if !p.Truncated {
			break
		}
		if len(p.Hits) > 0 {
			partial++
			if p.Hits[0].Address == "" {
				t.Fatalf("budget %s: a partial hit without an address", d)
			}
		}
	}
	t.Logf("%d partial pages with hits", partial)
}

// deadlineOnly carries a deadline but is never cancelled, so only the
// statement_timeout set from the budget can stop a query under it.
type deadlineOnly struct {
	context.Context
	at time.Time
}

func (d deadlineOnly) Deadline() (time.Time, bool) { return d.at, true }

// sessions, read and raw statements run under the budget's
// statement_timeout, as grep's and search's do, and a spent budget says
// it timed out.
func TestSessionsReadRawStatementTimeout(t *testing.T) {
	f := newFindFixture(t)
	f.add("user", "hello")
	ctx := context.Background()
	// Each call waits on a lock that a slow statement holds for 4s.
	locked := func() func() {
		t.Helper()
		lctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		lock, err := f.s.Pool.Begin(lctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := lock.Exec(lctx, `LOCK TABLE conversations, sources IN ACCESS EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { lock.Exec(lctx, `SELECT pg_sleep(4)`); close(done) }()
		return func() { cancel(); <-done; lock.Rollback(ctx) }
	}
	for name, call := range map[string]func(context.Context) error{
		"sessions": func(c context.Context) error { _, err := f.s.Sessions(c, "", 0, format.Filters{}); return err },
		"read": func(c context.Context) error {
			_, err := f.s.Read(c, "", format.ReadQuery{Address: "sess-1/1"}, format.Filters{})
			return err
		},
		"raw at": func(c context.Context) error { _, _, err := f.s.RawAt(c, "", "sess-1/1"); return err },
		"raw": func(c context.Context) error {
			_, _, err := f.s.Raw(c, uuid.NewString(), 0, 0, 10)
			return err
		},
	} {
		release := locked()
		start := time.Now()
		err := call(deadlineOnly{ctx, time.Now().Add(300 * time.Millisecond)})
		took := time.Since(start)
		release()
		if took > 3*time.Second || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "timed out after") {
			t.Errorf("%s: %v after %s", name, err, took)
		}
	}
}

// The archive fetch that finishes a raw read runs under the same budget:
// its statement_timeout is a timed-out error too, not a bare Postgres
// error that the API answers with 500.
func TestRawFetchStatementTimeout(t *testing.T) {
	f := newFindFixture(t)
	ctx := context.Background()
	var device string
	if err := f.s.Pool.QueryRow(ctx, `SELECT id::text FROM devices LIMIT 1`).Scan(&device); err != nil {
		t.Fatal(err)
	}
	src := uuid.NewString()
	if _, err := f.s.Pool.Exec(ctx, `INSERT INTO sources(id,device_id,agent,path,file_id,storage_kind,parser,first_seen_at) VALUES($1,$2,'claude','/x.jsonl','f','jsonl_append','test',now())`, src, device); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Pool.Exec(ctx, `INSERT INTO generations(source_id,generation,size,captured_at,complete) VALUES($1,0,100,now(),true)`, src); err != nil {
		t.Fatal(err)
	}
	lctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	lock, err := f.s.Pool.Begin(lctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Exec(lctx, `LOCK TABLE generations IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { lock.Exec(lctx, `SELECT pg_sleep(4)`); close(done) }()
	start := time.Now()
	_, _, err = f.s.Raw(deadlineOnly{ctx, time.Now().Add(300 * time.Millisecond)}, src, 0, 0, 10)
	took := time.Since(start)
	cancel()
	<-done
	lock.Rollback(ctx)
	if took > 3*time.Second || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v after %s", err, took)
	}
}
