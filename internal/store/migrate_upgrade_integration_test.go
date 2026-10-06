package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const busUpgradeDurableSnapshot = `SELECT jsonb_build_object(
 'messages', (SELECT jsonb_agg(to_jsonb(m)-'attempts'-'last_at' ORDER BY id) FROM bus_messages m),
 'audit', (SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM audit_events a),
 'users', (SELECT jsonb_agg(to_jsonb(u) ORDER BY id) FROM users u),
 'devices', (SELECT jsonb_agg(to_jsonb(d) ORDER BY id) FROM devices d),
 'accepts', (SELECT jsonb_agg(to_jsonb(a) ORDER BY recipient_user,sender_user) FROM bus_accepts a),
 'presence', (SELECT jsonb_agg(to_jsonb(p)-'idle_since' ORDER BY session_id) FROM bus_presence p)
)::text`

const busUpgradePriorLedgerSnapshot = `SELECT jsonb_agg(to_jsonb(m) ORDER BY name)::text
 FROM flopwire_schema_migrations m WHERE name <= '010_cass_recovery.sql'`
const busUpgradeFullLedgerSnapshot = `SELECT jsonb_agg(to_jsonb(m) ORDER BY name)::text
 FROM flopwire_schema_migrations m`

func busUpgradeSnapshot(t *testing.T, pool *pgxpool.Pool, query string) string {
	t.Helper()
	var got string
	if err := pool.QueryRow(context.Background(), query).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

func seedBusUpgrade(t *testing.T, legacy bool) (*pgxpool.Pool, []migrationFile) {
	t.Helper()
	ctx := context.Background()
	pool := newPool(t, pgtest.NewDatabase(t))
	all, prior := busUpgradeFiles(t, legacy)
	if err := migrate(ctx, pool, prior); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/bus_upgrade_data.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, string(raw)); err != nil {
		t.Fatal(err)
	}
	if !legacy {
		if _, err = pool.Exec(ctx, `UPDATE bus_messages SET attempts=3,last_at='2026-01-01T00:01:00Z' WHERE id='refusal'`); err != nil {
			t.Fatal(err)
		}
	}
	return pool, all
}

