package retrieval_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/ingest"
	"github.com/flopwire/flopwire/internal/redact"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestArchiveRepairTailAppend(t *testing.T) {
	ctx := context.Background()
	s := newServerChunks(t, devicesync.ChunkParams{Min: 1 << 20, Avg: 2 << 20, Max: 4 << 20})
	specs, dv, export := s.writeRedactFixtures()
	s.syncRedact(s.sy, specs, dv, export)
	var id string
	if err := s.pool.QueryRow(ctx, `SELECT m.id::text FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE c.agent='claude' AND strpos(m.text,'BLUEFALCON')>0`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	res, err := s.redact(s.client, "/v1/redactions", format.RedactRequest{Address: id + ":2-2", AllCopies: true})
	if err != nil || res.Tails != 3 {
		t.Fatalf("tail redaction: %+v %v", res, err)
	}
	for i := range 2 {
		f, err := os.OpenFile(specs[0].Path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.WriteString(jline(map[string]any{"type": "assistant", "uuid": fmt.Sprintf("tail-append-%d", i), "sessionId": specs[0].SessionKey, "message": map[string]any{"role": "assistant", "content": "after the redaction"}}))
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if err := s.sy.Sync(ctx, specs[0]); err != nil {
			t.Fatal(err)
		}
		// Simulate another worker process with the same durable database.
		worker := &ingest.Queue{Pool: s.pool, Objects: s.objects}
		if err := worker.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		if s.rowsWith("BLUEFALCON") != 0 || s.archiveHas("BLUEFALCON") {
			t.Fatal("append restored redacted tail")
		}
		if s.count(`SELECT count(*) FROM generations g JOIN sources s ON s.id=g.source_id WHERE s.path=$1`, specs[0].Path) != 1 {
			t.Fatal("redacted prefix forced a new generation instead of a full-tail retry")
		}
	}
	if s.count(`SELECT count(*) FROM archive_redaction_work`) != 0 {
		t.Fatal("repair work not cleared")
	}
}

// Future uploads are repaired even when multiple generations arrive before
// parsing; an older, unqueued archive is not scanned or backfilled.
func TestArchiveRepairFutureGenerations(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	specs, dv, export := s.writeRedactFixtures()
	s.syncRedact(s.sy, specs, dv, export)
	data, err := os.ReadFile(specs[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	uploader := &syncproto.Client{Server: s.client.Server, Token: s.client.Token, HTTP: http.DefaultClient}
	source := syncproto.Source{Path: "/w/companion-copy.jsonl", FileID: "archive-copy", Agent: "claude", StorageKind: "companion"}
	upload := func(gen int64, cut int) {
		t.Helper()
		h := syncproto.FlushHeader{Version: syncproto.Version, Generation: gen, CapturedAt: time.Now().UTC(), Source: source, Chunker: syncproto.ChunkerParams{Algorithm: devicesync.Algorithm, Min: 1024, Avg: 4096, Max: 16 << 10}}
		var wire bytes.Buffer
		parts := [][]byte{data}
		if cut > 0 {
			parts = [][]byte{data[:cut], data[cut:]}
		}
		var off int64
		for i, p := range parts {
			b, z := syncproto.EncodeBody(p)
			h.Bodies = append(h.Bodies, b)
			h.Entries = append(h.Entries, syncproto.Entry{Ordinal: int64(i), Hash: b.Hash, Offset: off, Size: b.Size})
			off += b.Size
			wire.Write(z)
		}
		res, err := uploader.Flush(ctx, &syncproto.FlushRequest{Header: h, Payload: bytes.NewReader(wire.Bytes())})
		if err != nil || res.Status != syncproto.StatusOK {
			t.Fatalf("upload: %+v %v", res, err)
		}
	}
	upload(0, 0)
	if err := s.queue.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if s.count(`SELECT count(*) FROM archive_redaction_work`) != 0 {
		t.Fatal("upload without known redactions queued repair")
	}
	var id string
	if err := s.pool.QueryRow(ctx, `SELECT m.id::text FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE c.agent='claude' AND strpos(m.text,'BLUEFALCON')>0`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.redact(s.client, "/v1/redactions", format.RedactRequest{Address: id + ":2-2", AllCopies: true}); err != nil {
		t.Fatal(err)
	}
	cut := bytes.Index(data, []byte("BLUEFALCON")) + 4
	upload(1, cut)
	upload(2, cut+2)
	if s.count(`SELECT count(*) FROM archive_redaction_work`) != 2 {
		t.Fatal("one work record required per pending generation")
	}
	// A fresh queue demonstrates durable work and a one-connection pool
	// catches plans that retain one connection while requesting another.
	config := s.pool.Config()
	config.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	worker := &ingest.Queue{Pool: pool, Objects: s.objects}
	drainCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := worker.Drain(drainCtx); err != nil {
		t.Fatal(err)
	}
	var src string
	if err := s.pool.QueryRow(ctx, `SELECT id::text FROM sources WHERE path=$1`, source.Path).Scan(&src); err != nil {
		t.Fatal(err)
	}
	for gen := int64(0); gen < 3; gen++ {
		g, err := ingest.LoadGeneration(ctx, s.pool, src, gen)
		if err != nil {
			t.Fatal(err)
		}
		r := ingest.NewReader(ctx, s.objects, g)
		raw, err := io.ReadAll(io.NewSectionReader(r, 0, r.Size()))
		if err != nil {
			t.Fatal(err)
		}
		holds := strings.Contains(string(raw), "BLUEFALCON")
		if holds != (gen == 0) {
			t.Fatalf("generation %d secret present=%v; only unqueued history should retain it", gen, holds)
		}
		if len(raw) != len(data) {
			t.Fatal("rewrite changed byte offsets")
		}
	}
	if s.count(`SELECT count(*) FROM archive_redaction_work`) != 0 {
		t.Fatal("work remains after repair")
	}
	for range 10 {
		n, err := s.store.ProcessDeletionJobs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	if s.count(`SELECT count(*) FROM chunks WHERE state='deletion_pending'`) != 0 {
		t.Fatal("rewritten chunks not purged")
	}
}

func TestArchiveRepairMixedChunkTail(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	data := []byte(jline(map[string]any{"type": "user", "uuid": "mixed-message", "sessionId": "mixed-session", "cwd": "/w/mixed", "message": map[string]any{"role": "user", "content": hidden}}))
	cut := bytes.Index(data, []byte("BLUEFALCON")) + 4
	source := syncproto.Source{Path: "/w/mixed.jsonl", FileID: "mixed", Agent: "claude", StorageKind: "jsonl_append", Parser: "claude-jsonl-v1"}
	// Use the repository's current parser name rather than a pinned contract.
	specs, _, _ := s.writeRedactFixtures()
	source.Parser = specs[0].Parser
	base := syncproto.FlushHeader{Version: syncproto.Version, CapturedAt: time.Now().UTC(), Source: source, Chunker: syncproto.ChunkerParams{Algorithm: devicesync.Algorithm, Min: 1024, Avg: 4096, Max: 16 << 10}}
	client := &syncproto.Client{Server: s.client.Server, Token: s.client.Token, HTTP: http.DefaultClient}
	b, z := syncproto.EncodeBody(data[:cut])
	first := base
	first.Entries = []syncproto.Entry{{Hash: b.Hash, Size: b.Size}}
	first.Bodies = []syncproto.Body{b}
	first.Tail = &syncproto.Tail{Offset: int64(cut), Size: int64(len(data) - cut), Hash: syncproto.Sum(data[cut:])}
	if _, err := client.Flush(ctx, &syncproto.FlushRequest{Header: first, Payload: bytes.NewReader(append(z, data[cut:]...))}); err != nil {
		t.Fatal(err)
	}
	if err := s.queue.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := s.pool.QueryRow(ctx, `SELECT id::text FROM messages WHERE strpos(text,'BLUEFALCON')>0`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.redact(s.client, "/v1/redactions", format.RedactRequest{Address: id + ":2-2"}); err != nil {
		t.Fatal(err)
	}
	extra := []byte(jline(map[string]any{"type": "assistant", "uuid": "mixed-append", "sessionId": "mixed-session", "message": map[string]any{"role": "assistant", "content": "done"}}))
	whole := append(bytes.Clone(data[cut:]), extra...)
	delta := base
	delta.Tail = &syncproto.Tail{Offset: int64(cut), From: int64(len(data) - cut), Size: int64(len(whole)), Hash: syncproto.Sum(whole)}
	resp, err := client.Flush(ctx, &syncproto.FlushRequest{Header: delta, Payload: bytes.NewReader(extra)})
	if err != nil || resp.Status != syncproto.StatusPartial || resp.TailOffset != -1 || resp.TailSize != 0 {
		t.Fatalf("full-tail hint: %+v %v", resp, err)
	}
	delta.Tail.From = 0
	if _, err := client.Flush(ctx, &syncproto.FlushRequest{Header: delta, Payload: bytes.NewReader(whole)}); err != nil {
		t.Fatal(err)
	}
	if err := s.queue.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if s.archiveHas("FALCON") || s.rowsWith("FALCON") != 0 {
		t.Fatal("raw suffix survived beside a previously masked chunk prefix")
	}
	if s.count(`SELECT count(*) FROM generations`) != 1 {
		t.Fatal("tail recovery forked the generation")
	}
}

func TestArchiveRewriteAbandonedPlan(t *testing.T) {
	ctx := context.Background()
	s := newServerChunks(t, devicesync.ChunkParams{Min: 64, Avg: 128, Max: 256})
	specs, dv, export := s.writeRedactFixtures()
	s.syncRedact(s.sy, specs, dv, export)
	data, err := os.ReadFile(specs[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	var source string
	if err := s.pool.QueryRow(ctx, `SELECT id::text FROM sources WHERE path=$1`, specs[0].Path).Scan(&source); err != nil {
		t.Fatal(err)
	}
	g, err := ingest.LoadGeneration(ctx, s.pool, source, 0)
	if err != nil {
		t.Fatal(err)
	}
	off := bytes.Index(data, []byte("BLUEFALCON"))
	plan, err := ingest.PrepareArchiveRewrite(ctx, s.pool, s.objects, []ingest.ArchiveMask{{Generation: g, Spans: []redact.Span{{Start: off, End: off + len("BLUEFALCON")}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	chunks, _ := plan.Counts()
	if chunks == 0 || s.count(`SELECT count(*) FROM chunks WHERE state='uploading'`) != chunks {
		t.Fatal("replacement not recorded before installation")
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, store.PurgeLockID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got {
		conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, store.PurgeLockID)
		t.Fatal("purge can enter during object replacement")
	}
	plan.Close() // Crash before Apply: references remain raw, replacement is orphaned.
	if !s.archiveHas("BLUEFALCON") {
		t.Fatal("abandoned plan changed references")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE chunks SET cleanup_after=now()-interval '1 second' WHERE state='uploading'`); err != nil {
		t.Fatal(err)
	}
	cleaned, err := s.store.ReconcileOrphanChunks(ctx, 100)
	if err != nil || cleaned != chunks || s.count(`SELECT count(*) FROM chunks WHERE state='uploading'`) != 0 {
		t.Fatalf("orphan cleanup: %d %v", cleaned, err)
	}
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, store.PurgeLockID).Scan(&got); err != nil || !got {
		t.Fatalf("purge fence not released: %v", err)
	}
	conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, store.PurgeLockID)
}
