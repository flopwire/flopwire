package e2e

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// This fixture keeps shard visibility separate from retained message text,
// matching the observer's role while an agent owns the production writers.
func localFindFixture(t *testing.T, detail string) *device {
	t.Helper()
	index := filepath.Join(t.TempDir(), "index.db")
	db, err := sql.Open("sqlite", index)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{`CREATE TABLE conversations(id INTEGER PRIMARY KEY,session_id TEXT,agent TEXT)`, `CREATE TABLE messages(id INTEGER PRIMARY KEY,conversation_id INTEGER,superseded INTEGER,on_active_path INTEGER,text BLOB)`, `INSERT INTO conversations VALUES(1,'session','claude')`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	shard, err := sql.Open("sqlite", index+"-tri0")
	if err != nil {
		t.Fatal(err)
	}
	defer shard.Close()
	if _, err := shard.Exec(`CREATE VIRTUAL TABLE fts_tri USING fts5(text,tokenize='trigram',detail=` + detail + `,content='')`); err != nil {
		t.Fatal(err)
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	rows := []struct {
		text    string
		sup     int
		onPath  any
		indexed bool
	}{
		{"prefix abcd suffix", 0, nil, true},
		{"old abcd row", 1, nil, true},
		{"inactive abcd row", 0, 0, true},
		{"abc bcd", 0, nil, true},             // Both candidate trigrams, but no exact substring.
		{"unindexed abcd row", 0, nil, false}, // Retained text alone must not satisfy readiness.
		{`unicode café "東京" evidence`, 0, nil, true},
	}
	for i, row := range rows {
		id := i + 1
		if _, err := db.Exec(`INSERT INTO messages VALUES(?,1,?,?,?)`, id, row.sup, row.onPath, enc.EncodeAll([]byte(row.text), nil)); err != nil {
			t.Fatal(err)
		}
		if row.indexed {
			if _, err := shard.Exec(`INSERT INTO fts_tri(rowid,text) VALUES(?,?)`, id, row.text); err != nil {
				t.Fatal(err)
			}
		}
	}
	return &device{h: &harness{ctx: context.Background()}, index: index}
}

func TestLocalFindTrigramDetails(t *testing.T) {
	for _, detail := range []string{"full", "column"} {
		t.Run(detail, func(t *testing.T) {
			d := localFindFixture(t, detail)
			rows, err := d.localFind("abcd")
			if err != nil {
				t.Fatal(err)
			}
			var ids []int64
			for _, row := range rows {
				ids = append(ids, row.ID)
			}
			slices.Sort(ids)
			if !slices.Equal(ids, []int64{1, 2, 3}) {
				t.Fatalf("rows=%v: exact verification and shard readiness must both hold", ids)
			}
			live, sup, err := d.localLive("abcd")
			if err != nil || live != 1 || sup != 1 {
				t.Fatalf("live=%d superseded=%d error=%v", live, sup, err)
			}
			rows, err = d.localFind(`café "東京"`)
			if err != nil || len(rows) != 1 || rows[0].ID != 6 {
				t.Fatalf("unicode/quote rows=%+v error=%v", rows, err)
			}
		})
	}
}

func TestLocalFindRequiresThreeRunes(t *testing.T) {
	d := localFindFixture(t, "column")
	for _, needle := range []string{"", "ab", "東京"} {
		t.Run(fmt.Sprintf("%q", needle), func(t *testing.T) {
			if _, err := d.localFind(needle); err == nil {
				t.Fatal("short needle must fail explicitly")
			}
		})
	}
}
