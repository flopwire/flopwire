package retrieval_test

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/backup"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/ingest"
	"github.com/flopwire/flopwire/internal/retrieval"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/klauspost/compress/zstd"
	"github.com/minio/minio-go/v7"
)

var zdec, _ = zstd.NewReader(nil)

// At-rest repair of redacted lines (notes/redaction.md, step 5): a
// redaction racing an upload, a repair scan, or a parse that moves its
// target to a new generation must still leave no stored byte, in any
// generation or in any bucket object, that holds the redacted line.

// rawUpload flushes data as one generation of source, split into chunks at
// the given cuts, with the device credential. It does not parse.
func (s *server) rawUpload(source syncproto.Source, gen int64, data []byte, cuts ...int) {
	s.t.Helper()
	h := syncproto.FlushHeader{Version: syncproto.Version, Generation: gen, CapturedAt: time.Now().UTC(), Source: source,
		Chunker: syncproto.ChunkerParams{Algorithm: devicesync.Algorithm, Min: 1024, Avg: 4096, Max: 16 << 10}}
	var wire bytes.Buffer
	var off int64
	prev := 0
	for i, cut := range append(cuts, len(data)) {
		b, z := syncproto.EncodeBody(data[prev:cut])
		h.Bodies = append(h.Bodies, b)
		h.Entries = append(h.Entries, syncproto.Entry{Ordinal: int64(i), Hash: b.Hash, Offset: off, Size: b.Size})
		off += b.Size
		wire.Write(z)
		prev = cut
	}
	c := &syncproto.Client{Server: s.client.Server, Token: s.client.Token, HTTP: http.DefaultClient}
	res, err := c.Flush(context.Background(), &syncproto.FlushRequest{Header: h, Payload: bytes.NewReader(wire.Bytes())})
	if err != nil || res.Status != syncproto.StatusOK {
		s.t.Fatalf("upload: %+v %v", res, err)
	}
}

// drainRepairs runs q until nothing is pending. A repair that lost a race
// (ErrArchiveChanged) is retried, as the queue's backoff would.
func (s *server) drainRepairs(q *ingest.Queue) {
	s.t.Helper()
	for attempt := 0; ; attempt++ {
		err := q.Drain(context.Background())
		if err == nil {
			return
		}
		if !errors.Is(err, ingest.ErrArchiveChanged) || attempt == 5 {
			s.t.Fatal(err)
		}
	}
}

