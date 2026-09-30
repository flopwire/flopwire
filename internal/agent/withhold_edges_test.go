package agent

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/auth"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/google/uuid"
)

// The upload has already sent its chunks but its manifest has not committed.
// A successful withhold must survive the later commit and parent/child parse.
func TestWithholdBeforeUploadCommit(t *testing.T) {
	s := newWithholdServer(t)
	f, _, _ := rulesFixture(t, "-", "deny /tmp/oracle-secret")
	arrived, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var held atomic.Bool
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == syncproto.PathFlush && held.CompareAndSwap(false, true) {
			close(arrived)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		s.h.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(gate.Close)
	s.sy = s.syncer(gate.URL, s.token)
	go func() { defer close(done); s.uploadAlpha(f, false) }()
	// Release even if an assertion fails, so cleanup cannot hang.
	defer func() {
		if !released(release) {
			close(release)
		}
		<-done
	}()
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("upload never reached flush")
	}
	if n := s.count(`SELECT count(*) FROM sources`); n != 0 {
		t.Fatalf("%d sources committed before barrier", n)
	}
	if err := WithholdSession(s.url, s.token, s.h.Client())(ctx, localindex.Withhold{Agent: transcript.AgentClaude, SessionID: alphaID, Mode: "deny"}); err != nil {
		t.Fatal(err)
	}
	if n := s.count(`SELECT count(*) FROM conversation_tombstones WHERE session_id=$1 AND scope='user'`, alphaID); n != 1 {
		t.Fatalf("accepted without durable tombstone: %d", n)
	}
	close(release)
	<-done
	s.drain()
	if n := s.alphaConvs(); n != 0 {
		t.Fatalf("late upload exposed %d conversations", n)
	}
	if n := s.count(`SELECT count(*) FROM sources WHERE tombstoned_at IS NULL`); n != 0 {
		t.Fatalf("late upload left %d live raw sources", n)
	}
}

func released(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func (s *withholdServer) syncer(server, token string) *devicesync.Syncer {
	s.t.Helper()
	st, err := devicesync.OpenStore(filepath.Join(s.t.TempDir(), "sync.db"))
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { st.Close() })
	spool, err := devicesync.OpenSpool(filepath.Join(s.t.TempDir(), "spool"), 64<<20)
	if err != nil {
		s.t.Fatal(err)
	}
	sy, err := devicesync.NewSyncer(devicesync.Config{}, st, spool, &syncproto.Client{Server: server, Token: token, HTTP: s.h.Client()})
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(sy.Close)
	return sy
}

func (s *withholdServer) scopedDevice(kind string, scopes []string) (string, string) {
	s.t.Helper()
	device := uuid.NewString()
	plain, hash, err := auth.NewToken()
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO devices(id,user_id,name,platform,kind,created_at) VALUES($1,$2,'sandbox','linux','ephemeral',now())`, device, s.user); err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO credentials(id,user_id,device_id,kind,token_hash,scopes,expires_at,created_at) VALUES($1,$2,$3,$4,$5,$6,now()+interval '1 hour',now())`, uuid.NewString(), s.user, device, kind, hash, scopes); err != nil {
		s.t.Fatal(err)
	}
	return device, plain
}

// The same user's enrolled device retains its parent and child, while a
// sandbox's copies are deleted and remain withheld on later uploads.
func TestUploadTokenWithholdIsDeviceScoped(t *testing.T) {
	for _, prospective := range []bool{false, true} {
		name := "parsed"
		if prospective {
			name = "before-upload"
		}
		t.Run(name, func(t *testing.T) {
			s := newWithholdServer(t)
			f, _, _ := rulesFixture(t, "-", "deny /tmp/oracle-secret")
			s.uploadAlpha(f, true)
			ownerConvs := s.alphaConvs()
			if ownerConvs < 2 {
				t.Fatal("fixture lacks parent/subagent")
			}
			device, token := s.scopedDevice("minted", []string{"upload"})
			s.sy = s.syncer(s.url, token)
			if !prospective {
				s.uploadAlpha(f, true)
			}
			var deletedID string
			if !prospective {
				if err := s.pool.QueryRow(ctx, `SELECT id::text FROM conversations WHERE device_id=$1 AND session_id=$2`, device, alphaID).Scan(&deletedID); err != nil {
					t.Fatal(err)
				}
			}
			cb := WithholdSession(s.url, token, s.h.Client())
			if err := cb(ctx, localindex.Withhold{Agent: transcript.AgentClaude, SessionID: alphaID, Mode: "local"}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.pg.ProcessDeletionJobs(ctx); err != nil {
				t.Fatal(err)
			}
			if n := s.count(`SELECT count(*) FROM conversations WHERE device_id=$1`, s.device); n != ownerConvs {
				t.Fatalf("sandbox removed owner's copy: %d, want %d", n, ownerConvs)
			}
			if n := s.count(`SELECT count(*) FROM conversations WHERE device_id=$1`, device); n != 0 {
				t.Fatalf("sandbox kept %d conversations", n)
			}
			if n := s.count(`SELECT count(*) FROM conversation_tombstones WHERE session_id=$1 AND device_id=$2 AND scope='device'`, alphaID, device); n != 1 {
				t.Fatalf("device tombstone: %d", n)
			}
			// Repetition is accepted, without widening authority.
			if err := cb(ctx, localindex.Withhold{Agent: transcript.AgentClaude, SessionID: alphaID, Mode: "local"}); err != nil {
				t.Fatal(err)
			}
			s.uploadAlpha(f, true)
			if n := s.count(`SELECT count(*) FROM conversations WHERE device_id=$1`, device); n != 0 {
				t.Fatalf("late upload restored %d sandbox conversations", n)
			}
			if n := s.count(`SELECT count(*) FROM sources WHERE device_id=$1 AND tombstoned_at IS NULL`, device); n != 0 {
				t.Fatalf("sandbox left %d live sources", n)
			}
			if n := s.count(`SELECT count(*) FROM sources WHERE device_id=$1 AND tombstoned_at IS NULL`, s.device); n == 0 {
				t.Fatal("sandbox purged owner's raw copies")
			}
			// A global request cannot treat a previous device-only job as success.
			// This is the state a global request sees after selecting a conversation
			// that a concurrent scoped request deletes before it acquires the lock.
			if deletedID != "" {
				if _, err := s.pg.RequestConversationDeletion(ctx, deletedID, s.user, s.device, true); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("narrow job acknowledged global deletion: %v", err)
				}
			}
			// A member's own request still covers all devices, even after scoped intent.
			if err := WithholdSession(s.url, s.token, s.h.Client())(ctx, localindex.Withhold{Agent: transcript.AgentClaude, SessionID: alphaID, Mode: "deny"}); err != nil {
				t.Fatal(err)
			}
			if n := s.alphaConvs(); n != 0 {
				t.Fatalf("member left %d copies", n)
			}
		})
	}
}

