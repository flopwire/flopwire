package devicesync

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/syncproto/synctest"
	"github.com/flopwire/flopwire/internal/transcript"
)

// small chunk sizes keep unit tests fast and produce many boundaries.
var small = ChunkParams{Min: 1 << 10, Avg: 4 << 10, Max: 16 << 10}

type env struct {
	t      *testing.T
	dir    string
	srv    *synctest.Server
	http   *httptest.Server
	store  *Store
	spool  *Spool
	sy     *Syncer
	client *syncproto.Client
	logs   *bytes.Buffer
}

func newEnv(t *testing.T, cfg Config, spoolCap int64) *env {
	t.Helper()
	e := &env{t: t, dir: t.TempDir(), srv: synctest.New("device-token"), logs: &bytes.Buffer{}}
	e.http = httptest.NewServer(e.srv)
	t.Cleanup(e.http.Close)
	var err error
	if e.store, err = OpenStore(filepath.Join(e.dir, "sync.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.store.Close() })
	if e.spool, err = OpenSpool(filepath.Join(e.dir, "spool"), spoolCap); err != nil {
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
	return e
}

func (e *env) path(name string) string { return filepath.Join(e.dir, name) }

func (e *env) spec(name string, kind transcript.StorageKind) SourceSpec {
	return SourceSpec{Path: e.path(name), Agent: transcript.AgentCodex, StorageKind: kind, Parser: "codex@1"}
}

func (e *env) sync(sp SourceSpec) {
	e.t.Helper()
	if err := e.sy.Sync(context.Background(), sp); err != nil {
		e.t.Fatalf("sync %s: %v\nlogs:\n%s", sp.Path, err, e.logs)
	}
}

func fileIDOf(t *testing.T, path string) string {
	t.Helper()
	id, err := transcript.StatIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	return id.ID.String()
}

// requireServerHas checks the server reconstructs generation gen of path as want.
func (e *env) requireServerHas(path, fileID string, gen int64, want []byte) {
	e.t.Helper()
	got, err := e.srv.Reconstruct(path, fileID, gen)
	if err != nil {
		e.t.Fatalf("reconstruct %s gen %d: %v", path, gen, err)
	}
	if !bytes.Equal(got, want) {
		e.t.Fatalf("server gen %d has %d bytes, want %d (first diff at %d)", gen, len(got), len(want), firstDiff(got, want))
	}
}

func firstDiff(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

func appendFile(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(b); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// jsonlLines returns n synthetic transcript lines with incompressible-ish
// text, deterministic per seed.
func jsonlLines(seed uint64, n, textLen int) []byte {
	r := rand.New(rand.NewPCG(seed, 7))
	var buf bytes.Buffer
	const alphabet = "abcdefghijklmnopqrstuvwxyz     .,"
	for i := range n {
		text := make([]byte, textLen/2+r.IntN(textLen))
		for j := range text {
			text[j] = alphabet[r.IntN(len(alphabet))]
		}
		fmt.Fprintf(&buf, `{"ordinal":%d,"type":"response_item","payload":{"id":"msg_%d_%d","text":"%s"}}`+"\n", i, seed, i, text)
	}
	return buf.Bytes()
}

// scanAll returns every finalized chunk of b and the tail offset.
func scanAll(t *testing.T, p ChunkParams, b []byte) ([]Chunk, int64) {
	t.Helper()
	var out []Chunk
	tail, err := Scan(p, bytes.NewReader(b), 0, int64(len(b)), nil, func(c Chunk, _ []byte) error {
		out = append(out, c)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out, tail
}

var _ io.ReaderAt = (*bytes.Reader)(nil)
