package retrieval_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/google/uuid"
)

// The team server answers the retrieval-gap features with the local
// index's semantics: stored digests and branches, the branch filter,
// sort orders, -o and -U, the outline, and live sessions from the
// device's reports.
func TestServerDigestsBranchesSortOutlineLive(t *testing.T) {
	ctx := context.Background()
	const held = "019a0000-0000-7000-8000-0000000000a1" // the Codex health-check session
	s := newServerWith(t, devicesync.ChunkParams{Min: 1 << 10, Avg: 4 << 10, Max: 16 << 10}, func() []string { return []string{held} })
	s.ingestFixtures()
	c := s.client

	// Digests and branches, stored at parse time.
	out, err := c.Sessions(ctx, "", 0, format.Filters{Branch: "fix/*", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var top *format.ConversationInfo
	for i := range out.Sessions {
		if out.Sessions[i].SessionID == "0b7e2c1a-0000-4000-8000-000000000002" {
			top = &out.Sessions[i]
		}
		if fmt.Sprint(out.Sessions[i].Branches) != "[fix/retry]" {
			t.Errorf("branch filter let in %s %v", out.Sessions[i].SessionID, out.Sessions[i].Branches)
		}
	}
	if top == nil || top.Digest == nil || top.Digest.Intent != "the upload retry test is flaky; find out why" || top.Digest.Subagents != 2 ||
		top.Digest.Failed != 1 || top.Digest.Tokens == nil || top.Digest.Tokens.Output != 850 || top.Digest.State != nil {
		t.Fatalf("digest: %+v", top)
	}
	var codexDigest string
	if err := s.pool.QueryRow(ctx, `SELECT digest::text FROM conversations WHERE session_id=$1`, held).Scan(&codexDigest); err != nil ||
		!strings.Contains(codexDigest, `"files_edited": ["internal/api/server.go", "internal/api/health.go"]`) {
		t.Fatalf("codex digest: %s %v", codexDigest, err)
	}

	// Sort orders, -o, -U, and the grouped layout's session info.
	grep := func(q format.GrepQuery, f format.Filters) *format.Page {
		t.Helper()
		p, err := c.Grep(ctx, q, f)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	newest := grep(format.GrepQuery{Pattern: "retr", Limit: 3}, format.Filters{})
	oldest := grep(format.GrepQuery{Pattern: "retr", Limit: 3}, format.Filters{Sort: format.SortOldest})
	if len(newest.Hits) != 3 || len(oldest.Hits) != 3 || !newest.Hits[0].TS.After(*oldest.Hits[0].TS) && !newest.Hits[0].TS.Equal(*oldest.Hits[0].TS) ||
		len(newest.SessionInfo) == 0 || newest.SessionInfo[0].Digest == nil {
		t.Fatalf("sort: newest %v oldest %v", newest.Hits, oldest.Hits)
	}
	if oldest.Hits[0].Address == newest.Hits[0].Address {
		t.Fatalf("oldest and newest start alike: %s", oldest.Hits[0].Address)
	}
	rel := grep(format.GrepQuery{Pattern: "retr", Limit: 1}, format.Filters{Sort: format.SortRelevance})
	if len(rel.Hits) != 1 || !strings.Contains(rel.Hits[0].Lines[0].Text, "retries") || !strings.Contains(rel.Hits[0].Lines[0].Text, "retry") {
		t.Fatalf("relevance: %+v", rel.Hits)
	}
	om := grep(format.GrepQuery{Pattern: `retr\w*`, OnlyMatching: true, Limit: 5}, format.Filters{})
	for _, h := range om.Hits {
		for _, l := range h.Lines {
			if !strings.HasPrefix(l.Text, "retr") || strings.Contains(l.Text, " ") {
				t.Fatalf("-o line %q", l.Text)
			}
		}
	}
	ml := grep(format.GrepQuery{Pattern: `upload\.test\.ts:9 timeout\n.*exit`, Multiline: true}, format.Filters{})
	if len(ml.Hits) != 1 || len(ml.Hits[0].Lines) != 2 {
		t.Fatalf("-U: %+v", ml.Hits)
	}
	if p, err := c.Search(ctx, format.SearchQuery{Query: "retry", Limit: 2}, format.Filters{Sort: format.SortNewest}); err != nil || len(p.Hits) != 2 ||
		p.Hits[0].TS.Before(*p.Hits[1].TS) {
		t.Fatalf("search newest: %v %+v", err, p)
	}

	// The outline: prompts and calls, failed calls, spawned subagents.
	cx, err := c.Read(ctx, format.ReadQuery{Address: "0b7e2c1a-0000-4000-8000-000000000002", Outline: true}, format.Filters{})
	if err != nil {
		t.Fatal(err)
	}
	var failed, spawned int
	for _, e := range cx.Outline {
		if e.Error {
			failed++
		}
		spawned += len(e.Subagents)
	}
	if cx.OutlineTotal != 10 || len(cx.Outline) != 10 || failed != 1 || spawned != 2 || cx.Outline[0].Kind != "user" {
		t.Fatalf("outline: total %d failed %d spawned %d %+v", cx.OutlineTotal, failed, spawned, cx.Outline)
	}

	// Live: the device reported the held session. Active 30 minutes ago,
	// it is live by the report; another session active 5 minutes ago is
	// live by recency.
	if _, err := s.pool.Exec(ctx, `UPDATE conversations SET last_activity_at=now()-interval '30 minutes' WHERE session_id=$1`, held); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE conversations SET last_activity_at=now()-interval '5 minutes' WHERE session_id='019a0000-0000-7000-8000-0000000000a2'`); err != nil {
		t.Fatal(err)
	}
	all, err := c.Sessions(ctx, "", 0, format.Filters{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	live := map[string]bool{}
	for _, x := range all.Sessions {
		if x.Live {
			live[x.SessionID] = true
		}
	}
	if len(live) != 2 || !live[held] || !live["019a0000-0000-7000-8000-0000000000a2"] {
		t.Fatalf("live sessions %v", live)
	}
	rest, err := c.Sessions(ctx, "", 0, format.Filters{Limit: 100, ExcludeLive: true})
	// The two live ones go; a2's subagent a5, idle since July, stays (a
	// subagent goes with its parent only when the harness holds it open).
	if err != nil || rest.Total != all.Total-2 {
		t.Fatalf("exclude live: %v %d of %d", err, rest.Total, all.Total)
	}
	for _, x := range rest.Sessions {
		if x.Live || x.SessionID == held {
			t.Fatalf("live session listed: %s", x.SessionID)
		}
	}
}

// Review fixes: session self matches only the caller's own session by
// its exact id; --exclude-live leaves out a reported session only while
// it wrote within the hour (as the live label has it); --branch ignores
// case as the local index does.
func TestServerSelfLiveBranchRules(t *testing.T) {
	ctx := context.Background()
	const held = "019a0000-0000-7000-8000-0000000000a1"
	s := newServerWith(t, devicesync.ChunkParams{Min: 1 << 10, Avg: 4 << 10, Max: 16 << 10}, func() []string { return []string{held} })
	s.ingestFixtures()
	c := s.client

	// Another user's conversation with the id the caller names as self.
	const other = "019a0000-0000-7000-8000-0000000000a3"
	user, device := uuid.NewString(), uuid.NewString()
	for _, q := range []string{
		`INSERT INTO users(id,email,name,role,identity_type,created_at) VALUES($1,'eve@example.test','Eve','member','human',now()) RETURNING $2::text`,
		`INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($2,$1,'laptop-e','darwin',now())`,
		`UPDATE conversations SET user_id=$1,device_id=$2 WHERE session_id='` + other + `'`,
	} {
		if _, err := s.pool.Exec(ctx, q, user, device); err != nil {
			t.Fatal(err)
		}
	}
	if p, err := c.Grep(ctx, format.GrepQuery{Pattern: "tests"}, format.Filters{Session: other}); err != nil || len(p.Hits) == 0 {
		t.Fatalf("the other user's session is searchable by name: %v %+v", err, p)
	}
	if p, err := c.Grep(ctx, format.GrepQuery{Pattern: "tests"}, format.Filters{Session: other, Self: true}); err != nil || len(p.Hits) != 0 {
		t.Fatalf("session self reached another user's session: %v %+v", err, p.Hits)
	}
	if _, err := c.Read(ctx, format.ReadQuery{Address: other, Self: true}, format.Filters{}); err == nil {
		t.Fatal("read self reached another user's session")
	}
	// A prefix of the caller's own session is not the session.
	if p, err := c.Grep(ctx, format.GrepQuery{Pattern: "health"}, format.Filters{Session: held[:30], Self: true}); err != nil || len(p.Hits) != 0 {
		t.Fatalf("session self matched a prefix: %v %+v", err, p.Hits)
	}
	if p, err := c.Grep(ctx, format.GrepQuery{Pattern: "health"}, format.Filters{Session: held, Self: true}); err != nil || len(p.Hits) == 0 {
		t.Fatalf("session self: %v %+v", err, p)
	}

	// Reported open but idle for two hours: neither live nor left out.
	if _, err := s.pool.Exec(ctx, `UPDATE conversations SET last_activity_at=now()-interval '2 hours' WHERE session_id=$1`, held); err != nil {
		t.Fatal(err)
	}
	listed := func(f format.Filters) *format.ConversationInfo {
		t.Helper()
		f.Limit = 100
		out, err := c.Sessions(ctx, "", 0, f)
		if err != nil {
			t.Fatal(err)
		}
		for i := range out.Sessions {
			if out.Sessions[i].SessionID == held {
				return &out.Sessions[i]
			}
		}
		return nil
	}
	if x := listed(format.Filters{}); x == nil || x.Live {
		t.Fatalf("idle held session: %+v", x)
	}
	if listed(format.Filters{ExcludeLive: true}) == nil {
		t.Fatal("--exclude-live left out a held session idle for two hours")
	}
	if listed(format.Filters{ExcludeLive: true, Live: []string{held}}) == nil {
		t.Fatal("--exclude-live left out a caller-reported session idle for two hours")
	}
	// An empty id in the list names no session.
	if listed(format.Filters{ExcludeLive: true, Live: []string{""}}) == nil {
		t.Fatal("an empty live id left out every top-level session")
	}

	// Branch globs ignore case, as the local index's LIKE does.
	if out, err := c.Sessions(ctx, "", 0, format.Filters{Branch: "FIX/*", Limit: 50}); err != nil || len(out.Sessions) == 0 {
		t.Fatalf("--branch FIX/*: %v %+v", err, out)
	}
}
