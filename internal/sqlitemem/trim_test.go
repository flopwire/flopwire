package sqlitemem

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestTrim runs Trim between SQLite work: the linknamed lock and allocator
// must be the ones libc uses, or SQLite breaks after a Trim.
func TestTrim(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (x TEXT)`); err != nil {
		t.Fatal(err)
	}
	for round := range 3 {
		tx, _ := db.Begin()
		for i := range 5000 {
			if _, err := tx.Exec(`INSERT INTO t VALUES (?)`, string(make([]byte, 100+i%3000))); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`PRAGMA shrink_memory`); err != nil {
			t.Fatal(err)
		}
		Trim()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil || n != 5000*(round+1) {
			t.Fatalf("round %d: n=%d err=%v", round, n, err)
		}
	}
}
