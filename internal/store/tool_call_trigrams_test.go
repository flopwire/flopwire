package store

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/flopwire/flopwire/migrations"
)

func TestToolCallTrigramMigrationAdoption(t *testing.T) {
	for _, tc := range []struct {
		name, ddl string
		reject    bool
	}{
		{"fresh", "", false},
		{"concurrent prebuild", `CREATE INDEX CONCURRENTLY messages_tool_call_trgm_idx ON messages USING gin(text gin_trgm_ops) WHERE kind='tool_call'`, false},
		{"wrong kind", `CREATE INDEX messages_tool_call_trgm_idx ON messages USING gin(text gin_trgm_ops) WHERE kind='tool_result'`, true},
		{"wrong column", `CREATE INDEX messages_tool_call_trgm_idx ON messages USING gin(kind gin_trgm_ops) WHERE kind='tool_call'`, true},
		{"wrong table", `CREATE INDEX messages_tool_call_trgm_idx ON conversations USING gin(title gin_trgm_ops) WHERE title='tool_call'`, true},
		{"wrong method", `CREATE INDEX messages_tool_call_trgm_idx ON messages(text) WHERE kind='tool_call'`, true},
		{"wrong collation", `CREATE INDEX messages_tool_call_trgm_idx ON messages USING gin(text COLLATE "C" gin_trgm_ops) WHERE kind='tool_call'`, true},
		{"wrong options", `CREATE INDEX messages_tool_call_trgm_idx ON messages USING gin(text gin_trgm_ops) WITH(fastupdate=off) WHERE kind='tool_call'`, true},
		{"invalid", `CREATE INDEX messages_tool_call_trgm_idx ON messages USING gin(text gin_trgm_ops) WHERE kind='tool_call'; UPDATE pg_index SET indisvalid=false WHERE indexrelid='messages_tool_call_trgm_idx'::regclass`, true},
		{"not ready", `CREATE INDEX messages_tool_call_trgm_idx ON messages USING gin(text gin_trgm_ops) WHERE kind='tool_call'; UPDATE pg_index SET indisready=false WHERE indexrelid='messages_tool_call_trgm_idx'::regclass`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newPool(t, pgtest.NewDatabase(t))
			files, err := migrationFiles(migrations.Files)
			if err != nil {
				t.Fatal(err)
			}
			at := slices.IndexFunc(files, func(f migrationFile) bool { return f.name == "017_tool_call_trigrams.sql" })
			if at < 0 {
				t.Fatal("missing additive migration")
			}
			if err = migrate(ctx, pool, files[:at]); err != nil {
				t.Fatal(err)
			}
			if tc.ddl != "" {
				if _, err = pool.Exec(ctx, tc.ddl); err != nil {
					t.Fatal(err)
				}
			}
			var before uint32
			if tc.name != "fresh" {
				if err = pool.QueryRow(ctx, `SELECT 'messages_tool_call_trgm_idx'::regclass::oid`).Scan(&before); err != nil {
					t.Fatal(err)
				}
			}
			err = Migrate(ctx, pool)
			if tc.reject {
				if err == nil || !strings.Contains(err.Error(), "required valid tool_call trigram index") {
					t.Fatalf("expected definition refusal: %v", err)
				}
				var applied bool
				if err = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM flopwire_schema_migrations WHERE name='017_tool_call_trigrams.sql')`).Scan(&applied); err != nil || applied {
					t.Fatalf("refusal recorded migration: %v %v", applied, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var after uint32
			if err = pool.QueryRow(ctx, `SELECT 'messages_tool_call_trgm_idx'::regclass::oid`).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if before != 0 && before != after {
				t.Fatal("prebuilt index replaced")
			}
			if err = Migrate(ctx, pool); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestToolCallTrigramsExtensionNamespace(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t, pgtest.NewDatabase(t))
	if _, err := pool.Exec(ctx, `CREATE SCHEMA extensions; CREATE EXTENSION pg_trgm WITH SCHEMA extensions`); err != nil {
		t.Fatal(err)
	}
	files, err := migrationFiles(migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	at := slices.IndexFunc(files, func(f migrationFile) bool { return f.name == "017_tool_call_trigrams.sql" })
	if at < 0 {
		t.Fatal("missing migration")
	}
	if err = migrate(ctx, pool, files[:at]); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `CREATE INDEX CONCURRENTLY messages_tool_call_trgm_idx ON messages USING gin(text extensions.gin_trgm_ops) WHERE kind='tool_call'`); err != nil {
		t.Fatal(err)
	}
	var before uint32
	if err = pool.QueryRow(ctx, `SELECT 'messages_tool_call_trgm_idx'::regclass::oid`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var namespace string
	var after uint32
	if err = pool.QueryRow(ctx, `SELECT n.nspname,i.indexrelid FROM pg_index i JOIN pg_opclass o ON o.oid=i.indclass[0] JOIN pg_namespace n ON n.oid=o.opcnamespace WHERE i.indexrelid='messages_tool_call_trgm_idx'::regclass`).Scan(&namespace, &after); err != nil || namespace != "extensions" || before != after {
		t.Fatalf("opclass namespace=%s sameindex=%v err=%v", namespace, before == after, err)
	}
}
