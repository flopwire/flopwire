package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/syncproto"
)

func TestAssignLegacyQueuePreservesStateAndCaptures(t *testing.T) {
	db := authorityDB(t)
	// Deliberately old/incomplete tables: opening a store would migrate them.
	for _, q := range []string{`CREATE TABLE devsync_sources(id INTEGER,path TEXT)`, `INSERT INTO devsync_sources VALUES(7,'lost source')`, `CREATE TABLE devsync_known(hash TEXT)`, `INSERT INTO devsync_known VALUES('acked')`, `CREATE TABLE meta(key TEXT,value TEXT)`, `INSERT INTO meta VALUES('mode','full')`} {
		if _, e := db.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	spool := filepath.Join(t.TempDir(), "spool")
	if e := os.MkdirAll(filepath.Join(spool, "chunks"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Mkdir(filepath.Join(spool, "tails"), 0700); e != nil {
		t.Fatal(e)
	}
	data := []byte("captured bytes from deleted source")
	hash := syncproto.Sum(data).String()
	if e := os.WriteFile(filepath.Join(spool, "chunks", hash), data, 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(spool, "tails", "7-2"), data, 0600); e != nil {
		t.Fatal(e)
	}
	cfg := client.Config{Server: "https://example.test", DeviceID: "a"}
	owner, _ := syncAuthorityOwner(cfg)
	key, e := assignLegacyQueue(t.Context(), db, spool, "/synthetic/index.db", owner)
	if e != nil {
		t.Fatal(e)
	}
	for _, scope := range []string{spool, filepath.Join(spool, key)} {
		for _, p := range []string{filepath.Join("chunks", hash), filepath.Join("tails", "7-2")} {
			raw, e := os.ReadFile(filepath.Join(scope, p))
			if e != nil || !bytes.Equal(raw, data) {
				t.Fatal("capture lost", e)
			}
		}
	}
	var saved string
	for query, want := range map[string]string{`SELECT value FROM meta WHERE key='mode'`: "full", `SELECT path FROM devsync_sources WHERE id=7`: "lost source", `SELECT hash FROM devsync_known`: "acked"} {
		if e := db.QueryRow(query).Scan(&saved); e != nil || saved != want {
			t.Fatal("state changed", e, saved)
		}
	}
	var n int
	if e := db.QueryRow(`SELECT count(*) FROM pragma_table_info('devsync_sources')`).Scan(&n); e != nil || n != 2 {
		t.Fatal("migrated schema", e, n)
	}
	// A repeat after acknowledgement cannot copy the retained original back.
	if e := os.Remove(filepath.Join(spool, key, "chunks", hash)); e != nil {
		t.Fatal(e)
	}
	if again, e := assignLegacyQueue(t.Context(), db, spool, "/synthetic/index.db", owner); e != nil || again != key {
		t.Fatal(e)
	}
	if _, e := os.Stat(filepath.Join(spool, key, "chunks", hash)); !os.IsNotExist(e) {
		t.Fatal("resurrected acknowledged capture")
	}
	if _, e := assignLegacyQueue(t.Context(), db, spool, "/synthetic/index.db", "foreign"); e == nil {
		t.Fatal("reassigned foreign queue")
	}
	if got, e := readSyncAuthority(t.Context(), db, cfg); e != nil || got != key {
		t.Fatal("startup cannot resolve assigned queue", e)
	}
}

func TestAssignLegacyQueueRecoversInterruptedCopy(t *testing.T) {
	db := authorityDB(t)
	spool := filepath.Join(t.TempDir(), "spool")
	if e := os.MkdirAll(filepath.Join(spool, "tails"), 0700); e != nil {
		t.Fatal(e)
	}
	// A nonregular capture interrupts after the durable journal is recorded.
	if e := os.Mkdir(filepath.Join(spool, "tails", "bad"), 0700); e != nil {
		t.Fatal(e)
	}
	if _, e := assignLegacyQueue(t.Context(), db, spool, "/index", "owner"); e == nil {
		t.Fatal("accepted directory capture")
	}
	plans, _ := filepath.Glob(filepath.Join(spool, ".assign-*.json"))
	if len(plans) != 1 {
		t.Fatal("missing durable plan")
	}
	before, e := os.ReadFile(plans[0])
	if e != nil {
		t.Fatal(e)
	}
	if _, e := assignLegacyQueue(t.Context(), db, spool, "/index", "foreign"); e == nil {
		t.Fatal("stole pending assignment")
	}
	if e := os.Remove(filepath.Join(spool, "tails", "bad")); e != nil {
		t.Fatal(e)
	}
	var plan legacyAssignment
	if e := json.Unmarshal(before, &plan); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(spool, "tails", "7-2"), []byte("original"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(spool, plan.Key, "tails", "7-2"), []byte("conflict"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := assignLegacyQueue(t.Context(), db, spool, "/index", "owner"); e == nil {
		t.Fatal("overwrote conflicting captured bytes")
	}
	if e := os.Remove(filepath.Join(spool, "tails", "7-2")); e != nil {
		t.Fatal(e)
	}
	if _, e := assignLegacyQueue(t.Context(), db, spool, "/index", "owner"); e == nil {
		t.Fatal("accepted stale capture absent from legacy spool")
	}
	if e := os.WriteFile(filepath.Join(spool, "tails", "7-2"), []byte("original"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Remove(filepath.Join(spool, plan.Key, "tails", "7-2")); e != nil {
		t.Fatal(e)
	}
	if _, e := assignLegacyQueue(t.Context(), db, spool, "/index", "owner"); e != nil {
		t.Fatal(e)
	}
	after, _ := os.ReadFile(plans[0])
	if !bytes.Equal(before, after) {
		t.Fatal("retry changed plan")
	}
}

func TestAssignLegacyQueueCommandRefusesLockAndWrongEnrollment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.db")
	db, e := sql.Open("sqlite", path)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(`CREATE TABLE sentinel(value TEXT)`); e != nil {
		t.Fatal(e)
	}
	db.Close()
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(dir, "config.json"))
	t.Setenv(client.EnvToken, "")
	if e = client.Save(client.Config{Server: "https://example.test", DeviceID: "a", Token: "token"}); e != nil {
		t.Fatal(e)
	}
	args := []string{"--db", path, "--server", "https://example.test", "--device-id", "a"}
	lock, e := localindex.AcquireMaintenanceLock(path)
	if e != nil {
		t.Fatal(e)
	}
	e = agentAssignLegacyQueue(t.Context(), args, &bytes.Buffer{})
	lock.Close()
	if e == nil || !strings.Contains(e.Error(), "stopped collector") {
		t.Fatal(e)
	}
	args[len(args)-1] = "b"
	if e = agentAssignLegacyQueue(t.Context(), args, &bytes.Buffer{}); e == nil || !strings.Contains(e.Error(), "does not match") {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(dir, "spool")); !os.IsNotExist(e) {
		t.Fatal("refusal created spool")
	}
	args[len(args)-1] = "a"
	if e = agentAssignLegacyQueue(t.Context(), args, &bytes.Buffer{}); e != nil {
		t.Fatal(e)
	}
	check, e := sql.Open("sqlite", path)
	if e != nil {
		t.Fatal(e)
	}
	defer check.Close()
	var count int
	if e = check.QueryRow(`SELECT count(*) FROM devsync_authority WHERE id=1`).Scan(&count); e != nil || count != 1 {
		t.Fatal("command did not bind", e)
	}
	if e = check.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT IN ('sentinel','devsync_authority')`).Scan(&count); e != nil || count != 0 {
		t.Fatal("command migrated schema", e)
	}
}

func TestAssignLegacyQueueRejectsCorruptChunkAndSymlink(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "corrupt-chunk", true: "symlink"}[symlink], func(t *testing.T) {
			db := authorityDB(t)
			spool := filepath.Join(t.TempDir(), "spool")
			if e := os.MkdirAll(filepath.Join(spool, "chunks"), 0700); e != nil {
				t.Fatal(e)
			}
			p := filepath.Join(spool, "chunks", syncproto.Sum([]byte("expected")).String())
			if symlink {
				if e := os.Symlink("/absent", p); e != nil {
					t.Fatal(e)
				}
			} else {
				if e := os.WriteFile(p, []byte("corrupt"), 0600); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := assignLegacyQueue(t.Context(), db, spool, "/index", "owner"); e == nil {
				t.Fatal("accepted unsafe capture")
			}
			var count int
			if e := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='devsync_authority'`).Scan(&count); e != nil || count != 0 {
				t.Fatal("bound after refusal", e)
			}
		})
	}
}
