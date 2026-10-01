package backup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A backup copies chunk objects verbatim. While a message redaction's
// at-rest repair is pending for a live source, those objects may still
// hold the redacted line, so the backup is refused unless the operator
// overrides it. Pending work of a tombstoned source does not count: its
// chunks stay out of the backup.
func TestBackupRefusesPendingRedactionRepair(t *testing.T) {
	ctx := context.Background()
	url := pgtest.NewDatabase(t)
	objects, bucket := pgtest.NewBucket(t)
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	user, device, live, dead := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now().UTC()
	mustExec(`INSERT INTO users(id,email,name,role,identity_type,created_at) VALUES($1,'a@example.test','A','admin','human',$2)`, user, now)
	mustExec(`INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$2,'mac','darwin',$3)`, device, user, now)
	mustExec(`INSERT INTO sources(id,device_id,agent,path,file_id,storage_kind,parser,first_seen_at) VALUES($1,$2,'codex','/live.jsonl','1:2','jsonl_append','codex@1',$3)`, live, device, now)
	mustExec(`INSERT INTO sources(id,device_id,agent,path,file_id,storage_kind,parser,first_seen_at,tombstoned_at) VALUES($1,$2,'codex','/dead.jsonl','1:3','jsonl_append','codex@1',$3,$3)`, dead, device, now)
	for _, src := range []string{live, dead} {
		mustExec(`INSERT INTO generations(source_id,generation,size,captured_at,complete) VALUES($1,0,0,$2,true)`, src, now)
		mustExec(`INSERT INTO archive_redaction_work(source_id,generation,from_offset) VALUES($1,0,0)`, src)
	}
	create := func(opts Options) (string, error) {
		dir := filepath.Join(t.TempDir(), "backup")
		_, err := Create(ctx, url, objects, bucket, dir, opts)
		return dir, err
	}

	dir, err := create(Options{EncryptedDestination: true})
	if !errors.Is(err, ErrRedactionRepairPending) {
		t.Fatalf("backup with a pending repair: %v", err)
	}
	raw, rerr := os.ReadFile(filepath.Join(dir, "state.json"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if state.Status != "failed" || state.Error != "redaction_repair_pending" {
		t.Fatalf("state: %+v", state)
	}
	if _, err := os.Stat(filepath.Join(dir, "postgres.dump")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused backup dumped the database: %v", err)
	}

	// The override goes past the check (pg_dump may be missing here).
	if _, err := create(Options{EncryptedDestination: true, AllowPendingRedactionRepair: true}); errors.Is(err, ErrRedactionRepairPending) {
		t.Fatal("override refused")
	}
	// Only a tombstoned source's work left: not refused.
	mustExec(`DELETE FROM archive_redaction_work WHERE source_id=$1`, live)
	if _, err := create(Options{EncryptedDestination: true}); errors.Is(err, ErrRedactionRepairPending) {
		t.Fatal("a tombstoned source's work refused the backup")
	}
}
