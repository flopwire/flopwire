package perfguard

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// sqliteItems is a fresh SQLite file with items(id pk, grp unindexed, v)
// of n rows, opened through SQLiteDriver and counted.
func sqliteItems(t testing.TB, n int) (*sql.DB, *SQLiteCounter) {
	t.Helper()
	dir := t.TempDir()
	c := CountSQLite(t, dir)
	db := OpenSQLite(t, "file:"+filepath.Join(dir, "items.db"))
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE items (id INTEGER PRIMARY KEY, grp INTEGER NOT NULL, v INTEGER NOT NULL, note TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `WITH RECURSIVE s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i < ?)
		INSERT INTO items SELECT i, i, i % 7, 'note number ' || i FROM s`, n); err != nil {
		t.Fatal(err)
	}
	return db, c
}

func TestSQLiteCounterCountsStatementsAndWrites(t *testing.T) {
	db, c := sqliteItems(t, 10)
	ctx := context.Background()
	cost := MeasureSQLite(c, func() {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		st, err := tx.PrepareContext(ctx, `UPDATE items SET v = ? WHERE id = ?`)
		if err != nil {
			t.Fatal(err)
		}
		for i := range 5 {
			if _, err := st.ExecContext(ctx, i, i+1); err != nil {
				t.Fatal(err)
			}
		}
		st.Close()
		if _, err := tx.ExecContext(ctx, `DELETE FROM items WHERE id > 8`); err != nil {
			t.Fatal(err)
		}
		rows, err := tx.QueryContext(ctx, `SELECT id FROM items`)
		if err != nil {
			t.Fatal(err)
		}
		rows.Close()
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	})
	if cost.Statements != 9 {
		t.Fatalf("statements %d, want BEGIN+5+1+1+COMMIT=9\n%s", cost.Statements, c)
	}
	if got := c.BySQL()["UPDATE items SET v = ? WHERE id = ?"]; got != 5 {
		t.Fatalf("prepared update counted %d, want 5\n%s", got, c)
	}
	if w := cost.Tables["main.items"]; w.TupUpd != 5 || w.TupDel != 2 || w.TupIns != 0 {
		t.Fatalf("writes %s, want upd=5 del=2", w)
	}
	if cost.Pages <= 0 {
		t.Fatalf("pages %d, want > 0", cost.Pages)
	}
}

// Connections whose DSN no counter claims are the plain driver's.
func TestSQLiteDriverPassesUnclaimedThrough(t *testing.T) {
	db := OpenSQLite(t, "file:"+filepath.Join(t.TempDir(), "x.db"))
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.Raw(func(dc any) error {
		if _, ok := dc.(*countedConn); ok {
			t.Error("unclaimed connection is wrapped")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// sqlitePerItem runs one lookup per item; rescan makes each one scan the
// whole table (quadratic in pages fetched), otherwise each is a primary
// key lookup.
func sqlitePerItem(rescan bool) func(t testing.TB, n int) Cost {
	return func(t testing.TB, n int) Cost {
		db, c := sqliteItems(t, n)
		ctx := context.Background()
		q := `SELECT v FROM items WHERE id = ?`
		if rescan {
			q = `SELECT sum(v) FROM items WHERE grp = ?`
		}
		return MeasureSQLite(c, func() {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			for i := 1; i <= n; i++ {
				var v int
				if err := tx.QueryRowContext(ctx, q, i).Scan(&v); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestSQLiteScalingLinearPasses(t *testing.T) {
	AssertScaling(t, Linear, 300, 8, sqlitePerItem(false))
}

func TestSQLiteScalingCatchesQuadratic(t *testing.T) {
	r := &recorder{TB: t}
	AssertScaling(r, Linear, 300, 8, sqlitePerItem(true))
	if !strings.Contains(r.failed(), "pages") {
		t.Fatalf("quadratic page fetches not caught: %q", r.failed())
	}
}

// One lookup per item is linear in statements: a Constant guard (an
// append that should not depend on the session size) fails on it.
func TestSQLiteScalingConstantCatchesPerItemStatements(t *testing.T) {
	r := &recorder{TB: t}
	AssertScaling(r, Constant, 300, 8, sqlitePerItem(false))
	if !strings.Contains(r.failed(), "statements") {
		t.Fatalf("linear statement count not caught: %q", r.failed())
	}
}

func TestAssertSQLitePlan(t *testing.T) {
	db, _ := sqliteItems(t, 10)
	if _, err := db.Exec(`CREATE INDEX items_v ON items (v)`); err != nil {
		t.Fatal(err)
	}
	AssertSQLitePlan(t, db, nil, `SELECT note FROM items WHERE id = ?`, 3)
	AssertSQLitePlan(t, db, nil, `SELECT id FROM items i WHERE i.v = ? ORDER BY i.v`, 3)
	for _, tc := range []struct{ q, want string }{
		{`SELECT note FROM items WHERE grp = ?`, "SCAN items"},
		{`SELECT note FROM items m WHERE m.grp = ?`, "SCAN m"},
		{`SELECT id FROM items WHERE id > ? ORDER BY grp`, "TEMP B-TREE"},
		{`SELECT v FROM items WHERE v > 0 OR id < 0 GROUP BY note`, "TEMP B-TREE"},
		{`SELECT count(*) FROM items i WHERE EXISTS (SELECT 1 FROM items j WHERE j.grp = i.id)`, "AUTOMATIC"},
	} {
		r := &recorder{TB: t}
		AssertSQLitePlan(r, db, nil, tc.q, 1)
		if !strings.Contains(r.failed(), tc.want) {
			t.Errorf("%s: want failure naming %q, got %q", tc.q, tc.want, r.failed())
		}
	}
	// Allowed by name (alias as the plan reports it).
	AssertSQLitePlan(t, db, []string{"m"}, `SELECT note FROM items m WHERE m.grp = ?`, 1)
}
