package ingest

import (
	"bytes"
	"context"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/auth"
	"github.com/flopwire/flopwire/internal/domain"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func flushOne(t *testing.T, e *env, path string, data []byte) *syncproto.FlushResponse {
	t.Helper()
	resp, err := tryFlush(e, path, data)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func tryFlush(e *env, path string, data []byte) (*syncproto.FlushResponse, error) {
	h := syncproto.Sum(data)
	return e.client.Flush(context.Background(), &syncproto.FlushRequest{
		Header: syncproto.FlushHeader{Version: syncproto.Version, CapturedAt: time.Now(),
			Source:  syncproto.Source{Path: path, FileID: "1:1", Agent: "codex", StorageKind: "jsonl_append"},
			Entries: []syncproto.Entry{{Ordinal: 0, Hash: h, Offset: 0, Size: int64(len(data))}},
			Bodies:  bodyOf(data)},
		Payload: zpayload(data)})
}

// V2: a chunk that a failed deletion job still owns (its object possibly
// already removed) no longer blocks a device that uploads the same bytes:
// the upload takes the chunk back and stores the object again.
func TestUploadReclaimsDeletionPendingChunk(t *testing.T) {
	e := newEnv(t)
	data := []byte("{\"same\":\"bytes as a deleted session's\"}\n")
	h := syncproto.Sum(data)
	tombstone, job := uuid.NewString(), uuid.NewString()
	e.exec(`INSERT INTO conversation_tombstones(id,user_id,device_id,agent,session_id,conversation_id,requested_by,requested_at) VALUES($1,$2,$3,'codex','gone',$4,$2,now())`, tombstone, e.userID, e.deviceID, uuid.NewString())
	e.exec(`INSERT INTO deletion_jobs(id,tombstone_id,requested_by,state,attempts,requested_at) VALUES($1,$2,$3,'failed',5,now())`, job, tombstone, e.userID)
	e.exec(`INSERT INTO chunks(hash,size,stored_size,object_key,state,deletion_job_id) VALUES($1,$2,$2,$3,'deletion_pending',$4)`, h[:], len(data), ChunkKey(h), job)

	if r := flushOne(t, e, "/live.jsonl", data); r.Status != syncproto.StatusOK || r.AckedEntries != 1 {
		t.Fatalf("flush: %+v", r)
	}
	if n := e.count(`SELECT count(*) FROM chunks WHERE hash=$1 AND state='committed' AND deletion_job_id IS NULL`, h[:]); n != 1 {
		t.Fatal("chunk not taken back from the deletion job")
	}
	got, err := GetChunk(context.Background(), e.objects, Chunk{Hash: h, Size: int64(len(data)), Key: ChunkKey(h)})
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("object %q %v", got, err)
	}
}

// V4: a failing parse backs off even while its session keeps flushing,
// and after MaxAttempts failures the source is quarantined: no longer
// retried, not counted as backlog, listed for the administrator, and
// released by them.
func TestParseFailureBackoffAndQuarantine(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	q := &Queue{Pool: e.pool, Objects: e.objects, Log: e.queue.Log, MaxAttempts: 3}
	q.init()
	// A generation whose only chunk object is missing: every parse fails.
	source := uuid.NewString()
	h := syncproto.Sum([]byte("missing object"))
	e.exec(`INSERT INTO sources(id,device_id,agent,path,file_id,storage_kind,parser,first_seen_at) VALUES($1,$2,'codex','/broken.jsonl','9:9','jsonl_append','codex@1',now())`, source, e.deviceID)
	e.exec(`INSERT INTO generations(source_id,generation,size,captured_at,complete) VALUES($1,0,14,now(),true)`, source)
	e.exec(`INSERT INTO chunks(hash,size,stored_size,object_key) VALUES($1,14,14,$2)`, h[:], ChunkKey(h))
	e.exec(`INSERT INTO manifest_entries(source_id,generation,ordinal,chunk_hash,byte_offset) VALUES($1,0,0,$2,0)`, source, h[:])
	e.exec(`INSERT INTO source_parse_state(source_id) VALUES($1)`, source)

	q.work(ctx, source)
	state := func() (attempts int, backedOff, quarantined bool) {
		t.Helper()
		if err := e.pool.QueryRow(ctx, `SELECT attempts,COALESCE(next_attempt_at>now(),false),quarantined_at IS NOT NULL FROM source_parse_state WHERE source_id=$1`, source).Scan(&attempts, &backedOff, &quarantined); err != nil {
			t.Fatal(err)
		}
		return
	}
	if a, b, qd := state(); a != 1 || !b || qd {
		t.Fatalf("after one failure: attempts=%d backoff=%v quarantined=%v", a, b, qd)
	}
	// A new flush of the source keeps the failure count and the backoff.
	if err := pgx.BeginTxFunc(ctx, e.pool, pgx.TxOptions{}, func(tx pgx.Tx) error { return requestParse(ctx, tx, source, false) }); err != nil {
		t.Fatal(err)
	}
	if a, b, _ := state(); a != 1 || !b {
		t.Fatalf("a flush reset the backoff: attempts=%d backoff=%v", a, b)
	}
	q.work(ctx, source)
	q.work(ctx, source)
	if a, _, qd := state(); a != 3 || !qd {
		t.Fatalf("after MaxAttempts failures: attempts=%d quarantined=%v", a, qd)
	}
	e.exec(`UPDATE source_parse_state SET next_attempt_at=now()-interval '1 second' WHERE source_id=$1`, source)
	q.sweep(ctx)
	if pending, _, _, _, quarantined := q.Status(); pending != 0 || quarantined != 1 {
		t.Fatalf("status: pending=%d quarantined=%d", pending, quarantined)
	}
	if len(q.ch) != 0 {
		t.Fatal("the sweep queued a quarantined source")
	}
	list, err := q.Quarantined(ctx, 10)
	if err != nil || len(list) != 1 || list[0].SourceID != source || list[0].Attempts != 3 || list[0].LastError == "" {
		t.Fatalf("quarantined: %+v %v", list, err)
	}
	ok, err := q.Release(ctx, source, domain.AuditEvent{ID: uuid.NewString(), Action: "source.parse.released", CreatedAt: time.Now()})
	if err != nil || !ok {
		t.Fatalf("release: %v %v", ok, err)
	}
	if a, _, qd := state(); a != 0 || qd {
		t.Fatalf("after release: attempts=%d quarantined=%v", a, qd)
	}
}

// V6: a chunk another request is storing is skipped, not waited for, so
// two requests carrying the same chunks in opposite orders cannot
// deadlock. The skipped chunk comes back as Missing and the device sends
// it again.
func TestBusyChunkIsMissingNotAwaited(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	data := []byte("{\"shared\":\"chunk\"}\n")
	h := syncproto.Sum(data)
	other, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Release()
	if _, err := other.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1,0))`, store.ChunkLockKey(h[:])); err != nil {
		t.Fatal(err)
	}
	done := make(chan *syncproto.FlushResponse, 1)
	go func() {
		r, err := tryFlush(e, "/busy.jsonl", data)
		if err != nil {
			r = &syncproto.FlushResponse{Status: syncproto.FlushStatus(err.Error())}
		}
		done <- r
	}()
	select {
	case r := <-done:
		if r.Status != syncproto.StatusPartial || len(r.Missing) != 1 || r.Missing[0] != h || r.AckedEntries != 0 {
			t.Fatalf("busy chunk: %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the flush waited for another request's chunk lock")
	}
	if _, err := other.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, store.ChunkLockKey(h[:])); err != nil {
		t.Fatal(err)
	}
	if r := flushOne(t, e, "/busy.jsonl", data); r.Status != syncproto.StatusOK || r.AckedEntries != 1 {
		t.Fatalf("retry: %+v", r)
	}
}

// D10 (proof of possession): has answers per user, and a hash alone never
// references another user's chunk; the body must come along.
func TestHasAndBodilessEntriesArePerUser(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	data := []byte("{\"secret\":\"only user one has this\"}\n")
	h := syncproto.Sum(data)
	if r := flushOne(t, e, "/one.jsonl", data); r.Status != syncproto.StatusOK {
		t.Fatalf("user one: %+v", r)
	}
	if missing, err := e.client.Has(ctx, []syncproto.Hash{h}); err != nil || len(missing) != 0 {
		t.Fatalf("owner's has: %v %v", missing, err)
	}
	// A second user's device.
	user2, device2 := uuid.NewString(), uuid.NewString()
	plain, hash, _ := auth.NewToken()
	e.exec(`INSERT INTO users(id,email,name,role,identity_type,created_at) VALUES($1,'two@example.test','Two','member','human',now())`, user2)
	e.exec(`INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$2,'box','linux',now())`, device2, user2)
	e.exec(`INSERT INTO credentials(id,user_id,device_id,kind,token_hash,created_at) VALUES($1,$2,$3,'device',$4,now())`, uuid.NewString(), user2, device2, hash)
	two := &syncproto.Client{Server: e.http.URL, Token: plain, HTTP: e.http.Client()}
	if missing, err := two.Has(ctx, []syncproto.Hash{h}); err != nil || len(missing) != 1 {
		t.Fatalf("has leaked another user's chunk: %v %v", missing, err)
	}
	hdr := syncproto.FlushHeader{Version: syncproto.Version, CapturedAt: time.Now(),
		Source:  syncproto.Source{Path: "/two.jsonl", FileID: "1:1", Agent: "codex", StorageKind: "jsonl_append"},
		Entries: []syncproto.Entry{{Ordinal: 0, Hash: h, Offset: 0, Size: int64(len(data))}}}
	r, err := two.Flush(ctx, &syncproto.FlushRequest{Header: hdr})
	if err != nil || r.Status != syncproto.StatusPartial || len(r.Missing) != 1 || r.AckedEntries != 0 {
		t.Fatalf("bodiless reference to another user's chunk: %+v %v", r, err)
	}
	hdr.Bodies = bodyOf(data)
	r, err = two.Flush(ctx, &syncproto.FlushRequest{Header: hdr, Payload: zpayload(data)})
	if err != nil || r.Status != syncproto.StatusOK || r.AckedEntries != 1 {
		t.Fatalf("with the body: %+v %v", r, err)
	}
	if missing, err := two.Has(ctx, []syncproto.Hash{h}); err != nil || len(missing) != 0 {
		t.Fatalf("after proving possession: %v %v", missing, err)
	}
}

// A chunk is stored as the zstd frame the device sent: the object is
// stored_size bytes, smaller than the chunk for text, marked zstd, and
// decodes to the chunk. Reads check the decoded bytes against the
// content address: an object replaced by a valid frame of other bytes is
// refused.
func TestChunkStoredCompressed(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	data := []byte(strings.Repeat(`{"type":"message","text":"the same words again"}`+"\n", 200))
	h := syncproto.Sum(data)
	if r := flushOne(t, e, "/z.jsonl", data); r.Status != syncproto.StatusOK {
		t.Fatalf("flush: %+v", r)
	}
	var size, stored int
	var enc string
	if err := e.pool.QueryRow(ctx, `SELECT size,stored_size,encoding FROM chunks WHERE hash=$1`, h[:]).Scan(&size, &stored, &enc); err != nil {
		t.Fatal(err)
	}
	obj, err := e.objects.Get(ctx, ChunkKey(h))
	if err != nil {
		t.Fatal(err)
	}
	if size != len(data) || stored != len(obj) || stored*10 > size || enc != "zstd" {
		t.Fatalf("size %d stored %d (object %d bytes) encoding %q", size, stored, len(obj), enc)
	}
	c := Chunk{Hash: h, Size: int64(len(data)), Key: ChunkKey(h)}
	if got, err := GetChunk(ctx, e.objects, c); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read back: %v", err)
	}
	other := bytes.ToUpper(data)
	if err := e.objects.Put(ctx, ChunkKey(h), syncproto.Compress(nil, other)); err != nil {
		t.Fatal(err)
	}
	if _, err := GetChunk(ctx, e.objects, c); err == nil {
		t.Fatal("an object decoding to other bytes was read")
	}
}

// Quotas count what object storage holds: a chunk's compressed object
// (stored_size), not its uncompressed size. A body padded past its chunk
// (a zstd skippable frame decodes to nothing) is counted at its stored
// length, so padding cannot store more than the quota allows.
func TestQuotaCountsStoredSize(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	data := []byte(strings.Repeat(`{"type":"message","text":"the same words again"}`+"\n", 200))
	h := syncproto.Sum(data)
	pad := make([]byte, 8+(256<<10)) // skippable frame: magic, length, payload
	binary.LittleEndian.PutUint32(pad[0:], 0x184D2A50)
	binary.LittleEndian.PutUint32(pad[4:], uint32(len(pad)-8))
	z := append(syncproto.Compress(nil, data), pad...)
	resp, err := e.client.Flush(ctx, &syncproto.FlushRequest{
		Header: syncproto.FlushHeader{Version: syncproto.Version, CapturedAt: time.Now(),
			Source:  syncproto.Source{Path: "/pad.jsonl", FileID: "1:1", Agent: "codex", StorageKind: "jsonl_append"},
			Entries: []syncproto.Entry{{Ordinal: 0, Hash: h, Offset: 0, Size: int64(len(data))}},
			Bodies:  []syncproto.Body{{Hash: h, Size: int64(len(data)), ZSize: int64(len(z))}}},
		Payload: bytes.NewReader(z)})
	if err == nil && resp.Status == syncproto.StatusOK {
		var stored int
		if err := e.pool.QueryRow(ctx, `SELECT stored_size FROM chunks WHERE hash=$1`, h[:]).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if used := e.count(`SELECT COALESCE(sum(bytes),0)::int FROM storage_usage`); used < stored {
			t.Errorf("storage usage %d bytes for an object of %d (chunk %d bytes)", used, stored, len(data))
		}
	}
	// An honest chunk counts its compressed size too.
	e2 := newEnv(t)
	flushOne(t, e2, "/z.jsonl", data)
	var stored int
	if err := e2.pool.QueryRow(ctx, `SELECT stored_size FROM chunks WHERE hash=$1`, h[:]).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if used := e2.count(`SELECT COALESCE(sum(bytes),0)::int FROM storage_usage`); used != stored {
		t.Errorf("storage usage %d bytes, want the stored size %d (chunk %d bytes)", used, stored, len(data))
	}
}
