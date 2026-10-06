package bus_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/bus"
	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/flopwire/flopwire/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Build the prior production schema and its real checksum ledger from
// unchanged embedded files, populate synthetic durable data, then run the
// actual migration runner. No migration checksum is reset or rewritten.
func TestDeliveryReliabilityUpgradePreservesProductionState(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, pgtest.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `CREATE TABLE flopwire_schema_migrations(name text PRIMARY KEY,checksum text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(migrations.Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	checksums := make(map[string]string)
	priorCount := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		raw, err := migrations.Files.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		sha := sha256.Sum256(raw)
		checksums[name] = hex.EncodeToString(sha[:])
		if name >= "011_" {
			continue
		}
		priorCount++
		if _, err := tx.Exec(ctx, string(raw)); err != nil {
			t.Fatalf("prior migration %s: %v", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO flopwire_schema_migrations(name,checksum) VALUES($1,$2)`, name, checksums[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, pool: pool, now: time.Now().UTC().Truncate(time.Second)}
	user := f.user("synthetic")
	device := f.device(user)
	f.exec(`INSERT INTO bus_presence(device_id,user_id,agent,session_id,repo,branch,title,busy,seen_at)
 VALUES($1,$2,'claude','source-1111','/synthetic','main','synthetic',false,$3)`, device.DeviceID, user, f.now)
	for _, n := range []struct{ id, state, reason string }{{"mlegacyqueued", "queued", ""}, {"mlegacyfailed", "undelivered", "unconfirmed"}} {
		f.exec(`INSERT INTO bus_messages(id,thread_id,from_user,from_device,from_agent,from_session,to_user,to_agent,to_session,addressed,sender,intent,body,body_sha,state,reason,created_at,expires_at)
   VALUES($1,$1,$2,$3,'claude','source-1111',$2,'codex','dest-2222','session','own','inform','synthetic-upgrade-private',decode(repeat('00',32),'hex'),$4,$5,$6,$7)`, n.id, user, device.DeviceID, n.state, n.reason, f.now, f.now.Add(busproto.DefaultTTL))
	}
	before := f.count(`SELECT count(*) FROM flopwire_schema_migrations`)
	if before != priorCount {
		t.Fatalf("prior migration count: got %d, want %d", before, priorCount)
	}
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("idempotent migration: %v", err)
	}
	if got := f.count(`SELECT count(*) FROM flopwire_schema_migrations`); got != len(checksums) {
		t.Fatalf("migration count: got %d, want %d embedded migrations", got, len(checksums))
	}
	rows, err := pool.Query(ctx, `SELECT name,checksum FROM flopwire_schema_migrations ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, checksum string
		if err := rows.Scan(&name, &checksum); err != nil {
			t.Fatal(err)
		}
		if want, ok := checksums[name]; !ok || checksum != want {
			t.Fatalf("migration %s: checksum %q, want embedded %q (known=%t)", name, checksum, want, ok)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if f.count(`SELECT count(*) FROM bus_messages WHERE body='synthetic-upgrade-private'`) != 2 || f.count(`SELECT count(*) FROM bus_presence WHERE session_id='source-1111'`) != 1 {
		t.Fatal("upgrade lost durable state")
	}
	if f.count(`SELECT count(*) FROM bus_delivery_failures WHERE message_id='mlegacyfailed' AND reason='unconfirmed'`) != 1 {
		t.Fatal("existing failure not backfilled")
	}
	f.s = &bus.Store{Pool: pool, Now: func() time.Time { return f.now }}
	got := f.present(device, live("source-1111", "claude", "/synthetic", false))
	if len(got.Failures) != 1 || got.Failures[0].ID != "mlegacyfailed" {
		t.Fatalf("backfilled status: %+v", got.Failures)
	}
	encoded, _ := json.Marshal(got.Failures)
	if strings.Contains(string(encoded), "synthetic-upgrade-private") {
		t.Fatal("upgrade status exposed body")
	}
}
