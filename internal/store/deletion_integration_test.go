package store

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/domain"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

// fakeObjects records removals and can fail them.
type fakeObjects struct {
	mu      sync.Mutex
	removed []string
	fail    error
}

func (f *fakeObjects) PutObject(context.Context, string, string, io.Reader, int64, minio.PutObjectOptions) (minio.UploadInfo, error) {
	return minio.UploadInfo{}, nil
}
func (f *fakeObjects) GetObject(context.Context, string, string, minio.GetObjectOptions) (*minio.Object, error) {
	return nil, errors.New("not supported")
}
func (f *fakeObjects) RemoveObject(_ context.Context, _ string, key string, _ minio.RemoveObjectOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.removed = append(f.removed, key)
	return nil
}
func (f *fakeObjects) BucketExists(context.Context, string) (bool, error) { return true, nil }

// deletionFixture: conversation A on source 1 (chunks a, shared), conversation
// B on source 2 (chunk shared). Deleting A must purge source 1 and chunk a but
// keep the shared chunk for B.
type deletionFixture struct {
	schemaFixture
	objects                 *fakeObjects
	p                       *Postgres
	convA, convB, source2   string
	chunkA, chunkShared     []byte
	keyA, keyShared, keyOld string
}

func newDeletionFixture(t *testing.T) deletionFixture {
	f := deletionFixture{schemaFixture: newSchemaFixture(t), objects: &fakeObjects{}}
	f.p = NewPostgres(f.pool, f.objects, "bucket")
	f.convA, f.convB, f.source2 = f.conv, uuid.NewString(), uuid.NewString()
	f.chunkA, f.chunkShared = hash32("a"), hash32("shared")
	f.keyA, f.keyShared = "chunks/a", "chunks/shared"
	now := time.Now().UTC()
	exec(t, f.pool, `INSERT INTO chunks(hash,size,stored_size,object_key) VALUES($1,10,10,'chunks/shared')`, f.chunkShared)
	exec(t, f.pool, `INSERT INTO manifest_entries(source_id,generation,ordinal,chunk_hash,byte_offset) VALUES($1,0,1,$2,100)`, f.source, f.chunkShared)
	exec(t, f.pool, `INSERT INTO sources(id,device_id,agent,path,file_id,storage_kind,parser,first_seen_at) VALUES($1,$2,'codex','/b.jsonl','1:3','jsonl_append','codex@1',$3)`, f.source2, f.device, now)
	exec(t, f.pool, `INSERT INTO generations(source_id,generation,size,captured_at,complete) VALUES($1,0,10,$2,true)`, f.source2, now)
	exec(t, f.pool, `INSERT INTO manifest_entries(source_id,generation,ordinal,chunk_hash,byte_offset) VALUES($1,0,0,$2,0)`, f.source2, f.chunkShared)
	exec(t, f.pool, `INSERT INTO conversations(id,source_id,agent,session_id,device_id,user_id) VALUES($1,$2,'codex','sess-2',$3,$4)`, f.convB, f.source2, f.device, f.user)
	f.message(t, "m1", "to be deleted", false, nil)
	return f
}

