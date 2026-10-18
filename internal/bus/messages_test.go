package bus_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
)

func TestMessagesAuthorityAndHistory(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, b, c := f.device(f.user("author")), f.device(f.user("recipient")), f.device(f.user("observer"))
	a2 := f.device(a.UserID)
	for i, d := range []busproto.Caller{a, b, c} {
		p := live([]string{"author-1111", "recipient-1111", "observer-1111"}[i], "codex", "/clones/api", true)
		p.Remote = "github.test/team/api"
		p.Main = "/clones/api"
		f.present(d, p)
	}
	held := f.mustSend(a, "author-1111", "recipient-1111", "secret held")
	q := busproto.MessagesQuery{Repo: "team/api", Remotes: []string{"github.test/team/api"}}
	get := func(caller busproto.Caller, q busproto.MessagesQuery) busproto.MessagesResponse {
		t.Helper()
		out, err := f.s.Messages(ctx, caller, q)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := get(b, q); len(out.Threads) != 0 {
		t.Fatalf("unaccepted incoming leaked: %+v", out)
	}
	if out := get(a2, q); len(out.Threads) != 1 || out.Threads[0].State != busproto.StateHeld {
		t.Fatalf("cross-device authored held missing: %+v", out)
	}
	if _, err := f.s.Messages(ctx, c, busproto.MessagesQuery{Thread: held.ThreadID, Remotes: q.Remotes}); code(err) != busproto.CodeNotFound {
		t.Fatalf("unauthorized thread: %v", err)
	}
	if _, err := f.s.Accept(ctx, b, "author"); err != nil {
		t.Fatal(err)
	}
	if out := get(b, q); len(out.Threads) != 1 {
		t.Fatalf("accepted incoming missing: %+v", out)
	}
	if _, err := f.s.Accept(ctx, c, "author"); err != nil {
		t.Fatal(err)
	}
	if out := get(c, q); len(out.Threads) != 0 {
		t.Fatal("one participant acceptance exposed third-party thread")
	}
	f.exec(`UPDATE users SET role='admin' WHERE id=$1`, c.UserID)
	if out := get(c, q); len(out.Threads) != 0 {
		t.Fatal("admin bypasses missing participant acceptance")
	}
	if _, err := f.s.Accept(ctx, c, "recipient"); err != nil {
		t.Fatal(err)
	}
	out := get(c, q)
	if len(out.Threads) != 1 || out.Threads[0].Direction != "observed" {
		t.Fatalf("team thread: %+v", out)
	}
	f.exec(`UPDATE users SET role='admin' WHERE id=$1`, c.UserID)
	if _, err := f.s.Revoke(ctx, c, "recipient"); err != nil {
		t.Fatal(err)
	}
	if out := get(c, q); len(out.Threads) != 0 {
		t.Fatal("admin bypass or stale acceptance")
	}
	// Authorized root does not authorize an incoming held row in the same thread.
	reply := f.mustSend(b, "recipient-1111", "author-1111", "hidden reply", replyTo(held.ID))
	tq := q
	tq.Thread = held.ThreadID
	if out := get(a, tq); len(out.Messages) != 1 || strings.Contains(out.Messages[0].Body, "hidden") {
		t.Fatalf("per-row boundary: %+v", out)
	}
	f.exec(`UPDATE bus_messages SET state='refused',reason='synthetic' WHERE id=$1`, reply.ID)
	if out := get(a, tq); len(out.Messages) != 1 {
		t.Fatal("refused incoming leaked")
	}
	f.exec(`UPDATE bus_messages SET state='expired' WHERE id=$1`, reply.ID)
	if out := get(a, tq); len(out.Messages) != 1 {
		t.Fatal("expired unaccepted incoming leaked")
	}
	// Reading must not mutate delivery, receipts, attempts or seq.
	var before, after []byte
	if err := f.pool.QueryRow(ctx, `SELECT json_agg(m ORDER BY id)::text FROM bus_messages m`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	get(a2, q)
	get(a, tq)
	if err := f.pool.QueryRow(ctx, `SELECT json_agg(m ORDER BY id)::text FROM bus_messages m`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("read mutated messages")
	}
	if _, err := f.s.Revoke(ctx, b, "author"); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE bus_messages SET state='delivered',delivered_at=now() WHERE id=$1`, held.ID)
	if out := get(b, tq); len(out.Messages) != 1 || out.Messages[0].ID != reply.ID {
		t.Fatalf("revoked delivered content visible: %+v", out)
	}
	if out := get(b, q); len(out.Threads) != 1 || out.Threads[0].ID != reply.ID {
		t.Fatalf("only caller-authored reply should remain: %+v", out)
	}
}

func TestMessagesPaginationFiltersAndRetention(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.device(f.user("alice"))
	b := f.device(a.UserID)
	p := live("alice-1111", "codex", "/one/api", true)
	p.Remote = "github.test/one/api"
	p.Main = "/one/api"
	f.present(a, p)
	f.present(b, live("other-1111", "codex", "/else/api", true))
	root := f.mustSend(a, p.SessionID, "other-1111", "first line\nfull body")
	reply := f.mustSend(a, p.SessionID, "other-1111", "second", replyTo(root.ID))
	other := f.mustSend(a, p.SessionID, "other-1111", "third")
	// Equal timestamps deliberately force id tie-breaking in both page modes.
	q := busproto.MessagesQuery{Remotes: []string{p.Remote}, Limit: 1}
	first, err := f.s.Messages(ctx, b, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Threads) != 1 || !first.More || first.Next == "" {
		t.Fatalf("thread page: %+v", first)
	}
	q.Before = first.Next
	second, err := f.s.Messages(ctx, b, q)
	if err != nil || len(second.Threads) != 1 || second.Threads[0].ThreadID == first.Threads[0].ThreadID || second.More {
		t.Fatalf("thread keyset: %+v %v", second, err)
	}
	q.Before = ""
	// Caller authored previews remain bounded and participants cover both sides.
	allQ := q
	allQ.Limit = 200
	all, err := f.s.Messages(ctx, b, allQ)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range all.Threads {
		if len(it.Participants) != 2 || strings.Contains(it.Body, "full body") {
			t.Fatalf("preview/attribution: %+v", it)
		}
	}
	q.Thread = root.ThreadID
	one, err := f.s.Messages(ctx, b, q)
	if err != nil || !one.More {
		t.Fatalf("message page: %+v %v", one, err)
	}
	q.Before = one.Next
	two, err := f.s.Messages(ctx, b, q)
	if err != nil || len(two.Messages) != 1 || two.Messages[0].ID == one.Messages[0].ID || two.More {
		t.Fatalf("message keyset: %+v %v", two, err)
	}
	q.Before = ""
	q.Limit = 200
	f.exec(`DELETE FROM bus_messages WHERE id=$1`, root.ID)
	out, err := f.s.Messages(ctx, b, q)
	if err != nil || len(out.Messages) != 1 || out.Messages[0].ID != reply.ID || out.Messages[0].ReplyTo != "" {
		t.Fatalf("retained child: %+v %v", out, err)
	}
	q.Thread = ""
	q.Session = "other"
	q.User = "alice"
	q.Since = f.now.Format(time.RFC3339Nano)
	q.Until = f.now.Add(time.Second).Format(time.RFC3339Nano)
	out, err = f.s.Messages(ctx, b, q)
	if err != nil || len(out.Threads) != 2 {
		t.Fatalf("participants and inclusive time: %+v %v", out, err)
	}
	q.Since = ""
	q.Until = f.now.Format(time.RFC3339Nano)
	out, err = f.s.Messages(ctx, b, q)
	if err != nil || len(out.Threads) != 0 {
		t.Fatal("until must exclude equal time")
	}
	q.Until = ""
	q.Remotes = []string{"github.test/unrelated/api"}
	out, err = f.s.Messages(ctx, b, q)
	if err != nil || len(out.Threads) != 0 {
		t.Fatal("same-name remote matched")
	}
	q.Remotes = []string{p.Remote}
	f.exec(`UPDATE bus_messages SET from_remote='',from_main='' WHERE id=$1`, other.ID)
	out, err = f.s.Messages(ctx, b, q)
	if err != nil || len(out.Threads) != 1 {
		t.Fatal("unknown old attribution must not match remote")
	}
	q.Roots = []string{p.Repo}
	q.Remotes = nil
	out, err = f.s.Messages(ctx, a, q)
	if err != nil || len(out.Threads) != 2 {
		t.Fatalf("provable recorded path: %+v %v", out, err)
	}
	raw, _ := json.Marshal(out)
	if len(raw) > 24000 {
		t.Fatal("output bound")
	}
}

func TestMessagesSendTimeRepoIdentityAndTimes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.device(f.user("alice"))
	b := f.device(a.UserID)
	p := live("alice-1111", "codex", "/same/api", true)
	p.Remote = "github.test/right/api"
	p.Main = "/same/api"
	f.present(a, p)
	bp := live("other-1111", "codex", "/other/api", true)
	bp.Remote = p.Remote
	f.present(b, bp)
	root := f.mustSend(a, p.SessionID, bp.SessionID, "first")
	f.advance(time.Second)
	reply := f.mustSend(b, bp.SessionID, p.SessionID, "later reply", replyTo(root.ID))
	q := busproto.MessagesQuery{Repo: p.Repo, Roots: []string{p.Repo}, Remotes: []string{p.Remote}, Until: f.now.Format(time.RFC3339Nano)}
	out, err := f.s.Messages(ctx, a, q)
	if err != nil || len(out.Threads) != 0 {
		t.Fatalf("list until applies to latest activity: %+v %v", out, err)
	}
	q.Thread = root.ThreadID
	out, err = f.s.Messages(ctx, a, q)
	if err != nil || len(out.Messages) != 1 || out.Messages[0].ID != root.ID {
		t.Fatalf("open until applies per message: %+v %v", out, err)
	}
	q.Until = ""
	q.Thread = ""
	// A later placement cannot rewrite send-time repository attribution.
	p.Remote = "github.test/wrong/api"
	f.present(a, p)
	f.mustSend(a, p.SessionID, bp.SessionID, "other repo same path")
	out, err = f.s.Messages(ctx, a, q)
	if err != nil || len(out.Threads) != 1 || out.Threads[0].ID != reply.ID {
		t.Fatalf("current placement or same path changed attribution: %+v %v", out, err)
	}
	q.Thread = root.ThreadID
	q.Before = "1970-01-01T00:00:00Z|none"
	out, err = f.s.Messages(ctx, a, q)
	if err != nil || len(out.Messages) != 0 {
		t.Fatalf("authorized exhausted page: %+v %v", out, err)
	}
	q.Thread = "unknown"
	if _, err = f.s.Messages(ctx, a, q); code(err) != busproto.CodeNotFound {
		t.Fatalf("unknown paged thread: %v", err)
	}
	// Unknown historical identity cannot be guessed from current presence.
	f.exec(`UPDATE bus_messages SET from_remote='',from_main='' WHERE id=$1`, root.ID)
	q.Thread = ""
	q.Before = ""
	q.Remotes = []string{"github.test/wrong/api"}
	q.Roots = nil
	out, err = f.s.Messages(ctx, a, q)
	if err != nil || len(out.Threads) != 1 {
		t.Fatalf("backfilled unknown root: %+v %v", out, err)
	}
}

func TestMessagesMixedAuthorityCrossRepoAndHiddenParent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, b, c, d := f.device(f.user("author")), f.device(f.user("recipient")), f.device(f.user("observer")), f.device(f.user("secret"))
	for i, caller := range []busproto.Caller{a, b, c, d} {
		p := live([]string{"author-1111", "recipient-1111", "observer-1111", "secret-1111"}[i], "codex", "/repo", true)
		p.Remote = "host/team/repo"
		f.present(caller, p)
	}
	for _, pair := range []struct {
		caller busproto.Caller
		user   string
	}{{b, "author"}, {c, "author"}, {c, "recipient"}} {
		if _, err := f.s.Accept(ctx, pair.caller, pair.user); err != nil {
			t.Fatal(err)
		}
	}
	root := f.mustSend(a, "author-1111", "recipient-1111", "original visible anchor")
	f.advance(time.Second)
	// A hidden parent in the same thread must not appear in links or attribution.
	hidden := f.mustSend(d, "secret-1111", "author-1111", "secret body")
	f.exec(`UPDATE bus_messages SET thread_id=$1 WHERE id=$2`, root.ThreadID, hidden.ID)
	p := live("author-1111", "codex", "/other", true)
	p.Remote = "host/other/repo"
	f.present(a, p)
	child := f.mustSend(a, "author-1111", "recipient-1111", "latest elsewhere", replyTo(root.ID))
	f.exec(`UPDATE bus_messages SET reply_to=$1 WHERE id=$2`, hidden.ID, child.ID)
	q := busproto.MessagesQuery{Remotes: []string{"host/team/repo"}}
	out, err := f.s.Messages(ctx, c, q)
	if err != nil || len(out.Threads) != 1 || out.Threads[0].ID != child.ID || out.Threads[0].ReplyTo != "" || len(out.Threads[0].Participants) != 2 {
		t.Fatalf("cross-repo and authority: %+v %v", out, err)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "secret") {
		t.Fatalf("hidden identity: %s", raw)
	}
	// Hidden participant filters must not select an otherwise visible thread.
	q.User = "secret"
	out, err = f.s.Messages(ctx, c, q)
	if err != nil || len(out.Threads) != 0 {
		t.Fatalf("hidden user filter: %+v %v", out, err)
	}
	q.User = ""
	q.Session = "secret"
	out, err = f.s.Messages(ctx, c, q)
	if err != nil || len(out.Threads) != 0 {
		t.Fatalf("hidden session filter: %+v %v", out, err)
	}
	q.Session = ""
	q.Thread = root.ThreadID
	out, err = f.s.Messages(ctx, c, q)
	if err != nil || len(out.Messages) != 2 || out.Messages[0].ReplyTo != "" {
		t.Fatalf("full rows: %+v %v", out, err)
	}
	// Use another root to exercise Preview on the unmodified stored body.
	p.Remote = "host/team/repo"
	f.present(a, p)
	preview := f.mustSend(a, "author-1111", "recipient-1111", "\n\n\x1b[31mVisible\u202e update")
	q.Thread = ""
	out, err = f.s.Messages(ctx, c, q)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range out.Threads {
		if it.ID == preview.ID {
			found = true
			if it.Body != busproto.Preview("\n\n\x1b[31mVisible\u202e update") {
				t.Fatalf("preview: %q", it.Body)
			}
		}
	}
	if !found {
		t.Fatal("preview row missing")
	}
}

func TestMessagesUploadedCheckoutSnapshotAndDeviceBoundary(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.device(f.user("alice"))
	b := f.device(a.UserID)
	f.uploaded(a, "codex", "uploaded-1111", "/main/.worktrees/sibling")
	f.uploaded(b, "codex", "other-1111", "/else")
	source := "12345678-1234-1234-1234-123456789abc"
	f.exec(`INSERT INTO sources(id,device_id,agent,path,file_id,storage_kind,parser,first_seen_at,checkout) VALUES($1,$2,'codex','/transcript','file','jsonl_append','codex',now(),'/main')`, source, a.DeviceID)
	f.exec(`UPDATE conversations SET source_id=$1 WHERE session_id='uploaded-1111'`, source)
	row := f.mustSend(a, "uploaded-1111", "other-1111", "uploaded only")
	f.exec(`UPDATE sources SET checkout='/changed',remote='host/changed/repo' WHERE id=$1`, source)
	q := busproto.MessagesQuery{Mains: []string{"/main"}}
	out, err := f.s.Messages(ctx, a, q)
	if err != nil || len(out.Threads) != 1 || out.Threads[0].ID != row.ID || out.Threads[0].Main != "/main" || out.Threads[0].Remote != "" {
		t.Fatalf("immutable uploaded checkout: %+v %v", out, err)
	}
	out, err = f.s.Messages(ctx, b, q)
	if err != nil || len(out.Threads) != 0 {
		t.Fatalf("same main on other device: %+v %v", out, err)
	}
	q.Mains = []string{"/changed"}
	q.Remotes = []string{"host/changed/repo"}
	out, err = f.s.Messages(ctx, a, q)
	if err != nil || len(out.Threads) != 0 {
		t.Fatalf("mutable source rewrote snapshot: %+v %v", out, err)
	}
}

func TestMessagesDatabaseBudgetPagingAndParticipantDiscovery(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.device(f.user("alice"))
	b := f.device(a.UserID)
	p := live("alice-1111", "codex", "/repo", true)
	p.Remote = "host/team/repo"
	f.present(a, p)
	f.present(b, live("other-1111", "codex", "/else", true))
	root := f.mustSend(a, "alice-1111", "other-1111", "root")
	// One thread with 400 eligible participant sessions exceeds MCP's 24k
	// bound without a bounded summary. Full rows still retain attribution.
	f.exec(`INSERT INTO bus_messages(id,thread_id,from_user,from_device,from_agent,from_session,to_user,to_agent,to_session,addressed,sender,intent,body,body_sha,state,created_at,expires_at,from_remote)
 SELECT 'many-'||i,$1,$2,$3,'codex','participant-'||i,$2,'codex','other-1111','session','own','inform','body',sha256('body'::bytea),'queued',$4,$5,'host/team/repo' FROM generate_series(1,400) i`, root.ThreadID, a.UserID, a.DeviceID, f.now, f.now.Add(time.Hour))
	var unbounded []byte
	if err := f.pool.QueryRow(ctx, `SELECT jsonb_agg(p) FROM (
 SELECT DISTINCT from_user::text AS user_id,'alice@example.test'::text AS "user",from_session AS session,from_agent AS agent FROM bus_messages WHERE thread_id=$1
 ) p`, root.ThreadID).Scan(&unbounded); err != nil {
		t.Fatal(err)
	}
	if len(unbounded) <= 24000 {
		t.Fatalf("fixture must reproduce a participant-only row exceeding MCP budget: %d", len(unbounded))
	}
	q := busproto.MessagesQuery{Remotes: []string{p.Remote}, MaxBytes: 24000}
	out, err := f.s.Messages(ctx, a, q)
	if err != nil || len(out.Threads) != 1 || out.Threads[0].ParticipantsRemaining < 380 || out.Threads[0].ThreadID != root.ThreadID {
		t.Fatalf("discoverable crowded thread: %+v %v", out, err)
	}
	q.Thread = root.ThreadID
	q.Limit = 200
	q.MaxBytes = 3000
	ids := map[string]bool{}
	for {
		out, err = f.s.Messages(ctx, a, q)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(out)
		if len(raw)+1 > q.MaxBytes {
			t.Fatal("database budget exceeded")
		}
		for _, it := range out.Messages {
			if ids[it.ID] {
				t.Fatalf("duplicate cursor row %s", it.ID)
			}
			ids[it.ID] = true
			if it.From == "" {
				t.Fatal("full attribution omitted")
			}
		}
		if !out.More {
			break
		}
		if out.Next == "" {
			t.Fatal("missing continuation")
		}
		q.Before = out.Next
	}
	if len(ids) != 401 {
		t.Fatalf("equal-time paging lost rows: %d", len(ids))
	}
	for _, bad := range []busproto.MessagesQuery{{Since: "bad"}, {Until: "bad"}, {Before: "2026-01-01T00:00:00Z|"}, {Since: "2026-01-02T00:00:00Z", Until: "2026-01-01T00:00:00Z"}} {
		bad.Remotes = q.Remotes
		if _, err = f.s.Messages(ctx, a, bad); err == nil {
			t.Fatalf("invalid time accepted: %+v", bad)
		}
	}
}