func TestBusUpgradePreservesDataAndLedger(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		name := "current"
		if legacy {
			name = "661a48b"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			pool, all := seedBusUpgrade(t, legacy)
			durable := busUpgradeSnapshot(t, pool, busUpgradeDurableSnapshot)
			ledger := busUpgradeSnapshot(t, pool, busUpgradePriorLedgerSnapshot)
			if err := Migrate(ctx, pool); err != nil {
				t.Fatal(err)
			}
			if got := busUpgradeSnapshot(t, pool, busUpgradeDurableSnapshot); got != durable {
				t.Fatal("upgrade changed durable data")
			}
			if got := busUpgradeSnapshot(t, pool, busUpgradePriorLedgerSnapshot); got != ledger {
				t.Fatal("upgrade changed historical checksums or applied_at")
			}
			var checksum string
			if err := pool.QueryRow(ctx, `SELECT checksum FROM flopwire_schema_migrations WHERE name=$1`, busMigrationName).Scan(&checksum); err != nil {
				t.Fatal(err)
			}
			want := busCanonicalChecksum
			if legacy {
				want = busLegacyChecksum
			}
			if checksum != want {
				t.Fatalf("009 checksum=%s want=%s", checksum, want)
			}
			var count int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM flopwire_schema_migrations`).Scan(&count); err != nil || count != len(all) {
				t.Fatalf("ledger rows=%d want=%d err=%v", count, len(all), err)
			}
			for _, file := range all {
				if file.name == busMigrationName {
					continue
				}
				if err := pool.QueryRow(ctx, `SELECT checksum FROM flopwire_schema_migrations WHERE name=$1`, file.name).Scan(&checksum); err != nil || checksum != file.checksum {
					t.Fatalf("%s checksum=%s err=%v", file.name, checksum, err)
				}
			}
			var defaults, refusalPreserved bool
			if err := pool.QueryRow(ctx, `SELECT bool_and(attempts=1 AND last_at IS NULL) FROM bus_messages WHERE id<>'refusal'`).Scan(&defaults); err != nil || !defaults {
				t.Fatalf("new defaults=%v err=%v", defaults, err)
			}
			wantAttempts := 3
			if legacy {
				wantAttempts = 1
			}
			if err := pool.QueryRow(ctx, `SELECT attempts=$1 AND (CASE WHEN $2 THEN last_at IS NULL ELSE last_at='2026-01-01T00:01:00Z' END) FROM bus_messages WHERE id='refusal'`, wantAttempts, legacy).Scan(&refusalPreserved); err != nil || !refusalPreserved {
				t.Fatalf("refusal preserved=%v err=%v", refusalPreserved, err)
			}

			fullLedger := busUpgradeSnapshot(t, pool, busUpgradeFullLedgerSnapshot)
			fullMessagesSQL := `SELECT jsonb_agg(to_jsonb(m) ORDER BY id)::text FROM bus_messages m`
			fullMessages := busUpgradeSnapshot(t, pool, fullMessagesSQL)
			if err := Migrate(ctx, pool); err != nil {
				t.Fatal(err)
			}
			if busUpgradeSnapshot(t, pool, busUpgradeFullLedgerSnapshot) != fullLedger || busUpgradeSnapshot(t, pool, busUpgradeDurableSnapshot) != durable || busUpgradeSnapshot(t, pool, fullMessagesSQL) != fullMessages {
				t.Fatal("second migration changed data or ledger")
			}
			_, prior := busUpgradeFiles(t, legacy)
			if err := migrate(ctx, pool, prior); err == nil || !strings.Contains(err.Error(), "older binary") {
				t.Fatalf("older binary accepted upgraded schema: %v", err)
			}
			// Also exercise direct SQL replay, independently of the ledger no-op.
			for _, file := range all {
				if file.name == busUpgradeName {
					if _, err := pool.Exec(ctx, file.sql); err != nil {
						t.Fatalf("013 SQL replay: %v", err)
					}
				}
			}
			if busUpgradeSnapshot(t, pool, busUpgradeDurableSnapshot) != durable || busUpgradeSnapshot(t, pool, fullMessagesSQL) != fullMessages || busUpgradeSnapshot(t, pool, busUpgradeFullLedgerSnapshot) != fullLedger {
				t.Fatal("013 SQL replay changed data")
			}

			for _, index := range []string{"bus_messages_retention_idx", "audit_bus_message_idx", "audit_bus_batch_idx"} {
				if !regclass(t, pool, index) {
					t.Fatalf("missing retention index %s", index)
				}
				var valid bool
				if err := pool.QueryRow(ctx, `SELECT indisvalid AND indpred IS NOT NULL FROM pg_index WHERE indexrelid=$1::regclass`, index).Scan(&valid); err != nil || !valid {
					t.Fatalf("index %s valid=%v err=%v", index, valid, err)
				}
			}
			for _, sql := range []string{
				`UPDATE bus_messages SET attempts=0 WHERE id='refusal'`,
				`UPDATE bus_messages SET attempts=2 WHERE id='reply'`,
				`UPDATE bus_messages SET attempts=NULL WHERE id='refusal'`,
			} {
				_, err := pool.Exec(ctx, sql)
				var pgerr *pgconn.PgError
				if !errors.As(err, &pgerr) || (pgerr.Code != "23514" && pgerr.Code != "23502") {
					t.Fatalf("invalid attempts accepted: %v", err)
				}
			}
			if _, err := pool.Exec(ctx, `UPDATE bus_messages SET attempts=attempts+1,last_at='2026-01-01T00:02:00Z' WHERE id='refusal'`); err != nil {
				t.Fatalf("refusal increment: %v", err)
			}
			var attempts int
			if err := pool.QueryRow(ctx, `SELECT sum(attempts)::int FROM bus_messages WHERE id='refusal' AND last_at='2026-01-01T00:02:00Z'`).Scan(&attempts); err != nil || attempts != wantAttempts+1 {
				t.Fatalf("refusal attempts=%d err=%v", attempts, err)
			}
			if _, err := pool.Exec(ctx, `UPDATE bus_messages SET state='queued' WHERE id='refusal'`); err == nil {
				t.Fatal("coalesced refusal became queued")
			}
			// Retention can remove a parent without deleting its surviving reply.
			if _, err := pool.Exec(ctx, `DELETE FROM bus_messages WHERE id='parent'`); err != nil {
				t.Fatal(err)
			}
			var survived bool
			if err := pool.QueryRow(ctx, `SELECT reply_to IS NULL AND body='durable reply' FROM bus_messages WHERE id='reply'`).Scan(&survived); err != nil || !survived {
				t.Fatalf("reply survived=%v err=%v", survived, err)
			}
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events`).Scan(&count); err != nil || count != 3 {
				t.Fatalf("migration/FK removed audits: %d %v", count, err)
			}
		})
	}
}