func (f deletionFixture) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestConversationDeletionTombstonesAndPurgesOnlyUnsharedEvidence(t *testing.T) {
	ctx := context.Background()
	f := newDeletionFixture(t)
	if _, err := f.p.RequestConversationDeletion(ctx, uuid.NewString(), f.user, f.device, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown conversation err=%v", err)
	}
	if _, err := f.p.RequestConversationDeletion(ctx, "not-a-uuid", f.user, f.device, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("malformed id err=%v", err)
	}
	job, err := f.p.RequestConversationDeletion(ctx, f.convA, f.user, f.device, false)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != domain.DeletionQueued || job.SessionID != "sess-1" || job.Agent != "codex" {
		t.Fatalf("job=%+v", job)
	}
	again, err := f.p.RequestConversationDeletion(ctx, f.convA, f.user, f.device, false)
	if err != nil || again.ID != job.ID {
		t.Fatalf("repeat request job=%s err=%v, want %s", again.ID, err, job.ID)
	}
	// Invisible at once: rows gone, tombstone present, raw evidence untouched.
	if n := f.count(t, `SELECT count(*) FROM messages WHERE conversation_id=$1`, f.convA); n != 0 {
		t.Fatalf("messages left=%d", n)
	}
	if n := f.count(t, `SELECT count(*) FROM conversation_tombstones WHERE user_id=$1 AND agent='codex' AND session_id='sess-1'`, f.user); n != 1 {
		t.Fatalf("tombstones=%d", n)
	}
	if n := f.count(t, `SELECT count(*) FROM manifest_entries WHERE source_id=$1`, f.source); n != 2 {
		t.Fatalf("manifest purged before the worker ran: %d", n)
	}

	done, err := f.p.ProcessDeletionJobs(ctx)
	if err != nil || done != 1 {
		t.Fatalf("process done=%d err=%v", done, err)
	}
	if n := f.count(t, `SELECT count(*) FROM sources WHERE id=$1 AND tombstoned_at IS NOT NULL`, f.source); n != 1 {
		t.Fatal("orphaned source is not kept as a tombstone")
	}
	if n := f.count(t, `SELECT count(*) FROM generations WHERE source_id=$1`, f.source); n != 0 {
		t.Fatal("orphaned source kept its generations")
	}
	if n := f.count(t, `SELECT count(*) FROM sources WHERE id=$1`, f.source2); n != 1 {
		t.Fatal("unrelated source was removed")
	}
	if n := f.count(t, `SELECT count(*) FROM chunks WHERE hash=$1`, f.chunkA); n != 0 {
		t.Fatal("unshared chunk row survived")
	}
	if n := f.count(t, `SELECT count(*) FROM chunks WHERE hash=$1 AND state='committed'`, f.chunkShared); n != 1 {
		t.Fatal("shared chunk was purged")
	}
	if len(f.objects.removed) != 1 || f.objects.removed[0] != f.keyA {
		t.Fatalf("removed objects=%v", f.objects.removed)
	}
	final, err := f.p.DeletionJobByID(ctx, job.ID)
	if err != nil || final.State != domain.DeletionComplete || final.CompletedAt.IsZero() {
		t.Fatalf("final=%+v err=%v", final, err)
	}
	if n := f.count(t, `SELECT count(*) FROM audit_events WHERE action IN ('conversation.deletion.requested','conversation.deletion.complete')`); n != 2 {
		t.Fatalf("deletion audits=%d", n)
	}
	if done, err = f.p.ProcessDeletionJobs(ctx); err != nil || done != 0 {
		t.Fatalf("idle pass done=%d err=%v", done, err)
	}
}

func TestConversationDeletionRetriesObjectStoreFailures(t *testing.T) {
	ctx := context.Background()
	f := newDeletionFixture(t)
	job, err := f.p.RequestConversationDeletion(ctx, f.convA, f.user, f.device, false)
	if err != nil {
		t.Fatal(err)
	}
	f.objects.fail = errors.New("minio down")
	if _, err = f.p.ProcessDeletionJobs(ctx); err == nil {
		t.Fatal("object failure was not reported")
	}
	waiting, _ := f.p.DeletionJobByID(ctx, job.ID)
	if waiting.State != domain.DeletionRetryWait || waiting.LastError != "object_store_unavailable" {
		t.Fatalf("after failure=%+v", waiting)
	}
	if n := f.count(t, `SELECT count(*) FROM chunks WHERE hash=$1 AND state='deletion_pending' AND deletion_job_id=$2`, f.chunkA, job.ID); n != 1 {
		t.Fatal("failed attempt released chunk ownership")
	}
	if _, err = f.p.RetryDeletionJob(ctx, job.ID, f.user, f.device); !errors.Is(err, ErrConflict) {
		t.Fatalf("retry of a waiting job err=%v", err)
	}
	// Exhaust attempts, then an administrator retries the failed job.
	exec(t, f.pool, `UPDATE deletion_jobs SET attempts=5,next_attempt_at=now()-interval '1 second' WHERE id=$1`, job.ID)
	if _, err = f.p.ProcessDeletionJobs(ctx); err == nil {
		t.Fatal("sixth attempt should fail")
	}
	failed, _ := f.p.DeletionJobByID(ctx, job.ID)
	if failed.State != domain.DeletionFailed {
		t.Fatalf("state=%s, want failed", failed.State)
	}
	f.objects.fail = nil
	if _, err = f.p.RetryDeletionJob(ctx, job.ID, f.user, f.device); err != nil {
		t.Fatal(err)
	}
	if done, err := f.p.ProcessDeletionJobs(ctx); err != nil || done != 1 {
		t.Fatalf("retry pass done=%d err=%v", done, err)
	}
	if n := f.count(t, `SELECT count(*) FROM chunks WHERE hash=$1`, f.chunkA); n != 0 {
		t.Fatal("chunk survived the successful retry")
	}
}

