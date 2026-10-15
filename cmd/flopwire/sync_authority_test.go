package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/devicesync"
)

func authorityDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSyncAuthorityBindingRotationAndIndependentQueues(t *testing.T) {
	db := authorityDB(t)
	cc := client.Config{Server: "https://EXAMPLE.test:443/", DeviceID: "device-a", Token: "old"}
	key, err := bindSyncAuthority(t.Context(), db, cc)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := savedSyncSpoolKey(t.Context(), db)
	if err != nil || stored != key {
		t.Fatal("maintenance resolved wrong spool")
	}
	cc.Server = "https://example.test"
	cc.Token = "rotated"
	same, err := bindSyncAuthority(t.Context(), db, cc)
	if err != nil || same != key {
		t.Fatalf("rotation changed queue %q %v", same, err)
	}
	other, err := bindSyncAuthority(t.Context(), authorityDB(t), cc)
	if err != nil || other == key {
		t.Fatalf("independent index reused spool key %q %v", other, err)
	}
	for _, change := range []string{"device", "server"} {
		next := cc
		if change == "device" {
			next.DeviceID = "other"
		} else {
			next.Server = "https://other.test"
		}
		if _, err := bindSyncAuthority(t.Context(), db, next); err == nil {
			t.Fatalf("accepted changed %s", change)
		}
	}
	same, err = bindSyncAuthority(t.Context(), db, cc)
	if err != nil || same != key {
		t.Fatal("mismatched attempt modified binding")
	}
}

func TestSyncAuthorityLegacyStateNeverAdoptsCurrentEnrollment(t *testing.T) {
	for _, kind := range []string{"pending", "fully-acked", "known-only", "empty"} {
		t.Run(kind, func(t *testing.T) {
			db := authorityDB(t)
			if _, err := devicesync.NewStore(db); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "pending", "fully-acked":
				if _, err := db.Exec(`INSERT INTO devsync_sources(id,path,spec,generation) VALUES(1,'/legacy','{}',0)`); err != nil {
					t.Fatal(err)
				}
				acked := 0
				if kind == "fully-acked" {
					acked = 1
				}
				if _, err := db.Exec(`INSERT INTO devsync_gens(source_id,generation,file_id,size,change_time,captured_at,entries,tail_offset,tail_size,acked,tail_acked,closed,lost,srv_tail_off,srv_tail_size) VALUES(1,0,'old',1,0,0,1,1,0,?,1,0,0,0,0)`, acked); err != nil {
					t.Fatal(err)
				}
			case "known-only":
				if _, err := db.Exec(`INSERT INTO devsync_known(hash) VALUES(x'01')`); err != nil {
					t.Fatal(err)
				}
			}
			_, err := bindSyncAuthority(t.Context(), db, client.Config{Server: "https://example.test", DeviceID: "device"})
			if kind == "empty" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "no recorded") {
				t.Fatalf("legacy %s binding = %v", kind, err)
			}
			var tables int
			if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='devsync_authority'`).Scan(&tables); err != nil {
				t.Fatal(err)
			}
			if tables != 0 {
				t.Fatal("refused adoption mutated legacy database")
			}
		})
	}
}

func TestSyncAuthorityUnenrolledTokenChangeDoesNotAdoptQueue(t *testing.T) {
	db := authorityDB(t)
	cc := client.Config{Server: "https://example.test", Token: "one"}
	key, err := bindSyncAuthority(t.Context(), db, cc)
	if err != nil {
		t.Fatal(err)
	}
	cc.Token = "two"
	if _, err := bindSyncAuthority(t.Context(), db, cc); err == nil {
		t.Fatal("unbound token changed queue principal")
	}
	cc.Token = "one"
	same, err := bindSyncAuthority(t.Context(), db, cc)
	if err != nil || same != key {
		t.Fatal("original token lost queue")
	}
}
