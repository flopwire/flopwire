package backup

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"github.com/zeebo/blake3"
)

// TestBackupVerifyRestoreRoundTrip needs Postgres, S3, and pg_dump and
// pg_restore on PATH whose major version is at least the server's.
func TestBackupVerifyRestoreRoundTrip(t *testing.T) {
	for _, tool := range []string{"pg_dump", "pg_restore"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " is not on PATH")
		}
	}
	ctx := context.Background()
	sourceURL := pgtest.NewDatabase(t)
	objects, bucket := pgtest.NewBucket(t)
	pool, err := pgxpool.New(ctx, sourceURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	// Two committed chunks referenced by a manifest, one orphan upload that
	// must stay out of the backup.
	user, device, source := uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now().UTC()
	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	mustExec(`INSERT INTO users(id,email,name,role,identity_type,created_at) VALUES($1,'a@example.test','A','admin','human',$2)`, user, now)
	mustExec(`INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$2,'mac','darwin',$3)`, device, user, now)
	mustExec(`INSERT INTO sources(id,device_id,agent,path,file_id,storage_kind,parser,first_seen_at) VALUES($1,$2,'codex','/r.jsonl','1:2','jsonl_append','codex@1',$3)`, source, device, now)
	mustExec(`INSERT INTO generations(source_id,generation,size,captured_at,complete) VALUES($1,0,0,$2,true)`, source, now)
	put := func(content, state string) []byte {
		t.Helper()
		sum := blake3.Sum256([]byte(content))
		key := "chunks/" + uuid.NewString()
		z := syncproto.Compress(nil, []byte(content))
		if _, err := objects.PutObject(ctx, bucket, key, bytes.NewReader(z), int64(len(z)), minio.PutObjectOptions{}); err != nil {
			t.Fatal(err)
		}
		mustExec(`INSERT INTO chunks(hash,size,stored_size,object_key,state,cleanup_after) VALUES($1,$2,$3,$4,$5,CASE WHEN $5='committed' THEN NULL ELSE now() END)`, sum[:], len(content), len(z), key, state)
		return sum[:]
	}
	a := put(`{"type":"message","text":"cobalt heron"}`+"\n", "committed")
	b := put(`{"type":"message","text":"amber otter"}`+"\n", "committed")
	put("never committed", "uploading")
	mustExec(`INSERT INTO manifest_entries(source_id,generation,ordinal,chunk_hash,byte_offset) VALUES($1,0,0,$2,0),($1,0,1,$3,40)`, source, a, b)

	// V2, D9: evidence of a deleted conversation stays out. A chunk that
	// only a tombstoned source references, and a chunk a failed deletion
	// job owns whose object is already gone (it made every backup fail).
	dead, tombstone, job := uuid.NewString(), uuid.NewString(), uuid.NewString()
	mustExec(`INSERT INTO sources(id,device_id,agent,path,file_id,storage_kind,parser,first_seen_at,tombstoned_at) VALUES($1,$2,'codex','/dead.jsonl','1:3','jsonl_append','codex@1',$3,$3)`, dead, device, now)
	mustExec(`INSERT INTO generations(source_id,generation,size,captured_at,complete) VALUES($1,0,0,$2,true)`, dead, now)
	c := put(`{"type":"message","text":"deleted words"}`+"\n", "committed")
	mustExec(`INSERT INTO manifest_entries(source_id,generation,ordinal,chunk_hash,byte_offset) VALUES($1,0,0,$2,0),($1,0,1,$3,40)`, dead, c, a)
	mustExec(`INSERT INTO conversation_tombstones(id,user_id,device_id,agent,session_id,conversation_id,requested_by,requested_at) VALUES($1,$2,$3,'codex','dead',$4,$2,$5)`, tombstone, user, device, uuid.NewString(), now)
	mustExec(`INSERT INTO deletion_jobs(id,tombstone_id,requested_by,state,attempts,requested_at) VALUES($1,$2,$3,'failed',5,$4)`, job, tombstone, user, now)
	gone := blake3.Sum256([]byte("purged already"))
	mustExec(`INSERT INTO chunks(hash,size,stored_size,object_key,state,deletion_job_id) VALUES($1,14,14,'chunks/purged-already','deletion_pending',$2)`, gone[:], job)

	dir := filepath.Join(t.TempDir(), "backup")
	if _, err = Create(ctx, sourceURL, objects, bucket, dir, Options{}); err == nil {
		t.Fatal("backup without the encrypted-destination acknowledgement succeeded")
	}
	manifest, err := Create(ctx, sourceURL, objects, bucket, dir, Options{EncryptedDestination: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Objects) != 2 {
		t.Fatalf("backup objects=%d, want the 2 committed chunks of the live source", len(manifest.Objects))
	}
	if _, err = Verify(dir); err != nil {
		t.Fatal(err)
	}

	// A target that is not empty is refused before anything changes.
	statePath := filepath.Join(t.TempDir(), "restore-state.json")
	if err = Restore(ctx, sourceURL, objects, bucket, dir, statePath); err == nil {
		t.Fatal("restore into the populated source succeeded")
	}
	targetURL := pgtest.NewDatabase(t)
	targetObjects, targetBucket := pgtest.NewBucket(t)
	if err = Restore(ctx, targetURL, targetObjects, targetBucket, dir, statePath); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state State
	if err = json.Unmarshal(raw, &state); err != nil || state.Status != "complete" {
		t.Fatalf("restore state=%s err=%v", raw, err)
	}
	target, err := pgxpool.New(ctx, targetURL)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	var users, manifests, chunks int
	if err = target.QueryRow(ctx, `SELECT (SELECT count(*) FROM users),(SELECT count(*) FROM manifest_entries),(SELECT count(*) FROM chunks)`).Scan(&users, &manifests, &chunks); err != nil {
		t.Fatal(err)
	}
	if users != 1 || manifests != 4 || chunks != 5 {
		t.Fatalf("restored users=%d manifests=%d chunks=%d", users, manifests, chunks)
	}
	// The restored schema is recognized by the migration ledger.
	if err = store.Migrate(ctx, target); err != nil {
		t.Fatalf("migrate restored database: %v", err)
	}
	for _, entry := range manifest.Objects {
		obj, err := targetObjects.GetObject(ctx, targetBucket, entry.Key, minio.GetObjectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(obj)
		_ = obj.Close()
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(body)) != entry.Size {
			t.Fatalf("restored object %s: %d bytes, want the %d stored", entry.Key, len(body), entry.Size)
		}
		data, err := syncproto.DecompressAny(body, entry.RawSize)
		if err != nil {
			t.Fatal(err)
		}
		sum := blake3.Sum256(data)
		if hex.EncodeToString(sum[:]) != entry.BLAKE3 {
			t.Fatalf("restored object %s does not match its content address", entry.Key)
		}
	}
}