// purge runs the deletion worker until no job is left, then the orphan
// reconciler over reservations that are due.
func (s *server) purge() {
	s.t.Helper()
	defer func() {
		if _, err := s.store.ReconcileOrphanChunks(context.Background(), 1000); err != nil {
			s.t.Fatal(err)
		}
	}()
	for range 20 {
		n, err := s.store.ProcessDeletionJobs(context.Background())
		if err != nil {
			s.t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
}

// bucketHas reports whether any object in the bucket, decompressed,
// contains needle: referenced or not, the bytes must be gone.
func (s *server) bucketHas(needle string) bool {
	s.t.Helper()
	ctx := context.Background()
	for obj := range s.objects.Client.ListObjects(ctx, s.objects.Bucket, minio.ListObjectsOptions{Recursive: true}) {
		if obj.Err != nil {
			s.t.Fatal(obj.Err)
		}
		z, err := s.objects.Get(ctx, obj.Key)
		if err != nil {
			s.t.Fatal(err)
		}
		data, err := zdec.DecodeAll(z, nil)
		if err != nil {
			s.t.Fatalf("object %s: %v", obj.Key, err)
		}
		if bytes.Contains(data, []byte(needle)) {
			var state string
			var refs int
			s.pool.QueryRow(ctx, `SELECT state||' job='||COALESCE(deletion_job_id::text,'-'),(SELECT count(*) FROM manifest_entries m WHERE m.chunk_hash=c.hash) FROM chunks c WHERE object_key=$1`, obj.Key).Scan(&state, &refs)
			s.t.Logf("bucket object %s holds %q (chunk %q, %d manifest entries)", obj.Key, needle, state, refs)
			return true
		}
	}
	return false
}

// requireNoSecretAtRest checks every generation and every bucket object
// after the deletion worker ran.
func (s *server) requireNoSecretAtRest(needle string) {
	s.t.Helper()
	s.purge()
	if s.archiveHas(needle) {
		s.t.Fatalf("a stored generation still holds %q", needle)
	}
	if s.bucketHas(needle) {
		s.t.Fatalf("a bucket object still holds %q", needle)
	}
	if n := s.rowsWith(needle); n != 0 {
		s.t.Fatalf("%d rows hold %q", n, needle)
	}
	if n := s.count(`SELECT count(*) FROM archive_redaction_work`); n != 0 {
		s.t.Fatalf("%d repair records left", n)
	}
}

// hiddenMessage is the Claude row holding the codename.
func (s *server) hiddenMessage() string {
	s.t.Helper()
	var id string
	if err := s.pool.QueryRow(context.Background(), `SELECT m.id::text FROM messages m JOIN conversations c ON c.id=m.conversation_id
		WHERE c.agent='claude' AND strpos(m.text,'BLUEFALCON')>0 AND NOT m.superseded`).Scan(&id); err != nil {
		s.t.Fatal(err)
	}
	return id
}

// redactFiller redacts an unrelated message, so that redacted lines exist
// before the redaction under test (uploads then record repair work).
func (s *server) redactFiller() {
	s.t.Helper()
	var id string
	if err := s.pool.QueryRow(context.Background(), `SELECT m.id::text FROM messages m JOIN conversations c ON c.id=m.conversation_id
		WHERE c.agent='claude' AND strpos(m.text,'filler 7 ')>0`).Scan(&id); err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.redact(s.client, "/v1/redactions", format.RedactRequest{Address: id}); err != nil {
		s.t.Fatal(err)
	}
	s.drainRepairs(s.queue)
}

// Race (a), first redaction: another device's copy of the session arrives
// after the redaction picked its targets and before it commits. No
// redacted line existed when the upload committed, so the upload recorded
// no repair; its parse masks the rows, but the stored chunks kept the line.
func TestAtRestUploadDuringFirstRedaction(t *testing.T) {
	s := newServer(t)
	specs, dv, export := s.writeRedactFixtures()
	s.syncRedact(s.sy, specs, dv, export)
	data, err := os.ReadFile(specs[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	addr := s.hiddenMessage()
	fired := 0
	retrieval.SetBeforeRedactTx(func() {
		fired++
		if fired == 1 {
			s.rawUpload(syncproto.Source{Path: "/w/other-laptop.jsonl", FileID: "copy:1", Agent: "claude", StorageKind: "jsonl_append", Parser: specs[0].Parser}, 0, data)
		}
	})
	defer retrieval.SetBeforeRedactTx(nil)
	if _, err := s.redact(s.client, "/v1/redactions", format.RedactRequest{Address: addr + ":2-2", AllCopies: true}); err != nil {
		t.Fatal(err)
	}
	if fired == 0 {
		t.Fatal("hook did not run")
	}
	// Until the repair ran, a backup would copy the unrepaired chunks.
	if _, err := s.backup(); !errors.Is(err, backup.ErrRedactionRepairPending) {
		t.Fatalf("backup during a pending repair: %v", err)
	}
	s.drainRepairs(s.queue)
	s.requireNoSecretAtRest("BLUEFALCON")
	if _, err := exec.LookPath("pg_dump"); err != nil {
		t.Log("pg_dump is not on PATH: backup contents not checked")
		return
	}
	dir, err := s.backup()
	if err != nil {
		t.Fatal(err)
	}
	if err := filepath.WalkDir(filepath.Join(dir, "objects"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		z, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		data, err := zdec.DecodeAll(z, nil)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte("BLUEFALCON")) {
			t.Errorf("backup object %s holds the redacted line", p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// backup takes a backup of the server into a new directory.
func (s *server) backup() (string, error) {
	dir := filepath.Join(s.t.TempDir(), "backup")
	_, err := backup.Create(context.Background(), s.pool.Config().ConnString(), s.objects.Client, s.objects.Bucket, dir, backup.Options{EncryptedDestination: true})
	return dir, err
}

// gatedObjects pauses the first object read until released.
type gatedObjects struct {
	ingest.Objects
	once             sync.Once
	reached, release chan struct{}
}

func (g *gatedObjects) Get(ctx context.Context, key string) ([]byte, error) {
	g.once.Do(func() {
		close(g.reached)
		<-g.release
	})
	return g.Objects.Get(ctx, key)
}

// Race (a), repair scan: an upload after an earlier redaction records
// repair work. Its worker loads the catalog and starts scanning; the new
// redaction commits meanwhile. The scan, with the old catalog, finds
// nothing and clears the work.
func TestAtRestRepairScanRacingRedaction(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	specs, dv, export := s.writeRedactFixtures()
	s.syncRedact(s.sy, specs, dv, export)
	s.redactFiller()
	data, err := os.ReadFile(specs[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	s.rawUpload(syncproto.Source{Path: "/w/other-laptop.jsonl", FileID: "copy:1", Agent: "claude", StorageKind: "jsonl_append", Parser: specs[0].Parser}, 0, data)
	if s.count(`SELECT count(*) FROM archive_redaction_work`) != 1 {
		t.Fatal("upload after a redaction recorded no repair")
	}
	gate := &gatedObjects{Objects: s.objects, reached: make(chan struct{}), release: make(chan struct{})}
	worker := &ingest.Queue{Pool: s.pool, Objects: gate}
	done := make(chan error, 1)
	go func() { done <- worker.Drain(ctx) }()
	select {
	case <-gate.reached:
	case err := <-done:
		t.Fatalf("worker finished without reading the archive: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("worker never read the archive")
	}
	_, rerr := s.redact(s.client, "/v1/redactions", format.RedactRequest{Address: s.hiddenMessage() + ":2-2", AllCopies: true})
	close(gate.release)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if err := <-done; err != nil && !errors.Is(err, ingest.ErrArchiveChanged) {
		t.Fatal(err)
	}
	s.drainRepairs(worker)
	s.requireNoSecretAtRest("BLUEFALCON")
}

// Race (b): while the redaction runs, the device uploads a new generation
// of the target's source (chunked differently) and the parse moves the
// target row to it with the same text. The redaction rewrites the
// generation it planned; the new one must be repaired too.
func TestAtRestTargetMovedToNewGeneration(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	specs, dv, export := s.writeRedactFixtures()
	s.syncRedact(s.sy, specs, dv, export)
	s.redactFiller()
	data, err := os.ReadFile(specs[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	src := syncproto.Source{Agent: "claude", StorageKind: "jsonl_append", Parser: specs[0].Parser, SessionKey: specs[0].SessionKey}
	var gen int64
	if err := s.pool.QueryRow(ctx, `SELECT path,file_id,(SELECT max(generation) FROM generations WHERE source_id=s.id) FROM sources s WHERE path=$1`, specs[0].Path).Scan(&src.Path, &src.FileID, &gen); err != nil {
		t.Fatal(err)
	}
	addr := s.hiddenMessage()
	cut := bytes.Index(data, []byte("BLUEFALCON")) + 4
	fired := 0
	retrieval.SetBeforeRedactTx(func() {
		fired++
		if fired > 1 {
			return
		}
		s.rawUpload(src, gen+1, data, cut, cut+700)
		if err := s.queue.Drain(ctx); err != nil {
			t.Error(err)
		}
	})
	defer retrieval.SetBeforeRedactTx(nil)
	if _, err := s.redact(s.client, "/v1/redactions", format.RedactRequest{Address: addr + ":2-2", AllCopies: true}); err != nil {
		t.Fatal(err)
	}
	if fired == 0 {
		t.Fatal("hook did not run")
	}
	t.Logf("redaction attempts: %d", fired)
	if n := s.count(`SELECT count(*) FROM messages m JOIN sources src ON src.id=m.source_id WHERE src.path=$1 AND m.source_generation=$2`, specs[0].Path, gen+1); n == 0 {
		t.Fatal("the parse did not move the rows to the new generation")
	}
	s.drainRepairs(s.queue)
	s.requireNoSecretAtRest("BLUEFALCON")
}
