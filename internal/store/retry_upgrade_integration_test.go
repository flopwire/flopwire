package store

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Historical snapshots compare the fields present before their upgrades.
// Check the new retry fields separately so excluding additive keys cannot
// hide an incorrect default or a fabricated successor receipt.
func assertLegacyRetryDefaults(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var wrong, receipts int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE retry_of IS NOT NULL OR admission<>''), (SELECT count(*) FROM bus_retries) FROM bus_messages`).Scan(&wrong, &receipts); err != nil || wrong != 0 || receipts != 0 {
		t.Fatalf("legacy retry defaults: changed=%d receipts=%d err=%v", wrong, receipts, err)
	}
}

func retry018Index(t *testing.T, files []migrationFile) int {
	t.Helper()
	for i, file := range files {
		if file.name == "018_bus_retry.sql" {
			return i
		}
	}
	t.Fatal("exact 018 migration missing")
	return 0
}

func TestRetry018PreservesExistingRowsAndLedger(t *testing.T) {
	ctx := context.Background()
	pool, all := seedBusUpgrade(t, false)
	index := retry018Index(t, all)
	through018 := all[:index+1]
	if err := migrate(ctx, pool, all[:index]); err != nil {
		t.Fatal(err)
	}
	const priorRows = `SELECT jsonb_agg(to_jsonb(m) ORDER BY id)::text FROM bus_messages m`
	const preservedRows = `SELECT jsonb_agg(to_jsonb(m)-'retry_of'-'admission' ORDER BY id)::text FROM bus_messages m`
	const priorLedger = `SELECT jsonb_agg(to_jsonb(m) ORDER BY name)::text FROM flopwire_schema_migrations m WHERE name<'018_'`
	beforeRows := busUpgradeSnapshot(t, pool, priorRows)
	beforeLedger := busUpgradeSnapshot(t, pool, priorLedger)
	if err := migrate(ctx, pool, through018); err != nil {
		t.Fatal(err)
	}
	if busUpgradeSnapshot(t, pool, preservedRows) != beforeRows {
		t.Fatal("018 changed a preexisting message field or row")
	}
	if busUpgradeSnapshot(t, pool, priorLedger) != beforeLedger {
		t.Fatal("018 changed a historical checksum or applied_at")
	}
	assertLegacyRetryDefaults(t, pool)
	fullRows := busUpgradeSnapshot(t, pool, priorRows)
	fullLedger := busUpgradeSnapshot(t, pool, busUpgradeFullLedgerSnapshot)
	if err := migrate(ctx, pool, through018); err != nil {
		t.Fatal(err)
	}
	if busUpgradeSnapshot(t, pool, priorRows) != fullRows || busUpgradeSnapshot(t, pool, busUpgradeFullLedgerSnapshot) != fullLedger {
		t.Fatal("018 rerun changed complete rows or ledger")
	}
	assertLegacyRetryDefaults(t, pool)
}

func TestRetry018ReceiptBackfill(t *testing.T) {
	ctx := context.Background()
	pool, all := seedBusUpgrade(t, false)
	index := retry018Index(t, all)
	file := all[index]
	if err := migrate(ctx, pool, all[:index]); err != nil {
		t.Fatal(err)
	}
	// Normal legacy upgrades have no retry links. Stage the embedded DDL
	// separately to exercise the receipt backfill with an existing link.
	ddl, backfill, ok := strings.Cut(file.sql, "INSERT INTO bus_retries")
	if !ok {
		t.Fatal("018 receipt backfill missing")
	}
	if _, err := pool.Exec(ctx, ddl); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE bus_messages SET retry_of='parent' WHERE id='reply'`); err != nil {
		t.Fatal(err)
	}
	const fullRows = `SELECT jsonb_agg(to_jsonb(m) ORDER BY id)::text FROM bus_messages m`
	before := busUpgradeSnapshot(t, pool, fullRows)
	if _, err := pool.Exec(ctx, "INSERT INTO bus_retries"+backfill); err != nil {
		t.Fatal(err)
	}
	if got := busUpgradeSnapshot(t, pool, `SELECT jsonb_object_agg(source_id,successor_id)::text FROM bus_retries`); got != `{"parent": "reply"}` {
		t.Fatalf("018 receipt mapping: %s", got)
	}
	if busUpgradeSnapshot(t, pool, fullRows) != before {
		t.Fatal("018 backfill changed message rows")
	}
}
