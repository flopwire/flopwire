package ingest_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/api"
	"github.com/flopwire/flopwire/internal/auth"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/ingest"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/flopwire/flopwire/internal/retrieval"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const chainNativeID = "11111111-2222-4333-8444-555555555555"

// These fixtures bridge the real collector, local capture ledger, authenticated
// policy endpoint, and PostgreSQL/MinIO evidence. Expectations come from the
// literal synthetic inputs, never from a second invocation of the parser.
type chainDevice struct {
	home, selected, metadata, main, child, companion string
	mainBytes, childBytes, companionBytes            []byte
	index                                            *localindex.Store
	syncDB                                           *sql.DB
	syncStore                                        *devicesync.Store
	spool                                            *devicesync.Spool
	sy                                               *devicesync.Syncer
	scheduler                                        *devicesync.Scheduler
	collector                                        *agent.Agent
	config                                           agent.Config
	client                                           *syncproto.Client
	stop                                             context.CancelFunc
	done                                             chan error
}

func chainWrite(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
}

func chainNativeRecord(session, native, text string) []byte {
	return []byte(fmt.Sprintf(`{"parentUuid":null,"cwd":"/sessions/qualification-workspace","sessionId":%q,"type":"user","message":{"role":"user","content":%q},"uuid":%q,"timestamp":"2026-10-06T11:00:00.000Z"}`+"\n", session, text, native))
}

func (d *chainDevice) writeMapping(t *testing.T, folders []string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"sessionId": "qualification-app", "cliSessionId": chainNativeID, "cwd": "/sessions/qualification-workspace", "userSelectedFolders": folders})
	if err != nil {
		t.Fatal(err)
	}
	chainWrite(t, d.metadata, body)
}

// A test-only capability override is used while production advertises zero.
// The real authenticated capability handler runs first. Qualification of an
// enablement candidate must instead pass override=false and use its actual route.
type chainCapabilityServer struct {
	server   *ingest.Server
	override bool
}

func (s chainCapabilityServer) ServeSync(w http.ResponseWriter, r *http.Request, device string) {
	if !s.override || r.URL.Path != syncproto.PathCapabilities {
		s.server.ServeSync(w, r, device)
		return
	}
	recorded := httptest.NewRecorder()
	s.server.ServeSync(recorded, r, device)
	if recorded.Code != http.StatusOK {
		for name, values := range recorded.Header() {
			w.Header()[name] = values
		}
		w.WriteHeader(recorded.Code)
		_, _ = w.Write(recorded.Body.Bytes())
		return
	}
	var capability syncproto.CapabilitiesResponse
	if err := json.Unmarshal(recorded.Body.Bytes(), &capability); err != nil {
		http.Error(w, "invalid real capability response", http.StatusInternalServerError)
		return
	}
	capability.PolicyPlacementsVersion = 1
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(capability)
}

type chainEnv struct {
	pool    *pgxpool.Pool
	queue   *ingest.Queue
	objects ingest.MinIO
	store   *store.Postgres
	userID  string
}

