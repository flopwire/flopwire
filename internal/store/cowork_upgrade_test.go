package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
)

func coworkUpgradeSnapshot(t *testing.T, pool *pgxpool.Pool, tables []string) map[string][32]byte {
	t.Helper()
	snap := make(map[string][32]byte, len(tables))
	for _, table := range tables {
		expr, where := "to_jsonb(r)", ""
		if table == "conversations" {
			expr = "to_jsonb(r)-'hidden_scope'"
		}
		if table == "flopwire_schema_migrations" {
			where = " WHERE name<'014'"
		}
		var rows string
		query := fmt.Sprintf(`SELECT COALESCE(jsonb_agg(v ORDER BY v::text),'[]'::jsonb)::text FROM (SELECT %s v FROM %s r%s) data`, expr, pgx.Identifier{table}.Sanitize(), where)
		if err := pool.QueryRow(context.Background(), query).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		snap[table] = sha256.Sum256([]byte(rows))
	}
	return snap
}

func coworkPGTool(t *testing.T, tool, url string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := osexec.CommandContext(ctx, tool, args...)
	// Connection details and credentials stay out of argv and logs.
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "PG") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	config, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid isolated test database connection")
	}
	sslMode := "disable"
	if config.TLSConfig != nil {
		sslMode = "require"
	}
	cmd.Env = append(cmd.Env, "PGDATABASE="+config.Database, "PGHOST="+config.Host, fmt.Sprintf("PGPORT=%d", config.Port), "PGUSER="+config.User, "PGPASSWORD="+config.Password, "PGSSLMODE="+sslMode, "PGCONNECT_TIMEOUT=10")
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s failed: %v (connection details suppressed)", filepath.Base(tool), err)
	}
}

