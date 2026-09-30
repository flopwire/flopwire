package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/flopwire/flopwire/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

func newPool(t *testing.T, url string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func regclass(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var got *string
	if err := pool.QueryRow(context.Background(), `SELECT to_regclass($1)::text`, name).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return got != nil
}

func TestMigrateFreshDatabaseCreatesSchemaAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t, pgtest.NewDatabase(t))
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{
		"users", "invites", "devices", "credentials", "device_rotations", "audit_events", "collection_policy",
		"sources", "generations", "chunks", "manifest_entries", "provisional_tails", "conversations", "messages",
		"conversation_tombstones", "deletion_jobs", "flopwire_schema_migrations",
		"messages_tsv_idx", "messages_text_trgm_idx", "messages_default_filter_idx",
	} {
		if !regclass(t, pool, "public."+table) {
			t.Errorf("missing %s", table)
		}
	}
	for _, gone := range []string{"segments", "raw_object_ledger", "session_tombstones"} {
		if regclass(t, pool, "public."+gone) {
			t.Errorf("CASS-era relation %s still exists", gone)
		}
	}
	files, err := migrationFiles(migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, pool); err != nil {
		t.Fatalf("replay: %v", err)
	}
	var applied int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM flopwire_schema_migrations`).Scan(&applied); err != nil || applied != len(files) {
		t.Fatalf("applied=%d want=%d err=%v", applied, len(files), err)
	}
}

func TestConcurrentMigrateCallersSerialize(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t, pgtest.NewDatabase(t))
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	start := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- Migrate(ctx, pool)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent migrate: %v", err)
		}
	}
	files, err := migrationFiles(migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	var applied int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM flopwire_schema_migrations`).Scan(&applied); err != nil || applied != len(files) {
		t.Fatalf("applied=%d want=%d err=%v", applied, len(files), err)
	}
}

func TestMigrateRefusesCASSEraSchemaWithoutLedger(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t, pgtest.NewDatabase(t))
	if _, err := pool.Exec(ctx, `CREATE TABLE users(id uuid PRIMARY KEY); CREATE TABLE segments(id uuid PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool); !errors.Is(err, ErrLegacySchema) {
		t.Fatalf("err=%v want ErrLegacySchema", err)
	}
	if regclass(t, pool, "public.flopwire_schema_migrations") {
		t.Fatal("refused migration committed a ledger")
	}
}

func TestMigrateFailureRollsBackSchemaAndLedger(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t, pgtest.NewDatabase(t))
	files, err := migrationFiles(migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	bad := append(append([]migrationFile{}, files...), migrationFile{name: "999_broken.sql", sql: "CREATE TABLE broken(", checksum: "x"})
	if err = migrate(ctx, pool, bad); err == nil || !strings.Contains(err.Error(), "999_broken.sql") {
		t.Fatalf("err=%v, want failure naming 999_broken.sql", err)
	}
	if regclass(t, pool, "public.users") || regclass(t, pool, "public.flopwire_schema_migrations") {
		t.Fatal("failed migration left schema or ledger behind")
	}
	if err = migrate(ctx, pool, files); err != nil {
		t.Fatalf("clean retry: %v", err)
	}
}

func TestMigrateRejectsEditedMissingAndUnknownLedgerEntries(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t, pgtest.NewDatabase(t))
	files, err := migrationFiles(migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	if err = migrate(ctx, pool, files); err != nil {
		t.Fatal(err)
	}
	edited := append([]migrationFile{}, files...)
	edited[0].checksum = "edited"
	if err = migrate(ctx, pool, edited); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("edited: err=%v", err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO flopwire_schema_migrations(name,checksum) VALUES('999_future.sql','x')`); err != nil {
		t.Fatal(err)
	}
	if err = migrate(ctx, pool, files); err == nil || !strings.Contains(err.Error(), "does not know") {
		t.Fatalf("unknown: err=%v", err)
	}
	later := append(append([]migrationFile{}, files...), migrationFile{name: "998_gap.sql", sql: "SELECT 1", checksum: "y"})
	if err = migrate(ctx, pool, append(later, migrationFile{name: "999_future.sql", sql: "SELECT 1", checksum: "x"})); err == nil || !strings.Contains(err.Error(), "contiguous") {
		t.Fatalf("gap: err=%v", err)
	}
}

// The runner binds to the first search_path schema: a second schema is
// migrated in full even when public already holds the schema and pg_trgm.
func TestMigrateBindsToTargetSchema(t *testing.T) {
	ctx := context.Background()
	url := pgtest.NewDatabase(t)
	public := newPool(t, url)
	if err := Migrate(ctx, public); err != nil {
		t.Fatal(err)
	}
	if _, err := public.Exec(ctx, `CREATE SCHEMA isolated`); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "isolated,public"
	isolated, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer isolated.Close()
	if err = Migrate(ctx, isolated); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"isolated.users", "isolated.messages", "isolated.messages_text_trgm_idx", "isolated.flopwire_schema_migrations"} {
		if !regclass(t, public, rel) {
			t.Errorf("missing %s", rel)
		}
	}
}

// Extraction must append to an already deployed redaction migration ledger.
func TestMigrateExtractionAfterMessageRedaction(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t, pgtest.NewDatabase(t))
	files, err := migrationFiles(migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	var prior []migrationFile
	for _, file := range files {
		if file.name < "005_extraction.sql" {
			prior = append(prior, file)
		}
	}
	if len(prior) != 4 {
		t.Fatal("redaction migration prefix missing")
	}
	if err := migrate(ctx, pool, prior); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("upgrade from redaction schema: %v", err)
	}
	if !regclass(t, pool, "extraction_attempt_seq") {
		t.Fatal("attempt sequence missing")
	}
	for _, col := range [][2]string{{"source_parse_state", "extraction_report"}, {"messages", "parse_attempt"}} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=$1 AND column_name=$2)`, col[0], col[1]).Scan(&exists); err != nil || !exists {
			t.Fatalf("column %v missing: %v", col, err)
		}
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("repeated upgrade: %v", err)
	}
}