func TestOrphanChunkReconcilerDeletesOnlyUnreferencedUploads(t *testing.T) {
	ctx := context.Background()
	f := newDeletionFixture(t)
	orphan, adopted := hash32("orphan"), hash32("adopted")
	exec(t, f.pool, `INSERT INTO chunks(hash,size,stored_size,object_key,state,cleanup_after) VALUES($1,5,5,'chunks/orphan','uploading',now()-interval '1 minute')`, orphan)
	exec(t, f.pool, `INSERT INTO chunks(hash,size,stored_size,object_key,state,cleanup_after) VALUES($1,5,5,'chunks/adopted','uploading',now()-interval '1 minute')`, adopted)
	exec(t, f.pool, `INSERT INTO manifest_entries(source_id,generation,ordinal,chunk_hash,byte_offset) VALUES($1,0,5,$2,500)`, f.source2, adopted)
	exec(t, f.pool, `INSERT INTO chunks(hash,size,stored_size,object_key,state,cleanup_after) VALUES($1,5,5,'chunks/fresh','uploading',now()+interval '1 hour')`, hash32("fresh"))

	f.objects.fail = errors.New("minio down")
	if _, err := f.p.ReconcileOrphanChunks(ctx, 10); err == nil {
		t.Fatal("object failure was not reported")
	}
	if n := f.count(t, `SELECT count(*) FROM chunks WHERE hash=$1 AND state='cleanup_pending' AND cleanup_after>now()`, orphan); n != 1 {
		t.Fatal("failed cleanup was not rescheduled")
	}
	exec(t, f.pool, `UPDATE chunks SET cleanup_after=now()-interval '1 second' WHERE hash=$1`, orphan)
	f.objects.fail = nil
	cleaned, err := f.p.ReconcileOrphanChunks(ctx, 10)
	if err != nil || cleaned != 1 {
		t.Fatalf("cleaned=%d err=%v", cleaned, err)
	}
	if n := f.count(t, `SELECT count(*) FROM chunks WHERE hash=$1`, orphan); n != 0 {
		t.Fatal("orphan row survived")
	}
	if n := f.count(t, `SELECT count(*) FROM chunks WHERE hash=$1 AND state='committed'`, adopted); n != 1 {
		t.Fatal("a referenced upload was not committed")
	}
	if n := f.count(t, `SELECT count(*) FROM chunks WHERE hash=$1 AND state='uploading'`, hash32("fresh")); n != 1 {
		t.Fatal("an in-grace upload was touched")
	}
	if len(f.objects.removed) != 1 || f.objects.removed[0] != "chunks/orphan" {
		t.Fatalf("removed=%v", f.objects.removed)
	}
}

func TestBackupSharedPurgeLockBlocksDeletionWorker(t *testing.T) {
	ctx := context.Background()
	f := newDeletionFixture(t)
	if _, err := f.p.RequestConversationDeletion(ctx, f.convA, f.user, f.device, false); err != nil {
		t.Fatal(err)
	}
	conn, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err = conn.Exec(ctx, `SELECT pg_advisory_lock_shared($1)`, purgeLockID); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if _, err = f.p.ProcessDeletionJobs(waitCtx); err == nil {
		t.Fatal("deletion worker ran while a backup held the purge lock")
	}
	if _, err = conn.Exec(ctx, `SELECT pg_advisory_unlock_shared($1)`, purgeLockID); err != nil {
		t.Fatal(err)
	}
	if done, err := f.p.ProcessDeletionJobs(ctx); err != nil || done != 1 {
		t.Fatalf("after backup done=%d err=%v", done, err)
	}
}