func TestWithholdCredentialAuthority(t *testing.T) {
	s := newWithholdServer(t)
	_, read := s.scopedDevice("minted", []string{"read"})
	_, readDevice := s.scopedDevice("device", []string{"read"})
	_, both := s.scopedDevice("minted", []string{"upload", "read"})
	for _, token := range []string{read, readDevice} {
		if err := WithholdSession(s.url, token, s.h.Client())(ctx, localindex.Withhold{Agent: transcript.AgentClaude, SessionID: "read-refused", Mode: "deny"}); err == nil {
			t.Fatal("read-only token withheld a session")
		}
	}
	if err := WithholdSession(s.url, both, s.h.Client())(ctx, localindex.Withhold{Agent: transcript.AgentClaude, SessionID: "both-scoped", Mode: "deny"}); err != nil {
		t.Fatal(err)
	}
	if n := s.count(`SELECT count(*) FROM conversation_tombstones WHERE session_id='both-scoped' AND scope='device'`); n != 1 {
		t.Fatal("minted read+upload token received user-wide scope")
	}
	// Prospective global intent must not be satisfied by a narrower tombstone.
	if err := WithholdSession(s.url, s.token, s.h.Client())(ctx, localindex.Withhold{Agent: transcript.AgentClaude, SessionID: "both-scoped", Mode: "deny"}); err != nil {
		t.Fatal(err)
	}
	if n := s.count(`SELECT count(*) FROM conversation_tombstones WHERE session_id='both-scoped' AND scope='user'`); n != 1 {
		t.Fatal("narrower intent prevented prospective user tombstone")
	}
	plain, hash, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO credentials(id,user_id,kind,token_hash,expires_at,created_at) VALUES($1,$2,'session',$3,now()+interval '1 hour',now())`, uuid.NewString(), s.user, hash); err != nil {
		t.Fatal(err)
	}
	if err := WithholdSession(s.url, plain, s.h.Client())(ctx, localindex.Withhold{Agent: transcript.AgentClaude, SessionID: "login-refused", Mode: "deny"}); err == nil {
		t.Fatal("login session withheld without a bound upload device")
	}
	// A service's default device credential has upload authority, no read.
	if _, err := s.pool.Exec(ctx, `UPDATE users SET identity_type='service' WHERE id=$1`, s.user); err != nil {
		t.Fatal(err)
	}
	if err := WithholdSession(s.url, s.token, s.h.Client())(ctx, localindex.Withhold{Agent: transcript.AgentClaude, SessionID: "service-scoped", Mode: "deny"}); err != nil {
		t.Fatal(err)
	}
	if n := s.count(`SELECT count(*) FROM conversation_tombstones WHERE session_id='service-scoped' AND scope='device'`); n != 1 {
		t.Fatal("service received user-wide scope")
	}
	req, _ := http.NewRequest("GET", s.url+"/v1/search?q=x", nil)
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := s.h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("service read status %d", resp.StatusCode)
	}
}

// An older server's 404 cannot acknowledge an upload that has not committed.
// Keep the local debt across restart until the server explicitly accepts it.
func TestWithhold404RemainsOwed(t *testing.T) {
	var accepted atomic.Bool
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if accepted.Load() {
			w.WriteHeader(http.StatusAccepted)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer h.Close()
	f, _, logs := rulesFixture(t, "-", "deny /tmp/oracle-secret")
	f.cfg.Withhold = WithholdSession(h.URL, "synthetic-test-token", h.Client())
	f.a = New(f.store, f.cfg)
	f.once()
	appendFile(t, f.path(alphaRel), claudeUserAt("/tmp/oracle-secret/x", "c9000000-0000-4000-8000-000000000022", "denied while upload pending"))
	f.once()
	if !strings.HasPrefix(f.withhold(), "deny\t") {
		t.Fatal("404 cleared owed withhold")
	}
	f.restart()
	f.a.sendWithholds(ctx)
	if f.withhold() == "" {
		t.Fatal("restart lost owed withhold")
	}
	accepted.Store(true)
	f.a.sendWithholds(ctx)
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if w := f.withhold(); w != "" {
		t.Fatalf("accepted request stays owed: %q logs: %s", w, logs.String())
	}
}
