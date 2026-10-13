package localindex

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// Opt-in qualification writes only an explicitly named disposable snapshot copy.
func TestPrivateSchema13MigrationCopy(t *testing.T) {
	path := os.Getenv("FLOPWIRE_PRIVATE_MIGRATION_COPY")
	if path == "" {
		t.Skip("private snapshot copy not supplied")
	}
	if filepath.Base(path) != "index-migration-copy.db" {
		t.Fatal("refuse a path that is not the designated disposable copy")
	}
	db, err := sql.Open(sqliteDriver, dsn(path, false, 8, false))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var details Details
	var mode string
	if err = db.QueryRow(`SELECT (SELECT value FROM meta WHERE key='tok_detail'),(SELECT value FROM meta WHERE key='tri_detail'),(SELECT CAST(value AS INTEGER) FROM meta WHERE key='tri_parts'),(SELECT value FROM meta WHERE key='mode')`).Scan(&details.Tok, &details.Tri, &details.TriParts, &mode); err != nil {
		t.Fatal(err)
	}
	counts := func() [3]int64 {
		var v [3]int64
		for i, table := range []string{"messages", "conversations", "sources"} {
			if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&v[i]); err != nil {
				t.Fatal(err)
			}
		}
		return v
	}
	before := counts()
	_, rebuilt, err := migrate(db, details, mode, false)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt {
		t.Fatal("additive upgrade rebuilt retained data")
	}
	if after := counts(); after != before {
		t.Fatalf("retained row counts changed: %v -> %v", before, after)
	}
	var version, events int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 14 {
		t.Fatalf("version %d: %v", version, err)
	}
	if err = db.QueryRow("SELECT count(*) FROM change_events").Scan(&events); err != nil || events == 0 {
		t.Fatalf("events %d: %v", events, err)
	}
	t.Logf("private snapshot additive upgrade passed: retained rows %v; commit observations %d", before, events)
}
