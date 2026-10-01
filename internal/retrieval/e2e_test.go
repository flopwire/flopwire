package retrieval_test

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/api"
	"github.com/flopwire/flopwire/internal/auth"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/ingest"
	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/flopwire/flopwire/internal/retrieval"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/flopwire/flopwire/internal/transcript/codex"
	"github.com/flopwire/flopwire/internal/transcript/devin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// server is a real Flopwire server (sync, parse, retrieval) with one member
// and one device, and the device's sync agent.
type server struct {
	t      *testing.T
	pool   *pgxpool.Pool
	queue  *ingest.Queue
	client client.HTTP // the member's device credential
	sy     *devicesync.Syncer
	home   string
	// objects and store reach the archive and the deletion worker.
	objects ingest.MinIO
	store   *store.Postgres
	userID  string
}

func newServer(t *testing.T) *server {
	return newServerChunks(t, devicesync.ChunkParams{Min: 1 << 10, Avg: 4 << 10, Max: 16 << 10})
}

func newServerChunks(t *testing.T, chunks devicesync.ChunkParams) *server {
	return newServerWith(t, chunks, nil)
}

// newServerWith is newServer whose device reports live sessions with
// every flush.
func newServerWith(t *testing.T, chunks devicesync.ChunkParams, live func() []string) *server {
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
	mc, bucket := pgtest.NewBucket(t)
	objects := ingest.MinIO{Client: mc, Bucket: bucket}
	user, device := uuid.NewString(), uuid.NewString()
	plain, hash, _ := auth.NewToken()
	now := time.Now().UTC()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id,email,name,role,identity_type,created_at) VALUES($1,'gary@example.test','Gary','member','human',$2)`, []any{user, now}},
		{`INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$2,'laptop-a','darwin',$3)`, []any{device, user, now}},
		{`INSERT INTO credentials(id,user_id,device_id,kind,token_hash,created_at) VALUES($1,$2,$3,'device',$4,$5)`, []any{uuid.NewString(), user, device, hash, now}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &server{t: t, pool: pool, queue: &ingest.Queue{Pool: pool, Objects: objects, Log: log}, home: t.TempDir(),
		objects: objects, store: store.NewPostgres(pool, mc, bucket), userID: user}
	h := httptest.NewServer(api.New(s.store, api.Config{Logger: log,
		Sync:      &ingest.Server{Pool: pool, Objects: objects, Log: log, Queue: s.queue},
		Retrieval: &retrieval.Store{Pool: pool, Objects: objects, RefreshSession: s.queue.RefreshSession}}).Handler(nil))
	t.Cleanup(h.Close)
	s.client = client.HTTP{Server: h.URL, Token: plain}
	st, err := devicesync.OpenStore(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	spool, _ := devicesync.OpenSpool(filepath.Join(t.TempDir(), "spool"), 1<<30)
	s.sy, err = devicesync.NewSyncer(devicesync.Config{Chunk: chunks, Logger: log, Live: live},
		st, spool, &syncproto.Client{Server: h.URL, Token: plain, HTTP: h.Client()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.sy.Close)
	return s
}

// ingestFixtures syncs the oracle fixtures (Claude with companions, Codex,
// Devin) and parses them.
func (s *server) ingestFixtures() {
	t := s.t
	ctx := context.Background()
	if err := os.CopyFS(s.home, os.DirFS("../../testdata/oracle/home")); err != nil {
		t.Fatal(err)
	}
	var specs []devicesync.SourceSpec
	sessions, err := claude.Discover(filepath.Join(s.home, ".claude", "projects"))
	if err != nil {
		t.Fatal(err)
	}
	for _, sess := range sessions {
		for _, src := range sess.Sources() {
			specs = append(specs, devicesync.SourceSpec{Path: src.Path, Agent: transcript.AgentClaude, StorageKind: transcript.StorageJSONLAppend, Parser: claude.ParserName})
		}
		for _, c := range sess.Companions {
			parent := filepath.Join(sess.ProjectDir, sess.SessionID+".jsonl")
			if c.Role == claude.CompanionMeta {
				parent = strings.TrimSuffix(c.Path, ".meta.json") + ".jsonl"
			}
			specs = append(specs, devicesync.SourceSpec{Path: c.Path, Agent: transcript.AgentClaude, StorageKind: transcript.StorageCompanion, Parent: parent})
		}
	}
	rollouts, err := codex.Discover(filepath.Join(s.home, ".codex"))
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range rollouts {
		specs = append(specs, devicesync.SourceSpec{Path: src.Path, Agent: transcript.AgentCodex, StorageKind: transcript.StorageJSONLAppend, Parser: codex.Name})
	}
	for _, sp := range specs {
		if err := s.sy.Sync(ctx, sp); err != nil {
			t.Fatalf("sync %s: %v", sp.Path, err)
		}
	}
	seed, _ := os.ReadFile("../../testdata/oracle/seeds/devin.sql")
	dbPath := filepath.Join(s.home, "sessions.db")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(string(seed)); err != nil {
		t.Fatal(err)
	}
	ids, _ := devin.ListSessions(ctx, dbPath)
	for _, id := range ids {
		data, _ := devin.Export(ctx, dbPath, id)
		if err := s.sy.SyncExport(ctx, devicesync.SourceSpec{Path: devin.ExportPath(dbPath, id), Agent: transcript.AgentDevin,
			StorageKind: transcript.StorageSQLite, Parser: devin.ExportFormat}, data); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.queue.Drain(ctx); err != nil {
		t.Fatal(err)
	}
}

func (s *server) count(q string, args ...any) int {
	s.t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	return n
}

func texts(hits []format.Hit) string {
	var b strings.Builder
	_ = format.WriteSearch(&b, &format.Page{Hits: hits}, format.Style{Flat: true})
	return b.String()
}

// find runs grep through the client and returns its hits.
func find(t *testing.T, c client.HTTP, pattern string, regex, cs bool, f format.Filters) ([]format.Hit, error) {
	t.Helper()
	p, err := c.Grep(context.Background(), format.GrepQuery{Pattern: pattern, Fixed: !regex, CaseSensitive: cs}, f)
	if err != nil {
		return nil, err
	}
	return p.Hits, nil
}

func firstLine(h format.Hit) (int, string) {
	for _, l := range h.Lines {
		if l.Match {
			return l.N, l.Text
		}
	}
	return 0, ""
}

func TestRetrievalEndToEnd(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	s.ingestFixtures()
	c := s.client

	// Ranked search: hits with snippets and provenance.
	page, err := c.Search(ctx, format.SearchQuery{Query: "exponential backoff"}, format.Filters{})
	if err != nil || len(page.Hits) == 0 {
		t.Fatalf("search: %v %v", page, err)
	}
	hits := page.Hits
	top := hits[0]
	if !strings.Contains(strings.ToLower(top.Snippet), "backoff") || top.Score <= 0 || top.Agent != "claude" ||
		!strings.HasSuffix(top.Provenance.Path, ".jsonl") || top.Provenance.LineNo == 0 || top.Provenance.ByteOffset == nil || top.Device != "laptop-a" {
		t.Fatalf("search hit: %+v", top)
	}
	if out := texts(hits[:1]); !strings.HasPrefix(out, top.Address+":") || !strings.HasPrefix(top.SessionID, strings.Split(top.Address, "/")[0]) {
		t.Fatalf("text form: %q", out)
	}

	// Raw: the hit's provenance returns exactly its JSONL line.
	raw, err := c.Raw(ctx, top.Provenance.SourceID, top.Provenance.Generation, *top.Provenance.ByteOffset, top.Provenance.ByteLen)
	file, _ := os.ReadFile(top.Provenance.Path)
	want := file[*top.Provenance.ByteOffset : *top.Provenance.ByteOffset+top.Provenance.ByteLen]
	if err != nil || !bytes.Equal(raw, want) {
		t.Fatalf("raw: %q, want %q (%v)", raw, want, err)
	}

	// Find: substring (case-insensitive by default), case-sensitive, regex.
	if hits, err = find(t, c, "EXPONENTIAL BACKOFF", false, false, format.Filters{}); err != nil || len(hits) == 0 || len(hits[0].Lines) == 0 {
		t.Fatalf("find: %v %v", hits, err)
	}
	if hits, _ = find(t, c, "EXPONENTIAL BACKOFF", false, true, format.Filters{}); len(hits) != 0 {
		t.Fatalf("case-sensitive find matched %v", hits)
	}
	hits, err = find(t, c, `upload\.test\.ts:\d+`, true, false, format.Filters{Kinds: []string{"tool_result"}})
	if _, l := firstLine(hits[0]); err != nil || len(hits) != 1 || l != "upload.test.ts:9 timeout" || hits[0].Kind != "tool_result" {
		t.Fatalf("regex find: %+v %v", hits, err)
	}
	if _, err = find(t, c, `(`, true, false, format.Filters{}); err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "regex") {
		t.Fatalf("bad regex: %v", err)
	}

	// Filters: agent, subagents, time, self-session.
	for _, tc := range []struct {
		name string
		f    format.Filters
		want func(n int) bool
	}{
		{"agent codex", format.Filters{Agent: "codex"}, func(n int) bool { return n == 0 }},
		{"agent claude", format.Filters{Agent: "claude"}, func(n int) bool { return n > 0 }},
		{"future", format.Filters{Since: time.Now().Add(time.Hour)}, func(n int) bool { return n == 0 }},
		{"exclude session", format.Filters{ExcludeSession: top.SessionID}, func(n int) bool { return n < len(hits) || n == 0 }},
	} {
		got, err := find(t, c, "backoff", false, false, tc.f)
		if err != nil || !tc.want(len(got)) {
			t.Errorf("%s: %d hits, err %v", tc.name, len(got), err)
		}
	}
	all, _ := find(t, c, "retry", false, false, format.Filters{})
	main, _ := find(t, c, "retry", false, false, format.Filters{ExcludeSubagents: true})
	if len(main) >= len(all) {
		t.Errorf("exclude_subagents: %d of %d", len(main), len(all))
	}

	// Branches: the abandoned Devin retry is off the active path.
	if got, _ := find(t, c, "abandoned retry", false, false, format.Filters{}); len(got) != 0 {
		t.Fatalf("off-path row in the default view: %+v", got)
	}
	if got, _ := find(t, c, "abandoned retry", false, false, format.Filters{IncludeBranches: true}); len(got) != 1 || !got[0].OffPath {
		t.Fatalf("include_branches: %+v", got)
	}

	// Read: the hit's address; neighbours in conversation order, focus
	// included.
	cx, err := c.Read(ctx, format.ReadQuery{Address: top.Address, Before: 2, After: 2}, format.Filters{})
	if err != nil || cx.Focus != top.MessageID || len(cx.Messages) < 3 || cx.Conversation.ID != top.ConversationID {
		t.Fatalf("read: %+v %v", cx, err)
	}
	focus := -1
	for i, m := range cx.Messages {
		if i > 0 && m.Ordinal < cx.Messages[i-1].Ordinal {
			t.Fatal("context out of order")
		}
		if m.ID == top.MessageID {
			focus = i
		}
	}
	if focus < 0 || focus > 2 || len(cx.Messages)-focus-1 > 2 {
		t.Fatalf("focus at %d of %d", focus, len(cx.Messages))
	}

	// Sessions: the parent session and its subagents, which name it.
	ss, err := c.Sessions(ctx, "", 0, format.Filters{Limit: 100})
	if err != nil || ss.Total < 3 {
		t.Fatalf("sessions: %+v %v", ss, err)
	}
	subs := 0
	for _, x := range ss.Sessions {
		if x.ParentSession != "" && strings.HasPrefix("0b7e2c1a-0000-4000-8000-000000000002", x.ParentSession) {
			subs++
		}
	}
	if subs < 2 {
		t.Fatalf("sessions: %d subagents name their parent: %+v", subs, ss.Sessions)
	}
	// A session address reads the session from its start.
	if cx, err = c.Read(ctx, format.ReadQuery{Address: "0b7e2c1a-0000-4000-8000-000000000002"}, format.Filters{}); err != nil || cx.Messages[0].ID != cx.Focus || len(cx.Messages) < 5 {
		t.Fatalf("read session: %+v %v", cx, err)
	}

	// Superseded rows: a rewrite drops the tail of a rollout; its rows
	// leave the default view and stay findable with include_superseded.
	var path string
	_ = s.pool.QueryRow(ctx, `SELECT path FROM sources WHERE path LIKE '%c0de.jsonl'`).Scan(&path)
	lines := strings.SplitAfter(string(file0(t, path)), "\n")
	if err := os.WriteFile(path, []byte(strings.Join(lines[:2], "")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.sy.Sync(ctx, devicesync.SourceSpec{Path: path, Agent: transcript.AgentCodex, StorageKind: transcript.StorageJSONLAppend, Parser: codex.Name}); err != nil {
		t.Fatal(err)
	}
	if err := s.queue.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	dropped := s.count(`SELECT count(*) FROM messages m JOIN sources s ON s.id=m.source_id WHERE s.path=$1 AND m.superseded`, path)
	if dropped == 0 {
		t.Fatal("rewrite superseded nothing")
	}
	var word string
	_ = s.pool.QueryRow(ctx, `SELECT split_part(m.text,' ',1) FROM messages m JOIN sources s ON s.id=m.source_id
		WHERE s.path=$1 AND m.superseded AND length(split_part(m.text,' ',1))>3 LIMIT 1`, path).Scan(&word)
	live, _ := find(t, c, word, false, true, format.Filters{Agent: "codex"})
	wide, _ := find(t, c, word, false, true, format.Filters{Agent: "codex", IncludeSuperseded: true})
	if len(wide) <= len(live) {
		t.Fatalf("include_superseded %q: %d vs %d", word, len(wide), len(live))
	}

	// Every search, find, context, and raw read is audited, with the
	// query text and result ids.
	if n := s.count(`SELECT count(*) FROM audit_events WHERE action='search' AND metadata->>'query'='exponential backoff' AND jsonb_array_length(metadata->'result_ids')>0`); n != 1 {
		t.Fatalf("search audit events: %d", n)
	}
	if n := s.count(`SELECT count(*) FROM audit_events WHERE action='raw.read' AND target_id=$1 AND metadata->>'outcome'='success'`, top.Provenance.SourceID); n != 1 {
		t.Fatalf("raw audit events: %d", n)
	}
	if n := s.count(`SELECT count(*) FROM audit_events WHERE action='search' AND metadata->>'outcome'='failed'`); n == 0 {
		t.Fatal("failed search not audited")
	}
}

func file0(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// V8, D10: a device reads raw bytes back by its own path (no text search
// needed to find the source), and every raw answer names whose evidence
// it is.
func TestRawByPathAndAttribution(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	s.ingestFixtures()
	page, err := s.client.Search(ctx, format.SearchQuery{Query: "exponential backoff"}, format.Filters{})
	if err != nil || len(page.Hits) == 0 {
		t.Fatalf("search: %v", err)
	}
	top := page.Hits[0]
	file := file0(t, top.Provenance.Path)
	want := file[*top.Provenance.ByteOffset : *top.Provenance.ByteOffset+top.Provenance.ByteLen]
	get := func(q url.Values) (*http.Response, []byte) {
		t.Helper()
		req, _ := http.NewRequest("GET", s.client.Server+"/v1/raw?"+q.Encode(), nil)
		req.Header.Set("Authorization", "Bearer "+s.client.Token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res, body
	}
	off, n := strconv.FormatInt(*top.Provenance.ByteOffset, 10), strconv.FormatInt(top.Provenance.ByteLen, 10)
	for name, q := range map[string]url.Values{
		"by source": {"source_id": {top.Provenance.SourceID}, "generation": {strconv.FormatInt(top.Provenance.Generation, 10)}, "offset": {off}, "length": {n}},
		"by path":   {"path": {top.Provenance.Path}, "offset": {off}, "length": {n}},
	} {
		res, body := get(q)
		if res.StatusCode != 200 || !bytes.Equal(body, want) {
			t.Fatalf("%s: %d %q", name, res.StatusCode, body)
		}
		for h, v := range map[string]string{"User": "gary@example.test", "Device": "laptop-a", "Agent": "claude",
			"Session": top.SessionID, "Source-Id": top.Provenance.SourceID, "Path": top.Provenance.Path} {
			if got := res.Header.Get("X-Flopwire-" + h); got != v {
				t.Errorf("%s: X-Flopwire-%s = %q, want %q", name, h, got, v)
			}
		}
	}
	if res, _ := get(url.Values{"path": {"/nowhere.jsonl"}, "offset": {"0"}, "length": {"1"}}); res.StatusCode != 404 {
		t.Fatalf("unknown path: %d", res.StatusCode)
	}
}

// A conversation hidden by an admin path rule change (D18) is left out of
// search, grep, sessions, read and raw, as are its subagents; the rest
// stays found.
func TestHiddenConversationsLeftOut(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	s.ingestFixtures()
	c := s.client
	page, err := c.Search(ctx, format.SearchQuery{Query: "exponential backoff"}, format.Filters{})
	if err != nil || len(page.Hits) == 0 {
		t.Fatalf("search: %v %v", page, err)
	}
	top := page.Hits[0]
	before, err := c.Sessions(ctx, "", 0, format.Filters{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `WITH RECURSIVE t AS (SELECT id,session_id FROM conversations WHERE session_id=$1
			UNION SELECT c.id,c.session_id FROM conversations c JOIN t ON c.parent_conversation_id=t.id OR c.parent_native_session_id=t.session_id)
		UPDATE conversations SET hidden_at=now(),hidden_rule='/x',hidden_rules_version=1,hidden_root=(SELECT id FROM conversations WHERE session_id=$1 AND depth=0 LIMIT 1)
		WHERE id IN (SELECT id FROM t)`, top.SessionID); err != nil {
		t.Fatal(err)
	}
	hiddenN := s.count(`SELECT count(*) FROM conversations WHERE hidden_at IS NOT NULL`)
	if hiddenN == 0 {
		t.Fatal("nothing hidden")
	}
	page, err = c.Search(ctx, format.SearchQuery{Query: "exponential backoff"}, format.Filters{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range page.Hits {
		if h.SessionID == top.SessionID {
			t.Fatalf("search found the hidden session: %+v", h)
		}
	}
	hits, err := find(t, c, "backoff", false, false, format.Filters{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.SessionID == top.SessionID {
			t.Fatalf("grep found the hidden session: %+v", h)
		}
	}
	after, err := c.Sessions(ctx, "", 0, format.Filters{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Sessions) != len(before.Sessions)-hiddenN {
		t.Errorf("sessions: %d before, %d after hiding %d", len(before.Sessions), len(after.Sessions), hiddenN)
	}
	for _, ss := range after.Sessions {
		if ss.SessionID == top.SessionID {
			t.Fatal("sessions lists the hidden session")
		}
	}
	for _, addr := range []string{top.Address, top.SessionID, top.MessageID, top.Provenance.Path + ":" + strconv.FormatInt(top.Provenance.LineNo, 10)} {
		if _, err := c.Read(ctx, format.ReadQuery{Address: addr}, format.Filters{}); err == nil || !strings.Contains(err.Error(), "404") {
			t.Errorf("read %s of a hidden session: %v", addr, err)
		}
		if _, err := c.RawAt(ctx, addr); err == nil {
			t.Errorf("raw at %s of a hidden session: %v", addr, err)
		}
	}
	if _, err := c.Raw(ctx, top.Provenance.SourceID, top.Provenance.Generation, *top.Provenance.ByteOffset, top.Provenance.ByteLen); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("raw of a hidden session's source: %v", err)
	}
	if _, err := c.RawByPath(ctx, top.Provenance.Path, "", -1, 0, 10); err == nil {
		t.Error("raw by path of a hidden session's source")
	}
	if len(after.Sessions) == 0 {
		t.Fatal("hiding one session hid everything")
	}
	if _, err := c.Read(ctx, format.ReadQuery{Address: after.Sessions[0].SessionID}, format.Filters{}); err != nil {
		t.Errorf("read of a visible session: %v", err)
	}
}