func newChainEnv(t *testing.T) *chainEnv {
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
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := &chainEnv{pool: pool, objects: objects, store: store.NewPostgres(pool, mc, bucket), userID: uuid.NewString()}
	e.queue = &ingest.Queue{Pool: pool, Objects: objects, Log: log}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,email,name,role,identity_type,created_at) VALUES($1,$2,'Qualification','member','human',now())`, e.userID, e.userID+"@example.test"); err != nil {
		t.Fatal(err)
	}
	return e
}

// Only credential identities are seeded. Conversation, source, generation,
// manifest, and recovered-provenance rows must come from actual uploaded bytes.
func (e *chainEnv) credential(t *testing.T, name string) (string, string) {
	t.Helper()
	id := uuid.NewString()
	token, hash, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$2,$3,'darwin',now())`, id, e.userID, name); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO credentials(id,user_id,device_id,kind,token_hash,created_at) VALUES($1,$2,$3,'device',$4,now())`, uuid.NewString(), e.userID, id, hash); err != nil {
		t.Fatal(err)
	}
	return id, token
}

func newChainAPI(t *testing.T, e *chainEnv, override bool) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := &ingest.Server{Pool: e.pool, Objects: e.objects, Log: log, Queue: e.queue}
	r := &retrieval.Store{Pool: e.pool, Objects: e.objects, RefreshSession: func(ctx context.Context, id string) { e.queue.RefreshSession(ctx, id) }}
	h := httptest.NewServer(api.New(e.store, api.Config{Logger: log, Sync: chainCapabilityServer{server: server, override: override}, Parse: e.queue, Retrieval: r}).Handler(nil))
	t.Cleanup(h.Close)
	return h
}

func newChainDevice(t *testing.T, deviceID, token string, h *httptest.Server) *chainDevice {
	t.Helper()
	home := t.TempDir()
	index, err := localindex.Open(filepath.Join(home, "index.db"), localindex.Options{DeviceID: deviceID, DeferCommit: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	if err := index.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	syncDB, err := sql.Open("sqlite", "file:"+index.Path()+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	syncDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = syncDB.Close() })
	syncStore, err := devicesync.NewStore(syncDB)
	if err != nil {
		t.Fatal(err)
	}
	spool, err := devicesync.OpenSpool(filepath.Join(home, "spool"), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	client := &syncproto.Client{Server: h.URL, Token: token, HTTP: h.Client()}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sy, err := devicesync.NewSyncer(devicesync.Config{Logger: log, SealAfter: -1, Chunk: devicesync.ChunkParams{Min: 1024, Avg: 4096, Max: 16384}}, syncStore, spool, client)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sy.Close)
	d := &chainDevice{home: home, selected: filepath.Join(home, "selected"), index: index, syncDB: syncDB, syncStore: syncStore, spool: spool, sy: sy, client: client}
	d.scheduler = devicesync.NewScheduler(sy, devicesync.SchedulerConfig{Append: devicesync.Cadence{Debounce: time.Millisecond, MaxWait: time.Millisecond}, Document: devicesync.Cadence{Debounce: time.Millisecond, MaxWait: time.Millisecond}})
	d.config = agent.Config{Home: home, CoworkRoot: filepath.Join(home, "cowork"), DesktopCodeRoot: "-", ClaudeProjects: filepath.Join(home, "cli-projects"), CodexHome: filepath.Join(home, "codex"), DevinDB: "-", OpencodeDB: "-", OpencodeRegistry: "-", Workers: 2, Sync: d.scheduler, Logger: log, CoworkPolicy: &devicesync.PolicyClient{Server: h.URL, Token: token, HTTP: h.Client()}}
	d.metadata = filepath.Join(d.config.CoworkRoot, "account", "workspace", "qualification-app.json")
	project := filepath.Join(d.config.CoworkRoot, "account", "workspace", "qualification-app", ".claude", "projects", "-sessions-qualification-workspace")
	d.main = filepath.Join(project, chainNativeID+".jsonl")
	d.child = filepath.Join(project, chainNativeID, "subagents", "agent-cafe.jsonl")
	d.companion = filepath.Join(project, chainNativeID, "tool-results", "qualification.txt")
	d.mainBytes = chainNativeRecord(chainNativeID, "qualification-main", "full chain mapped parent evidence")
	d.childBytes = chainNativeRecord("agent-cafe", "qualification-child", "full chain mapped child evidence")
	d.companionBytes = []byte("full chain mapped companion evidence\n")
	if err := os.MkdirAll(d.selected, 0700); err != nil {
		t.Fatal(err)
	}
	d.writeMapping(t, []string{d.selected})
	chainWrite(t, d.main, d.mainBytes)
	chainWrite(t, d.child, d.childBytes)
	chainWrite(t, d.companion, d.companionBytes)
	return d
}

func (d *chainDevice) start(t *testing.T) {
	t.Helper()
	d.collector = agent.New(d.index, d.config)
	ctx, cancel := context.WithCancel(context.Background())
	d.stop, d.done = cancel, make(chan error, 1)
	go func() { d.done <- d.scheduler.Run(ctx) }()
	t.Cleanup(func() { d.halt(t) })
}

func (d *chainDevice) halt(t *testing.T) {
	t.Helper()
	if d.stop == nil {
		return
	}
	d.stop()
	select {
	case <-d.done:
	case <-time.After(10 * time.Second):
		t.Fatal("qualification scheduler did not stop")
	}
	d.stop = nil
}

func (d *chainDevice) once(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := d.collector.Once(ctx); err != nil {
		t.Fatal(err)
	}
}

func chainEventually(t *testing.T, note string, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		if last = fn(); last == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s: %v", note, last)
}

func (e *chainEnv) source(d *chainDevice, deviceID, path string) (string, int64, int64, error) {
	var source string
	var generation, size int64
	err := e.pool.QueryRow(context.Background(), `SELECT s.id::text,g.generation,g.size FROM sources s JOIN generations g ON g.source_id=s.id WHERE s.device_id=$1 AND s.path=$2 ORDER BY g.generation DESC LIMIT 1`, deviceID, path).Scan(&source, &generation, &size)
	return source, generation, size, err
}

func (e *chainEnv) familyRaw(t *testing.T, d *chainDevice, deviceID string) {
	t.Helper()
	c := client.HTTP{Server: d.client.Server, Token: d.client.Token, Client: d.client.HTTP}
	for _, fixture := range []struct {
		path string
		body []byte
	}{{d.main, d.mainBytes}, {d.child, d.childBytes}, {d.companion, d.companionBytes}} {
		chainEventually(t, "literal archived bytes "+filepath.Base(fixture.path), func() error {
			if err := e.queue.Drain(context.Background()); err != nil {
				return err
			}
			source, generation, size, err := e.source(d, deviceID, fixture.path)
			if err != nil {
				return err
			}
			if size != int64(len(fixture.body)) {
				return fmt.Errorf("archived size %d, want %d", size, len(fixture.body))
			}
			got, err := c.Raw(context.Background(), source, generation, 0, size)
			if err != nil {
				return err
			}
			if !bytes.Equal(got, fixture.body) {
				return fmt.Errorf("raw differs from literal fixture")
			}
			return nil
		})
	}
}

func chainHits(d *chainDevice, text string) (*format.Page, error) {
	c := client.HTTP{Server: d.client.Server, Token: d.client.Token, Client: d.client.HTTP}
	return c.Grep(context.Background(), format.GrepQuery{Pattern: text, Fixed: true}, format.Filters{})
}

// Production remains at capability zero during implementation qualification.
// The exact enablement candidate must rerun with FLOPWIRE_CHAIN_REAL_CAPABILITY=1
// to use its real capability response, without the test-only override.
func TestCoworkAgentFullChainMappedFamily(t *testing.T) {
	e := newChainEnv(t)
	h := newChainAPI(t, e, os.Getenv("FLOPWIRE_CHAIN_REAL_CAPABILITY") != "1")
	firstID, firstToken := e.credential(t, "chain-first")
	secondID, secondToken := e.credential(t, "chain-second")
	first := newChainDevice(t, firstID, firstToken, h)
	second := newChainDevice(t, secondID, secondToken, h)
	first.start(t)
	second.start(t)
	first.once(t)
	second.once(t)
	e.familyRaw(t, first, firstID)
	e.familyRaw(t, second, secondID)
	for _, d := range []*chainDevice{first, second} {
		var proofs int
		if err := d.syncDB.QueryRow(`SELECT count(*) FROM devsync_gens WHERE capture_proof IS NOT NULL`).Scan(&proofs); err != nil {
			t.Fatal(err)
		}
		if proofs != 3 {
			t.Fatalf("capture proofs %d, want 3 actual native files", proofs)
		}
	}
	page, err := chainHits(first, "full chain mapped parent evidence")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Hits) != 1 {
		t.Fatalf("folded identical text hits %d, want 1", len(page.Hits))
	}
	first.writeMapping(t, []string{})
	first.once(t)
	chainEventually(t, "device-scoped unknown mapping revocation", func() error {
		var hidden int
		if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM conversations WHERE device_id=$1 AND hidden_at IS NOT NULL AND session_id IN ($2,'agent-cafe')`, firstID, chainNativeID).Scan(&hidden); err != nil {
			return err
		}
		if hidden != 2 {
			return fmt.Errorf("hidden native family %d, want 2", hidden)
		}
		return nil
	})
	for _, path := range []string{first.main, first.child, first.companion} {
		source, gen, size, err := e.source(first, firstID, path)
		if err != nil {
			t.Fatal(err)
		}
		c := client.HTTP{Server: h.URL, Token: firstToken, Client: h.Client()}
		if _, err := c.Raw(context.Background(), source, gen, 0, size); err == nil {
			t.Fatal("revoked device raw evidence remained accessible")
		}
	}
	e.familyRaw(t, second, secondID)
	first.writeMapping(t, []string{first.selected})
	first.once(t)
	e.familyRaw(t, first, firstID)
	first.config.UserRules = filepath.Join(first.home, "rules")
	chainWrite(t, first.config.UserRules, []byte("deny "+first.selected+"\n"))
	first.halt(t)
	first.sy.Close()
	var restartErr error
	first.sy, restartErr = devicesync.NewSyncer(devicesync.Config{Logger: first.config.Logger, SealAfter: -1}, first.syncStore, first.spool, first.client)
	if restartErr != nil {
		t.Fatal(restartErr)
	}
	t.Cleanup(first.sy.Close)
	first.scheduler = devicesync.NewScheduler(first.sy, devicesync.SchedulerConfig{})
	first.config.Sync = first.scheduler
	first.start(t)
	first.once(t)
	chainEventually(t, "restarted explicit folder denial", func() error {
		var visible int
		if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM conversations WHERE device_id=$1 AND hidden_at IS NULL`, firstID).Scan(&visible); err != nil {
			return err
		}
		if visible != 0 {
			return fmt.Errorf("visible denied conversations %d", visible)
		}
		return nil
	})
	e.familyRaw(t, second, secondID)
}