func TestCowork014DumpRestoreUpgrade(t *testing.T) {
	dumpTool, err := osexec.LookPath("pg_dump")
	if err != nil {
		t.Skip("pg_dump unavailable: upgrade qualification not executed")
	}
	restoreTool, err := osexec.LookPath("pg_restore")
	if err != nil {
		t.Skip("pg_restore unavailable: upgrade qualification not executed")
	}
	ctx := context.Background()
	files, err := migrationFiles(migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 14 || files[13].name != "014_cowork_policy.sql" {
		t.Fatal("exact embedded 014 migration missing")
	}
	prior := files[:13]
	sourceURL := pgtest.NewDatabase(t)
	source := newPool(t, sourceURL)
	if err = migrate(ctx, source, prior); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile("testdata/bus_upgrade_data.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = source.Exec(ctx, string(fixture)); err != nil {
		t.Fatal(err)
	}
	data := []byte("synthetic archived Cowork upgrade evidence\n")
	body, compressed := syncproto.EncodeBody(data)
	const archiveFixture = `
 INSERT INTO sources(id,device_id,agent,path,file_id,session_key,storage_kind,parser,first_seen_at)
 VALUES('10000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000003','claude','/synthetic/session.jsonl','1:2','synthetic-upgrade','jsonl_append','claude@1','2026-01-01');
 INSERT INTO generations(source_id,generation,size,captured_at,complete) VALUES('10000000-0000-0000-0000-000000000001',0,45,'2026-01-01',true);
 INSERT INTO provisional_tails(source_id,generation,byte_offset,bytes,updated_at) VALUES('10000000-0000-0000-0000-000000000001',0,44,decode('0a','hex'),'2026-01-01');
 INSERT INTO conversations(id,source_id,agent,session_id,device_id,user_id,cwd,extra) VALUES('10000000-0000-0000-0000-000000000002','10000000-0000-0000-0000-000000000001','claude','synthetic-upgrade','00000000-0000-0000-0000-000000000003','00000000-0000-0000-0000-000000000001','/synthetic','{"durable":true}');
 INSERT INTO message_redactions(id,requested_by,device_id,message_id,all_copies,by_admin,messages,chunks,tails,created_at) VALUES('10000000-0000-0000-0000-000000000003','00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000003','10000000-0000-0000-0000-000000000004',true,true,1,1,1,'2026-01-01');
 INSERT INTO chunk_redirects(old_hash,new_hash,redaction_id) VALUES(decode(repeat('01',32),'hex'),decode(repeat('02',32),'hex'),'10000000-0000-0000-0000-000000000003');
 INSERT INTO redacted_lines(line_sha,spans,redaction_id) VALUES(decode(repeat('03',32),'hex'),'[[0,4]]','10000000-0000-0000-0000-000000000003');
 INSERT INTO conversation_tombstones(id,user_id,device_id,agent,session_id,conversation_id,requested_by,requested_at) VALUES('10000000-0000-0000-0000-000000000005','00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000003','claude','deleted-synthetic','10000000-0000-0000-0000-000000000006','00000000-0000-0000-0000-000000000001','2026-01-01');
 UPDATE collection_policy SET path_rules='["deny /synthetic/private"]',unplaceable='local',max_storage_bytes=100000,updated_by='00000000-0000-0000-0000-000000000001',updated_at='2026-01-01';`
	if _, err = source.Exec(ctx, archiveFixture); err != nil {
		t.Fatal(err)
	}
	masked := "[REDACTED]"
	maskedHash := sha256.Sum256([]byte(masked))
	if _, err = source.Exec(ctx, `INSERT INTO messages(id,conversation_id,source_id,native_id,ordinal,kind,role,text,text_len,content_sha,source_generation,parser)
 VALUES('10000000-0000-0000-0000-000000000004','10000000-0000-0000-0000-000000000002','10000000-0000-0000-0000-000000000001','synthetic-message',0,'user','user',$1,$2,$3,0,'claude@1')`, masked, len(masked), maskedHash[:]); err != nil {
		t.Fatal(err)
	}
	if _, err = source.Exec(ctx, `INSERT INTO chunks(hash,size,stored_size,object_key,uploaded_by_device) VALUES($1,$2,$3,'synthetic-upgrade-chunk','00000000-0000-0000-0000-000000000003')`, body.Hash[:], len(data), len(compressed)); err != nil {
		t.Fatal(err)
	}
	if _, err = source.Exec(ctx, `INSERT INTO manifest_entries(source_id,generation,ordinal,chunk_hash,byte_offset) VALUES('10000000-0000-0000-0000-000000000001',0,0,$1,0)`, body.Hash[:]); err != nil {
		t.Fatal(err)
	}
	// Align raw generation offsets with the exact synthetic chunk bytes.
	if _, err = source.Exec(ctx, `UPDATE generations SET size=$1`, len(data)+1); err != nil {
		t.Fatal(err)
	}
	if _, err = source.Exec(ctx, `UPDATE provisional_tails SET byte_offset=$1`, len(data)); err != nil {
		t.Fatal(err)
	}
	rows, err := source.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY tablename`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var table string
		if err = rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	before := coworkUpgradeSnapshot(t, source, tables)
	mc, bucket := pgtest.NewBucket(t)
	if _, err := mc.PutObject(ctx, bucket, "synthetic-upgrade-chunk", bytes.NewReader(compressed), int64(len(compressed)), minio.PutObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "synthetic-013.dump")
	coworkPGTool(t, dumpTool, sourceURL, "--format=custom", "--no-owner", "--no-acl", "--file", archive)
	restoredURL := pgtest.NewDatabase(t)
	coworkPGTool(t, restoreTool, restoredURL, "--dbname", restoredDatabaseName(t, restoredURL), "--no-owner", "--no-acl", "--exit-on-error", archive)
	restored := newPool(t, restoredURL)
	if got := coworkUpgradeSnapshot(t, restored, tables); !reflect.DeepEqual(got, before) {
		t.Fatal("dump/restore changed pre014 table data or ledger")
	}
	// A failing exact014 transaction cannot alter existing data or record014.
	broken := append([]migrationFile(nil), files[:14]...)
	broken[13].sql += "\nUPDATE collection_policy SET max_storage_bytes=1; SELECT 1/0;"
	if err = migrate(ctx, restored, broken); err == nil {
		t.Fatal("broken014 accepted")
	}
	if got := coworkUpgradeSnapshot(t, restored, tables); !reflect.DeepEqual(got, before) {
		t.Fatal("failed014 changed pre014 rows")
	}
	if regclass(t, restored, "session_policy_placements") {
		t.Fatal("failed014 committed schema")
	}
	if err = Migrate(ctx, restored); err != nil {
		t.Fatal(err)
	}
	if got := coworkUpgradeSnapshot(t, restored, tables); !reflect.DeepEqual(got, before) {
		t.Fatal("014 changed existing rows,checksums,or applied_at")
	}
	var storedRules []byte
	var ruleLines []string
	if err = restored.QueryRow(ctx, `SELECT path_rules FROM collection_policy WHERE singleton`).Scan(&storedRules); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(storedRules, &ruleLines); err != nil {
		t.Fatal("restored collection policy is not valid stored rule strings")
	}
	parsedRules, err := pathpolicy.ParseRules(ruleLines)
	if err != nil || len(parsedRules) != 1 || parsedRules[0].Mode != pathpolicy.Deny {
		t.Fatalf("restored restrictive collection policy invalid: %v", err)
	}
	var wrong, count int
	if err = restored.QueryRow(ctx, `SELECT count(*) FROM conversations WHERE hidden_scope<>'user'`).Scan(&wrong); err != nil || wrong != 0 {
		t.Fatalf("hidden_scope default: %d %v", wrong, err)
	}
	if err = restored.QueryRow(ctx, `SELECT count(*) FROM flopwire_schema_migrations`).Scan(&count); err != nil || count != len(files) {
		t.Fatalf("ledger count %d %v", count, err)
	}
	fullLedger := busUpgradeSnapshot(t, restored, busUpgradeFullLedgerSnapshot)
	restored.Close()
	restored = newPool(t, restoredURL)
	if err = Migrate(ctx, restored); err != nil {
		t.Fatal(err)
	}
	if busUpgradeSnapshot(t, restored, busUpgradeFullLedgerSnapshot) != fullLedger {
		t.Fatal("reopen changed ledger")
	}
	if err = migrate(ctx, restored, prior); err == nil || !strings.Contains(err.Error(), "older binary") {
		t.Fatalf("old013 must refuse newer ledger: %v", err)
	}
	if got := coworkUpgradeSnapshot(t, restored, tables); !reflect.DeepEqual(got, before) {
		t.Fatal("old013 refusal changed data")
	}
	if busUpgradeSnapshot(t, restored, busUpgradeFullLedgerSnapshot) != fullLedger {
		t.Fatal("old013 refusal changed ledger")
	}
	t.Run("actual schema013 CLI refuses014", func(t *testing.T) {
		oldBinary := os.Getenv("FLOPWIRE_UPGRADE_OLD_BINARY")
		if oldBinary == "" {
			t.Skip("FLOPWIRE_UPGRADE_OLD_BINARY unset: actual old CLI refusal not qualified")
		}
		probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		cmd := osexec.CommandContext(probeCtx, oldBinary, "serve", "--tls=off", "--addr=127.0.0.1:0")
		for _, env := range os.Environ() {
			if !strings.HasPrefix(env, "DATABASE_URL=") && !strings.HasPrefix(env, "S3_") && !strings.HasPrefix(env, "FLOPWIRE_") {
				cmd.Env = append(cmd.Env, env)
			}
		}
		cmd.Env = append(cmd.Env, "DATABASE_URL="+restoredURL, "S3_ENDPOINT=127.0.0.1:1", "S3_ACCESS_KEY=synthetic", "S3_SECRET_KEY=synthetic", "S3_BUCKET=synthetic", "S3_SECURE=false")
		output, probeErr := cmd.CombinedOutput()
		if probeCtx.Err() != nil {
			t.Fatal("old CLI failed to refuse schema014 within15seconds")
		}
		if probeErr == nil || !bytes.Contains(output, []byte("older binary")) || !bytes.Contains(output, []byte("migration ledger")) {
			t.Fatal("old CLI did not return required unknown-newer-ledger refusal; output suppressed")
		}
		if got := coworkUpgradeSnapshot(t, restored, tables); !reflect.DeepEqual(got, before) {
			t.Fatal("old CLI refusal changed existing data")
		}
		if busUpgradeSnapshot(t, restored, busUpgradeFullLedgerSnapshot) != fullLedger {
			t.Fatal("old CLI refusal changed full migration ledger")
		}
	})
	t.Run("synthetic MinIO readback", func(t *testing.T) {
		var key string
		var hash []byte
		if err := restored.QueryRow(ctx, `SELECT object_key,hash FROM chunks`).Scan(&key, &hash); err != nil {
			t.Fatal(err)
		}
		obj, err := mc.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer obj.Close()
		got, err := io.ReadAll(obj)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, compressed) || !bytes.Equal(hash, body.Hash[:]) {
			t.Fatal("restored chunk metadata or object bytes differ")
		}
	})
}

func restoredDatabaseName(t *testing.T, url string) string {
	t.Helper()
	config, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid isolated test database connection")
	}
	return config.Database
}