func TestBusUpgradeFailureIsAtomic(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		name := "current"
		if legacy {
			name = "661a48b"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			pool, all := seedBusUpgrade(t, legacy)
			durable := busUpgradeSnapshot(t, pool, busUpgradeDurableSnapshot)
			ledger := busUpgradeSnapshot(t, pool, busUpgradeFullLedgerSnapshot)
			messagesSQL := `SELECT jsonb_agg(to_jsonb(m) ORDER BY id)::text FROM bus_messages m`
			messages := busUpgradeSnapshot(t, pool, messagesSQL)
			fkSQL := `SELECT confdeltype::text FROM pg_constraint WHERE conrelid='bus_messages'::regclass AND conname='bus_messages_reply_to_fkey'`
			fk := busUpgradeSnapshot(t, pool, fkSQL)
			bad := append(append([]migrationFile(nil), all...), migrationFile{name: "999_failure.sql", checksum: "failure", sql: `CREATE TABLE migration_failure_probe(id int); UPDATE bus_messages SET body='damaged'; SELECT 1/0`})
			if err := migrate(ctx, pool, bad); err == nil || !strings.Contains(err.Error(), "999_failure.sql") {
				t.Fatalf("err=%v", err)
			}
			if busUpgradeSnapshot(t, pool, busUpgradeDurableSnapshot) != durable || busUpgradeSnapshot(t, pool, busUpgradeFullLedgerSnapshot) != ledger || busUpgradeSnapshot(t, pool, fkSQL) != fk || busUpgradeSnapshot(t, pool, messagesSQL) != messages {
				t.Fatal("failure changed data, ledger, or FK")
			}
			if regclass(t, pool, "migration_failure_probe") {
				t.Fatal("failure probe table committed")
			}
			var upgradeCheck bool
			if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='bus_messages'::regclass AND conname='bus_messages_refusal_attempts_check')`).Scan(&upgradeCheck); err != nil || upgradeCheck {
				t.Fatalf("013 check committed on failure: %v %v", upgradeCheck, err)
			}
			if legacy {
				var attempts bool
				if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='bus_messages' AND column_name='attempts')`).Scan(&attempts); err != nil || attempts {
					t.Fatalf("attempts committed on failure: %v %v", attempts, err)
				}
				if regclass(t, pool, "bus_messages_retention_idx") || regclass(t, pool, "audit_bus_message_idx") || regclass(t, pool, "audit_bus_batch_idx") {
					t.Fatal("retention indexes committed on failure")
				}
			}
			if err := Migrate(ctx, pool); err != nil {
				t.Fatalf("retry: %v", err)
			}
		})
	}
}

func TestBusUpgradeRejectsUnsafeBinaries(t *testing.T) {
	for _, name := range []string{"unknown009", "missing013", "edited013", "editedCanonical009"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			pool := newPool(t, pgtest.NewDatabase(t))
			all, prior := busUpgradeFiles(t, true)
			if name == "unknown009" {
				// Create the unknown applied checksum by running edited bytes, not
				// by changing a ledger row after application.
				prior[8].sql += "\n-- unknown historical edit\n"
				prior[8].checksum = fmt.Sprintf("%x", sha256.Sum256([]byte(prior[8].sql)))
			}
			if err := migrate(ctx, pool, prior); err != nil {
				t.Fatal(err)
			}
			ledger := busUpgradeSnapshot(t, pool, busUpgradeFullLedgerSnapshot)
			switch name {
			case "missing013":
				var without []migrationFile
				for _, f := range all {
					if f.name != busUpgradeName {
						without = append(without, f)
					}
				}
				all = without
			case "edited013":
				for i := range all {
					if all[i].name == busUpgradeName {
						all[i].sql = "SELECT 1"
						all[i].checksum = "noop"
					}
				}
			case "editedCanonical009":
				all[8].checksum = "edited canonical 009"
			}
			if err := migrate(ctx, pool, all); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
				t.Fatalf("unsafe binary err=%v", err)
			}
			if busUpgradeSnapshot(t, pool, busUpgradeFullLedgerSnapshot) != ledger {
				t.Fatal("refusal changed ledger")
			}
			if regclass(t, pool, "bus_messages_retention_idx") {
				t.Fatal("refusal applied pending upgrade")
			}
		})
	}
}
