package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/api"
	"github.com/flopwire/flopwire/internal/auth"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/ingest"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// withholdServer is a real Flopwire server (API, sync, parse) with one
// member and one device, and a device syncer that uploads to it.
type withholdServer struct {
	t      *testing.T
	pool   *pgxpool.Pool
	pg     *store.Postgres
	queue  *ingest.Queue
	url    string
	token  string
	user   string
	device string
	h      *httptest.Server
	sy     *devicesync.Syncer
}

func newWithholdServer(t *testing.T) *withholdServer {
	t.Helper()
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
	s := &withholdServer{t: t, pool: pool, pg: store.NewPostgres(pool, mc, bucket), queue: &ingest.Queue{Pool: pool, Objects: objects, Log: log}, token: plain, user: user, device: device}
	s.h = httptest.NewServer(api.New(s.pg, api.Config{Logger: log,
		Sync: &ingest.Server{Pool: pool, Objects: objects, Log: log, Queue: s.queue}}).Handler(nil))
	t.Cleanup(s.h.Close)
	s.url = s.h.URL
	st, err := devicesync.OpenStore(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	spool, _ := devicesync.OpenSpool(filepath.Join(t.TempDir(), "spool"), 64<<20)
	s.sy, err = devicesync.NewSyncer(devicesync.Config{Logger: log}, st, spool, &syncproto.Client{Server: s.url, Token: plain, HTTP: s.h.Client()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.sy.Close)
	return s
}

// uploadAlpha uploads the alpha session's transcripts (its own and its
// subagent's) as they are now; parse drains the server's parse queue.
func (s *withholdServer) uploadAlpha(f *fixture, parse bool) {
	s.t.Helper()
	sessions, err := claude.Discover(filepath.Join(f.home, ".claude", "projects"))
	if err != nil {
		s.t.Fatal(err)
	}
	n := 0
	for _, sess := range sessions {
		if sess.SessionID != alphaID {
			continue
		}
		for _, src := range sess.Sources() {
			if err := s.sy.Sync(ctx, devicesync.SourceSpec{Path: src.Path, Agent: transcript.AgentClaude, SessionKey: src.SessionKey,
				StorageKind: transcript.StorageJSONLAppend, Parser: claude.ParserName}); err != nil {
				s.t.Fatal(err)
			}
			n++
		}
	}
	if n < 2 {
		s.t.Fatalf("alpha session has %d transcripts, want its own and a subagent's", n)
	}
	if parse {
		s.drain()
	}
}

func (s *withholdServer) drain() {
	s.t.Helper()
	if err := s.queue.Drain(ctx); err != nil {
		s.t.Fatal(err)
	}
}

func (s *withholdServer) count(q string, args ...any) int {
	s.t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	return n
}

// alphaConvs counts the server's conversations of the alpha session and
// the subagents below it.
func (s *withholdServer) alphaConvs() int {
	return s.count(`SELECT count(*) FROM conversations WHERE session_id=$1 OR parent_native_session_id=$1`, alphaID)
}

func (f *fixture) withhold() string {
	f.t.Helper()
	var s string
	if err := f.store.DB().QueryRow(`SELECT ifnull(withhold, '') FROM placements WHERE agent = 'claude' AND session_id = ?`, alphaID).Scan(&s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

// When a later directory moves an uploaded session to local, the device
// asks the server to delete what it holds of it: the conversation and its
// subagent's are deleted and tombstoned, the raw uploads purged, the
// request audited as conversation.withheld with the rule, and an upload of
// the session afterwards does not bring it back. The local rows stay.
func TestWithheldSessionDeletedOnServer(t *testing.T) {
	s := newWithholdServer(t)
	f, _, _ := rulesFixture(t, "-", "local:/tmp/oracle-secret")
	f.cfg.Withhold = WithholdSession(s.url, s.token, s.h.Client())
	f.a = New(f.store, f.cfg)
	f.once()
	s.uploadAlpha(f, true)
	if n := s.alphaConvs(); n < 2 {
		t.Fatalf("server holds %d conversations of the alpha session, want it and its subagent's", n)
	}

	appendFile(t, f.path(alphaRel), claudeUserAt("/tmp/oracle-secret", "c9000000-0000-4000-8000-000000000011", "now in the secret checkout"))
	f.once()

	if n := s.alphaConvs(); n != 0 {
		t.Errorf("server still holds %d conversations of the withheld session", n)
	}
	if n := s.count(`SELECT count(*) FROM conversation_tombstones WHERE session_id=$1`, alphaID); n != 1 {
		t.Errorf("tombstones of the withheld session: %d", n)
	}
	var meta string
	if err := s.pool.QueryRow(ctx, `SELECT metadata::text FROM audit_events WHERE action='conversation.withheld'`).Scan(&meta); err != nil {
		t.Fatalf("no conversation.withheld audit event: %v", err)
	}
	for _, want := range []string{alphaID, `"mode": "local"`, `local:/tmp/oracle-secret`} {
		if !strings.Contains(meta, want) {
			t.Errorf("audit metadata %s lacks %s", meta, want)
		}
	}
	if w := f.withhold(); w != "" {
		t.Errorf("deletion still owed after the server took it: %q", w)
	}
	if len(f.find("secret checkout", false)) != 1 {
		t.Error("the local rows of a session moved to local are gone")
	}
	if c := f.a.serverCopiesNotice(); c != nil {
		t.Errorf("server-copies notice although the server deleted them: %+v", c)
	}
	if _, err := s.pg.ProcessDeletionJobs(ctx); err != nil {
		t.Fatal(err)
	}
	if n := s.count(`SELECT count(*) FROM sources WHERE session_key=$1 AND tombstoned_at IS NULL`, alphaID); n != 0 {
		t.Errorf("%d raw sources of the withheld session left", n)
	}
	// An upload of it afterwards (another agent, an older binary) stays out.
	s.uploadAlpha(f, true)
	if n := s.alphaConvs(); n != 0 {
		t.Errorf("an upload brought %d conversations of the withheld session back", n)
	}
}

// A deletion the server did not take stays owed across a restart and is
// sent when the agent runs again; a deny verdict both purges the local
// rows and deletes the server's copy. The session was uploaded but not
// parsed yet: the tombstone keeps its parse from storing it.
func TestWithholdOwedAcrossRestart(t *testing.T) {
	s := newWithholdServer(t)
	f, _, _ := rulesFixture(t, "-", "deny /tmp/oracle-secret")
	var calls atomic.Int32
	f.cfg.Withhold = func(context.Context, localindex.Withhold) error {
		calls.Add(1)
		return errors.New("offline")
	}
	f.a = New(f.store, f.cfg)
	f.once()
	s.uploadAlpha(f, false) // not parsed yet

	appendFile(t, f.path(alphaRel), claudeUserAt("/tmp/oracle-secret/x", "c9000000-0000-4000-8000-000000000012", "denied later"))
	f.once()
	if calls.Load() == 0 {
		t.Fatal("the server deletion was never tried")
	}
	if w := f.withhold(); !strings.HasPrefix(w, "deny\t") || !strings.Contains(w, "/tmp/oracle-secret") {
		t.Fatalf("deletion owed: %q", w)
	}

	f.cfg.Withhold = WithholdSession(s.url, s.token, s.h.Client())
	f.restart()
	stop := f.run()
	waitFor(t, func() bool { return f.withhold() == "" })
	stop()
	s.drain()
	if n := s.alphaConvs(); n != 0 {
		t.Errorf("the parse stored %d conversations of a withheld session", n)
	}
	if n := s.count(`SELECT count(*) FROM sources WHERE session_key=$1 AND tombstoned_at IS NULL`, alphaID); n != 0 {
		t.Errorf("%d raw sources of the withheld session left", n)
	}
	if n := s.count(`SELECT count(*) FROM audit_events WHERE action='conversation.withheld'`); n != 1 {
		t.Errorf("conversation.withheld audit events: %d", n)
	}
	// Nothing stored of a session: done, not an error.
	if err := WithholdSession(s.url, s.token, s.h.Client())(ctx, localindex.Withhold{Agent: transcript.AgentClaude, SessionID: "never-uploaded", Mode: "deny"}); err != nil {
		t.Errorf("withholding a session the server never saw: %v", err)
	}
}

// A crash right after a parse saved its watermark must not lose the
// directories its lines named: the next parse appends past those lines.
func TestCrashAfterWatermarkKeepsLaterCwd(t *testing.T) {
	f, _, _ := rulesFixture(t, "-", "deny /tmp/oracle-secret")
	f.once()
	appendFile(t, f.path(alphaRel), claudeUserAt("/tmp/oracle-secret", "c9000000-0000-4000-8000-000000000013", "crash line"))
	testHookAfterFlush = func() error { return errors.New("crash") }
	_ = f.a.Once(ctx)
	testHookAfterFlush = nil

	f.restart()
	f.once()
	if f.a.allowUpload(f.alphaSpec()) {
		t.Error("after the crash the sync filter lets the session upload")
	}
	if n, ok := f.a.uploadBound(f.alphaSpec()); ok && n >= 0 {
		t.Errorf("after the crash sync may read %d bytes of the denied session", n)
	}
	if n := f.count(`SELECT count(*) FROM conversations WHERE session_id = ?`, alphaID); n != 0 {
		t.Errorf("%d conversations left of the denied session", n)
	}
}
