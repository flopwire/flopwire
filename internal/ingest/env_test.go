package ingest

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/api"
	"github.com/flopwire/flopwire/internal/auth"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/syncproto/synctest"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// env is a real server (API + ingest + parse queue) on a fresh Postgres
// database and MinIO bucket, with one enrolled device.
type env struct {
	t                       *testing.T
	ctx                     context.Context
	pool                    *pgxpool.Pool
	objects                 Objects
	store                   *store.Postgres
	userID, deviceID, token string
	queue                   *Queue
	http                    *httptest.Server
	client                  *syncproto.Client
}

func newEnv(t *testing.T) *env {
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
	e := &env{t: t, ctx: ctx, pool: pool, objects: MinIO{Client: mc, Bucket: bucket}, store: store.NewPostgres(pool, mc, bucket)}
	e.userID, e.deviceID = uuid.NewString(), uuid.NewString()
	plain, hash, _ := auth.NewToken()
	e.token = plain
	now := time.Now().UTC()
	e.exec(`INSERT INTO users(id,email,name,role,identity_type,created_at) VALUES($1,$2,'Gary','member','human',$3)`, e.userID, e.userID+"@example.test", now)
	e.exec(`INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$2,'mac','darwin',$3)`, e.deviceID, e.userID, now)
	e.exec(`INSERT INTO credentials(id,user_id,device_id,kind,token_hash,created_at) VALUES($1,$2,$3,'device',$4,$5)`, uuid.NewString(), e.userID, e.deviceID, hash, now)
	e.start()
	return e
}

// start (re)starts the server process state: HTTP server, ingest server,
// parse queue. The database and bucket persist.
func (e *env) start() {
	if e.http != nil {
		e.http.Close()
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e.queue = &Queue{Pool: e.pool, Objects: e.objects, Log: log}
	srv := &Server{Pool: e.pool, Objects: e.objects, Log: log, Queue: e.queue}
	e.http = httptest.NewServer(api.New(e.store, api.Config{Logger: log, Sync: srv, Parse: e.queue}).Handler(nil))
	e.t.Cleanup(e.http.Close)
	e.client = &syncproto.Client{Server: e.http.URL, Token: e.token, HTTP: e.http.Client()}
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (e *env) drain() {
	e.t.Helper()
	if err := e.queue.Drain(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

// reconstruct reads a generation of the device's source back.
func (e *env) reconstruct(path, fileID string, gen int64) ([]byte, error) {
	var id string
	if err := e.pool.QueryRow(e.ctx, `SELECT id::text FROM sources WHERE device_id=$1 AND path=$2 AND file_id=$3`, e.deviceID, path, fileID).Scan(&id); err != nil {
		return nil, err
	}
	g, err := LoadGeneration(e.ctx, e.pool, id, gen)
	if err != nil {
		return nil, err
	}
	r := NewReader(e.ctx, e.objects, g)
	return io.ReadAll(io.NewSectionReader(r, 0, r.Size()))
}

// syncer is a device agent syncing to the env's server.
func (e *env) syncer(cfg devicesync.Config) *devicesync.Syncer {
	e.t.Helper()
	dir := e.t.TempDir()
	st, err := devicesync.OpenStore(filepath.Join(dir, "sync.db"))
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { st.Close() })
	sp, err := devicesync.OpenSpool(filepath.Join(dir, "spool"), 1<<30)
	if err != nil {
		e.t.Fatal(err)
	}
	if cfg.Chunk == (devicesync.ChunkParams{}) {
		cfg.Chunk = devicesync.ChunkParams{Min: 1 << 10, Avg: 4 << 10, Max: 16 << 10}
	}
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	// The client is looked up per request so a restarted server is reached.
	sy, err := devicesync.NewSyncer(cfg, st, sp, transport{e})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(sy.Close)
	return sy
}

type transport struct{ e *env }

func (t transport) Has(ctx context.Context, h []syncproto.Hash) ([]syncproto.Hash, error) {
	return t.e.client.Has(ctx, h)
}
func (t transport) Flush(ctx context.Context, r *syncproto.FlushRequest) (*syncproto.FlushResponse, error) {
	return t.e.client.Flush(ctx, r)
}

// The protocol conformance suite, against the real server.
func TestConformance(t *testing.T) {
	synctest.Conformance(t, func(t *testing.T) synctest.Target {
		e := newEnv(t)
		return synctest.Target{Client: e.client, Reconstruct: e.reconstruct}
	})
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.Walk(from, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		if fi.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// bodyOf announces data as one compressed body; zpayload is its payload.
func bodyOf(data []byte) []syncproto.Body {
	b, _ := syncproto.EncodeBody(data)
	return []syncproto.Body{b}
}

func zpayload(data []byte) io.Reader {
	_, z := syncproto.EncodeBody(data)
	return bytes.NewReader(z)
}
