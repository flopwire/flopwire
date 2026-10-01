package devicesync

import (
	"bytes"
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/perfguard"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/syncproto/synctest"
	"github.com/flopwire/flopwire/internal/transcript"
)

// newCountedEnv is newEnv with the store opened through perfguard's
// counting SQLite driver.
func newCountedEnv(t *testing.T, cfg Config) (*env, *perfguard.SQLiteCounter) {
	t.Helper()
	e := &env{t: t, dir: t.TempDir(), srv: synctest.New("device-token"), logs: &bytes.Buffer{}}
	e.http = httptest.NewServer(e.srv)
	t.Cleanup(e.http.Close)
	counter := perfguard.CountSQLite(t, e.dir)
	db := perfguard.OpenSQLite(t, "file:"+filepath.Join(e.dir, "sync.db")+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)")
	var err error
	if e.store, err = NewStore(db); err != nil {
		t.Fatal(err)
	}
	if e.spool, err = OpenSpool(filepath.Join(e.dir, "spool"), 1<<30); err != nil {
		t.Fatal(err)
	}
	e.client = &syncproto.Client{Server: e.http.URL, Token: "device-token", HTTP: e.http.Client()}
	if cfg.Chunk == (ChunkParams{}) {
		cfg.Chunk = small
	}
	cfg.Logger = slog.New(slog.NewTextHandler(e.logs, nil))
	if e.sy, err = NewSyncer(cfg, e.store, e.spool, e.client); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.sy.Close)
	return e, counter
}

// syncStatements syncs a fresh source of lines transcript lines and
// returns the SQLite statements the sync ran and the chunks it produced.
func syncStatements(t *testing.T, kind transcript.StorageKind, lines int) (int64, int64, *perfguard.SQLiteCounter) {
	t.Helper()
	e, counter := newCountedEnv(t, Config{})
	name := "s.jsonl"
	if kind == transcript.StorageJSONDoc {
		name = "s.json"
	}
	sp := e.spec(name, kind)
	if err := os.WriteFile(sp.Path, jsonlLines(3, lines, 200), 0o600); err != nil {
		t.Fatal(err)
	}
	counter.Reset()
	cost := perfguard.MeasureSQLite(counter, func() { e.sync(sp) })
	src, err := e.store.source(context.Background(), sp.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	g, err := e.store.gen(context.Background(), src.ID, src.Gen)
	if err != nil || g == nil || !g.done() {
		t.Fatalf("generation not uploaded: %+v %v\n%s", g, err, e.logs)
	}
	return cost.Statements, g.Entries, counter
}

// A sync's SQLite statements must not grow with its chunk count beyond a
// batched bound: the known-chunk lookups (capture and nextBatch), manifest
// inserts and acknowledgements go in batches, not one statement per chunk
// (perf-guards.md #6).
func TestSyncStatementsBatched(t *testing.T) {
	for _, kind := range []transcript.StorageKind{transcript.StorageJSONLAppend, transcript.StorageJSONDoc} {
		t.Run(string(kind), func(t *testing.T) {
			s1, c1, _ := syncStatements(t, kind, 1000)
			s8, c8, counter := syncStatements(t, kind, 8*1000)
			added := c8 - c1
			if added < 300 {
				t.Fatalf("fixture too small: %d → %d chunks", c1, c8)
			}
			// At most one statement per 64 added chunks.
			if extra := s8 - s1; extra*64 > added {
				t.Errorf("%d → %d chunks: statements %d → %d (+%d, bound +%d)\n%s", c1, c8, s1, s8, extra, added/64, counter)
			} else {
				t.Logf("%d → %d chunks: statements %d → %d", c1, c8, s1, s8)
			}
		})
	}
}

// The store's lookups by hash and by generation use their indexes.
func TestSyncStorePlans(t *testing.T) {
	e, _ := newCountedEnv(t, Config{})
	db := e.store.db
	h := make([]byte, 32)
	perfguard.AssertSQLitePlan(t, db, nil, `SELECT hash FROM devsync_known WHERE hash IN (?, ?)`, h, h)
	perfguard.AssertSQLitePlan(t, db, nil, `SELECT ordinal, hash, offset, size FROM devsync_manifest
	  WHERE source_id = ? AND generation = ? AND ordinal >= ? ORDER BY ordinal LIMIT ?`, 1, 1, 0, 10)
	perfguard.AssertSQLitePlan(t, db, nil, `SELECT `+genCols+` FROM devsync_gens
	  WHERE source_id = ? AND lost = 0 AND (acked < entries OR tail_acked = 0) ORDER BY generation`, 1)
	perfguard.AssertSQLitePlan(t, db, nil, referencedSQL(2), h, h)
	perfguard.AssertSQLitePlan(t, db, nil, `SELECT id, spec, generation, watermark FROM devsync_sources WHERE path = ?`, "x")
}

// The batched hash operations cross batch boundaries correctly.
func TestStoreHashSetsAcrossBatches(t *testing.T) {
	e, _ := newCountedEnv(t, Config{})
	ctx := context.Background()
	hs := make([]syncproto.Hash, 3*batchRows+7)
	for i := range hs {
		hs[i] = syncproto.Sum([]byte{byte(i), byte(i >> 8)})
	}
	if err := e.store.remember(ctx, hs[:2*batchRows+3]); err != nil {
		t.Fatal(err)
	}
	if err := e.store.forget(ctx, hs[:batchRows+1]); err != nil {
		t.Fatal(err)
	}
	known, err := e.store.known(ctx, hs)
	if err != nil {
		t.Fatal(err)
	}
	for i, h := range hs {
		if want := i > batchRows && i < 2*batchRows+3; known[h] != want {
			t.Fatalf("hash %d known=%v, want %v", i, known[h], want)
		}
	}
	if len(known) != batchRows+2 {
		t.Fatalf("%d known, want %d", len(known), batchRows+2)
	}
}