// D9: a delete forgets the session for its user on every device, cascades
// to subagent conversations (linked or not yet linked), drops the
// session's provisional tails at once, and a member may delete only their
// own conversations.
func TestConversationDeletionCascadesAcrossDevicesAndSubagents(t *testing.T) {
	ctx := context.Background()
	f := newDeletionFixture(t)
	now := time.Now().UTC()
	laptop2, conv2, child, orphanChild, stranger := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	exec(t, f.pool, `INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$2,'laptop-2','darwin',$3)`, laptop2, f.user, now)
	// The same session synced from the user's second laptop.
	exec(t, f.pool, `INSERT INTO conversations(id,agent,session_id,device_id,user_id) VALUES($1,'codex','sess-1',$2,$3)`, conv2, laptop2, f.user)
	// A linked subagent and one still waiting for its parent's id.
	exec(t, f.pool, `INSERT INTO conversations(id,agent,session_id,device_id,user_id,parent_conversation_id,parent_native_session_id,depth) VALUES($1,'codex','sess-1-child',$2,$3,$4,'sess-1',1)`, child, f.device, f.user, f.convA)
	exec(t, f.pool, `INSERT INTO conversations(id,agent,session_id,device_id,user_id,parent_native_session_id,depth) VALUES($1,'codex','sess-1-late',$2,$3,'sess-1',1)`, orphanChild, laptop2, f.user)
	// Another user's conversation of the same session id stays.
	exec(t, f.pool, `INSERT INTO conversations(id,agent,session_id,device_id,user_id) VALUES($1,'codex','sess-1',$2,$3)`, stranger, f.otherDevice, f.otherUser)

	if _, err := f.p.RequestConversationDeletion(ctx, f.convA, f.otherUser, f.otherDevice, true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member deleting another user's conversation: %v", err)
	}
	job, err := f.p.RequestConversationDeletion(ctx, f.convA, f.user, f.device, true)
	if err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM conversations WHERE id=ANY($1::uuid[])`, []string{f.convA, conv2, child, orphanChild}); n != 0 {
		t.Fatalf("%d doomed conversations left", n)
	}
	if n := f.count(t, `SELECT count(*) FROM conversations WHERE id=$1 OR id=$2`, stranger, f.convB); n != 2 {
		t.Fatalf("unrelated conversations deleted: %d left", n)
	}
	if n := f.count(t, `SELECT count(*) FROM conversation_tombstones WHERE user_id=$1 AND job_id=$2`, f.user, job.ID); n != 3 {
		t.Fatalf("tombstones: %d, want the session and its two subagents", n)
	}
	if n := f.count(t, `SELECT count(*) FROM provisional_tails WHERE source_id=$1`, f.source); n != 0 {
		t.Fatal("the deleted session's tail survived the request")
	}
	// Deleting a subagent of an already deleted session answers its job.
	again, err := f.p.RequestConversationDeletion(ctx, child, f.user, f.device, true)
	if err != nil || again.ID != job.ID {
		t.Fatalf("repeat on a cascaded child: %v %v", again.ID, err)
	}
}

// Two concurrent requests to delete the same conversation (a double
// click) both get the one job: the second waits on the session lock and
// must then see the first's tombstone, not fail on it.
func TestConcurrentDuplicateDeletionIsIdempotent(t *testing.T) {
	ctx := context.Background()
	f := newDeletionFixture(t)
	hold, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Release()
	key := ConversationLockKey(f.user, "codex", "sess-1")
	if _, err = hold.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1,0))`, key); err != nil {
		t.Fatal(err)
	}
	type result struct {
		job domain.DeletionJob
		err error
	}
	out := make(chan result, 2)
	for range 2 {
		go func() {
			job, err := f.p.RequestConversationDeletion(ctx, f.convA, f.user, f.device, true)
			out <- result{job, err}
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for f.count(t, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted`) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("requests never waited on the session lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err = hold.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, key); err != nil {
		t.Fatal(err)
	}
	a, b := <-out, <-out
	if a.err != nil || b.err != nil {
		t.Fatalf("concurrent duplicate deletion: %v / %v", a.err, b.err)
	}
	if a.job.ID != b.job.ID {
		t.Fatalf("two jobs for one conversation: %s %s", a.job.ID, b.job.ID)
	}
}
